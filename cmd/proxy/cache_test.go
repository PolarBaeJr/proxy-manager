package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newCacheRouter builds a single-backend cached route. CacheTTL is set
// BEFORE Router.Set because Set is what creates g.cache; callers that need
// a controllable clock override g.cache.now afterwards.
func newCacheRouter(t *testing.T, host string, ttl time.Duration, target string) (*Router, *RouteGroup) {
	t.Helper()
	g := mkGroup(t, host, "", false, target)
	g.CacheTTL = ttl
	r := &Router{}
	r.Set([]*RouteGroup{g})
	return r, g
}

// doCached issues one request through the same accessWriter wrapping
// production uses, so SetBackend("cache") attribution is observable.
func doCached(r *Router, method, target string, hdr map[string]string) (*httptest.ResponseRecorder, *accessWriter) {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	aw := &accessWriter{ResponseWriter: rec}
	r.ServeHTTP(aw, req)
	return rec, aw
}

func countingBackend(t *testing.T, hits *atomic.Int32, h func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCacheStorabilityMatrix(t *testing.T) {
	big := strings.Repeat("x", cacheMaxBody+1)
	cases := []struct {
		name   string
		status int
		hdr    map[string]string
		body   string
		stored bool
	}{
		{"plain 200", 200, nil, "ok", true},
		{"404", 404, nil, "nope", false},
		{"500", 500, nil, "boom", false},
		{"301", 301, map[string]string{"Location": "/x"}, "", false},
		{"set-cookie", 200, map[string]string{"Set-Cookie": "s=1"}, "ok", false},
		{"cc private", 200, map[string]string{"Cache-Control": "private"}, "ok", false},
		{"cc no-store", 200, map[string]string{"Cache-Control": "no-store"}, "ok", false},
		{"cc no-cache", 200, map[string]string{"Cache-Control": "no-cache"}, "ok", false},
		{"cc public max-age", 200, map[string]string{"Cache-Control": "public, max-age=60"}, "ok", true},
		{"vary accept-encoding", 200, map[string]string{"Vary": "Accept-Encoding"}, "ok", true},
		{"vary accept-encoding twice", 200, map[string]string{"Vary": "accept-encoding, Accept-Encoding"}, "ok", true},
		{"vary cookie", 200, map[string]string{"Vary": "Cookie"}, "ok", false},
		{"vary rsc", 200, map[string]string{"Vary": "rsc, next-router-state-tree"}, "ok", false},
		{"vary star", 200, map[string]string{"Vary": "*"}, "ok", false},
		{"over cap", 200, nil, big, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := countingBackend(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.hdr {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			r, _ := newCacheRouter(t, "m.example.org", 5*time.Second, srv.URL)

			rec1, _ := doCached(r, "GET", "http://m.example.org/p", nil)
			if rec1.Header().Get("X-Cache") != "MISS" {
				t.Fatalf("first X-Cache = %q, want MISS", rec1.Header().Get("X-Cache"))
			}
			if rec1.Body.Len() != len(tc.body) {
				t.Fatalf("first body len = %d, want %d (recorder must forward every byte even past the cap)", rec1.Body.Len(), len(tc.body))
			}
			rec2, _ := doCached(r, "GET", "http://m.example.org/p", nil)
			wantX, wantHits := "MISS", int32(2)
			if tc.stored {
				wantX, wantHits = "HIT", 1
			}
			if got := rec2.Header().Get("X-Cache"); got != wantX {
				t.Fatalf("second X-Cache = %q, want %s", got, wantX)
			}
			if hits.Load() != wantHits {
				t.Fatalf("backend hits = %d, want %d", hits.Load(), wantHits)
			}
		})
	}
}

func TestCacheRequestBypassMatrix(t *testing.T) {
	var hits atomic.Int32
	srv := countingBackend(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	cases := []struct {
		name   string
		method string
		path   string
		hdr    map[string]string
	}{
		{"POST", "POST", "/api/x", nil},
		{"Cookie", "GET", "/api/x", map[string]string{"Cookie": "a=b"}},
		{"Authorization", "GET", "/api/x", map[string]string{"Authorization": "Bearer x"}},
		{"Range", "GET", "/api/x", map[string]string{"Range": "bytes=0-1"}},
		{"outside CachePaths", "GET", "/other", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := mkGroup(t, "b.example.org", "", false, srv.URL)
			g.CacheTTL = 5 * time.Second
			g.CachePaths = []string{"/api"}
			r := &Router{}
			r.Set([]*RouteGroup{g})
			hits.Store(0)
			for i := 0; i < 2; i++ {
				rec, _ := doCached(r, tc.method, "http://b.example.org"+tc.path, tc.hdr)
				if got := rec.Header().Get("X-Cache"); got != "BYPASS" {
					t.Fatalf("request %d X-Cache = %q, want BYPASS", i+1, got)
				}
			}
			if hits.Load() != 2 {
				t.Fatalf("backend hits = %d, want 2", hits.Load())
			}
		})
	}
	t.Run("control inside CachePaths", func(t *testing.T) {
		g := mkGroup(t, "b.example.org", "", false, srv.URL)
		g.CacheTTL = 5 * time.Second
		g.CachePaths = []string{"/api"}
		r := &Router{}
		r.Set([]*RouteGroup{g})
		hits.Store(0)
		doCached(r, "GET", "http://b.example.org/api/x", nil)
		rec, _ := doCached(r, "GET", "http://b.example.org/api/x", nil)
		if got := rec.Header().Get("X-Cache"); got != "HIT" {
			t.Fatalf("X-Cache = %q, want HIT", got)
		}
		if hits.Load() != 1 {
			t.Fatalf("backend hits = %d, want 1", hits.Load())
		}
	})
}

func TestCacheHitMissLifecycle(t *testing.T) {
	var hits atomic.Int32
	const body = "hello cache"
	srv := countingBackend(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	r, g := newCacheRouter(t, "l.example.org", 5*time.Second, srv.URL)
	base := time.Now()
	now := base
	g.cache.now = func() time.Time { return now }

	rec, aw := doCached(r, "GET", "http://l.example.org/", nil)
	if rec.Header().Get("X-Cache") != "MISS" || hits.Load() != 1 {
		t.Fatalf("first: X-Cache=%q hits=%d, want MISS/1", rec.Header().Get("X-Cache"), hits.Load())
	}
	if aw.backend == "cache" {
		t.Fatal("MISS attributed to cache")
	}

	rec, aw = doCached(r, "GET", "http://l.example.org/", nil)
	if rec.Header().Get("X-Cache") != "HIT" || hits.Load() != 1 {
		t.Fatalf("second: X-Cache=%q hits=%d, want HIT/1", rec.Header().Get("X-Cache"), hits.Load())
	}
	if rec.Header().Get("Age") != "0" {
		t.Fatalf("Age = %q, want 0", rec.Header().Get("Age"))
	}
	if aw.backend != "cache" {
		t.Fatalf("access-log backend = %q, want cache", aw.backend)
	}
	if rec.Header().Get("Content-Length") != strconv.Itoa(len(body)) || rec.Body.String() != body {
		t.Fatalf("HIT Content-Length=%q body=%q", rec.Header().Get("Content-Length"), rec.Body.String())
	}

	now = base.Add(5 * time.Second)
	rec, _ = doCached(r, "GET", "http://l.example.org/", nil)
	if rec.Header().Get("X-Cache") != "MISS" || hits.Load() != 2 {
		t.Fatalf("after TTL: X-Cache=%q hits=%d, want MISS/2", rec.Header().Get("X-Cache"), hits.Load())
	}

	now = now.Add(3 * time.Second)
	rec, _ = doCached(r, "GET", "http://l.example.org/", nil)
	if rec.Header().Get("X-Cache") != "HIT" || rec.Header().Get("Age") != "3" {
		t.Fatalf("inside TTL: X-Cache=%q Age=%q, want HIT/3", rec.Header().Get("X-Cache"), rec.Header().Get("Age"))
	}
}

func TestCacheAcceptEncodingKeying(t *testing.T) {
	var hits atomic.Int32
	srv := countingBackend(t, &hits, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "Accept-Encoding")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write([]byte("gz"))
			return
		}
		_, _ = w.Write([]byte("plain"))
	})
	r, _ := newCacheRouter(t, "e.example.org", 5*time.Second, srv.URL)

	gz := map[string]string{"Accept-Encoding": "gzip"}
	id := map[string]string{"Accept-Encoding": "identity"}
	rec, _ := doCached(r, "GET", "http://e.example.org/", gz)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip request Content-Encoding = %q", rec.Header().Get("Content-Encoding"))
	}
	rec, _ = doCached(r, "GET", "http://e.example.org/", id)
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "plain" {
		t.Fatalf("identity request got Content-Encoding=%q body=%q", rec.Header().Get("Content-Encoding"), rec.Body.String())
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2 (one per encoding variant)", hits.Load())
	}
	for _, hdr := range []map[string]string{gz, id} {
		rec, _ = doCached(r, "GET", "http://e.example.org/", hdr)
		if rec.Header().Get("X-Cache") != "HIT" {
			t.Fatalf("repeat with %v: X-Cache = %q, want HIT", hdr, rec.Header().Get("X-Cache"))
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("hits after repeats = %d, want 2", hits.Load())
	}

	for in, want := range map[string]string{
		"gzip, deflate, br":  "br,deflate,gzip",
		"br;q=0, gzip;q=0.8": "gzip",
		"GZIP, gzip":         "gzip",
		"identity":           "",
	} {
		if got := normalizeAcceptEncoding(in); got != want {
			t.Errorf("normalizeAcceptEncoding(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCacheHeadBehavior(t *testing.T) {
	var hits atomic.Int32
	const body = "head body"
	srv := countingBackend(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	r, _ := newCacheRouter(t, "h.example.org", 5*time.Second, srv.URL)

	rec, _ := doCached(r, "HEAD", "http://h.example.org/", nil)
	if rec.Header().Get("X-Cache") != "BYPASS" || hits.Load() != 1 {
		t.Fatalf("HEAD first: X-Cache=%q hits=%d, want BYPASS/1", rec.Header().Get("X-Cache"), hits.Load())
	}
	rec, _ = doCached(r, "GET", "http://h.example.org/", nil)
	if rec.Header().Get("X-Cache") != "MISS" || hits.Load() != 2 {
		t.Fatalf("GET: X-Cache=%q hits=%d, want MISS/2", rec.Header().Get("X-Cache"), hits.Load())
	}
	rec, _ = doCached(r, "HEAD", "http://h.example.org/", nil)
	if rec.Header().Get("X-Cache") != "HIT" || hits.Load() != 2 {
		t.Fatalf("HEAD after GET: X-Cache=%q hits=%d, want HIT/2", rec.Header().Get("X-Cache"), hits.Load())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD HIT wrote %d body bytes", rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
		t.Fatalf("HEAD HIT Content-Length = %q, want %d", rec.Header().Get("Content-Length"), len(body))
	}
}

// waitForWaiters polls until n coalesced requests are parked on key's
// in-flight fill, so releasing the backend is deterministic rather than a
// sleep-and-hope.
func waitForWaiters(t *testing.T, c *responseCache, key string, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		f := c.inflight[key]
		c.mu.Unlock()
		if f != nil && f.waiters.Load() == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d parked waiters on %q", n, key)
}

func TestCacheCoalescesConcurrentMisses(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := countingBackend(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte("coalesced"))
	})
	r, g := newCacheRouter(t, "c.example.org", 5*time.Second, srv.URL)
	key := cacheKey("c.example.org", "/x", "", "")

	const n = 20
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i], _ = doCached(r, "GET", "http://c.example.org/x", nil)
		}(i)
	}
	waitForWaiters(t, g.cache, key, n-1)
	close(release)
	wg.Wait()

	if hits.Load() != 1 {
		t.Fatalf("backend hits = %d, want 1", hits.Load())
	}
	var miss, hit int
	for i, rec := range recs {
		if rec.Code != 200 || rec.Body.String() != "coalesced" {
			t.Fatalf("request %d: code=%d body=%q", i, rec.Code, rec.Body.String())
		}
		switch rec.Header().Get("X-Cache") {
		case "MISS":
			miss++
		case "HIT":
			hit++
		default:
			t.Fatalf("request %d: X-Cache = %q", i, rec.Header().Get("X-Cache"))
		}
	}
	if miss != 1 || hit != n-1 {
		t.Fatalf("miss=%d hit=%d, want 1/%d", miss, hit, n-1)
	}
}

