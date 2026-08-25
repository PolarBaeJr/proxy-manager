package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestImagesEndpointHostParamForwardsToPeer proves the full request path —
// not just peerImagesHandler in isolation (peers_test.go) — actually
// forwards GET /api/images?host=<identity> to the matching peer, tags the
// response with that peer's Machine, and strips every DeleteToken before it
// reaches the caller (the actual security boundary — see the SECURITY
// comment in api.go).
func TestImagesEndpointHostParamForwardsToPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	var localHit atomic.Bool
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit.Store(true)
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))

	peerDC := imagesDockerStub(t)
	peerRS, err := loadReleasesStore(filepath.Join(t.TempDir(), "releases.json"))
	if err != nil {
		t.Fatal(err)
	}
	peerIH, err := loadImageHistoryStore(filepath.Join(t.TempDir(), "image-history.json"))
	if err != nil {
		t.Fatal(err)
	}
	peerOnb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	peerSrv := httptest.NewServer(peerImagesHandler("s3cret", "dashboard-b", peerDC, peerRS, peerIH, peerOnb))
	t.Cleanup(peerSrv.Close)

	// Confirm the peer's raw data genuinely has a non-empty DeleteToken —
	// otherwise the strip-to-"" assertion below would pass vacuously.
	{
		req := httptest.NewRequest(http.MethodGet, "/peer/images", nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		peerImagesHandler("s3cret", "dashboard-b", peerDC, peerRS, peerIH, peerOnb).ServeHTTP(rec, req)
		var raw peerImagesResp
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, si := range raw.Images.Services {
			for _, e := range si.Entries {
				if e.DeleteToken != "" {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("peer's raw /peer/images response has no non-empty DeleteToken to strip — test fixture is broken: %s", rec.Body.String())
		}
	}

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(peerSrv.URL, true, "dashboard-b", "dev")

	rs, err := loadReleasesStore(filepath.Join(t.TempDir(), "releases.json"))
	if err != nil {
		t.Fatal(err)
	}
	ih, err := loadImageHistoryStore(filepath.Join(t.TempDir(), "image-history.json"))
	if err != nil {
		t.Fatal(err)
	}
	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	ic := newImageChecker(dc)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, rs, nil, ih, nil, nil, reg)

	req := httptest.NewRequest("GET", "/api/images?host=dashboard-b", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var info imagesInfoResp
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Machine != "dashboard-b" {
		t.Errorf("Machine = %q, want %q", info.Machine, "dashboard-b")
	}
	if len(info.Services) == 0 {
		t.Fatal("Services = [], want at least one service from the peer's stub data")
	}
	for _, si := range info.Services {
		for _, e := range si.Entries {
			if e.DeleteToken != "" {
				t.Fatalf("entry %+v: DeleteToken = %q, want empty (peer image tokens must never round-trip)", e, e.DeleteToken)
			}
		}
	}
	if localHit.Load() {
		t.Error("local docker stub was hit — GET /api/images?host=<peer> must not touch the local daemon")
	}
}

// TestImagesEndpointNoHostParamTagsLocalMachine proves the plain (no
// ?host=) request path — the one every existing UI render hits — still tags
// the response with this host's own identity and does NOT strip
// DeleteToken (stripping is a peer-forwarding-only security boundary; local
// callers are allowed to mutate their own images).
func TestImagesEndpointNoHostParamTagsLocalMachine(t *testing.T) {
	dc := imagesDockerStub(t)
	rs, err := loadReleasesStore(filepath.Join(t.TempDir(), "releases.json"))
	if err != nil {
		t.Fatal(err)
	}
	ih, err := loadImageHistoryStore(filepath.Join(t.TempDir(), "image-history.json"))
	if err != nil {
		t.Fatal(err)
	}
	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	ic := newImageChecker(dc)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	reg := newPeerRegistry(nil, "s3cret", "dashboard-a", "dev", 0, nil)

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, rs, nil, ih, nil, nil, reg)

	req := httptest.NewRequest("GET", "/api/images", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var info imagesInfoResp
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Machine != "dashboard-a" {
		t.Errorf("Machine = %q, want %q", info.Machine, "dashboard-a")
	}
	found := false
	for _, si := range info.Services {
		for _, e := range si.Entries {
			if e.DeleteToken != "" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no entry had a non-empty DeleteToken — local (non-peer) responses must not be stripped: %s", rec.Body.String())
	}
}

func TestImagesEndpointHostParamUnknownHost(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

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

	reg := newPeerRegistry([]string{"http://peer-b:8098"}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult("http://peer-b:8098", true, "dashboard-b", "dev")

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, nil, nil, nil, nil, nil, reg)

	req := httptest.NewRequest("GET", "/api/images?host=nonexistent-host", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

func TestImagesEndpointHostParamNoRegistry(t *testing.T) {
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

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest("GET", "/api/images?host=anything", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusNotFound)
	}
}

func TestImagesEndpointHostParamPeerUnreachable(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

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

	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := peerSrv.URL
	peerSrv.Close() // guarantees connection-refused without hardcoding a port

	reg := newPeerRegistry([]string{url}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult(url, true, "dashboard-b", "dev")

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, nil, nil, nil, nil, nil, reg)

	req := httptest.NewRequest("GET", "/api/images?host=dashboard-b", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if body["error"] == "" {
		t.Errorf("body = %+v, want a non-empty error field", body)
	}
}

// TestImagesMutatingEndpointsRejectHostParam proves every mutating route
// under /api/images/ rejects a ?host= query param with 400 BEFORE touching
// any store or the docker daemon — the plan's constraint that image
// mutations stay strictly local-only.
func TestImagesMutatingEndpointsRejectHostParam(t *testing.T) {
	var mutated atomic.Bool
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/images/") {
			mutated.Store(true)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			json.NewEncoder(w).Encode([]dockerImage{})
		default:
			json.NewEncoder(w).Encode([]dockerContainer{})
		}
	}))
	rs, err := loadReleasesStore(filepath.Join(t.TempDir(), "releases.json"))
	if err != nil {
		t.Fatal(err)
	}
	ih, err := loadImageHistoryStore(filepath.Join(t.TempDir(), "image-history.json"))
	if err != nil {
		t.Fatal(err)
	}
	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	ic := newImageChecker(dc)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, rs, nil, ih, nil, nil, nil)

	cases := []struct {
		method, path, body string
	}{
		{"POST", "/api/images/mark?host=dashboard-b", `{"service":"app","tag":"v1","label":"x"}`},
		{"DELETE", "/api/images/mark?host=dashboard-b", `{"service":"app","tag":"v1"}`},
		{"DELETE", "/api/images/delete?host=dashboard-b", `{"token":"ghcr.io/org/app:v2"}`},
		{"POST", "/api/images/prune?host=dashboard-b", `{"service":"","keep_n":0}`},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status = %d, body %s, want %d", c.method, c.path, rec.Code, rec.Body.String(), http.StatusBadRequest)
		}
	}
	if mutated.Load() {
		t.Error("docker image removal endpoint was hit — a rejected ?host= mutation must not reach the daemon")
	}
	if all := rs.All(); len(all) != 0 {
		t.Errorf("rs.All() = %v, want empty — a rejected ?host= mark must not write a release mark", all)
	}
}
