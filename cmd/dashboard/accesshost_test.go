package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const fakeAccessBody = `{"now":1000,"total":3,"count":2,"entries":[{"t":900,"method":"GET","host":"a.example.com","path":"/x","status":200,"bytes":12,"ms":3,"ip":"1.2.3.4","backend":"http://b:80","ua":"curl"}]}`

func TestPeerAccessHandlerValidSecret(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeAccessBody))
	}))
	t.Cleanup(proxy.Close)

	h := peerAccessHandler("s3cret", "dashboard-a", proxy.URL)
	req := httptest.NewRequest(http.MethodGet, "/peer/access", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("X-Peer-Identity"); got != "dashboard-a" {
		t.Errorf("X-Peer-Identity = %q, want dashboard-a", got)
	}
	if rec.Body.String() != fakeAccessBody {
		t.Errorf("body = %q, want %q", rec.Body.String(), fakeAccessBody)
	}
}

func TestPeerAccessHandlerWrongSecret(t *testing.T) {
	h := peerAccessHandler("s3cret", "dashboard-a", "http://unused")
	req := httptest.NewRequest(http.MethodGet, "/peer/access", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPeerAccessHandlerEmptySecretDisabled(t *testing.T) {
	h := peerAccessHandler("", "dashboard-a", "http://unused")
	req := httptest.NewRequest(http.MethodGet, "/peer/access", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestPeerAccessHandlerWrongMethod(t *testing.T) {
	h := peerAccessHandler("s3cret", "dashboard-a", "http://unused")
	req := httptest.NewRequest(http.MethodPost, "/peer/access", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestPeerAccessHandlerNoLocalProxyConfigured(t *testing.T) {
	h := peerAccessHandler("s3cret", "dashboard-a", "")
	req := httptest.NewRequest(http.MethodGet, "/peer/access", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestPeerAccessHandlerUpstreamUnreachable(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := proxy.URL
	proxy.Close() // guarantees connection-refused without hardcoding a port

	h := peerAccessHandler("s3cret", "dashboard-a", url)
	req := httptest.NewRequest(http.MethodGet, "/peer/access", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestPeerAccessHandlerForwardsQueryParams(t *testing.T) {
	var gotLimit string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeAccessBody))
	}))
	t.Cleanup(proxy.Close)

	h := peerAccessHandler("s3cret", "dashboard-a", proxy.URL)
	req := httptest.NewRequest(http.MethodGet, "/peer/access?limit=50", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if gotLimit != "50" {
		t.Errorf("proxy saw limit=%q, want 50", gotLimit)
	}
}

func TestPeerAccessHandlerPropagatesUpstreamStatus(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(proxy.Close)

	h := peerAccessHandler("s3cret", "dashboard-a", proxy.URL)
	req := httptest.NewRequest(http.MethodGet, "/peer/access", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("body = %q, want it to contain the upstream's body", rec.Body.String())
	}
}

// TestPeerAccessHandlerIgnoresHostParam proves the loop-safety invariant
// documented on accesshost.go: /peer/access answers with THIS host's own
// local proxy only, never re-reading a host= param to forward elsewhere. A
// stray host= in the incoming request is simply ignored by the upstream
// proxy's own accessHandler (unrecognized query keys), not specially
// handled here.
func TestPeerAccessHandlerIgnoresHostParam(t *testing.T) {
	var gotQuery string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeAccessBody))
	}))
	t.Cleanup(proxy.Close)

	h := peerAccessHandler("s3cret", "dashboard-a", proxy.URL)
	req := httptest.NewRequest(http.MethodGet, "/peer/access?host=dashboard-c&limit=10", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != fakeAccessBody {
		t.Errorf("body = %q, want the local fake proxy's own response %q", rec.Body.String(), fakeAccessBody)
	}
	if !strings.Contains(gotQuery, "host=dashboard-c") || !strings.Contains(gotQuery, "limit=10") {
		t.Errorf("proxy saw query %q, want it to include the untouched host= and limit= params", gotQuery)
	}
}

func newAccessTestMux(t *testing.T, reg *PeerRegistry) http.Handler {
	t.Helper()
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	ic := newImageChecker(dc)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	return newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, nil, nil, nil, nil, nil, reg)
}

func TestAccessEndpointHostParamForwardsToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	t.Setenv("PROXY_URL", "")

	peerProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeAccessBody))
	}))
	t.Cleanup(peerProxy.Close)
	peerSrv := httptest.NewServer(peerAccessHandler("s3cret", "dashboard-b", peerProxy.URL))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", false)

	mux := newAccessTestMux(t, reg)

	req := httptest.NewRequest("GET", "/api/access?host=dashboard-b", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != fakeAccessBody {
		t.Errorf("body = %q, want the peer's fake response %q", rec.Body.String(), fakeAccessBody)
	}
}

