package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// This file is the permanent regression coverage for a production bug: a
// stale STOPPED/EXITED container left behind by a prior operation (e.g. a
// removal that silently failed — replaceService and friends only log a
// removal error, they don't return it) could be picked as the "template" a
// replace/scale/autoupdate-toggle/canary-stage clones its env, mounts and
// labels from, and could inflate the recreated replica count, purely
// because listAll queries Docker with all=true and liveOnly/canaryOnly only
// filter on the canary label, never on State.
//
// Concretely this dropped manually-added env vars (including secrets) on a
// LATER replace that didn't itself pass an env override, and could grow a
// service's replica count across repeated replaces. See preferRunning and
// runningOnly in docker.go, and onboardedBaseEnv in onboarded.go, for the fix.

// TestReplaceServicePrefersRunningContainerAsTemplate reproduces the core
// bug directly: a stale EXITED leftover (no FOO=bar) is listed BEFORE the
// real RUNNING container (has FOO=bar) in Docker's /containers/json — as
// Docker gives no ordering guarantee between running and exited containers.
// replaceService must:
//   - use the RUNNING container's env as the template (not the exited one's),
//   - create exactly ONE replacement (not two — the exited leftover must not
//     count toward the preserved replica count), and
//   - still tear down BOTH old containers (stale exited leftovers must get
//     cleaned up too, not accumulate forever).
func TestReplaceServicePrefersRunningContainerAsTemplate(t *testing.T) {
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	var mu sync.Mutex
	var createCount int
	removed := map[string]bool{}

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			// Stale EXITED leftover (no FOO) listed BEFORE the current live
			// one (has FOO) — simulating a failed-removal leftover.
			json.NewEncoder(w).Encode([]dockerContainer{
				{
					ID: "stale1", Names: []string{"/goproxy-app-0"}, State: "exited",
					Image:  "ghcr.io/org/app:v1",
					Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
				},
				{
					ID: "live1", Names: []string{"/goproxy-app-1"}, State: "running",
					Image:  "ghcr.io/org/app:v2",
					Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/stale1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{"PORT=8080"}}})
		case strings.HasSuffix(r.URL.Path, "/live1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{"PORT=8080", "FOO=bar"}}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			createCount++
			var body struct {
				Env []string `json:"Env"`
			}
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			foundFoo := false
			for _, e := range body.Env {
				if e == "FOO=bar" {
					foundFoo = true
				}
			}
			if !foundFoo {
				t.Errorf("replaceService used the stale exited container's env as template, dropping FOO=bar; created env = %v", body.Env)
			}
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		case strings.HasSuffix(r.URL.Path, "/stale1") && r.Method == http.MethodDelete:
			removed["stale1"] = true
		case strings.HasSuffix(r.URL.Path, "/live1") && r.Method == http.MethodDelete:
			removed["live1"] = true
		default:
			w.Write([]byte("{}"))
		}
	}))

	if err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v3"}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if createCount != 1 {
		t.Errorf("createCount = %d, want 1 (the exited leftover must not inflate the recreated replica count)", createCount)
	}
	if !removed["stale1"] || !removed["live1"] {
		t.Errorf("teardown removed = %v, want both stale1 and live1 removed (teardown scope must stay broad, cleaning up stale leftovers too)", removed)
	}
}

