package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

// svcCallTracker records every docker HTTP call a services-write test's
// docker stub saw, guarded by a mutex — same explicit-synchronization
// convention as imageswritehost_test.go's deletionTracker.
type svcCallTracker struct {
	mu   sync.Mutex
	hits []string
}

func (c *svcCallTracker) record(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hits = append(c.hits, s)
}

func (c *svcCallTracker) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.hits...)
}

// newTestOnboardedStore is a fresh on-disk-backed OnboardedStore — needed by
// runServiceScale (onb.Get) even on the label-managed path, since a nil
// *OnboardedStore panics on first use.
func newTestOnboardedStore(t *testing.T) *OnboardedStore {
	t.Helper()
	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	return onb
}

// noopProxyStub stands in for the proxy's /refresh endpoint wherever a test
// doesn't care about proxyRefresh specifically — keeps runServiceLifecycle's
// unconditional proxyRefresh call from falling through to the unreachable
// defaultProxyURL.
func noopProxyStub(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

// twoReplicaContainers is the standard ("app", 2 replicas) fixture: c1 is
// the original (never removed by scale-down), c2 is a goproxy-prefixed
// clone (removable). Both share the same state and unscalable label.
func twoReplicaContainers(state string, unscalable bool) []dockerContainer {
	lbl := "false"
	if unscalable {
		lbl = "true"
	}
	labels := func() map[string]string {
		return map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80", labelUnscalable: lbl}
	}
	return []dockerContainer{
		{ID: "c1", Names: []string{"/app"}, State: state, Image: "ghcr.io/org/app:v1", Labels: labels()},
		{ID: "c2", Names: []string{"/goproxy-app-2"}, State: state, Image: "ghcr.io/org/app:v1", Labels: labels()},
	}
}

// canaryContainers is a live "app" replica plus a staged canary member.
func canaryContainers() []dockerContainer {
	return []dockerContainer{
		{ID: "c1", Names: []string{"/app"}, State: "running", Image: "ghcr.io/org/app:v1",
			Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"}},
		{ID: "c2", Names: []string{"/goproxy-app-c"}, State: "running", Image: "ghcr.io/org/app:v2",
			Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80", labelCanary: "true"}},
	}
}

// servicesDockerStub is the shared docker-daemon stub shape for every test
// below: GET /containers/json (listAll/listServices, any filter) returns
// containers; POST .../stop, POST .../start, and DELETE /containers/<id>
// (scale-down's removeContainer) are recorded and answered 200.
func servicesDockerStub(t *testing.T, calls *svcCallTracker, containers []dockerContainer) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/stop"):
			calls.record("stop " + r.URL.Path)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/start"):
			calls.record("start " + r.URL.Path)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/"):
			calls.record("remove " + r.URL.Path)
			w.WriteHeader(http.StatusOK)
		default:
			json.NewEncoder(w).Encode(containers)
		}
	}))
}

// newPeerServicesServer stands up a peer's own /peer/services/ write handler
// on an httptest.Server, backed by a fresh docker stub over containers, and
// returns the server plus the call tracker recording every docker hit it saw.
func newPeerServicesServer(t *testing.T, containers []dockerContainer, writesEnabled bool) (*httptest.Server, *svcCallTracker) {
	t.Helper()
	calls := &svcCallTracker{}
	dc := servicesDockerStub(t, calls, containers)
	onb := newTestOnboardedStore(t)
	proxy := noopProxyStub(t)
	srv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", dc, onb, "", proxy, writesEnabled))
	t.Cleanup(srv.Close)
	return srv, calls
}

func newTestPeerRegistry(peerURL string, writes bool) *PeerRegistry {
	reg := newPeerRegistry([]string{peerURL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerURL, true, "dashboard-b", "dev", writes)
	return reg
}

// newLocalTestMux wires a local dashboard mux (elevated auth via
// internalToken) against localDC and reg — every forwarding test's local
// side, and every no-?host= regression test's only side.
func newLocalTestMux(t *testing.T, localDC *dockerClient, reg *PeerRegistry) http.Handler {
	t.Helper()
	localOnb := newTestOnboardedStore(t)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)
	return newDashboardMux(localDC, nil, auth, newRateLimiter(), newImageChecker(localDC), "", nil, localOnb, nil, nil, nil, nil, nil, reg)
}

