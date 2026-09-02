package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cacheStub is a dockerStub that counts /containers/json hits and serves a
// fixed list for them; every other path 404s so a stray call shows up as an
// error rather than a silently-decoded empty array.
func cacheStub(t *testing.T, items []dockerContainer, hits *atomic.Int32, sawFilters *atomic.Bool) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/containers/json") {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		if sawFilters != nil && r.URL.Query().Get("filters") != "" {
			sawFilters.Store(true)
		}
		json.NewEncoder(w).Encode(items)
	}))
}

func TestContainerCacheHitInvalidateAndExpiry(t *testing.T) {
	var hits atomic.Int32
	dc := cacheStub(t, []dockerContainer{{ID: "c1", Names: []string{"/a"}, State: "running"}}, &hits, nil)
	dc.cache = newContainerCache(dc, 10*time.Second)
	now := time.Now()
	dc.cache.now = func() time.Time { return now }
	cctx := withCachedListing(context.Background())

	for i := 0; i < 2; i++ {
		out, err := dc.listAll(cctx, "")
		if err != nil {
			t.Fatalf("listAll #%d: %v", i+1, err)
		}
		if len(out) != 1 {
			t.Fatalf("listAll #%d: got %d containers, want 1", i+1, len(out))
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("two cached lists cost %d docker hits, want 1", got)
	}

	dc.cache.invalidate()
	if _, err := dc.listAll(cctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("list after invalidate cost %d total hits, want 2", got)
	}

	now = now.Add(11 * time.Second)
	if _, err := dc.listAll(cctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("list after max-age cost %d total hits, want 3", got)
	}
}

func TestContainerCacheSingleflight(t *testing.T) {
	var hits atomic.Int32
	entered := make(chan struct{})
	var enteredOnce sync.Once
	release := make(chan struct{})
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/containers/json") {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		enteredOnce.Do(func() { close(entered) })
		<-release
		json.NewEncoder(w).Encode([]dockerContainer{{ID: "c1"}, {ID: "c2"}})
	}))
	cache := newContainerCache(dc, 10*time.Second)

	const n = 10
	var wg sync.WaitGroup
	lens := make([]int, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := cache.list(context.Background())
			lens[i], errs[i] = len(out), err
		}(i)
	}
	<-entered
	// Give the other nine a moment to pile up behind the in-flight fetch.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Fatalf("%d concurrent misses cost %d docker hits, want 1", n, got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: %v", i, errs[i])
		}
		if lens[i] != 2 {
			t.Errorf("goroutine %d: got %d containers, want 2", i, lens[i])
		}
	}
}

