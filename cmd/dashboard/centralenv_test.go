package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func newTestCentralCache(t *testing.T) *centralEnvCache {
	t.Helper()
	c, err := loadCentralEnvCache(filepath.Join(t.TempDir(), "central-env-cache"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// newTestCentralEnv is an enabled resolver for identity with an empty store
// and cache, no secrets and no fetcher.
func newTestCentralEnv(t *testing.T, identity string) *centralEnv {
	t.Helper()
	return &centralEnv{enabled: true, identity: identity, store: newTestCentralStore(t), cache: newTestCentralCache(t)}
}

func TestCentralEnvCachePutRejectsOlderAndRotatesPrev(t *testing.T) {
	c := newTestCentralCache(t)
	if err := c.Put("app", "o", 3, []string{"A=3"}); err != nil {
		t.Fatal(err)
	}
	var older errCentralEnvCacheOlder
	if err := c.Put("app", "o", 2, []string{"A=2"}); !errors.As(err, &older) || older.Cached != 3 {
		t.Fatalf("older Put = %v", err)
	}
	if err := c.Put("app", "o", 5, []string{"A=5"}); err != nil {
		t.Fatal(err)
	}
	got, ok := c.Get("app")
	if !ok || got.Version != 5 || !reflect.DeepEqual(got.Env, []string{"A=5"}) || got.PrevVersion != 3 || !reflect.DeepEqual(got.PrevEnv, []string{"A=3"}) {
		t.Fatalf("after rotation = %+v", got)
	}
	// Same version refreshes without rotating prev away.
	c.Put("app", "o", 5, []string{"A=5"})
	got, _ = c.Get("app")
	if got.PrevVersion != 3 {
		t.Fatalf("same-version Put rotated prev: %+v", got)
	}
	// Survives a reload, and Get is a copy.
	got.Env[0] = "mutated"
	reloaded, err := loadCentralEnvCache(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	disk, _ := reloaded.Get("app")
	if disk.Version != 5 || disk.Env[0] != "A=5" {
		t.Fatalf("reloaded = %+v", disk)
	}
	if err := c.Delete("app"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("app"); ok {
		t.Fatal("Delete left the entry")
	}
}

func TestCentralEnvCacheConcurrentPutGet(t *testing.T) {
	c := newTestCentralCache(t)
	var wg sync.WaitGroup
	for i := 1; i <= 40; i++ {
		wg.Add(2)
		go func(v uint64) {
			defer wg.Done()
			_ = c.Put("app", "o", v, []string{fmt.Sprintf("V=%d", v)})
		}(uint64(i))
		go func() {
			defer wg.Done()
			if e, ok := c.Get("app"); ok && len(e.Env) == 1 && e.Env[0] != fmt.Sprintf("V=%d", e.Version) {
				t.Errorf("torn entry: %+v", e)
			}
		}()
	}
	wg.Wait()
	got, _ := c.Get("app")
	if got.Version != 40 {
		t.Fatalf("final version = %d, want 40 (highest wins, never rolled back)", got.Version)
	}
	reloaded, _ := loadCentralEnvCache(c.dir)
	disk, _ := reloaded.Get("app")
	if !reflect.DeepEqual(disk, got) {
		t.Fatalf("disk %+v != memory %+v", disk, got)
	}
}

func TestStampEnvLabelsCopiesAndStripsCompose(t *testing.T) {
	src := map[string]string{
		labelService: "app", "com.docker.compose.project": "stack", "com.docker.compose.service": "app",
		labelEnvOrigin: "stale", "other": "kept",
	}
	orig := map[string]string{}
	for k, v := range src {
		orig[k] = v
	}
	out := stampEnvLabels(src, centralEnvResult{Origin: "dashboard-a", Version: 7})
	if !reflect.DeepEqual(src, orig) {
		t.Fatalf("stampEnvLabels mutated its source: %v", src)
	}
	want := map[string]string{labelService: "app", "other": "kept", labelEnvOrigin: "dashboard-a", labelEnvVersion: "7"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("stamped = %v, want %v", out, want)
	}
	out["other"] = "changed"
	if src["other"] != "kept" {
		t.Fatal("stamped map aliases the source")
	}
}

func TestSubtractImageEnv(t *testing.T) {
	image := []string{"PATH=/usr/bin", "LANG=C", "PORT=80"}
	container := []string{"PATH=/usr/bin", "LANG=en_US", "PORT=80", "DB=x", "NOEQUALS", "EMPTY="}
	got := subtractImageEnv(container, image)
	want := map[string]string{"LANG": "en_US", "DB": "x", "EMPTY": ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("subtractImageEnv = %v, want %v", got, want)
	}
}

func TestCentralEnvResolveOrigin(t *testing.T) {
	ce := newTestCentralEnv(t, "dashboard-a")
	ce.secrets = newTestSecrets(t, "app", "TOKEN=tok")
	ce.store.Create("app", "dashboard-a", map[string]string{"A": "1", "T": "ref:TOKEN"}, map[string]map[string]string{"dashboard-a": {"A": "self"}, "dashboard-b": {"A": "peer"}}, "", "")

	res, managed, err := ce.Resolve(context.Background(), "app", nil)
	if err != nil || !managed || res.Stale || res.Origin != "dashboard-a" || res.Version != 1 || !reflect.DeepEqual(res.Env, []string{"A=self", "T=tok"}) {
		t.Fatalf("origin Resolve = %+v, %v, %v", res, managed, err)
	}
	peer, owned, err := ce.ResolveForPeer("app", "dashboard-b")
	if err != nil || !owned || !reflect.DeepEqual(peer.Env, []string{"A=peer", "T=tok"}) {
		t.Fatalf("ResolveForPeer = %+v, %v, %v", peer, owned, err)
	}
	if err := ce.Accept("app", "dashboard-b", 9, []string{"A=x"}, nil); err == nil {
		t.Fatal("origin accepted a peer-supplied copy of its own service")
	}
}

func TestCentralEnvResolveNonOriginFetchOK(t *testing.T) {
	ce := newTestCentralEnv(t, "dashboard-b")
	var gotFor string
	ce.fetchFromOrigin = func(_ context.Context, origin, svc, forIdentity string) (centralEnvFetched, error) {
		gotFor = origin + "/" + svc + "/" + forIdentity
		return centralEnvFetched{Env: []string{"A=fresh"}, Version: 4}, nil
	}
	res, managed, err := ce.Resolve(context.Background(), "app", map[string]string{labelEnvOrigin: "dashboard-a"})
	if err != nil || !managed || res.Stale || res.Version != 4 || !reflect.DeepEqual(res.Env, []string{"A=fresh"}) {
		t.Fatalf("Resolve = %+v, %v, %v", res, managed, err)
	}
	if gotFor != "dashboard-a/app/dashboard-b" {
		t.Fatalf("fetch called with %q", gotFor)
	}
	if cached, ok := ce.cache.Get("app"); !ok || cached.Version != 4 {
		t.Fatalf("fetch result not cached: %+v", cached)
	}
}

func TestCentralEnvResolveFetchFailUsesStaleCache(t *testing.T) {
	ce := newTestCentralEnv(t, "dashboard-b")
	ce.cache.Put("app", "dashboard-a", 3, []string{"A=cached"})
	ce.fetchFromOrigin = func(context.Context, string, string, string) (centralEnvFetched, error) {
		return centralEnvFetched{}, errors.New("dial tcp: connection refused")
	}
	// No origin label (pre-stamp replica): the cache alone marks it managed.
	res, managed, err := ce.Resolve(context.Background(), "app", nil)
	if err != nil || !managed || !res.Stale || res.Version != 3 || !reflect.DeepEqual(res.Env, []string{"A=cached"}) {
		t.Fatalf("Resolve = %+v, %v, %v", res, managed, err)
	}
	if !strings.Contains(res.Warning, "dashboard-a") || !strings.Contains(res.Warning, "version 3") {
		t.Fatalf("warning = %q", res.Warning)
	}
	// A nil fetcher (PR-A) behaves the same.
	ce.fetchFromOrigin = nil
	if res, _, err := ce.Resolve(context.Background(), "app", nil); err != nil || !res.Stale {
		t.Fatalf("nil fetcher Resolve = %+v, %v", res, err)
	}
	// An origin answering with an OLDER version than cached doesn't roll back.
	ce.fetchFromOrigin = func(context.Context, string, string, string) (centralEnvFetched, error) {
		return centralEnvFetched{Env: []string{"A=old"}, Version: 1}, nil
	}
	if res, _, err := ce.Resolve(context.Background(), "app", nil); err != nil || res.Version != 3 {
		t.Fatalf("older fetch Resolve = %+v, %v", res, err)
	}
}

func TestCentralEnvResolveFetchFailNoCacheRefuses(t *testing.T) {
	ce := newTestCentralEnv(t, "dashboard-b")
	_, managed, err := ce.Resolve(context.Background(), "app", map[string]string{labelEnvOrigin: "dashboard-a"})
	var unavailable errCentralEnvUnavailable
	if !managed || !errors.As(err, &unavailable) || unavailable.Origin != "dashboard-a" {
		t.Fatalf("Resolve = managed %v, %v", managed, err)
	}
	// Labels naming THIS host as origin with no store record fail closed too.
	_, managed, err = ce.Resolve(context.Background(), "app", map[string]string{labelEnvOrigin: "dashboard-b"})
	if !managed || !errors.As(err, &unavailable) {
		t.Fatalf("self-origin without record = managed %v, %v", managed, err)
	}
	// A cache from a different origin is not a fallback for this one.
	ce.cache.Put("app", "dashboard-z", 2, []string{"A=z"})
	_, _, err = ce.Resolve(context.Background(), "app", map[string]string{labelEnvOrigin: "dashboard-a"})
	if !errors.As(err, &unavailable) {
		t.Fatalf("foreign-origin cache used as fallback: %v", err)
	}
}

func TestCentralEnvResolveUnmanagedAndDisabled(t *testing.T) {
	ce := newTestCentralEnv(t, "dashboard-a")
	if _, managed, err := ce.Resolve(context.Background(), "app", map[string]string{labelService: "app"}); managed || err != nil {
		t.Fatalf("unmanaged Resolve = %v, %v", managed, err)
	}
	if ce.Managed("app", nil) {
		t.Fatal("Managed true for an unknown service")
	}
	ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "", "")
	ce.enabled = false
	if _, managed, _ := ce.Resolve(context.Background(), "app", nil); managed {
		t.Fatal("disabled resolver reported managed")
	}
	var nilCE *centralEnv
	if nilCE.Enabled() {
		t.Fatal("nil resolver enabled")
	}
}

// TestCentralEnvNeverLeaksValues seeds a sentinel value everywhere a value
// can live — store base, a per-host override, a secrets file, a peer cache,
// and a per-request env edit — then drives every error/warning/log path this
// PR adds and asserts the sentinel never appears in any of them.
func TestCentralEnvNeverLeaksValues(t *testing.T) {
	const sentinel = "SENTINEL-S3CRET-9f2a"
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	prev := log.Writer()
	log.SetOutput(&lockedWriter{w: &logBuf, mu: &logMu})
	t.Cleanup(func() { log.SetOutput(prev) })

	var outputs []string
	collect := func(what string, err error) {
		if err != nil {
			outputs = append(outputs, what+": "+err.Error())
		}
	}

	ce := newTestCentralEnv(t, "dashboard-a")
	ce.secrets = newTestSecrets(t, "app", "S="+sentinel)
	ce.store.Create("app", "dashboard-a", map[string]string{"P": sentinel, "R": "ref:S"}, map[string]map[string]string{"dashboard-b": {"O": sentinel}}, "", "")

	// Conflict, replay, bad key, missing ref, broken record.
	_, err := ce.store.Apply("app", centralEnvChange{IfVersion: 99, Base: map[string]string{"P": sentinel}}, "")
	collect("conflict", err)
	_, err = ce.store.Apply("app", centralEnvChange{IfVersion: 1, Base: map[string]string{"BAD=" + "K": sentinel}}, "")
	collect("badkey", err)
	ce.store.Apply("app", centralEnvChange{IfVersion: 1, RequestID: "r", Base: map[string]string{"P": sentinel, "R": "ref:NOPE"}}, "")
	_, _, err = ce.Resolve(context.Background(), "app", nil)
	collect("ref failure", err)
	_, _, err = ce.ResolveForPeer("app", "dashboard-b")
	collect("peer ref failure", err)
	_, err = ce.store.effectiveEnv("app", "dashboard-b", ce.secrets)
	collect("effectiveEnv", err)

	corruptDir := t.TempDir()
	writeFileAtomic(filepath.Join(corruptDir, "bad.json"), []byte(`{"service":"bad","base":{"K":"`+sentinel+`",}`), 0o600)
	broken, _ := loadCentralEnvStore(corruptDir)
	_, err = broken.effectiveEnv("bad", "h", nil)
	collect("broken", err)

	// Non-origin: stale warning and older-version cache refusal.
	peer := newTestCentralEnv(t, "dashboard-b")
	peer.cache.Put("app", "dashboard-a", 5, []string{"P=" + sentinel})
	peer.fetchFromOrigin = func(context.Context, string, string, string) (centralEnvFetched, error) {
		return centralEnvFetched{}, errors.New("upstream said " + sentinel)
	}
	res, _, err := peer.Resolve(context.Background(), "app", nil)
	collect("stale", err)
	outputs = append(outputs, "warning: "+res.Warning)
	collect("older", peer.Accept("app", "dashboard-a", 1, []string{"P=" + sentinel}, nil))
	collect("unavailable", func() error {
		_, _, err := peer.Resolve(context.Background(), "other", map[string]string{labelEnvOrigin: "dashboard-a"})
		return err
	}())

	// Create-path refusals through the real dockerClient.
	withFastRecreate(t)
	f := newCenvFakeDocker()
	f.seedTemplate(nil)
	dc := f.client(t)
	dc.central = ce
	collect("scale", dc.scaleService(context.Background(), "app", 2))
	ce.store.Apply("app", centralEnvChange{IfVersion: 2, Base: map[string]string{"P": sentinel}}, "")
	collect("replace edit", dc.replaceService(context.Background(), "app", ReplaceServiceRequest{Image: "x:2", Env: map[string]string{"P": sentinel}}))
	collect("canary edit", dc.createCanaryReplicas(context.Background(), "app", ReplaceServiceRequest{Image: "x:2", Env: map[string]string{"P": sentinel}}, 1))
	peerDC := f.client(t)
	peerDC.central = peer
	collect("peer scale", peerDC.scaleService(context.Background(), "app", 3))

	// PR-B: peer endpoints, propagation, reconcile, API and MCP.
	outputs = append(outputs, centralEnvPropagationOutputs(t, sentinel)...)
	// PR-C: adopt (dry run, import, un-adopt, execute), live-keys, release, MCP.
	outputs = append(outputs, centralEnvAdoptOutputs(t, sentinel)...)

	logMu.Lock()
	outputs = append(outputs, "log: "+logBuf.String())
	logMu.Unlock()
	if len(outputs) < 10 {
		t.Fatalf("expected every path to produce output, got %d: %v", len(outputs), outputs)
	}
	for _, o := range outputs {
		if strings.Contains(o, sentinel) {
			t.Fatalf("sentinel leaked: %s", o)
		}
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestPeerHandshakeAdvertisesFeaturesOnlyWhenSet(t *testing.T) {
	post := func(h http.Handler) map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/peer/handshake", nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if _, ok := post(peerHandshakeHandler("s3cret", "a", "1", false, nil))["features"]; ok {
		t.Fatal("flag-off handshake advertised features")
	}
	got := post(peerHandshakeHandler("s3cret", "a", "1", false, []string{centralEnvFeature}))["features"]
	if !reflect.DeepEqual(got, []any{centralEnvFeature}) {
		t.Fatalf("features = %v", got)
	}
}

func TestPeerHandshakeFeaturesRoundTrip(t *testing.T) {
	newPeer := httptest.NewServer(peerHandshakeHandler("s3cret", "dashboard-b", "7", true, []string{centralEnvFeature}))
	defer newPeer.Close()
	// An old peer: exactly the pre-feature response shape.
	oldPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"peer":"dashboard-c","ok":true,"version":"6","writes":true}`))
	}))
	defer oldPeer.Close()

	reg := newPeerRegistry([]string{newPeer.URL, oldPeer.URL}, "s3cret", "dashboard-a", "7", 0, nil)
	reg.send(context.Background(), newPeer.URL)
	reg.send(context.Background(), oldPeer.URL)
	st := reg.Status()
	if !reflect.DeepEqual(st[newPeer.URL].Features, []string{centralEnvFeature}) {
		t.Fatalf("new peer features = %v", st[newPeer.URL].Features)
	}
	if len(st[oldPeer.URL].Features) != 0 {
		t.Fatalf("old peer features = %v, want none", st[oldPeer.URL].Features)
	}
	// Status hands out copies.
	st[newPeer.URL].Features[0] = "mutated"
	if reg.Status()[newPeer.URL].Features[0] != centralEnvFeature {
		t.Fatal("Status shares the Features slice")
	}
	// A failed handshake preserves; a successful downgrade clears.
	reg.recordResult(newPeer.URL, false, "", "", false, nil)
	if len(reg.Status()[newPeer.URL].Features) != 1 {
		t.Fatal("failed handshake dropped features")
	}
	reg.recordResult(newPeer.URL, true, "dashboard-b", "8", true, nil)
	if len(reg.Status()[newPeer.URL].Features) != 0 {
		t.Fatal("downgraded peer kept its features")
	}
	// peersStatusHandler surfaces them.
	reg.recordResult(newPeer.URL, true, "dashboard-b", "8", true, []string{centralEnvFeature})
	rec := httptest.NewRecorder()
	peersStatusHandler(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/peers", nil))
	if !strings.Contains(rec.Body.String(), `"features":["`+centralEnvFeature+`"]`) {
		t.Fatalf("peers status body = %s", rec.Body.String())
	}
}

func TestPeerRegistryFeaturesConcurrent(t *testing.T) {
	reg := newPeerRegistry([]string{"http://p"}, "s", "a", "1", 0, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			reg.recordResult("http://p", i%3 != 0, "b", "1", true, []string{centralEnvFeature, fmt.Sprint(i)})
		}(i)
		go func() {
			defer wg.Done()
			for _, st := range reg.Status() {
				_ = append(st.Features, "x")
			}
		}()
	}
	wg.Wait()
}
