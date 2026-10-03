package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// drainBackend is mkBackend with a container ID, as assembleGroups sets it.
func drainBackend(t *testing.T, host, id string, srv *httptest.Server) *Backend {
	t.Helper()
	b := mkBackend(t, host, srv)
	b.ContainerID = id
	return b
}

func okServer(t *testing.T, body string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	return srv
}

func deadBackend(t *testing.T, host string) *Backend {
	t.Helper()
	u, _ := url.Parse("http://127.0.0.1:1")
	return makeBackend("http://127.0.0.1:1", 1, "dead", "", u, host)
}

func TestAssembleGroupsSetsContainerID(t *testing.T) {
	dc := fakeDocker(t, dockerJSON(container("cid-1", "app", "running",
		map[string]string{labelHost: "a.example.org", labelPort: "80"},
		map[string]string{managedNetwork: "172.20.0.5"})))
	groups, _, err := assembleGroups(context.Background(), dc, "")
	if err != nil {
		t.Fatal(err)
	}
	if g := findGroup(groups, "a.example.org", ""); g == nil || g.Backends[0].ContainerID != "cid-1" {
		t.Fatalf("ContainerID not set: %+v", g)
	}
}

func TestMarkDrainingSkipsInPickers(t *testing.T) {
	host := "d.example.org"
	a := drainBackend(t, host, "a", okServer(t, "A"))
	b := drainBackend(t, host, "b", okServer(t, "B"))
	g := mkGroupMulti(host, "", a, b)
	g.Sticky = true
	r := &Router{}
	r.Set([]*RouteGroup{g})
	r.markDraining("a")

	for i := 0; i < 6; i++ {
		if got := g.pickHealthy(nil, true, ""); got != b {
			t.Fatalf("pickHealthy picked %v, want b", got.ContainerID)
		}
	}
	// Panic mode distrusts probe health but must still skip a draining one.
	a.markHealthy(false)
	b.markHealthy(false)
	for i := 0; i < 4; i++ {
		if got := g.pickAny(nil, true, ""); got != b {
			t.Fatalf("pickAny picked %v, want b", got.ContainerID)
		}
	}
	b.markHealthy(true)

	// A sticky pin to the draining backend falls through to the other one.
	req := httptest.NewRequest("GET", "http://"+host+"/", nil)
	req.AddCookie(&http.Cookie{Name: stickyCookieName(g), Value: a.stickyID})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Body.String() != "B" {
		t.Fatalf("sticky pin to draining backend served %q, want B", rec.Body.String())
	}

	r.markDraining("b")
	if got := g.pickAny(nil, true, ""); got != nil {
		t.Fatalf("pickAny with every backend draining = %v, want nil", got.URL)
	}
}

func TestDrainingReappliedBySetAndClearedOnStart(t *testing.T) {
	host := "d.example.org"
	srv := okServer(t, "A")
	build := func() []*RouteGroup {
		return []*RouteGroup{mkGroupMulti(host, "", drainBackend(t, host, "a", srv))}
	}
	r := &Router{}
	refresh := func() { r.Set(build()) }
	refresh()

	handleContainerEvent(r, refresh, killEvent("a", "15"))
	if b := r.Snapshot()[0].Backends[0]; !b.draining.Load() {
		t.Fatal("kill 15 should leave the rebuilt backend draining")
	}
	refresh()
	if b := r.Snapshot()[0].Backends[0]; !b.draining.Load() {
		t.Fatal("Set must re-apply draining to a freshly built backend")
	}
	ev := dockerEvent{Action: "start"}
	ev.Actor.ID = "a"
	handleContainerEvent(r, refresh, ev)
	if b := r.Snapshot()[0].Backends[0]; b.draining.Load() {
		t.Fatal("start should clear draining")
	}
}

func killEvent(id, signal string) dockerEvent {
	ev := dockerEvent{Type: "container", Action: "kill"}
	ev.Actor.ID = id
	ev.Actor.Attributes = map[string]string{"signal": signal}
	return ev
}

func TestKillEventSignals(t *testing.T) {
	host := "d.example.org"
	srv := okServer(t, "A")
	for _, c := range []struct {
		signal string
		drain  bool
	}{
		{"15", true}, {"9", true}, {"2", true}, {"3", true}, {"SIGTERM", true}, {"sigkill", true},
		{"1", false}, {"10", false}, {"SIGHUP", false}, {"SIGUSR1", false}, {"28", false}, {"", false},
	} {
		r := &Router{}
		refresh := func() { r.Set([]*RouteGroup{mkGroupMulti(host, "", drainBackend(t, host, "a", srv))}) }
		refresh()
		handleContainerEvent(r, refresh, killEvent("a", c.signal))
		if got := r.Snapshot()[0].Backends[0].draining.Load(); got != c.drain {
			t.Errorf("signal %q: draining = %v, want %v", c.signal, got, c.drain)
		}
	}

	r := &Router{}
	refresh := func() { r.Set([]*RouteGroup{mkGroupMulti(host, "", drainBackend(t, host, "a", srv))}) }
	refresh()
	handleContainerEvent(r, refresh, killEvent("a", "15"))
	ev := dockerEvent{Action: "die"}
	ev.Actor.ID = "a"
	handleContainerEvent(r, refresh, ev)
	if r.Snapshot()[0].Backends[0].draining.Load() {
		t.Fatal("die should clear draining")
	}
}

