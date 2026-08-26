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

// putOnboardedAppWithCanary is putOnboardedApp's counterpart for tests that
// exercise promote/discard on the ONBOARDED path — those both error
// immediately ("no canary to promote/discard for %q") unless CanaryImage is
// already set, which putOnboardedApp deliberately leaves empty.
func putOnboardedAppWithCanary(t *testing.T, onb *OnboardedStore) {
	t.Helper()
	if err := onb.Put(OnboardedService{
		Name: "app", Host: "app.example", Image: "ghcr.io/org/app:v1", Replicas: 1,
		OriginalRouted: true, CanaryImage: "ghcr.io/org/app:v2", CanaryReplicas: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// onboardedCanaryClones is the container fixture matching
// putOnboardedAppWithCanary: a single goproxy-onb-<name>-c1 container —
// promoteOnboarded/discardOnboarded locate it by that literal name prefix
// (the "goproxy-onb-<name>-" filter passed to listAll is not actually
// enforced server-side by replaceDockerStub below, so the fixture itself is
// what makes the prefix match hold).
func onboardedCanaryClones(name string) []dockerContainer {
	return []dockerContainer{
		{ID: "onb-c1", Names: []string{"/goproxy-onb-" + name + "-c1"}, State: "running", Image: "ghcr.io/org/app:v2"},
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
	srv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", dc, onb, newImageChecker(dc), nil, "", proxy, writesEnabled, nil))
	t.Cleanup(srv.Close)
	return srv, calls
}

// replaceDockerStub extends servicesDockerStub with everything replace/
// stage/promote/discard need beyond scale/stop/start: GET
// /containers/{id}/json (inspectEnv/inspectCloneSpec/inspectHostConfigUnknowns
// all hit this — answered with a body carrying zero HostConfig/Config fields
// so inspectHostConfigUnknowns never refuses), POST /containers/create
// (returns a fixed id — nothing here depends on distinct container ids), and
// POST /images/create (pullImage — always 200). Every hit is recorded in
// calls, matching autoUpdateDockerStub's convention (unlike
// servicesDockerStub, which only records stop/start/remove).
func replaceDockerStub(t *testing.T, calls *svcCallTracker, containers []dockerContainer) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			calls.record("list " + r.URL.Path)
			json.NewEncoder(w).Encode(containers)
		case strings.Contains(r.URL.Path, "/containers/create"):
			calls.record("create " + r.URL.Path)
			json.NewEncoder(w).Encode(map[string]any{"Id": "newid"})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/start"):
			calls.record("start " + r.URL.Path)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/stop"):
			calls.record("stop " + r.URL.Path)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/"):
			calls.record("remove " + r.URL.Path)
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/images/create"):
			calls.record("pull " + r.URL.Path)
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			calls.record("inspect " + r.URL.Path)
			w.Write([]byte(`{"Image":"sha256:abc","Config":{"Env":[]},"HostConfig":{"Mounts":[]},"NetworkSettings":{"Networks":{"edge":{}}}}`))
		default:
			calls.record("other " + r.URL.Path)
			w.Write([]byte("{}"))
		}
	}))
}

// newPeerServicesServerReplace is newPeerServicesServer's counterpart backed
// by replaceDockerStub — needed by every replace/stage/promote/discard test
// on the LABEL-MANAGED path, since servicesDockerStub's default branch
// answers GET /containers/{id}/json with the container list (an array),
// which inspectEnv/inspectCloneSpec/inspectHostConfigUnknowns can't decode.
func newPeerServicesServerReplace(t *testing.T, containers []dockerContainer, writesEnabled bool) (*httptest.Server, *svcCallTracker) {
	t.Helper()
	calls := &svcCallTracker{}
	dc := replaceDockerStub(t, calls, containers)
	onb := newTestOnboardedStore(t)
	proxy := noopProxyStub(t)
	srv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", dc, onb, newImageChecker(dc), nil, "", proxy, writesEnabled, nil))
	t.Cleanup(srv.Close)
	return srv, calls
}

// newPeerServicesServerOnboarded is newPeerServicesServerReplace's
// counterpart for the ONBOARDED path — replace/stage/promote/discard there
// go through rebuildOnboardedRoute, which unconditionally writes
// routesConfigPath, so (unlike every scale/stop/start test, which never
// touches it) "" would fail os.WriteFile(""). Needs a real temp file. Caller
// supplies the onboarded fixture (onb) since Put must happen before the
// server can see it.
func newPeerServicesServerOnboarded(t *testing.T, containers []dockerContainer, onb *OnboardedStore, writesEnabled bool) (*httptest.Server, *svcCallTracker) {
	t.Helper()
	calls := &svcCallTracker{}
	dc := replaceDockerStub(t, calls, containers)
	proxy := noopProxyStub(t)
	routesPath := filepath.Join(t.TempDir(), "routes.json")
	srv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", dc, onb, newImageChecker(dc), nil, routesPath, proxy, writesEnabled, nil))
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
	return newDashboardMux(localDC, nil, auth, newRateLimiter(), newImageChecker(localDC), "", nil, localOnb, nil, nil, nil, nil, nil, reg, nil, nil, nil)
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
	inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil)

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
	peerSrv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", peerProxy.URL, true, nil))
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
	peerSrv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil))
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
	inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil)

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

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), newImageChecker(localDC), "", nil, localOnb, nil, nil, nil, nil, nil, reg, nil, nil, nil)

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

// ---------------------------------------------------------------------
// Write-Mesh Phase 4: replace / stage / promote / discard(canary)
// ---------------------------------------------------------------------

