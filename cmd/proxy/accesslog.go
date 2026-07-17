package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AccessLog keeps the last N request records in a lock-free-ish ring buffer.
// Deliberately RAM-only — unlike the counters in metrics.go, the ring is not
// persisted across restarts (recent-requests debugging data, not history).
// Writes are mu-protected; reads copy out the live window. Memory cap is
// ~ringSize × ~200B → a few hundred KB, fine for our scale.
const ringSize = 2000

type AccessEntry struct {
	Time      int64  `json:"t"`         // epoch milliseconds
	Method    string `json:"method"`
	Host      string `json:"host"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	Bytes     int64  `json:"bytes"`
	LatencyMs int64  `json:"ms"`        // wall clock from request enter to handler return
	ClientIP  string `json:"ip"`        // first X-Forwarded-For hop, else RemoteAddr
	Backend   string `json:"backend"`   // URL of the upstream picked, "-" on no-route
	UA        string `json:"ua,omitempty"`
}

type AccessLog struct {
	mu  sync.RWMutex
	buf [ringSize]AccessEntry
	pos uint64 // next write slot; ID of next entry. Reads use the snapshot of pos.
	seq atomic.Uint64
}

func NewAccessLog() *AccessLog { return &AccessLog{} }

func (a *AccessLog) Record(e AccessEntry) {
	a.mu.Lock()
	idx := a.pos % ringSize
	a.buf[idx] = e
	a.pos++
	a.mu.Unlock()
	a.seq.Add(1)
}

// Snapshot returns up to `limit` most-recent entries, newest first. If `since`
// is > 0 only entries strictly newer than that epoch-ms timestamp are returned
// (useful for incremental polling). Limit is capped at ringSize.
func (a *AccessLog) Snapshot(limit int, since int64) []AccessEntry {
	if limit <= 0 || limit > ringSize {
		limit = ringSize
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	total := a.pos
	if total == 0 {
		return []AccessEntry{}
	}
	// Walk backwards from newest until we hit limit or `since` or buffer wrap.
	count := uint64(ringSize)
	if total < count {
		count = total
	}
	out := make([]AccessEntry, 0, limit)
	for i := uint64(0); i < count && len(out) < limit; i++ {
		idx := (a.pos - 1 - i) % ringSize
		e := a.buf[idx]
		if e.Time <= since {
			break
		}
		out = append(out, e)
	}
	return out
}

// accessWriter captures status, bytes, the backend URL the router picked, and
// whether the request matched no route (so the metrics layer can collapse that
// traffic into one bucket). It implements SetBackend so the router can attach
// the upstream identity without depending on the access-log type directly.
type accessWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	backend  string
	unrouted bool
}

// Hijack forwards to the embedded ResponseWriter so WebSocket / protocol
// upgrades survive this wrapper. accessWriter embeds the http.ResponseWriter
// *interface*, so Hijack isn't promoted even though the concrete server writer
// implements it — without this method *accessWriter fails the http.Hijacker
// assertion, the upgrade fails, and the reverse proxy's ErrorHandler falsely
// marks the backend unhealthy. This is the outer half of the fix whose inner
// half lives on errCatchingWriter in router.go.
func (w *accessWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("proxy: underlying ResponseWriter does not support hijacking")
	}
	// A hijacked connection switches protocols (101) and its bytes flow
	// directly over the raw conn, bypassing this wrapper's accounting.
	w.status = http.StatusSwitchingProtocols
	return hj.Hijack()
}

func (w *accessWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *accessWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}
func (w *accessWriter) SetBackend(url string) { w.backend = url }
func (w *accessWriter) MarkUnrouted()         { w.unrouted = true }

// withAccessLog wraps a handler, recording one AccessEntry per request after
// the handler returns. Skips its own /access endpoint (handled on the metrics
// listener), but the proxy serves user traffic only, so there's nothing to skip.
func withAccessLog(next http.Handler, log *AccessLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If withMetrics already wrapped us, unwrap to a single capture writer.
		aw, ok := w.(*accessWriter)
		if !ok {
			aw = &accessWriter{ResponseWriter: w, status: 0}
		}
		start := time.Now()
		next.ServeHTTP(aw, r)
		host := r.Host
		if i := strings.IndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		status := aw.status
		if status == 0 {
			status = 200
		}
		backend := aw.backend
		if backend == "" {
			backend = "-"
		}
		log.Record(AccessEntry{
			Time:      start.UnixMilli(),
			Method:    r.Method,
			Host:      host,
			Path:      r.URL.Path,
			Status:    status,
			Bytes:     aw.bytes,
			LatencyMs: time.Since(start).Milliseconds(),
			ClientIP:  clientIP(r),
			Backend:   backend,
			UA:        truncate(r.UserAgent(), 160),
		})
	})
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		return strings.TrimSpace(xff)
	}
	if r.RemoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// accessHandler returns the /access endpoint. Query: ?limit=N&since=epoch_ms.
// Mounted on the internal metrics listener — no public exposure.
func accessHandler(a *AccessLog) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		since := int64(0)
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		if v := r.URL.Query().Get("since"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				since = n
			}
		}
		entries := a.Snapshot(limit, since)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"now":     time.Now().UnixMilli(),
			"total":   a.seq.Load(),
			"count":   len(entries),
			"entries": entries,
		})
	}
}