func TestContainerCacheInProcessFilters(t *testing.T) {
	var hits atomic.Int32
	var sawFilters atomic.Bool
	items := []dockerContainer{
		{ID: "c1", Names: []string{"/goproxy-onb-web-1"}, State: "running", Labels: map[string]string{labelService: "web"}},
		{ID: "c2", Names: []string{"/goproxy-web-2"}, State: "exited", Labels: map[string]string{labelService: "web", labelEnable: "true"}},
		{ID: "c3", Names: []string{"/other"}, State: "running", Labels: map[string]string{labelService: "api", labelEnable: "true"}},
		{ID: "c4", Names: []string{"/plain"}, State: "running", Labels: map[string]string{}},
	}
	dc := cacheStub(t, items, &hits, &sawFilters)
	dc.cache = newContainerCache(dc, 10*time.Second)
	cctx := withCachedListing(context.Background())

	ids := func(cs []dockerContainer) string {
		var s []string
		for _, c := range cs {
			s = append(s, c.ID)
		}
		return strings.Join(s, ",")
	}
	cases := []struct {
		name, filter string
		running      bool
		want         string
	}{
		{"label presence", `{"label":["proxy.service"]}`, false, "c1,c2,c3"},
		{"label equals", `{"label":["proxy.service=web"]}`, false, "c1,c2"},
		{"name substring", `{"name":["goproxy-onb-web-"]}`, false, "c1"},
		{"running with label", `{"label":["proxy.enable=true"]}`, true, "c3"},
		{"empty filter", ``, false, "c1,c2,c3,c4"},
	}
	for _, tc := range cases {
		var out []dockerContainer
		var err error
		if tc.running {
			out, err = dc.listRunning(cctx, tc.filter)
		} else {
			out, err = dc.listAll(cctx, tc.filter)
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := ids(out); got != tc.want {
			t.Errorf("%s: got [%s], want [%s]", tc.name, got, tc.want)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("in-process filters cost %d docker hits, want 1", got)
	}
	if sawFilters.Load() {
		t.Fatal("cached fetch passed a filters= query to docker; it must fetch unfiltered")
	}

	// Filters the cache can't apply in-process fall through to Docker with
	// the filter intact.
	if _, err := dc.listAll(cctx, `{"status":["running"]}`); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("status filter cost %d total hits, want 2 (must go to docker)", got)
	}
	if !sawFilters.Load() {
		t.Fatal("status filter reached docker without its filters= query")
	}
	if _, err := dc.listAll(cctx, `{"label":["proxy.service"],"name":["x"]}`); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("two-key filter cost %d total hits, want 3 (must go to docker)", got)
	}
}

func TestContainerCacheNonCachedContextHitsDocker(t *testing.T) {
	var hits atomic.Int32
	dc := cacheStub(t, nil, &hits, nil)
	dc.cache = newContainerCache(dc, 10*time.Second)
	for i := 0; i < 2; i++ {
		if _, err := dc.listAll(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("plain-context lists cost %d docker hits, want 2 (cache must be opt-in)", got)
	}
}

func TestContainerCacheWatchEvents(t *testing.T) {
	var hits, connections atomic.Int32
	proceed1 := make(chan struct{})
	proceed2 := make(chan struct{})
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			hits.Add(1)
			json.NewEncoder(w).Encode([]dockerContainer{{ID: "c1"}})
		case strings.HasSuffix(r.URL.Path, "/events"):
			if connections.Add(1) > 1 {
				// Later reconnects just hold the stream open until the
				// watcher's ctx is cancelled.
				<-r.Context().Done()
				return
			}
			if got := r.URL.Query().Get("filters"); got != `{"type":["container"]}` {
				t.Errorf("events filters = %q", got)
			}
			fl := w.(http.Flusher)
			write := func(line string) {
				w.Write([]byte(line + "\n"))
				fl.Flush()
			}
			// A benign event straight away so the test can tell when the
			// connect-time invalidate has already happened.
			write(`{"Type":"container","Action":"exec_create: /bin/sh"}`)
			<-proceed1
			write(`{"Type":"container","Action":"exec_start: /bin/sh"}`)
			<-proceed2
			write(`{"Type":"container","Action":"die"}`)
			// Return → EOF on the client → reconnect.
		default:
			http.NotFound(w, r)
		}
	}))
	cache := newContainerCache(dc, 10*time.Second)
	cache.reconnectMin, cache.reconnectMax = time.Millisecond, time.Millisecond
	type hookEvent struct {
		action      string
		invalidated bool
	}
	events := make(chan hookEvent, 16)
	cache.eventHook = func(action string, invalidated bool) {
		events <- hookEvent{action, invalidated}
	}
	dc.cache = cache
	cctx := withCachedListing(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		cache.watchEvents(ctx)
	}()
	next := func() hookEvent {
		t.Helper()
		select {
		case ev := <-events:
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for an event")
			return hookEvent{}
		}
	}

	if ev := next(); !strings.HasPrefix(ev.action, "exec_create") || ev.invalidated {
		t.Fatalf("event #1 = %+v, want exec_create, not invalidating", ev)
	}
	// Prime the cache now that the connect-time invalidate is behind us.
	if _, err := dc.listAll(cctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("prime cost %d hits, want 1", got)
	}

	close(proceed1)
	if ev := next(); !strings.HasPrefix(ev.action, "exec_start") || ev.invalidated {
		t.Fatalf("event #2 = %+v, want exec_start, not invalidating", ev)
	}
	if _, err := dc.listAll(cctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("list after exec_start cost %d total hits, want 1 (exec events must not invalidate)", got)
	}

	close(proceed2)
	if ev := next(); ev.action != "die" || !ev.invalidated {
		t.Fatalf("event #3 = %+v, want die, invalidating", ev)
	}
	if _, err := dc.listAll(cctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("list after die cost %d total hits, want 2 (die must invalidate)", got)
	}

	// The stream ended after "die"; the watcher must reconnect on its own.
	deadline := time.Now().Add(5 * time.Second)
	for connections.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("watcher never reconnected: connections = %d", connections.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("watchEvents did not return after cancel")
	}
}