// TestCacheCoalescingFallsThroughOnUncacheable proves waiters woken to an
// empty cache each go to the backend themselves. One backend per request
// (pickTier's atomic cursor hands the n picks out round-robin, so each of
// the n concurrent requests lands on its own *Backend) because tryProxy's
// per-call ErrorHandler assignment is a pre-existing data race under -race
// when several goroutines drive the same *Backend at once.
func TestCacheCoalescingFallsThroughOnUncacheable(t *testing.T) {
	const n = 20
	var hits atomic.Int32
	release := make(chan struct{})
	var first sync.Once
	handler := func(w http.ResponseWriter, _ *http.Request) {
		first.Do(func() { <-release })
		w.Header().Set("Set-Cookie", "s=1")
		_, _ = w.Write([]byte("personal"))
	}
	backends := make([]*Backend, n)
	for i := range backends {
		backends[i] = mkBackend(t, "f.example.org", countingBackend(t, &hits, handler))
	}
	g := mkGroupMulti("f.example.org", "", backends...)
	g.CacheTTL = 5 * time.Second
	r := &Router{}
	r.Set([]*RouteGroup{g})
	key := cacheKey("f.example.org", "/x", "", "")

	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i], _ = doCached(r, "GET", "http://f.example.org/x", nil)
		}(i)
	}
	waitForWaiters(t, g.cache, key, n-1)
	close(release)
	wg.Wait()

	if hits.Load() != n {
		t.Fatalf("backend hits = %d, want %d (nothing storable, so every waiter falls through)", hits.Load(), n)
	}
	for i, rec := range recs {
		if rec.Code != 200 {
			t.Fatalf("request %d: code = %d", i, rec.Code)
		}
	}
}

