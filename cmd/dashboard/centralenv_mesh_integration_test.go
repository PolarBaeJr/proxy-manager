package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two in-process dashboards — A (origin) and B — each with its own fake
// Docker, central env, propagation manager, peer-mesh server and dashboard
// API, wired to each other exactly as main.go wires real ones: registries
// keyed by identity with handshake features, the real origin fetcher, the
// real /peer/central-env/ handler.

type meshNode struct {
	*syncHost
	identity string
	addr     string
	url      string
	hs       *http.Server
	mux      http.Handler
	// setDelay stalls /set before handling it — an origin that applies a
	// forwarded write but answers too late.
	setDelay atomic.Int64
	mu       sync.Mutex
	notifies []string
	peerMux  http.Handler
}

func (n *meshNode) notifyLog() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.notifies...)
}

// start (re)listens on the node's fixed peer address; stop closes the
// listener, so connections are refused — a host that is off, not one
// answering errors.
func (n *meshNode) start(t *testing.T) {
	t.Helper()
	addr := n.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	n.addr = ln.Addr().String()
	n.url = "http://" + n.addr
	n.hs = &http.Server{Handler: http.HandlerFunc(n.serve)}
	go n.hs.Serve(ln)
}

func (n *meshNode) stop() { n.hs.Close() }

func (n *meshNode) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/notify") {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		n.mu.Lock()
		n.notifies = append(n.notifies, string(b))
		n.mu.Unlock()
	}
	if strings.HasSuffix(r.URL.Path, "/set") {
		if d := n.setDelay.Load(); d > 0 {
			time.Sleep(time.Duration(d))
		}
	}
	n.peerMux.ServeHTTP(w, r)
}

// orderLog records creates across both hosts, in the order they happened.
type orderLog struct {
	mu  sync.Mutex
	seq []string
}

func (o *orderLog) add(s string) {
	o.mu.Lock()
	o.seq = append(o.seq, s)
	o.mu.Unlock()
}

func (o *orderLog) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.seq...)
}

// newMesh builds A and B. bCapable=false makes B a dashboard without
// central env: it doesn't advertise the feature and its /peer/central-env/
// answers 404, but it still runs "app" (so /peer/services lists it).
func newMesh(t *testing.T, bCapable bool) (a, b *meshNode, order *orderLog) {
	t.Helper()
	setInternalToken(t)
	a = &meshNode{identity: "dashboard-a"}
	b = &meshNode{identity: "dashboard-b"}
	a.start(t)
	b.start(t)
	t.Cleanup(func() { a.stop(); b.stop() })

	var bFeatures []string
	if bCapable {
		bFeatures = []string{centralEnvFeature}
	}
	regA := newTestPeerRegistryFeatures("dashboard-a", b.url, "dashboard-b", true, bFeatures)
	regB := newTestPeerRegistryFeatures("dashboard-b", a.url, "dashboard-a", true, []string{centralEnvFeature})
	a.syncHost = newSyncHost(t, "dashboard-a", regA, "s3cret")
	b.syncHost = newSyncHost(t, "dashboard-b", regB, "s3cret")
	a.ce.fetchFromOrigin = newOriginFetcher(regA, "s3cret")
	b.ce.fetchFromOrigin = newOriginFetcher(regB, "s3cret")

	order = &orderLog{}
	a.f.onCreate = func(name string) { order.add("dashboard-a:" + name) }
	b.f.onCreate = func(name string) { order.add("dashboard-b:" + name) }

	for _, n := range []*meshNode{a, b} {
		ce := n.ce
		if n == b && !bCapable {
			ce = nil
		}
		identity := n.identity
		pm := http.NewServeMux()
		pm.Handle("/peer/central-env/", peerCentralEnvHandler("s3cret", ce, n.dc, true))
		pm.HandleFunc("/peer/services", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(peerServicesResp{Identity: identity, Services: []Service{{Name: "app"}}})
		})
		n.peerMux = pm
		auth, _ := newConfirmedStore(t, "alice", "correct horse")
		reg := regA
		if n == b {
			reg = regB
		}
		n.mux = newDashboardMux(n.dc, nil, auth, newRateLimiter(), newImageChecker(n.dc), "", nil, newTestOnboardedStore(t), nil, nil, nil, nil, nil, reg, n.rm, nil, n.rom)
	}
	return a, b, order
}

