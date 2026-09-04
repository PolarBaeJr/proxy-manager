package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Backend struct {
	URL         string
	Weight      int
	Container   string
	HealthPath  string
	proxy       *httputil.ReverseProxy
	healthyFlag atomic.Bool

	// stickyID is the opaque per-backend value carried in the affinity
	// cookie when RouteGroup.Sticky is on (see setStickyCookie). Derived
	// from URL via backendStickyID at construction and never mutated —
	// deliberately not the raw backend URL itself, which would leak
	// internal container topology to the client.
	stickyID string

	// consecFails counts consecutive failed periodic probes (health.go's
	// checkBackend only — see recordHealthCheck). Not carried forward across
	// a Router.Set() refresh the way healthyFlag is: refreshes are rare in
	// practice (driven by docker events / peer sync, not a fixed tick), and
	// losing a partial, sub-threshold streak on the odd refresh only delays
	// marking a backend down by a bit longer, never falsely clears a real
	// unhealthy verdict — healthyFlag itself still carries forward.
	consecFails atomic.Int32

	// DockerUnhealthy floors healthy() to false whenever Docker's own
	// HEALTHCHECK reports this container unhealthy, independent of the
	// proxy's own TCP/HTTP probe result. Written once at construction (see
	// assembleGroups) before the Backend is published via Router.Set() —
	// plain bool, no atomic needed, same pattern as URL/Weight/Container.
	DockerUnhealthy bool

	// Learned marks a backend synthesized from a peer's pushed route (see
	// peermerge.go) rather than discovered locally via docker labels or
	// routes.json. PeerID is the identity of the peer that advertised it.
	Learned bool
	PeerID  string
}

// unhealthyAfterConsecutiveFails gates recordHealthCheck's failure side:
// deliberately asymmetric with recovery (immediate on a single success)
// because a backend that's slow to come back is strictly worse than one
// that's slow to go away — one dropped packet inside the probe's timeout
// budget shouldn't blackhole a good backend for a full health-check
// interval.
const unhealthyAfterConsecutiveFails = 2

// recordHealthCheck applies hysteresis to a periodic probe result — use this
// from health.go's checkBackend, not markHealthy directly. tryProxy's
// ErrorHandler (router.go) deliberately keeps calling markHealthy directly:
// that path has already observed one genuine failed request and needs no
// corroborating probe before reacting.
func (b *Backend) recordHealthCheck(ok bool) {
	if ok {
		b.consecFails.Store(0)
		b.markHealthy(true)
		return
	}
	if b.consecFails.Add(1) >= unhealthyAfterConsecutiveFails {
		b.markHealthy(false)
	}
}

func (b *Backend) markHealthy(ok bool) { b.healthyFlag.Store(ok) }
func (b *Backend) healthy() bool       { return !b.DockerUnhealthy && b.healthyFlag.Load() }

// excludedFrom is healthy()'s inverse, but lets pickAny's panic mode
// (ignoreHealth=true) distrust only healthyFlag — the proxy's own probe
// result, exactly what panic mode exists to second-guess — while still
// honoring DockerUnhealthy unconditionally. DockerUnhealthy comes from
// Docker's own HEALTHCHECK, run from inside/alongside the container, a
// different and more authoritative signal than our own TCP/HTTP probe; it's
// also what catches a deadlocked process that still accepts TCP connections
// — precisely the case where panic-mode routing would trade a fast 503 for
// a hang. So it stays load-bearing even when we've decided not to trust our
// own probe anymore.
func (b *Backend) excludedFrom(ignoreHealth bool) bool {
	if b.DockerUnhealthy {
		return true
	}
	return !ignoreHealth && !b.healthyFlag.Load()
}

type RouteGroup struct {
	Host        string
	PathPrefix  string
	StripPrefix bool
	Name        string
	Service     string
	Backends    []*Backend

	// DropHeaders are stripped from the request before it's forwarded to any
	// backend in this group (see ServeHTTP) — for routes whose backend has a
	// hard limit on total header size and doesn't need the header at all,
	// e.g. Cookie on a Supabase /realtime or /rest prefix that authenticates
	// via apikey/Authorization instead.
	DropHeaders []string

	// Sticky opts this route into cookie-based session affinity: once a
	// client is routed to a backend, subsequent requests pin back to it (see
	// stickyCookieName/setStickyCookie in ServeHTTP). Dual-path config,
	// same as DropHeaders — routes.json's "sticky" field or the
	// proxy.sticky docker label. Deliberately not advertised across the
	// peer mesh (peersync.go/peermerge.go never touch it) and never applied
	// to a hopped request — see the hopped gating in ServeHTTP.
	Sticky bool

	// CacheTTL > 0 opts this route into the anonymous GET/HEAD micro-cache
	// (cache.go); CachePaths optionally narrows it to client-path prefixes.
	// Dual-path config like DropHeaders/Sticky — routes.json's "cache" /
	// "cache_paths" or the proxy.cache / proxy.cache.paths labels. Not
	// advertised over the peer mesh (peersync.go/peermerge.go never touch
	// it): each proxy caches what it serves. Composes with the other
	// per-route knobs by construction rather than special-casing: an
	// auth-gated route is effectively always BYPASS (the SSO cookie is a
	// Cookie header), and a sticky route never stores (setStickyCookie
	// queues a Set-Cookie before every backend attempt).
	CacheTTL   time.Duration
	CachePaths []string
	// cache is set only by Router.Set (like limiter below), which persists
	// the store across rebuilds in Router.caches so a refresh doesn't dump
	// every cached body. nil when CacheTTL == 0.
	cache *responseCache

	AuthRequired bool
	AuthUsers    []string // lowercased; empty = any authenticated user
	AuthMode     string   // "" = sso (cookie or login redirect), "oauth" = bearer-first MCP mode

	RateLimit bool
	RateRPM   int
	limiter   limiter

	// Spread turns this group's learned (peer) backends from a failover tier
	// into equal members of one load-balanced pool — the "treating two
	// hosts' backends for the same route as one pool" case
	// docs/PEER_MESH_PLAN.md scopes, whose stated prerequisite (the
	// Redis-backed shared limiter in redisrl.go, so a client can't get
	// N x instances throughput by being balanced across peers) is already
	// in place. Opt-in only: set from the proxy.spread label locally, or
	// adopted from a peer that advertises it (peermerge.go's overlay).
	Spread bool

	// SpreadLocal is the half of Spread that came from THIS host's own
	// labels, and is the only half peersync.go advertises. Advertising the
	// adopted value instead would latch the flag on permanently: this host
	// would re-advertise what it learned, the peer would adopt its own claim
	// back, and neither side could ever clear it while the route itself kept
	// being advertised (so TTL expiry never fires). Symmetry is unaffected —
	// whichever host actually carries the label keeps advertising it.
	SpreadLocal bool

	cursor atomic.Uint64

	// panicLogAt throttles the panic-mode fallback's log line (see
	// logPanicOnce/ServeHTTP) to once per panicLogInterval per group — a real
	// outage can push every request in the group through the fallback, and
	// logging each one would spam the log exactly when it's least useful.
	// Unix nanoseconds, 0 = never logged.
	panicLogAt atomic.Int64

	// static marks a group whose backend list is owned by routes.json / a
	// hand-curated static config entry. Once true, a lingering docker label
	// for the same host+path must not re-join this group as a direct
	// backend or merge its auth/ratelimit config in — routes.json has
	// exclusive ownership of the group's identity. (Onboarding no longer
	// writes routes.json entries — see cmd/dashboard/onboarded.go — so the
	// case this protects against today is an incidental host+path
	// collision between a hand-curated entry and an unrelated label-managed
	// container, not a not-yet-relabeled onboarded original.) Service-field
	// backend resolution (Service != "") is the one deliberate exception:
	// a static group can still pick up backends from label-managed
	// containers that carry the matching proxy.service label.
	static bool
}