func TestResponseCacheEviction(t *testing.T) {
	base := time.Unix(1_000_000, 0)

	c := newResponseCache(time.Minute)
	c.now = func() time.Time { return base }
	for i := 0; i <= cacheMaxEntries; i++ {
		c.put("k"+strconv.Itoa(i), &cacheEntry{status: 200, body: []byte("x"), storedAt: base.Add(time.Duration(i) * time.Millisecond)})
	}
	if len(c.entries) != cacheMaxEntries {
		t.Fatalf("entries = %d, want %d", len(c.entries), cacheMaxEntries)
	}
	if _, ok := c.entries["k0"]; ok {
		t.Fatal("oldest entry k0 survived eviction")
	}

	c = newResponseCache(time.Minute)
	c.now = func() time.Time { return base }
	body := make([]byte, cacheMaxBody)
	for i := 0; i <= cacheMaxBytes/cacheMaxBody; i++ {
		c.put("b"+strconv.Itoa(i), &cacheEntry{status: 200, body: body, storedAt: base.Add(time.Duration(i) * time.Millisecond)})
	}
	if c.bytes > cacheMaxBytes {
		t.Fatalf("bytes = %d, want <= %d", c.bytes, cacheMaxBytes)
	}
	if _, ok := c.entries["b0"]; ok {
		t.Fatal("earliest entry b0 survived byte-bound eviction")
	}

	// Two expired entries plus one over the count bound: expired-first
	// drops both, whereas oldest-first alone would only drop one.
	c = newResponseCache(time.Minute)
	c.now = func() time.Time { return base }
	c.put("exp0", &cacheEntry{status: 200, body: []byte("x"), storedAt: base.Add(-2 * time.Minute)})
	c.put("exp1", &cacheEntry{status: 200, body: []byte("x"), storedAt: base.Add(-2 * time.Minute)})
	for i := 0; i < cacheMaxEntries-1; i++ {
		c.put("live"+strconv.Itoa(i), &cacheEntry{status: 200, body: []byte("x"), storedAt: base})
	}
	if _, ok := c.entries["exp0"]; ok {
		t.Fatal("expired exp0 survived")
	}
	if _, ok := c.entries["exp1"]; ok {
		t.Fatal("expired exp1 survived")
	}
	if len(c.entries) != cacheMaxEntries-1 {
		t.Fatalf("entries = %d, want %d (all live entries kept)", len(c.entries), cacheMaxEntries-1)
	}
}