// TestSequentialReplacePreservesEnvAcrossReplaces is a baseline sanity check
// against a "clean" daemon (no stale leftovers): a two-call sequence where
// only the FIRST call passes an env edit must still have that edit present
// after the SECOND call, which passes no env at all. This alone doesn't
// prove the stale-template bug (there's nothing stale here to mis-pick) but
// guards against a regression in the ordinary merge-onto-current-env path.
func TestSequentialReplacePreservesEnvAcrossReplaces(t *testing.T) {
	var mu sync.Mutex
	liveEnv := []string{"PORT=8080", "SECRET_KEY=hunter2"}
	liveImage := "ghcr.io/org/app:v1"
	liveName := "goproxy-app-1"

	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/" + liveName}, State: "running",
				Image:  liveImage,
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": liveEnv}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			var body struct {
				Image string   `json:"Image"`
				Env   []string `json:"Env"`
			}
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			// Simulate the daemon: this new container becomes "the" live one.
			liveEnv = body.Env
			liveImage = body.Image
			liveName = "goproxy-app-2"
			json.NewEncoder(w).Encode(map[string]any{"Id": "tpl1"})
		default:
			w.Write([]byte("{}"))
		}
	}))

	ctx := context.Background()

	// Call 1: add FOO=bar.
	if err := dc.replaceService(ctx, "app", ReplaceServiceRequest{
		Image: "ghcr.io/org/app:v2",
		Env:   map[string]string{"FOO": "bar"},
	}); err != nil {
		t.Fatalf("replace 1: %v", err)
	}

	// Call 2: no env at all — should keep FOO.
	if err := dc.replaceService(ctx, "app", ReplaceServiceRequest{
		Image: "ghcr.io/org/app:v3",
	}); err != nil {
		t.Fatalf("replace 2: %v", err)
	}

	mu.Lock()
	afterSecond := append([]string(nil), liveEnv...)
	mu.Unlock()

	found := false
	for _, e := range afterSecond {
		if e == "FOO=bar" {
			found = true
		}
	}
	if !found {
		t.Errorf("FOO=bar was dropped on the second replace (no env passed); env = %v", afterSecond)
	}
}

// TestOnboardedBaseEnvPrefersRunningClone is onboardedBaseEnv's equivalent of
// TestReplaceServicePrefersRunningContainerAsTemplate: a stale EXITED
// goproxy-onb-<name>-* clone (no FOO=bar) is listed BEFORE the real RUNNING
// clone (has FOO=bar). onboardedBaseEnv must return the running clone's env,
// not the exited one's.
func TestOnboardedBaseEnvPrefersRunningClone(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{
				{
					ID: "stale1", Names: []string{"/goproxy-onb-app-1"}, State: "exited",
					Image: "ghcr.io/org/app:v1",
				},
				{
					ID: "live1", Names: []string{"/goproxy-onb-app-2"}, State: "running",
					Image: "ghcr.io/org/app:v2",
				},
			})
		case strings.HasSuffix(r.URL.Path, "/stale1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{"PORT=8080"}}})
		case strings.HasSuffix(r.URL.Path, "/live1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{"PORT=8080", "FOO=bar"}}})
		default:
			w.Write([]byte("{}"))
		}
	}))

	env := dc.onboardedBaseEnv(context.Background(), "app", OnboardedService{Name: "app"})
	found := false
	for _, e := range env {
		if e == "FOO=bar" {
			found = true
		}
	}
	if !found {
		t.Errorf("onboardedBaseEnv used the stale exited clone's env, dropping FOO=bar; got %v", env)
	}
}

// TestGuardUnscalablePrefersRunningContainer covers the same existing[0]
// pattern in guardUnscalable: a stale EXITED leftover predating the addition
// of proxy.unscalable to the compose file (so it lacks the label) is listed
// BEFORE the real RUNNING container (which has the label). The guard must
// still fire — picking the exited container would silently let an
// unscalable service scale past 1.
func TestGuardUnscalablePrefersRunningContainer(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			json.NewEncoder(w).Encode([]dockerContainer{
				{
					ID: "stale1", Names: []string{"/goproxy-app-0"}, State: "exited",
					Labels: map[string]string{labelEnable: "true", labelService: "app"}, // no unscalable label
				},
				{
					ID: "live1", Names: []string{"/goproxy-app-1"}, State: "running",
					Labels: map[string]string{labelEnable: "true", labelService: "app", labelUnscalable: "true"},
				},
			})
			return
		}
		w.Write([]byte("{}"))
	}))

	if err := dc.guardUnscalable(context.Background(), "app", 3); err == nil {
		t.Error("guardUnscalable did not refuse scaling past 1; it picked the stale exited container (missing the label) as the template")
	}
}

