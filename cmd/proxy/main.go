// proxy: request-path only. Reverse proxy + load balancer + health checks.
// Read-only access to the Docker socket. No auth, no management endpoints.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":8092", "proxy listen address")
	metricsAddr := flag.String("metrics-addr", ":8094", "internal metrics endpoint listen address")
	staticConfig := flag.String("config", "/etc/proxy/routes.json", "static routes JSON (ignored if missing)")
	statePath := flag.String("state", "/data/metrics.json", "metrics persistence file")
	stateInterval := flag.Duration("state-interval", 30*time.Second, "how often to snapshot metrics to -state")
	authDomains := flag.String("auth-domains", "", "comma-separated parent domains with an auth.<domain> login host (empty = auth gate disabled)")
	authTrustedCIDRs := flag.String("auth-trusted-cidrs", "", "comma-separated CIDRs that bypass auth entirely (e.g. LAN ranges)")
	authXFFTrustedCIDRs := flag.String("auth-xff-trusted-cidrs", "127.0.0.0/8,172.16.0.0/12", "comma-separated CIDRs of peers whose X-Forwarded-For is trusted")
	authVerifyTokenURL := flag.String("auth-verify-token-url", "http://dashboard:8093/api/auth/verify-token", "dashboard endpoint used to verify bearer API tokens")
	rateLimitGlobal := flag.String("ratelimit-global", "", `whole-proxy aggregate request cap as "rps" or "rps:burst" (empty = disabled)`)
	flag.Parse()

	metrics := NewMetrics()
	if st, ok := loadMetricsState(*statePath); ok {
		metrics.restoreState(st)
		log.Printf("restored metrics state from %s (total=%d, %d host(s), saved %s)",
			*statePath, st.Total, len(st.ByHost), st.SavedAt.Format(time.RFC3339))
	}
	access := NewAccessLog()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go persistLoop(ctx, *statePath, *stateInterval, metrics)
	saveOnShutdown(*statePath, metrics)

	dc := newDockerClient()
	router := &Router{}
	router.limits = newLimiterRegistry()
	if *rateLimitGlobal != "" {
		spec, ok := parseRateSpec(*rateLimitGlobal)
		if !ok {
			// Operator config error — refuse to start rather than run unlimited.
			log.Fatalf(`bad -ratelimit-global %q: want "rps" or "rps:burst" with 1 <= rps <= burst`, *rateLimitGlobal)
		}
		router.global = newTokenBucket(spec, nil)
		log.Printf("global rate limit: %d req/s (burst %d)", spec.RPS, spec.Burst)
	}
	if *authDomains != "" {
		var secret []byte
		if envHex := strings.TrimSpace(os.Getenv("PMGR_AUTH_SECRET")); envHex != "" {
			if b, err := hex.DecodeString(envHex); err == nil {
				secret = b
			} else {
				log.Printf("auth: PMGR_AUTH_SECRET is not valid hex (%v) — protected hosts will fail closed", err)
			}
		}
		router.auth = newAuthGate(secret, *authDomains, *authTrustedCIDRs, *authXFFTrustedCIDRs, *authVerifyTokenURL)
		log.Printf("auth gate enabled for domain(s) %s", *authDomains)
	}
	refresh := func() {
		groups, err := assembleGroups(ctx, dc, *staticConfig)
		if err != nil {
			log.Printf("refresh: %v", err)
			return
		}
		router.Set(groups)
		total := 0
		for _, g := range groups {
			total += len(g.Backends)
		}
		log.Printf("loaded %d route(s), %d backend(s)", len(groups), total)
	}
	refresh()

	// Pass refresh into the metrics server so /refresh can be hit by the
	// dashboard after it edits routes.json — saves a docker restart.
	metricsServer(*metricsAddr, metrics, access, refresh, router.Snapshot)
	log.Printf("metrics on %s/metrics — access log on %s/access", *metricsAddr, *metricsAddr)

	go dc.streamEvents(ctx, func(action string) {
		switch action {
		case "start", "die", "destroy", "kill", "stop":
			refresh()
		}
	})
	go runHealthChecks(ctx, router)

	log.Printf("proxy on %s", *addr)
	handler := withAccessLog(withMetrics(router, metrics), access)
	if err := http.ListenAndServe(*addr, handler); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// unroutedHost is the synthetic metrics bucket for requests that matched no
// route. Keeps attacker-controlled Host headers from growing the per-host maps.
const unroutedHost = "(unrouted)"

// withMetrics wraps the router to record per-request counters + latency. It
// wraps the response in an *accessWriter when one isn't already in place, so
// the access-log layer downstream can reuse the same capture.
func withMetrics(next http.Handler, m *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.InFlight.Add(1)
		defer m.InFlight.Add(-1)
		start := time.Now()
		aw, ok := w.(*accessWriter)
		if !ok {
			aw = &accessWriter{ResponseWriter: w}
		}
		next.ServeHTTP(aw, r)
		host := r.Host
		if i := strings.IndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		if aw.unrouted {
			host = unroutedHost
		}
		if aw.ratelimited {
			m.RecordRateLimited(host)
		}
		status := aw.status
		if status == 0 {
			status = 200
		}
		m.Record(host, r.Method, status, aw.bytes, time.Since(start))
	})
}
