package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, onb, nil, nil, nil, nil, nil, nil)

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
