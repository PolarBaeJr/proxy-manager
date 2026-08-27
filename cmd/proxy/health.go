package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	healthInterval = 5 * time.Second
	healthTimeout  = 2 * time.Second
	// Dialing many backends at the exact same instant can overwhelm Tailscale's
	// connection setup for remote (non-Docker-network) backends: measured
	// empirically, 30+ simultaneous dials to a single Tailscale-routed peer
	// start timing out (10-40% failure), while a handful never do. Capping
	// in-flight checks keeps every tick well under that threshold regardless
	// of how many backends the mesh grows to.
	healthCheckConcurrency = 8
)

func runHealthChecks(ctx context.Context, r *Router) {
	tick := time.NewTicker(healthInterval)
	defer tick.Stop()
	sem := make(chan struct{}, healthCheckConcurrency)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			dispatchHealthChecks(ctx, r.Snapshot(), sem, nil)
		}
	}
}

// dispatchHealthChecks fans out one checkBackend call per backend across
// every group, gated by sem so at most healthCheckConcurrency run at once.
// wg is nil in production; tests pass one to await completion without
// depending on the real health-check ticker.
func dispatchHealthChecks(ctx context.Context, groups []*RouteGroup, sem chan struct{}, wg *sync.WaitGroup) {
	for _, g := range groups {
		for _, b := range g.Backends {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			if wg != nil {
				wg.Add(1)
			}
			go func() {
				defer func() { <-sem }()
				if wg != nil {
					defer wg.Done()
				}
				checkBackend(b)
			}()
		}
	}
}

func checkBackend(b *Backend) {
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()

	if b.HealthPath != "" {
		req, _ := http.NewRequestWithContext(ctx, "GET", b.URL+b.HealthPath, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("health check %s%s: %v (timeout budget %s) — recording failure", b.URL, b.HealthPath, err, healthTimeout)
			b.recordHealthCheck(false)
			return
		}
		resp.Body.Close()
		ok := resp.StatusCode/100 == 2
		if !ok {
			log.Printf("health check %s%s: status %d — recording failure", b.URL, b.HealthPath, resp.StatusCode)
		}
		b.recordHealthCheck(ok)
		return
	}
	// A learned (peer) backend has no HealthPath — deliberately: this bare TCP
	// dial only proves the peer proxy process is reachable at all, not that it
	// still serves this specific route. Route-level staleness is handled by
	// TTL expiry in PeerRouteStore.overlay, not health checks — don't invent a
	// route-specific health path here.
	u, _ := url.Parse(b.URL)
	d := net.Dialer{Timeout: healthTimeout}
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		log.Printf("health check dial %s: %v (timeout budget %s) — recording failure", b.URL, err, healthTimeout)
		b.recordHealthCheck(false)
		return
	}
	conn.Close()
	b.recordHealthCheck(true)
}
