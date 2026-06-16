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
		serveUnavailable(w, http.StatusNotFound, reqHost, "There's no service routed at this address.")
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
	serveUnavailable(w, http.StatusServiceUnavailable, reqHost, "This service is offline or restarting. The page will reload automatically.")
}

// serveUnavailable writes a small styled HTML page with a meta-refresh so the
// browser retries periodically. Used when a host has no healthy backends (all
// replicas stopped, container crashed, etc.) or when the host has no route at
// all. The page is intentionally minimal — no JS, no external assets — so it
// works even when the only thing the proxy can do is fail.
func serveUnavailable(w http.ResponseWriter, status int, host, reason string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "10")
	w.WriteHeader(status)
	title := "Service unavailable"
	emoji := "⏳"
	if status == http.StatusNotFound {
		title = "Not found"
		emoji = "🚫"
	}
	fmt.Fprintf(w, `<!doctype html><html lang=en><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<meta http-equiv=refresh content="10">
<title>%s · %s</title>
<style>
  :root{color-scheme:dark}
  html,body{height:100%%;margin:0}
  body{background:#0a0a0a;color:#e6e6e6;font:14px/1.5 -apple-system,BlinkMacSystemFont,"Inter",system-ui,sans-serif;display:flex;align-items:center;justify-content:center;padding:24px}
  .box{max-width:460px;text-align:center;background:#141414;border:1px solid #262626;border-radius:14px;padding:36px 28px;box-shadow:0 1px 0 #ffffff08 inset,0 8px 30px #00000060}
  .ic{font-size:46px;line-height:1;margin-bottom:14px}
  h1{margin:0 0 8px;font-size:18px;font-weight:650;letter-spacing:-.01em}
  p{margin:0 0 18px;color:#a0a0a0}
  code{background:#1f1f1f;border:1px solid #2a2a2a;padding:2px 7px;border-radius:5px;font:12px/1 ui-monospace,SFMono-Regular,Menlo,monospace;color:#cfcfcf}
  .meta{margin-top:18px;font-size:11.5px;color:#6a6a6a;letter-spacing:.04em;text-transform:uppercase}
  .dot{display:inline-block;width:7px;height:7px;border-radius:50%%;background:#f7d976;margin-right:6px;vertical-align:middle;animation:p 1.4s infinite}
  @keyframes p{0%%,100%%{opacity:.35}50%%{opacity:1}}
</style>
<div class=box>
  <div class=ic>%s</div>
  <h1>%s</h1>
  <p>%s</p>
  <code>%s</code>
  <div class=meta><span class=dot></span>retrying in 10 s</div>
</div>
`, title, host, emoji, title, reason, host)
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
			log.Printf("skip %s: no IP on any network", name)
			continue
		}
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
		weight := 1
		if w, err := strconv.Atoi(c.Labels[labelWeight]); err == nil && w > 0 {
			weight = w
		}
		backendURL := fmt.Sprintf("http://%s:%d", ip, port)
		u, _ := url.Parse(backendURL)
		g.Backends = append(g.Backends, makeBackend(backendURL, weight, name, c.Labels[labelHealth], u, host))
	}

	out := make([]*RouteGroup, 0, len(groupsByKey))
	for _, g := range groupsByKey {
		sort.SliceStable(g.Backends, func(i, j int) bool { return g.Backends[i].URL < g.Backends[j].URL })
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
