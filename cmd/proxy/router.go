package main

import (
	"bufio"
	"context"
	"encoding/json"
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
)

type Backend struct {
	URL         string
	Weight      int
	Container   string
	HealthPath  string
	proxy       *httputil.ReverseProxy
	healthyFlag atomic.Bool
}

func (b *Backend) markHealthy(ok bool) { b.healthyFlag.Store(ok) }
func (b *Backend) healthy() bool       { return b.healthyFlag.Load() }

type RouteGroup struct {
	Host        string
	PathPrefix  string
	StripPrefix bool
	Name        string
	Service     string
	Backends    []*Backend

	AuthRequired bool
	AuthUsers    []string // lowercased; empty = any authenticated user
	AuthMode     string   // "" = sso (cookie or login redirect), "oauth" = bearer-first MCP mode

	RateLimit bool
	RateRPM   int
	limiter   *rateLimiter

	cursor atomic.Uint64

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

func (g *RouteGroup) pickHealthy(skip map[*Backend]bool) *Backend {
	var pool []*Backend
	for _, b := range g.Backends {
		if !b.healthy() || skip[b] {
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

type Router struct {
	mu     sync.RWMutex
	groups []*RouteGroup

	// limiters persist across refreshes, keyed by group key host+"|"+path, so
	// config edits don't reset per-IP buckets. Guarded by mu.
	limiters   map[string]*rateLimiter
	xffTrusted []*net.IPNet

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
		r.limiters = map[string]*rateLimiter{}
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
			rl := newRateLimiter(g.RateRPM)
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
	r.groups = groups
	r.mu.Unlock()

	prevHealth := map[string]bool{}
	for _, g := range prev {
		for _, b := range g.Backends {
			prevHealth[b.URL] = b.healthy()
		}
	}
	for _, g := range groups {
		for _, b := range g.Backends {
			if h, ok := prevHealth[b.URL]; ok {
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
	if group.StripPrefix && group.PathPrefix != "" {
		req.URL.Path = strings.TrimPrefix(req.URL.Path, group.PathPrefix)
		if !strings.HasPrefix(req.URL.Path, "/") {
			req.URL.Path = "/" + req.URL.Path
		}
	}

	tried := map[*Backend]bool{}
	for attempt := 0; attempt < maxRetries; attempt++ {
		b := group.pickHealthy(tried)
		if b == nil {
			break
		}
		tried[b] = true
		// Tell whichever wrapping writer cares (access log) which upstream we picked.
		// Interface-based so this file stays free of accesslog imports.
		if setter, ok := w.(interface{ SetBackend(string) }); ok {
			setter.SetBackend(b.URL)
		}
		if tryProxy(w, req, b) {
			return
		}
	}
	// Metrics are the raw truth — count every 503 honestly. The dashboard
	// UI is the layer that lets the operator choose to hide stopped-service
	// hosts from the Top hosts / error-rate view via a toggle.
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
	b.proxy.ErrorHandler = func(_ http.ResponseWriter, _ *http.Request, err error) {
		log.Printf("backend %s error: %v — marking unhealthy", b.URL, err)
		b.markHealthy(false)
		failed = true
	}
	b.proxy.ServeHTTP(rec, req)
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
	Service   string   `json:"service,omitempty"`
	Health    string   `json:"health,omitempty"`
	Auth      bool     `json:"auth,omitempty"`
	AuthUsers []string `json:"auth_users,omitempty"`
	AuthMode  string   `json:"auth_mode,omitempty"`
	RateLimit bool     `json:"ratelimit,omitempty"`
	RateRPM   int      `json:"ratelimit_rpm,omitempty"`
}

type staticConfig struct {
	Routes []staticRoute `json:"routes"`
}

func assembleGroups(ctx context.Context, dc *dockerClient, configPath string) ([]*RouteGroup, error) {
	groupsByKey := map[string]*RouteGroup{}

	if configPath != "" {
		if data, err := os.ReadFile(configPath); err == nil {
			var cfg staticConfig
			if err := json.Unmarshal(data, &cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", configPath, err)
			}
			for _, sr := range cfg.Routes {
				key := sr.Host + "|" + sr.Path
				g, ok := groupsByKey[key]
				if !ok {
					g = &RouteGroup{
						Host: sr.Host, PathPrefix: sr.Path, StripPrefix: sr.Strip, Name: sr.Name, Service: sr.Service,
						AuthRequired: sr.Auth, AuthUsers: normalizeAuthUsers(sr.AuthUsers), AuthMode: sr.AuthMode,
						RateLimit: sr.RateLimit, RateRPM: sr.RateRPM,
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
		return nil, err
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
	return out, nil
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

func makeBackend(rawURL string, weight int, container, healthPath string, u *url.URL, hostHeader string) *Backend {
	p := httputil.NewSingleHostReverseProxy(u)
	orig := p.Director
	p.Director = func(req *http.Request) {
		orig(req)
		req.Host = hostHeader
	}
	return &Backend{
		URL: rawURL, Weight: weight, Container: container, HealthPath: healthPath, proxy: p,
	}
}
