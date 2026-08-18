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

func registerMCPTools(s *Server, a *apiCaller, allowWrites bool) {
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
			zone := ""
			if _, ok := args["zone"]; ok {
				var err error
				if zone, err = argString(args, "zone"); err != nil {
					return "", err
				}
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
		Description: "Force an immediate check of whether a newer image is available in the registry for this service, instead of waiting for the periodic background poll (runs roughly every 10 minutes). Does not change anything about the running service — only refreshes the cached 'update available' status, which then shows up in list_services.",
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
		}, "service"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			b, err := a.call(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/check", nil)
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
			b, err := a.call(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/scale", map[string]any{"replicas": n})
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
			b, err := a.call(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/"+action, nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "set_autoupdate",
		Title:       "Toggle auto-update",
		Description: "Opt a service in or out of unattended updates when a newer image digest appears. Onboarded services only.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"enabled": prop("boolean", "true to enable unattended updates."),
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
			b, err := a.call(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/autoupdate", map[string]any{"enabled": on})
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
					"keys in env_ack to confirm the overwrite.",
			},
			"env_ack": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Env var names from a prior conflict response that should be " +
					"overwritten. Resubmit the same env plus these names.",
			},
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
			body := map[string]any{"image": image}
			if len(env) > 0 {
				body["env"] = env
			}
			if len(ack) > 0 {
				body["env_ack"] = ack
			}
			b, err := a.call(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/stage", body)
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
					"keys in env_ack to confirm the overwrite.",
			},
			"env_ack": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
				"description": "Env var names from a prior conflict response that should be " +
					"overwritten. Resubmit the same env plus these names.",
			},
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
			body := map[string]any{"image": image}
			if len(env) > 0 {
				body["env"] = env
			}
			if len(ack) > 0 {
				body["env_ack"] = ack
			}
			b, err := a.call(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/replace", body)
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
			switch action {
			case "promote":
				b, err := a.call(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/promote", nil)
				if err != nil {
					return "", err
				}
				return pretty(b), nil
			case "discard":
				b, err := a.call(ctx, "DELETE", "/api/services/"+url.PathEscape(name)+"/canary", nil)
				if err != nil {
					return "", err
				}
				return pretty(b), nil
			}
			return "", fmt.Errorf("action must be \"promote\" or \"discard\", got %q", action)
		},
	})

	s.Register(Tool{
		Name:        "restart_replica",
		Title:       "Start, stop, or restart a replica",
		Description: "Start, stop, or restart one replica of a service (not the whole service — use lifecycle_service for that). member is a name from list_services' member_summaries. restart is stop immediately followed by start.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"member":  prop("string", "Replica name from list_services' member_summaries."),
			"action":  map[string]any{"type": "string", "enum": []string{"start", "stop", "restart"}, "description": "start, stop, or restart"},
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
			base := "/api/services/" + url.PathEscape(name) + "/replicas/" + url.PathEscape(member) + "/"
			switch action {
			case "start", "stop":
				b, err := a.call(ctx, "POST", base+action, nil)
				if err != nil {
					return "", err
				}
				return pretty(b), nil
			case "restart":
				// Abort on stop failure rather than attempting start anyway —
				// a replica already down for another reason shouldn't be
				// force-started as a side effect of a restart request.
				if _, err := a.call(ctx, "POST", base+"stop", nil); err != nil {
					return "", err
				}
				b, err := a.call(ctx, "POST", base+"start", nil)
				if err != nil {
					return "", err
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
			zone := ""
			if _, ok := args["zone"]; ok {
				var err error
				if zone, err = argString(args, "zone"); err != nil {
					return "", err
				}
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
			zone := ""
			if _, ok := args["zone"]; ok {
				var err error
				if zone, err = argString(args, "zone"); err != nil {
					return "", err
				}
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
			zone := ""
			if _, ok := args["zone"]; ok {
				var err error
				if zone, err = argString(args, "zone"); err != nil {
					return "", err
				}
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