// TestServicesLifecycleMutationsForwardToPeer proves all four Phase 4
// endpoints — replace, stage, promote, discard(canary) — reach the peer's
// own docker client (never local) when called with ?host=<peer>, on BOTH the
// label-managed path (docker.go's replaceService/stageCanary/promoteCanary/
// discardCanary) and the onboarded path (onboarded.go's counterparts), since
// peerServicesMutateHandler's onboarded-vs-label-managed dispatch is exactly
// the thing this phase adds.
func TestServicesLifecycleMutationsForwardToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	t.Setenv("PROXY_URL", noopProxyStub(t))

	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	var localHit atomic.Bool
	newLocal := func(t *testing.T) *dockerClient {
		return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			localHit.Store(true)
			json.NewEncoder(w).Encode([]dockerContainer{})
		}))
	}

	t.Run("label-managed replace", func(t *testing.T) {
		peerSrv, calls := newPeerServicesServerReplace(t, twoReplicaContainers("running", false), true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		req := httptest.NewRequest(http.MethodPost, "/api/services/app/replace?host=dashboard-b", strings.NewReader(`{"image":"ghcr.io/org/app:v2"}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — replace did not reach the peer")
		}
	})
	t.Run("label-managed stage", func(t *testing.T) {
		peerSrv, calls := newPeerServicesServerReplace(t, twoReplicaContainers("running", false), true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		req := httptest.NewRequest(http.MethodPost, "/api/services/app/stage?host=dashboard-b", strings.NewReader(`{"image":"ghcr.io/org/app:v2"}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — stage did not reach the peer")
		}
	})
	t.Run("label-managed promote", func(t *testing.T) {
		peerSrv, calls := newPeerServicesServerReplace(t, canaryContainers(), true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodPost, "/api/services/app/promote?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — promote did not reach the peer")
		}
	})
	t.Run("label-managed discard", func(t *testing.T) {
		peerSrv, calls := newPeerServicesServerReplace(t, canaryContainers(), true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodDelete, "/api/services/app/canary?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — discard did not reach the peer")
		}
	})

	t.Run("onboarded replace", func(t *testing.T) {
		onb := newTestOnboardedStore(t)
		putOnboardedApp(t, onb, false)
		peerSrv, calls := newPeerServicesServerOnboarded(t, []dockerContainer{}, onb, true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		req := httptest.NewRequest(http.MethodPost, "/api/services/app/replace?host=dashboard-b", strings.NewReader(`{"image":"ghcr.io/org/app:v2"}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — onboarded replace did not reach the peer")
		}
	})
	t.Run("onboarded stage", func(t *testing.T) {
		onb := newTestOnboardedStore(t)
		putOnboardedApp(t, onb, false)
		peerSrv, calls := newPeerServicesServerOnboarded(t, []dockerContainer{}, onb, true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		req := httptest.NewRequest(http.MethodPost, "/api/services/app/stage?host=dashboard-b", strings.NewReader(`{"image":"ghcr.io/org/app:v2"}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — onboarded stage did not reach the peer")
		}
	})
	t.Run("onboarded promote", func(t *testing.T) {
		onb := newTestOnboardedStore(t)
		putOnboardedAppWithCanary(t, onb)
		peerSrv, calls := newPeerServicesServerOnboarded(t, onboardedCanaryClones("app"), onb, true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodPost, "/api/services/app/promote?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — onboarded promote did not reach the peer")
		}
	})
	t.Run("onboarded discard", func(t *testing.T) {
		onb := newTestOnboardedStore(t)
		putOnboardedAppWithCanary(t, onb)
		peerSrv, calls := newPeerServicesServerOnboarded(t, onboardedCanaryClones("app"), onb, true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodDelete, "/api/services/app/canary?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — onboarded discard did not reach the peer")
		}
	})

	if localHit.Load() {
		t.Error("local docker stub was hit — ?host=<peer> requests must never touch the local daemon")
	}
}

// mutationCalls filters a svcCallTracker's hits down to actual mutations
// (create/start/stop/remove/pull) — needed because replaceDockerStub
// (unlike servicesDockerStub) also records plain reads (list/inspect), and
// the outer self-guard's own serviceContainsSelfByName check legitimately
// issues a "list" read before it can reject — that read is not the bug a
// self-guard-rejection test is trying to catch.
func mutationCalls(all []string) []string {
	var out []string
	for _, c := range all {
		if strings.HasPrefix(c, "list ") || strings.HasPrefix(c, "inspect ") {
			continue
		}
		out = append(out, c)
	}
	return out
}

// TestServicesLifecycleReplacePeerSelfGuardRejects proves the PEER's own
// self-guard rejects (403) a replace targeting its own container, verified
// both directly against peerServicesMutateHandler and through the full
// ?host= forwarding path (502, per mapPeerMutationErr's never-relay-a-peer-
// 401/403-verbatim rule) — same two-pronged shape as
// TestServicesPeerSelfGuardRejects, for one representative Phase 4 action.
func TestServicesLifecycleReplacePeerSelfGuardRejects(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	calls := &svcCallTracker{}
	peerDC := replaceDockerStub(t, calls, []dockerContainer{{
		ID: "abc123def456", Names: []string{"/dashboard"}, State: "running",
		Labels: map[string]string{labelEnable: "true", labelService: "dashboard", labelHost: "dashboard.example", labelPort: "8093"},
	}})
	peerOnb := newTestOnboardedStore(t)
	inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil)

	directReq := httptest.NewRequest(http.MethodPost, "/peer/services/dashboard/replace", strings.NewReader(`{"image":"ghcr.io/org/dash:v2"}`))
	directReq.Header.Set("Authorization", "Bearer s3cret")
	directRec := httptest.NewRecorder()
	inner.ServeHTTP(directRec, directReq)
	if directRec.Code != http.StatusForbidden {
		t.Fatalf("direct peer handler: status = %d, body %s, want 403", directRec.Code, directRec.Body.String())
	}
	if got := mutationCalls(calls.all()); len(got) != 0 {
		t.Errorf("peer docker stub saw a mutation call during a self-guard rejection: %v", got)
	}

	peerSrv := httptest.NewServer(inner)
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/services/dashboard/replace?host=dashboard-b", strings.NewReader(`{"image":"ghcr.io/org/dash:v2"}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("forwarded: status = %d, body %s, want 502 (mapPeerMutationErr never relays a peer 403 verbatim)", rec.Code, rec.Body.String())
	}
	if got := mutationCalls(calls.all()); len(got) != 0 {
		t.Errorf("peer docker stub saw a mutation call during a self-guard rejection: %v", got)
	}
}