func TestDrainingTTL(t *testing.T) {
	host := "d.example.org"
	srv := okServer(t, "A")
	now := time.Unix(1_000_000, 0)
	r := &Router{now: func() time.Time { return now }}
	refresh := func() { r.Set([]*RouteGroup{mkGroupMulti(host, "", drainBackend(t, host, "a", srv))}) }
	refresh()
	r.markDraining("a")
	now = now.Add(drainTTL - time.Second)
	refresh()
	if !r.Snapshot()[0].Backends[0].draining.Load() {
		t.Fatal("still inside the TTL — should be draining")
	}
	now = now.Add(2 * time.Second)
	refresh()
	if r.Snapshot()[0].Backends[0].draining.Load() {
		t.Fatal("past the TTL — should rejoin")
	}
	if len(r.drainSet) != 0 {
		t.Fatalf("expired entry not pruned: %v", r.drainSet)
	}
}

func TestPeerPayloadExcludesDraining(t *testing.T) {
	host := "d.example.org"
	a := drainBackend(t, host, "a", okServer(t, "A"))
	b := drainBackend(t, host, "b", okServer(t, "B"))
	r := &Router{}
	r.Set([]*RouteGroup{mkGroupMulti(host, "", a, b)})
	r.markDraining("a")
	body, ok := buildPeerPayload(r, "me", "http://me:8092")
	if !ok || !strings.Contains(string(body), `"backends":1`) {
		t.Fatalf("payload = %s, want one advertised backend", body)
	}
	r.markDraining("b")
	if _, ok := buildPeerPayload(r, "me", "http://me:8092"); ok {
		t.Fatal("a group whose local backends all drain should not be advertised")
	}
}

func TestInFlightRequestSurvivesDrainMidRequest(t *testing.T) {
	host := "d.example.org"
	entered := make(chan struct{})
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte("slow"))
	}))
	defer slow.Close()
	r := &Router{}
	refresh := func() { r.Set([]*RouteGroup{mkGroupMulti(host, "", drainBackend(t, host, "a", slow))}) }
	refresh()

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		r.ServeHTTP(rec, httptest.NewRequest("GET", "http://"+host+"/", nil))
		close(done)
	}()
	<-entered
	handleContainerEvent(r, refresh, killEvent("a", "15"))
	close(release)
	<-done
	if rec.Code != 200 || rec.Body.String() != "slow" {
		t.Fatalf("in-flight request = %d %q, want 200 slow", rec.Code, rec.Body.String())
	}
}

func TestGetToRefusedBackendRetries(t *testing.T) {
	host := "d.example.org"
	g := mkGroupMulti(host, "", deadBackend(t, host), mkBackend(t, host, okServer(t, "B")))
	r := &Router{}
	r.Set([]*RouteGroup{g})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "http://"+host+"/", nil))
	if rec.Code != 200 || rec.Body.String() != "B" {
		t.Fatalf("got %d %q, want 200 B", rec.Code, rec.Body.String())
	}
}

// serverBody mimics a server request body: once closed (the transport
// closes it after a failed attempt), reads fail.
type serverBody struct {
	r      io.Reader
	closed bool
}

func (b *serverBody) Read(p []byte) (int, error) {
	if b.closed {
		return 0, http.ErrBodyReadAfterClose
	}
	return b.r.Read(p)
}

func (b *serverBody) Close() error { b.closed = true; return nil }

// echoServer returns the request body it received; hits counts requests.
func echoServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBodyRetry(t *testing.T) {
	host := "d.example.org"
	small := strings.Repeat("s", 1000)
	exact := strings.Repeat("e", maxReplayBody)
	large := strings.Repeat("L", maxReplayBody+1)

	cases := []struct {
		name          string
		body          string
		unknownLength bool
		wantCode      int
	}{
		{"small", small, false, 200},
		{"small unknown length", small, true, 200},
		{"exactly the cap", exact, false, 200},
		{"exactly the cap, unknown length", exact, true, 200},
		{"large", large, false, http.StatusBadGateway},
		{"large unknown length", large, true, http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits atomic.Int32
			good := mkBackend(t, host, echoServer(t, &hits))
			g := mkGroupMulti(host, "", deadBackend(t, host), good)
			r := &Router{}
			r.Set([]*RouteGroup{g})
			req := httptest.NewRequest("POST", "http://"+host+"/", nil)
			req.Body = &serverBody{r: strings.NewReader(c.body)}
			req.ContentLength = int64(len(c.body))
			if c.unknownLength {
				req.ContentLength = -1
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != c.wantCode {
				t.Fatalf("code = %d, want %d", rec.Code, c.wantCode)
			}
			if c.wantCode == 200 {
				if rec.Body.String() != c.body {
					t.Fatalf("body arrived with %d bytes, want %d intact", rec.Body.Len(), len(c.body))
				}
				return
			}
			if hits.Load() != 0 {
				t.Fatalf("a non-replayable body was retried (%d hits on the second backend)", hits.Load())
			}
			if !good.healthy() {
				t.Fatal("the second backend must not be marked unhealthy")
			}
		})
	}
}

// A large body straight to a healthy backend still streams through intact,
// including the bytes prepareRetryBody already read for an unknown length.
func TestLargeBodyStreamsIntact(t *testing.T) {
	host := "d.example.org"
	var hits atomic.Int32
	r := &Router{}
	r.Set([]*RouteGroup{mkGroupMulti(host, "", mkBackend(t, host, echoServer(t, &hits)))})
	body := bytes.Repeat([]byte("0123456789"), 20_000)
	req := httptest.NewRequest("POST", "http://"+host+"/", bytes.NewReader(body))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("code=%d len=%d, want 200 and %d intact bytes", rec.Code, rec.Body.Len(), len(body))
	}
}
