package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// GET /env, set and sync for a service that runs ONLY on a peer (its
// origin): asked of a host that knows nothing about it, they are forwarded
// — to ?host=, or to the peer discovery finds.

// peerOnlyMesh has "app" adopted with B as its origin and running only on
// B; A has no replica, record or cache for it.
func peerOnlyMesh(t *testing.T) (a, b *meshNode) {
	t.Helper()
	a, b, _ = newMesh(t, true)
	b.ce.store.Create("app", "dashboard-b", map[string]string{"A": "1", "SHARED": "s"}, nil, "test", "")
	b.f.seedMember("b1", "goproxy-app-1", "dashboard-b", 1, []string{"A=1", "SHARED=s"}, cenvTemplateHealth)
	return a, b
}

// envForwardCounter counts the forwarded-env peer actions (view,
// set-request, sync-request) that reach n. Other peer traffic (B's
// propagation asking A's /status) is left out on purpose.
func envForwardCounter(n *meshNode) *atomic.Int32 {
	var c atomic.Int32
	n.mu.Lock()
	inner := n.peerMux
	n.mu.Unlock()
	n.setPeerMux(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, s := range []string{"/view", "/set-request", "/sync-request"} {
			if strings.HasSuffix(r.URL.Path, s) {
				c.Add(1)
			}
		}
		inner.ServeHTTP(w, r)
	}))
	return &c
}

func decodeView(t *testing.T, rec *httptest.ResponseRecorder) centralEnvView {
	t.Helper()
	var v centralEnvView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
		t.Fatalf("view = %d %s", rec.Code, rec.Body.String())
	}
	return v
}

func assertANeverLearned(t *testing.T, a *meshNode) {
	t.Helper()
	if _, ok := a.ce.cache.Get("app"); ok || a.ce.store.Has("app") {
		t.Fatal("A learned about app — later assertions would test the local path, not discovery")
	}
}

// TestMeshEnvGetForwardedToPeer: A knows nothing of app, yet its GET /env
// shows B's view — found by asking the peers, or named with ?host=. ?host=
// naming A itself stays local (and does no discovery).
func TestMeshEnvGetForwardedToPeer(t *testing.T) {
	withFastSync(t)
	a, b := peerOnlyMesh(t)
	bCalls := envForwardCounter(b)
	aCalls := envForwardCounter(a)

	for _, path := range []string{"/api/services/app/env", "/api/services/app/env?host=dashboard-b"} {
		v := decodeView(t, apiDo(t, a.mux, "GET", path, ""))
		if !v.Managed || v.Origin != "dashboard-b" || v.Role != envSyncRoleOrigin || v.Version != 1 || strings.Join(v.Keys, ",") != "A,SHARED" {
			t.Fatalf("%s = %+v", path, v)
		}
	}
	if bCalls.Load() != 2 || aCalls.Load() != 0 {
		t.Fatalf("B saw %d forwarded views (want 2), A saw %d (want 0)", bCalls.Load(), aCalls.Load())
	}
	if v := decodeView(t, apiDo(t, a.mux, "GET", "/api/services/app/env?host=dashboard-a", "")); v.Managed || bCalls.Load() != 2 {
		t.Fatalf("?host=self = %+v (B calls %d)", v, bCalls.Load())
	}
	if rec := apiDo(t, a.mux, "GET", "/api/services/app/env?host=dashboard-z", ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "unknown host dashboard-z") {
		t.Fatalf("unknown host = %d %s", rec.Code, rec.Body.String())
	}
	// A service nobody manages: B is asked once, never asks A back.
	if v := decodeView(t, apiDo(t, a.mux, "GET", "/api/services/ghost/env", "")); v.Managed || bCalls.Load() != 3 || aCalls.Load() != 0 {
		t.Fatalf("unmanaged everywhere = %+v (B calls %d, A calls %d)", v, bCalls.Load(), aCalls.Load())
	}
	assertANeverLearned(t, a)
	// A locally-known service is answered locally — no peer is asked.
	a.ce.store.Create("mine", "dashboard-a", map[string]string{"M": "1"}, nil, "test", "")
	if v := decodeView(t, apiDo(t, a.mux, "GET", "/api/services/mine/env", "")); !v.Managed || v.Origin != "dashboard-a" || bCalls.Load() != 3 {
		t.Fatalf("local service = %+v (B calls %d)", v, bCalls.Load())
	}
}