// TestServicesReplaceEnvConflictRelayedByPeer is the concrete regression test
// proving the peer-side replace/stage branches use writeServiceErr (not
// plain http.Error): an unresolved env conflict must produce a 409 carrying
// {error, conflicts}, and that body must survive mapPeerMutationErr's
// verbatim-relay-on-409 path back to the local caller unchanged.
func TestServicesReplaceEnvConflictRelayedByPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode(twoReplicaContainers("running", false))
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			w.Write([]byte(`{"Image":"sha256:abc","Config":{"Env":["FOO=current"]},"HostConfig":{"Mounts":[]},"NetworkSettings":{"Networks":{"edge":{}}}}`))
		default:
			w.Write([]byte("{}"))
		}
	}))
	peerOnb := newTestOnboardedStore(t)
	peerSrv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil))
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/replace?host=dashboard-b", strings.NewReader(`{"image":"ghcr.io/org/app:v2","env":{"FOO":"newvalue"}}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s, want 409 (env conflict relayed verbatim via writeServiceErr + mapPeerMutationErr)", rec.Code, rec.Body.String())
	}
	var body struct {
		Error     string        `json:"error"`
		Conflicts []EnvConflict `json:"conflicts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v, body=%s", err, rec.Body.String())
	}
	if len(body.Conflicts) != 1 || body.Conflicts[0].Key != "FOO" {
		t.Fatalf("conflicts = %+v, want exactly one conflict on key FOO", body.Conflicts)
	}
}

// TestServicesStageAlreadyStagedRejectedByPeer proves the PEER re-validates
// "already has a canary" against its own live state for stage — distinct
// from Phase 2's per-replica-stop canary 409 (a different code path: that one
// is stageCanary's own guard, not the replicas/{member}/stop guard).
func TestServicesStageAlreadyStagedRejectedByPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerSrv, _ := newPeerServicesServerReplace(t, canaryContainers(), true)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/stage?host=dashboard-b", strings.NewReader(`{"image":"ghcr.io/org/app:v3"}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s, want 400 (peer must re-validate 'already has a canary', relayed verbatim)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "canary") {
		t.Errorf("body = %q, want it to mention the existing canary", rec.Body.String())
	}
}

// TestServicesLifecycleMutationPeerWritesDisabled proves a peer with
// -peer-writes=false answers 404 to every one of the four Phase 4 mutation
// endpoints.
func TestServicesLifecycleMutationPeerWritesDisabled(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerSrv, _ := newPeerServicesServerReplace(t, twoReplicaContainers("running", false), false)
	reg := newTestPeerRegistry(peerSrv.URL, false)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	for _, tc := range []struct{ name, method, path, body string }{
		{"replace", http.MethodPost, "/api/services/app/replace?host=dashboard-b", `{"image":"x"}`},
		{"stage", http.MethodPost, "/api/services/app/stage?host=dashboard-b", `{"image":"x"}`},
		{"promote", http.MethodPost, "/api/services/app/promote?host=dashboard-b", ""},
		{"discard", http.MethodDelete, "/api/services/app/canary?host=dashboard-b", ""},
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

// TestServicesLifecycleMutationPeerReachabilityErrors covers unknown-host,
// nil-registry, unreachable-peer, and peer-401 for a single representative
// Phase 4 action (replace) — established Phase 2/3 precedent that these
// generic peerMutate/forwarding failure paths don't need repeating per
// endpoint.
func TestServicesLifecycleMutationPeerReachabilityErrors(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	t.Run("unknown host", func(t *testing.T) {
		localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode([]dockerContainer{})
		}))
		reg := newTestPeerRegistry("http://peer-b:8098", true)
		mux := newLocalTestMux(t, localDC, reg)

		rec := doReq(mux, http.MethodPost, "/api/services/app/replace?host=nonexistent-host")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
		}
	})
	t.Run("nil registry", func(t *testing.T) {
		localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode([]dockerContainer{})
		}))
		mux := newLocalTestMux(t, localDC, nil)

		rec := doReq(mux, http.MethodPost, "/api/services/app/replace?host=anything")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
		}
	})
	t.Run("unreachable peer", func(t *testing.T) {
		localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode([]dockerContainer{})
		}))
		peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := peerSrv.URL
		peerSrv.Close() // guarantees connection-refused without hardcoding a port
		reg := newTestPeerRegistry(url, true)
		mux := newLocalTestMux(t, localDC, reg)

		rec := doReq(mux, http.MethodPost, "/api/services/app/replace?host=dashboard-b")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
		}
	})
	t.Run("peer 401", func(t *testing.T) {
		localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode([]dockerContainer{})
		}))
		peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "peer's own auth error body", http.StatusUnauthorized)
		}))
		t.Cleanup(peerSrv.Close)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, localDC, reg)

		rec := doReq(mux, http.MethodPost, "/api/services/app/replace?host=dashboard-b")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
		}
		if strings.Contains(rec.Body.String(), "peer's own auth error body") {
			t.Errorf("body = %q, must not contain the peer's own auth-failure body", rec.Body.String())
		}
	})
}

