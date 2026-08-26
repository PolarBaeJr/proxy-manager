package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestServicesDualTrackedFlag proves GET /api/services marks DualTracked only
// for a service that is BOTH label-managed (discovered live from docker ps)
// AND onboarded (tracked in onboarded.json) under the same name — the exact
// pattern behind the sfubadminton.com path-keying incident and the OCI
// image-label staleness bug. A pure onboarded-only, standalone card must NOT
// get the flag.
func TestServicesDualTrackedFlag(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "c1", Names: []string{"/goproxy-web-1"}, State: "running",
			Image:  "ghcr.io/org/web:v1",
			Labels: map[string]string{labelEnable: "true", labelService: "web", labelHost: "web.example"},
		}})
	}))

	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	// "web" matches the labeled container above — dual-tracked.
	if err := onb.Put(OnboardedService{Name: "web", Host: "web.example", Image: "ghcr.io/org/web:v1", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	// "solo" has no labeled counterpart — pure onboarded, standalone card.
	if err := onb.Put(OnboardedService{Name: "solo", Host: "solo.example", Image: "ghcr.io/org/solo:v1", Replicas: 1}); err != nil {
		t.Fatal(err)
	}

	ic := newImageChecker(dc)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest("GET", "/api/services", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var svcs []Service
	if err := json.Unmarshal(rec.Body.Bytes(), &svcs); err != nil {
		t.Fatalf("decode: %v", err)
	}

	web := pickService(svcs, "web")
	if web == nil {
		t.Fatal("web service missing from response")
	}
	if !web.Onboarded || !web.DualTracked {
		t.Errorf("web: Onboarded=%v DualTracked=%v, want both true", web.Onboarded, web.DualTracked)
	}

	solo := pickService(svcs, "solo")
	if solo == nil {
		t.Fatal("solo service missing from response")
	}
	if !solo.Onboarded {
		t.Error("solo: Onboarded=false, want true")
	}
	if solo.DualTracked {
		t.Error("solo: DualTracked=true, want false (onboarded-only, not label-managed)")
	}
}

// TestServicesEndpointImageCheckError proves GET /api/services surfaces a
// failing image checker via image_check_error: populated for the service
// whose registry check is failing, empty for a healthy one.
func TestServicesEndpointImageCheckError(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{
				{
					ID: "c1", Names: []string{"/goproxy-bad-1"}, State: "running",
					Image:  "ghcr.io/org/bad:v1",
					Labels: map[string]string{labelEnable: "true", labelService: "bad", labelHost: "bad.example"},
				},
				{
					ID: "c2", Names: []string{"/goproxy-good-1"}, State: "running",
					Image:  "ghcr.io/org/good:v1",
					Labels: map[string]string{labelEnable: "true", labelService: "good", labelHost: "good.example"},
				},
			})
		case strings.Contains(r.URL.Path, "/images/"):
			json.NewEncoder(w).Encode(map[string]any{"RepoDigests": []string{}})
		case strings.Contains(r.URL.Path, "/distribution/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.Write([]byte("{}"))
		}
	}))
	ic := newImageChecker(dc)
	// Only "bad"'s image fails its check — "good" is left unchecked, i.e.
	// no cached status, which must also render as an empty
	// image_check_error (not an error itself).
	ic.Check(context.Background(), "ghcr.io/org/bad:v1")

	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest("GET", "/api/services", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var svcs []Service
	if err := json.Unmarshal(rec.Body.Bytes(), &svcs); err != nil {
		t.Fatalf("decode: %v", err)
	}

	bad := pickService(svcs, "bad")
	if bad == nil || bad.ImageCheckError == "" {
		t.Fatalf("bad.ImageCheckError = %q, want non-empty", bad.ImageCheckError)
	}
	good := pickService(svcs, "good")
	if good == nil || good.ImageCheckError != "" {
		t.Fatalf("good.ImageCheckError = %q, want empty", good.ImageCheckError)
	}
}

// TestServicesEndpointMergesPeer proves the full request path — not just
// fetchPeerServices/peerServicesHandler in isolation (peers_test.go) —
// actually merges a peer's services into /api/services when called through
// newDashboardMux's auth+registry wiring, mirroring
// TestServiceStatusEndpointMergesPeer (servicestatus_test.go).
func TestServicesEndpointMergesPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "c1", Names: []string{"/goproxy-app-1"}, State: "running",
			Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
		}})
	}))
	peerDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]dockerContainer{{
			ID: "c2", Names: []string{"/goproxy-player-1"}, State: "running",
			Labels: map[string]string{labelEnable: "true", labelService: "player", labelHost: "badminton-mac.example", labelPort: "3000"},
		}})
	}))

	onb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	peerOnb, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}

	peerSrv := httptest.NewServer(peerServicesHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil))
	t.Cleanup(peerSrv.Close)

	reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)

	ic := newImageChecker(dc)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, nil, nil, nil, nil, nil, reg, nil, nil, nil)

	req := httptest.NewRequest("GET", "/api/services", nil)
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var svcs []Service
	if err := json.Unmarshal(rec.Body.Bytes(), &svcs); err != nil {
		t.Fatalf("decode: %v", err)
	}

	local := pickService(svcs, "app")
	if local == nil || local.Machine != "dashboard-a" {
		t.Fatalf("local service = %+v, want tagged dashboard-a", local)
	}
	peer := pickService(svcs, "player")
	if peer == nil || peer.Machine != "dashboard-b" {
		t.Fatalf("peer service = %+v, want tagged dashboard-b", peer)
	}
}
