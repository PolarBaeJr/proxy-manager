package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Bounds on the per-route micro-cache. It exists to absorb bursts of
// identical anonymous GETs (a shared link, a crawler hammering one page),
// not to be a CDN — a 1 MiB per-body cap and a 32 MiB per-route ceiling keep
// it from ever being a memory concern on the Pi.
const (
	cacheMaxBody    = 1 << 20
	cacheMaxEntries = 1024
	cacheMaxBytes   = 32 << 20
)

type cacheEntry struct {
	status   int
	header   http.Header
	body     []byte
	storedAt time.Time
}

// cacheFill marks a key whose response is currently being fetched from the
// backend; coalesced requests park on done. waiters counts them so a test
// can wait for every coalesced request to actually be parked before
// releasing the backend, instead of sleeping and hoping. Always handled by
// pointer (holds an atomic).
type cacheFill struct {
	done    chan struct{}
	waiters atomic.Int32
}

// responseCache is one route group's store. It outlives the RouteGroup it
// hangs off: every refresh builds fresh *RouteGroups, and Router.Set
// re-attaches the same *responseCache from Router.caches, the same way it
// preserves limiters. now is overridable so tests can drive TTL expiry.
type responseCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	entries  map[string]*cacheEntry
	inflight map[string]*cacheFill
	bytes    int64
	now      func() time.Time
}

func newResponseCache(ttl time.Duration) *responseCache {
	return &responseCache{
		ttl:      ttl,
		entries:  map[string]*cacheEntry{},
		inflight: map[string]*cacheFill{},
		now:      time.Now,
	}
}

// setTTL flushes the store on a TTL change: entries stored under the old TTL
// would otherwise live for the wrong lifetime in either direction (a 1m→5s
// edit would keep serving minute-old bodies). In-flight fills are left
// alone — they finish and store under the new TTL.
func (c *responseCache) setTTL(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d != c.ttl {
		c.ttl = d
		c.entries = map[string]*cacheEntry{}
		c.bytes = 0
	}
}

func (c *responseCache) get(key string, now time.Time) (*cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !now.Before(e.storedAt.Add(c.ttl)) {
		delete(c.entries, key)
		c.bytes -= int64(len(e.body))
		return nil, false
	}
	return e, true
}

// put stores e (storedAt already set by the caller) and, only on the put
// that crosses a bound, evicts back under both bounds.
func (c *responseCache) put(key string, e *cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[key]; ok {
		c.bytes -= int64(len(old.body))
	}
	c.entries[key] = e
	c.bytes += int64(len(e.body))
	if len(c.entries) > cacheMaxEntries || c.bytes > cacheMaxBytes {
		c.evictLocked(c.now())
	}
}

// evictLocked drops expired entries first (dead weight regardless of the
// bounds), then oldest-first until both bounds hold. Linear scans are fine
// at cacheMaxEntries and eviction only runs when a put crosses a bound.
func (c *responseCache) evictLocked(now time.Time) {
	for k, e := range c.entries {
		if !now.Before(e.storedAt.Add(c.ttl)) {
			delete(c.entries, k)
			c.bytes -= int64(len(e.body))
		}
	}
	for len(c.entries) > cacheMaxEntries || c.bytes > cacheMaxBytes {
		var oldestKey string
		var oldest *cacheEntry
		for k, e := range c.entries {
			if oldest == nil || e.storedAt.Before(oldest.storedAt) {
				oldestKey, oldest = k, e
			}
		}
		delete(c.entries, oldestKey)
		c.bytes -= int64(len(oldest.body))
	}
}

// beginFill claims key for the caller if nobody is already fetching it.
// Returns the existing fill and false otherwise, so the caller can park on
// its done channel.
func (c *responseCache) beginFill(key string) (*cacheFill, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.inflight[key]; ok {
		return f, false
	}
	f := &cacheFill{done: make(chan struct{})}
	c.inflight[key] = f
	return f, true
}

// endFill releases the waiters. The identity check guards against a stale
// owner clearing a newer fill for the same key.
func (c *responseCache) endFill(key string, f *cacheFill) {
	c.mu.Lock()
	if c.inflight[key] == f {
		delete(c.inflight, key)
	}
	c.mu.Unlock()
	close(f.done)
}