// PeerHopHeader marks a request that has already been forwarded once by a
// peer proxy in the mesh. Its presence lets the receiving side (a) skip
// re-charging the shared rate limiter for a request already charged by the
// originating peer (see ServeHTTP) and (b) refuse to forward the request to
// yet another learned (peer) backend, which is what actually prevents a
// forwarding loop between two proxies that both only know about each other
// for a given route.
const PeerHopHeader = "X-Pmgr-Peer-Hop"

// panicLogInterval bounds how often ServeHTTP logs the panic-mode fallback
// engaging for a given group — see RouteGroup.panicLogAt.
const panicLogInterval = 30 * time.Second

// logPanicOnce logs that this group's panic-mode fallback engaged, at most
// once per panicLogInterval. CompareAndSwap (rather than a plain load+store)
// so concurrent requests hitting the fallback simultaneously log exactly
// once, not once each.
func (g *RouteGroup) logPanicOnce(host string) {
	now := time.Now().UnixNano()
	last := g.panicLogAt.Load()
	if now-last < int64(panicLogInterval) {
		return
	}
	if g.panicLogAt.CompareAndSwap(last, now) {
		log.Printf("proxy: group %q (host %s) has no healthy backends — panic-mode routing anyway (health data untrusted)", g.Service, host)
	}
}

// pickHealthy is a two-tier preference: a healthy local (non-learned) backend
// is always chosen over a healthy learned (peer) one when both exist for the
// route, so a peer is only ever used as a failover — per
// docs/PEER_MESH_PLAN.md — not load-balanced alongside a working local
// backend. Only when the local tier has nothing eligible does it fall
// through to the learned tier (gated on allowPeer, same as before). Each
// tier does its own weighted round-robin via the shared cursor; the cursor
// is only advanced by a tier that actually has a candidate, so an empty
// local tier never perturbs the round-robin sequence within the learned
// tier (and vice versa) — this keeps existing local-only routing behavior
// (and its round-robin sequence) untouched when there are no learned
// backends at all.
func (g *RouteGroup) pickHealthy(skip map[*Backend]bool, allowPeer bool) *Backend {
	// Spread collapses the two tiers into one pool, but only for a request
	// that hasn't already been forwarded once (allowPeer). A hopped request
	// still gets the local-only tier, which is what keeps two spread proxies
	// from bouncing a request between each other forever.
	if g.Spread && allowPeer {
		if b := g.pickPool(skip, false); b != nil {
			return b
		}
		return nil
	}
	if b := g.pickTier(skip, false, false); b != nil {
		return b
	}
	if allowPeer {
		return g.pickTier(skip, true, false)
	}
	return nil
}

// pickAny is pickHealthy with the probe-based healthyFlag filter dropped —
// a last-resort fallback for when a group has nothing left that its own
// probe data trusts. Only ever called after pickHealthy has already
// returned nil for this request, so it never overrides or races a
// legitimately-healthy pick; see the panic-mode fallback in ServeHTTP. Same
// tiering/skip/weight rules as pickHealthy, so it can't select a Learned
// backend on a hopped request either — it only widens what "eligible" means
// within the same tiers. Deliberately does NOT drop the DockerUnhealthy
// floor — see Backend.excludedFrom for why that signal stays load-bearing
// even in panic mode.
func (g *RouteGroup) pickAny(skip map[*Backend]bool, allowPeer bool) *Backend {
	if g.Spread && allowPeer {
		if b := g.pickPool(skip, true); b != nil {
			return b
		}
		return nil
	}
	if b := g.pickTier(skip, false, true); b != nil {
		return b
	}
	if allowPeer {
		return g.pickTier(skip, true, true)
	}
	return nil
}

// pickPool is pickTier without the Learned partition: one weighted
// round-robin over every eligible backend, local and peer alike. ignoreHealth
// drops the probe-based healthyFlag filter — see pickAny/excludedFrom.
func (g *RouteGroup) pickPool(skip map[*Backend]bool, ignoreHealth bool) *Backend {
	var pool []*Backend
	for _, b := range g.Backends {
		if skip[b] || b.excludedFrom(ignoreHealth) {
			continue
		}
		w := b.Weight
		if w < 1 {
			w = 1
		}
		for i := 0; i < w; i++ {
			pool = append(pool, b)
		}
	}
	if len(pool) == 0 {
		return nil
	}
	return pool[int(g.cursor.Add(1)-1)%len(pool)]
}

// pickTier restricts selection to backends whose Learned flag matches
// wantLearned, applying the same skip-set + weighted round-robin logic
// pickHealthy always used. ignoreHealth drops the probe-based healthyFlag
// filter — see pickAny/excludedFrom.
func (g *RouteGroup) pickTier(skip map[*Backend]bool, wantLearned, ignoreHealth bool) *Backend {
	var pool []*Backend
	for _, b := range g.Backends {
		if skip[b] || b.Learned != wantLearned || b.excludedFrom(ignoreHealth) {
			continue
		}
		w := b.Weight
		if w < 1 {
			w = 1
		}
		for i := 0; i < w; i++ {
			pool = append(pool, b)
		}
	}
	if len(pool) == 0 {
		return nil
	}
	return pool[int(g.cursor.Add(1)-1)%len(pool)]
}