// TestServicesLifecycleMutationsLocalStillWork is the no-?host= regression
// test: all four Phase 4 actions must still work exactly as before the
// forwarding branch was added.
func TestServicesLifecycleMutationsLocalStillWork(t *testing.T) {
	t.Setenv("PROXY_URL", noopProxyStub(t))
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	t.Run("replace", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := replaceDockerStub(t, calls, twoReplicaContainers("running", false))
		mux := newLocalTestMux(t, dc, nil)
		req := httptest.NewRequest(http.MethodPost, "/api/services/app/replace", strings.NewReader(`{"image":"ghcr.io/org/app:v2"}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("stage", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := replaceDockerStub(t, calls, twoReplicaContainers("running", false))
		mux := newLocalTestMux(t, dc, nil)
		req := httptest.NewRequest(http.MethodPost, "/api/services/app/stage", strings.NewReader(`{"image":"ghcr.io/org/app:v2"}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("promote", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := replaceDockerStub(t, calls, canaryContainers())
		mux := newLocalTestMux(t, dc, nil)
		rec := doReq(mux, http.MethodPost, "/api/services/app/promote")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("discard", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := replaceDockerStub(t, calls, canaryContainers())
		mux := newLocalTestMux(t, dc, nil)
		rec := doReq(mux, http.MethodDelete, "/api/services/app/canary")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
}

// TestServicesLifecycleReplaceHostParamOwnIdentity proves
// ?host=<this host's own identity> behaves identically to no host param at
// all — processed locally, never forwarded — for one representative Phase 4
// action (replace).
func TestServicesLifecycleReplaceHostParamOwnIdentity(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	t.Setenv("PROXY_URL", noopProxyStub(t))
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	calls := &svcCallTracker{}
	localDC := replaceDockerStub(t, calls, twoReplicaContainers("running", false))

	var peerHit atomic.Bool
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { peerHit.Store(true) }))
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/replace?host=dashboard-a", strings.NewReader(`{"image":"ghcr.io/org/app:v2"}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
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

// TestServicesPromoteMutationForwardsActorAssertion is
// TestServicesMutationForwardsActorAssertion's Phase 4 counterpart, for one
// representative action (promote): a token-authenticated request must reach
// the peer carrying a verifiable X-Pmgr-Actor assertion, and the peer's own
// audit entry must read "alice (via peer-mesh)" — not the double-wrapped
// "alice (via alice (via peer-mesh))" that auditUser(r, "peer-mesh") would
// produce (the exact bug class this convention exists to catch).
func TestServicesPromoteMutationForwardsActorAssertion(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withActorSecret(t, testActorSecret)
	readAudit := withAuditFile(t)

	calls := &svcCallTracker{}
	peerDC := replaceDockerStub(t, calls, canaryContainers())
	peerOnb := newTestOnboardedStore(t)
	inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil)

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

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), newImageChecker(localDC), "", nil, localOnb, nil, nil, nil, nil, nil, reg, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/promote?host=dashboard-b", nil)
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

// ---------------------------------------------------------------------
// Write-Mesh Phase 5a: cross-host offboard + delete
// ---------------------------------------------------------------------

// dockerStubAlwaysErrorsTracked is dockerStubAlwaysErrors' (autoupdatewritehost_test.go)
// recording counterpart — every request returns 500, but each hit is
// recorded first, so a test can assert not just "the operation failed" but
// "not even a stop/remove/disconnect call was attempted" before it failed.
func dockerStubAlwaysErrorsTracked(t *testing.T, calls *svcCallTracker) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.record(r.Method + " " + r.URL.Path)
		http.Error(w, "docker daemon unreachable", http.StatusInternalServerError)
	}))
}

// noRemovalMutationCalls fails the test if any recorded call looks like an
// actual mutation (stop, disconnect-from-edge, or a DELETE against
// /containers/) — GET list/inspect reads (including the self-identity
// check's own lookup, which is expected to run and fail) are not a
// violation.
func noRemovalMutationCalls(t *testing.T, all []string) {
	t.Helper()
	for _, c := range all {
		if strings.Contains(c, "/stop") || strings.Contains(c, "/disconnect") ||
			(strings.HasPrefix(c, "DELETE ") && strings.Contains(c, "/containers/")) {
			t.Errorf("docker stub saw a mutation call before the fail-closed check blocked it: %v", all)
			return
		}
	}
}

// threeReplicaContainers is a three-member "app" fixture used by the
// partial-teardown test below — deleteService iterates in listAll's
// (unreordered) order, so c1/c2/c3 here map directly to attempt order.
func threeReplicaContainers() []dockerContainer {
	labels := map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"}
	return []dockerContainer{
		{ID: "c1", Names: []string{"/app"}, State: "running", Image: "ghcr.io/org/app:v1", Labels: labels},
		{ID: "c2", Names: []string{"/goproxy-app-2"}, State: "running", Image: "ghcr.io/org/app:v1", Labels: labels},
		{ID: "c3", Names: []string{"/goproxy-app-3"}, State: "running", Image: "ghcr.io/org/app:v1", Labels: labels},
	}
}

// partialFailDeleteDockerStub is servicesDockerStub's counterpart for
// TestServicesDeletePartialTeardownMembersActed: identical stop handling,
// but DELETE on the configured failID's container returns 500 — simulating
// deleteService failing partway through a multi-replica teardown, so
// membersActed can be pinned to exactly what succeeded before the failure.
func partialFailDeleteDockerStub(t *testing.T, calls *svcCallTracker, containers []dockerContainer, failID string) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/stop"):
			calls.record("stop " + r.URL.Path)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/"+failID):
			calls.record("remove-fail " + r.URL.Path)
			http.Error(w, "remove failed", http.StatusInternalServerError)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/"):
			calls.record("remove " + r.URL.Path)
			w.WriteHeader(http.StatusOK)
		default:
			json.NewEncoder(w).Encode(containers)
		}
	}))
}

