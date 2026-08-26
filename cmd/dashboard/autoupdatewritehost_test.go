package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

// autoUpdateDockerStub extends servicesDockerStub's containers/stop/start/
// remove behavior with WORKING /images/{name}/json and
// /distribution/{name}/json responses, so ic.Check(...) actually succeeds
// with real digests. servicesDockerStub's default branch answers every
// unmatched path (including those two) with the container list — decoding
// that into imageStatus's shapes either silently no-ops (RepoDigests) or
// fails loudly (Descriptor.Digest), so every existing services test
// effectively never exercises the image-checker's happy path. This stub
// records every hit (including plain container-list GETs) in calls, unlike
// servicesDockerStub which only records stop/start/remove.
func autoUpdateDockerStub(t *testing.T, calls *svcCallTracker, containers []dockerContainer) *dockerClient {
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
		case strings.Contains(r.URL.Path, "/images/"):
			calls.record("images " + r.URL.Path)
			json.NewEncoder(w).Encode(map[string]any{"RepoDigests": []string{"ghcr.io/org/app@sha256:local"}})
		case strings.Contains(r.URL.Path, "/distribution/"):
			calls.record("distribution " + r.URL.Path)
			json.NewEncoder(w).Encode(map[string]any{"Descriptor": map[string]any{"digest": "sha256:registry"}})
		default:
			calls.record("list " + r.URL.Path)
			json.NewEncoder(w).Encode(containers)
		}
	}))
}

// dockerStubAlwaysErrors 500s every Docker call — used to make
// dc.serviceContainsSelfByName (and therefore runServiceAutoUpdateSet's own
// identity check) fail, proving the fail-CLOSED fix this phase exists for.
func dockerStubAlwaysErrors(t *testing.T) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "docker daemon unreachable", http.StatusInternalServerError)
	}))
}

// newLocalTestMuxWithOnboarded is newLocalTestMux's counterpart for
// autoupdate tests, which need to pre-populate the LOCAL onboarded store
// (runServiceAutoUpdateSet keys everything off onb.Get) — something
// newLocalTestMux can't support since it builds its own onboarded store
// internally with no way to reach in and Put a fixture first.
func newLocalTestMuxWithOnboarded(t *testing.T, localDC *dockerClient, onb *OnboardedStore, reg *PeerRegistry) http.Handler {
	t.Helper()
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)
	return newDashboardMux(localDC, nil, auth, newRateLimiter(), newImageChecker(localDC), "", nil, onb, nil, nil, nil, nil, nil, reg, nil, nil, nil)
}

// putOnboardedApp is the standard onboarded "app" fixture shared by most
// tests below — routed (Host set), matching twoReplicaContainers'/
// canaryContainers' image and host so check's live discovery and
// autoupdate's onboarded-store path agree on the same service.
func putOnboardedApp(t *testing.T, onb *OnboardedStore, autoUpdate bool) {
	t.Helper()
	if err := onb.Put(OnboardedService{Name: "app", Host: "app.example", Image: "ghcr.io/org/app:v1", Replicas: 2, AutoUpdate: autoUpdate}); err != nil {
		t.Fatal(err)
	}
}