func TestAccessEndpointHostParamUnknownHost(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	reg := newPeerRegistry([]string{"http://peer-b:8098"}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult("http://peer-b:8098", true, "dashboard-b", "dev", false)

	mux := newAccessTestMux(t, reg)

	req := httptest.NewRequest("GET", "/api/access?host=dashboard-ghost", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

func TestAccessEndpointHostParamEqualsSelfIdentityUsesLocal(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	var localHit bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit = true
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeAccessBody))
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("PROXY_URL", proxy.URL)

	// If the peer handler were ever hit for host==self, this would panic —
	// it must never be reached.
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("peer handler was hit for host == self identity — must be served locally")
	}))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-a", "dev", false)

	mux := newAccessTestMux(t, reg)

	req := httptest.NewRequest("GET", "/api/access?host=dashboard-a", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !localHit {
		t.Error("local proxy was never hit — host == self identity must be served locally")
	}
}

func TestAccessEndpointNoHostParamUsesLocalUnchanged(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeAccessBody))
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("PROXY_URL", proxy.URL)

	mux := newAccessTestMux(t, nil)

	req := httptest.NewRequest("GET", "/api/access", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != fakeAccessBody {
		t.Errorf("body = %q, want %q", rec.Body.String(), fakeAccessBody)
	}
}

func TestAccessEndpointRegisteredWithoutLocalProxy(t *testing.T) {
	t.Setenv("PROXY_URL", "")

	mux := newAccessTestMux(t, nil)

	req := httptest.NewRequest("GET", "/api/access", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusServiceUnavailable)
	}
	if rec.Body.String() == "" {
		t.Error("want a non-empty error body")
	}
}

func TestAccessEndpointHostParamPeerUnreachable(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := peerSrv.URL
	peerSrv.Close() // guarantees connection-refused without hardcoding a port

	reg := newPeerRegistry([]string{url}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(url, true, "dashboard-b", "dev", false)

	mux := newAccessTestMux(t, reg)

	req := httptest.NewRequest("GET", "/api/access?host=dashboard-b", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
}

// TestAccessEndpointHostParamPeerRejectsSecretBecomes502 proves a peer's
// non-200 response (here, 401 from a mismatched/rotated
// DASHBOARD_PEER_SECRET) is never relayed verbatim — see the reasoning on
// forwardAccessLogToPeer in accesshost.go. A raw 401 would be treated by the
// UI's api() helper as "session expired", popping an auth dialog on every
// 5s poll while "follow" is on.
func TestAccessEndpointHostParamPeerRejectsSecretBecomes502(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	// Peer's own /peer/access is configured with a DIFFERENT secret, so it
	// answers this caller's bearer token with 401.
	peerSrv := httptest.NewServer(peerAccessHandler("rotated", "dashboard-b", "http://unused"))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev", false)

	mux := newAccessTestMux(t, reg)

	req := httptest.NewRequest("GET", "/api/access?host=dashboard-b", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("status = %d — a peer's 401 must never be relayed verbatim (dashboard auth vocabulary), got it anyway", rec.Code)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
}

// TestAccessEndpointHostParamEmptyPeerSecretUnknownHost mirrors Images'
// guard: a recorded peer but no local DASHBOARD_PEER_SECRET to authenticate
// with must fail the same way as an unresolvable host (404), not attempt a
// request with an empty bearer token.
func TestAccessEndpointHostParamEmptyPeerSecretUnknownHost(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "")

	reg := newPeerRegistry([]string{"http://peer-b:8098"}, "", "dashboard-a", "dev", 0, nil)
	reg.recordResult("http://peer-b:8098", true, "dashboard-b", "dev", false)

	mux := newAccessTestMux(t, reg)

	req := httptest.NewRequest("GET", "/api/access?host=dashboard-b", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

func TestAccessEndpointHostParamNoRegistryIgnoresHost(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeAccessBody))
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("PROXY_URL", proxy.URL)

	mux := newAccessTestMux(t, nil)

	req := httptest.NewRequest("GET", "/api/access?host=dashboard-b", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != fakeAccessBody {
		t.Errorf("body = %q, want the plain local response %q (no registry = ignore stray host=)", rec.Body.String(), fakeAccessBody)
	}
}