// TestServicesOffboardDeleteMutationsForwardToPeer proves offboard and
// delete both reach the peer's own docker client / onboarded store (never
// local) when called with ?host=<peer>, on BOTH the label-managed and
// onboarded paths — four subtests covering the full matrix, since offboard
// and delete each dispatch differently depending on onb.Get(name).
func TestServicesOffboardDeleteMutationsForwardToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	t.Setenv("PROXY_URL", noopProxyStub(t))

	newLocal := func(t *testing.T) *dockerClient {
		return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("local docker stub was hit — ?host=<peer> requests must never touch the local daemon")
			json.NewEncoder(w).Encode([]dockerContainer{})
		}))
	}

	t.Run("offboard label-managed", func(t *testing.T) {
		peerSrv, calls := newPeerServicesServer(t, twoReplicaContainers("running", false), true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodPost, "/api/services/app/offboard?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["status"] != "offboarded" {
			t.Errorf("status = %v, want offboarded", body["status"])
		}
		_ = calls
	})

	t.Run("offboard onboarded", func(t *testing.T) {
		peerOnb := newTestOnboardedStore(t)
		putOnboardedApp(t, peerOnb, false)
		peerSrv, _ := newPeerServicesServerOnboarded(t, twoReplicaContainers("running", false), peerOnb, true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodPost, "/api/services/app/offboard?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["status"] != "offboarded" {
			t.Errorf("status = %v, want offboarded", body["status"])
		}
		if _, ok := peerOnb.Get("app"); ok {
			t.Error("peer's onboarded record for app still present after a successful offboard")
		}
	})

	t.Run("delete label-managed", func(t *testing.T) {
		peerSrv, _ := newPeerServicesServer(t, twoReplicaContainers("running", false), true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodDelete, "/api/services/app?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["status"] != "deleted" {
			t.Errorf("status = %v, want deleted", body["status"])
		}
		if body["members_acted"] != float64(2) {
			t.Errorf("members_acted = %v, want 2", body["members_acted"])
		}
	})

	t.Run("delete onboarded", func(t *testing.T) {
		peerOnb := newTestOnboardedStore(t)
		putOnboardedApp(t, peerOnb, false)
		peerSrv, _ := newPeerServicesServerOnboarded(t, twoReplicaContainers("running", false), peerOnb, true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodDelete, "/api/services/app?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		// DELETE on an onboarded service has always meant "offboard", not
		// "destroy the user's original container" — see offboardContainer's
		// doc comment.
		if body["status"] != "offboarded" {
			t.Errorf("status = %v, want offboarded (DELETE-on-onboarded delegates to offboard)", body["status"])
		}
		if _, ok := peerOnb.Get("app"); ok {
			t.Error("peer's onboarded record for app still present after a successful delete")
		}
	})
}

// TestServicesOffboardDeletePeerSelfGuardRejects proves the PEER's own
// (outer) self-guard rejects (403) a delete targeting its own container,
// verified both directly against peerServicesMutateHandler and through the
// full ?host= forwarding path (502, per mapPeerMutationErr's
// never-relay-a-peer-401/403-verbatim rule) — same two-pronged shape as
// TestServicesPeerSelfGuardRejects, for delete (representative; offboard
// shares the same outer per-request guard, checked before any subpath
// dispatch).
func TestServicesOffboardDeletePeerSelfGuardRejects(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	calls := &svcCallTracker{}
	peerDC := servicesDockerStub(t, calls, []dockerContainer{{
		ID: "abc123def456", Names: []string{"/dashboard"}, State: "running",
		Labels: map[string]string{labelEnable: "true", labelService: "dashboard", labelHost: "dashboard.example", labelPort: "8093"},
	}})
	peerOnb := newTestOnboardedStore(t)
	inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil)

	directReq := httptest.NewRequest(http.MethodDelete, "/peer/services/dashboard", strings.NewReader(`{"confirm":"dashboard"}`))
	directReq.Header.Set("Authorization", "Bearer s3cret")
	directRec := httptest.NewRecorder()
	inner.ServeHTTP(directRec, directReq)
	if directRec.Code != http.StatusForbidden {
		t.Fatalf("direct peer handler: status = %d, body %s, want 403", directRec.Code, directRec.Body.String())
	}
	if got := mutationCalls(calls.all()); len(got) != 0 {
		t.Errorf("peer docker stub saw a mutation call during a self-guard rejection: %v", got)
	}

	peerSrv := httptest.NewServer(inner)
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	rec := doReq(mux, http.MethodDelete, "/api/services/dashboard?host=dashboard-b")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("forwarded: status = %d, body %s, want 502 (mapPeerMutationErr never relays a peer 403 verbatim)", rec.Code, rec.Body.String())
	}
	if got := mutationCalls(calls.all()); len(got) != 0 {
		t.Errorf("peer docker stub saw a mutation call during a self-guard rejection: %v", got)
	}
}

