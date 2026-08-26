// Write-Mesh Phase 5b: POST /api/discovery/{name}/onboard forwarding to a
// peer via ?host=<identity>. Mirrors serviceswritehost_test.go and
// autoupdatewritehost_test.go's conventions, adapted for onboard's separate
// mux prefix (/peer/discovery/) and its identity-based (not label-based)
// self-guard (checkOnboardTarget, inside onboardContainer itself — see
// peers.go's peerDiscoveryMutateHandler doc comment).

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PolarBaeJr/proxy-manager/internal/sso"
)

// onboardMutationTracker records every create/start/stop/remove call an
// onboard test's fake docker daemon saw — the mutation-only subset relevant
// to "did onboarding actually happen", mirroring svcCallTracker plus
// mutationCalls' filtering (no separate filter step needed here since
// newOnboardFakeDockerServer's own request switch already only reaches these
// branches for genuine mutations).
type onboardMutationTracker struct {
	mu   sync.Mutex
	hits []string
}

func (c *onboardMutationTracker) record(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hits = append(c.hits, s)
}

func (c *onboardMutationTracker) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.hits...)
}

// newTrackedOnboardFakeDockerServer wraps newOnboardFakeDockerServer's fake
// container with an outer handler that records every create/start/stop/
// DELETE hit before delegating to the real dockerClient's transport — needed
// because newOnboardFakeDockerServer itself doesn't expose a call tracker.
func newTrackedOnboardFakeDockerServer(t *testing.T, seed *onboardFakeContainer, calls *onboardMutationTracker) *dockerClient {
	t.Helper()
	inner := newOnboardFakeDockerServer(t, seed)
	orig := inner.http.Transport
	inner.http.Transport = trackingRoundTripper{orig: orig, calls: calls}
	return inner
}

// trackingRoundTripper records create/start/stop/DELETE requests before
// delegating to orig — a thin http.RoundTripper wrapper since
// newOnboardFakeDockerServer's dockerClient talks to its stub over a real
// net.Conn dialer, not a handler we can intercept directly.
type trackingRoundTripper struct {
	orig  http.RoundTripper
	calls *onboardMutationTracker
}

func (t trackingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create"):
		t.calls.record("create " + r.URL.Path)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
		t.calls.record("start " + r.URL.Path)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
		t.calls.record("stop " + r.URL.Path)
	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/"):
		t.calls.record("remove " + r.URL.Path)
	}
	return t.orig.RoundTrip(r)
}

// newPeerDiscoveryServer stands up a peer's own /peer/discovery/ write
// handler on an httptest.Server, backed by a fresh onboard fake docker
// daemon, and returns the server plus the call tracker.
func newPeerDiscoveryServer(t *testing.T, seed *onboardFakeContainer, proxyURL string, writesEnabled bool) (*httptest.Server, *onboardMutationTracker) {
	t.Helper()
	calls := &onboardMutationTracker{}
	dc := newTrackedOnboardFakeDockerServer(t, seed, calls)
	srv := httptest.NewServer(peerDiscoveryMutateHandler("s3cret", "dashboard-b", dc, proxyURL, writesEnabled))
	t.Cleanup(srv.Close)
	return srv, calls
}