// TestSetAutoUpdateLabelCountMatchesRunningReplicas mirrors
// TestReplaceServicePrefersRunningContainerAsTemplate for
// setAutoUpdateLabel's identical tplSet fix: a stale exited leftover must
// not inflate the recreated replica count.
func TestSetAutoUpdateLabelCountMatchesRunningReplicas(t *testing.T) {
	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	var mu sync.Mutex
	var createCount int

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{
				{
					ID: "stale1", Names: []string{"/goproxy-app-0"}, State: "exited",
					Image:  "ghcr.io/org/app:v1",
					Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
				},
				{
					ID: "live1", Names: []string{"/goproxy-app-1"}, State: "running",
					Image:  "ghcr.io/org/app:v1",
					Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/stale1/json"), strings.HasSuffix(r.URL.Path, "/live1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{"PORT=8080"}}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			createCount++
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		default:
			w.Write([]byte("{}"))
		}
	}))

	if err := dc.setAutoUpdateLabel(context.Background(), "app", true); err != nil {
		t.Fatalf("setAutoUpdateLabel: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if createCount != 1 {
		t.Errorf("createCount = %d, want 1 (the exited leftover must not inflate the recreated replica count)", createCount)
	}
}

// TestScaleServiceRemovesStoppedReplicaBeforeRunningOne reproduces the
// stepper's companion bug: the UI displays the RUNNING count (a stopped
// member already reads as "not there" — see replicaCtrl in ui.go), so
// scaling down by one must reclaim the already-stopped member rather than
// stopping a live one and leaving the stale stopped one behind. Before the
// fix, scaleService's removal order only looked at name (highest index
// first), so a stopped LOW-index replica sitting next to a running
// HIGH-index one would survive while the running one got killed.
func TestScaleServiceRemovesStoppedReplicaBeforeRunningOne(t *testing.T) {
	var removedIDs []string

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{
				{
					ID: "orig", Names: []string{"/app"}, State: "running",
					Image:  "ghcr.io/org/app:v1",
					Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
				},
				{
					// Lower index, but STOPPED — this is the one that must go.
					ID: "stopped2", Names: []string{"/goproxy-app-2"}, State: "exited",
					Image:  "ghcr.io/org/app:v1",
					Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
				},
				{
					// Higher index, RUNNING — must survive the scale-down.
					ID: "running3", Names: []string{"/goproxy-app-3"}, State: "running",
					Image:  "ghcr.io/org/app:v1",
					Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
				},
			})
		case strings.Contains(r.URL.Path, "/stop"):
			removedIDs = append(removedIDs, "stop:"+strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/stop"))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/containers/")
			removedIDs = append(removedIDs, "rm:"+id)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Write([]byte("{}"))
		}
	}))

	// 3 total (1 original + 2 goproxy-managed) -> 2: exactly one goproxy-
	// managed replica must be removed, and it must be the stopped one.
	if err := dc.scaleService(context.Background(), "app", 2); err != nil {
		t.Fatalf("scaleService: %v", err)
	}
	var sawStoppedRemoved, sawRunningRemoved bool
	for _, id := range removedIDs {
		if strings.Contains(id, "stopped2") {
			sawStoppedRemoved = true
		}
		if strings.Contains(id, "running3") {
			sawRunningRemoved = true
		}
	}
	if !sawStoppedRemoved {
		t.Errorf("expected the already-stopped replica (stopped2) to be removed, removedIDs=%v", removedIDs)
	}
	if sawRunningRemoved {
		t.Errorf("running replica (running3) must not be removed while a stopped one remains, removedIDs=%v", removedIDs)
	}
}
