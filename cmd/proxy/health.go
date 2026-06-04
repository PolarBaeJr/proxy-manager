package main

import (
	"context"
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
			b.markHealthy(false)
			return
		}
		resp.Body.Close()
		b.markHealthy(resp.StatusCode/100 == 2)
		return
	}
	u, _ := url.Parse(b.URL)
	d := net.Dialer{Timeout: healthTimeout}
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		b.markHealthy(false)
		return
	}
	conn.Close()
	b.markHealthy(true)
}