// TestMeshEnvSetForwardedToPeer: a set made on A lands in B's store and
// rolls on B — discovered, or named with ?host=. The request_id minted on
// A replays on B.
func TestMeshEnvSetForwardedToPeer(t *testing.T) {
	withFastSync(t)
	for _, host := range []string{"", "dashboard-b"} {
		t.Run("host="+host, func(t *testing.T) {
			a, b := peerOnlyMesh(t)
			aCalls := envForwardCounter(a)
			path := withHost("/api/services/app/env", host)
			rec := apiDo(t, a.mux, "POST", path, `{"if_version":1,"set":{"A":"2"}}`)
			var res struct {
				Version   uint64 `json:"version"`
				Origin    string `json:"origin"`
				RequestID string `json:"request_id"`
			}
			json.Unmarshal(rec.Body.Bytes(), &res)
			if rec.Code != http.StatusOK || res.Version != 2 || res.Origin != "dashboard-b" || res.RequestID == "" || !b.ce.store.HasRequestID("app", res.RequestID) {
				t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
			}
			if r, _, _ := b.ce.store.Get("app"); r.Version != 2 || r.Base["A"] != "2" {
				t.Fatalf("B record v%d", r.Version)
			}
			if j := b.waitJob(t, "app"); j.Status != envSyncStatusConverged || j.Target != 2 {
				t.Fatalf("B job = %+v", j)
			}
			if a.ce.store.Has("app") || aCalls.Load() != 0 {
				t.Fatalf("A wrote a record or was asked back (%d)", aCalls.Load())
			}
			if host == "" {
				assertANeverLearned(t, a)
			}
			rec = apiDo(t, a.mux, "POST", path, `{"request_id":"`+res.RequestID+`","if_version":1,"set":{"A":"2"}}`)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"replayed":true`) {
				t.Fatalf("replay = %d %s", rec.Code, rec.Body.String())
			}
			rec = apiDo(t, a.mux, "POST", path, `{"if_version":1,"set":{"A":"3"}}`)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"current_version":2`) {
				t.Fatalf("stale if_version = %d %s", rec.Code, rec.Body.String())
			}
			b.waitJob(t, "app")
		})
	}
}

// TestMeshEnvWriteNoDiscoveryForLocalUnmanaged: A runs its own unmanaged
// "app" while B manages one of the same name — a set or sync on A without
// ?host= must stay local (404), never land on B's record.
func TestMeshEnvWriteNoDiscoveryForLocalUnmanaged(t *testing.T) {
	withFastSync(t)
	a, b := peerOnlyMesh(t)
	a.f.seedMember("a1", "goproxy-app-1", "", 0, []string{"A=local"}, cenvTemplateHealth)
	aCalls, bCalls := envForwardCounter(a), envForwardCounter(b)
	if rec := apiDo(t, a.mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"2"}}`); rec.Code != http.StatusNotFound {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	if rec := apiDo(t, a.mux, "POST", "/api/services/app/env/sync", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("sync = %d %s", rec.Code, rec.Body.String())
	}
	if r, _, _ := b.ce.store.Get("app"); r.Version != 1 || bCalls.Load() != 0 || aCalls.Load() != 0 {
		t.Fatalf("B touched: v%d, B asked %d, A asked %d", r.Version, bCalls.Load(), aCalls.Load())
	}
}

// TestMeshEnvSetForwardTimeoutRetrySameRequestID: B applies but answers
// too late → 504 with the request_id A minted; retrying it replays on B.
func TestMeshEnvSetForwardTimeoutRetrySameRequestID(t *testing.T) {
	withFastSync(t)
	a, b := peerOnlyMesh(t)
	b.mu.Lock()
	inner := b.peerMux
	b.mu.Unlock()
	var delay atomic.Int64
	b.setPeerMux(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := delay.Load(); d > 0 && strings.HasSuffix(r.URL.Path, "/set-request") {
			rec := httptest.NewRecorder()
			inner.ServeHTTP(rec, r.WithContext(context.WithoutCancel(r.Context())))
			time.Sleep(time.Duration(d))
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
			return
		}
		inner.ServeHTTP(w, r)
	}))
	centralEnvForwardTimeout = 50 * time.Millisecond
	delay.Store(int64(300 * time.Millisecond))
	rec := apiDo(t, a.mux, "POST", "/api/services/app/env?host=dashboard-b", `{"if_version":1,"set":{"A":"2"}}`)
	var unknown struct {
		RequestID string `json:"request_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &unknown)
	if rec.Code != http.StatusGatewayTimeout || unknown.RequestID == "" {
		t.Fatalf("slow peer = %d %s", rec.Code, rec.Body.String())
	}
	waitFor(t, "the late set to land on B", func() bool { return b.ce.store.HasRequestID("app", unknown.RequestID) })
	delay.Store(0)
	centralEnvForwardTimeout = 5 * time.Second
	rec = apiDo(t, a.mux, "POST", "/api/services/app/env?host=dashboard-b", `{"request_id":"`+unknown.RequestID+`","if_version":1,"set":{"A":"2"}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"replayed":true`) {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body.String())
	}
	if r, _, _ := b.ce.store.Get("app"); r.Version != 2 {
		t.Fatalf("B at v%d after a retried set — want exactly one", r.Version)
	}
	b.waitJob(t, "app")
}