// seedConverged puts both hosts at v1: A owns {A=1, SHARED=s}, B has it
// cached and runs a replica of it that ALSO carries a key only B ever had.
func seedConverged(a, b *meshNode) {
	a.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1", "SHARED": "s"}, nil, "test", "")
	a.f.seedMember("a1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1", "SHARED=s"}, cenvTemplateHealth)
	b.ce.cache.PutWithHealthcheck("app", "dashboard-a", 1, []string{"A=1", "SHARED=s"}, cenvTemplateHealth)
	b.f.seedMember("b1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1", "LOCAL_ONLY=x", "SHARED=s"}, cenvTemplateHealth)
}

// dropPeerHealthcheck strips B's own replica of its healthcheck, so any
// healthcheck on what B creates can only have come from the origin (via the
// fetch, or the cache it filled).
func dropPeerHealthcheck(b *meshNode) {
	b.f.mu.Lock()
	in := b.f.inspect["b1"]
	in.health = nil
	b.f.inspect["b1"] = in
	b.f.mu.Unlock()
}

func assertCreatesCarryOriginHealthcheck(t *testing.T, n *meshNode) {
	t.Helper()
	creates := n.f.createsSnapshot()
	if len(creates) == 0 {
		t.Fatalf("%s created nothing", n.identity)
	}
	for _, c := range creates {
		if !reflect.DeepEqual(c.body.Healthcheck, cenvTemplateHealth) {
			t.Fatalf("%s: %s healthcheck = %+v, want the origin's", n.identity, c.name, c.body.Healthcheck)
		}
	}
}

func apiDo(t *testing.T, mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader = strings.NewReader(body)
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// waitFor polls cond until true or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func hostResult(j *envSyncJob, host string) envSyncHostResult {
	for _, h := range j.Hosts {
		if h.Host == host {
			return h
		}
	}
	return envSyncHostResult{}
}