func TestAssembleGroupsCacheLabelMerge(t *testing.T) {
	dc := fakeDocker(t, dockerJSON(
		container("r1", "app-cache-r1", "running",
			map[string]string{labelHost: "cm.example.org", labelPort: "8080", labelHealth: "/h", labelCache: "5s", labelCachePaths: "/a"},
			map[string]string{managedNetwork: "172.20.0.21"}),
		container("r2", "app-cache-r2", "running",
			map[string]string{labelHost: "cm.example.org", labelPort: "8080", labelHealth: "/h", labelCache: "10s", labelCachePaths: "/b, /a"},
			map[string]string{managedNetwork: "172.20.0.22"}),
		container("z", "app-cache-zero", "running",
			map[string]string{labelHost: "zero.example.org", labelPort: "8080", labelHealth: "/h", labelCache: "0"},
			map[string]string{managedNetwork: "172.20.0.23"}),
		container("f", "app-cache-false", "running",
			map[string]string{labelHost: "false.example.org", labelPort: "8080", labelHealth: "/h", labelCache: "false"},
			map[string]string{managedNetwork: "172.20.0.24"}),
		container("e", "app-cache-empty", "running",
			map[string]string{labelHost: "empty.example.org", labelPort: "8080", labelHealth: "/h", labelCache: ""},
			map[string]string{managedNetwork: "172.20.0.25"}),
		container("g", "app-cache-garbage", "running",
			map[string]string{labelHost: "garbage.example.org", labelPort: "8080", labelHealth: "/h", labelCache: "garbage"},
			map[string]string{managedNetwork: "172.20.0.26"}),
	))
	cacheLabelWarnMu.Lock()
	delete(cacheLabelWarnSeen, "app-cache-garbage")
	cacheLabelWarnMu.Unlock()

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	groups, _, err := assembleGroups(context.Background(), dc, "")
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	if _, _, err := assembleGroups(context.Background(), dc, ""); err != nil {
		t.Fatalf("assembleGroups (2nd): %v", err)
	}

	g := findGroup(groups, "cm.example.org", "")
	if g == nil || g.CacheTTL != 10*time.Second {
		t.Fatalf("merged CacheTTL = %+v, want 10s", g)
	}
	if len(g.CachePaths) != 2 || g.CachePaths[0] != "/a" || g.CachePaths[1] != "/b" {
		t.Fatalf("merged CachePaths = %v, want [/a /b]", g.CachePaths)
	}
	for _, host := range []string{"zero.example.org", "false.example.org", "empty.example.org", "garbage.example.org"} {
		if g := findGroup(groups, host, ""); g == nil || g.CacheTTL != 0 {
			t.Fatalf("%s CacheTTL = %+v, want 0", host, g)
		}
	}
	if got := strings.Count(buf.String(), "bad "+labelCache); got != 1 {
		t.Fatalf("bad-label warning logged %d time(s) across two assembleGroups() calls, want exactly 1", got)
	}
}