// TestMeshEnvSyncForwardedToPeer: a sync on A starts B's propagation.
func TestMeshEnvSyncForwardedToPeer(t *testing.T) {
	withFastSync(t)
	for _, host := range []string{"", "dashboard-b"} {
		t.Run("host="+host, func(t *testing.T) {
			a, b := peerOnlyMesh(t)
			if _, ok := b.m.get("app"); ok {
				t.Fatal("B already had a job")
			}
			rec := apiDo(t, a.mux, "POST", withHost("/api/services/app/env/sync", host), "")
			if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "sync requested") {
				t.Fatalf("sync = %d %s", rec.Code, rec.Body.String())
			}
			if j := b.waitJob(t, "app"); j.Status != envSyncStatusConverged || j.Target != 1 {
				t.Fatalf("B job = %+v", j)
			}
		})
	}
}

func TestMeshEnvForwardErrors(t *testing.T) {
	withFastSync(t)
	oldPeer := func(t *testing.T, a, b *meshNode) {
		// A central env dashboard from before /view etc. existed.
		inner := b.peerMuxFor(b.ce, true)
		b.setPeerMux(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, s := range []string{"/view", "/set-request", "/sync-request"} {
				if strings.HasSuffix(r.URL.Path, s) {
					http.NotFound(w, r)
					return
				}
			}
			inner.ServeHTTP(w, r)
		}))
	}
	stopB := func(t *testing.T, a, b *meshNode) {
		b.stop()
		t.Cleanup(func() { b.start(t) })
	}
	writesOff := func(t *testing.T, a, b *meshNode) { b.setPeerMux(b.peerMuxFor(b.ce, false)) }
	for _, tc := range []struct {
		name         string
		setup        func(t *testing.T, a, b *meshNode)
		method, path string
		code         int
		want         string
	}{
		{"set unknown host", nil, "POST", "/api/services/app/env?host=dashboard-z", http.StatusNotFound, "unknown host dashboard-z"},
		{"sync unknown host", nil, "POST", "/api/services/app/env/sync?host=dashboard-z", http.StatusNotFound, "unknown host dashboard-z"},
		{"set invalid host", nil, "POST", "/api/services/app/env?host=a%2Fb", http.StatusBadRequest, "invalid host"},
		{"set writes off", writesOff, "POST", "/api/services/app/env?host=dashboard-b", http.StatusConflict, "does not support forwarded env set"},
		// /view needs no -peer-writes, so discovery still finds B.
		{"discovered set writes off", writesOff, "POST", "/api/services/app/env", http.StatusConflict, "does not support forwarded env set"},
		{"sync writes off", writesOff, "POST", "/api/services/app/env/sync?host=dashboard-b", http.StatusConflict, "does not support forwarded env sync"},
		{"get writes off", writesOff, "GET", "/api/services/app/env", http.StatusOK, `"origin":"dashboard-b"`},
		{"get peer down", stopB, "GET", "/api/services/app/env", http.StatusOK, `"managed":false`},
		{"get host peer down", stopB, "GET", "/api/services/app/env?host=dashboard-b", http.StatusServiceUnavailable, "dashboard-b is unreachable"},
		{"set peer down", stopB, "POST", "/api/services/app/env", http.StatusNotFound, "not centrally managed"},
		{"set host peer down", stopB, "POST", "/api/services/app/env?host=dashboard-b", http.StatusServiceUnavailable, "dashboard-b is unreachable"},
		{"sync peer down", stopB, "POST", "/api/services/app/env/sync", http.StatusNotFound, "not centrally managed"},
		{"get old peer", oldPeer, "GET", "/api/services/app/env", http.StatusOK, `"managed":false`},
		{"get host old peer", oldPeer, "GET", "/api/services/app/env?host=dashboard-b", http.StatusConflict, "does not support forwarded env view"},
		{"set old peer", oldPeer, "POST", "/api/services/app/env", http.StatusNotFound, "not centrally managed"},
		{"sync old peer", oldPeer, "POST", "/api/services/app/env/sync", http.StatusNotFound, "not centrally managed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := peerOnlyMesh(t)
			if tc.setup != nil {
				tc.setup(t, a, b)
			}
			body := ""
			if tc.method == "POST" && !strings.Contains(tc.path, "/sync") {
				body = `{"if_version":1,"set":{"A":"2"}}`
			}
			rec := apiDo(t, a.mux, tc.method, tc.path, body)
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("= %d %s, want %d %q", rec.Code, rec.Body.String(), tc.code, tc.want)
			}
			if r, _, _ := b.ce.store.Get("app"); r.Version != 1 || a.ce.store.Has("app") {
				t.Fatal("a refused set was applied")
			}
		})
	}
}