// TestMeshSetOnPeerForwardsToOriginAndRollsOriginFirst: an edit made on B
// lands on A (the origin), A rolls its own replicas first, then B's — and
// B's replica ends up with exactly the central env, its local-only key
// gone, without anyone touching B.
func TestMeshSetOnPeerForwardsToOriginAndRollsOriginFirst(t *testing.T) {
	withFastSync(t)
	a, b, order := newMesh(t, true)
	seedConverged(a, b)
	dropPeerHealthcheck(b)

	rec := apiDo(t, b.mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"2"}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"origin":"dashboard-a"`) || !strings.Contains(rec.Body.String(), `"version":2`) {
		t.Fatalf("set via B = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := b.ce.store.Get("app"); ok {
		t.Fatal("B wrote an origin record of its own")
	}
	ja := a.waitJob(t, "app")
	if ja.Status != envSyncStatusConverged || hostResult(ja, "dashboard-b").Status != envSyncHostConverged {
		t.Fatalf("A job = %+v", ja)
	}
	jb := b.waitJob(t, "app")
	if jb.Status != envSyncStatusConverged || jb.Target != 2 {
		t.Fatalf("B job = %+v", jb)
	}

	seq := order.snapshot()
	if len(seq) != 2 || !strings.HasPrefix(seq[0], "dashboard-a:") || !strings.HasPrefix(seq[1], "dashboard-b:") {
		t.Fatalf("create order = %v, want A then B", seq)
	}
	for n, mv := range b.f.members() {
		if strings.Join(mv.env, ",") != "A=2,SHARED=s" || mv.version != "2" {
			t.Fatalf("B %s = %+v (local-only key must be dropped)", n, mv)
		}
	}
	if c, _ := b.ce.cache.Get("app"); c.Version != 2 {
		t.Fatalf("B cache at v%d", c.Version)
	}
	assertCreatesCarryOriginHealthcheck(t, b)
	view := apiDo(t, b.mux, "GET", "/api/services/app/env", "")
	var v centralEnvView
	json.Unmarshal(view.Body.Bytes(), &v)
	if !v.Managed || v.Role != envSyncRolePeer || v.Version != 2 || v.Stale || len(v.Hosts) != 2 || !v.Hosts[0].Converged || !v.Hosts[1].Converged {
		t.Fatalf("B view = %s", view.Body.String())
	}
}

// TestMeshOriginDownThenReconcile: with A off, B still scales from its
// cache and says so (stale); a version A took while B couldn't hear about
// it is pulled by B's reconcile the moment A is back.
func TestMeshOriginDownThenReconcile(t *testing.T) {
	withFastSync(t)
	a, b, _ := newMesh(t, true)
	seedConverged(a, b)
	dropPeerHealthcheck(b)

	a.stop()
	if err := b.dc.scaleService(context.Background(), "app", 2); err != nil {
		t.Fatalf("scale with origin down: %v", err)
	}
	assertCreatesCarryOriginHealthcheck(t, b)
	for n, mv := range b.f.members() {
		if mv.version != "1" {
			t.Fatalf("B %s = %+v", n, mv)
		}
	}
	rec := apiDo(t, b.mux, "GET", "/api/services/app/env", "")
	var v centralEnvView
	json.Unmarshal(rec.Body.Bytes(), &v)
	if !v.Stale || v.Version != 1 || strings.Join(v.Keys, ",") != "A,SHARED" || len(v.Warnings) == 0 {
		t.Fatalf("B view with origin down = %s", rec.Body.String())
	}
	if rec := apiDo(t, b.mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"9"}}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("set with origin down = %d %s", rec.Code, rec.Body.String())
	}

	// A moves on while B can't hear it (no job on A, so no notify).
	if _, err := a.ce.store.Apply("app", centralEnvChange{IfVersion: 1, Base: map[string]string{"A": "2", "SHARED": "s"}}, "test"); err != nil {
		t.Fatal(err)
	}
	b.m.reconcileOnce(context.Background())
	if b.m.busy("app") {
		t.Fatal("reconcile started a job with the origin down")
	}

	a.start(t)
	b.m.reconcileOnce(context.Background())
	j := b.waitJob(t, "app")
	if j.Status != envSyncStatusConverged || j.Target != 2 {
		t.Fatalf("B job after origin returned = %+v", j)
	}
	ms := b.f.members()
	if len(ms) != 2 {
		t.Fatalf("B members = %+v", ms)
	}
	for n, mv := range ms {
		if mv.version != "2" || !envHas(mv.env, "A=2") {
			t.Fatalf("B %s = %+v", n, mv)
		}
	}
	assertCreatesCarryOriginHealthcheck(t, b)
}

// TestMeshOriginGateFailureNeverReachesPeer: a version that fails on the
// origin is reverted there and B never runs it — no notify for it, no
// replica created from it. (B does get the revert — same content as what it
// runs, as a new version — which recreates its replica once.)
func TestMeshOriginGateFailureNeverReachesPeer(t *testing.T) {
	withFastSync(t)
	a, b, _ := newMesh(t, true)
	seedConverged(a, b)
	a.f.setUnhealthy(func(c createBody) bool { return envHas(c.Env, "A=bad") })

	if rec := apiDo(t, a.mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"bad"}}`); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	a.waitJob(t, "app")
	waitFor(t, "B idle", func() bool { return !b.m.busy("app") })
	rec, _, _ := a.ce.store.Get("app")
	if rec.Version != 3 || rec.State != centralEnvStateReverted {
		t.Fatalf("A store = v%d %s", rec.Version, rec.State)
	}
	for _, n := range b.notifyLog() {
		if strings.Contains(n, `"version":2`) {
			t.Fatalf("B was notified of the failing version: %s", n)
		}
	}
	for _, c := range b.f.createsSnapshot() {
		if envHas(c.body.Env, "A=bad") {
			t.Fatalf("B created a replica from the failing version: %v", c.body.Env)
		}
	}
	for n, mv := range b.f.members() {
		if !envHas(mv.env, "A=1") {
			t.Fatalf("B %s = %+v", n, mv)
		}
	}
	// The revert's own (converged) job replaced the failed one in the job
	// slot; the failure itself must still be visible.
	var v centralEnvView
	json.Unmarshal(apiDo(t, a.mux, "GET", "/api/services/app/env", "").Body.Bytes(), &v)
	if v.Job == nil || v.Job.Status != envSyncStatusConverged || v.LastFailure == nil ||
		v.LastFailure.Status != envSyncStatusFailedReverted || v.LastFailure.Version != 2 {
		t.Fatalf("A view job=%+v last_failure=%+v", v.Job, v.LastFailure)
	}
	if !strings.Contains(strings.Join(v.Warnings, " "), "most recent failure: v2 ended failed_reverted") {
		t.Fatalf("A view warnings = %v", v.Warnings)
	}
	json.Unmarshal(apiDo(t, b.mux, "GET", "/api/services/app/env", "").Body.Bytes(), &v)
	if v.LastFailure == nil || v.LastFailure.Status != envSyncStatusFailedReverted {
		t.Fatalf("B view (from the origin) last_failure = %+v", v.LastFailure)
	}
}

