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
	metricsServer(*metricsAddr, metrics)
	log.Printf("metrics on %s/metrics", *metricsAddr)

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

	go dc.streamEvents(ctx, func(action string) {
		switch action {
		case "start", "die", "destroy", "kill", "stop":
			refresh()
		}
	})
	go runHealthChecks(ctx, router)

	log.Printf("proxy on %s", *addr)
	if err := http.ListenAndServe(*addr, withMetrics(router, metrics)); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// withMetrics wraps the router to record per-request counters + latency.
func withMetrics(next http.Handler, m *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.InFlight.Add(1)
		defer m.InFlight.Add(-1)
		start := time.Now()
		mw := &metricsWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(mw, r)
		host := r.Host
		if i := indexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		m.Record(host, r.Method, mw.status, mw.bytes, time.Since(start))
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

type metricsWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (m *metricsWriter) WriteHeader(code int) {
	m.status = code
	m.ResponseWriter.WriteHeader(code)
}
func (m *metricsWriter) Write(b []byte) (int, error) {
	n, err := m.ResponseWriter.Write(b)
	m.bytes += int64(n)
	return n, err
}