// TestAutoUpdateMutationsForwardToPeer proves both new write-mesh
// endpoints — autoupdate (enable AND disable) and check — reach the peer's
// own docker client / onboarded store (never local) when called with
// ?host=<peer>.
func TestAutoUpdateMutationsForwardToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	t.Setenv("PROXY_URL", noopProxyStub(t))

	var localHit atomic.Bool
	newLocal := func(t *testing.T) *dockerClient {
		return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			localHit.Store(true)
			json.NewEncoder(w).Encode([]dockerContainer{})
		}))
	}
	newPeer := func(t *testing.T, autoUpdate bool) (*httptest.Server, *svcCallTracker, *OnboardedStore) {
		calls := &svcCallTracker{}
		peerDC := autoUpdateDockerStub(t, calls, twoReplicaContainers("running", false))
		peerOnb := newTestOnboardedStore(t)
		putOnboardedApp(t, peerOnb, autoUpdate)
		srv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil))
		t.Cleanup(srv.Close)
		return srv, calls, peerOnb
	}

	t.Run("autoupdate enable", func(t *testing.T) {
		peerSrv, calls, peerOnb := newPeer(t, false)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		req := httptest.NewRequest(http.MethodPost, "/api/services/app/autoupdate?host=dashboard-b", strings.NewReader(`{"enabled":true}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — autoupdate enable did not reach the peer")
		}
		if got, _ := peerOnb.Get("app"); !got.AutoUpdate {
			t.Error("peer's onboarded store was not flipped to AutoUpdate=true")
		}
	})
	t.Run("autoupdate disable", func(t *testing.T) {
		// Disabling never calls Docker (no self-check on de-escalation), so
		// the proof of "reached the peer" is the peer's own onboarded store
		// flipping — the local mux's onboarded store never has "app" at all.
		peerSrv, _, peerOnb := newPeer(t, true)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		req := httptest.NewRequest(http.MethodPost, "/api/services/app/autoupdate?host=dashboard-b", strings.NewReader(`{"enabled":false}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if got, _ := peerOnb.Get("app"); got.AutoUpdate {
			t.Error("peer's onboarded store was not flipped to AutoUpdate=false")
		}
	})
	t.Run("check", func(t *testing.T) {
		peerSrv, calls, _ := newPeer(t, false)
		reg := newTestPeerRegistry(peerSrv.URL, true)
		mux := newLocalTestMux(t, newLocal(t), reg)
		rec := doReq(mux, http.MethodPost, "/api/services/app/check?host=dashboard-b")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		if len(calls.all()) == 0 {
			t.Error("peer docker stub was never hit — check did not reach the peer")
		}
	})

	if localHit.Load() {
		t.Error("local docker stub was hit — ?host=<peer> requests must never touch the local daemon")
	}
}

// TestAutoUpdatePeerSelfGuardRejects proves the PEER's outer self-guard (the
// pre-existing block in peerServicesMutateHandler, run before any subpath
// dispatch) still rejects (403) an autoupdate enable targeting its own
// container — same shape as TestServicesPeerSelfGuardRejects for the
// original five endpoints.
func TestAutoUpdatePeerSelfGuardRejects(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	calls := &svcCallTracker{}
	peerDC := autoUpdateDockerStub(t, calls, []dockerContainer{{
		ID: "abc123def456", Names: []string{"/dashboard"}, State: "running",
		Labels: map[string]string{labelEnable: "true", labelService: "dashboard", labelHost: "dashboard.example", labelPort: "8093"},
	}})
	peerOnb := newTestOnboardedStore(t)
	if err := peerOnb.Put(OnboardedService{Name: "dashboard", Host: "dashboard.example", Image: "ghcr.io/org/dash:v1", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil)

	req := httptest.NewRequest(http.MethodPost, "/peer/services/dashboard/autoupdate", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	inner.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body %s, want 403", rec.Code, rec.Body.String())
	}
	if got, _ := peerOnb.Get("dashboard"); got.AutoUpdate {
		t.Error("AutoUpdate flipped true despite the self-guard rejection")
	}
}

// TestAutoUpdatePeerSelfGuardFailsClosedOnDockerError is THE test this phase
// exists to add: for an ONBOARDED (not label-managed) service, a Docker
// error verifying identity must be treated the same as "this might be self"
// — refused 403 — not "assume safe" the way the pre-fix code (a bare
// onb.SetAutoUpdate call with no independent live-state re-read) effectively
// did. dockerStubAlwaysErrors makes dc.serviceContainsSelfByName fail for
// BOTH the outer per-request self-guard (which fails OPEN on error, by
// design — see peers.go's doc comment) and runServiceAutoUpdateSet's own
// check — so a 403 here proves the NEW inner check is what's blocking this,
// not the outer guard (which would let it through).
func TestAutoUpdatePeerSelfGuardFailsClosedOnDockerError(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerDC := dockerStubAlwaysErrors(t)
	peerOnb := newTestOnboardedStore(t)
	putOnboardedApp(t, peerOnb, false)
	inner := peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil)

	req := httptest.NewRequest(http.MethodPost, "/peer/services/app/autoupdate", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	inner.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body %s, want 403 (fail CLOSED on a Docker error verifying identity)", rec.Code, rec.Body.String())
	}
	if got, _ := peerOnb.Get("app"); got.AutoUpdate {
		t.Error("AutoUpdate flipped true despite the identity check failing — fail-closed fix did not hold")
	}
}

// TestAutoUpdateManagedOnlyRejectedByPeer proves the PEER re-validates
// "managed-only" (Host == "") against its own onboarded store — a forwarded
// autoupdate enable on a routeless onboarded service is rejected 400,
// relayed verbatim via mapPeerMutationErr.
func TestAutoUpdateManagedOnlyRejectedByPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	calls := &svcCallTracker{}
	peerDC := autoUpdateDockerStub(t, calls, twoReplicaContainers("running", false))
	peerOnb := newTestOnboardedStore(t)
	if err := peerOnb.Put(OnboardedService{Name: "app", Host: "", Image: "ghcr.io/org/app:v1", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	peerSrv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), true, nil))
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/autoupdate?host=dashboard-b", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s, want 400 (peer must re-validate managed-only, relayed verbatim)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "managed-only") {
		t.Errorf("body = %q, want it to mention managed-only", rec.Body.String())
	}
}

