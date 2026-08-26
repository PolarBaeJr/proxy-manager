package main

import (
	"net/http"
	"testing"
)

// newRollingOpGuardTestMux mirrors newRolloutTestMux (rollout_test.go) but
// also threads a *rollingOpManager, since that helper's signature predates
// rom and always passes nil.
func newRollingOpGuardTestMux(t *testing.T, dc *dockerClient, onb *OnboardedStore, rom *rollingOpManager) http.Handler {
	t.Helper()
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)
	return newDashboardMux(dc, nil, auth, newRateLimiter(), newImageChecker(dc), "", nil, onb, nil, nil, nil, nil, nil, nil, nil, nil, rom)
}

// TestRollingOpActiveGuardRejectsOtherMutations proves the guard extension
// in api.go: a service with an active rolling-replace job must reject any
// other mutation (replace, scale) with 409, must reject a second
// rolling-replace START with 409, but must still allow the rolling-replace
// STATUS poll (GET) through — the same three-way split
// TestRolloutActiveGuardRejectsOtherMutations already proves for the
// canary-rollout guard, applied to the new rolling-op guard instead.
func TestRollingOpActiveGuardRejectsOtherMutations(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 2)
	dc := newRolloutDockerStub(t, f)
	onb := newTestOnboardedStore(t)

	rom := newRollingOpManager(dc)
	rom.ops["app"] = &rollingOpState{Service: "app", Status: rollingOpStatusRunning, Total: 2}

	mux := newRollingOpGuardTestMux(t, dc, onb, rom)

	rec := doJSONReq(mux, http.MethodPost, "/api/services/app/replace", `{"image":"ghcr.io/org/app:v2"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("replace during active rolling replace: status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}

	rec = doJSONReq(mux, http.MethodPost, "/api/services/app/scale", `{"Replicas":3}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("scale during active rolling replace: status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}

	rec = doJSONReq(mux, http.MethodPost, "/api/services/app/rolling-replace", `{"image":"ghcr.io/org/app:v3"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("starting a 2nd rolling replace: status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}

	rec = doJSONReq(mux, http.MethodGet, "/api/services/app/rolling-replace", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET rolling-replace during active job: status = %d, want 200 (status poll must stay exempt); body %s", rec.Code, rec.Body.String())
	}
}

// TestRollingOpGuardExemptsUnrelatedService proves the guard is keyed by
// service name, not global: a rolling replace active on one service must not
// block a mutation on a different, unrelated service.
func TestRollingOpGuardExemptsUnrelatedService(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 2)
	seedLiveReplicas(f, "other", 1)
	dc := newRolloutDockerStub(t, f)
	onb := newTestOnboardedStore(t)

	rom := newRollingOpManager(dc)
	rom.ops["app"] = &rollingOpState{Service: "app", Status: rollingOpStatusRunning, Total: 2}

	mux := newRollingOpGuardTestMux(t, dc, onb, rom)

	rec := doJSONReq(mux, http.MethodPost, "/api/services/other/scale", `{"Replicas":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("scale on unrelated service: status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
}