// stickyCookiePrefix names every affinity cookie this proxy issues, so
// setStickyCookie can recognize and strip its own prior header value without
// touching an unrelated Set-Cookie (e.g. an SSO auth cookie) added earlier in
// the same ServeHTTP call.
const stickyCookiePrefix = "_pmgr_sticky_"

// stickyCookieMaxAge is reissued on every request that reaches a sticky
// group, so it slides forward for an active session rather than expiring a
// client mid-use.
const stickyCookieMaxAge = 2 * time.Hour

// stickyCookieName derives the cookie NAME (not just its Path) from the
// route's own identity — Host + PathPrefix — so two different sticky
// RouteGroups on the same Host at different PathPrefixes can never collide.
// Path-scoping alone isn't sufficient: browsers don't guarantee a
// deterministic Cookie-header order across same-named cookies set at
// different paths.
func stickyCookieName(g *RouteGroup) string {
	sum := sha256.Sum256([]byte(g.Host + "|" + g.PathPrefix))
	return stickyCookiePrefix + hex.EncodeToString(sum[:4])
}

// stickyCookiePath is the cookie's Path attribute — PathPrefix, or "/" when
// the route has none. Defense in depth alongside the name-scoping above.
func stickyCookiePath(g *RouteGroup) string {
	if g.PathPrefix != "" {
		return g.PathPrefix
	}
	return "/"
}

// backendByStickyID resolves a cookie's pinned value back to a live backend
// in this group, or nil if the pin no longer matches anything (backend
// removed, group reconfigured, etc.) — the caller falls through to normal
// pickHealthy in that case.
func (g *RouteGroup) backendByStickyID(id string) *Backend {
	for _, b := range g.Backends {
		if b.stickyID == id {
			return b
		}
	}
	return nil
}

