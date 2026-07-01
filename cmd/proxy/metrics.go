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
		status[strconv.Itoa(k)] = v
	}
	method := copyMap(m.byMethod)
	hostStatus := map[string]map[string]uint64{}
	for h, statuses := range m.byHostStatus {
		hostStatus[h] = map[string]uint64{}
		for s, c := range statuses {
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

// metricsState is the on-disk snapshot of the counters that survive restarts
// (see persist.go). The latency ring is included so percentiles don't reset
// to zero on every deploy.
type metricsState struct {
	Version      int                       `json:"version"`
	SavedAt      time.Time                 `json:"saved_at"`
	Total        uint64                    `json:"total"`
	BytesOut     uint64                    `json:"bytes_out"`
	ByHost       map[string]uint64         `json:"by_host"`
	ByStatus     map[int]uint64            `json:"by_status"`
	ByMethod     map[string]uint64         `json:"by_method"`
	ByHostStatus map[string]map[int]uint64 `json:"by_host_status"`
	LatencyMs    []float64                 `json:"latency_ms"`
	LatencyHead  int                       `json:"latency_head"`
}

const metricsStateVersion = 1

// exportState deep-copies the counters into a serializable snapshot. No
// marshalling or I/O happens under the mutex — callers do that outside.
func (m *Metrics) exportState() metricsState {
	st := metricsState{
		Version:  metricsStateVersion,
		SavedAt:  time.Now().UTC(),
		Total:    m.Total.Load(),
		BytesOut: m.BytesOut.Load(),
	}
	m.mu.Lock()
	st.ByHost = copyMap(m.byHost)
	st.ByStatus = make(map[int]uint64, len(m.byStatus))
	for k, v := range m.byStatus {
		st.ByStatus[k] = v
	}
	st.ByMethod = copyMapStr(m.byMethod)
	st.ByHostStatus = make(map[string]map[int]uint64, len(m.byHostStatus))
	for h, statuses := range m.byHostStatus {
		inner := make(map[int]uint64, len(statuses))
		for s, c := range statuses {
			inner[s] = c
		}
		st.ByHostStatus[h] = inner
	}
	st.LatencyMs = append([]float64(nil), m.latencyMs...)
	st.LatencyHead = m.latencyHead
	m.mu.Unlock()
	return st
}

// restoreState installs a previously-saved snapshot. StartedAt is deliberately
// NOT restored: "total" now spans restarts while uptime_seconds stays
// per-process, so total can legitimately exceed what the uptime implies.
func (m *Metrics) restoreState(st metricsState) {
	m.Total.Store(st.Total)
	m.BytesOut.Store(st.BytesOut)
	m.mu.Lock()
	if st.ByHost != nil {
		m.byHost = st.ByHost
	}
	if st.ByStatus != nil {
		m.byStatus = st.ByStatus
	}
	if st.ByMethod != nil {
		m.byMethod = st.ByMethod
	}
	if st.ByHostStatus != nil {
		m.byHostStatus = st.ByHostStatus
	}
	if st.LatencyMs != nil {
		if len(st.LatencyMs) > latencyWindow {
			st.LatencyMs = st.LatencyMs[:latencyWindow]
		}
		m.latencyMs = st.LatencyMs
		if st.LatencyHead >= 0 && st.LatencyHead < len(st.LatencyMs) {
			m.latencyHead = st.LatencyHead
		}
	}
	m.mu.Unlock()
}

func copyMap(in map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func maxOr0(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	return s[len(s)-1]
}

// metricsServer starts an HTTP server on addr exposing /metrics (JSON),
// /access (per-request log ring), and /refresh (rebuild the router from
// labels + routes.json on demand). Bind to internal addresses only.
func metricsServer(addr string, m *Metrics, a *AccessLog, refresh func()) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.Snapshot())
	})
	if a != nil {
		mux.HandleFunc("/access", accessHandler(a))
	}
	if refresh != nil {
		mux.HandleFunc("/refresh", func(w http.ResponseWriter, _ *http.Request) {
			refresh()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"refreshed"}`))
		})
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// metrics is non-fatal — log via the default logger and continue.
		}
	}()
}
