package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// In-process metrics for the request path. Exposed read-only on a separate
// listener (-metrics-addr) so the public proxy port doesn't leak internals.

type Metrics struct {
	mu sync.Mutex

	StartedAt    time.Time
	Total        atomic.Uint64
	BytesOut     atomic.Uint64
	InFlight     atomic.Int64

	byHost       map[string]uint64
	byStatus     map[int]uint64
	byMethod     map[string]uint64
	byHostStatus map[string]map[int]uint64 // host → status → count

	// Latency reservoir — last 1000 observations. Cheap percentile estimation.
	latencyMs    []float64
	latencyHead  int
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

// Snapshot returns a JSON-serializable view of current metrics.
func (m *Metrics) Snapshot() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	host := copyMap(m.byHost)
	status := map[string]uint64{}
	for k, v := range m.byStatus {
		status[itoa(k)] = v
	}
	method := copyMapStr(m.byMethod)
	hostStatus := map[string]map[string]uint64{}
	for h, statuses := range m.byHostStatus {
		hostStatus[h] = map[string]uint64{}
		for s, c := range statuses {
			hostStatus[h][itoa(s)] = c
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
			"p50": pct(0.50),
			"p90": pct(0.90),
			"p95": pct(0.95),
			"p99": pct(0.99),
			"max": maxOr0(lat),
		},
		"sample_size": len(lat),
	}
}

func copyMap(in map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func copyMapStr(in map[string]uint64) map[string]uint64 { return copyMap(in) }
func maxOr0(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	return s[len(s)-1]
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = '0' + byte(n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// metricsServer starts an HTTP server on addr exposing /metrics (JSON).
// Bind to internal addresses only; do NOT expose publicly.
func metricsServer(addr string, m *Metrics) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.Snapshot())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// metrics is non-fatal — log via the default logger and continue.
		}
	}()
}