// normalizeAcceptEncoding reduces a client Accept-Encoding to the sorted,
// deduplicated set of codings the cache distinguishes, so "gzip, deflate,
// br" and "br;q=1.0, gzip" share one entry rather than fragmenting the
// cache per browser fingerprint. Only codings a backend plausibly
// negotiates on are kept; identity and unknowns collapse to "".
func normalizeAcceptEncoding(s string) string {
	seen := map[string]bool{}
	var out []string
	for _, tok := range strings.Split(s, ",") {
		parts := strings.Split(tok, ";")
		name := strings.ToLower(strings.TrimSpace(parts[0]))
		switch name {
		case "br", "deflate", "gzip", "zstd":
		default:
			continue
		}
		drop := false
		for _, p := range parts[1:] {
			if q, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(p)), "q="); ok {
				if v, err := strconv.ParseFloat(q, 64); err == nil && v == 0 {
					drop = true
				}
			}
		}
		if drop || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// cacheKey is keyed on the ORIGINAL client path (before StripPrefix) so the
// key is what the client asked for, not what the backend saw. NUL separates
// the fields because it can never survive URL/host parsing, so no host or
// path can be crafted to collide with a different (host, path) pair.
func cacheKey(host, path, rawQuery, acceptEncoding string) string {
	return host + "\x00" + path + "?" + rawQuery + "\x00" + normalizeAcceptEncoding(acceptEncoding)
}

// cacheRequestEligible decides whether a request may be answered from (or
// stored into) the cache at all. Anything that could make the response
// personal — a Cookie (any, including the SSO cookie), Authorization, or a
// Range — bypasses, so the cache can never serve one user's response to
// another. Cookie is checked as a slice, not via Get: an empty "Cookie:"
// header still means the client is sending cookies. Client Cache-Control:
// no-cache / Pragma are deliberately ignored — honoring them would let any
// client bypass the cache and defeat the burst protection this exists for.
func cacheRequestEligible(req *http.Request, g *RouteGroup, origPath string) bool {
	if g.cache == nil || g.CacheTTL <= 0 {
		return false
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	if req.Header.Get("Authorization") != "" || len(req.Header["Cookie"]) != 0 || req.Header.Get("Range") != "" {
		return false
	}
	if len(g.CachePaths) == 0 {
		return true
	}
	for _, p := range g.CachePaths {
		if strings.HasPrefix(origPath, p) {
			return true
		}
	}
	return false
}

// cacheResponseStorable is the backend's side of the personalisation guard:
// a Set-Cookie, a private/no-store/no-cache directive, or a Vary on anything
// but Accept-Encoding all mean the body may differ per client. max-age is
// deliberately not read — the route's TTL is the operator's call, not the
// backend's.
func cacheResponseStorable(h http.Header) bool {
	if len(h["Set-Cookie"]) != 0 {
		return false
	}
	for _, v := range h.Values("Cache-Control") {
		for _, tok := range strings.Split(v, ",") {
			name, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tok)), "=")
			switch name {
			case "private", "no-store", "no-cache":
				return false
			}
		}
	}
	for _, v := range h.Values("Vary") {
		for _, tok := range strings.Split(v, ",") {
			tok = strings.ToLower(strings.TrimSpace(tok))
			if tok != "" && tok != "accept-encoding" {
				return false
			}
		}
	}
	return true
}

// cloneStorableHeader snapshots a response header for storage minus
// everything that describes THIS transfer rather than the resource:
// hop-by-hop headers (ReverseProxy already strips these, but the stored
// copy is replayed by a different writer, so be defensive), Content-Length
// (recomputed from the stored body), and the cache's own X-Cache/Age.
func cloneStorableHeader(h http.Header) http.Header {
	out := h.Clone()
	for _, v := range out.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out.Del(name)
			}
		}
	}
	for _, k := range []string{
		"Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade",
		"Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
		"Trailer", "Te", "Content-Length", "X-Cache", "Age",
	} {
		out.Del(k)
	}
	return out
}

// parseCacheTTL parses the proxy.cache label / routes.json "cache" value.
// The off spellings mirror the boolean labels so "false" on a cache label
// reads the way an operator expects.
func parseCacheTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "0", "false", "off":
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("negative duration %q", s)
	}
	return d, nil
}

