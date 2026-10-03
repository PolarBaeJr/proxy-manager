// proxy: request-path only. Reverse proxy + load balancer + health checks.
// Read-only access to the Docker socket. No auth, no management endpoints.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/PolarBaeJr/proxy-manager/internal/graceful"
	"github.com/PolarBaeJr/proxy-manager/internal/selfcheck"
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
	healthcheck := flag.Bool("healthcheck", false, "probe this binary's own /healthz endpoints and exit 0/1 (Docker HEALTHCHECK)")
	flag.Parse()
	selfCfg, selfOn := selfcheck.FromEnv("proxy", selfcheck.LoopbackURL(*addr, "/healthz"), selfcheck.LoopbackURL(*metricsAddr, "/healthz"))
	if *healthcheck {
		os.Exit(selfcheck.RunHealthcheck(selfCfg.URLs...))
	}
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
	hopAuth := peerHopAuthToken(peerSecret)
	routeStore.hopAuth = hopAuth
	router.peerHopAuth = hopAuth
	routeStore.abApply = router.applyPeerAB
	router.peerSyncInterval = *peerSyncInterval

	refresh := func() {
		groups, backendsByService, err := assembleGroups(ctx, dc, *staticConfig)
		if err != nil {
			log.Printf("refresh: %v", err)
			return
		}
		groups = routeStore.overlay(groups, backendsByService)
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
	metricsSrv := metricsServer(*metricsAddr, metrics, access, refresh, router.Snapshot, router.RateLimitSnapshot, router.ABReport, ph)
	log.Printf("metrics on %s/metrics — access log on %s/access", *metricsAddr, *metricsAddr)

	go dc.streamEvents(ctx, func(ev dockerEvent) {
		handleContainerEvent(router, refresh, ev)
	})
	go runHealthChecks(ctx, router)
	go router.runABEvaluator(ctx)

	// The watchdog gets its own context so a drain can stop it first: a
	// probe failing while the listener shuts down must not exit(1) mid-drain.
	watchdogCtx, stopWatchdog := context.WithCancel(ctx)
	defer stopWatchdog()
	if selfOn {
		selfcheck.Start(watchdogCtx, selfCfg)
	}

	// No Read/WriteTimeout: proxied uploads, downloads and streams are
	// bounded by the backend, and a server-wide write deadline would cut
	// long SSE/WebSocket sessions.
	srv := &http.Server{Addr: *addr, Handler: mainHandler(router, metrics, access), ReadHeaderTimeout: 5 * time.Second}
	timeout, streamGrace := graceful.TimeoutFromEnv()
	log.Printf("proxy on %s", *addr)
	err = graceful.Run(graceful.Options{
		Primary:     []*http.Server{srv},
		Aux:         []*http.Server{metricsSrv},
		Timeout:     timeout,
		StreamGrace: streamGrace,
		OnSignal: []func(){
			stopWatchdog,
			func() {
				log.Printf("proxy: draining in-flight requests")
				// Early save: if the drain outlives the container's stop
				// grace period, the latest snapshot is already on disk.
				if err := saveMetricsState(*statePath, metrics); err != nil {
					log.Printf("metrics state: shutdown save: %v", err)
				}
			},
		},
		// main's ctx stays alive through the drain (Docker events, health
		// checks and peer sync keep routing correct) and is only cancelled
		// once every request has finished.
		Hooks: []func(context.Context){
			func(context.Context) {
				if err := saveMetricsState(*statePath, metrics); err != nil {
					log.Printf("metrics state: shutdown save: %v", err)
				}
			},
			func(context.Context) { cancel() },
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}

// drainSignals are the kill-event signals that mean "this container is
// going away" (docker stop sends 15 then 9; compose/kill may name them).
// Docker reports them numerically — observed live: {"signal":"15"} — the
// names are accepted defensively. HUP/USR1/USR2/WINCH are app-level
// reload/rotate signals and must not deroute a healthy replica.
var drainSignals = map[string]bool{
	"15": true, "2": true, "3": true, "9": true,
	"SIGTERM": true, "SIGINT": true, "SIGQUIT": true, "SIGKILL": true,
}

// handleContainerEvent reacts to one Docker container event. A stop signal
// marks the container draining BEFORE the refresh, because assembleGroups
// still sees it as running until it actually exits.
func handleContainerEvent(router *Router, refresh func(), ev dockerEvent) {
	id := ev.Actor.ID
	if id == "" {
		id = ev.ID
	}
	switch ev.Action {
	case "kill":
		if drainSignals[strings.ToUpper(ev.Actor.Attributes["signal"])] {
			router.markDraining(id)
		}
		refresh()
		return
	case "start", "die", "destroy":
		router.clearDraining(id)
		refresh()
		return
	case "stop":
		refresh()
		return
	}
	if strings.HasPrefix(ev.Action, "health_status") {
		refresh()
	}
}

// mainHandler is the :8092 handler chain. selfcheck.Handler sits outermost so
// the container's own loopback /healthz probes never reach the unrouted
// limiter, metrics or access log.
func mainHandler(router http.Handler, metrics *Metrics, access *AccessLog) http.Handler {
	return selfcheck.Handler(withAccessLog(withMetrics(router, metrics), access))
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
