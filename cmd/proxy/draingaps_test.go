package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func serve(r *Router, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func assertTransient503(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
	if !strings.Contains(rec.Body.String(), `<meta http-equiv=refresh content="5">`) {
		t.Fatalf("body missing 5s meta-refresh: %s", rec.Body.String())
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestTombstoneRemovedRouteServes503(t *testing.T) {
	host := "gone.example.org"
	r := &Router{}
	r.Set([]*RouteGroup{mkGroup(t, host, "", false, okServer(t, "ok").URL)})
	r.Set(nil)

	assertTransient503(t, serve(r, "http://"+host+"/x"))

	rec := serve(r, "http://other.example.org/")
	if rec.Code != http.StatusNotFound || rec.Header().Get("Retry-After") != "300" {
		t.Fatalf("unrelated host = %d Retry-After %q, want 404 / 300", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestTombstoneDoesNotMarkUnrouted(t *testing.T) {
	host := "gone.example.org"
	r := &Router{}
	r.Set([]*RouteGroup{mkGroup(t, host, "", false, okServer(t, "ok").URL)})
	r.Set(nil)
	m := &markRecorder{ResponseRecorder: httptest.NewRecorder()}
	r.ServeHTTP(m, httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil))
	if m.marked {
		t.Fatal("tombstoned request was marked unrouted")
	}
}

type markRecorder struct {
	*httptest.ResponseRecorder
	marked bool
}

func (m *markRecorder) MarkUnrouted() { m.marked = true }

func TestTombstoneExpiresTo404(t *testing.T) {
	host := "gone.example.org"
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	r := &Router{now: clk.now}
	r.Set([]*RouteGroup{mkGroup(t, host, "", false, okServer(t, "ok").URL)})
	r.Set(nil)

	clk.advance(routeTombstoneTTL - time.Second)
	assertTransient503(t, serve(r, "http://"+host+"/"))

	clk.advance(2 * time.Second)
	if rec := serve(r, "http://"+host+"/"); rec.Code != http.StatusNotFound {
		t.Fatalf("after TTL code = %d, want 404", rec.Code)
	}

	// The next Set prunes it.
	r.Set(nil)
	if n := len(r.tombstones); n != 0 {
		t.Fatalf("%d tombstones left after an expired-pruning Set, want 0", n)
	}
}

func TestTombstoneClearedWhenRouteReturns(t *testing.T) {
	host := "back.example.org"
	srv := okServer(t, "ok")
	r := &Router{}
	r.Set([]*RouteGroup{mkGroup(t, host, "", false, srv.URL)})
	r.Set(nil)
	assertTransient503(t, serve(r, "http://"+host+"/"))

	r.Set([]*RouteGroup{mkGroup(t, host, "", false, srv.URL)})
	if rec := serve(r, "http://"+host+"/"); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("returned route = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
	if n := len(r.tombstones); n != 0 {
		t.Fatalf("%d tombstones after the route returned, want 0", n)
	}
}

func TestTombstonePathRoutes(t *testing.T) {
	host := "paths.example.org"
	root := okServer(t, "root")
	api := okServer(t, "api")
	r := &Router{}
	r.Set([]*RouteGroup{mkGroup(t, host, "", false, root.URL), mkGroup(t, host, "/api", false, api.URL)})

	// Removed /api is more specific than the live root route: 503 under /api.
	r.Set([]*RouteGroup{mkGroup(t, host, "", false, root.URL)})
	assertTransient503(t, serve(r, "http://"+host+"/api/x"))
	if rec := serve(r, "http://"+host+"/other"); rec.Body.String() != "root" {
		t.Fatalf("/other = %d %q, want root", rec.Code, rec.Body.String())
	}

	// Removed root is less specific than the live /api route: /api still serves.
	r2 := &Router{}
	r2.Set([]*RouteGroup{mkGroup(t, host, "", false, root.URL), mkGroup(t, host, "/api", false, api.URL)})
	r2.Set([]*RouteGroup{mkGroup(t, host, "/api", false, api.URL)})
	if rec := serve(r2, "http://"+host+"/api/x"); rec.Body.String() != "api" {
		t.Fatalf("/api/x = %d %q, want api", rec.Code, rec.Body.String())
	}
	assertTransient503(t, serve(r2, "http://"+host+"/"))
}

func TestTombstoneHostCaseInsensitive(t *testing.T) {
	r := &Router{}
	r.Set([]*RouteGroup{mkGroup(t, "Mixed.Example.org", "", false, okServer(t, "ok").URL)})
	r.Set(nil)
	assertTransient503(t, serve(r, "http://mixed.EXAMPLE.org/"))
	if r.tombstones[0].host != "mixed.example.org" {
		t.Fatalf("tombstone host = %q, want lowercased", r.tombstones[0].host)
	}

	// Returning under a different case still clears it.
	r.Set([]*RouteGroup{mkGroup(t, "MIXED.example.org", "", false, okServer(t, "ok").URL)})
	if n := len(r.tombstones); n != 0 {
		t.Fatalf("%d tombstones after a case-differing return, want 0", n)
	}
}

// A peer-learned route the origin stops advertising (draining) expires out of
// the overlay; the entrance must answer 503, not 404.
func TestTombstonePeerLearnedExpiry(t *testing.T) {
	host := "peer.example.org"
	start := time.Unix(1_700_000_000, 0)
	storeClk := &fakeClock{t: start}
	s := newPeerRouteStore(15 * time.Second)
	s.now = storeClk.now
	s.merge(peerRoutePayload{Peer: "b", Advertise: "http://127.0.0.1:1", Routes: []peerRouteInfo{{Host: host, Backends: 1}}})

	r := &Router{}
	r.Set(s.overlay(nil, nil))
	if findGroup(r.Snapshot(), host, "") == nil {
		t.Fatal("learned group missing")
	}

	storeClk.advance(time.Minute)
	r.Set(s.overlay(nil, nil))
	if findGroup(r.Snapshot(), host, "") != nil {
		t.Fatal("learned group should have expired")
	}
	assertTransient503(t, serve(r, "http://"+host+"/"))
}

func TestNoHealthyBackendsRetryAfter5(t *testing.T) {
	host := "dead.example.org"
	r := &Router{}
	r.Set([]*RouteGroup{mkGroupMulti(host, "", deadBackend(t, host))})
	assertTransient503(t, serve(r, "http://"+host+"/"))
}

func TestABNoHealthyBackendsRetryAfter5(t *testing.T) {
	f := newABFixture(t, abLabels(), nil)
	f.a.be.draining.Store(true)
	f.b.be.draining.Store(true)
	assertTransient503(t, f.do("GET", "/", nil, nil))
}

func TestSetTransportRegistry(t *testing.T) {
	host := "t.example.org"
	s1 := okServer(t, "1")
	s2 := okServer(t, "2")
	b1 := mkBackend(t, host, s1)
	b2 := mkBackend(t, host, s2)
	b1other := mkBackend(t, "o.example.org", s1)
	injected := &http.Transport{}
	bInj := mkBackend(t, "i.example.org", okServer(t, "i"))
	bInj.proxy.Transport = injected
	bare := &Backend{URL: "http://127.0.0.1:9"}

	r := &Router{}
	shared := mkGroupMulti("s.example.org", "", b1)
	r.Set([]*RouteGroup{
		mkGroupMulti(host, "", b1, b2),
		mkGroupMulti("o.example.org", "", b1other),
		shared,
		mkGroupMulti("i.example.org", "", bInj),
		mkGroupMulti("bare.example.org", "", bare),
	})

	if b1.transport == nil || b1.proxy.Transport != b1.transport {
		t.Fatal("b1 not wired to a registry transport")
	}
	if b1.transport == b2.transport {
		t.Fatal("different URLs share a transport")
	}
	if b1other.transport != b1.transport {
		t.Fatal("same URL in another group got a different transport")
	}
	if bInj.proxy.Transport != injected || bInj.transport != nil {
		t.Fatal("test-injected transport was replaced")
	}
	if bare.transport != nil {
		t.Fatal("proxy-less backend got a transport")
	}
	t1 := b1.transport

	// Fresh Backend objects for the same URL reuse it; a repeat Set of the
	// same objects leaves them alone.
	nb1 := mkBackend(t, host, s1)
	r.Set([]*RouteGroup{mkGroupMulti(host, "", nb1), shared})
	if nb1.transport != t1 || b1.transport != t1 {
		t.Fatal("same URL lost its transport across Sets")
	}
	if _, ok := r.transports[s2.URL]; ok {
		t.Fatal("removed URL's transport still registered")
	}
	if len(r.transports) != 1 {
		t.Fatalf("registry has %d transports, want 1", len(r.transports))
	}
}

// connStateServer counts each connection state transition on a channel.
func connStateServer(t *testing.T, h http.Handler) (*httptest.Server, chan http.ConnState) {
	t.Helper()
	ch := make(chan http.ConnState, 64)
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		select {
		case ch <- s:
		default:
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, ch
}

func waitConnState(t *testing.T, ch chan http.ConnState, want http.ConnState, msg string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case s := <-ch:
			if s == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %v: %s", want, msg)
		}
	}
}

func TestMarkDrainingClosesIdleConns(t *testing.T) {
	host := "idle.example.org"
	srv, states := connStateServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	b := drainBackend(t, host, "cid", srv)
	r := &Router{}
	r.Set([]*RouteGroup{mkGroupMulti(host, "", b)})

	if rec := serve(r, "http://"+host+"/"); rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	waitConnState(t, states, http.StateIdle, "conn never went idle")
	r.markDraining("cid")
	waitConnState(t, states, http.StateClosed, "idle keep-alive conn to the draining backend was not closed")
}

// Drained while a request is on the wire: the conn finishes that request and
// is closed after, not returned to the pool. Flips draining directly so only
// tryProxy's close can be responsible.
func TestTryProxyClosesConnAfterInFlightDrain(t *testing.T) {
	host := "inflight.example.org"
	entered := make(chan struct{})
	release := make(chan struct{})
	srv, states := connStateServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte("done"))
	}))
	b := drainBackend(t, host, "cid", srv)
	r := &Router{}
	r.Set([]*RouteGroup{mkGroupMulti(host, "", b)})

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- serve(r, "http://"+host+"/") }()
	<-entered
	b.draining.Store(true)
	close(release)
	rec := <-done
	if rec.Code != 200 || rec.Body.String() != "done" {
		t.Fatalf("in-flight request = %d %q, want 200 done", rec.Code, rec.Body.String())
	}
	waitConnState(t, states, http.StateClosed, "conn used by a request to a draining backend went back to the pool")
}

