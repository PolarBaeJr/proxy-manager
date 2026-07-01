// proxy: request-path only. Reverse proxy + load balancer + health checks.
// Read-only access to the Docker socket. No auth, no management endpoints.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":8092", "proxy listen address")
	metricsAddr := flag.String("metrics-addr", ":8094", "internal metrics endpoint listen address")
	staticConfig := flag.String("config", "/etc/proxy/routes.json", "static routes JSON (ignored if missing)")
	flag.Parse()

	metrics := NewMetrics()
	access := NewAccessLog()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dc := newDockerClient()
	router := &Router{}
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
	metricsServer(*metricsAddr, metrics, access, refresh)
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
		if i := indexByte(host, ':'); i >= 0 {
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

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
