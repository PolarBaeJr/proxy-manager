package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// POST /api/services/{name}/check forces an immediate registry-digest
// comparison and returns the refreshed imageStatus — service-not-found is a
// 404, a known service reaches the checker and its result comes back.
func TestServicesCheckEndpoint(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "c1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image:  "ghcr.io/org/app:v1",
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
			}})
		case strings.Contains(r.URL.Path, "/images/"):
			json.NewEncoder(w).Encode(map[string]any{"RepoDigests": []string{"ghcr.io/org/app@sha256:local"}})
		case strings.Contains(r.URL.Path, "/distribution/"):
			json.NewEncoder(w).Encode(map[string]any{"Descriptor": map[string]any{"digest": "sha256:registry"}})
		default:
			w.Write([]byte("{}"))
		}
	}))
	ic := newImageChecker(dc)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")

	prev := internalToken
	internalToken = "pmt_internal_test"
	t.Cleanup(func() { internalToken = prev })

	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), ic, "", nil, nil, nil, nil, nil, nil, nil, nil, nil)

	do := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("Authorization", "Bearer "+internalToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("/api/services/nope/check"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown service: status = %d, want 404", rec.Code)
	}

	rec := do("/api/services/app/check")
	if rec.Code != http.StatusOK {
		t.Fatalf("known service: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var status imageStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.LocalDigest != "sha256:local" || status.RegistryDigest != "sha256:registry" {
		t.Errorf("status = %+v, want local/registry digests populated", status)
	}
	if !status.UpdateAvailable {
		t.Errorf("expected UpdateAvailable=true when digests differ, got %+v", status)
	}
}
