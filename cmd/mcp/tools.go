// Tool surface over the dashboard's HTTP API.
//
// Deliberately goes through the dashboard rather than the Docker socket: the
// API carries guardUnscalable, onboarded-vs-label handling, canary state and —
// importantly — the audit log, so an LLM-driven change is attributable in the
// same place a human one is. Talking to Docker directly would fork all of that.
//
// Mutating tools are only REGISTERED when writes are enabled, so a read-only
// deployment cannot reach one by guessing its name.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dashClient talks to the dashboard with an API token. That token bypasses 2FA
// (the dashboard treats possession as proof of elevation), which is exactly why
// the write half of this server is opt-in.
type dashClient struct {
	base  string
	token string
	http  *http.Client
}

func newDashClient(base, token string) *dashClient {
	return &dashClient{
		base:  strings.TrimSuffix(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *dashClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	// Attribution only. The token above is what authorizes the call; this just
	// tells the dashboard which person to name in the audit record.
	if a := actorFrom(ctx); a != "" {
		req.Header.Set(actorHeader, a)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		// Surfaced verbatim to the model: the dashboard's own messages
		// ("unknown host", "no live replicas") are what let it self-correct.
		return nil, fmt.Errorf("dashboard %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// pretty re-encodes a dashboard response with indentation. Tool output is read
// by a model, so readable beats compact; unparseable bodies pass through as-is
// rather than becoming an error.
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

// registerTools wires the tool surface. allowWrites gates every tool that can
// change anything.
func registerTools(s *Server, c *dashClient, allowWrites bool) {
	// ---- read-only ----

	s.Register(Tool{
		Name:        "list_services",
		Title:       "List services",
		Description: "List every service the proxy manages: image, replica count, running count, host, whether an image update is available, and canary state.",
		InputSchema: schema(map[string]any{}),
		Handler: func(ctx context.Context, _ map[string]any) (string, error) {
			b, err := c.do(ctx, "GET", "/api/services", nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "list_routes",
		Title:       "List routes",
		Description: "List the host/path routes the proxy currently serves and their backends. Includes both container-label routes and static ones.",
		InputSchema: schema(map[string]any{}),
		Handler: func(ctx context.Context, _ map[string]any) (string, error) {
			b, err := c.do(ctx, "GET", "/api/routes", nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "get_logs",
		Title:       "Get container logs",
		Description: "Fetch recent log lines for one container. Use list_services first to get exact container names.",
		InputSchema: schema(map[string]any{
			"container": prop("string", "Exact container name."),
			"tail":      prop("number", "How many trailing lines to return (default 200, max 2000)."),
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
			b, err := c.do(ctx, "GET", "/api/logs/"+url.PathEscape(name)+"?tail="+fmt.Sprint(tail), nil)
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
	})

	s.Register(Tool{
		Name:        "maintenance_status",
		Title:       "Maintenance status",
		Description: "Show which hosts are currently serving the maintenance page, and which have their own custom page.",
		InputSchema: schema(map[string]any{}),
		Handler: func(ctx context.Context, _ map[string]any) (string, error) {
			b, err := c.do(ctx, "GET", "/api/maintenance", nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "list_dns",
		Title:       "List DNS records",
		Description: "List Cloudflare DNS records for a zone. Omit zone to use the default zone.",
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
			b, err := c.do(ctx, "GET", "/api/cf/records?zone="+url.QueryEscape(zone), nil)
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
		Description: "Put a host into maintenance (public visitors get a 503 page) or take it out. Reversible. The host must already be routed by the proxy.",
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
			b, err := c.do(ctx, method, "/api/maintenance/"+url.PathEscape(host), nil)
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
			b, err := c.do(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/scale", map[string]any{"replicas": n})
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "lifecycle_service",
		Title:       "Start or stop a service",
		Description: "Stop all replicas of a service, or start them again. Stopping keeps the containers and their config, so it is reversible.",
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
			b, err := c.do(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/"+action, nil)
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
			b, err := c.do(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/autoupdate", map[string]any{"enabled": on})
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "stage_canary",
		Title:       "Stage a canary",
		Description: "Deploy a new image alongside the live one so traffic is split across both. Nothing is removed. Follow with promote_canary or discard_canary. Preferred over a direct replace: it is reversible.",
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
			b, err := c.do(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/stage", map[string]any{"image": image})
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
				b, err := c.do(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/promote", nil)
				if err != nil {
					return "", err
				}
				return pretty(b), nil
			case "discard":
				b, err := c.do(ctx, "DELETE", "/api/services/"+url.PathEscape(name)+"/canary", nil)
				if err != nil {
					return "", err
				}
				return pretty(b), nil
			}
			return "", fmt.Errorf("action must be \"promote\" or \"discard\", got %q", action)
		},
	})
}