func TestDispatchHealthChecksSkipsDraining(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	drained := &Backend{URL: srv.URL, HealthPath: "/", Weight: 1}
	drained.draining.Store(true)
	live := &Backend{URL: srv.URL, HealthPath: "/", Weight: 1}

	sem := make(chan struct{}, 1)
	var wg sync.WaitGroup
	dispatchHealthChecks(context.Background(), []*RouteGroup{{Host: "h.example.org", Backends: []*Backend{drained, drained, live}}}, sem, &wg)
	wg.Wait()
	if n := hits.Load(); n != 1 {
		t.Fatalf("probes = %d, want 1 (draining backends skipped)", n)
	}
}

func TestHealthCheckLeavesNoIdleConn(t *testing.T) {
	srv, states := connStateServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	b := &Backend{URL: srv.URL, HealthPath: "/", Weight: 1}
	checkBackend(b)
	if !b.healthy() {
		t.Fatal("probe failed")
	}
	waitConnState(t, states, http.StateClosed, "health probe left its conn open")
}

func TestDrainTransportRace(t *testing.T) {
	host := "race.example.org"
	srv := okServer(t, "ok")
	u, _ := url.Parse(srv.URL)
	build := func() []*RouteGroup {
		b := makeBackend(srv.URL, 1, "c", "", u, host)
		b.ContainerID = "cid"
		return []*RouteGroup{mkGroupMulti(host, "", b)}
	}
	r := &Router{}
	r.Set(build())

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := serve(r, "http://"+host+"/")
				_, _ = io.Copy(io.Discard, rec.Body)
			}
		}()
	}
	for i := 0; i < 50; i++ {
		r.markDraining("cid")
		r.Set(build())
		r.clearDraining("cid")
		r.Set(build())
		if i%10 == 0 {
			r.Set(nil)
			r.Set(build())
		}
	}
	close(stop)
	wg.Wait()
}
