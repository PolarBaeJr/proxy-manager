package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withFastSync shrinks every propagation/health timer for one test. Call it
// FIRST in a test: t.Cleanup is LIFO, so the restore then runs after every
// syncHost's drain — restoring a var a still-running job reads is a race.
func withFastSync(t *testing.T) {
	t.Helper()
	oldSettle, oldReady, oldPoll := replaceSettleDelay, rollingReadyTimeout, canaryPromoteHealthPoll
	oldRetry, oldPeerPoll, oldPeerTimeout := envSyncRetryInterval, envSyncPeerPollInterval, envSyncPeerTimeout
	oldReconcile, oldForward := envSyncReconcileInterval, centralEnvForwardTimeout
	replaceSettleDelay = 0
	rollingReadyTimeout = 40 * time.Millisecond
	canaryPromoteHealthPoll = 5 * time.Millisecond
	envSyncRetryInterval = 10 * time.Millisecond
	envSyncPeerPollInterval = 10 * time.Millisecond
	envSyncPeerTimeout = 10 * time.Second
	envSyncReconcileInterval = time.Hour
	t.Cleanup(func() {
		replaceSettleDelay, rollingReadyTimeout, canaryPromoteHealthPoll = oldSettle, oldReady, oldPoll
		envSyncRetryInterval, envSyncPeerPollInterval, envSyncPeerTimeout = oldRetry, oldPeerPoll, oldPeerTimeout
		envSyncReconcileInterval, centralEnvForwardTimeout = oldReconcile, oldForward
	})
}

// newTestPeerRegistryFeatures is newTestPeerRegistry for a peer that
// advertises features (central-env/1, typically) under identity.
func newTestPeerRegistryFeatures(self, peerURL, identity string, writes bool, features []string) *PeerRegistry {
	reg := newPeerRegistry([]string{peerURL}, "s3cret", self, "dev", 0, nil)
	reg.recordResult(peerURL, true, identity, "dev", writes, features)
	return reg
}