// TestAutoUpdateDisableSkipsSelfGuard proves runServiceAutoUpdateSet's
// self-check only runs when enabled=true — disabling on a self-owned
// service succeeds (de-escalating, not blocked). This deliberately calls
// runServiceAutoUpdateSet directly rather than through HTTP: the OUTER
// per-request self-guard in both api.go and peers.go blocks EVERY mutating
// action (not just enabling autoupdate) against a self-owned service before
// any subpath dispatch runs, so an HTTP-level test could never isolate
// "does the disable path itself skip its own self-check" from "did the
// outer guard block this regardless" — both would 403 identically.
func TestAutoUpdateDisableSkipsSelfGuard(t *testing.T) {
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "abc123def456", Names: []string{"/dashboard"}, State: "running",
			Labels: map[string]string{labelEnable: "true", labelService: "dashboard", labelHost: "dashboard.example", labelPort: "8093"},
		}})
	}))
	onb := newTestOnboardedStore(t)
	if err := onb.Put(OnboardedService{Name: "dashboard", Host: "dashboard.example", Image: "ghcr.io/org/dash:v1", Replicas: 1, AutoUpdate: true}); err != nil {
		t.Fatal(err)
	}

	if err := runServiceAutoUpdateSet(context.Background(), dc, onb, "dashboard", false); err != nil {
		t.Fatalf("runServiceAutoUpdateSet(disable) on a self-owned service returned an error, want success (de-escalating skips the self-check): %v", err)
	}
	if got, _ := onb.Get("dashboard"); got.AutoUpdate {
		t.Error("AutoUpdate still true after disable")
	}
}

// TestAutoUpdateMutationPeerWritesDisabled proves a peer with
// -peer-writes=false answers 404 to both new /peer/services/* mutation
// endpoints.
func TestAutoUpdateMutationPeerWritesDisabled(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	calls := &svcCallTracker{}
	peerDC := autoUpdateDockerStub(t, calls, twoReplicaContainers("running", false))
	peerOnb := newTestOnboardedStore(t)
	putOnboardedApp(t, peerOnb, false)
	peerSrv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil, "", noopProxyStub(t), false, nil))
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, false)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	for _, tc := range []struct{ name, path, body string }{
		{"autoupdate", "/api/services/app/autoupdate?host=dashboard-b", `{"enabled":true}`},
		{"check", "/api/services/app/check?host=dashboard-b", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(http.MethodPost, tc.path, body)
			req.Header.Set("Authorization", "Bearer "+internalToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
			}
		})
	}
}