// setStickyCookie issues (or reissues) this group's affinity cookie. It
// first strips any prior Set-Cookie value for this same cookie name already
// queued on w — needed because a failed pinned attempt followed by a
// successful fallback pick must not emit two Set-Cookie headers for the same
// name in one ServeHTTP call — while leaving every other queued Set-Cookie
// header (e.g. an SSO auth cookie) untouched.
func setStickyCookie(w http.ResponseWriter, g *RouteGroup, stickyID string) {
	name := stickyCookieName(g)
	existing := w.Header()["Set-Cookie"]
	w.Header().Del("Set-Cookie")
	for _, v := range existing {
		if !strings.HasPrefix(v, name+"=") {
			w.Header().Add("Set-Cookie", v)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    stickyID,
		Path:     stickyCookiePath(g),
		MaxAge:   int(stickyCookieMaxAge / time.Second),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

type Router struct {
	mu     sync.RWMutex
	groups []*RouteGroup

	// limiters persist across refreshes, keyed by group key host+"|"+path, so
	// config edits don't reset per-IP buckets. Guarded by mu.
	limiters   map[string]limiter
	caches     map[string]*responseCache // same key scheme as limiters; guarded by mu
	xffTrusted []*net.IPNet

	// newLimiter constructs a fresh limiter for a route group, given its
	// host+"|"+path route key (for the Redis key scheme) and rpm; nil
	// defaults to the plain in-memory rateLimiter (today's behavior). main.go
	// sets this to build a Redis-backed hybridLimiter when -redis-addr is
	// configured. Kept as a constructor func (rather than a *redis.Client
	// field here) so router.go stays free of the redis import.
	newLimiter func(routeKey string, rpm int) limiter

	// unroutedLimiter throttles requests that match no route (bad Host/path),
	// keyed by client IP, independent of any per-group limiter. Set once at
	// startup; nil (disabled) in tests that don't wire it up.
	unroutedLimiter *rateLimiter

	auth     *authGate // nil = auth gating disabled (proxy.auth hosts fail closed)
	authWarn sync.Once
}

func (r *Router) Set(groups []*RouteGroup) {
	sort.SliceStable(groups, func(i, j int) bool {
		return len(groups[i].PathPrefix) > len(groups[j].PathPrefix)
	})
	r.mu.Lock()
	prev := r.groups
	// Reconcile per-service limiters, preserving bucket state across refreshes.
	// Done under the same lock, with g.limiter assigned before r.groups is
	// published, so ServeHTTP never reads a group whose limiter isn't wired up.
	if r.limiters == nil {
		r.limiters = map[string]limiter{}
	}
	live := map[string]bool{}
	for _, g := range groups {
		if !g.RateLimit {
			continue
		}
		key := g.Host + "|" + g.PathPrefix
		live[key] = true
		if rl, ok := r.limiters[key]; ok {
			rl.SetRate(g.RateRPM)
			g.limiter = rl
		} else {
			var rl limiter
			if r.newLimiter != nil {
				rl = r.newLimiter(key, g.RateRPM)
			} else {
				rl = newRateLimiter(g.RateRPM)
			}
			r.limiters[key] = rl
			g.limiter = rl
		}
	}
	for key, rl := range r.limiters {
		if !live[key] {
			rl.Stop()
			delete(r.limiters, key)
		}
	}
	// Per-route caches reconcile the same way, and for the same reason
	// g.cache is wired before r.groups is published.
	if r.caches == nil {
		r.caches = map[string]*responseCache{}
	}
	liveCache := map[string]bool{}
	for _, g := range groups {
		if g.CacheTTL <= 0 {
			continue
		}
		key := g.Host + "|" + g.PathPrefix
		liveCache[key] = true
		if c, ok := r.caches[key]; ok {
			c.setTTL(g.CacheTTL)
			g.cache = c
		} else {
			c := newResponseCache(g.CacheTTL)
			r.caches[key] = c
			g.cache = c
		}
	}
	for key := range r.caches {
		if !liveCache[key] {
			delete(r.caches, key)
		}
	}
	r.groups = groups
	r.mu.Unlock()

	// Keyed by route identity + backend URL, not the bare URL alone: two
	// different routes can legitimately point at the same literal backend
	// (e.g. two static routes.json entries proxying the same upstream), and
	// a bare-URL key would conflate their health state on every refresh —
	// one route's transient failure silently flips the other's backend
	// unhealthy (or vice versa) even though they're unrelated route entries.
	prevHealth := map[string]bool{}
	for _, g := range prev {
		for _, b := range g.Backends {
			prevHealth[g.Host+"|"+g.PathPrefix+"|"+b.URL] = b.healthyFlag.Load()
		}
	}
	for _, g := range groups {
		for _, b := range g.Backends {
			if h, ok := prevHealth[g.Host+"|"+g.PathPrefix+"|"+b.URL]; ok {
				b.healthyFlag.Store(h)
			} else {
				b.healthyFlag.Store(true)
			}
		}
	}
}

func (r *Router) Snapshot() []*RouteGroup {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*RouteGroup, len(r.groups))
	copy(out, r.groups)
	return out
}

// RouteRateLimitSnapshot is one rate-limited route group's current bucket
// state, for the dashboard's read-only rate-limit view.
type RouteRateLimitSnapshot struct {
	Host    string           `json:"host"`
	Path    string           `json:"path"`
	RPM     int              `json:"rpm"`
	Buckets []bucketSnapshot `json:"buckets"`
}

// RateLimitSnapshot reports current bucket state for every rate-limited
// route group. Read-only — delegates to each group's limiter.Snapshot().
func (r *Router) RateLimitSnapshot() []RouteRateLimitSnapshot {
	r.mu.RLock()
	groups := r.groups
	r.mu.RUnlock()
	out := make([]RouteRateLimitSnapshot, 0)
	for _, g := range groups {
		if !g.RateLimit || g.limiter == nil {
			continue
		}
		out = append(out, RouteRateLimitSnapshot{
			Host: g.Host, Path: g.PathPrefix, RPM: g.RateRPM, Buckets: g.limiter.Snapshot(),
		})
	}
	return out
}

func hostOnly(s string) string {
	if i := strings.IndexByte(s, ':'); i != -1 {
		return s[:i]
	}
	return s
}

// rlKey is the per-IP rate-limit bucket key. It reuses realClientIP's
// XFF-trust logic so a client behind a trusted hop is keyed by its real IP,
// and an untrusted peer can't mint fresh buckets by rotating X-Forwarded-For.
// IPv6 addresses are keyed by their /64 (the smallest block an ISP typically
// hands a single customer) rather than the full /128 — otherwise one client
// can mint an unbounded number of distinct buckets just by rotating the host
// portion of its own address.
func rlKey(req *http.Request, xffTrusted []*net.IPNet) string {
	if ip := realClientIP(req, xffTrusted); ip != nil {
		return maskRLKey(ip)
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return maskRLKey(ip)
	}
	return host
}

func maskRLKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

const maxRetries = 3

// defaultRateRPM is the fallback capacity for an enabled-but-misconfigured
// rate limit (proxy.ratelimit=true with a missing/invalid proxy.ratelimit.rpm).
const defaultRateRPM = 60

// retryAfterSeconds derives a Retry-After value from the group's actual rpm
// instead of a fixed guess — at rpm=600 a client only needs to wait ~0.1s for
// its next token; at rpm=6 it needs 10s.
func retryAfterSeconds(rpm int) int {
	if rpm <= 0 {
		rpm = defaultRateRPM
	}
	secs := int(math.Ceil(60.0 / float64(rpm)))
	if secs < 1 {
		secs = 1
	}
	return secs
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Strip any client-supplied attribution header before ANY routing or auth
	// decision, on every request — gated, ungated, matched or not. The
	// signature stops a forged assertion being believed; this is what stops a
	// backend ever seeing an attacker-controlled value in the header at all.
	// Only the proxy may set it, and only after it knows who is calling.
	req.Header.Del(ActorHeader)

	// A hopped request is one a peer proxy already forwarded to us. It skips
	// forwarding to another learned backend (loop prevention, below) — that's
	// its only effect. It does NOT skip the rate-limit charge: a hop only
	// happens when the receiving side has no healthy local backend for the
	// route (see pickHealthy's local-first tiering), so a hopped request is
	// charged again here on top of whatever the originating peer charged.
	// That's a mildly conservative double-charge during failover, not a
	// correctness bug, and it's the price of not trusting this header for
	// anything security-relevant.
	//
	// Unlike ActorHeader above, this arrives unauthenticated: a peer forwards
	// to our proxy port, not the bearer-gated metrics port, so there's no
	// signature to check it against. Using it for loop prevention only is
	// fail-safe against forgery — a client forging it only denies itself
	// peer failover. It must never gate a rate-limit decision: any client
	// can set this header on a direct request, and skipping the limiter
	// whenever it's present would be a self-inflicted bypass.
	hopped := req.Header.Get(PeerHopHeader) != ""
	req.Header.Del(PeerHopHeader)

	r.mu.RLock()
	groups := r.groups
	r.mu.RUnlock()

	reqHost := hostOnly(req.Host)
	// OAuth resource-server metadata (and the legacy AS-metadata fallback)
	// must be answered before path-prefix matching AND before auth — clients
	// fetch it unauthenticated from the protected host itself.
	if r.handleOAuthWellKnown(w, req, groups, reqHost) {
		return
	}
	var group *RouteGroup
	for _, g := range groups {
		if !strings.EqualFold(reqHost, g.Host) {
			continue
		}
		if g.PathPrefix != "" && !strings.HasPrefix(req.URL.Path, g.PathPrefix) {
			continue
		}
		group = g
		break
	}
	if group == nil {
		// No route matched: tell the metrics layer to bucket this request as
		// unrouted so scanner-supplied Host headers can't grow the metrics maps.
		// Interface-based so this file stays free of accesslog imports.
		if m, ok := w.(interface{ MarkUnrouted() }); ok {
			m.MarkUnrouted()
		}
		// Unrouted traffic is exactly the shape a scanner produces (random
		// Host headers/paths) — throttle it too, not just matched routes.
		// Shares the bounded-bucket-table defense in ratelimit.go, so this
		// can't be turned into its own memory-exhaustion vector.
		if r.unroutedLimiter != nil && !r.unroutedLimiter.Allow(rlKey(req, r.xffTrusted)) {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(defaultRateRPM)))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		serveUnavailable(w, http.StatusNotFound, reqHost, "Service unavailable at this time, try again later.")
		return
	}
	// Always charged, hopped or not — see the hopped comment above for why
	// PeerHopHeader must never gate this decision.
	if group.limiter != nil {
		key := rlKey(req, r.xffTrusted)
		if !group.limiter.Allow(key) {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(group.RateRPM)))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}
	if group.AuthRequired {
		if r.auth == nil {
			// proxy.auth=true but the gate isn't configured (-auth-domains
			// unset) — fail closed rather than silently exposing the host.
			r.authWarn.Do(func() {
				log.Printf("auth: host %s requires auth but -auth-domains is unset — failing closed with 503", group.Host)
			})
			serveUnavailable(w, http.StatusServiceUnavailable, reqHost, "Service unavailable at this time, try again later.")
			return
		}
		if !r.auth.authorize(w, req, group) {
			return
		}
	}
	// Captured before the StripPrefix mutation below: the cache key must be
	// what the client asked for, not what the backend will see.
	origPath, origQuery := req.URL.Path, req.URL.RawQuery
	if group.StripPrefix && group.PathPrefix != "" {
		req.URL.Path = strings.TrimPrefix(req.URL.Path, group.PathPrefix)
		if !strings.HasPrefix(req.URL.Path, "/") {
			req.URL.Path = "/" + req.URL.Path
		}
	}
	// Sticky is read here, between the StripPrefix mutation above and the
	// DropHeaders strip below — load-bearing order: a route with
	// DropHeaders: ["Cookie"] deletes the whole Cookie header, so reading
	// after that loop would break sticky on any route that also drops
	// cookies. Gated on !hopped, same as the allowPeer plumbing below — a
	// hopped request never applies sticky logic.
	var stickyPin string
	if group.Sticky && !hopped {
		if c, err := req.Cookie(stickyCookieName(group)); err == nil && c.Value != "" {
			stickyPin = c.Value
		}
	}

	// Cache eligibility reads Cookie/Authorization/Range, so like sticky it
	// must be evaluated BEFORE DropHeaders can delete Cookie — otherwise a
	// route dropping cookies would happily cache a logged-in user's page.
	cacheable := cacheRequestEligible(req, group, origPath)
	var key string
	if group.cache != nil {
		key = cacheKey(group.Host, origPath, origQuery, req.Header.Get("Accept-Encoding"))
	}

	for _, h := range group.DropHeaders {
		req.Header.Del(h)
	}

	if group.cache == nil {
		r.proxyToGroup(w, req, group, reqHost, hopped, stickyPin)
		return
	}
	r.serveWithCache(w, req, group, reqHost, hopped, stickyPin, cacheable, key)
}

