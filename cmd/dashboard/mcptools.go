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
		Name:        "stage_canary",
		Title:       "Stage a canary",
		Description: "Deploy a new image alongside the live one so traffic splits across both. Nothing is removed. Follow with resolve_canary. Preferred over a direct replace because it is reversible.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"image":   prop("string", "Full image reference including tag."),
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
			b, err := a.call(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/stage", map[string]any{"image": image})
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
}