func doReq(mux http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestServicesMutationsForwardToPeer proves all five write-mesh endpoints —
// scale, stop, start, per-replica stop, per-replica start — reach the
// peer's own docker client (never the local stub) when called with
// ?host=<peer>.
func TestServicesMutationsForwardToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	t.Setenv("PROXY_URL", noopProxyStub(t))

	var localHit atomic.Bool
	newLocal := func(t *testing.T) *dockerClient {
		return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			localHit.Store(true)
			json.NewEncoder(w).Encode([]dockerContainer{})
		}))
	}

	t.Run("scale", func(t *testing.T) {
		peerSrv, calls := newPeerServicesServer(t, twoReplicaContainers("running", false), true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		req := httptest.NewRequest(http.MethodPost, "/api/services/app/scale?host=dashboard-b", strings.NewReader(`{"replicas":1}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — scale did not reach the peer")
		}
	})
	t.Run("stop", func(t *testing.T) {
		peerSrv, calls := newPeerServicesServer(t, twoReplicaContainers("running", false), true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodPost, "/api/services/app/stop?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — stop did not reach the peer")
		}
	})
	t.Run("start", func(t *testing.T) {
		peerSrv, calls := newPeerServicesServer(t, twoReplicaContainers("exited", false), true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodPost, "/api/services/app/start?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — start did not reach the peer")
		}
	})
	t.Run("replica stop", func(t *testing.T) {
		peerSrv, calls := newPeerServicesServer(t, twoReplicaContainers("running", false), true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodPost, "/api/services/app/replicas/goproxy-app-2/stop?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — replica stop did not reach the peer")
		}
	})
	t.Run("replica start", func(t *testing.T) {
		peerSrv, calls := newPeerServicesServer(t, twoReplicaContainers("exited", false), true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodPost, "/api/services/app/replicas/goproxy-app-2/start?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — replica start did not reach the peer")
		}
	})

	if localHit.Load() {
		t.Error("local docker stub was hit — ?host=<peer> requests must never touch the local daemon")
	}
}

// TestServicesPeerSelfGuardRejects proves the PEER's own self-guard rejects
// (403) a mutation targeting its own container, verified two ways: directly
// against peerServicesMutateHandler (proving the guard itself fires), and
// through the full ?host= forwarding path (proving no docker mutation call
// ever reaches the peer's daemon). The forwarding path asserts 502, not 403
// — mapPeerMutationErr deliberately never relays a peer's own 401/403
// verbatim (see its doc comment: that status range is the dashboard's own
// session-auth vocabulary), so a peer self-guard rejection is
// indistinguishable, from the caller's side, from a mesh-credential
// mismatch. That collision is accepted, documented behavior, not a bug this
// test should paper over.
func TestServicesPeerSelfGuardRejects(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	calls := &svcCallTracker{}
	peerDC := servicesDockerStub(t, calls, []dockerContainer{{
		ID: "abc123def456", Names: []string{"/dashboard"}, State: "running",
		Labels: map[string]string{labelEnable: "true", labelService: "dashboard", labelHost: "dashboard.example", labelPort: "8093"},
	}})
	peerOnb := newTestOnboardedStore(t)
	inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, "", noopProxyStub(t), true)

	directReq := httptest.NewRequest(http.MethodPost, "/peer/services/dashboard/stop", nil)
	directReq.Header.Set("Authorization", "Bearer s3cret")
	directRec := httptest.NewRecorder()
	inner.ServeHTTP(directRec, directReq)
	if directRec.Code != http.StatusForbidden {
		t.Fatalf("direct peer handler: status = %d, body %s, want 403", directRec.Code, directRec.Body.String())
	}
	if len(calls.all()) != 0 {
		t.Errorf("peer docker stub saw a mutation call during a self-guard rejection: %v", calls.all())
	}

	peerSrv := httptest.NewServer(inner)
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	rec := doReq(mux, http.MethodPost, "/api/services/dashboard/stop?host=dashboard-b")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("forwarded: status = %d, body %s, want 502 (mapPeerMutationErr never relays a peer 403 verbatim)", rec.Code, rec.Body.String())
	}
	if len(calls.all()) != 0 {
		t.Errorf("peer docker stub saw a mutation call during a self-guard rejection: %v", calls.all())
	}
}

// TestServicesUnscalableRejectedByPeer proves the PEER re-validates
// guardUnscalable against its own live state — a forwarded scale request
// asking for anything other than 1 replica on an unscalable service is
// rejected 400, relayed verbatim via mapPeerMutationErr.
func TestServicesUnscalableRejectedByPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerSrv, calls := newPeerServicesServer(t, twoReplicaContainers("running", true), true)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/scale?host=dashboard-b", strings.NewReader(`{"replicas":2}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s, want 400 (peer must re-validate unscalable)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unscalable") {
		t.Errorf("body = %q, want it to mention the unscalable rejection", rec.Body.String())
	}
	if len(calls.all()) != 0 {
		t.Errorf("peer docker stub saw a mutation call for a rejected scale: %v", calls.all())
	}
}

// TestServicesCanaryReplicaRejectedByPeer proves a forwarded per-replica stop
// of a canary member is rejected by the PEER's own live MemberSummaries
// (409), never trusting the requester's claim about which member is safe.
func TestServicesCanaryReplicaRejectedByPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerSrv, calls := newPeerServicesServer(t, canaryContainers(), true)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	rec := doReq(mux, http.MethodPost, "/api/services/app/replicas/goproxy-app-c/stop?host=dashboard-b")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s, want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "canary") {
		t.Errorf("body = %q, want it to mention canary", rec.Body.String())
	}
	if len(calls.all()) != 0 {
		t.Errorf("peer docker stub saw a mutation call for a rejected canary stop: %v", calls.all())
	}
}

// TestServicesProxyRefreshHitsPeerProxy is the one test that actually
// catches "forgot proxyRefresh in the peer handler": a stop forwarded to the
// peer must POST /refresh against the PEER's own proxy — the URL wired into
// peerServicesMutateHandler's proxyURL param — not any local one.
func TestServicesProxyRefreshHitsPeerProxy(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	var refreshHit atomic.Bool
	peerProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/refresh" {
			refreshHit.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(peerProxy.Close)

	calls := &svcCallTracker{}
	peerDC := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
	peerOnb := newTestOnboardedStore(t)
	peerSrv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, "", peerProxy.URL, true))
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	var localRefreshHit atomic.Bool
	localProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/refresh" {
			localRefreshHit.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(localProxy.Close)
	t.Setenv("PROXY_URL", localProxy.URL)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	rec := doReq(mux, http.MethodPost, "/api/services/app/stop?host=dashboard-b")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !refreshHit.Load() {
		t.Error("peer's own /refresh was never hit — runServiceLifecycle's proxyRefresh did not fire on the peer side")
	}
	if localRefreshHit.Load() {
		t.Error("the LOCAL proxy's /refresh was hit by a ?host=<peer> stop — must only refresh the peer's own proxy")
	}
}