// TestServicesOffboardDeletePeerInnerGuardFailsClosedOnDockerError is THE
// adversarial test this phase exists to add, covering both new fail-closed
// checks: for an ONBOARDED service, a Docker error verifying identity during
// offboard must be treated the same as "this might be self" (403), not
// "assume safe" — offboardContainer's final step (store.Remove) has no live
// re-read of its own, so the outer per-request guard's fail-OPEN behavior
// (see peers.go's doc comment) isn't enough. For a LABEL-MANAGED service,
// the same must hold for delete, even though dc.deleteService re-reads live
// state itself — the consequence (irreversible container destruction) still
// warrants an independent fail-closed check. dockerStubAlwaysErrorsTracked
// makes dc.serviceContainsSelfByName fail for BOTH the outer per-request
// self-guard (which fails OPEN on error, by design) and the NEW inner checks
// in runServiceOffboard/runServiceDelete — so a 403 here, with zero recorded
// stop/remove/disconnect calls, proves the NEW inner check is what's
// blocking this, not the outer guard (which would let it through).
func TestServicesOffboardDeletePeerInnerGuardFailsClosedOnDockerError(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	t.Run("offboard onboarded", func(t *testing.T) {
		calls := &svcCallTracker{}
		peerDC := dockerStubAlwaysErrorsTracked(t, calls)
		peerOnb := newTestOnboardedStore(t)
		putOnboardedApp(t, peerOnb, false)
		inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil)

		req := httptest.NewRequest(http.MethodPost, "/peer/services/app/offboard", nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, body %s, want 403 (fail CLOSED on a Docker error verifying identity)", rec.Code, rec.Body.String())
		}
		if _, ok := peerOnb.Get("app"); !ok {
			t.Error("onboarded record for app vanished — store.Remove ran despite the identity check failing")
		}
		noRemovalMutationCalls(t, calls.all())
	})

	t.Run("delete label-managed", func(t *testing.T) {
		calls := &svcCallTracker{}
		peerDC := dockerStubAlwaysErrorsTracked(t, calls)
		peerOnb := newTestOnboardedStore(t) // "app" NOT onboarded — forces the label-managed path
		inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil)

		req := httptest.NewRequest(http.MethodDelete, "/peer/services/app", strings.NewReader(`{"confirm":"app"}`))
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, body %s, want 403 (fail CLOSED on a Docker error verifying identity)", rec.Code, rec.Body.String())
		}
		noRemovalMutationCalls(t, calls.all())
	})
}

// TestServicesDeletePeerConfirmValidation proves the PEER independently
// validates the {"confirm": name} field on DELETE before any Docker
// mutation — REQUIRED there (unlike the local handler, where it's optional):
// missing, empty, or mismatched confirm is rejected 400 regardless of
// whether the request arrived via forwardServiceMutation (which always
// sends a correct one, constructed server-side) or some other caller
// hitting the peer endpoint directly — bypassing forwarding entirely, as
// this test does, is exactly how a bug in forwardServiceMutation's own
// confirm construction would otherwise go undetected.
func TestServicesDeletePeerConfirmValidation(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	for _, tc := range []struct{ name, body string }{
		{"missing confirm", `{}`},
		{"empty confirm", `{"confirm":""}`},
		{"mismatched confirm", `{"confirm":"not-app"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := &svcCallTracker{}
			peerDC := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
			peerOnb := newTestOnboardedStore(t)
			inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil)

			req := httptest.NewRequest(http.MethodDelete, "/peer/services/app", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer s3cret")
			rec := httptest.NewRecorder()
			inner.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s, want 400", rec.Code, rec.Body.String())
			}
			if got := mutationCalls(calls.all()); len(got) != 0 {
				t.Errorf("peer docker stub saw a mutation call before confirm was validated: %v", got)
			}
		})
	}
}

// TestServicesDeleteLocalConfirmBehavior covers the local DELETE handler's
// confirm-body handling end to end: no body (backward compat — the
// pre-existing, pre-Phase-5a behavior, unchanged for existing direct-API/MCP
// callers), a correct confirm, an empty confirm (treated the same as no
// body), and a mismatched confirm (400, rejected before any Docker
// mutation) — across both the label-managed and onboarded dispatch paths.
func TestServicesDeleteLocalConfirmBehavior(t *testing.T) {
	t.Setenv("PROXY_URL", noopProxyStub(t))

	t.Run("label-managed no body", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
		mux := newLocalTestMux(t, dc, nil)
		rec := doReq(mux, http.MethodDelete, "/api/services/app")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s, want 200 (no body preserves pre-existing behavior)", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["status"] != "deleted" || body["members_acted"] != float64(2) {
			t.Errorf("body = %v, want status=deleted members_acted=2", body)
		}
	})
	t.Run("label-managed correct confirm", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
		mux := newLocalTestMux(t, dc, nil)
		req := httptest.NewRequest(http.MethodDelete, "/api/services/app", strings.NewReader(`{"confirm":"app"}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("label-managed empty confirm", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
		mux := newLocalTestMux(t, dc, nil)
		req := httptest.NewRequest(http.MethodDelete, "/api/services/app", strings.NewReader(`{"confirm":""}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s, want 200 (empty confirm preserves no-body behavior)", rec.Code, rec.Body.String())
		}
	})
	t.Run("label-managed mismatched confirm", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
		mux := newLocalTestMux(t, dc, nil)
		req := httptest.NewRequest(http.MethodDelete, "/api/services/app", strings.NewReader(`{"confirm":"not-app"}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body %s, want 400", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "confirmation does not match") {
			t.Errorf("body = %q, want it to mention the confirmation mismatch", rec.Body.String())
		}
		if len(calls.all()) != 0 {
			t.Errorf("docker stub saw a mutation call despite a mismatched confirm: %v", calls.all())
		}
	})
	t.Run("onboarded no body", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := replaceDockerStub(t, calls, twoReplicaContainers("running", false))
		onb := newTestOnboardedStore(t)
		putOnboardedApp(t, onb, false)
		routesPath := filepath.Join(t.TempDir(), "routes.json")
		auth, _ := newConfirmedStore(t, "alice", "correct horse")
		setInternalToken(t)
		mux := newDashboardMux(dc, nil, auth, newRateLimiter(), newImageChecker(dc), routesPath, nil, onb, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		rec := doReq(mux, http.MethodDelete, "/api/services/app")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["status"] != "offboarded" {
			t.Errorf("status = %v, want offboarded (DELETE on onboarded has always meant offboard)", body["status"])
		}
	})
}

// TestServicesOffboardDeleteMutationPeerWritesDisabled proves a peer with
// -peer-writes=false answers 404 to both new /peer/services/* mutation
// endpoints.
func TestServicesOffboardDeleteMutationPeerWritesDisabled(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerSrv, _ := newPeerServicesServer(t, twoReplicaContainers("running", false), false)
	reg := newTestPeerRegistry(peerSrv.URL, false)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	for _, tc := range []struct{ name, method, path string }{
		{"offboard", http.MethodPost, "/api/services/app/offboard?host=dashboard-b"},
		{"delete", http.MethodDelete, "/api/services/app?host=dashboard-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(mux, tc.method, tc.path)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
			}
		})
	}
}