// TestMeshPeerFailureRollsBackAndOriginPartial: B's own replica fails the
// new version's gate → B rolls itself back, A keeps the new version and
// reports partial, and reconcile doesn't keep re-trying B at that version.
func TestMeshPeerFailureRollsBackAndOriginPartial(t *testing.T) {
	withFastSync(t)
	a, b, _ := newMesh(t, true)
	seedConverged(a, b)
	b.f.setUnhealthy(func(c createBody) bool { return envHas(c.Env, "B=bfail") })

	body := `{"if_version":1,"set":{"A":"2"},"host_overrides":{"dashboard-b":{"B":"bfail"}}}`
	if rec := apiDo(t, a.mux, "POST", "/api/services/app/env", body); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	ja := a.waitJob(t, "app")
	if ja.Status != envSyncStatusPartial || hostResult(ja, "dashboard-b").Status != envSyncHostRolledBack || hostResult(ja, "dashboard-a").Status != envSyncHostConverged {
		t.Fatalf("A job = %+v", ja)
	}
	jb := b.waitJob(t, "app")
	if jb.Status != envSyncStatusFailedRolledBack {
		t.Fatalf("B job = %+v", jb)
	}
	if rec, _, _ := a.ce.store.Get("app"); rec.Version != 2 || rec.State != centralEnvStateActive {
		t.Fatalf("origin was reverted for a peer failure: v%d %s", rec.Version, rec.State)
	}
	for n, mv := range b.f.members() {
		if mv.version != "1" || !envHas(mv.env, "A=1") || envHas(mv.env, "B=bfail") {
			t.Fatalf("B %s after rollback = %+v", n, mv)
		}
	}
	for n, mv := range a.f.members() {
		if mv.version != "2" || !envHas(mv.env, "A=2") {
			t.Fatalf("A %s = %+v", n, mv)
		}
	}
	notified := len(b.notifyLog())
	a.m.reconcileOnce(context.Background())
	b.m.reconcileOnce(context.Background())
	waitFor(t, "idle", func() bool { return !a.m.busy("app") && !b.m.busy("app") })
	if len(b.notifyLog()) != notified || b.m.busy("app") {
		t.Fatal("reconcile re-tried B at the version it already failed")
	}

	// B's cache went back to v1 with v2 marked failed, so a later scale on
	// B — whose Resolve still reaches A, which still serves v2 — creates
	// from v1, never from the env that just failed.
	if c, _ := b.ce.cache.Get("app"); c.Version != 1 || c.FailedVersion != 2 {
		t.Fatalf("B cache after rollback = v%d failed=v%d", c.Version, c.FailedVersion)
	}
	createsBefore := len(b.f.createsSnapshot())
	if err := b.dc.scaleService(context.Background(), "app", 2); err != nil {
		t.Fatalf("scale on B: %v", err)
	}
	creates := b.f.createsSnapshot()
	if len(creates) != createsBefore+1 {
		t.Fatalf("scale created %d", len(creates)-createsBefore)
	}
	if nc := creates[len(creates)-1]; !envHas(nc.body.Env, "A=1") || envHas(nc.body.Env, "B=bfail") || nc.body.Labels[labelEnvVersion] != "1" {
		t.Fatalf("scaled replica = v%s %v, want the rolled-back v1", nc.body.Labels[labelEnvVersion], nc.body.Env)
	}

	// Both views keep the failure visible.
	var v centralEnvView
	json.Unmarshal(apiDo(t, a.mux, "GET", "/api/services/app/env", "").Body.Bytes(), &v)
	if v.LastFailure == nil || v.LastFailure.Status != envSyncStatusPartial || v.LastFailure.Version != 2 ||
		hostResult(&envSyncJob{Hosts: v.LastFailure.Hosts}, "dashboard-b").Status != envSyncHostRolledBack {
		t.Fatalf("A view last_failure = %+v", v.LastFailure)
	}
	if !strings.Contains(strings.Join(v.Warnings, " "), "dashboard-b failed v2 and stays on v1") {
		t.Fatalf("A view warnings = %v", v.Warnings)
	}

	// An operator's sync on the origin (after fixing the cause) retries B:
	// the notify carries retry, B forgets v2 failed, and converges.
	b.f.setUnhealthy(nil)
	if rec := apiDo(t, a.mux, "POST", "/api/services/app/env/sync", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("sync = %d", rec.Code)
	}
	ja = a.waitJob(t, "app")
	b.waitJob(t, "app")
	if ja.Status != envSyncStatusConverged {
		t.Fatalf("A job after sync = %+v", ja)
	}
	if c, _ := b.ce.cache.Get("app"); c.Version != 2 || c.FailedVersion != 0 {
		t.Fatalf("B cache after retry = v%d failed=v%d", c.Version, c.FailedVersion)
	}
	for n, mv := range b.f.members() {
		if mv.version != "2" || !envHas(mv.env, "B=bfail") {
			t.Fatalf("B %s after retry = %+v", n, mv)
		}
	}
}