// TestServicesMutationPeerWritesDisabled proves a peer with -peer-writes=false
// answers 404 to every one of the five /peer/services/* mutation endpoints.
func TestServicesMutationPeerWritesDisabled(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerSrv, _ := newPeerServicesServer(t, twoReplicaContainers("running", false), false)
	reg := newTestPeerRegistry(peerSrv.URL, false)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"scale", http.MethodPost, "/api/services/app/scale?host=dashboard-b", `{"replicas":1}`},
		{"stop", http.MethodPost, "/api/services/app/stop?host=dashboard-b", ""},
		{"start", http.MethodPost, "/api/services/app/start?host=dashboard-b", ""},
		{"replica stop", http.MethodPost, "/api/services/app/replicas/goproxy-app-2/stop?host=dashboard-b", ""},
		{"replica start", http.MethodPost, "/api/services/app/replicas/goproxy-app-2/start?host=dashboard-b", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Authorization", "Bearer "+internalToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
			}
		})
	}
}

// TestServicesMutationUnknownHost proves a ?host= not matching any known
// peer identity 404s without attempting to reach anything.
func TestServicesMutationUnknownHost(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	reg := newTestPeerRegistry("http://peer-b:8098", true)
	mux := newLocalTestMux(t, localDC, reg)

	rec := doReq(mux, http.MethodPost, "/api/services/app/stop?host=nonexistent-host")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

// TestServicesMutationNoRegistry proves a ?host= with a nil registry 404s.
func TestServicesMutationNoRegistry(t *testing.T) {
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, nil)

	rec := doReq(mux, http.MethodPost, "/api/services/app/stop?host=anything")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

// TestServicesMutationPeerUnreachable proves an unreachable peer surfaces as
// 502, via peerMutate's transport-level error path through mapPeerMutationErr.
func TestServicesMutationPeerUnreachable(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := peerSrv.URL
	peerSrv.Close() // guarantees connection-refused without hardcoding a port

	reg := newTestPeerRegistry(url, true)
	mux := newLocalTestMux(t, localDC, reg)

	rec := doReq(mux, http.MethodPost, "/api/services/app/stop?host=dashboard-b")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
}

// TestServicesMutationPeerAuthRejected proves a peer's own 401 (mesh-secret
// mismatch) maps to 502, never relayed verbatim.
func TestServicesMutationPeerAuthRejected(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "peer's own auth error body", http.StatusUnauthorized)
	}))
	t.Cleanup(peerSrv.Close)

	reg := newTestPeerRegistry(peerSrv.URL, true)
	mux := newLocalTestMux(t, localDC, reg)

	rec := doReq(mux, http.MethodPost, "/api/services/app/stop?host=dashboard-b")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
	if strings.Contains(rec.Body.String(), "peer's own auth error body") {
		t.Errorf("body = %q, must not contain the peer's own auth-failure body", rec.Body.String())
	}
}