// serveWithCache is the cached-route tail of ServeHTTP. It sits AFTER the
// limiter and auth gates on purpose: a HIT still costs a rate-limit token
// (the cache protects the backend, not the proxy), and an auth-gated route
// always ends up BYPASS because the SSO cookie is itself a Cookie header —
// the cache never gets to answer for an authorize() it didn't run. Sticky
// routes never store (setStickyCookie queues a Set-Cookie on every attempt)
// and client no-cache is ignored (see cacheRequestEligible).
func (r *Router) serveWithCache(w http.ResponseWriter, req *http.Request, group *RouteGroup, reqHost string, hopped bool, stickyPin string, cacheable bool, key string) {
	c := group.cache
	if !cacheable {
		r.proxyToGroup(&cacheRecorder{ResponseWriter: w, mode: "BYPASS"}, req, group, reqHost, hopped, stickyPin)
		return
	}
	now := c.now()
	if e, ok := c.get(key, now); ok {
		serveCached(w, req, e, now)
		return
	}
	// A HEAD miss never fills: the backend's HEAD response has no body to
	// store, and pretending otherwise would cache an empty 200 for GETs.
	if req.Method == http.MethodHead {
		r.proxyToGroup(&cacheRecorder{ResponseWriter: w, mode: "BYPASS"}, req, group, reqHost, hopped, stickyPin)
		return
	}
	f, owner := c.beginFill(key)
	if !owner {
		f.waiters.Add(1)
		select {
		case <-f.done:
		case <-req.Context().Done():
			return
		}
		now = c.now()
		if e, ok := c.get(key, now); ok {
			serveCached(w, req, e, now)
			return
		}
		// The filler produced nothing storable (Set-Cookie, non-200, over
		// the cap...). Go to the backend ourselves, but as an UNREGISTERED
		// filler: still record and store if this response happens to be
		// storable, never take the inflight slot or wait a second time.
	}
	rw := &cacheRecorder{ResponseWriter: w, mode: "MISS", record: true}
	// Defer order is load-bearing (LIFO): the store must land BEFORE the
	// waiters are released, or they'd wake to a miss and all stampede the
	// backend — exactly what coalescing exists to prevent. Registering
	// endFill first also guarantees release on a panic or 503.
	if owner {
		defer c.endFill(key, f)
	}
	// completed gates the store on a normal return: ReverseProxy aborts a
	// mid-body copy failure (backend died, client vanished) by panicking
	// with http.ErrAbortHandler, and these defers still run on the way out
	// — without the gate, a truncated body would be cached for the full
	// TTL. endFill above still releases the waiters either way.
	completed := false
	defer func() {
		if !completed {
			return
		}
		if e := rw.entry(); e != nil {
			e.storedAt = c.now()
			c.put(key, e)
		}
	}()
	r.proxyToGroup(rw, req, group, reqHost, hopped, stickyPin)
	completed = true
}

// proxyToGroup is ServeHTTP's backend loop: pick, attribute, attempt, retry,
// and finally 503. Split out so the cached and uncached paths share it
// verbatim; w may be a cacheRecorder, which forwards SetBackend so the
// attribution still reaches the access-log writer underneath.
func (r *Router) proxyToGroup(w http.ResponseWriter, req *http.Request, group *RouteGroup, reqHost string, hopped bool, stickyPin string) {
	tried := map[*Backend]bool{}
	for attempt := 0; attempt < maxRetries; attempt++ {
		var b *Backend
		if stickyPin != "" {
			// A pin to a Learned (peer) backend is only honored when Spread
			// is on: peers are otherwise failover-only, never load-balanced,
			// so honoring a stale pin after local backends recover would
			// strand the client on an unnecessary extra hop.
			if pinned := group.backendByStickyID(stickyPin); pinned != nil && pinned.healthy() && (!pinned.Learned || group.Spread) {
				b = pinned
			}
			// Only the first iteration consults the pin — a failed pinned
			// attempt falls through to normal pickHealthy for any retry.
			stickyPin = ""
		}
		if b == nil {
			b = group.pickHealthy(tried, !hopped)
			if b == nil {
				// Health data says nothing eligible is left — either every
				// backend is genuinely down, or the health state is simply
				// wrong. Rather than guarantee a 503 on that belief, stop
				// trusting it and try whatever's left anyway: stale health data
				// is a likelier explanation than every backend being
				// simultaneously dead. Still gated by allowPeer/tried inside
				// pickAny, so a hopped request can't loop back onto a peer.
				if pb := group.pickAny(tried, !hopped); pb != nil {
					group.logPanicOnce(reqHost)
					b = pb
				}
			}
		}
		if b == nil {
			break
		}
		tried[b] = true
		// Tell whichever wrapping writer cares (access log) which upstream we picked.
		// Interface-based so this file stays free of accesslog imports.
		if setter, ok := w.(interface{ SetBackend(string) }); ok {
			setter.SetBackend(b.URL)
		}
		if group.Sticky && !hopped {
			setStickyCookie(w, group, b.stickyID)
		}
		if tryProxy(w, req, b) {
			return
		}
	}
	// Metrics are the raw truth — count every 503 honestly. The dashboard
	// UI is the layer that lets the operator choose to hide stopped-service
	// hosts from the Top hosts / error-rate view via a toggle.
	log.Printf("proxy: group %q (host %s) has no healthy backends — serving 503", group.Service, reqHost)
	serveUnavailable(w, http.StatusServiceUnavailable, reqHost, "Service unavailable at this time, try again later.")
}

