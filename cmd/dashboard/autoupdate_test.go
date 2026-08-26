package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestShouldAutoUpdate(t *testing.T) {
	base := Service{Name: "app", Image: "img:latest", AutoUpdate: true}
	avail := &imageStatus{Image: "img:latest", UpdateAvailable: true}
	cases := []struct {
		name  string
		svc   Service
		st    *imageStatus
		fails int
		want  bool
	}{
		{"opted in + update available", base, avail, 0, true},
		{"not opted in", Service{Name: "app", Image: "img:latest"}, avail, 0, false},
		{"nil status", base, nil, 0, false},
		{"no update available", base, &imageStatus{Image: "img:latest"}, 0, false},
		{"checker error", base, &imageStatus{Image: "img:latest", UpdateAvailable: true, Err: "boom"}, 0, false},
		{"canary in flight", func() Service { s := base; s.CanaryImage = "img:new"; return s }(), avail, 0, false},
		{"all replicas stopped", func() Service { s := base; s.AllStopped = true; return s }(), avail, 0, false},
		{"empty image", func() Service { s := base; s.Image = ""; return s }(), avail, 0, false},
		{"failures at cap", base, avail, autoUpdateMaxFailures, false},
		{"failures below cap", base, avail, autoUpdateMaxFailures - 1, true},
	}
	for _, tc := range cases {
		if got := shouldAutoUpdate(tc.svc, tc.st, tc.fails); got != tc.want {
			t.Errorf("%s: shouldAutoUpdate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAutoUpdateSkipReason(t *testing.T) {
	base := Service{Name: "app", Image: "img:latest", AutoUpdate: true}
	avail := &imageStatus{Image: "img:latest", UpdateAvailable: true}
	cases := []struct {
		name string
		svc  Service
		st   *imageStatus
		want string
	}{
		{"no update available", base, &imageStatus{Image: "img:latest"}, ""},
		{"nil status", base, nil, ""},
		{"would fire", base, avail, ""},
		{"not opted in", Service{Name: "app", Image: "img:latest"}, avail, "auto-update is off for this service"},
		{"empty image", func() Service { s := base; s.Image = ""; return s }(), avail, "no image recorded"},
		{"canary in flight", func() Service { s := base; s.CanaryImage = "img:new"; return s }(), avail, "a canary is staged — promote or discard first"},
		{"all replicas stopped", func() Service { s := base; s.AllStopped = true; return s }(), avail, "service is fully stopped"},
		{"checker error", base, &imageStatus{Image: "img:latest", UpdateAvailable: true, Err: "boom"}, "last registry check failed: boom"},
	}
	ctx := context.Background()
	for _, tc := range cases {
		if got := autoUpdateSkipReason(ctx, nil, tc.svc, tc.st); got != tc.want {
			t.Errorf("%s: autoUpdateSkipReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestAutoUpdateSkipReasonHostConfigRefusal is the regression for the
// visibility gap: a service with proxy.autoupdate=true whose live container
// carries a refuse-listed HostConfig field (here, a published port) must get
// an immediate, concrete skip reason from autoUpdateSkipReason itself — not
// only after autoUpdateMaxFailures wasted retry cycles have already run and
// blocks.Get has latched a sticky reason (see TestRunOnceStickyBlockReason).
func TestAutoUpdateSkipReasonHostConfigRefusal(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	dc := runOnceHostConfigStub(t, &failing, &atomic.Bool{})

	ctx := context.Background()
	svcs, err := dc.listServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	app := pickService(svcs, "app")
	if app == nil {
		t.Fatal("listServices did not return \"app\"")
	}

	st := &imageStatus{Image: "ghcr.io/org/app:v1", UpdateAvailable: true}
	reason := autoUpdateSkipReason(ctx, dc, *app, st)
	if reason == "" {
		t.Fatal("autoUpdateSkipReason empty on the very first check, want an immediate host-config-refusal reason")
	}
	if !strings.Contains(reason, "PortBindings") {
		t.Errorf("reason = %q, want it to mention PortBindings", reason)
	}
}

// TestAutoUpdateBlockStore proves the store's own Set/Clear/Get semantics,
// including nil-receiver safety for call sites (mostly tests) that don't
// care about this feature and pass a nil *autoUpdateBlockStore.
func TestAutoUpdateBlockStore(t *testing.T) {
	var nilStore *autoUpdateBlockStore
	if got := nilStore.Get("app"); got != "" {
		t.Errorf("nil store Get() = %q, want empty", got)
	}
	nilStore.Set("app", "boom") // must not panic
	nilStore.Clear("app")       // must not panic

	s := newAutoUpdateBlockStore()
	if got := s.Get("app"); got != "" {
		t.Errorf("Get() on empty store = %q, want empty", got)
	}
	s.Set("app", "refusing to replace")
	if got := s.Get("app"); got != "refusing to replace" {
		t.Errorf("Get() = %q, want %q", got, "refusing to replace")
	}
	s.Clear("app")
	if got := s.Get("app"); got != "" {
		t.Errorf("Get() after Clear() = %q, want empty", got)
	}
}

// runOnceHostConfigStub answers the docker calls runOnce's replace path
// needs for a single label-managed "app" service: the container list, the
// per-container inspect (used by inspectHostConfigUnknowns), and the image/
// registry digest lookups imageChecker.Check needs. While failing is true,
// the inspect response carries a PortBindings entry — the same
// hostConfigRefuseFields hit that made the real badminton-staging-discord-bot
// replace attempt fail — so replaceService's guard refuses every attempt.
// While resolved is true, the local digest is reported equal to the
// registry digest — the actual way runOnce clears a capped failure count in
// production: not a retry succeeding (the cap forbids ever retrying again
// after autoUpdateMaxFailures), but the NEXT image-checker cycle finding
// nothing left to update once someone fixes the underlying problem and
// pulls the image manually.
func runOnceHostConfigStub(t *testing.T, failing, resolved *atomic.Bool) *dockerClient {
	t.Helper()
	containers := []dockerContainer{{
		ID: "c1", Names: []string{"/app"}, State: "running", Image: "ghcr.io/org/app:v1",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80", labelAutoUpdate: "true"},
	}}
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode(containers)
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			hc := map[string]any{}
			if failing.Load() {
				hc["PortBindings"] = map[string]any{"80/tcp": []any{map[string]any{"HostPort": "8080"}}}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"Image":           "sha256:imgid",
				"HostConfig":      hc,
				"Config":          map[string]any{},
				"NetworkSettings": map[string]any{"Networks": map[string]any{}},
			})
		case strings.Contains(r.URL.Path, "/images/"):
			digest := "sha256:local"
			if resolved.Load() {
				digest = "sha256:registry"
			}
			json.NewEncoder(w).Encode(map[string]any{"RepoDigests": []string{"ghcr.io/org/app@" + digest}})
		case strings.Contains(r.URL.Path, "/distribution/"):
			json.NewEncoder(w).Encode(map[string]any{"Descriptor": map[string]any{"digest": "sha256:registry"}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

// runOnceStartFailureStub is runOnceHostConfigStub's counterpart for a
// failure mode autoUpdateSkipReason's proactive inspectHostConfigUnknowns
// check CANNOT see in advance: the container's HostConfig carries nothing a
// recreate would drop, but starting the freshly created replacement
// genuinely fails every cycle while failing is true. Used to keep exercising
// the sticky post-latch fallback (autoUpdateBlockStore) on its own, now that
// a host-config refusal is caught immediately by autoUpdateSkipReason itself
// (see TestAutoUpdateSkipReasonHostConfigRefusal).
func runOnceStartFailureStub(t *testing.T, failing, resolved *atomic.Bool) *dockerClient {
	t.Helper()
	containers := []dockerContainer{{
		ID: "c1", Names: []string{"/app"}, State: "running", Image: "ghcr.io/org/app:v1",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80", labelAutoUpdate: "true"},
	}}
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode(containers)
		case strings.Contains(r.URL.Path, "/containers/create"):
			json.NewEncoder(w).Encode(map[string]string{"Id": "new1"})
		case strings.HasSuffix(r.URL.Path, "/start"):
			if failing.Load() {
				http.Error(w, "start failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			json.NewEncoder(w).Encode(map[string]any{
				"Image":           "sha256:imgid",
				"HostConfig":      map[string]any{},
				"Config":          map[string]any{},
				"NetworkSettings": map[string]any{"Networks": map[string]any{}},
			})
		case strings.Contains(r.URL.Path, "/images/"):
			digest := "sha256:local"
			if resolved.Load() {
				digest = "sha256:registry"
			}
			json.NewEncoder(w).Encode(map[string]any{"RepoDigests": []string{"ghcr.io/org/app@" + digest}})
		case strings.Contains(r.URL.Path, "/distribution/"):
			json.NewEncoder(w).Encode(map[string]any{"Descriptor": map[string]any{"digest": "sha256:registry"}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

// TestRunOnceStickyBlockReason is the end-to-end proof for the gap the
// badminton peer session's second report surfaced: shouldAutoUpdate's own
// retry cap means runOnce goes completely silent about a service after
// autoUpdateMaxFailures consecutive failures — from the outside a
// permanently-stuck service becomes indistinguishable from a resolved one.
// Drives three real runOnce cycles against a container whose replace
// attempt genuinely fails every time (starting the replacement container,
// a failure autoUpdateSkipReason's own proactive host-config check cannot
// predict), then confirms buildManagedServices still surfaces WHY once
// autoUpdateSkipReason's own gate-based check goes blank, and that the
// reason clears again once the underlying problem is fixed and a replace
// finally succeeds.
func TestRunOnceStickyBlockReason(t *testing.T) {
	old := autoUpdateGap
	autoUpdateGap = 0
	t.Cleanup(func() { autoUpdateGap = old })

	var failing, resolved atomic.Bool
	failing.Store(true)
	dc := runOnceStartFailureStub(t, &failing, &resolved)

	onb := newTestOnboardedStore(t)
	ic := newImageChecker(dc)
	ctx := context.Background()
	ic.Check(ctx, "ghcr.io/org/app:v1")

	blocks := newAutoUpdateBlockStore()
	au := newAutoUpdater(dc, ic, onb, "", noopProxyStub(t), blocks, nil, nil)

	for i := 0; i < autoUpdateMaxFailures; i++ {
		au.runOnce(ctx)
	}

	reason := blocks.Get("app")
	if reason == "" {
		t.Fatal("blocks.Get(\"app\") empty after 3 consecutive failures, want a sticky reason")
	}
	if !strings.Contains(reason, "start failed") {
		t.Errorf("blocked reason = %q, want it to mention the start failure", reason)
	}

	svcs, err := buildManagedServices(ctx, dc, onb, ic, blocks)
	if err != nil {
		t.Fatalf("buildManagedServices: %v", err)
	}
	app := pickService(svcs, "app")
	if app == nil {
		t.Fatal("buildManagedServices did not return \"app\"")
	}
	want := "stopped retrying after 3 consecutive failures:"
	if !strings.Contains(app.AutoUpdateSkipReason, want) {
		t.Errorf("AutoUpdateSkipReason = %q, want it to contain %q", app.AutoUpdateSkipReason, want)
	}

	// Simulate the problem being fixed and the image manually pulled up to
	// date: the next image-checker cycle sees no difference left to update,
	// which is runOnce's actual (only) path to clearing a capped failure —
	// the cap itself forbids ever attempting another automatic retry.
	failing.Store(false)
	resolved.Store(true)
	ic.Check(ctx, "ghcr.io/org/app:v1")
	au.runOnce(ctx)
	if got := blocks.Get("app"); got != "" {
		t.Errorf("blocks.Get(\"app\") = %q after the update resolved, want empty", got)
	}
}

// TestRunOnceDefersOnActiveRollingReplaceWithoutCountingFailure proves
// runOnce's rom interlock (autoupdate.go): a service with an active rolling
// replace must be skipped without touching the failures counter or the
// sticky block store — it isn't the service's fault, and burning its
// failures budget here would let three coincidental rolling-replace ticks
// trip autoUpdateMaxFailures and permanently block the service for a reason
// that was never real. Uses a stub whose replace path always fails if
// reached, so a broken interlock would surface here as a nonzero failure
// count rather than silently passing.
func TestRunOnceDefersOnActiveRollingReplaceWithoutCountingFailure(t *testing.T) {
	old := autoUpdateGap
	autoUpdateGap = 0
	t.Cleanup(func() { autoUpdateGap = old })

	var failing, resolved atomic.Bool
	failing.Store(true)
	dc := runOnceStartFailureStub(t, &failing, &resolved)

	onb := newTestOnboardedStore(t)
	ic := newImageChecker(dc)
	ctx := context.Background()
	ic.Check(ctx, "ghcr.io/org/app:v1")

	rom := newRollingOpManager(dc)
	rom.ops["app"] = &rollingOpState{Service: "app", Status: rollingOpStatusRunning}

	blocks := newAutoUpdateBlockStore()
	au := newAutoUpdater(dc, ic, onb, "", noopProxyStub(t), blocks, nil, rom)

	au.runOnce(ctx)

	if n := au.failures["app"]; n != 0 {
		t.Errorf("failures[\"app\"] = %d, want 0 — an active rolling replace must defer, not fail", n)
	}
	if got := blocks.Get("app"); got != "" {
		t.Errorf("blocks.Get(\"app\") = %q, want empty — deferral must not latch a block reason", got)
	}
}

func TestSetAutoUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onboarded.json")
	store, err := loadOnboardedStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(OnboardedService{Name: "app", Host: "app.example.org", Image: "img", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAutoUpdate("app", true); err != nil {
		t.Fatal(err)
	}
	// Persists through a reload round-trip.
	store2, err := loadOnboardedStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := store2.Get("app")
	if !ok || !got.AutoUpdate {
		t.Fatalf("after reload AutoUpdate = %v (found=%v), want true", got.AutoUpdate, ok)
	}
	if err := store2.SetAutoUpdate("app", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := store2.Get("app"); got.AutoUpdate {
		t.Fatal("SetAutoUpdate(false) did not clear the flag")
	}
	if err := store2.SetAutoUpdate("nope", true); err == nil {
		t.Fatal("SetAutoUpdate on unknown name should error")
	}
}