// TestMeshEnvPeerActionsNeverForward: B's /view, /set-request and
// /sync-request act on B alone — a ?host= there is ignored, and a service
// B doesn't know is refused (409, never 404) rather than looked up on A.
func TestMeshEnvPeerActionsNeverForward(t *testing.T) {
	withFastSync(t)
	a, b := peerOnlyMesh(t)
	a.ce.store.Create("ghost", "dashboard-a", map[string]string{"G": "1"}, nil, "test", "")
	aCalls := envForwardCounter(a)

	rec := peerDo(t, b.peerMux, "GET", "/peer/central-env/app/view?host=dashboard-a", "s3cret", "")
	if v := decodeView(t, rec); !v.Managed || v.Origin != "dashboard-b" {
		t.Fatalf("peer view = %+v", v)
	}
	rec = peerDo(t, b.peerMux, "POST", "/peer/central-env/app/set-request?host=dashboard-a", "s3cret", `{"if_version":1,"set":{"A":"2"}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"origin":"dashboard-b"`) || !strings.Contains(rec.Body.String(), `"request_id"`) {
		t.Fatalf("peer set-request = %d %s", rec.Code, rec.Body.String())
	}
	b.waitJob(t, "app")
	rec = peerDo(t, b.peerMux, "POST", "/peer/central-env/app/sync-request?host=dashboard-a", "s3cret", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("peer sync-request = %d %s", rec.Code, rec.Body.String())
	}
	b.waitJob(t, "app")

	// ghost is A's alone: B knows nothing, and must not go looking.
	if v := decodeView(t, peerDo(t, b.peerMux, "GET", "/peer/central-env/ghost/view", "s3cret", "")); v.Managed {
		t.Fatalf("B's view of ghost = %+v", v)
	}
	for _, p := range []string{"/peer/central-env/ghost/set-request", "/peer/central-env/ghost/sync-request"} {
		rec := peerDo(t, b.peerMux, "POST", p, "s3cret", `{"if_version":1,"set":{"G":"2"}}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not centrally managed") {
			t.Fatalf("%s = %d %s", p, rec.Code, rec.Body.String())
		}
	}
	if aCalls.Load() != 0 {
		t.Fatalf("A saw %d forwarded env calls, want 0", aCalls.Load())
	}
	if r, _, _ := a.ce.store.Get("ghost"); r.Version != 1 {
		t.Fatalf("ghost at v%d", r.Version)
	}
}

// TestMeshEnvPeerSetRequestSelfGuard: the peer runs its own own-service
// guard, and A reports it as a refusal — not as a credentials problem.
func TestMeshEnvPeerSetRequestSelfGuard(t *testing.T) {
	withFastSync(t)
	a, b := peerOnlyMesh(t)
	withSelfHostname(t, func() (string, error) { return "b1", nil })
	rec := apiDo(t, a.mux, "POST", "/api/services/app/env?host=dashboard-b", `{"if_version":1,"set":{"A":"2"}}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "own service") {
		t.Fatalf("self-guarded set = %d %s", rec.Code, rec.Body.String())
	}
	if r, _, _ := b.ce.store.Get("app"); r.Version != 1 {
		t.Fatal("applied anyway")
	}
}

// centralEnvForwardOutputs drives the forwarded GET / set / sync, their
// peer actions and the MCP tools over a peer-only service whose env is
// sentinel everywhere, for TestCentralEnvNeverLeaksValues.
func centralEnvForwardOutputs(t *testing.T, sentinel string) []string {
	withFastSync(t)
	readAudit := withAuditFile(t)
	a, b, _ := newMesh(t, true)
	b.ce.store.Create("app", "dashboard-b", map[string]string{"P": sentinel}, map[string]map[string]string{"dashboard-a": {"O": sentinel}}, "test", "")
	b.f.seedMember("b1", "goproxy-app-1", "dashboard-b", 1, []string{"P=" + sentinel}, cenvTemplateHealth)
	var out []string
	add := func(what, s string) { out = append(out, what+": "+s) }
	api := func(method, path, body string) {
		rec := apiDo(t, a.mux, method, path, body)
		add("dashboard-a "+method+" "+path, rec.Body.String())
	}
	api("GET", "/api/services/app/env", "")
	api("GET", "/api/services/app/env?host=dashboard-b", "")
	api("POST", "/api/services/app/env", `{"if_version":1,"set":{"P2":"`+sentinel+`"}}`)
	b.waitJob(t, "app")
	api("POST", "/api/services/app/env?host=dashboard-b", `{"if_version":1,"set":{"P3":"`+sentinel+`"}}`)
	api("POST", "/api/services/app/env?host=dashboard-b", `{"if_version":2,"set":{"BAD=K":"`+sentinel+`"}}`)
	api("POST", "/api/services/app/env/sync", "")
	b.waitJob(t, "app")
	for _, p := range []struct{ method, path, body string }{
		{"GET", "/peer/central-env/app/view", ""},
		{"POST", "/peer/central-env/app/set-request", `{"if_version":1,"set":{"P":"` + sentinel + `"}}`},
		{"POST", "/peer/central-env/app/sync-request", ""},
	} {
		rec := peerDo(t, b.peerMux, p.method, p.path, "s3cret", p.body)
		add("dashboard-b peer "+p.method+" "+p.path, rec.Body.String())
	}
	b.waitJob(t, "app")
	s := NewServer("t", "v")
	registerMCPTools(s, &apiCaller{mux: a.mux}, true, true)
	for _, c := range []struct{ tool, args string }{
		{"get_service_env", `{"service":"app"}`},
		{"get_service_env", `{"service":"app","host":"dashboard-b"}`},
		{"set_service_env", `{"service":"app","host":"dashboard-b","if_version":2,"set":{"P4":"` + sentinel + `"}}`},
	} {
		res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+c.tool+`","arguments":`+c.args+`}}`)
		j, _ := json.Marshal(res)
		add("mcp "+c.tool, string(j))
	}
	b.waitJob(t, "app")
	entries, _ := json.Marshal(readAudit())
	add("audit", string(entries))
	return out
}

func TestCentralEnvForwardOutputsAreNotEmpty(t *testing.T) {
	outs := strings.Join(centralEnvForwardOutputs(t, "SENTINEL-S3CRET-9f2a"), "\n")
	for _, want := range []string{`"managed":true`, `"origin":"dashboard-b"`, `"request_id"`, `"current_version"`, "sync requested", `"overrides"`, "service.env_set", "service.env_sync_start"} {
		if !strings.Contains(outs, want) {
			t.Errorf("outputs never contained %q", want)
		}
	}
}