// TestServicesMutationsLocalStillWork is the no-?host= regression test: all
// five actions must still work exactly as before the forwarding branch was
// added.
func TestServicesMutationsLocalStillWork(t *testing.T) {
	t.Setenv("PROXY_URL", noopProxyStub(t))

	t.Run("scale", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
		mux := newLocalTestMux(t, dc, nil)
		req := httptest.NewRequest(http.MethodPost, "/api/services/app/scale", strings.NewReader(`{"replicas":1}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("stop", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
		mux := newLocalTestMux(t, dc, nil)
		rec := doReq(mux, http.MethodPost, "/api/services/app/stop")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("start", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := servicesDockerStub(t, calls, twoReplicaContainers("exited", false))
		mux := newLocalTestMux(t, dc, nil)
		rec := doReq(mux, http.MethodPost, "/api/services/app/start")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("replica stop", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
		mux := newLocalTestMux(t, dc, nil)
		rec := doReq(mux, http.MethodPost, "/api/services/app/replicas/goproxy-app-2/stop")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("replica start", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := servicesDockerStub(t, calls, twoReplicaContainers("exited", false))
		mux := newLocalTestMux(t, dc, nil)
		rec := doReq(mux, http.MethodPost, "/api/services/app/replicas/goproxy-app-2/start")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
}

// TestServicesForwardingRunsBeforeSelfGuard is the critical ordering test:
// a LOCAL service that IS this process's own container must still forward
// (and be judged by the PEER's own self-guard, not the local one) when
// ?host= names an unrelated peer whose same-named service is NOT its own
// container. A 403 here without this test would look like "the guard is
// working" while actually misfiring against the wrong host's data.
func TestServicesForwardingRunsBeforeSelfGuard(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	t.Setenv("PROXY_URL", noopProxyStub(t))
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	// Peer's own "dashboard" service is a DIFFERENT container from this
	// test process's fake self — the peer's self-guard must not trip.
	calls := &svcCallTracker{}
	peerDC := servicesDockerStub(t, calls, []dockerContainer{{
		ID: "peer-owns-this-one", Names: []string{"/dashboard"}, State: "running",
		Labels: map[string]string{labelEnable: "true", labelService: "dashboard", labelHost: "dashboard.example", labelPort: "8093"},
	}})
	peerOnb := newTestOnboardedStore(t)
	peerSrv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, "", noopProxyStub(t), true))
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	// LOCAL's own "dashboard" service IS this test process's self — if the
	// forwarding branch ran AFTER the local self-guard, this request would
	// incorrectly 403 locally instead of ever reaching the peer.
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "abc123def456", Names: []string{"/dashboard"}, State: "running",
			Labels: map[string]string{labelEnable: "true", labelService: "dashboard", labelHost: "dashboard.example", labelPort: "8093"},
		}})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	rec := doReq(mux, http.MethodPost, "/api/services/dashboard/stop?host=dashboard-b")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s, want 200 — the LOCAL self-guard must never judge a peer-bound mutation", rec.Code, rec.Body.String())
	}
	if len(calls.all()) == 0 {
		t.Error("peer's own docker stub was never hit — the request did not forward")
	}
}

// TestServicesHostParamOwnIdentity proves ?host=<this host's own identity>
// behaves identically to no host param at all — processed locally, never
// forwarded.
func TestServicesHostParamOwnIdentity(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	t.Setenv("PROXY_URL", noopProxyStub(t))

	calls := &svcCallTracker{}
	localDC := servicesDockerStub(t, calls, twoReplicaContainers("running", false))

	// A peer IS configured, but must never be contacted — ?host= names this
	// host's own registry identity ("dashboard-a"), not the peer's.
	var peerHit atomic.Bool
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { peerHit.Store(true) }))
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	mux := newLocalTestMux(t, localDC, reg)

	rec := doReq(mux, http.MethodPost, "/api/services/app/stop?host=dashboard-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(calls.all()) == 0 {
		t.Error("local docker stub was never hit — ?host=<own identity> should process locally")
	}
	if peerHit.Load() {
		t.Error("peer was contacted for ?host=<own identity> — must be treated identically to no host param")
	}
}

// TestServicesMutationForwardsActorAssertion proves a token-authenticated
// (not cookie-session) scale request reaches the peer carrying a verifiable
// X-Pmgr-Actor assertion naming the real user, end to end through
// forwardServiceMutation and peerMutate — the same actor-attribution bug
// class fixed once already for images (session-only actor lookup silently
// producing no attribution for token callers) must be re-proven here, not
// assumed inherited.
func TestServicesMutationForwardsActorAssertion(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withActorSecret(t, testActorSecret)
	// The header-arrived check below can't catch a double-wrapped fallback
	// (audit() already resolves auditUser internally — see peers.go; passing
	// auditUser(r, "peer-mesh") into audit() a second time would record
	// "alice (via alice (via peer-mesh))" instead of "alice (via peer-mesh)").
	// Pin what the peer actually writes, not just what it received.
	readAudit := withAuditFile(t)

	calls := &svcCallTracker{}
	peerDC := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
	peerOnb := newTestOnboardedStore(t)
	inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, "", noopProxyStub(t), true)

	var headerMu sync.Mutex
	var gotHeader string
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerMu.Lock()
		gotHeader = r.Header.Get(actorHeader)
		headerMu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(peerSrv.Close)

	reg := newTestPeerRegistry(peerSrv.URL, true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	localOnb := newTestOnboardedStore(t)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	raw, _, err := auth.CreateToken("alice", "ci")
	if err != nil {
		t.Fatal(err)
	}

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), newImageChecker(localDC), "", nil, localOnb, nil, nil, nil, nil, nil, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/scale?host=dashboard-b", strings.NewReader(`{"replicas":1}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	headerMu.Lock()
	header := gotHeader
	headerMu.Unlock()
	if header == "" {
		t.Fatal("peer never received an X-Pmgr-Actor header for a token-authenticated request")
	}
	claims, ok := sso.VerifyActor(header, testActorSecret)
	if !ok || claims.Username != "alice" {
		t.Fatalf("VerifyActor(header) = %+v, ok=%v, want Username=alice", claims, ok)
	}

	entries := readAudit()
	if len(entries) != 1 {
		t.Fatalf("wrote %d audit entries, want 1", len(entries))
	}
	if entries[0]["user"] != "alice (via peer-mesh)" {
		t.Fatalf("peer audit user = %v, want %q (a double-wrapped fallback would produce \"alice (via alice (via peer-mesh))\")",
			entries[0]["user"], "alice (via peer-mesh)")
	}
}