// TestMeshForwardedSetTimeoutRetrySameRequestID: the origin applies a
// forwarded edit but answers too late → B says "outcome unknown" (504) with
// the request_id; retrying with it replays instead of bumping again.
func TestMeshForwardedSetTimeoutRetrySameRequestID(t *testing.T) {
	withFastSync(t)
	centralEnvForwardTimeout = 50 * time.Millisecond
	a, b, _ := newMesh(t, true)
	seedConverged(a, b)
	a.setDelay.Store(int64(300 * time.Millisecond))

	body := `{"request_id":"rid-1","if_version":1,"set":{"A":"2"}}`
	rec := apiDo(t, b.mux, "POST", "/api/services/app/env", body)
	if rec.Code != http.StatusGatewayTimeout || !strings.Contains(rec.Body.String(), "rid-1") {
		t.Fatalf("slow origin = %d %s", rec.Code, rec.Body.String())
	}
	waitFor(t, "the late write to land on A", func() bool {
		r, _, _ := a.ce.store.Get("app")
		return r.Version == 2
	})
	a.setDelay.Store(0)
	// The retry's own round trip must not race the tiny timeout on a
	// loaded runner — that would be a second, spurious 504.
	centralEnvForwardTimeout = 5 * time.Second
	rec = apiDo(t, b.mux, "POST", "/api/services/app/env", body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"replayed":true`) || !strings.Contains(rec.Body.String(), `"version":2`) {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body.String())
	}
	if r, _, _ := a.ce.store.Get("app"); r.Version != 2 {
		t.Fatalf("A at v%d after a retried request — want exactly one bump", r.Version)
	}
	a.waitJob(t, "app")
	b.waitJob(t, "app")
}

// TestMeshPeerWithoutFeatureIsUnsupported: a peer that runs the service but
// has no central env is reported unsupported (and never touched), not
// silently left behind.
func TestMeshPeerWithoutFeatureIsUnsupported(t *testing.T) {
	withFastSync(t)
	a, b, _ := newMesh(t, false)
	a.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	a.f.seedMember("a1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)
	b.f.seedMember("b1", "goproxy-app-1", "", 0, []string{"A=1"}, nil)

	if rec := apiDo(t, a.mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"2"}}`); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	j := a.waitJob(t, "app")
	if hostResult(j, "dashboard-b").Status != envSyncHostUnsupported || !strings.Contains(strings.Join(j.Warnings, " "), "does not support central env") {
		t.Fatalf("A job = %+v", j)
	}
	if len(b.f.createsSnapshot()) != 0 || len(b.notifyLog()) != 0 {
		t.Fatal("an unsupported peer was touched")
	}
}
