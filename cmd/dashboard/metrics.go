package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Same shape as proxy/metrics.go + edge/metrics.go — copied per-binary so each
// module stays standalone. Exposes the same fields the monitor expects.

type Metrics struct {
	mu sync.Mutex

	StartedAt time.Time
	Total     atomic.Uint64
	BytesOut  atomic.Uint64
	InFlight  atomic.Int64

	byHost       map[string]uint64
	byStatus     map[int]uint64
	byMethod     map[string]uint64
	byHostStatus map[string]map[int]uint64

	latencyMs   []float64
	latencyHead int
}

const latencyWindow = 1000

func NewMetrics() *Metrics {
	return &Metrics{
		StartedAt:    time.Now(),
		byHost:       map[string]uint64{},
		byStatus:     map[int]uint64{},
		byMethod:     map[string]uint64{},
		byHostStatus: map[string]map[int]uint64{},
		latencyMs:    make([]float64, 0, latencyWindow),
	}
}

func (m *Metrics) Record(host, method string, status int, bytes int64, dur time.Duration) {
	m.Total.Add(1)
	if bytes > 0 {
		m.BytesOut.Add(uint64(bytes))
	}
	m.mu.Lock()
	m.byHost[host]++
	m.byStatus[status]++
	m.byMethod[method]++
	if m.byHostStatus[host] == nil {
		m.byHostStatus[host] = map[int]uint64{}
	}
	m.byHostStatus[host][status]++
	ms := float64(dur.Microseconds()) / 1000.0
	if len(m.latencyMs) < latencyWindow {
		m.latencyMs = append(m.latencyMs, ms)
	} else {
		m.latencyMs[m.latencyHead] = ms
		m.latencyHead = (m.latencyHead + 1) % latencyWindow
	}
	m.mu.Unlock()
}

func (m *Metrics) Snapshot() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	host := map[string]uint64{}
	for k, v := range m.byHost {
		host[k] = v
	}
	status := map[string]uint64{}
	for k, v := range m.byStatus {
		status[strconv.Itoa(k)] = v
	}
	method := map[string]uint64{}
	for k, v := range m.byMethod {
		method[k] = v
	}
	hostStatus := map[string]map[string]uint64{}
	for h, sts := range m.byHostStatus {
		hostStatus[h] = map[string]uint64{}
		for s, c := range sts {
			hostStatus[h][strconv.Itoa(s)] = c
		}
	}
	lat := append([]float64(nil), m.latencyMs...)
	sort.Float64s(lat)
	pct := func(p float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		i := int(float64(len(lat)) * p)
		if i >= len(lat) {
			i = len(lat) - 1
		}
		return lat[i]
	}
	maxV := 0.0
	if len(lat) > 0 {
		maxV = lat[len(lat)-1]
	}
	return map[string]any{
		"started_at":     m.StartedAt.UTC().Format(time.RFC3339),
		"uptime_seconds": int64(time.Since(m.StartedAt).Seconds()),
		"total":          m.Total.Load(),
		"bytes_out":      m.BytesOut.Load(),
		"in_flight":      m.InFlight.Load(),
		"by_host":        host,
		"by_status":      status,
		"by_method":      method,
		"by_host_status": hostStatus,
		"latency_ms": map[string]float64{
			"p50": pct(0.50), "p90": pct(0.90), "p95": pct(0.95), "p99": pct(0.99), "max": maxV,
		},
		"sample_size": len(lat),
	}
}

// metricsServer exposes /metrics on a separate listener, distinct from the
// public dashboard port. Bind to internal addresses only.
func metricsServer(addr string, m *Metrics) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.Snapshot())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.ListenAndServe() }()
}

// withMetrics records per-request counters + latency for every request handled
// by the wrapped handler.
func withMetrics(next http.Handler, m *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.InFlight.Add(1)
		defer m.InFlight.Add(-1)
		start := time.Now()
		mw := &metricsWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(mw, r)
		host := r.Host
		for i := 0; i < len(host); i++ {
			if host[i] == ':' {
				host = host[:i]
				break
			}
		}
		m.Record(host, r.Method, mw.status, mw.bytes, time.Since(start))
	})
}

type metricsWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *metricsWriter) WriteHeader(c int) {
	w.status = c
	w.ResponseWriter.WriteHeader(c)
}
func (w *metricsWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}