// postCentralSpread is postSpread against a target advertising central env.
func postCentralSpread(t *testing.T, dc *dockerClient, target *httptest.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	reg := newTestPeerRegistryFeatures("dashboard-a", target.URL, "dashboard-b", true, []string{centralEnvFeature})
	mux := newLocalTestMux(t, dc, reg)
	req := httptest.NewRequest(http.MethodPost, "/api/services/app/spread", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// fakeProxyRefresh stands in for the proxy's /refresh — the real default
// URL is a live proxy on the machine running the tests.
func fakeProxyRefresh(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/refresh" {
			n.Add(1)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// syncHost is one dashboard's central-env side, wired like main.go does it,
// against a cenvFakeDocker.
type syncHost struct {
	f         *cenvFakeDocker
	dc        *dockerClient
	ce        *centralEnv
	m         *envSyncManager
	rm        *rolloutManager
	rom       *rollingOpManager
	refreshes *atomic.Int32
}

func newSyncHost(t *testing.T, identity string, reg *PeerRegistry, secret string) *syncHost {
	t.Helper()
	f := newCenvFakeDocker()
	dc := f.client(t)
	ce := newTestCentralEnv(t, identity)
	dc.central = ce
	proxyURL, n := fakeProxyRefresh(t)
	rm := newRolloutManager(dc, nil, "", proxyURL)
	rom := newRollingOpManager(dc)
	m := newEnvSyncManager(dc, ce, rm, rom, reg, secret, proxyURL)
	ce.sync = m
	h := &syncHost{f: f, dc: dc, ce: ce, m: m, rm: rm, rom: rom, refreshes: n}
	t.Cleanup(func() { h.drain(t) })
	return h
}

// drain waits for every job this host's manager started to finish.
func (h *syncHost) drain(t *testing.T) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		h.m.mu.Lock()
		n := len(h.m.running)
		h.m.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("%s: sync jobs still running at cleanup", h.ce.identity)
}

// waitJob waits until svc's job is finished and nothing more is queued.
func (h *syncHost) waitJob(t *testing.T, svc string) *envSyncJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !h.m.busy(svc) {
			if j, ok := h.m.get(svc); ok && envSyncTerminal(j.Status) {
				return j
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	j, _ := h.m.get(svc)
	t.Fatalf("%s: job for %s never finished: %+v", h.ce.identity, svc, j)
	return nil
}

// seedMember adds one stamped live "app" replica.
func (f *cenvFakeDocker) seedMember(id, name, origin string, version uint64, env []string, health *healthcheckSpec) {
	labels := map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080", labelSpread: "true"}
	if origin != "" {
		labels[labelEnvOrigin] = origin
		labels[labelEnvVersion] = strconv.FormatUint(version, 10)
	}
	f.seed(dockerContainer{ID: id, Names: []string{"/" + name}, Image: "ghcr.io/org/app:v1", State: "running", Status: "Up 1 hour", Labels: labels},
		cenvInspect{env: env, health: health, edge: []string{name}, networks: map[string][]string{}})
}

// members is every live "app" container: name -> (version label, env).
type memberView struct {
	id      string
	version string
	env     []string
}

func (f *cenvFakeDocker) members() map[string]memberView {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]memberView{}
	for id, c := range f.items {
		if c.Labels[labelService] != "app" || c.Labels[labelCanary] == "true" {
			continue
		}
		out[c.name()] = memberView{id: id, version: c.Labels[labelEnvVersion], env: f.inspect[id].env}
	}
	return out
}

func envHas(env []string, entry string) bool {
	for _, e := range env {
		if e == entry {
			return true
		}
	}
	return false
}

func (f *cenvFakeDocker) setUnhealthy(fn func(createBody) bool) {
	f.mu.Lock()
	f.unhealthy = fn
	f.mu.Unlock()
}

func TestEnvSyncOriginRollsEveryStaleMember(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-a", nil, "")
	h.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	h.f.seedMember("m1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)
	h.f.seedMember("m2", "goproxy-app-2", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)

	res, err := applyCentralEnvSet(h.ce, "app", centralEnvSetRequest{IfVersion: 1, Set: map[string]string{"A": "2"}}, "alice")
	if err != nil || res.Version != 2 || strings.Join(res.ChangedKeys, ",") != "A" {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	j := h.waitJob(t, "app")
	if j.Status != envSyncStatusConverged || j.Target != 2 || j.Role != envSyncRoleOrigin {
		t.Fatalf("job = %+v", j)
	}
	ms := h.f.members()
	if len(ms) != 2 {
		t.Fatalf("members = %+v", ms)
	}
	for n, mv := range ms {
		if mv.version != "2" || !envHas(mv.env, "A=2") {
			t.Fatalf("%s = %+v, want v2 with A=2", n, mv)
		}
	}
	for _, c := range h.f.createsSnapshot() {
		if c.body.Labels[labelEnvOrigin] != "dashboard-a" || !reflect2(c.body.Healthcheck, cenvTemplateHealth) {
			t.Fatalf("create %s labels %v hc %+v", c.name, c.body.Labels, c.body.Healthcheck)
		}
	}
	if h.refreshes.Load() == 0 {
		t.Fatal("proxy never refreshed after the roll")
	}
	// Idempotent: a second request with everything at v2 creates nothing.
	before := len(h.f.createsSnapshot())
	h.m.request("app")
	h.waitJob(t, "app")
	if len(h.f.createsSnapshot()) != before {
		t.Fatal("a converged service was rolled again")
	}
}

func reflect2(a, b *healthcheckSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	return strings.Join(a.Test, " ") == strings.Join(b.Test, " ") && a.Interval == b.Interval && a.Retries == b.Retries
}

// TestEnvSyncForeignMemberNeverTouched: an unstamped member (a compose
// original) beside a stale stamped one is warned about and left running.
func TestEnvSyncForeignMemberNeverTouched(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-a", nil, "")
	h.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	h.f.seedTemplate(nil) // "app", compose-created, no pmgr.env.* stamp
	h.f.seedMember("m1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)

	applyCentralEnvSet(h.ce, "app", centralEnvSetRequest{IfVersion: 1, Set: map[string]string{"A": "2"}}, "alice")
	j := h.waitJob(t, "app")
	if j.Status != envSyncStatusConverged {
		t.Fatalf("job = %+v", j)
	}
	ms := h.f.members()
	tpl, ok := ms["app"]
	if !ok || tpl.id != "tpl1" || !envHas(tpl.env, "A=1") {
		t.Fatalf("foreign member was touched: %+v", ms)
	}
	if _, ok := ms["goproxy-app-1"]; ok {
		t.Fatal("stale stamped member was not replaced")
	}
	if len(j.Warnings) == 0 || !strings.Contains(strings.Join(j.Warnings, " "), "foreign") {
		t.Fatalf("no foreign-member warning: %v", j.Warnings)
	}
	for _, c := range h.f.createsSnapshot() {
		if c.body.Labels["com.docker.compose.project"] != "" {
			t.Fatalf("a created replica carries compose labels: %v", c.body.Labels)
		}
	}
}

// TestEnvSyncOriginGateFailureReverts: the new version fails its health gate
// on the origin → the store reverts (a new version with the old content) and
// the service ends up running that, never the bad env.
func TestEnvSyncOriginGateFailureReverts(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-a", nil, "")
	h.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	h.f.seedMember("m1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)
	h.f.setUnhealthy(func(b createBody) bool { return envHas(b.Env, "A=bad") })

	applyCentralEnvSet(h.ce, "app", centralEnvSetRequest{IfVersion: 1, Set: map[string]string{"A": "bad"}}, "alice")
	h.waitJob(t, "app")
	rec, _, _ := h.ce.store.Get("app")
	if rec.Version != 3 || rec.State != centralEnvStateReverted || rec.Base["A"] != "1" {
		t.Fatalf("store = v%d %s %v", rec.Version, rec.State, rec.Base)
	}
	for n, mv := range h.f.members() {
		if envHas(mv.env, "A=bad") || mv.version != "3" {
			t.Fatalf("%s = %+v", n, mv)
		}
	}
}

// TestEnvSyncOriginRevertFailureDegrades: when the revert itself fails its
// gate the record goes degraded and nothing further is attempted — the
// original replica keeps serving.
func TestEnvSyncOriginRevertFailureDegrades(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-a", nil, "")
	h.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	h.f.seedMember("m1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)
	h.f.setUnhealthy(func(createBody) bool { return true })

	applyCentralEnvSet(h.ce, "app", centralEnvSetRequest{IfVersion: 1, Set: map[string]string{"A": "2"}}, "alice")
	j := h.waitJob(t, "app")
	rec, _, _ := h.ce.store.Get("app")
	if rec.State != centralEnvStateDegraded || j.Status != envSyncStatusDegraded {
		t.Fatalf("store state %s, job %+v", rec.State, j)
	}
	ms := h.f.members()
	if len(ms) != 1 || ms["goproxy-app-1"].id != "m1" {
		t.Fatalf("members = %+v", ms)
	}
	// Degraded stops automatic attempts: a request is a no-op.
	n := len(h.f.createsSnapshot())
	h.m.request("app")
	h.waitJob(t, "app")
	if len(h.f.createsSnapshot()) != n {
		t.Fatal("a degraded service was rolled again")
	}
}

// TestEnvSyncDefersToActiveRolloutAndSkipsIntermediate: while a canary
// rollout owns the service the job stays pending (never failed), and the
// versions applied meanwhile collapse into one roll of the newest.
func TestEnvSyncDefersToActiveRolloutAndSkipsIntermediate(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-a", nil, "")
	h.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	h.f.seedMember("m1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)
	h.rm.mu.Lock()
	h.rm.rollouts["app"] = &rolloutState{Service: "app", Status: rolloutStatusAwaitingAdvance}
	h.rm.mu.Unlock()

	for v := uint64(1); v <= 3; v++ {
		if _, err := applyCentralEnvSet(h.ce, "app", centralEnvSetRequest{IfVersion: v, Set: map[string]string{"A": strconv.FormatUint(v+1, 10)}}, "alice"); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(60 * time.Millisecond)
	if j, _ := h.m.get("app"); j == nil || j.Status != envSyncStatusPending {
		t.Fatalf("job while a rollout is active = %+v", j)
	}
	if n := len(h.f.createsSnapshot()); n != 0 {
		t.Fatalf("created %d while a rollout was active", n)
	}
	h.rm.mu.Lock()
	h.rm.rollouts["app"].Status = rolloutStatusCompleted
	h.rm.mu.Unlock()
	j := h.waitJob(t, "app")
	if j.Status != envSyncStatusConverged || j.Target != 4 {
		t.Fatalf("job = %+v", j)
	}
	for _, c := range h.f.createsSnapshot() {
		if !envHas(c.body.Env, "A=4") {
			t.Fatalf("an intermediate version was rolled: %v", c.body.Env)
		}
	}
}

// TestEnvSyncReconcileSecretRotation: a referenced secret changing under an
// unchanged record is noticed by reconcile, bumped as its own version, and
// rolled.
func TestEnvSyncReconcileSecretRotation(t *testing.T) {
	withFastSync(t)
	h := newSyncHost(t, "dashboard-a", nil, "")
	h.ce.secrets = newTestSecrets(t, "app", "TOKEN=one-rotated-away")
	h.ce.store.Create("app", "dashboard-a", map[string]string{"TOKEN": "ref:TOKEN"}, nil, "test", "")
	h.f.seedMember("m1", "goproxy-app-1", "dashboard-a", 1, []string{"TOKEN=one-rotated-away"}, cenvTemplateHealth)

	ctx := context.Background()
	h.m.reconcileOnce(ctx)
	if h.m.busy("app") || len(h.f.createsSnapshot()) != 0 {
		t.Fatal("reconcile rolled a converged service")
	}
	if err := os.WriteFile(filepath.Join(h.ce.secrets.dir, "app.env"), []byte("TOKEN=two-the-new-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.m.reconcileOnce(ctx)
	j := h.waitJob(t, "app")
	rec, _, _ := h.ce.store.Get("app")
	if rec.Version != 2 || rec.UpdatedBy != "secret-rotation" || j.Status != envSyncStatusConverged {
		t.Fatalf("store v%d by %q, job %+v", rec.Version, rec.UpdatedBy, j)
	}
	for n, mv := range h.f.members() {
		if !envHas(mv.env, "TOKEN=two-the-new-one") || mv.version != "2" {
			t.Fatalf("%s = %+v", n, mv)
		}
	}
	// Steady state again: no further bump.
	h.m.reconcileOnce(ctx)
	h.waitJob(t, "app")
	if rec, _, _ := h.ce.store.Get("app"); rec.Version != 2 {
		t.Fatalf("version moved to %d with nothing rotated", rec.Version)
	}
}

// TestEnvSyncGetIsDeepCopy: mutating a returned job never reaches the
// manager's copy.
func TestEnvSyncGetIsDeepCopy(t *testing.T) {
	m := newEnvSyncManager(nil, newTestCentralEnv(t, "a"), nil, nil, nil, "", "")
	m.begin("app", envSyncRoleOrigin, 1)
	m.addHost("app", envSyncHostResult{Host: "a", Status: envSyncHostConverged})
	m.addWarnings("app", []string{"w"})
	j, _ := m.get("app")
	j.Hosts[0].Status = "mutated"
	j.Warnings[0] = "mutated"
	j2, _ := m.get("app")
	if j2.Hosts[0].Status != envSyncHostConverged || j2.Warnings[0] != "w" {
		t.Fatalf("get shares state: %+v", j2)
	}
}

// TestRollingStartWithPinnedEnv: rom.startWith rolls exactly the pinned env
// and labels (no env merge, no Resolve), refuses Env edits alongside a pin,
// and reports through done.
func TestRollingStartWithPinnedEnv(t *testing.T) {
	withFastSync(t)
	f := newCenvFakeDocker()
	f.seedMember("m1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1", "LOCAL=x"}, nil)
	dc := f.client(t)
	rom := newRollingOpManager(dc)
	done := make(chan error, 1)
	_, err := rom.startWith("app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v1"}, rollingOpts{
		pinnedEnv: []string{"A=2"}, pinnedLabels: map[string]string{labelEnvOrigin: "dashboard-a", labelEnvVersion: "2"},
		pinnedHealthcheck: cenvTemplateHealth, removeOnGateFailure: true, skipPull: true, done: done,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c := f.createsSnapshot()
	if len(c) != 1 || strings.Join(c[0].body.Env, ",") != "A=2" || c[0].body.Labels[labelEnvVersion] != "2" || !reflect2(c[0].body.Healthcheck, cenvTemplateHealth) {
		t.Fatalf("create = %+v", c)
	}

	done = make(chan error, 1)
	rom.startWith("app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v1", Env: map[string]string{"A": "3"}}, rollingOpts{pinnedEnv: []string{"A=2"}, done: done})
	if err := <-done; err == nil || !strings.Contains(err.Error(), "centrally managed") {
		t.Fatalf("pinned + Env edits = %v", err)
	}
}