func TestInvalidateAfterWrite(t *testing.T) {
	var hits atomic.Int32
	dc := cacheStub(t, nil, &hits, nil)
	dc.cache = newContainerCache(dc, 10*time.Second)
	cctx := withCachedListing(context.Background())
	h := invalidateAfterWrite(dc, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	do := func(method string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/x", nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: status = %d", method, rec.Code)
		}
	}

	if _, err := dc.listAll(cctx, ""); err != nil {
		t.Fatal(err)
	}
	do(http.MethodGet)
	if _, err := dc.listAll(cctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("GET through the wrapper invalidated: %d hits, want 1", got)
	}
	do(http.MethodPost)
	if _, err := dc.listAll(cctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("POST through the wrapper did not invalidate: %d hits, want 2", got)
	}

	// newDashboardMux is built with dc == nil in tests, and the cache is
	// nil until main.go attaches it — neither may panic.
	for _, nilish := range []*dockerClient{nil, {}} {
		rec := httptest.NewRecorder()
		invalidateAfterWrite(nilish, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).
			ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/x", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("nil-ish dc: status = %d", rec.Code)
		}
	}
}

func TestServicesEndpointUsesContainerCache(t *testing.T) {
	var hits atomic.Int32
	dc := cacheStub(t, []dockerContainer{{
		ID: "c1", Names: []string{"/goproxy-web-1"}, State: "running",
		Image:  "ghcr.io/org/web:v1",
		Labels: map[string]string{labelEnable: "true", labelService: "web", labelHost: "web.example"},
	}}, &hits, nil)
	dc.cache = newContainerCache(dc, 10*time.Second)

	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	ic := newImageChecker(dc)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/api/services", nil)
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET #%d: status = %d, body %s", i+1, rec.Code, rec.Body.String())
		}
		var svcs []Service
		if err := json.Unmarshal(rec.Body.Bytes(), &svcs); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(svcs) != 1 || svcs[0].Name != "web" {
			t.Fatalf("GET #%d: services = %+v, want just web", i+1, svcs)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("two GET /api/services cost %d /containers/json hits, want 1", got)
	}
}

func TestPeerServicesHandlerUsesContainerCache(t *testing.T) {
	var hits atomic.Int32
	dc := cacheStub(t, []dockerContainer{{
		ID: "c1", Names: []string{"/goproxy-app-1"}, State: "running",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
	}}, &hits, nil)
	dc.cache = newContainerCache(dc, 10*time.Second)
	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	ic := newImageChecker(dc)
	h := peerServicesHandler("s3cret", "dashboard-b", dc, onb, ic, nil)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/peer/services", nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET #%d: status = %d, body %s", i+1, rec.Code, rec.Body.String())
		}
		var body peerServicesResp
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Services) != 1 {
			t.Fatalf("GET #%d: services = %+v, want one", i+1, body.Services)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("two GET /peer/services cost %d /containers/json hits, want 1", got)
	}
}