// TestOnboardForwardsToPeer proves ?host=<peer> reaches the peer's OWN
// docker client (never local), onboarding a legitimate non-self target
// there, with the same response shape as the local handler.
func TestOnboardForwardsToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })
	withSelfHostname(t, func() (string, error) { return "not-orig-id", nil })

	peerSrv, calls := newPeerDiscoveryServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1",
		env: []string{"FOO=1"}, state: "running", labels: map[string]string{},
	}, noopProxyStub(t), true)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	var localHit atomic.Bool
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit.Store(true)
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/myapp/onboard?host=dashboard-b",
		strings.NewReader(`{"host":"myapp.example.org","port":8080,"replicas":2}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "onboarded" || resp.Name != "myapp" {
		t.Fatalf("response = %+v, want status=onboarded name=myapp", resp)
	}
	if len(calls.all()) == 0 {
		t.Error("peer docker stub was never hit — onboard did not reach the peer")
	}
	if localHit.Load() {
		t.Error("local docker stub was hit — ?host=<peer> requests must never touch the local daemon")
	}
}

// TestOnboardPeerSelfGuardRejectsInfraName proves the PEER's own
// checkOnboardTarget refuses an infra-named target (e.g. "proxy") against
// the PEER's own state, both directly against peerDiscoveryMutateHandler and
// through the full ?host= forwarding path (502, per mapPeerMutationErr's
// never-relay-a-peer-403-verbatim rule).
func TestOnboardPeerSelfGuardRejectsInfraName(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	if !infraContainerNames["proxy"] {
		t.Fatal("test assumes \"proxy\" is in infraContainerNames")
	}
	withSelfHostname(t, func() (string, error) { return "totally-unrelated-hostname", nil })

	calls := &onboardMutationTracker{}
	dc := newTrackedOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "proxy-id", name: "proxy", image: "ghcr.io/org/proxy:v1", state: "running", labels: map[string]string{},
	}, calls)
	inner := peerDiscoveryMutateHandler("s3cret", "dashboard-b", dc, noopProxyStub(t), true)

	directReq := httptest.NewRequest(http.MethodPost, "/peer/discovery/proxy/onboard",
		strings.NewReader(`{"host":"proxy.example.org","port":8080}`))
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

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/proxy/onboard?host=dashboard-b",
		strings.NewReader(`{"host":"proxy.example.org","port":8080}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("forwarded: status = %d, body %s, want 502", rec.Code, rec.Body.String())
	}
	if len(calls.all()) != 0 {
		t.Errorf("peer docker stub saw a mutation call during a self-guard rejection: %v", calls.all())
	}
}

// TestOnboardPeerSelfGuardRejectsOwnIdentity proves the PEER's own
// checkOnboardTarget refuses a target that IS the PEER's own container by
// identity (withSelfHostname set for the PEER's own onboardContainer call —
// NOT the requester's view), both directly and via forwarding.
func TestOnboardPeerSelfGuardRejectsOwnIdentity(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })

	calls := &onboardMutationTracker{}
	dc := newTrackedOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "abc123def456", name: "dashboard-b-svc", image: "ghcr.io/org/dashboard:v1", state: "running", labels: map[string]string{},
	}, calls)
	inner := peerDiscoveryMutateHandler("s3cret", "dashboard-b", dc, noopProxyStub(t), true)

	directReq := httptest.NewRequest(http.MethodPost, "/peer/discovery/dashboard-b-svc/onboard",
		strings.NewReader(`{"host":"dashboard-b.example.org","port":8093}`))
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

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/dashboard-b-svc/onboard?host=dashboard-b",
		strings.NewReader(`{"host":"dashboard-b.example.org","port":8093}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("forwarded: status = %d, body %s, want 502", rec.Code, rec.Body.String())
	}
	if len(calls.all()) != 0 {
		t.Errorf("peer docker stub saw a mutation call during a self-guard rejection: %v", calls.all())
	}
}

// TestOnboardPeerFailsClosedOnHostnameLookupError proves a selfHostname()
// lookup error on the PEER during checkOnboardTarget refuses (403), not a
// silent onboard — zero mutation calls must reach the peer's docker stub.
func TestOnboardPeerFailsClosedOnHostnameLookupError(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withSelfHostname(t, func() (string, error) { return "", errors.New("boom: no /etc/hostname") })

	calls := &onboardMutationTracker{}
	dc := newTrackedOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1", env: []string{"FOO=1"}, state: "running", labels: map[string]string{},
	}, calls)
	inner := peerDiscoveryMutateHandler("s3cret", "dashboard-b", dc, noopProxyStub(t), true)

	directReq := httptest.NewRequest(http.MethodPost, "/peer/discovery/myapp/onboard",
		strings.NewReader(`{"host":"myapp.example.org","port":8080}`))
	directReq.Header.Set("Authorization", "Bearer s3cret")
	directRec := httptest.NewRecorder()
	inner.ServeHTTP(directRec, directReq)
	if directRec.Code != http.StatusForbidden {
		t.Fatalf("direct peer handler: status = %d, body %s, want 403 (fail closed on lookup error)", directRec.Code, directRec.Body.String())
	}
	if len(calls.all()) != 0 {
		t.Errorf("peer docker stub saw a mutation call despite a fail-closed identity-check error: %v", calls.all())
	}
}

// TestOnboardRejectedByPeerRelayedVerbatim proves the PEER's own definitive
// rejection (not a self-guard 403, but an ordinary 400 from
// inspectHostConfigUnknowns) is relayed verbatim via mapPeerMutationErr's
// 400/404/409 branch — demonstrating forwardOnboardMutation's doc-comment
// claim that the peer independently re-validates against its own live
// Docker state, not just something asserted in a comment. PortBindings is
// peer-local HostConfig the requester cannot see or influence from its own
// OnboardRequest body.
func TestOnboardRejectedByPeerRelayedVerbatim(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withSelfHostname(t, func() (string, error) { return "not-orig-id", nil })

	peerSrv, calls := newPeerDiscoveryServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1", state: "running", labels: map[string]string{},
		hostConfig: map[string]any{
			"NetworkMode":  "edge",
			"PortBindings": map[string]any{"80/tcp": []map[string]any{{"HostPort": "8080"}}},
		},
	}, noopProxyStub(t), true)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/myapp/onboard?host=dashboard-b",
		strings.NewReader(`{"host":"myapp.example.org","port":8080}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s, want 400 (peer must relay its own definitive rejection verbatim)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "PortBindings") {
		t.Errorf("body = %q, want it to mention PortBindings", rec.Body.String())
	}
	if len(calls.all()) != 0 {
		t.Errorf("peer docker stub saw a mutation call for a rejected onboard: %v", calls.all())
	}
}