func TestAssembleGroupsCacheStatic(t *testing.T) {
	cfgPath := writeStaticConfig(t,
		staticRoute{Host: "cs.example.com", Backends: []string{"http://10.0.0.9:8080"}, Cache: "5s", CachePaths: []string{"/api"}},
		staticRoute{Host: "bad.example.com", Backends: []string{"http://10.0.0.9:8080"}, Cache: "nope"},
	)
	groups, _, err := assembleGroups(context.Background(), fakeDocker(t, "[]"), cfgPath)
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	g := findGroup(groups, "cs.example.com", "")
	if g == nil || g.CacheTTL != 5*time.Second || len(g.CachePaths) != 1 || g.CachePaths[0] != "/api" {
		t.Fatalf("static cache group = %+v, want 5s [/api]", g)
	}
	if g := findGroup(groups, "bad.example.com", ""); g == nil || g.CacheTTL != 0 {
		t.Fatalf("bad static cache group = %+v, want CacheTTL 0", g)
	}
}

func TestCachePersistsAcrossRouterSet(t *testing.T) {
	var hits atomic.Int32
	srv := countingBackend(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("persist"))
	})
	r, g1 := newCacheRouter(t, "p.example.org", 5*time.Second, srv.URL)
	doCached(r, "GET", "http://p.example.org/", nil)

	g2 := mkGroup(t, "p.example.org", "", false, srv.URL)
	g2.CacheTTL = 5 * time.Second
	r.Set([]*RouteGroup{g2})
	if g2.cache != g1.cache {
		t.Fatal("rebuild got a fresh cache instead of the persisted one")
	}
	rec, _ := doCached(r, "GET", "http://p.example.org/", nil)
	if rec.Header().Get("X-Cache") != "HIT" || hits.Load() != 1 {
		t.Fatalf("after rebuild: X-Cache=%q hits=%d, want HIT/1", rec.Header().Get("X-Cache"), hits.Load())
	}

	g3 := mkGroup(t, "p.example.org", "", false, srv.URL)
	g3.CacheTTL = 20 * time.Second
	r.Set([]*RouteGroup{g3})
	if g3.cache != g1.cache {
		t.Fatal("TTL change replaced the cache instead of flushing it")
	}
	rec, _ = doCached(r, "GET", "http://p.example.org/", nil)
	if rec.Header().Get("X-Cache") != "MISS" || hits.Load() != 2 {
		t.Fatalf("after TTL change: X-Cache=%q hits=%d, want MISS/2", rec.Header().Get("X-Cache"), hits.Load())
	}

	g4 := mkGroup(t, "p.example.org", "", false, srv.URL)
	r.Set([]*RouteGroup{g4})
	if g4.cache != nil || len(r.caches) != 0 {
		t.Fatalf("cache off: g.cache=%v len(r.caches)=%d, want nil/0", g4.cache, len(r.caches))
	}
	rec, _ = doCached(r, "GET", "http://p.example.org/", nil)
	if _, ok := rec.Header()["X-Cache"]; ok {
		t.Fatalf("uncached route emitted X-Cache: %q", rec.Header().Get("X-Cache"))
	}
}

