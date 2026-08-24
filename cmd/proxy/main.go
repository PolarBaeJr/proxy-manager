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

	"github.com/redis/go-redis/v9"
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
	redisAddr := flag.String("redis-addr", "", "shared Redis address for cross-peer rate limiting, e.g. 100.83.62.68:6379 (empty = in-memory-only rate limiting, today's behavior)")
	peers := flag.String("peers", "", "comma-separated peer proxy base URLs for the discovery handshake, e.g. http://100.83.62.68:8094 (empty = disabled)")
	peerSyncInterval := flag.Duration("peer-sync-interval", 5*time.Second, "how often to handshake with peers and push/resync learned routes (matches the proxy's health-check cadence)")
	peerAdvertiseURL := flag.String("peer-advertise-url", "", "this proxy's own base URL as reachable by peers, e.g. http://100.83.62.68:8092 — empty disables route push")
	flag.Parse()
	peerSecret := strings.TrimSpace(os.Getenv("PMGR_PEER_SECRET"))

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
	// Keying rate limits by real client IP must be spoof-resistant even when the
	// auth gate is disabled, so parse the trusted-XFF CIDRs unconditionally.
	router.xffTrusted = parseCIDRList(*authXFFTrustedCIDRs)
	router.unroutedLimiter = newRateLimiter(defaultRateRPM)
	if *redisAddr != "" {
		redisClient := redis.NewClient(&redis.Options{
			Addr:     *redisAddr,
			Password: os.Getenv("REDIS_PASSWORD"),
		})
		router.newLimiter = func(routeKey string, rpm int) limiter {
			return newHybridLimiter(redisClient, routeKey, rpm, idleEvict)
		}
		log.Printf("shared rate limiting enabled via Redis at %s", *redisAddr)
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
		// Attribution secret: lets the proxy tell a backend WHO it authenticated so
		// a service-to-service call can be audited against a person. Deliberately
		// separate from PMGR_AUTH_SECRET — a backend holding this one can verify
		// attributions but cannot forge SSO cookies or OAuth access tokens.
		// Unset simply means no actor header is ever sent.
		var actorSecret []byte
		if envHex := strings.TrimSpace(os.Getenv("PMGR_ACTOR_SECRET")); envHex != "" {
			b, err := hex.DecodeString(envHex)
			if err != nil {
				log.Printf("auth: PMGR_ACTOR_SECRET is not valid hex (%v) — attribution disabled", err)
			} else {
				actorSecret = b
			}
		}
		router.auth = newAuthGate(secret, actorSecret, *authDomains, *authTrustedCIDRs, *authXFFTrustedCIDRs, *authVerifyTokenURL)
		log.Printf("auth gate enabled for domain(s) %s", *authDomains)
	}
	// routeStore holds routes pushed by peers (Phase 3 of
	// docs/PEER_MESH_PLAN.md), overlaid onto freshly-assembled groups on every
	// refresh() below. ttl is a multiple of the sync interval so a route
	// survives a couple of missed pushes before it's dropped.
	routeStore := newPeerRouteStore(3 * *peerSyncInterval)

	refresh := func() {
		groups, err := assembleGroups(ctx, dc, *staticConfig)
		if err != nil {
			log.Printf("refresh: %v", err)
			return
		}
		groups = routeStore.overlay(groups)
		router.Set(groups)
		total := 0
		for _, g := range groups {
			total += len(g.Backends)
		}
		log.Printf("loaded %d route(s), %d backend(s)", len(groups), total)
	}
	refresh()

	// Peer mesh discovery handshake (Phase 2 of docs/PEER_MESH_PLAN.md) and
	// route sync (Phase 3): both only enabled when a shared secret is
	// present.
	identity, err := os.Hostname()
	if err != nil || identity == "" {
		identity = "proxy"
	}
	peerList := splitAndTrim(*peers)
	var ph peerHandlers
	if peerSecret != "" {
		ph.Handshake = peerHandshakeHandler(peerSecret, identity)
		ph.Routes = peerRoutesHandler(peerSecret, routeStore, refresh)

		// Periodic TTL-eviction check: refresh() is otherwise only driven by
		// Docker events (see streamEvents below) and a successful peer route
		// push (see peerRoutesHandler, which skips refresh() on a
		// steady-state push that doesn't add a new route/peer). Without
		// this, a PeerRouteStore entry that ages past its TTL would never
		// actually get evicted from the live router. Deliberately gated on
		// peerSecret alone, NOT on len(peerList) > 0: a receive-only host
		// (secret set, no outbound -peers) still accepts pushes into
		// routeStore via /peer/routes and needs this same eviction —
		// gating it behind outbound peers being configured would leave TTL
		// permanently decorative for exactly that host.
		//
		// Each tick only does a cheap in-memory scan (routeStore.hasExpired,
		// no Docker call) and calls the actual (Docker-listing) refresh()
		// only when that scan finds something past its TTL — refresh() is
		// deliberately event-driven and must not be polled unconditionally
		// against the Docker API just to notice an idle store has nothing to
		// evict.
		go func() {
			t := time.NewTicker(*peerSyncInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if routeStore.hasExpired() {
						refresh()
					}
				}
			}
		}()

		if len(peerList) > 0 {
			registry := newPeerRegistry(peerList, peerSecret, identity, *peerSyncInterval)
			go registry.Run(ctx)
			log.Printf("proxy peers: handshaking with %d peer(s) every %s", len(peerList), *peerSyncInterval)

			if *peerAdvertiseURL != "" {
				routePush := newPeerSync(router, peerList, peerSecret, identity, *peerAdvertiseURL, *peerSyncInterval)
				go routePush.Run(ctx)
				log.Printf("proxy peers: pushing routes to %d peer(s) every %s, advertising %s", len(peerList), *peerSyncInterval, *peerAdvertiseURL)
			} else {
				log.Printf("proxy peers: -peer-advertise-url unset — route push disabled (receive-only)")
			}
		} else {
			log.Printf("proxy peers: /peer/handshake and /peer/routes enabled (receive-only, no outbound peers configured)")
		}
	} else if len(peerList) > 0 {
		log.Printf("proxy peers: peers configured but PMGR_PEER_SECRET empty — handshake disabled")
	}

	// Pass refresh into the metrics server so /refresh can be hit by the
	// dashboard after it edits routes.json — saves a docker restart.
	metricsServer(*metricsAddr, metrics, access, refresh, router.Snapshot, router.RateLimitSnapshot, ph)
	log.Printf("metrics on %s/metrics — access log on %s/access", *metricsAddr, *metricsAddr)

	go dc.streamEvents(ctx, func(action string) {
		switch action {
		case "start", "die", "destroy", "kill", "stop":
			refresh()
			return
		}
		if strings.HasPrefix(action, "health_status") {
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
		status := aw.status
		if status == 0 {
			status = 200
		}
		m.Record(host, r.Method, status, aw.bytes, time.Since(start))
	})
}