// TestAutoUpdateMutationUnknownHost proves a ?host= not matching any known
// peer identity 404s without attempting to reach anything.
func TestAutoUpdateMutationUnknownHost(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	reg := newTestPeerRegistry("http://peer-b:8098", true)
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/autoupdate?host=nonexistent-host", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

// TestAutoUpdateMutationNoRegistry proves a ?host= with a nil registry 404s.
func TestAutoUpdateMutationNoRegistry(t *testing.T) {
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, nil)

	rec := doReq(mux, http.MethodPost, "/api/services/app/check?host=anything")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

// TestAutoUpdateMutationPeerUnreachable proves an unreachable peer surfaces
// as 502, via peerMutate's transport-level error path through
// mapPeerMutationErr.
func TestAutoUpdateMutationPeerUnreachable(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := peerSrv.URL
	peerSrv.Close() // guarantees connection-refused without hardcoding a port

	reg := newTestPeerRegistry(url, true)
	mux := newLocalTestMux(t, localDC, reg)

	rec := doReq(mux, http.MethodPost, "/api/services/app/check?host=dashboard-b")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
}

// TestAutoUpdateMutationPeerAuthRejected proves a peer's own 401
// (mesh-secret mismatch) maps to 502, never relayed verbatim.
func TestAutoUpdateMutationPeerAuthRejected(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/autoupdate?host=dashboard-b", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
	if strings.Contains(rec.Body.String(), "peer's own auth error body") {
		t.Errorf("body = %q, must not contain the peer's own auth-failure body", rec.Body.String())
	}
}

// TestAutoUpdateMutationsLocalStillWork is the no-?host= regression test:
// both new actions must still work exactly as before the forwarding
// branches were added.
func TestAutoUpdateMutationsLocalStillWork(t *testing.T) {
	t.Run("autoupdate", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := autoUpdateDockerStub(t, calls, twoReplicaContainers("running", false))
		onb := newTestOnboardedStore(t)
		putOnboardedApp(t, onb, false)
		mux := newLocalTestMuxWithOnboarded(t, dc, onb, nil)

		req := httptest.NewRequest(http.MethodPost, "/api/services/app/autoupdate", strings.NewReader(`{"enabled":true}`))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("check", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := autoUpdateDockerStub(t, calls, twoReplicaContainers("running", false))
		onb := newTestOnboardedStore(t)
		mux := newLocalTestMuxWithOnboarded(t, dc, onb, nil)

		rec := doReq(mux, http.MethodPost, "/api/services/app/check")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
	})
}

// TestAutoUpdateHostParamOwnIdentity proves ?host=<this host's own identity>
// behaves identically to no host param at all — processed locally, never
// forwarded.
func TestAutoUpdateHostParamOwnIdentity(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	calls := &svcCallTracker{}
	localDC := autoUpdateDockerStub(t, calls, twoReplicaContainers("running", false))
	localOnb := newTestOnboardedStore(t)
	putOnboardedApp(t, localOnb, false)

	// A peer IS configured, but must never be contacted — ?host= names this
	// host's own registry identity ("dashboard-a"), not the peer's.
	var peerHit atomic.Bool
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { peerHit.Store(true) }))
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	mux := newLocalTestMuxWithOnboarded(t, localDC, localOnb, reg)

	rec := doReq(mux, http.MethodPost, "/api/services/app/check?host=dashboard-a")
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

// TestAutoUpdateMutationForwardsActorAssertion proves a token-authenticated
// (not cookie-session) request reaches the peer carrying a verifiable
// X-Pmgr-Actor assertion naming the real user, AND pins what the peer
// actually WRITES to its audit log — "alice (via peer-mesh)", not the
// double-wrapped "alice (via alice (via peer-mesh))" that would result from
// passing auditUser(r, "peer-mesh") into audit() a second time (audit()
// already resolves auditUser internally; that exact bug was found and fixed
// once already for the original five endpoints and must not be
// reintroduced here). Covers both autoupdate and check, each in its own
// subtest with its own audit file so the entry counts don't mix.
func TestAutoUpdateMutationForwardsActorAssertion(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withActorSecret(t, testActorSecret)

	run := func(t *testing.T, path, body, wantAction string) {
		readAudit := withAuditFile(t)

		calls := &svcCallTracker{}
		peerDC := autoUpdateDockerStub(t, calls, twoReplicaContainers("running", false))
		peerOnb := newTestOnboardedStore(t)
		putOnboardedApp(t, peerOnb, false)
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

		var reqBody *strings.Reader
		if body != "" {
			reqBody = strings.NewReader(body)
		} else {
			reqBody = strings.NewReader("")
		}
		req := httptest.NewRequest(http.MethodPost, path, reqBody)
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
		if entries[0]["action"] != wantAction {
			t.Fatalf("peer audit action = %v, want %q", entries[0]["action"], wantAction)
		}
	}

	t.Run("autoupdate", func(t *testing.T) {
		run(t, "/api/services/app/autoupdate?host=dashboard-b", `{"enabled":true}`, "service.autoupdate_set")
	})
	t.Run("check", func(t *testing.T) {
		run(t, "/api/services/app/check?host=dashboard-b", "", "service.check_image")
	})
}