// TestCacheWebSocketUpgradeOnCachedRoute is TestServeHTTPWebSocketUpgrade
// with caching on, proving the recorder forwards Hijack.
func TestCacheWebSocketUpgradeOnCachedRoute(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Connection"), "Upgrade") {
			http.Error(w, "expected upgrade", http.StatusBadRequest)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("backend hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
		line, _ := buf.ReadString('\n')
		_, _ = buf.WriteString("echo:" + line)
		_ = buf.Flush()
	}))
	defer backend.Close()

	r := &Router{}
	group := mkGroup(t, "ws.example.org", "", false, backend.URL)
	group.CacheTTL = 5 * time.Second
	r.Set([]*RouteGroup{group})

	handler := withMetrics(withAccessLog(r, NewAccessLog()), NewMetrics())
	front := httptest.NewServer(handler)
	defer front.Close()

	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: ws.example.org\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status line = %q, want 101 Switching Protocols", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	fmt.Fprintf(conn, "ping\n")
	echo, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if strings.TrimSpace(echo) != "echo:ping" {
		t.Fatalf("echo = %q, want echo:ping", strings.TrimSpace(echo))
	}
	if !group.Backends[0].healthy() {
		t.Fatal("backend was marked unhealthy — upgrade tripped the ErrorHandler")
	}
}

// A backend that dies partway through a body must never populate the cache:
// once via the declared Content-Length not matching what was recorded (the
// non-panicking copy-error path), and once via ReverseProxy's
// ErrAbortHandler panic, which still runs serveWithCache's defers.
func TestCacheNeverStoresTruncatedBody(t *testing.T) {
	t.Run("short write vs declared Content-Length", func(t *testing.T) {
		var hits atomic.Int32
		srv := countingBackend(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("short"))
		})
		r, _ := newCacheRouter(t, "trunc.example.org", 5*time.Second, srv.URL)
		doCached(r, "GET", "http://trunc.example.org/", nil)
		rec, _ := doCached(r, "GET", "http://trunc.example.org/", nil)
		if hits.Load() != 2 || rec.Header().Get("X-Cache") == "HIT" {
			t.Fatalf("truncated body was served from cache: hits=%d X-Cache=%q", hits.Load(), rec.Header().Get("X-Cache"))
		}
	})
	t.Run("backend aborts mid-stream", func(t *testing.T) {
		var hits atomic.Int32
		srv := countingBackend(t, &hits, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("partial"))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		})
		r, _ := newCacheRouter(t, "abort.example.org", 5*time.Second, srv.URL)
		// ServerContextKey makes ReverseProxy take its production path and
		// panic on the copy error instead of suppressing it for tests.
		req := httptest.NewRequest("GET", "http://abort.example.org/", nil)
		req = req.WithContext(context.WithValue(req.Context(), http.ServerContextKey, &http.Server{}))
		func() {
			defer func() {
				if got := recover(); got != http.ErrAbortHandler {
					t.Fatalf("recovered %v, want http.ErrAbortHandler", got)
				}
			}()
			r.ServeHTTP(&accessWriter{ResponseWriter: httptest.NewRecorder()}, req)
		}()
		rec, _ := doCached(r, "GET", "http://abort.example.org/", nil)
		if hits.Load() != 2 || rec.Header().Get("X-Cache") == "HIT" {
			t.Fatalf("aborted body was served from cache: hits=%d X-Cache=%q", hits.Load(), rec.Header().Get("X-Cache"))
		}
	})
}