// TestServicesDeleteMutationPeerReachabilityErrors covers unknown-host,
// nil-registry, unreachable-peer, and peer-401 for delete — one
// representative Phase 5a action (same shape as
// TestServicesMutationUnknownHost/NoRegistry/PeerUnreachable/PeerAuthRejected).
func TestServicesDeleteMutationPeerReachabilityErrors(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	t.Run("unknown host", func(t *testing.T) {
		reg := newTestPeerRegistry("http://peer-b:8098", true)
		mux := newLocalTestMux(t, localDC, reg)
		rec := doReq(mux, http.MethodDelete, "/api/services/app?host=nonexistent-host")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
		}
	})
	t.Run("nil registry", func(t *testing.T) {
		mux := newLocalTestMux(t, localDC, nil)
		rec := doReq(mux, http.MethodDelete, "/api/services/app?host=anything")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
		}
	})
	t.Run("peer unreachable", func(t *testing.T) {
		peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := peerSrv.URL
		peerSrv.Close() // guarantees connection-refused without hardcoding a port
		reg := newTestPeerRegistry(url, true)
		mux := newLocalTestMux(t, localDC, reg)
		rec := doReq(mux, http.MethodDelete, "/api/services/app?host=dashboard-b")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
		}
	})
	t.Run("peer auth rejected", func(t *testing.T) {
		peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "peer's own auth error body", http.StatusUnauthorized)
		}))
		t.Cleanup(peerSrv.Close)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, localDC, reg)
		rec := doReq(mux, http.MethodDelete, "/api/services/app?host=dashboard-b")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
		}
		if strings.Contains(rec.Body.String(), "peer's own auth error body") {
			t.Errorf("body = %q, must not contain the peer's own auth-failure body", rec.Body.String())
		}
	})
}

// TestServicesOffboardDeleteHostParamOwnIdentity proves
// ?host=<this host's own identity> behaves identically to no host param at
// all — processed locally, never forwarded — for delete (representative).
func TestServicesOffboardDeleteHostParamOwnIdentity(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	t.Setenv("PROXY_URL", noopProxyStub(t))

	calls := &svcCallTracker{}
	localDC := servicesDockerStub(t, calls, twoReplicaContainers("running", false))

	var peerHit atomic.Bool
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { peerHit.Store(true) }))
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	mux := newLocalTestMux(t, localDC, reg)

	rec := doReq(mux, http.MethodDelete, "/api/services/app?host=dashboard-a")
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

// TestServicesDeleteMutationForwardsActorAssertion is
// TestServicesMutationForwardsActorAssertion's Phase 5a counterpart, for
// delete: a token-authenticated request must reach the peer carrying a
// verifiable X-Pmgr-Actor assertion, and the peer's own audit entry must
// read "alice (via peer-mesh)" — not the double-wrapped
// "alice (via alice (via peer-mesh))" that auditUser(r, "peer-mesh") would
// produce.
func TestServicesDeleteMutationForwardsActorAssertion(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withActorSecret(t, testActorSecret)
	readAudit := withAuditFile(t)

	calls := &svcCallTracker{}
	peerDC := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
	peerOnb := newTestOnboardedStore(t)
	inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil)

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

	mux := newDashboardMux(localDC, nil, auth, newRateLimiter(), newImageChecker(localDC), "", nil, localOnb, nil, nil, nil, nil, nil, reg, nil, nil, nil)

	req := httptest.NewRequest(http.MethodDelete, "/api/services/app?host=dashboard-b", nil)
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

