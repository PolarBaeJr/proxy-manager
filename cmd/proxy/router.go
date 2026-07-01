package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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

	// UserStopped is true when every matched container is in "exited" state
	// (Docker stop / user-initiated). Crashes end up in "dead" or repeatedly
	// "restarting" and leave this false. Only groups with UserStopped=true
	// skip metric recording on the resulting 503 — crashes still count as
	// errors so the operator sees them on the dashboard.
	UserStopped bool

	cursor atomic.Uint64
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
}

func (r *Router) Set(groups []*RouteGroup) {
	sort.SliceStable(groups, func(i, j int) bool {
		return len(groups[i].PathPrefix) > len(groups[j].PathPrefix)
	})
	r.mu.Lock()
	prev := r.groups
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

const maxRetries = 3

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	groups := r.groups
	r.mu.RUnlock()

	reqHost := hostOnly(req.Host)
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
		serveUnavailable(w, http.StatusNotFound, reqHost, "Service unavailable at this time, try again later.")
		return
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
	// Only skip metrics for a user-stopped service (every member cleanly
	// exited). Crashes leave containers in "dead" / "restarting" / a mix,
	// so UserStopped stays false and those 503s are counted as errors —
	// that's the signal the operator needs to notice a crash.
	if group.UserStopped {
		if m, ok := w.(interface{ MarkStructural() }); ok {
			m.MarkStructural()
		}
	}
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

// ---- Assembly: docker labels + static config ----

type staticRoute struct {
	Host     string   `json:"host"`
	Path     string   `json:"path,omitempty"`
	Strip    bool     `json:"strip,omitempty"`
	Name     string   `json:"name,omitempty"`
	Backends []string `json:"backends"`
	Health   string   `json:"health,omitempty"`
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
					g = &RouteGroup{Host: sr.Host, PathPrefix: sr.Path, StripPrefix: sr.Strip, Name: sr.Name}
					groupsByKey[key] = g
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

	containers, err := dc.listEnabledContainers(ctx)
	if err != nil {
		return nil, err
	}
	// Per-group state tally so we can decide user-stopped vs crashed for the
	// no-live-backends case. Only groups with EVERY member in "exited" are
	// considered user-stopped; a "dead" or "restarting" member means we treat
	// the whole group as crashed and still count 503s as errors.
	groupExited := map[string]int{}
	groupOther := map[string]int{}
	groupRunning := map[string]int{}
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
		// Always register the route group so a stopped container's host
		// still maps to *something* — that's what turns the user-visible
		// page from 404 (no such host) into 503 (host exists, nothing
		// healthy). Backends are only appended for containers that are
		// running AND have a reachable IP.
		path := c.Labels[labelPath]
		key := host + "|" + path
		g, ok := groupsByKey[key]
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
		switch c.State {
		case "running":
			groupRunning[key]++
		case "exited":
			groupExited[key]++
		default:
			// created, paused, restarting, dead, removing → not a clean stop
			groupOther[key]++
		}
		if c.State != "running" {
			continue
		}
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
			continue
		}
		weight := 1
		if w, err := strconv.Atoi(c.Labels[labelWeight]); err == nil && w > 0 {
			weight = w
		}
		backendURL := fmt.Sprintf("http://%s:%d", ip, port)
		u, _ := url.Parse(backendURL)
		g.Backends = append(g.Backends, makeBackend(backendURL, weight, name, c.Labels[labelHealth], u, host))
	}

	out := make([]*RouteGroup, 0, len(groupsByKey))
	for key, g := range groupsByKey {
		sort.SliceStable(g.Backends, func(i, j int) bool { return g.Backends[i].URL < g.Backends[j].URL })
		// UserStopped only when every matched container is cleanly exited —
		// zero running, zero in an unusual state (dead/restarting/removing).
		g.UserStopped = groupRunning[key] == 0 && groupOther[key] == 0 && groupExited[key] > 0
		out = append(out, g)
	}
	return out, nil
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
