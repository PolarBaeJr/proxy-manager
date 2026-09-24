package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// ---- auto-update vs central env propagation ----

// newAutoUpdateHost is an origin owning "app" (A=1) with two opted-in
// replicas running image sha256:old, a tag that a pull moves to
// sha256:new, an image checker that says an update is available, and an
// auto-updater wired to the same rm/rom as the propagation manager.
func newAutoUpdateHost(t *testing.T) (*syncHost, http.Handler, *autoUpdater) {
	t.Helper()
	oldGap := autoUpdateGap
	autoUpdateGap = 0
	t.Cleanup(func() { autoUpdateGap = oldGap })
	h, mux := newAPIHost(t, map[string]string{"A": "1"})
	h.f.seedMember("m2", "goproxy-app-2", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)
	h.f.mu.Lock()
	for id, c := range h.f.items {
		c.Labels[labelAutoUpdate] = "true"
		c.ImageID = "sha256:old"
		h.f.items[id] = c
	}
	h.f.imageID = "sha256:old"
	h.f.pullTo = "sha256:new"
	h.f.mu.Unlock()
	ic := newImageChecker(h.dc)
	ic.statuses["ghcr.io/org/app:v1"] = &imageStatus{Image: "ghcr.io/org/app:v1", UpdateAvailable: true}
	au := newAutoUpdater(h.dc, ic, newTestOnboardedStore(t), "", h.rm.proxyURL, nil, h.rm, h.rom)
	return h, mux, au
}

// claimLog records, for every create, who held the service's claim.
type claimLog struct {
	mu      sync.Mutex
	holders []string
}

func (l *claimLog) hook(h *syncHost) func(string) {
	return func(string) {
		who := h.dc.claims.holder("app")
		l.mu.Lock()
		l.holders = append(l.holders, who)
		l.mu.Unlock()
	}
}

// check: every create happened under a claim, and the two mutators never
// interleaved — at most one hand-over between contiguous runs.
func (l *claimLog) check(t *testing.T) []string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var runs []string
	for i, who := range l.holders {
		if who == "" {
			t.Fatalf("create %d happened with nobody holding the claim: %v", i, l.holders)
		}
		if len(runs) == 0 || runs[len(runs)-1] != who {
			runs = append(runs, who)
		}
	}
	if len(runs) > 2 {
		t.Fatalf("auto-update and propagation interleaved: %v", l.holders)
	}
	return runs
}

func assertNewImageAndVersion(t *testing.T, h *syncHost, version string) {
	t.Helper()
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	n := 0
	for _, c := range h.f.items {
		if c.Labels[labelService] != "app" {
			continue
		}
		n++
		if c.ImageID != "sha256:new" || c.Labels[labelEnvVersion] != version || !envHas(h.f.inspect[c.ID].env, "A=2") {
			t.Fatalf("%s: image %s env v%s %v — want sha256:new, v%s, A=2", c.name(), c.ImageID, c.Labels[labelEnvVersion], h.f.inspect[c.ID].env, version)
		}
	}
	if n != 2 {
		t.Fatalf("%d replicas, want 2", n)
	}
}

// TestAutoUpdateThenEnvJobSerialize: an auto-update replace is mid-flight
// when an env edit lands — the propagation job waits for it (pending),
// then rolls; the end state is the new image AND the new env version.
func TestAutoUpdateThenEnvJobSerialize(t *testing.T) {
	withFastSync(t)
	h, mux, au := newAutoUpdateHost(t)
	var log claimLog
	record := log.hook(h)
	created, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.f.onCreate = func(name string) {
		record(name)
		once.Do(func() {
			close(created)
			<-release
		})
	}
	auDone := make(chan struct{})
	go func() {
		au.runOnce(context.Background())
		close(auDone)
	}()
	<-created
	if rec := apiDo(t, mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"2"}}`); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	waitFor(t, "the env job to wait for the auto-update", func() bool {
		j, ok := h.m.get("app")
		return ok && j.Status == envSyncStatusPending
	})
	close(release)
	<-auDone
	j := h.waitJob(t, "app")
	if j.Status != envSyncStatusConverged {
		t.Fatalf("job = %+v", j)
	}
	if runs := log.check(t); runs[0] != autoUpdateClaimOwner {
		t.Fatalf("claim runs = %v, want the auto-update first", runs)
	}
	if au.failures["app"] != 0 {
		t.Fatalf("auto-update failures = %d", au.failures["app"])
	}
	assertNewImageAndVersion(t, h, "2")
}

// TestEnvJobThenAutoUpdateDefers: the auto-updater finds a propagation job
// holding the service (between/within its rolls, where rom alone can look
// idle) and defers without counting a failure; its next tick updates.
func TestEnvJobThenAutoUpdateDefers(t *testing.T) {
	withFastSync(t)
	h, mux, au := newAutoUpdateHost(t)
	var log claimLog
	record := log.hook(h)
	created, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.f.onCreate = func(name string) {
		record(name)
		once.Do(func() {
			close(created)
			<-release
		})
	}
	if rec := apiDo(t, mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"2"}}`); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	<-created
	before := len(h.f.createsSnapshot())
	au.runOnce(context.Background())
	if n := len(h.f.createsSnapshot()); n != before || h.f.pulls != 0 || au.failures["app"] != 0 {
		t.Fatalf("auto-update ran over a propagation job: creates %d->%d pulls %d failures %d", before, n, h.f.pulls, au.failures["app"])
	}
	close(release)
	h.waitJob(t, "app")
	au.runOnce(context.Background())
	if runs := log.check(t); len(runs) != 2 || runs[0] != envSyncClaimOwner || runs[1] != autoUpdateClaimOwner {
		t.Fatalf("claim runs = %v, want propagation then auto-update", runs)
	}
	assertNewImageAndVersion(t, h, "2")
}

