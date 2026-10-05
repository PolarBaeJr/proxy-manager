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
	"time"
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

// rollingReplaceToolPollInterval is how often a tool re-polls
// GET /api/services/{name}/rolling-replace while a job is running. A var
// only so tests can shrink it.
var rollingReplaceToolPollInterval = 2 * time.Second

// pollRollingOp blocks until the rolling-replace job whose latest state is b
// is terminal (completed or failed), re-polling statusPath, and returns that
// final state — the synchronous contract rolling_replace_service and an
// overlap restart_replica both offer on top of the async job.
func (a *apiCaller) pollRollingOp(ctx context.Context, statusPath string, b []byte) (string, error) {
	deadline := time.Now().Add(rollingOpTimeout)
	for {
		var st rollingOpState
		if err := json.Unmarshal(b, &st); err != nil {
			return "", err
		}
		if st.Status == rollingOpStatusCompleted || st.Status == rollingOpStatusFailed {
			return pretty(b), nil
		}
		if !time.Now().Before(deadline) {
			// The server-side job is itself bounded by rollingOpTimeout, so
			// hitting this client-side cap means the job should already be
			// terminal or about to become so — return the last known state
			// rather than manufacturing an error.
			return pretty(b), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(rollingReplaceToolPollInterval):
		}
		var err error
		b, err = a.call(ctx, "GET", statusPath, nil)
		if err != nil {
			return "", err
		}
	}
}

// abToolPollInterval is how often an A/B tool re-polls
// GET /api/services/{name}/ab while its op is being applied. A var only so
// tests can shrink it.
var abToolPollInterval = 2 * time.Second

// pollABOp blocks until the A/B op whose 202 body is accepted has been
// applied — op cleared, the test gone, or last_error set — re-polling
// statusPath, and returns the final status. It never waits on pending: a
// non-force promote/discard drains for up to max_session, and the
// manager's Run loop finalizes it later. A failed op is an error carrying
// the status, so the model can't mistake it for success.
func (a *apiCaller) pollABOp(ctx context.Context, statusPath string, accepted []byte) (string, error) {
	var acc struct {
		ID string `json:"id"`
		Op string `json:"op"`
	}
	if err := json.Unmarshal(accepted, &acc); err != nil || acc.Op == "" {
		return pretty(accepted), nil
	}
	deadline := time.Now().Add(abOpTimeout)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(abToolPollInterval):
		}
		b, err := a.call(ctx, "GET", statusPath, nil)
		if err != nil {
			return "", err
		}
		var st abStatusView
		if err := json.Unmarshal(b, &st); err != nil {
			return "", err
		}
		out := pretty(stripABHist(b))
		// A start can turn into finalize_discard (B removal after a failed
		// start); any other op change means ours was applied.
		settled := st.State == "none" || st.Op == "" || st.LastError != "" || (acc.Op == abOpStart && st.Op != abOpStart)
		if !settled {
			if time.Now().Before(deadline) {
				continue
			}
			// The server-side op is bounded by abOpTimeout too: return the
			// last known state rather than manufacturing an error.
			return out, nil
		}
		if st.LastError != "" {
			return "", fmt.Errorf("A/B %s failed: %s\n%s", acc.Op, st.LastError, out)
		}
		if acc.Op == abOpStart && st.State == "none" {
			if n := len(st.History); n > 0 && st.History[n-1].Outcome == "start_failed" && (acc.ID == "" || st.History[n-1].ID == acc.ID) {
				return "", fmt.Errorf("A/B test failed to start: %s\n%s", st.History[n-1].Detail, out)
			}
		}
		return out, nil
	}
}