// TestServiceCheckImageResponseShapes is a focused unit test on
// runServiceCheckImage directly (not HTTP), proving all of its response
// shapes are unchanged from the pre-extraction handler: not-found (404),
// findService error, no-canary success/failure (200 *imageStatus / 502
// string), and staged-canary success/failure (200 map[string]any / 502
// string).
func TestServiceCheckImageResponseShapes(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode([]dockerContainer{})
		}))
		ic := newImageChecker(dc)
		onb := newTestOnboardedStore(t)
		payload, status, err := runServiceCheckImage(context.Background(), dc, ic, onb, "nope")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", status)
		}
		if payload != nil {
			t.Fatalf("payload = %v, want nil", payload)
		}
	})

	t.Run("findService error", func(t *testing.T) {
		dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		ic := newImageChecker(dc)
		onb := newTestOnboardedStore(t)
		_, _, err := runServiceCheckImage(context.Background(), dc, ic, onb, "app")
		if err == nil {
			t.Fatal("err = nil, want the underlying findService/listServices error")
		}
	})

	t.Run("no canary, success", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := autoUpdateDockerStub(t, calls, twoReplicaContainers("running", false))
		ic := newImageChecker(dc)
		onb := newTestOnboardedStore(t)
		payload, status, err := runServiceCheckImage(context.Background(), dc, ic, onb, "app")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if status != http.StatusOK {
			t.Fatalf("status = %d, body %v, want 200", status, payload)
		}
		st, ok := payload.(*imageStatus)
		if !ok {
			t.Fatalf("payload = %#v (%T), want *imageStatus", payload, payload)
		}
		if st.LocalDigest != "sha256:local" || st.RegistryDigest != "sha256:registry" {
			t.Errorf("status = %+v, want digests populated", st)
		}
	})

	t.Run("no canary, check fails", func(t *testing.T) {
		calls := &svcCallTracker{}
		// Plain servicesDockerStub answers /images/ and /distribution/ with
		// the container list — decoding that into the distribution shape
		// fails loudly, giving live.Err != "" (see autoUpdateDockerStub's own
		// doc comment for why this is the DEFAULT for every other services
		// test, not a specially-rigged failure here).
		dc := servicesDockerStub(t, calls, twoReplicaContainers("running", false))
		ic := newImageChecker(dc)
		onb := newTestOnboardedStore(t)
		payload, status, err := runServiceCheckImage(context.Background(), dc, ic, onb, "app")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, body %v, want 502", status, payload)
		}
		if _, ok := payload.(string); !ok {
			t.Fatalf("payload = %#v (%T), want string error message", payload, payload)
		}
	})

	t.Run("staged canary, success", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := autoUpdateDockerStub(t, calls, canaryContainers())
		ic := newImageChecker(dc)
		onb := newTestOnboardedStore(t)
		payload, status, err := runServiceCheckImage(context.Background(), dc, ic, onb, "app")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if status != http.StatusOK {
			t.Fatalf("status = %d, body %v, want 200", status, payload)
		}
		m, ok := payload.(map[string]any)
		if !ok {
			t.Fatalf("payload = %#v (%T), want map[string]any{live,canary}", payload, payload)
		}
		if _, ok := m["live"]; !ok {
			t.Error(`payload missing "live"`)
		}
		if _, ok := m["canary"]; !ok {
			t.Error(`payload missing "canary"`)
		}
	})

	t.Run("staged canary, check fails", func(t *testing.T) {
		calls := &svcCallTracker{}
		dc := servicesDockerStub(t, calls, canaryContainers())
		ic := newImageChecker(dc)
		onb := newTestOnboardedStore(t)
		payload, status, err := runServiceCheckImage(context.Background(), dc, ic, onb, "app")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, body %v, want 502", status, payload)
		}
		msg, ok := payload.(string)
		if !ok {
			t.Fatalf("payload = %#v (%T), want string error message", payload, payload)
		}
		if !strings.Contains(msg, "live") || !strings.Contains(msg, "canary") {
			t.Errorf("msg = %q, want it to mention both live and canary failures", msg)
		}
	})
}