// serveUnavailable writes a small styled HTML page with a 5-minute
// meta-refresh so the browser silently retries in the background. Used
// when a host has no healthy backends (all replicas stopped, container
// crashed, etc.) or when the host has no route at all. The page is
// intentionally minimal — no JS, no external assets — so it works even
// when the only thing the proxy can do is fail.
func serveUnavailable(w http.ResponseWriter, status int, host, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "300")
	w.WriteHeader(status)
	title := "Service unavailable"
	if status == http.StatusNotFound {
		title = "Not found"
	}
	fmt.Fprintf(w, `<!doctype html><html lang=en><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<meta http-equiv=refresh content="300">
<title>%d %s · %s</title>
<style>
  :root{color-scheme:dark}
  html,body{height:100%%;margin:0}
  body{background:#0a0a0a;color:#e6e6e6;font:15px/1.55 -apple-system,BlinkMacSystemFont,"Inter",system-ui,sans-serif;display:flex;align-items:center;justify-content:center;padding:24px}
  .box{max-width:420px;text-align:center}
  .code{font:600 12px/1 ui-monospace,SFMono-Regular,Menlo,monospace;letter-spacing:.12em;color:#6a6a6a;margin-bottom:14px;text-transform:uppercase}
  h1{margin:0 0 10px;font-size:20px;font-weight:600;letter-spacing:-.015em;color:#fafafa}
  p{margin:0;color:#8a8a8a;font-size:14px}
</style>
<div class=box>
  <div class=code>%d · %s</div>
  <h1>%s</h1>
  <p>%s</p>
</div>
`, status, title, host, status, http.StatusText(status), title, reason)
}

func tryProxy(w http.ResponseWriter, req *http.Request, b *Backend) bool {
	rec := &errCatchingWriter{ResponseWriter: w}
	failed := false
	clientGone := false
	b.proxy.ErrorHandler = func(_ http.ResponseWriter, r *http.Request, err error) {
		// A canceled request context means the CLIENT went away (refresh,
		// navigation, closed tab) — Go's reverse proxy reports that the same
		// way it reports a dead backend. Don't let a healthy backend get
		// marked down just because someone hit stop; that's how a handful of
		// aborted requests to a group with only one (peer-learned) backend
		// took the whole group down until the next health tick.
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			log.Printf("backend %s: client disconnected mid-request, not marking unhealthy", b.URL)
			clientGone = true
			return
		}
		log.Printf("backend %s error: %v — marking unhealthy", b.URL, err)
		b.markHealthy(false)
		failed = true
	}
	b.proxy.ServeHTTP(rec, req)
	if clientGone {
		// Nobody is waiting for a response, so there's no one left to serve by
		// retrying — and re-dispatching to a DIFFERENT backend can duplicate
		// whatever side effect the first backend already started (this
		// backend may still be mid-handler when the client gave up; a
		// non-idempotent handler that claims work before finishing it has no
		// way to know a second backend is about to redo it). Stop the retry
		// loop here instead of falling through to another backend.
		return true
	}
	return !(failed && !rec.wroteHeader)
}

type errCatchingWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *errCatchingWriter) WriteHeader(code int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}
func (w *errCatchingWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// Hijack forwards to the underlying ResponseWriter so WebSocket / protocol
// upgrades (e.g. Supabase Realtime) work through the proxy. Without this the
// reverse proxy can't switch protocols, fails the upgrade, and — via the
// ErrorHandler — falsely marks the backend unhealthy, causing spurious 503s.
func (w *errCatchingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("proxy: underlying ResponseWriter does not support hijacking")
}

// healthLabelWarned tracks which label-managed containers we've already
// warned about missing proxy.health, keyed by container name, for the
// process lifetime. Needed because refresh() now also fires on Docker
// health_status events (see main.go), far more often than the original
// start/die/destroy/kill/stop set — without dedup, a flapping container
// would spam this warning on every transition.
var (
	healthLabelWarnMu   sync.Mutex
	healthLabelWarnSeen = map[string]bool{}
)

// cacheLabelWarnSeen dedups the bad-proxy.cache warning the same way, for
// the same reason.
var (
	cacheLabelWarnMu   sync.Mutex
	cacheLabelWarnSeen = map[string]bool{}
)

// ---- Assembly: docker labels + static config ----

type staticRoute struct {
	Host     string   `json:"host"`
	Path     string   `json:"path,omitempty"`
	Strip    bool     `json:"strip,omitempty"`
	Name     string   `json:"name,omitempty"`
	Backends []string `json:"backends"`
	// Service resolves additional backends from label-managed containers
	// carrying a matching proxy.service label — for the one case that still
	// needs a hand-curated routes.json entry: per-path rate limits on a
	// single container serving multiple internal paths. May be set alongside
	// literal Backends; both are used.
	Service     string   `json:"service,omitempty"`
	Health      string   `json:"health,omitempty"`
	Auth        bool     `json:"auth,omitempty"`
	AuthUsers   []string `json:"auth_users,omitempty"`
	AuthMode    string   `json:"auth_mode,omitempty"`
	RateLimit   bool     `json:"ratelimit,omitempty"`
	RateRPM     int      `json:"ratelimit_rpm,omitempty"`
	DropHeaders []string `json:"drop_headers,omitempty"`
	Sticky      bool     `json:"sticky,omitempty"`
	Cache       string   `json:"cache,omitempty"`
	CachePaths  []string `json:"cache_paths,omitempty"`
}

type staticConfig struct {
	Routes []staticRoute `json:"routes"`
}

// assembleGroups also returns its local backendsByService map (see the
// field's own comment below) so peermerge.go's overlay() can apply the same
// Service-name backfill to a route it only knows about from a peer — see
// PeerRouteStore.overlay's localBackendsByService parameter.
func assembleGroups(ctx context.Context, dc *dockerClient, configPath string) ([]*RouteGroup, map[string][]*Backend, error) {
	groupsByKey := map[string]*RouteGroup{}

	if configPath != "" {
		if data, err := os.ReadFile(configPath); err == nil {
			var cfg staticConfig
			if err := json.Unmarshal(data, &cfg); err != nil {
				return nil, nil, fmt.Errorf("parse %s: %w", configPath, err)
			}
			for _, sr := range cfg.Routes {
				key := sr.Host + "|" + sr.Path
				g, ok := groupsByKey[key]
				if !ok {
					cacheTTL, err := parseCacheTTL(sr.Cache)
					if err != nil {
						log.Printf("static route %s%s: bad cache=%q, caching off", sr.Host, sr.Path, sr.Cache)
						cacheTTL = 0
					}
					g = &RouteGroup{
						Host: sr.Host, PathPrefix: sr.Path, StripPrefix: sr.Strip, Name: sr.Name, Service: sr.Service,
						AuthRequired: sr.Auth, AuthUsers: normalizeAuthUsers(sr.AuthUsers), AuthMode: sr.AuthMode,
						RateLimit: sr.RateLimit, RateRPM: sr.RateRPM, DropHeaders: sr.DropHeaders,
						Sticky: sr.Sticky, CacheTTL: cacheTTL, CachePaths: unionStrings(nil, sr.CachePaths),
					}
					groupsByKey[key] = g
					g.static = true
				}
				for _, raw := range sr.Backends {
					u, err := url.Parse(raw)
					if err != nil {
						log.Printf("static route %s: bad backend %q", sr.Host, raw)
						continue
					}
					g.Backends = append(g.Backends, makeBackend(raw, 1, "static", sr.Health, u, sr.Host))
				}
			}
		} else if !os.IsNotExist(err) {
			log.Printf("read %s: %v", configPath, err)
		}
	}

	// backendsByService collects one *Backend per running, non-canary,
	// proxy.service-labeled container, keyed by that service name — the pool
	// a static routes.json entry with a matching Service field backfills
	// from below. Canary is deliberately excluded here — see the labelCanary
	// comment in docker.go for why this is an intentional asymmetry with the
	// dashboard's own serviceBackends (cmd/dashboard/docker.go), which does
	// include canary.
	backendsByService := map[string][]*Backend{}

	containers, err := dc.listEnabledContainers(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range containers {
		name := c.name()
		host := c.Labels[labelHost]
		portStr := c.Labels[labelPort]
		if host == "" || portStr == "" {
			log.Printf("skip %s: missing %s or %s", name, labelHost, labelPort)
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			log.Printf("skip %s: bad %s=%q", name, labelPort, portStr)
			continue
		}

		// Resolve this container's own backend (if running with a reachable
		// IP) once — used both for backendsByService (below, independent of
		// static ownership of this host+path) and, further down, for a
		// label-managed group's own Backends.
		var backend *Backend
		if c.State == "running" {
			// Prefer the managed (edge) network IP for multi-network containers.
			var ip string
			if n, ok := c.NetworkSettings.Networks[managedNetwork]; ok && n.IPAddress != "" {
				ip = n.IPAddress
			} else {
				for _, n := range c.NetworkSettings.Networks {
					if n.IPAddress != "" {
						ip = n.IPAddress
						break
					}
				}
			}
			if ip == "" {
				log.Printf("skip backend %s: no IP on any network (state=%s)", name, c.State)
			} else {
				weight := 1
				if w, werr := strconv.Atoi(c.Labels[labelWeight]); werr == nil && w > 0 {
					weight = w
				}
				backendURL := fmt.Sprintf("http://%s:%d", ip, port)
				u, _ := url.Parse(backendURL)
				backend = makeBackend(backendURL, weight, name, c.Labels[labelHealth], u, host)
				backend.DockerUnhealthy = dockerUnhealthy(c.Status)
				if backend.DockerUnhealthy {
					log.Printf("backend %s (%s): Docker reports unhealthy — excluded from routing for %s%s", name, backend.URL, host, c.Labels[labelPath])
				}
				if c.Labels[labelHealth] == "" {
					healthLabelWarnMu.Lock()
					if !healthLabelWarnSeen[name] {
						healthLabelWarnSeen[name] = true
						log.Printf("%s (%s%s): no %s label — falling back to TCP-only health checks; if you add one, the endpoint must respond within %s", name, host, c.Labels[labelPath], labelHealth, healthTimeout)
					}
					healthLabelWarnMu.Unlock()
				}
			}
		}

		// Feed backendsByService BEFORE the static-ownership check below: a
		// static route resolving this container's proxy.service label and
		// this container's own default label-managed route are independent
		// concerns, even when they happen to share a host+path.
		if svc := c.Labels[labelService]; svc != "" && c.Labels[labelCanary] != "true" && backend != nil {
			backendsByService[svc] = append(backendsByService[svc], backend)
		}

		// Always register the route group so a stopped container's host
		// still maps to *something* — that's what turns the user-visible
		// page from 404 (no such host) into 503 (host exists, nothing
		// healthy). Backends are only appended for containers that are
		// running AND have a reachable IP.
		path := c.Labels[labelPath]
		key := host + "|" + path
		g, ok := groupsByKey[key]
		if ok && g.static {
			// routes.json already owns this host+path — a lingering docker
			// label for the same key must not sneak back in as a direct
			// backend or merge its auth/ratelimit config into the static
			// group. (The backendsByService feed above is the one exception,
			// already handled.)
			continue
		}
		if !ok {
			display := c.Labels[labelName]
			if display == "" {
				display = host
			}
			g = &RouteGroup{
				Host: host, PathPrefix: path, StripPrefix: c.Labels[labelStrip] == "true",
				Name: display, Service: c.Labels[labelService],
				DropHeaders: splitTrimmed(c.Labels[labelDropHeaders]),
			}
			groupsByKey[key] = g
		}
		// Auth fields merge across replicas, unlike Strip/Name (first-wins):
		// ANY replica carrying proxy.auth=true protects the whole group (fail
		// toward protection), and the allowlist comes from the first replica
		// that sets proxy.auth.users.
		if c.Labels[labelAuth] == "true" {
			g.AuthRequired = true
		}
		if c.Labels[labelAuthMode] == "oauth" {
			g.AuthMode = "oauth"
		}
		if len(g.AuthUsers) == 0 {
			g.AuthUsers = normalizeAuthUsers(strings.Split(c.Labels[labelAuthUsers], ","))
		}
		// Rate-limit fields merge across replicas the same way auth does: ANY
		// replica carrying proxy.ratelimit=true throttles the whole group, and
		// the rpm comes from the first replica that sets a valid one.
		if c.Labels[labelRateLimit] == "true" {
			g.RateLimit = true
		}
		// Spread merges across replicas the same way auth/ratelimit do: one
		// replica opting the route in is enough, since a cross-host scale
		// only ever labels the replicas it places on the peer, never the
		// pre-existing originals it found here.
		if c.Labels[labelSpread] == "true" {
			g.SpreadLocal = true
			g.Spread = true
		}
		// Sticky merges across replicas the same way auth/ratelimit/spread
		// do: one replica opting the route in is enough.
		if c.Labels[labelSticky] == "true" {
			g.Sticky = true
		}
		// Cache merges as the largest TTL any replica sets plus the union of
		// their path prefixes: a rolling replace briefly runs old and new
		// labels side by side, and the more generous setting is the one the
		// operator just added.
		if v := c.Labels[labelCache]; v != "" {
			d, err := parseCacheTTL(v)
			if err != nil {
				cacheLabelWarnMu.Lock()
				if !cacheLabelWarnSeen[name] {
					cacheLabelWarnSeen[name] = true
					log.Printf("%s: bad %s=%q, caching off", name, labelCache, v)
				}
				cacheLabelWarnMu.Unlock()
			} else if d > g.CacheTTL {
				g.CacheTTL = d
			}
		}
		g.CachePaths = unionStrings(g.CachePaths, splitTrimmed(c.Labels[labelCachePaths]))
		if rpmStr := c.Labels[labelRateRPM]; rpmStr != "" && g.RateRPM == 0 {
			rpm, err := strconv.Atoi(rpmStr)
			if err != nil || rpm <= 0 {
				log.Printf("%s: bad %s=%q, rate limit still enabled at the default %d rpm", name, labelRateRPM, rpmStr, defaultRateRPM)
			} else {
				g.RateRPM = rpm
			}
		}
		if backend != nil {
			g.Backends = append(g.Backends, backend)
		}
	}

	out := make([]*RouteGroup, 0, len(groupsByKey))
	for _, g := range groupsByKey {
		// Backfill a static, service-resolved group's backends from
		// whatever label-managed containers carry the matching
		// proxy.service label. A literal Backends list (if any) was already
		// populated by the static-config loop above — both are used.
		if g.static && g.Service != "" {
			if bs, ok := backendsByService[g.Service]; ok {
				g.Backends = append(g.Backends, bs...)
			} else {
				log.Printf("static route %s%s: service %q resolved to zero backends", g.Host, g.PathPrefix, g.Service)
			}
		}
		// Footgun guard: an enabled limiter must never have capacity 0.
		if g.RateLimit && g.RateRPM <= 0 {
			g.RateRPM = defaultRateRPM
		}
		sort.SliceStable(g.Backends, func(i, j int) bool { return g.Backends[i].URL < g.Backends[j].URL })
		out = append(out, g)
	}
	return out, backendsByService, nil
}

// splitTrimmed splits a comma-separated label value, trimming whitespace and
// dropping empties. Returns nil (not an empty slice) for "" so an unset
// label leaves RouteGroup.DropHeaders nil, matching the static-config
// zero-value case.
func splitTrimmed(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalizeAuthUsers trims, drops empties, and lowercases so membership
// checks against the signed cookie username are case-insensitive.
func normalizeAuthUsers(in []string) []string {
	var out []string
	for _, u := range in {
		u = strings.ToLower(strings.TrimSpace(u))
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

// backendStickyID derives the opaque per-backend value used in the affinity
// cookie from its raw URL — deterministic across refreshes (so an existing
// client's cookie still resolves after a Router.Set()) without ever exposing
// the URL itself to the client. 16 hex chars is enough entropy to avoid
// collisions within one route group's small backend set.
func backendStickyID(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:8])
}

func makeBackend(rawURL string, weight int, container, healthPath string, u *url.URL, hostHeader string) *Backend {
	p := httputil.NewSingleHostReverseProxy(u)
	orig := p.Director
	p.Director = func(req *http.Request) {
		orig(req)
		req.Host = hostHeader
	}
	return &Backend{
		URL: rawURL, Weight: weight, Container: container, HealthPath: healthPath, proxy: p,
		stickyID: backendStickyID(rawURL),
	}
}

// makePeerBackend builds a synthetic *Backend that forwards to another proxy
// instance in the mesh instead of a local container. routeHost/pathPrefix
// are the route's own identity (not the peer's) — pathPrefix and stripPrefix
// mirror the owning RouteGroup so the Director can restore the prefix that
// ServeHTTP already stripped from req.URL.Path before it ever reaches here
// (see the StripPrefix block above); without that restoration the peer's own
// ServeHTTP wouldn't be able to match/strip it itself. Returns nil if
// peerBaseURL doesn't parse.
//
// weight is the number of local backends the peer advertised for this route,
// so a spread group balances per-REPLICA rather than per-proxy: without it a
// peer running four replicas would receive the same share as one running a
// single replica. Values < 1 are floored to 1 by pickHealthy*, so a peer that
// advertises nothing usable still stays selectable as a failover backend.
func makePeerBackend(peerBaseURL, routeHost, pathPrefix string, stripPrefix bool, peerID string, weight int) *Backend {
	u, err := url.Parse(peerBaseURL)
	if err != nil {
		log.Printf("peer backend: bad URL %q: %v", peerBaseURL, err)
		return nil
	}
	p := httputil.NewSingleHostReverseProxy(u)
	orig := p.Director
	p.Director = func(req *http.Request) {
		orig(req)
		req.Host = routeHost
		if stripPrefix && pathPrefix != "" {
			req.URL.Path = pathPrefix + req.URL.Path
		}
		req.Header.Set(PeerHopHeader, "1")
	}
	if weight < 1 {
		weight = 1
	}
	return &Backend{
		URL: peerBaseURL, Weight: weight, Container: "peer:" + peerID, proxy: p,
		Learned: true, PeerID: peerID,
		stickyID: backendStickyID(peerBaseURL),
	}
}