// unionStrings is an order-preserving dedupe of a then b, trimming and
// dropping empties. Returns nil (not an empty slice) when nothing survives,
// matching splitTrimmed's convention for an unset label.
func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string(nil), a...), b...) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// serveCached replays a stored entry. SetBackend("cache") only ever reaches
// the access log's Backend column — Metrics.Record never sees it and health
// iterates g.Backends, so no other consumer can mistake it for a real
// upstream.
func serveCached(w http.ResponseWriter, req *http.Request, e *cacheEntry, now time.Time) {
	h := w.Header()
	for k, vs := range e.header {
		h[k] = append([]string(nil), vs...)
	}
	age := int(now.Sub(e.storedAt) / time.Second)
	if age < 0 {
		age = 0
	}
	h.Set("Content-Length", strconv.Itoa(len(e.body)))
	h.Set("Age", strconv.Itoa(age))
	h.Set("X-Cache", "HIT")
	if setter, ok := w.(interface{ SetBackend(string) }); ok {
		setter.SetBackend("cache")
	}
	w.WriteHeader(e.status)
	if req.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(e.body)
}

// cacheRecorder sits between the access-log writer and tryProxy's
// errCatchingWriter, stamping X-Cache and (on a MISS) buffering a storable
// 200 body as it streams through. Header() is deliberately NOT overridden:
// the embedded writer's map is what setStickyCookie and ReverseProxy both
// mutate, and the recorder reads it at WriteHeader time.
type cacheRecorder struct {
	http.ResponseWriter
	mode        string // "MISS" or "BYPASS"
	record      bool
	wroteHeader bool
	status      int
	storable    bool
	header      http.Header
	buf         []byte
	overflow    bool
	// declaredLen is the backend's Content-Length (-1 if absent), checked
	// against the bytes actually recorded: a backend that dies mid-body
	// leaves a short body that must never be stored as the whole resource.
	declaredLen int64
}

// WriteHeader finalizes on the first final (>= 200) status only: ReverseProxy
// forwards 1xx interim responses through WriteHeader too, and those must
// pass straight through without stamping or snapshotting anything.
func (w *cacheRecorder) WriteHeader(code int) {
	if code < 200 || w.wroteHeader {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.wroteHeader = true
	w.status = code
	// Set, not Add: ReverseProxy copies upstream headers into this same map,
	// and an upstream that itself emits X-Cache must not produce two values.
	w.Header().Set("X-Cache", w.mode)
	if w.record {
		w.storable = code == http.StatusOK && cacheResponseStorable(w.Header())
		if w.storable {
			w.header = cloneStorableHeader(w.Header())
			w.declaredLen = -1
			if cl := w.Header().Get("Content-Length"); cl != "" {
				if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
					w.declaredLen = n
				}
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cacheRecorder) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.storable && !w.overflow {
		if len(w.buf)+len(p) > cacheMaxBody {
			w.overflow = true
			w.buf = nil
		} else {
			w.buf = append(w.buf, p...)
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *cacheRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards for the same reason errCatchingWriter.Hijack does: a
// WebSocket upgrade on a cached route must still be able to switch
// protocols, or the failed upgrade falsely marks the backend unhealthy.
func (w *cacheRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("proxy: underlying ResponseWriter does not support hijacking")
}

// SetBackend forwards so proxyToGroup's attribution still reaches the
// access-log writer underneath.
func (w *cacheRecorder) SetBackend(s string) {
	if setter, ok := w.ResponseWriter.(interface{ SetBackend(string) }); ok {
		setter.SetBackend(s)
	}
}

// entry is non-nil only for a complete, storable, under-cap 200 body;
// storedAt is the caller's to fill. A body shorter than the backend
// declared is a truncated transfer, not a resource.
func (w *cacheRecorder) entry() *cacheEntry {
	if !w.record || !w.wroteHeader || !w.storable || w.overflow {
		return nil
	}
	if w.declaredLen >= 0 && int64(len(w.buf)) != w.declaredLen {
		return nil
	}
	return &cacheEntry{status: w.status, header: w.header, body: w.buf}
}