// stripABHist drops the 64-bucket latency histograms from a GET ab
// body's windows — bulky, and the per-variant p50/p95 already summarize
// them. Falls back to the untouched body on any decode error.
func stripABHist(b []byte) []byte {
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	ws, _ := v["windows"].([]any)
	for _, w := range ws {
		wm, _ := w.(map[string]any)
		vars, _ := wm["variants"].(map[string]any)
		for _, c := range vars {
			if c, ok := c.(map[string]any); ok {
				delete(c, "hist")
			}
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
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

// stripServiceLabels drops each service's raw Labels map before it reaches
// an MCP client. Labels carries every proxy.* AND docker-compose-generated
// label verbatim — by far the biggest thing in a /api/services response once
// there are more than a couple of services — and no MCP tool here reads it
// back. The dashboard UI is a different consumer of the same /api/services
// endpoint and still needs Labels (ui.go checks proxy.autoupdate directly),
// so this only trims the MCP-facing copy, never the underlying API response.
// Falls back to the untouched body on any decode error rather than hiding
// the real response behind a swallowed error.
func stripServiceLabels(b []byte) []byte {
	var svcs []Service
	if err := json.Unmarshal(b, &svcs); err != nil {
		return b
	}
	for i := range svcs {
		svcs[i].Labels = nil
	}
	out, err := json.Marshal(svcs)
	if err != nil {
		return b
	}
	return out
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
		Description: "List every service the proxy manages: image, replica count, running count, host, whether an image update is available, canary state, and (image_check_error) whether the registry/distribution check for its image is currently failing.",
		InputSchema: schema(map[string]any{}),
		Handler: func(ctx context.Context, _ map[string]any) (string, error) {
			b, err := a.call(ctx, "GET", "/api/services", nil)
			if err != nil {
				return "", err
			}
			return pretty(stripServiceLabels(b)), nil
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

	s.Register(Tool{
		Name:  "get_service_env",
		Title: "Get a service's central env",
		Description: "Show a centrally managed service's env by KEY NAME only (values are never returned): " +
			"its origin host, current version, state, which keys are \"ref:NAME\" secret references, " +
			"per-host override keys, each host's version and convergence, the last propagation job, and " +
			"last_failure — the most recent propagation that did not converge (reverted, rolled back, " +
			"partial, degraded), kept even after later jobs. " +
			"managed=false means the service's env is still per-host.",
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"host":    prop("string", "Optional host identity (see \"machine\" in list_services) to ask instead of this dashboard. If omitted and this dashboard doesn't know the service, peers are asked automatically. Requires MCP_ALLOW_PEER_WRITES."),
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
			b, err := a.call(ctx, "GET", withHost("/api/services/"+url.PathEscape(name)+"/env", host), nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:  "get_service_labels",
		Title: "Get a service's central labels",
		Description: "Show a service's central (Redis-managed) proxy.* labels: managed (adopted or not), version " +
			"(pass it as if_version to set_service_labels), labels (the central map), effective (what is in force), " +
			"container_labels per host and member (raw), drift (container labels the central map overrides — compose " +
			"edits to managed keys are ignored once adopted), import_preview + conflicts (when not adopted yet), " +
			"readonly_keys (labels that can only change by recreating), live_keys, store status, and applied: each " +
			"host's proxy overlay version/source and whether it has the service. A change is live once every " +
			"proxy's applied.version is at least the global_version after the write (~5s). Live keys: " + strings.Join(managedLabelKeys, ", ") + ".",
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"host":    prop("string", "Optional host identity (see \"machine\" in list_services) whose own containers/proxy to report instead of this dashboard's full view. Requires MCP_ALLOW_PEER_WRITES."),
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
			b, err := a.call(ctx, "GET", withHost("/api/services/"+url.PathEscape(name)+"/labels", host), nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:  "get_ab_test",
		Title: "Get a service's A/B test",
		Description: "Show a service's A/B test (B = canary replicas on a different image, A = the live replicas): " +
			"state (active | none), id, image, replicas, phase (running | aborted | promoting | discarding), " +
			"pending (promote | discard waiting for the old variant's pinned sessions to drain — up to max_session, " +
			"default 24h — after which the dashboard finalizes it), op (a change still being applied) and last_error, " +
			"config (the proxy.ab.* labels), drain, stats (A and B: requests, errors, error_rate, p50_ms, p95_ms, " +
			"merged across hosts), per_host (reachability, stats, pinned sessions, and judge status: warmup, " +
			"min_runtime, insufficient_samples — auto-abort can't fire yet, fewer than proxy.ab.min_samples " +
			"(default 500) requests per variant — ok, or abort), abort, hint, env_pending_note, and the history " +
			"of past tests. Per-window latency histograms are omitted.",
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"host":    prop("string", "Optional host identity (see \"machine\" in list_services) whose test to read instead of this dashboard's. Requires MCP_ALLOW_PEER_WRITES."),
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
			b, err := a.call(ctx, "GET", withHost("/api/services/"+url.PathEscape(name)+"/ab", host), nil)
			if err != nil {
				return "", err
			}
			return pretty(stripABHist(b)), nil
		},
	})

	// Registered read-only: its default (and, without both write opt-ins,
	// only) mode is a dry run that changes nothing. Executing is refused in
	// the handler unless MCP_ALLOW_WRITES and MCP_ALLOW_PEER_WRITES are both
	// on — an adopt recreates the service on every host.
	s.Register(Tool{
		Name:     "adopt_service_env",
		Title:    "Adopt a service into central env",
		Mutating: allowWrites && allowPeerWrites,
		Description: "Bring a label-managed service (often compose-created) under central env management. Its origin " +
			"is the host arg, or THIS dashboard's host if omitted; the adopt runs on that host, which must run the " +
			"service. Defaults to a DRY RUN that changes nothing and returns a report by KEY NAME " +
			"only (values are never returned): blockers, warnings, required_acks/missing_acks, a per-host env diff " +
			"(peer_only keys a peer would lose, origin_only keys it would gain, keys whose values differ, host-local " +
			"looking keys), members per host, compose ownership + retire_steps, and a fingerprint. To execute, pass " +
			"dry_run:false with that fingerprint, every required ack, and every peer_only key either in import " +
			"({host:{keys:[...], as:\"base\"|\"override\"}}) or accept_dropped. Execution needs MCP_ALLOW_WRITES and " +
			"MCP_ALLOW_PEER_WRITES, recreates every replica on every host (origin first, health-gated), strips compose " +
			"labels, and is undone automatically if the origin's first roll fails. Watch it with get_service_env.",
		InputSchema: schema(map[string]any{
			"service":            prop("string", "Service name from list_services."),
			"host":               prop("string", "Optional host identity (see \"machine\" in list_services) to become the ORIGIN instead of this dashboard's host. Use the SAME host for the dry run and the execute — a fingerprint is only valid on the origin that produced it. Requires MCP_ALLOW_PEER_WRITES."),
			"dry_run":            prop("boolean", "Default true. false executes the adopt."),
			"fingerprint":        prop("string", "The fingerprint from the dry run this execute is based on (required to execute)."),
			"request_id":         prop("string", "Optional idempotency key; reuse it to retry an execute whose outcome was unknown."),
			"ack_no_health":      prop("boolean", "Acknowledge the service has no healthcheck (only the container staying up gates the roll)."),
			"ack_compose":        prop("boolean", "Acknowledge the compose_owned warning (compose labels are stripped; never compose up/down/pull those services again)."),
			"ack_restart_policy": prop("boolean", "Acknowledge restart policies are normalized to unless-stopped."),
			"ack_image_update":   prop("boolean", "Acknowledge the roll also moves replicas onto a newer, already-pulled image."),
			"import": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"keys": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"as":   map[string]any{"type": "string", "enum": []string{"base", "override"}},
					},
				},
				"description": "Per peer host: key NAMES to take from that host's live env — as base (every host) or override (that host only). Values are fetched server-side, never through the model.",
			},
			"accept_dropped": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "peer_only key names you accept will be dropped from the peers that have them.",
			},
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
			req, err := argAdoptRequest(args)
			if err != nil {
				return "", err
			}
			if !req.dryRun() && (!allowWrites || !allowPeerWrites) {
				return "", fmt.Errorf("executing an adopt restarts %s on every host — it needs MCP_ALLOW_WRITES=true and MCP_ALLOW_PEER_WRITES=true (a dry run works without them)", name)
			}
			b, err := a.call(ctx, "POST", withHost("/api/services/"+url.PathEscape(name)+"/env/adopt", host), req)
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

	// Deliberately a separate tool rather than a flag on scale_service: there,
	// "host" means "the copy that already lives on that host", and overloading
	// it to also mean "put a copy there" is exactly the ambiguity an
	// LLM-driven caller resolves wrongly. Once spread_service has seeded a
	// peer, scale_service's existing host parameter adjusts the peer's replica
	// count through the unchanged path.
	s.Register(Tool{
		Name:        "spread_service",
		Title:       "Scale a service onto another host",
		Description: "Place replicas of a service on a DIFFERENT host as members of the SAME logical service — same proxy.service identity, same route, load-balanced across both hosts. Refused for singleton services, services with volumes or bind mounts, and services whose env names a host-local database address. Requires MCP_ALLOW_PEER_WRITES.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service":               prop("string", "Service name from list_services, as it exists on THIS host."),
			"target":                prop("string", "Peer hostname/identity (see the \"machine\" field returned by list_services) to place replicas on."),
			"replicas":              prop("number", "How many replicas to run on the target host (1-10, default 1)."),
			"allow_unreachable_env": prop("boolean", "Override the refusal when the service's env names a host-local address the replica could not reach. Only set this after confirming the address is routable from the target host."),
		}, "service", "target"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			// Routed through hostArg purely for its MCP_ALLOW_PEER_WRITES
			// gate — spreading always writes to another host.
			target, err := hostArg(args, "target", allowPeerWrites)
			if err != nil {
				return "", err
			}
			if target == "" {
				return "", fmt.Errorf("target is required")
			}
			body := map[string]any{"target": target}
			if _, ok := args["replicas"]; ok {
				n, err := argInt(args, "replicas")
				if err != nil {
					return "", err
				}
				body["replicas"] = n
			}
			if _, ok := args["allow_unreachable_env"]; ok {
				allow, err := argBool(args, "allow_unreachable_env")
				if err != nil {
					return "", err
				}
				body["allow_unreachable_env"] = allow
			}
			b, err := a.call(ctx, "POST", "/api/services/"+url.PathEscape(name)+"/spread", body)
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

	// Deliberately stays on the synchronous POST /api/services/{name}/replace
	// endpoint, not the newer async /rolling-replace one (see rollingop.go) —
	// an MCP caller expects this call to return only once the replace has
	// actually happened, the same contract autoupdate.go's direct
	// replaceService call relies on.
	s.Register(Tool{
		Name:  "replace_service",
		Title: "Replace a service's image directly",
		Description: "Replace a service's running image directly — NOT reversible (old " +
			"containers are torn down immediately). Optionally edits env vars using the same " +
			"merge/conflict rules as stage_canary. Prefer stage_canary + resolve_canary when " +
			"you want a reversible rollout; use this only when a direct swap is intended. " +
			"This path is NOT health-gated — new containers are created, given a brief fixed " +
			"settle delay, then the old ones are torn down unconditionally, even if the new " +
			"ones never became healthy. On a multi-replica service this can zero its capacity. " +
			"Prefer rolling_replace_service, which health-gates each replica one at a time and " +
			"never drops capacity, for any production multi-replica service. Exception: on a " +
			"proxy.overlap service (overlap: true in list_services) this IS health-gated — the " +
			"new copy must pass its health check before the old one is drained, and is removed " +
			"(old one untouched) if it never does.",
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
		Name:  "rolling_replace_service",
		Title: "Replace a service's image one replica at a time",
		Description: "Replace a service's running image via a health-gated, surge-of-one " +
			"rolling replace: each replica is created and health-checked BEFORE its " +
			"predecessor is torn down, so capacity never drops. Refuses to start if it would " +
			"leave the service with fewer than " + fmt.Sprint(minHealthyReplicasForRollingReplace) +
			" healthy replicas across every host (a single-replica service on one host alone " +
			"cannot pass this — spread it across hosts first, or use replace_service if you " +
			"accept that risk). A proxy.overlap singleton (overlap: true in list_services) is " +
			"exempt: it is replaced by health-gated overlap instead. The job itself runs asynchronously on the dashboard, but this " +
			"tool call blocks and polls until it actually finishes (completed or failed) before " +
			"returning, matching the synchronous contract replace_service documents. Like " +
			"replace_service, this is NOT reversible.",
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
					"literal value here — it would land in this tool call and in your transcript. " +
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
			path := "/api/services/" + url.PathEscape(name) + "/rolling-replace"
			b, err := a.call(ctx, "POST", withHost(path, host), body)
			if err != nil {
				return "", err
			}

			return a.pollRollingOp(ctx, withHost(path, host), b)
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

	// The A/B tools POST to the async /ab endpoints and then poll GET ab
	// until the change is applied (pollABOp). The backend validators run
	// first here too, so a bad argument never reaches the dashboard.
	abBusyNote := " The A/B lock is held until the change is applied, so an op sent right after another can briefly fail with \"already in progress\" — retry it."
	abHostProp := prop("string", "Optional peer hostname/identity (see the \"machine\" field returned by list_services) to target a service on a DIFFERENT dashboard host instead of this one. Requires MCP_ALLOW_PEER_WRITES.")
	abGroupsProp := func(desc string) map[string]any {
		return map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string", "enum": []string{"A", "B"}},
			"description":          desc,
		}
	}
	abStrings := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}

	s.Register(Tool{
		Name:  "start_ab_test",
		Title: "Start an A/B test",
		Description: "Start an A/B test on a label-managed service: B is a canary with a DIFFERENT IMAGE (image-only — " +
			"env is refused; build a different image for B), A is the live replicas. New sessions are split by account " +
			"(ab_uid / ab_group cookies the app sets), a signed-out visitor's ab_anon cookie, a header, or randomly, and " +
			"each session stays pinned to its variant. The proxy records per-variant requests, error rate and p50/p95, " +
			"and auto-aborts (new sessions back to A) when B is clearly worse. The QA override ?pm_variant= is off unless " +
			"override is true. This call blocks while B is created and passes its health gate, then returns get_ab_test's " +
			"status." + abBusyNote,
		Mutating: true,
		InputSchema: schema(map[string]any{
			"service":      prop("string", "Service name from list_services."),
			"image":        prop("string", "B's full image reference including tag."),
			"replicas":     prop("number", "B replicas, 1.."+fmt.Sprint(abMaxReplicas)+" (default 1)."),
			"split":        prop("number", "Percent of ungrouped and signed-out sessions sent to B, 0..100 (default 10)."),
			"assign":       map[string]any{"type": "string", "enum": []string{"cookie", "random", "header"}, "description": "cookie (default, browsers), random (per request), or header (API clients: hash of the header named by header)."},
			"header":       prop("string", "Header name to hash, only with assign=header."),
			"groups":       abGroupsProp("Group name -> \"A\" or \"B\", matched against the app's ab_group cookie (names a-z 0-9 _ -, 1..32). Grouped accounts skip the split."),
			"uid_cookie":   prop("string", "Cookie holding the app's opaque account hash (default ab_uid)."),
			"group_cookie": prop("string", "Cookie holding the app's group name (default ab_group)."),
			"anon":         prop("boolean", "Split signed-out visitors by a per-browser ab_anon cookie (default true); false draws per session."),
			"session_idle": prop("string", "A pinned session ends after this long idle, 5m..24h (default 30m)."),
			"pin_refresh":  prop("string", "How often the pin cookie is refreshed, 1m..session_idle/2 (default 5m)."),
			"max_session":  prop("string", "Longest a session keeps its variant, and the drain deadline, 1h..168h (default 24h)."),
			"exclude":      abStrings("Path prefixes that always go to A, unpinned and uncounted (e.g. /api/cron, /api/webhooks, /api/health)."),
			"static":       abStrings("Static path prefixes retried on the other variant after a 404 (default /_next/static/); [\"none\"] disables."),
			"cookie_js":    prop("boolean", "Also set a non-HttpOnly ab_vjs_ cookie holding A or B for client-side code (default false)."),
			"override":     prop("boolean", "Allow the ?pm_variant=A|B QA override (default false — prefer a qa:B group)."),
			"thresholds": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"autoabort":          prop("boolean", "Auto-abort on (default true)."),
					"min_samples":        prop("number", "Requests per variant before judging starts (default 500)."),
					"min_runtime":        prop("string", "Runtime before judging starts, 0..24h (default 15m)."),
					"warmup":             prop("string", "Warm-up after start or a split/groups change, 0..1h (default 3m)."),
					"window":             prop("string", "Judging window, 1m..1h (default 5m)."),
					"windows":            prop("number", "Consecutive bad windows that abort, 1..12 (default 2)."),
					"window_min_samples": prop("number", "Requests per variant for a window to count (default 50)."),
					"err_delta":          prop("number", "Error-rate gap in percentage points, 0.1..100 (default 2)."),
					"err_ratio":          prop("number", "Error-rate ratio B/A, 1..100 (default 2)."),
					"p95_ratio":          prop("number", "p95 latency ratio B/A, 1..100 (default 1.5)."),
					"p95_slack":          prop("string", "Extra p95 allowance, 0..60s (default 200ms)."),
				},
				"additionalProperties": false,
				"description":          "Auto-abort tuning; omit for the defaults.",
			},
			"host": abHostProp,
		}, "service", "image"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			if _, ok := args["env"]; ok {
				return "", fmt.Errorf("env is not supported in an A/B test (B is image-only in v1) — build a different image for B")
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			req, err := argABStart(args)
			if err != nil {
				return "", err
			}
			if _, err := abConfigLabels(req); err != nil {
				return "", err
			}
			path := withHost("/api/services/"+url.PathEscape(name)+"/ab", host)
			b, err := a.call(ctx, "POST", path, req)
			if err != nil {
				return "", err
			}
			return a.pollABOp(ctx, path, b)
		},
	})

	s.Register(Tool{
		Name:        "set_ab_split",
		Title:       "Change an A/B test's split",
		Description: "Change the percent of ungrouped and signed-out sessions sent to B. Applies to NEW sessions only (pinned sessions keep their variant), restarts the warm-up, and recreates B with the new label (health-gated). Allowed while running or aborted." + abBusyNote,
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"split":   prop("number", "Percent to B, 0..100."),
			"host":    abHostProp,
		}, "service", "split"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			split, err := argInt(args, "split")
			if err != nil {
				return "", err
			}
			if err := abCheckInt("split", split, 0, 100); err != nil {
				return "", err
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			base := "/api/services/" + url.PathEscape(name) + "/ab"
			b, err := a.call(ctx, "POST", withHost(base+"/split", host), struct {
				Split int `json:"split"`
			}{split})
			if err != nil {
				return "", err
			}
			return a.pollABOp(ctx, withHost(base, host), b)
		},
	})

	s.Register(Tool{
		Name:        "set_ab_groups",
		Title:       "Change an A/B test's groups",
		Description: "Replace the group -> variant map (matched against the app's ab_group cookie). Affects NEW sessions only, restarts the warm-up, and recreates B with the new label (health-gated). Allowed while running or aborted." + abBusyNote,
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"groups":  abGroupsProp("The complete new map, group name -> \"A\" or \"B\" (names a-z 0-9 _ -, 1..32). {} clears it."),
			"host":    abHostProp,
		}, "service", "groups"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			if _, ok := args["groups"]; !ok {
				return "", fmt.Errorf("missing required argument %q", "groups")
			}
			groups, err := argABGroups(args, "groups")
			if err != nil {
				return "", err
			}
			if err := abValidateGroups(groups); err != nil {
				return "", err
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			base := "/api/services/" + url.PathEscape(name) + "/ab"
			b, err := a.call(ctx, "POST", withHost(base+"/groups", host), map[string]any{"groups": groups})
			if err != nil {
				return "", err
			}
			return a.pollABOp(ctx, withHost(base, host), b)
		},
	})

	s.Register(Tool{
		Name:  "resolve_ab_test",
		Title: "Promote, discard, abort or reset an A/B test",
		Description: "End or steer an A/B test. promote: new sessions go to B; once A's pinned sessions drain (idle, or " +
			"max_session — default 24h) the dashboard makes B the live set. Promote finalize first rolls every PEER host " +
			"running the service onto B's image (health-gated; a peer needs -peer-writes, else the promote stays pending " +
			"with last_error). An aborted B needs confirm_aborted. discard: new sessions go to A; B is removed once its " +
			"sessions drain. Without force, promote and discard return once the phase has flipped (pending is set) and " +
			"finalize later; force skips the drain and finalizes now, cutting pinned sessions over. abort: new sessions go " +
			"to A, B stays up (pins keep their variant). reset: a new test id with fresh stats, invalidating every pin; " +
			"needs B drained unless force." + abBusyNote,
		Mutating: true,
		InputSchema: schema(map[string]any{
			"service":         prop("string", "Service name from list_services."),
			"action":          map[string]any{"type": "string", "enum": []string{"promote", "discard", "abort", "reset"}, "description": "promote, discard, abort or reset"},
			"force":           prop("boolean", "promote/discard/reset only: skip the drain (default false)."),
			"confirm_aborted": prop("boolean", "promote only: promote a B that was aborted (default false)."),
			"host":            abHostProp,
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
			var force, confirm bool
			if _, ok := args["force"]; ok {
				if force, err = argBool(args, "force"); err != nil {
					return "", err
				}
			}
			if _, ok := args["confirm_aborted"]; ok {
				if confirm, err = argBool(args, "confirm_aborted"); err != nil {
					return "", err
				}
			}
			var body any
			switch action {
			case "promote":
				body = struct {
					Force          bool `json:"force"`
					ConfirmAborted bool `json:"confirm_aborted"`
				}{force, confirm}
			case "discard", "reset":
				if confirm {
					return "", fmt.Errorf("confirm_aborted only applies to promote")
				}
				body = struct {
					Force bool `json:"force"`
				}{force}
			case "abort":
				if force || confirm {
					return "", fmt.Errorf("abort takes no force or confirm_aborted")
				}
			default:
				return "", fmt.Errorf("action must be \"promote\", \"discard\", \"abort\" or \"reset\", got %q", action)
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			base := "/api/services/" + url.PathEscape(name) + "/ab"
			b, err := a.call(ctx, "POST", withHost(base+"/"+action, host), body)
			if err != nil {
				return "", err
			}
			return a.pollABOp(ctx, withHost(base, host), b)
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
		Description: "Start, stop, or restart one replica of a service (not the whole service — use lifecycle_service for that). member must be a NON-canary replica name from list_services' member_summaries (one where is_canary is false/absent) — canary replicas can't be managed here, use resolve_canary instead. restart is normally stop immediately followed by start; on a service's only running (non-canary) replica this causes a brief window with no live backend for that host, so check member_summaries/replicas count first if that matters. Exception: on a proxy.overlap service (list_services shows overlap: true — an unscalable singleton) restart instead creates a new copy from the image reference the service already runs (whatever that tag points to locally, not re-pulled), waits for it to pass its health check, then drains the old one — no downtime. That runs as a job this call blocks on (it returns the final rolling-replace state; status failed with last_error means the new copy never became healthy and was removed while the old one kept serving). It refuses a service with anonymous volumes or with no HEALTHCHECK/proxy.health rather than falling back to stop/start.",
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
				b, err := a.call(ctx, "POST", withHost(base+"restart", host), nil)
				if err == nil {
					var r struct {
						Mode string `json:"mode"`
					}
					if json.Unmarshal(b, &r) == nil && r.Mode == "overlap" {
						return a.pollRollingOp(ctx, withHost("/api/services/"+url.PathEscape(name)+"/rolling-replace", host), b)
					}
					return pretty(b), nil
				}
				// Only a route-level 404 means an older peer without the
				// restart endpoint; "replica not found" is a real answer.
				if !strings.Contains(err.Error(), ": 404 404 page not found") {
					return "", err
				}
				// Abort on stop failure rather than attempting start anyway —
				// a replica already down for another reason shouldn't be
				// force-started as a side effect of a restart request.
				if _, err := a.call(ctx, "POST", withHost(base+"stop", host), nil); err != nil {
					return "", err
				}
				b, err = a.call(ctx, "POST", withHost(base+"start", host), nil)
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

	// A central env edit restarts the service on EVERY host it runs on, so it
	// needs the cross-host opt-in on top of MCP_ALLOW_WRITES.
	if !allowPeerWrites {
		return
	}

	s.Register(Tool{
		Name:  "set_service_env",
		Title: "Edit a service's central env",
		Description: "Edit a centrally managed service's env (see get_service_env). The change lands on its " +
			"origin as a new version and then rolls every replica on every host, health-gated one at " +
			"a time: the origin first, then each peer. A failing origin reverts automatically; a " +
			"failing peer rolls back just that host. Pass if_version from get_service_env — a stale " +
			"one is refused with the current version. Credential-looking keys (TOKEN, SECRET, " +
			"PASSWORD, KEY, ...) must be given as \"ref:NAME\", never a literal value. Returns key names " +
			"and the new version only.",
		Mutating: true,
		InputSchema: schema(map[string]any{
			"service":    prop("string", "Service name from list_services."),
			"host":       prop("string", "Optional host identity (see \"machine\" in list_services) to ask instead of this dashboard. If omitted and this dashboard doesn't know the service, peers are asked automatically. Requires MCP_ALLOW_PEER_WRITES."),
			"if_version": prop("number", "The version this edit is based on, from get_service_env."),
			"request_id": prop("string", "Optional idempotency key. Reuse the same one to retry a call whose outcome was unknown (timeout) — it is applied at most once."),
			"set": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "Keys to set (name -> value) for every host. For a real secret pass \"ref:NAME\"; " +
					"the dashboard resolves NAME from the service's secrets file server-side.",
			},
			"unset": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Key names to remove for every host.",
			},
			"host_overrides": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type": "object", "additionalProperties": map[string]any{"type": "string"},
				},
				"description": "Per-host overrides: host identity -> {name -> value}, layered over the shared keys on that host only.",
			},
			"unset_host_overrides": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"description":          "Per-host override keys to drop: host identity -> [names].",
			},
		}, "service", "if_version"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			ifVersion, err := argInt(args, "if_version")
			if err != nil {
				return "", err
			}
			if ifVersion < 1 {
				return "", fmt.Errorf("if_version must be >= 1")
			}
			requestID, err := argOptionalString(args, "request_id")
			if err != nil {
				return "", err
			}
			set, err := argEnvEdits(args, "set")
			if err != nil {
				return "", err
			}
			unset, err := argStringSlice(args, "unset")
			if err != nil {
				return "", err
			}
			overrides, err := argHostEnvEdits(args, "host_overrides")
			if err != nil {
				return "", err
			}
			unsetOverrides, err := argHostKeyLists(args, "unset_host_overrides")
			if err != nil {
				return "", err
			}
			if err := refuseLiteralCredentials(set); err != nil {
				return "", err
			}
			for _, kv := range overrides {
				if err := refuseLiteralCredentials(kv); err != nil {
					return "", err
				}
			}
			body := centralEnvSetRequest{RequestID: requestID, IfVersion: uint64(ifVersion), Set: set, Unset: unset,
				HostOverrides: overrides, UnsetHostOverrides: unsetOverrides}
			b, err := a.call(ctx, "POST", withHost("/api/services/"+url.PathEscape(name)+"/env", host), body)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	// A central label write applies to the service on EVERY host at once
	// (shared Redis), so it sits behind the cross-host opt-in too.
	s.Register(Tool{
		Name:  "set_service_labels",
		Title: "Set a service's central labels",
		Description: "Change a service's live proxy.* labels centrally (Redis). Applies on every host within ~5s with " +
			"NO container restart or recreate; verify with get_service_labels (applied.version on each host). " +
			"Settable keys: " + strings.Join(managedLabelKeys, ", ") + ". Identity keys (proxy.enable, proxy.service, " +
			"proxy.host, proxy.port, proxy.path), auth keys (proxy.auth, proxy.auth.users, proxy.auth.mode) and " +
			"per-replica keys (proxy.canary, proxy.ab.*, ...) are rejected. The first call on a service adopts it: pass " +
			"if_version 0 — its current container labels are imported (resolve any conflicts get_service_labels " +
			"reports via resolve_conflicts, set or unset); after that, container/compose edits to these keys are " +
			"ignored. Later calls pass if_version = the version from get_service_labels (a stale one is refused with " +
			"the current version). proxy.drop_headers is tighten-only unless allow_loosen. proxy.weight is refused " +
			"while a canary is staged.",
		Mutating: true,
		InputSchema: schema(map[string]any{
			"service":    prop("string", "Service name from list_services."),
			"host":       prop("string", "Optional host identity (see \"machine\" in list_services) to run the write on (its Docker state is used for the guards and the import). Labels are shared, so the change applies everywhere regardless."),
			"if_version": prop("number", "The version this edit is based on, from get_service_labels; 0 = first adopt/import."),
			"request_id": prop("string", "Optional idempotency key. Reuse the same one to retry a call whose outcome was unknown (timeout) — it is applied at most once."),
			"set": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description":          "Keys to set (name -> value), e.g. {\"proxy.health\": \"/healthz\"}.",
			},
			"unset": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Key names to unset (the proxy/dashboard default applies).",
			},
			"allow_loosen": prop("boolean", "Allow removing headers from proxy.drop_headers."),
			"resolve_conflicts": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description":          "First adopt only: key -> value to import for a key the hosts/replicas disagree on (\"\" = unset).",
			},
		}, "service", "if_version"),
		Handler: func(ctx context.Context, args map[string]any) (string, error) {
			name, err := argString(args, "service")
			if err != nil {
				return "", err
			}
			host, err := hostArg(args, "host", allowPeerWrites)
			if err != nil {
				return "", err
			}
			ifVersion, err := argInt(args, "if_version")
			if err != nil {
				return "", err
			}
			if ifVersion < 0 {
				return "", fmt.Errorf("if_version must be >= 0")
			}
			requestID, err := argOptionalString(args, "request_id")
			if err != nil {
				return "", err
			}
			set, err := argEnvEdits(args, "set")
			if err != nil {
				return "", err
			}
			unset, err := argStringSlice(args, "unset")
			if err != nil {
				return "", err
			}
			resolve, err := argEnvEdits(args, "resolve_conflicts")
			if err != nil {
				return "", err
			}
			allowLoosen := false
			if _, ok := args["allow_loosen"]; ok {
				if allowLoosen, err = argBool(args, "allow_loosen"); err != nil {
					return "", err
				}
			}
			body := labelsSetRequest{IfVersion: uint64(ifVersion), RequestID: requestID, Set: set, Unset: unset, AllowLoosen: allowLoosen, ResolveConflicts: resolve}
			b, err := a.call(ctx, "POST", withHost("/api/services/"+url.PathEscape(name)+"/labels", host), body)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:  "release_service_env",
		Title: "Release a service from central env",
		Description: "Undo an adopt: on the service's origin, recreate every replica on every host (origin first, " +
			"health-gated) from its current env but without the central-env stamp, drop each peer's cached copy, then " +
			"delete the central record — the service's env is per-host again. If any host fails, the service stays " +
			"\"releasing\" (creates keep working) and calling this again resumes. Watch it with get_service_env. " +
			"Runs on the service's origin; called on any other host it is forwarded there automatically.",
		Mutating: true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"host":    prop("string", "Optional: the origin you expect. Refused (409) if it isn't the service's origin; never redirects."),
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
			b, err := a.call(ctx, "POST", withHost("/api/services/"+url.PathEscape(name)+"/env/release", host), nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})

	s.Register(Tool{
		Name:        "sync_service_env",
		Title:       "Re-sync a service's central env",
		Description: "Re-run propagation of a centrally managed service's current env version to every host (a no-op for hosts already converged). Use after a host was down or a peer rolled back. Watch progress with get_service_env.",
		Mutating:    true,
		InputSchema: schema(map[string]any{
			"service": prop("string", "Service name from list_services."),
			"host":    prop("string", "Optional host identity (see \"machine\" in list_services) to ask instead of this dashboard. If omitted and this dashboard doesn't know the service, peers are asked automatically. Requires MCP_ALLOW_PEER_WRITES."),
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
			b, err := a.call(ctx, "POST", withHost("/api/services/"+url.PathEscape(name)+"/env/sync", host), nil)
			if err != nil {
				return "", err
			}
			return pretty(b), nil
		},
	})
}

// argAdoptRequest reads adopt_service_env's arguments. dry_run defaults to
// true: only an explicit false executes.
func argAdoptRequest(args map[string]any) (centralEnvAdoptRequest, error) {
	var req centralEnvAdoptRequest
	dry := true
	if _, ok := args["dry_run"]; ok {
		v, err := argBool(args, "dry_run")
		if err != nil {
			return req, err
		}
		dry = v
	}
	req.DryRun = &dry
	var err error
	if req.Fingerprint, err = argOptionalString(args, "fingerprint"); err != nil {
		return req, err
	}
	if req.RequestID, err = argOptionalString(args, "request_id"); err != nil {
		return req, err
	}
	for key, dst := range map[string]*bool{"ack_no_health": &req.AckNoHealth, "ack_compose": &req.AckCompose,
		"ack_restart_policy": &req.AckRestartPolicy, "ack_image_update": &req.AckImageUpdate} {
		if _, ok := args[key]; ok {
			if *dst, err = argBool(args, key); err != nil {
				return req, err
			}
		}
	}
	if req.AcceptDropped, err = argStringSlice(args, "accept_dropped"); err != nil {
		return req, err
	}
	if raw, ok := args["import"]; ok {
		b, _ := json.Marshal(raw)
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req.Import); err != nil {
			return req, fmt.Errorf("argument \"import\" must be {host: {keys: [names], as: \"base\"|\"override\"}}")
		}
	}
	return req, nil
}

// argABStart reads start_ab_test's arguments into the typed request, so
// nothing outside abStartRequest (env above all) can reach the wire.
// Bounds are checked by the caller with the backend's abConfigLabels.
func argABStart(args map[string]any) (abStartRequest, error) {
	req := abStartRequest{Replicas: 1}
	var err error
	if req.Image, err = argString(args, "image"); err != nil {
		return req, err
	}
	if _, ok := args["replicas"]; ok {
		if req.Replicas, err = argInt(args, "replicas"); err != nil {
			return req, err
		}
	}
	if _, ok := args["split"]; ok {
		split, err := argInt(args, "split")
		if err != nil {
			return req, err
		}
		req.Split = &split
	}
	for key, dst := range map[string]*string{"assign": &req.Assign, "header": &req.Header, "uid_cookie": &req.UIDCookie,
		"group_cookie": &req.GroupCookie, "session_idle": &req.SessionIdle, "pin_refresh": &req.PinRefresh, "max_session": &req.MaxSession} {
		if *dst, err = argOptionalString(args, key); err != nil {
			return req, err
		}
	}
	if req.Groups, err = argABGroups(args, "groups"); err != nil {
		return req, err
	}
	if _, ok := args["anon"]; ok {
		anon, err := argBool(args, "anon")
		if err != nil {
			return req, err
		}
		req.Anon = &anon
	}
	for key, dst := range map[string]*bool{"cookie_js": &req.CookieJS, "override": &req.Override} {
		if _, ok := args[key]; ok {
			if *dst, err = argBool(args, key); err != nil {
				return req, err
			}
		}
	}
	if req.Exclude, err = argStringSlice(args, "exclude"); err != nil {
		return req, err
	}
	if req.Static, err = argStringSlice(args, "static"); err != nil {
		return req, err
	}
	if raw, ok := args["thresholds"]; ok {
		b, _ := json.Marshal(raw)
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req.Thresholds); err != nil {
			return req, fmt.Errorf("argument \"thresholds\" must be an object of autoabort, min_samples, min_runtime, warmup, window, windows, window_min_samples, err_delta, err_ratio, p95_ratio, p95_slack: %v", err)
		}
	}
	return req, nil
}

// argABGroups reads an optional {group name: "A"|"B"} object. Names and
// values are validated by the caller with abValidateGroups.
func argABGroups(args map[string]any, key string) (map[string]string, error) {
	v, ok := args[key]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("argument %q must be an object of group name -> \"A\" or \"B\"", key)
	}
	out := make(map[string]string, len(m))
	for name, raw := range m {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("argument %q: value for %q must be \"A\" or \"B\"", key, name)
		}
		out[name] = s
	}
	return out, nil
}

// refuseLiteralCredentials rejects a literal value for any key that looks
// like a credential: through MCP it would sit in the model's transcript.
// Names only in the error.
func refuseLiteralCredentials(edits map[string]string) error {
	for k, v := range edits {
		if strings.HasPrefix(v, secretRefPrefix) {
			continue
		}
		if len(credentialEnvKeys([]string{k + "="})) > 0 {
			return fmt.Errorf("%s looks like a credential — pass \"ref:NAME\" (resolved server-side from the service's secrets file), never the literal value", k)
		}
	}
	return nil
}

// argHostEnvEdits reads {host: {name: value}}.
func argHostEnvEdits(args map[string]any, key string) (map[string]map[string]string, error) {
	v, ok := args[key]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("argument %q must be an object", key)
	}
	out := make(map[string]map[string]string, len(m))
	for host, raw := range m {
		edits, err := argEnvEdits(map[string]any{key: raw}, key)
		if err != nil {
			return nil, fmt.Errorf("%s (host %s)", err, host)
		}
		out[strings.TrimSpace(host)] = edits
	}
	return out, nil
}

// argHostKeyLists reads {host: [name, ...]}.
func argHostKeyLists(args map[string]any, key string) (map[string][]string, error) {
	v, ok := args[key]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("argument %q must be an object", key)
	}
	out := make(map[string][]string, len(m))
	for host, raw := range m {
		keys, err := argStringSlice(map[string]any{key: raw}, key)
		if err != nil {
			return nil, fmt.Errorf("%s (host %s)", err, host)
		}
		out[strings.TrimSpace(host)] = keys
	}
	return out, nil
}