// TestAutoUpdateDefersToClaimAlone covers the window the rom check can't
// see — the propagation job between waitIdle and its rolling replace, or
// between two of them, when rom is idle but the job owns the service.
func TestAutoUpdateDefersToClaimAlone(t *testing.T) {
	withFastSync(t)
	h, _, au := newAutoUpdateHost(t)
	if !h.dc.claims.tryClaim("app", envSyncClaimOwner) {
		t.Fatal("claim")
	}
	au.runOnce(context.Background())
	if n := len(h.f.createsSnapshot()); n != 0 || h.f.pulls != 0 || au.failures["app"] != 0 {
		t.Fatalf("auto-update ignored a held claim: creates %d pulls %d failures %d", n, h.f.pulls, au.failures["app"])
	}
	h.dc.claims.release("app", envSyncClaimOwner)
	au.runOnce(context.Background())
	if n := len(h.f.createsSnapshot()); n != 2 {
		t.Fatalf("auto-update after release created %d", n)
	}
	if who := h.dc.claims.holder("app"); who != "" {
		t.Fatalf("auto-update left the claim held by %q", who)
	}
}

// TestAutoUpdateAndEnvJobConcurrent starts both at the same instant, with
// no staging: whichever order they land in, they never interleave, nobody
// fails, and one more auto-update tick (a no-op if it already ran) leaves
// the new image and the new env version everywhere. Meant for -race.
func TestAutoUpdateAndEnvJobConcurrent(t *testing.T) {
	withFastSync(t)
	h, mux, au := newAutoUpdateHost(t)
	var log claimLog
	h.f.onCreate = log.hook(h)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		au.runOnce(context.Background())
	}()
	go func() {
		defer wg.Done()
		<-start
		if rec := apiDo(t, mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"2"}}`); rec.Code != http.StatusOK {
			t.Errorf("set = %d %s", rec.Code, rec.Body.String())
		}
	}()
	close(start)
	wg.Wait()
	h.waitJob(t, "app")
	au.runOnce(context.Background())
	h.waitJob(t, "app")
	log.check(t)
	if n := len(h.f.createsSnapshot()); n > 4 {
		t.Fatalf("%d creates for 2 replicas × 2 changes", n)
	}
	if au.failures["app"] != 0 {
		t.Fatalf("auto-update failures = %d", au.failures["app"])
	}
	assertNewImageAndVersion(t, h, "2")
}

// TestEnvJobRecreatesOnConfigImageWithoutPull: the propagation job
// recreates on the template's Config.Image (the reference it was created
// from — not the list's Image, which decays to a digest), never pulls, and
// says so when that tag has been pulled forward but not applied.
func TestEnvJobRecreatesOnConfigImageWithoutPull(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-a", nil, "")
	h.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	h.f.seed(dockerContainer{ID: "m1", Names: []string{"/goproxy-app-1"}, Image: "sha256:0123abcd", ImageID: "sha256:old", State: "running", Status: "Up 1 hour",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080", labelEnvOrigin: "dashboard-a", labelEnvVersion: "1"}},
		cenvInspect{configImage: "ghcr.io/org/app:stable", env: []string{"A=1"}, health: cenvTemplateHealth, edge: []string{"goproxy-app-1"}, networks: map[string][]string{}})
	h.f.mu.Lock()
	h.f.imageID = "sha256:pulled-not-applied"
	h.f.mu.Unlock()

	if _, err := applyCentralEnvSet(h.ce, "app", centralEnvSetRequest{IfVersion: 1, Set: map[string]string{"A": "2"}}, "alice"); err != nil {
		t.Fatal(err)
	}
	j := h.waitJob(t, "app")
	if j.Status != envSyncStatusConverged {
		t.Fatalf("job = %+v", j)
	}
	creates := h.f.createsSnapshot()
	if len(creates) == 0 {
		t.Fatal("nothing recreated")
	}
	for _, c := range creates {
		if c.body.Image != "ghcr.io/org/app:stable" {
			t.Fatalf("%s created from %q, want the template's Config.Image", c.name, c.body.Image)
		}
	}
	if h.f.pulls != 0 {
		t.Fatalf("propagation pulled %d times", h.f.pulls)
	}
	if !strings.Contains(strings.Join(j.Warnings, " "), "newer pulled image") {
		t.Fatalf("no moved-tag warning: %v", j.Warnings)
	}
}

// ---- failed version on a peer ----

func TestCentralEnvCacheFailedVersion(t *testing.T) {
	dir := t.TempDir()
	c, _ := loadCentralEnvCache(dir)
	c.Put("app", "o", 1, []string{"A=1"})
	c.Put("app", "o", 2, []string{"A=2"})
	if err := c.RollBack("app", 2, []string{"A=1"}, 1); err != nil {
		t.Fatal(err)
	}
	e, _ := c.Get("app")
	if e.Version != 1 || e.Env[0] != "A=1" || e.FailedVersion != 2 || !e.usable() {
		t.Fatalf("after rollback = %+v", e)
	}
	var failed errCentralEnvCacheFailed
	if err := c.Put("app", "o", 2, []string{"A=2"}); !errors.As(err, &failed) {
		t.Fatalf("re-Put of the failed version = %v", err)
	}
	if err := c.Put("app", "o", 1, []string{"A=1"}); err != nil {
		t.Fatalf("refresh of the rolled-back version = %v", err)
	}
	// Survives a restart.
	c2, _ := loadCentralEnvCache(dir)
	if e, _ := c2.Get("app"); e.FailedVersion != 2 || e.Version != 1 {
		t.Fatalf("reloaded = %+v", e)
	}
	// A newer version clears it.
	if err := c.Put("app", "o", 3, []string{"A=3"}); err != nil {
		t.Fatal(err)
	}
	if e, _ := c.Get("app"); e.FailedVersion != 0 || e.PrevVersion != 1 {
		t.Fatalf("after newer = %+v", e)
	}
	// Nothing to roll back to: the current entry is unusable, and a
	// same-version refresh can't launder it.
	c.RollBack("app", 3, nil, 0)
	if e, _ := c.Get("app"); e.usable() {
		t.Fatalf("failed current entry still usable: %+v", e)
	}
	if err := c.Put("app", "o", 3, []string{"A=3"}); !errors.As(err, &failed) {
		t.Fatalf("same-version refresh of a failed entry = %v", err)
	}
	c.ClearFailed("app")
	if err := c.Put("app", "o", 3, []string{"A=3"}); err != nil {
		t.Fatalf("after ClearFailed = %v", err)
	}
}

// TestResolveNeverRecreatesAFailedVersion: with the origin still serving
// the version this host failed, Resolve uses the rolled-back copy; with no
// earlier copy it refuses rather than recreate the failed env.
func TestResolveNeverRecreatesAFailedVersion(t *testing.T) {
	ce := newTestCentralEnv(t, "dashboard-b")
	ce.fetchFromOrigin = func(context.Context, string, string, string) (centralEnvFetched, error) {
		return centralEnvFetched{Version: 2, Env: []string{"A=bad"}}, nil
	}
	ce.cache.Put("app", "dashboard-a", 1, []string{"A=1"})
	ce.cache.Put("app", "dashboard-a", 2, []string{"A=bad"})
	ce.cache.RollBack("app", 2, []string{"A=1"}, 1)
	res, managed, err := ce.Resolve(context.Background(), "app", nil)
	if err != nil || !managed || res.Version != 1 || res.Env[0] != "A=1" || res.Stale || !strings.Contains(res.Warning, "failed its health gate") {
		t.Fatalf("Resolve = %+v, %v", res, err)
	}
	// A host whose first-ever version (v2) failed: nothing to fall back to.
	ce.cache.Delete("app")
	ce.cache.Put("app", "dashboard-a", 2, []string{"A=bad"})
	ce.cache.RollBack("app", 2, nil, 0)
	var here errCentralEnvFailedHere
	if _, _, err := ce.Resolve(context.Background(), "app", nil); !errors.As(err, &here) {
		t.Fatalf("Resolve with only a failed copy = %v", err)
	}
	ce.fetchFromOrigin = nil
	if _, _, err := ce.Resolve(context.Background(), "app", nil); !errors.As(err, &here) {
		t.Fatalf("offline Resolve with only a failed copy = %v", err)
	}
}
