package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	healthInterval = 5 * time.Second
	healthTimeout  = 2 * time.Second
)

func runHealthChecks(ctx context.Context, r *Router) {
	tick := time.NewTicker(healthInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			for _, g := range r.Snapshot() {
				for _, b := range g.Backends {
					go checkBackend(b)
				}
			}
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
			log.Printf("health check DIAG %s%s: dial/req error: %v (timeout budget %s) — marking unhealthy", b.URL, b.HealthPath, err, healthTimeout)
			b.markHealthy(false)
			return
		}
		resp.Body.Close()
		ok := resp.StatusCode/100 == 2
		log.Printf("health check DIAG %s%s: status %d ok=%v", b.URL, b.HealthPath, resp.StatusCode, ok)
		b.markHealthy(ok)
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
		log.Printf("health check DIAG dial %s: fail: %v (timeout budget %s) — marking unhealthy", b.URL, err, healthTimeout)
		b.markHealthy(false)
		return
	}
	conn.Close()
	log.Printf("health check DIAG dial %s: ok", b.URL)
	b.markHealthy(true)
}