// TestOnboardMutationPeerWritesDisabled proves a peer with -peer-writes=false
// answers 404 to /peer/discovery/{name}/onboard.
func TestOnboardMutationPeerWritesDisabled(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerSrv, _ := newPeerDiscoveryServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1", state: "running", labels: map[string]string{},
	}, noopProxyStub(t), false)
	reg := newTestPeerRegistry(peerSrv.URL, false)

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/myapp/onboard?host=dashboard-b",
		strings.NewReader(`{"host":"myapp.example.org","port":8080}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

// TestOnboardMutationUnknownHost proves a ?host= not matching any known peer
// identity 404s without attempting to reach anything.
func TestOnboardMutationUnknownHost(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	reg := newTestPeerRegistry("http://peer-b:8098", true)
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/myapp/onboard?host=nonexistent-host",
		strings.NewReader(`{"host":"myapp.example.org","port":8080}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

// TestOnboardMutationNoRegistry proves a ?host= with a nil registry 404s.
func TestOnboardMutationNoRegistry(t *testing.T) {
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	mux := newLocalTestMux(t, localDC, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/myapp/onboard?host=anything",
		strings.NewReader(`{"host":"myapp.example.org","port":8080}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

// TestOnboardMutationPeerUnreachable proves an unreachable peer surfaces as
// 502, via peerMutate's transport-level error path through mapPeerMutationErr.
func TestOnboardMutationPeerUnreachable(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := peerSrv.URL
	peerSrv.Close() // guarantees connection-refused without hardcoding a port

	reg := newTestPeerRegistry(url, true)
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/myapp/onboard?host=dashboard-b",
		strings.NewReader(`{"host":"myapp.example.org","port":8080}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
}

// TestOnboardMutationPeerAuthRejected proves a peer's own 401 (mesh-secret
// mismatch) maps to 502, never relayed verbatim.
func TestOnboardMutationPeerAuthRejected(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/myapp/onboard?host=dashboard-b",
		strings.NewReader(`{"host":"myapp.example.org","port":8080}`))
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

// TestOnboardMutationLocalStillWorks is the no-?host= regression test:
// onboard must still work exactly as before the forwarding branch was added.
func TestOnboardMutationLocalStillWorks(t *testing.T) {
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })
	withSelfHostname(t, func() (string, error) { return "not-orig-id", nil })
	t.Setenv("PROXY_URL", noopProxyStub(t))

	dc := newOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1", env: []string{"FOO=1"}, state: "running", labels: map[string]string{},
	})
	mux := newLocalTestMux(t, dc, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/myapp/onboard",
		strings.NewReader(`{"host":"myapp.example.org","port":8080}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}

// TestOnboardHostParamOwnIdentity proves ?host=<this host's own identity>
// behaves identically to no host param at all — processed locally, peer
// never contacted.
func TestOnboardHostParamOwnIdentity(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })
	withSelfHostname(t, func() (string, error) { return "not-orig-id", nil })
	t.Setenv("PROXY_URL", noopProxyStub(t))

	dc := newOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1", env: []string{"FOO=1"}, state: "running", labels: map[string]string{},
	})

	var peerHit atomic.Bool
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { peerHit.Store(true) }))
	t.Cleanup(peerSrv.Close)
	reg := newTestPeerRegistry(peerSrv.URL, true)

	mux := newLocalTestMux(t, dc, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/myapp/onboard?host=dashboard-a",
		strings.NewReader(`{"host":"myapp.example.org","port":8080}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if peerHit.Load() {
		t.Error("peer was contacted for ?host=<own identity> — must be treated identically to no host param")
	}
}

// TestOnboardProxyRefreshHitsPeerProxy proves a forwarded onboard's
// proxyRefresh(proxyURL) call targets the PEER's own proxy, not the local
// instance's.
func TestOnboardProxyRefreshHitsPeerProxy(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })
	withSelfHostname(t, func() (string, error) { return "not-orig-id", nil })

	var peerRefreshHit atomic.Bool
	peerProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/refresh" {
			peerRefreshHit.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(peerProxy.Close)

	peerSrv, calls := newPeerDiscoveryServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1", env: []string{"FOO=1"}, state: "running", labels: map[string]string{},
	}, peerProxy.URL, true)
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

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/myapp/onboard?host=dashboard-b",
		strings.NewReader(`{"host":"myapp.example.org","port":8080}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(calls.all()) == 0 {
		t.Fatal("peer docker stub was never hit")
	}
	if !peerRefreshHit.Load() {
		t.Error("peer's own /refresh was never hit — the peer handler's proxyRefresh did not fire")
	}
	if localRefreshHit.Load() {
		t.Error("the LOCAL proxy's /refresh was hit by a ?host=<peer> onboard — must only refresh the peer's own proxy")
	}
}

// TestOnboardMutationForwardsActorAssertion mirrors
// TestServicesMutationForwardsActorAssertion: a token-authenticated request
// forwards a verifiable X-Pmgr-Actor header, and the peer's audit entry reads
// "alice (via peer-mesh)", not a double-wrapped form.
func TestOnboardMutationForwardsActorAssertion(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	withActorSecret(t, testActorSecret)
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })
	withSelfHostname(t, func() (string, error) { return "not-orig-id", nil })
	readAudit := withAuditFile(t)

	calls := &onboardMutationTracker{}
	peerDC := newTrackedOnboardFakeDockerServer(t, &onboardFakeContainer{
		id: "orig-id", name: "myapp", image: "ghcr.io/org/myapp:v1", env: []string{"FOO=1"}, state: "running", labels: map[string]string{},
	}, calls)
	inner := peerDiscoveryMutateHandler("s3cret", "dashboard-b", peerDC, noopProxyStub(t), true)

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

	req := httptest.NewRequest(http.MethodPost, "/api/discovery/myapp/onboard?host=dashboard-b",
		strings.NewReader(`{"host":"myapp.example.org","port":8080}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(calls.all()) == 0 {
		t.Fatal("peer docker stub was never hit")
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