// TestServicesDeletePartialTeardownMembersActed proves a delete that fails
// partway through a multi-replica service reports how many members were
// actually torn down (members_acted) rather than a bare status string —
// both locally and through the full ?host= forwarding path, where the JSON
// body (including members_acted) must survive mapPeerMutationErr's
// verbatim-relay-on-400 path back to the local caller (deliberately routed
// through 400, not the pre-existing plain-500, specifically so this body
// survives the hop — see writeServiceDeleteErr's doc comment).
func TestServicesDeletePartialTeardownMembersActed(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	t.Run("local", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := partialFailDeleteDockerStub(t, calls, threeReplicaContainers(), "c2")
		mux := newLocalTestMux(t, dc, nil)
		rec := doReq(mux, http.MethodDelete, "/api/services/app")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body %s, want 400", rec.Code, rec.Body.String())
		}
		var body struct {
			Error        string `json:"error"`
			MembersActed int    `json:"members_acted"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v, body=%s", err, rec.Body.String())
		}
		if body.MembersActed != 1 {
			t.Fatalf("members_acted = %d, want 1 (only c1 removed before c2 failed)", body.MembersActed)
		}
	})

	t.Run("forwarded", func(t *testing.T) {
		calls := &svcCallTracker{}
		peerDC := partialFailDeleteDockerStub(t, calls, threeReplicaContainers(), "c2")
		peerOnb := newTestOnboardedStore(t)
		peerSrv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil))
		t.Cleanup(peerSrv.Close)
		reg := newTestPeerRegistry(peerSrv.URL, true)

		localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode([]dockerContainer{})
		}))
		mux := newLocalTestMux(t, localDC, reg)

		rec := doReq(mux, http.MethodDelete, "/api/services/app?host=dashboard-b")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body %s, want 400 (relayed verbatim by mapPeerMutationErr)", rec.Code, rec.Body.String())
		}
		var body struct {
			Error        string `json:"error"`
			MembersActed int    `json:"members_acted"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v, body=%s", err, rec.Body.String())
		}
		if body.MembersActed != 1 {
			t.Fatalf("forwarded members_acted = %d, want 1 — the peer's partial-teardown count did not survive the hop", body.MembersActed)
		}
	})
}

// TestServicesDuplicateForwardsToOwningPeer proves "Duplicate to host…" on a
// FOREIGN (peer-owned) service works end to end across two hops: the local
// dashboard forwards the browser's POST straight through to the owning
// peer's own /peer/services/{name}/duplicate (never touching the local
// daemon), and that peer's own runServiceDuplicate call reaches a THIRD
// host's /peer/duplicate exactly as it would for a locally-owned service.
// This is the gap dupAttr's unconditional foreign-row lock used to hide:
// duplicate was never wired into forwardServiceMutation/
// peerServicesMutateHandler at all.
func TestServicesDuplicateForwardsToOwningPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	// The ultimate destination ("dashboard-c"): a real /peer/duplicate
	// handler over an empty container list, recording the create call.
	var finalCreate atomic.Bool
	finalDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{})
		case strings.Contains(r.URL.Path, "/containers/create"):
			finalCreate.Store(true)
			json.NewEncoder(w).Encode(map[string]string{"Id": "newid"})
		default:
			w.Write([]byte("{}"))
		}
	}))
	finalSrv := httptest.NewServer(peerDuplicateHandler("s3cret", "dashboard-c", finalDC, true))
	t.Cleanup(finalSrv.Close)

	// The owning peer ("dashboard-b"): has "app" as a real label-managed
	// service, and its own registry resolves "dashboard-c" to finalSrv —
	// exactly as runServiceDuplicate expects for the local-owner case.
	calls := &svcCallTracker{}
	ownerDC := replaceDockerStub(t, calls, []dockerContainer{{
		ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running", Image: "ghcr.io/org/app:v1",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080"},
	}})
	ownerOnb := newTestOnboardedStore(t)
	ownerReg := newPeerRegistry([]string{finalSrv.URL}, "s3cret", "dashboard-b", "dev", 0, nil)
	ownerReg.recordResult(finalSrv.URL, true, "dashboard-c", "dev", true)
	ownerRoutesPath := filepath.Join(t.TempDir(), "routes.json")
	ownerSrv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", ownerDC, ownerOnb, newImageChecker(ownerDC), ownerReg, ownerRoutesPath, noopProxyStub(t), true, nil))
	t.Cleanup(ownerSrv.Close)

	// The local dashboard: registry resolves "dashboard-b" to ownerSrv. Its
	// own docker stub must never be hit — the service isn't local here.
	var localHit atomic.Bool
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit.Store(true)
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	reg := newTestPeerRegistry(ownerSrv.URL, true)
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/duplicate?host=dashboard-b",
		strings.NewReader(`{"target":"dashboard-c","publish_port":18080}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if localHit.Load() {
		t.Error("local docker stub was hit — a foreign service's duplicate must never touch the local daemon")
	}
	if len(calls.all()) == 0 {
		t.Error("owning peer's docker stub was never hit — duplicate did not run against the real owner")
	}
	if !finalCreate.Load() {
		t.Error("final target never saw /containers/create — the second hop (owner -> ultimate target) never happened")
	}
}

// TestServicesDuplicateRefusesSingleton proves runServiceDuplicate refuses a
// service labeled proxy.unscalable=true before ever contacting a peer —
// covering both the local (api.go) and peer-mesh (peers.go) call paths,
// which both funnel through this one guard. This is the fix for the
// incident where duplicating a singleton service created a second,
// independent container plus a competing routes.json entry for the same
// host+path.
func TestServicesDuplicateRefusesSingleton(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	var peerHit atomic.Bool
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerHit.Store(true)
		json.NewEncoder(w).Encode(peerDuplicateResponse{Status: "ok"})
	}))
	t.Cleanup(peerSrv.Close)

	calls := &svcCallTracker{}
	dc := replaceDockerStub(t, calls, []dockerContainer{{
		ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running", Image: "ghcr.io/org/app:v1",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080", labelUnscalable: "true"},
	}})
	onb := newTestOnboardedStore(t)
	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", true)
	routesPath := filepath.Join(t.TempDir(), "routes.json")

	_, err := runServiceDuplicate(context.Background(), dc, reg, onb, routesPath, "app",
		DuplicateServiceRequest{Target: "dashboard-b", PublishPort: 18080}, "")
	if err == nil {
		t.Fatal("expected an error duplicating a singleton service, got nil")
	}
	if !strings.Contains(err.Error(), "singleton") {
		t.Errorf("error = %q, want it to mention singleton", err.Error())
	}
	if peerHit.Load() {
		t.Error("peer was contacted — singleton refusal must happen before any peer call")
	}
}
