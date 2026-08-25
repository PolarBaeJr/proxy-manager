// MCP tool surface.
//
// Every tool dispatches through the dashboard's OWN API mux rather than
// touching Docker or the stores directly. That is deliberate: the handlers
// carry guardUnscalable, the onboarded-vs-label distinction, canary
// bookkeeping, proxy refresh, and the audit log. Calling the internals would
// fork all of it and the two paths would drift.
//
// Requests are served in-process — no socket, no network hop — authenticated
// with the process-local credential from auth.go. The caller's actor assertion
// is copied across so the audit log names the person, not the service.
//
// Mutating tools are only REGISTERED when writes are enabled, so a read-only
// deployment cannot reach one by guessing a name.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
)

// apiCaller dispatches a request through the dashboard's API handlers.
type apiCaller struct {
	mux http.Handler
}

// call builds a request, authenticates it as the internal principal, forwards
// the actor assertion, and returns the handler's body.
//
// A non-2xx becomes an error carrying the handler's own message — "no live
// replicas", "unknown host" — which is what lets the model correct itself.
func (a *apiCaller) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rdr *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if internalToken == "" {
		return nil, fmt.Errorf("internal credential unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+internalToken)
	// Attribution only — the credential above is what authorizes the call.
	if a := actorFrom(ctx); a != "" {
		req.Header.Set(actorHeader, a)
	}

	rec := httptest.NewRecorder()
	a.mux.ServeHTTP(rec, req)
	out := rec.Body.Bytes()
	if rec.Code/100 != 2 {
		return nil, fmt.Errorf("%s %s: %d %s", method, path, rec.Code, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// pretty re-indents a JSON response. Tool output is read by a model, so
// readable beats compact; a non-JSON body passes through untouched.
func pretty(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(b)
	}
	return string(out)
}

// hostArg reads the optional peer-targeting argument named by key, gated
// separately from allowWrites: a model acting on a hallucinated or wrong
// host could mutate a service it never meant to touch, so reaching another
// host by name requires its own explicit opt-in (MCP_ALLOW_PEER_WRITES)
// even when local writes are already enabled.
//
// key is not always "host" — onboard_service already uses that name for the
// hostname to ROUTE (an unrelated, required argument), so it reads this one
// under "peer_host" instead; every other tool here reads it under "host".
func hostArg(args map[string]any, key string, allowPeerWrites bool) (string, error) {
	host, err := argOptionalString(args, key)
	if err != nil {
		return "", err
	}
	if host == "" {
		return "", nil
	}
	if !allowPeerWrites {
		return "", fmt.Errorf("%s %q given but cross-host MCP writes are disabled (set MCP_ALLOW_PEER_WRITES=true to enable)", key, host)
	}
	return host, nil
}

// withHost appends ?host=<host> (or &host=<host> if path already has a
// query string) to path, or returns path unchanged if host is empty.
func withHost(path, host string) string {
	if host == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "host=" + url.QueryEscape(host)
}

func registerMCPTools(s *Server, a *apiCaller, allowWrites, allowPeerWrites bool) {
	// ---- read-only ----

	s.Register(Tool{
		Name:        "list_services",
		Title:       "List services",
		Description: "List every service the proxy manages: image, replica count, running count, host, whether an image update is available, and canary state.",
		InputSchema: schema(map[string]any{}),
		Handler: func(ctx context.Context, _ map[string]any) (string, error) {
			b, err := a.call(ctx, "GET", "/api/services", nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "list_routes",
		Title:       "List routes",
		Description: "List the host/path routes the proxy currently serves and their backends, from both container labels and static config.",
		InputSchema: schema(map[string]any{}),
		Handler: func(ctx context.Context, _ map[string]any) (string, error) {
			b, err := a.call(ctx, "GET", "/api/routes", nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "get_logs",
		Title:       "Get container logs",
		Description: "Fetch recent log lines for one container. Use list_services first for exact container names.",
		InputSchema: schema(map[string]any{
			"container": prop("string", "Exact container name."),
			"tail":      prop("number", "Trailing lines to return (default 200, max 2000)."),
		}, "container"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "container")
			if err != nil {
				return "", err
			}
			tail := 200
			if _, ok := args["tail"]; ok {
				if tail, err = argInt(args, "tail"); err != nil {
					return "", err
				}
			}
			if tail < 1 {
				tail = 1
			}
			if tail > 2000 {
				tail = 2000
			}
			b, err := a.call(ctx, "GET", "/api/logs/"+url.PathEscape(name)+"?tail="+fmt.Sprint(tail), nil)
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
	})

	s.Register(Tool{
		Name:        "maintenance_status",
		Title:       "Maintenance status",
		Description: "Show which hosts currently serve the maintenance page, and which have their own custom page.",
		InputSchema: schema(map[string]any{}),
		Handler: func(ctx context.Context, _ map[string]any) (string, error) {
			b, err := a.call(ctx, "GET", "/api/maintenance", nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "list_dns",
		Title:       "List DNS records",
		Description: "List Cloudflare DNS records for a zone. Omit zone for the default.",
		InputSchema: schema(map[string]any{
			"zone": prop("string", "Zone domain, e.g. polardev.org. Omit for the default."),
		}),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			// argOptionalString, not argString: an explicit zone:"" is a
			// realistic input from clients that always send every schema key,
			// and the backend (cfZoneFromReq) already treats "" identically
			// to an omitted zone (falls back to the default zone).
			zone, err := argOptionalString(args, "zone")
			if err != nil {
				return "", err
			}
			b, err := a.call(ctx, "GET", "/api/cf/records?zone="+url.QueryEscape(zone), nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	// Registered read-only (above the allowWrites gate) even though the
	// backend route it calls is wrapped in requireElevated: the handler only
	// forces a registry-digest comparison and caches the result, it never
	// touches the running service. The internal credential in apiCaller.call
	// already bypasses the elevated check, same as every other tool here.
	s.Register(Tool{
		Name:        "check_for_update",
		Title:       "Check for an image update",
		Description: "Force an immediate check of whether a newer image is available in the registry for this service, instead of waiting for the periodic background poll (runs roughly every 10 minutes). Does not change anything about the running service — only refreshes the cached 'update available' status, which then shows up in list_services. If a canary is currently staged, its image is checked too — the response is a single status object when there's no canary, or {\"live\":..., \"canary\":...} when there is.",
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"host":    prop("string", "Optional peer hostname/identity (see the \"machine\" field returned by list_services) to target a service on a DIFFERENT dashboard host instead of this one. Requires MCP_ALLOW_PEER_WRITES."),
		}, "service"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			path := "/api/services/" + url.PathEscape(name) + "/check"
			b, err := a.call(ctx, "POST", withHost(path, host), nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	if !allowWrites {
		return
	}

	// ---- mutating (MCP_ALLOW_WRITES=true only) ----

	s.Register(Tool{
		Name:        "set_maintenance",
		Title:       "Set maintenance mode",
		Description: "Put a host into maintenance (public visitors get a 503 page) or take it out. Reversible. The host must already be routed.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"host":    prop("string", "Hostname, e.g. sfubadminton.com."),
			"enabled": prop("boolean", "true to turn maintenance on, false to turn it off."),
		}, "host", "enabled"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			host, err := argString(args, "host")
			if err != nil {
				return "", err
			}
			on, err := argBool(args, "enabled")
			if err != nil {
				return "", err
			}
			method := "DELETE"
			if on {
				method = "POST"
			}
			b, err := a.call(ctx, method, "/api/maintenance/"+url.PathEscape(host), nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "scale_service",
		Title:       "Scale a service",
		Description: "Change a service's replica count. Refused for services labelled proxy.unscalable.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service":  prop("string", "Service name from list_services."),
			"replicas": prop("number", "Desired replica count (>= 0)."),
			"host":     prop("string", "Optional peer hostname/identity (see the \"machine\" field returned by list_services) to target a service on a DIFFERENT dashboard host instead of this one. Requires MCP_ALLOW_PEER_WRITES."),
		}, "service", "replicas"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			n, err := argInt(args, "replicas")
			if err != nil {
				return "", err
			}
			if n < 0 {
				return "", fmt.Errorf("replicas must not be negative")
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			path := "/api/services/" + url.PathEscape(name) + "/scale"
			b, err := a.call(ctx, "POST", withHost(path, host), map[string]any{"replicas": n})
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "lifecycle_service",
		Title:       "Start or stop a service",
		Description: "Stop all replicas of a service, or start them again. Stopping keeps containers and their config, so it is reversible.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"action":  map[string]any{"type": "string", "enum": []string{"start", "stop"}, "description": "start or stop"},
			"host":    prop("string", "Optional peer hostname/identity (see the \"machine\" field returned by list_services) to target a service on a DIFFERENT dashboard host instead of this one. Requires MCP_ALLOW_PEER_WRITES."),
		}, "service", "action"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			action, err := argString(args, "action")
			if err != nil {
				return "", err
			}
			if action != "start" && action != "stop" {
				return "", fmt.Errorf("action must be \"start\" or \"stop\", got %q", action)
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			path := "/api/services/" + url.PathEscape(name) + "/" + action
			b, err := a.call(ctx, "POST", withHost(path, host), nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "set_autoupdate",
		Title:       "Toggle auto-update",
		Description: "Opt a service in or out of unattended updates when a newer image digest appears. Works for any routed service, onboarded or label-managed.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"enabled": prop("boolean", "true to enable unattended updates."),
			"host":    prop("string", "Optional peer hostname/identity (see the \"machine\" field returned by list_services) to target a service on a DIFFERENT dashboard host instead of this one. Requires MCP_ALLOW_PEER_WRITES."),
		}, "service", "enabled"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			on, err := argBool(args, "enabled")
			if err != nil {
				return "", err
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			path := "/api/services/" + url.PathEscape(name) + "/autoupdate"
			b, err := a.call(ctx, "POST", withHost(path, host), map[string]any{"enabled": on})
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:  "stage_canary",
		Title: "Stage a canary",
		Description: "Deploy a new image alongside the live one so traffic splits across both, " +
			"optionally editing env vars at the same time. Nothing is removed. Follow with " +
			"resolve_canary. Preferred over a direct replace because it is reversible. " +
			"Env edits are MERGED onto the service's current env, not a replacement: a key " +
			"absent from the current env is added, a key with an identical value is a no-op, " +
			"and a key whose value differs is refused as a conflict (see env_ack). There is " +
			"no way to remove an env var through this tool.",
		Mutating: true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"image":   prop("string", "Full image reference including tag."),
			"env": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "Env var edits (name -> new value), merged onto the service's " +
					"current env. A name whose value differs from what's running is refused as " +
					"a conflict unless also listed in env_ack; call again with the conflicting " +
					"keys in env_ack to confirm the overwrite. For a REAL SECRET, never pass the " +
					"literal value here \u2014 it would land in this tool call and in your transcript. " +
					"Pass \"ref:NAME\" instead; the dashboard resolves NAME server-side from a " +
					"secrets file (SECRETS_DIR/<service>.env, mounted only on the Pi) and the literal value " +
					"never reaches this API. Ask the operator to add the KEY=VALUE line to " +
					"this service's own secrets file (SECRETS_DIR/<service>.env) if the ref " +
					"doesn't resolve.",
			},
			"env_ack": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Env var names from a prior conflict response that should be " +
					"overwritten. Resubmit the same env plus these names.",
			},
			"host": prop("string", "Optional peer hostname/identity (see the \"machine\" field returned by list_services) to target a service on a DIFFERENT dashboard host instead of this one. Requires MCP_ALLOW_PEER_WRITES."),
		}, "service", "image"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			image, err := argString(args, "image")
			if err != nil {
				return "", err
			}
			env, err := argEnvEdits(args, "env")
			if err != nil {
				return "", err
			}
			ack, err := argStringSlice(args, "env_ack")
			if err != nil {
				return "", err
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			body := map[string]any{"image": image}
			if len(env) > 0 {
				body["env"] = env
			}
			if len(ack) > 0 {
				body["env_ack"] = ack
			}
			path := "/api/services/" + url.PathEscape(name) + "/stage"
			b, err := a.call(ctx, "POST", withHost(path, host), body)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:  "replace_service",
		Title: "Replace a service's image directly",
		Description: "Replace a service's running image directly — NOT reversible (old " +
			"containers are torn down immediately). Optionally edits env vars using the same " +
			"merge/conflict rules as stage_canary. Prefer stage_canary + resolve_canary when " +
			"you want a reversible rollout; use this only when a direct swap is intended.",
		Mutating: true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"image":   prop("string", "Full image reference including tag."),
			"env": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "Env var edits (name -> new value), merged onto the service's " +
					"current env. A name whose value differs from what's running is refused as " +
					"a conflict unless also listed in env_ack; call again with the conflicting " +
					"keys in env_ack to confirm the overwrite. For a REAL SECRET, never pass the " +
					"literal value here \u2014 it would land in this tool call and in your transcript. " +
					"Pass \"ref:NAME\" instead; the dashboard resolves NAME server-side from a " +
					"secrets file (SECRETS_DIR/<service>.env, mounted only on the Pi) and the literal value " +
					"never reaches this API. Ask the operator to add the KEY=VALUE line to " +
					"this service's own secrets file (SECRETS_DIR/<service>.env) if the ref " +
					"doesn't resolve.",
			},
			"env_ack": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Env var names from a prior conflict response that should be " +
					"overwritten. Resubmit the same env plus these names.",
			},
			"host": prop("string", "Optional peer hostname/identity (see the \"machine\" field returned by list_services) to target a service on a DIFFERENT dashboard host instead of this one. Requires MCP_ALLOW_PEER_WRITES."),
		}, "service", "image"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			image, err := argString(args, "image")
			if err != nil {
				return "", err
			}
			env, err := argEnvEdits(args, "env")
			if err != nil {
				return "", err
			}
			ack, err := argStringSlice(args, "env_ack")
			if err != nil {
				return "", err
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			body := map[string]any{"image": image}
			if len(env) > 0 {
				body["env"] = env
			}
			if len(ack) > 0 {
				body["env_ack"] = ack
			}
			path := "/api/services/" + url.PathEscape(name) + "/replace"
			b, err := a.call(ctx, "POST", withHost(path, host), body)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "resolve_canary",
		Title:       "Promote or discard a canary",
		Description: "Promote a staged canary to live (old replicas removed) or discard it (canary replicas removed, live untouched).",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"action":  map[string]any{"type": "string", "enum": []string{"promote", "discard"}, "description": "promote or discard"},
			"host":    prop("string", "Optional peer hostname/identity (see the \"machine\" field returned by list_services) to target a service on a DIFFERENT dashboard host instead of this one. Requires MCP_ALLOW_PEER_WRITES."),
		}, "service", "action"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			action, err := argString(args, "action")
			if err != nil {
				return "", err
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			switch action {
			case "promote":
				path := "/api/services/" + url.PathEscape(name) + "/promote"
				b, err := a.call(ctx, "POST", withHost(path, host), nil)
				if err != nil {
					return "", err
				}
				return pretty(b), nil
			case "discard":
				path := "/api/services/" + url.PathEscape(name) + "/canary"
				b, err := a.call(ctx, "DELETE", withHost(path, host), nil)
				if err != nil {
					return "", err
				}
				return pretty(b), nil
			}
			return "", fmt.Errorf("action must be \"promote\" or \"discard\", got %q", action)
		},
	})

	s.Register(Tool{
		Name:  "onboard_service",
		Title: "Onboard an unmanaged container as a label-managed service",
		Description: "Relabels and DESTRUCTIVELY RECREATES an existing unmanaged container as N " +
			"ordinary label-managed replicas: captures its image/env/mounts, builds proxy.* labels " +
			"from the given host/port/path, creates and starts the replacement replicas, then stops " +
			"and removes the ORIGINAL container. This is not a passive adopt — the original " +
			"container is gone afterward and the replacements (goproxy-<service>-N) are " +
			"indistinguishable from any docker-compose-created labeled service. Refuses up front if " +
			"the container has HostConfig a recreate can't reproduce (extra port bindings, " +
			"capabilities, a second docker network, ...) rather than silently dropping it. Use " +
			"list_services or the discovery view to find unmanaged container names first.",
		Mutating: true,
		InputSchema: schema(map[string]any{
			"service":  prop("string", "Name of the existing unmanaged container to onboard."),
			"host":     prop("string", "Hostname to route (e.g. app.example.com)."),
			"port":     prop("number", "Container's internal port to route to."),
			"path":     prop("string", "Path prefix to route (default: all paths)."),
			"strip":    prop("boolean", "Strip the path prefix before forwarding (default: false)."),
			"replicas": prop("number", "Number of replacement replicas to create (default: 1)."),
			"peer_host": prop("string", "Optional peer hostname/identity (see the \"machine\" field returned by "+
				"list_services) to onboard a container on a DIFFERENT dashboard host instead of this one. Not "+
				"to be confused with \"host\" above, which is the hostname to ROUTE. Requires MCP_ALLOW_PEER_WRITES."),
		}, "service", "host", "port"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			host, err := argString(args, "host")
			if err != nil {
				return "", err
			}
			port, err := argInt(args, "port")
			if err != nil {
				return "", err
			}
			path, err := argOptionalString(args, "path")
			if err != nil {
				return "", err
			}
			strip := false
			if _, ok := args["strip"]; ok {
				if strip, err = argBool(args, "strip"); err != nil {
					return "", err
				}
			}
			replicas := 1
			if _, ok := args["replicas"]; ok {
				if replicas, err = argInt(args, "replicas"); err != nil {
					return "", err
				}
			}
			peerHost, err := hostArg(args, "peer_host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			path2 := "/api/discovery/" + url.PathEscape(name) + "/onboard"
			b, err := a.call(ctx, "POST", withHost(path2, peerHost), OnboardRequest{
				Host: host, Port: port, Path: path, Strip: strip, Replicas: replicas,
			})
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:  "offboard_service",
		Title: "Stop routing a service without destroying its containers",
		Description: "Stops routing a service without stopping or deleting its container(s). For " +
			"an ordinary label-managed service this disconnects its container(s) from the edge " +
			"docker network only — they keep running, just unrouted; reconnect (or `docker compose " +
			"up -d`) to resume routing. For a service still tracked in the legacy OnboardedStore, " +
			"this instead removes the onboarded tracking record and routes.json entry and tears " +
			"down any cloned/canary containers it created (goproxy-onb-<name>-*), leaving the " +
			"ORIGINAL container running untouched.",
		Mutating: true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"host":    prop("string", "Optional peer hostname/identity (see the \"machine\" field returned by list_services) to target a service on a DIFFERENT dashboard host instead of this one. Requires MCP_ALLOW_PEER_WRITES."),
		}, "service"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			path := "/api/services/" + url.PathEscape(name) + "/offboard"
			b, err := a.call(ctx, "POST", withHost(path, host), nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "restart_replica",
		Title:       "Start, stop, or restart a replica",
		Description: "Start, stop, or restart one replica of a service (not the whole service — use lifecycle_service for that). member must be a NON-canary replica name from list_services' member_summaries (one where is_canary is false/absent) — canary replicas can't be managed here, use resolve_canary instead. restart is stop immediately followed by start; on a service's only running (non-canary) replica this causes a brief window with no live backend for that host, so check member_summaries/replicas count first if that matters.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"member":  prop("string", "Replica name from list_services' member_summaries."),
			"action":  map[string]any{"type": "string", "enum": []string{"start", "stop", "restart"}, "description": "start, stop, or restart"},
			"host":    prop("string", "Optional peer hostname/identity (see the \"machine\" field returned by list_services) to target a service on a DIFFERENT dashboard host instead of this one. Requires MCP_ALLOW_PEER_WRITES."),
		}, "service", "member", "action"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			member, err := argString(args, "member")
			if err != nil {
				return "", err
			}
			action, err := argString(args, "action")
			if err != nil {
				return "", err
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			base := "/api/services/" + url.PathEscape(name) + "/replicas/" + url.PathEscape(member) + "/"
			switch action {
			case "start", "stop":
				b, err := a.call(ctx, "POST", withHost(base+action, host), nil)
				if err != nil {
					return "", err
				}
				return pretty(b), nil
			case "restart":
				// Abort on stop failure rather than attempting start anyway —
				// a replica already down for another reason shouldn't be
				// force-started as a side effect of a restart request.
				if _, err := a.call(ctx, "POST", withHost(base+"stop", host), nil); err != nil {
					return "", err
				}
				b, err := a.call(ctx, "POST", withHost(base+"start", host), nil)
				if err != nil {
					// The stop already succeeded, so this is not a generic
					// failure — the replica is now down and needs the caller's
					// attention, not a silent "the start call failed" that
					// leaves them thinking it's still running.
					return "", fmt.Errorf("stop succeeded but start failed — replica %q of service %q is now STOPPED (no live backend from it), retry with action=start: %w", member, name, err)
				}
				return pretty(b), nil
			}
			return "", fmt.Errorf("action must be \"start\", \"stop\", or \"restart\", got %q", action)
		},
	})

	s.Register(Tool{
		Name:        "create_dns_record",
		Title:       "Create a DNS record",
		Description: "Create a Cloudflare DNS record. Omit zone for the default.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"zone":     prop("string", "Zone domain, e.g. polardev.org. Omit for the default."),
			"type":     prop("string", "Record type, e.g. A, CNAME, TXT, MX."),
			"name":     prop("string", "Record name."),
			"content":  prop("string", "Record content — the target/value."),
			"proxied":  prop("boolean", "Whether Cloudflare proxies this record. Defaults to false."),
			"ttl":      prop("number", "TTL in seconds. Defaults to 1 (automatic) when proxied."),
			"priority": prop("number", "Priority — MX records only."),
		}, "type", "name", "content"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			// argOptionalString, not argString: an explicit zone:"" is a
			// realistic input from clients that always send every schema key,
			// and the backend (cfZoneFromReq) already treats "" identically
			// to an omitted zone (falls back to the default zone).
			zone, err := argOptionalString(args, "zone")
			if err != nil {
				return "", err
			}
			typ, err := argString(args, "type")
			if err != nil {
				return "", err
			}
			name, err := argString(args, "name")
			if err != nil {
				return "", err
			}
			content, err := argString(args, "content")
			if err != nil {
				return "", err
			}
			body := map[string]any{"type": typ, "name": name, "content": content}
			if _, ok := args["proxied"]; ok {
				proxied, err := argBool(args, "proxied")
				if err != nil {
					return "", err
				}
				body["proxied"] = proxied
			}
			if _, ok := args["ttl"]; ok {
				ttl, err := argInt(args, "ttl")
				if err != nil {
					return "", err
				}
				body["ttl"] = ttl
			}
			if _, ok := args["priority"]; ok {
				priority, err := argInt(args, "priority")
				if err != nil {
					return "", err
				}
				body["priority"] = priority
			}
			b, err := a.call(ctx, "POST", "/api/cf/records?zone="+url.QueryEscape(zone), body)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "update_dns_record",
		Title:       "Update a DNS record",
		Description: "Partially update a Cloudflare DNS record. Only the fields provided are changed; omit a field to leave it untouched. Omit zone for the default.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"zone":    prop("string", "Zone domain, e.g. polardev.org. Omit for the default."),
			"id":      prop("string", "Cloudflare record ID, from list_dns."),
			"content": prop("string", "New content — the target/value."),
			"proxied": prop("boolean", "Whether Cloudflare proxies this record."),
			"name":    prop("string", "New record name."),
		}, "id"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			// argOptionalString, not argString: an explicit zone:"" is a
			// realistic input from clients that always send every schema key,
			// and the backend (cfZoneFromReq) already treats "" identically
			// to an omitted zone (falls back to the default zone).
			zone, err := argOptionalString(args, "zone")
			if err != nil {
				return "", err
			}
			id, err := argString(args, "id")
			if err != nil {
				return "", err
			}
			body := map[string]any{}
			if _, ok := args["content"]; ok {
				content, err := argString(args, "content")
				if err != nil {
					return "", err
				}
				body["content"] = content
			}
			if _, ok := args["proxied"]; ok {
				proxied, err := argBool(args, "proxied")
				if err != nil {
					return "", err
				}
				body["proxied"] = proxied
			}
			if _, ok := args["name"]; ok {
				name, err := argString(args, "name")
				if err != nil {
					return "", err
				}
				body["name"] = name
			}
			if len(body) == 0 {
				return "", fmt.Errorf("update_dns_record: provide at least one of content, proxied, name")
			}
			b, err := a.call(ctx, "PATCH", "/api/cf/records/"+url.PathEscape(id)+"?zone="+url.QueryEscape(zone), body)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "delete_dns_record",
		Title:       "Delete a DNS record",
		Description: "Delete a Cloudflare DNS record. Omit zone for the default.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"zone": prop("string", "Zone domain, e.g. polardev.org. Omit for the default."),
			"id":   prop("string", "Cloudflare record ID, from list_dns."),
		}, "id"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			// argOptionalString, not argString: an explicit zone:"" is a
			// realistic input from clients that always send every schema key,
			// and the backend (cfZoneFromReq) already treats "" identically
			// to an omitted zone (falls back to the default zone).
			zone, err := argOptionalString(args, "zone")
			if err != nil {
				return "", err
			}
			id, err := argString(args, "id")
			if err != nil {
				return "", err
			}
			b, err := a.call(ctx, "DELETE", "/api/cf/records/"+url.PathEscape(id)+"?zone="+url.QueryEscape(zone), nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})
}
