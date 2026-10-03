package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fakes ----

// abFakeProxy stands in for the proxy's metrics port: /ab (filtered by
// ?service= like the real one) and /refresh.
type abFakeProxy struct {
	mu        sync.Mutex
	exps      []abProxyExperiment
	down      bool
	refreshes atomic.Int32
	srv       *httptest.Server
}

func newABFakeProxy(t *testing.T) *abFakeProxy {
	t.Helper()
	p := &abFakeProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/refresh":
			p.refreshes.Add(1)
		case "/ab":
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.down {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			rep := abProxyReport{HistBoundsMs: abHistBoundsDefault, Experiments: []abProxyExperiment{}}
			for _, e := range p.exps {
				if svc := r.URL.Query().Get("service"); svc == "" || svc == e.Service {
					rep.Experiments = append(rep.Experiments, e)
				}
			}
			json.NewEncoder(w).Encode(rep)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *abFakeProxy) set(exps ...abProxyExperiment) {
	p.mu.Lock()
	p.exps = exps
	p.mu.Unlock()
}

func (p *abFakeProxy) setDown(down bool) {
	p.mu.Lock()
	p.down = down
	p.mu.Unlock()
}

type abClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *abClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *abClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type abHarness struct {
	f     *cenvFakeDocker
	dc    *dockerClient
	px    *abFakeProxy
	m     *abManager
	clock *abClock
	path  string
}

// withFastAB shrinks the health gates every A/B op runs through. Call it
// first (see withFastSync).
func withFastAB(t *testing.T) {
	t.Helper()
	withFastSync(t)
	old := canaryPromoteHealthTimeout
	canaryPromoteHealthTimeout = 50 * time.Millisecond
	t.Cleanup(func() { canaryPromoteHealthTimeout = old })
}

// newABHarness: a dashboard host running "app" (the cenv template, env
// A=1/SECRET=tpl) with an A/B manager against a fake proxy.
func newABHarness(t *testing.T, reg *PeerRegistry, secret string) *abHarness {
	t.Helper()
	f := newCenvFakeDocker()
	f.seedTemplate(nil)
	return newABHarnessOn(t, f, f.client(t), reg, secret, filepath.Join(t.TempDir(), "abtests.json"))
}

func newABHarnessOn(t *testing.T, f *cenvFakeDocker, dc *dockerClient, reg *PeerRegistry, secret, path string) *abHarness {
	t.Helper()
	px := newABFakeProxy(t)
	store, err := loadABStore(path)
	if err != nil {
		t.Fatal(err)
	}
	clock := &abClock{t: time.Unix(1_800_000_000, 0)}
	m := newABManager(dc, store, nil, nil, nil, reg, secret, px.srv.URL)
	m.now = clock.now
	dc.ab = m
	return &abHarness{f: f, dc: dc, px: px, m: m, clock: clock, path: path}
}

// bReplicas is every container carrying A/B labels.
func (h *abHarness) bReplicas() []dockerContainer {
	var out []dockerContainer
	for _, c := range h.f.list() {
		if c.Labels[labelABVariant] != "" {
			out = append(out, c)
		}
	}
	return out
}

// seedB adds a B replica for "app" with exactly rec's labels, plus a
// matching record (unless noRecord).
func (h *abHarness) seedB(t *testing.T, id string, rec *abRecord, noRecord bool) {
	t.Helper()
	labels := map[string]string{
		labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080",
		labelCanary: "true", labelPrevImage: "ghcr.io/org/app:v1",
	}
	for k, v := range rec.labels() {
		labels[k] = v
	}
	h.f.seed(dockerContainer{ID: id, Names: []string{"/goproxy-app-canary-000001"}, Image: rec.Image, State: "running", Status: "Up 1 minute", Labels: labels},
		cenvInspect{env: []string{"A=1", "SECRET=b-sentinel-value"}, health: cenvTemplateHealth, edge: []string{"goproxy-app-canary-000001"}, networks: map[string][]string{}})
	if !noRecord {
		if err := h.m.store.put("app", rec); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *abHarness) record(t *testing.T) *abRecord {
	t.Helper()
	rec, ok := h.m.store.get("app")
	if !ok {
		t.Fatal("no A/B record for app")
	}
	return rec
}

// exp is a proxy experiment for rec with the given pins.
func abExp(rec *abRecord, lastA, lastB int64) abProxyExperiment {
	return abProxyExperiment{
		Service: "app", ID: rec.ID, Phase: rec.Phase, PhaseAt: rec.PhaseAt, TrackingSince: rec.Started - 86400,
		Pinned: abProxyPair[abProxyPinned]{A: abProxyPinned{ActiveSessions: 3, LastSeen: lastA}, B: abProxyPinned{ActiveSessions: 2, LastSeen: lastB}},
	}
}

func abTestRecord(now time.Time) *abRecord {
	t := now.Unix()
	return &abRecord{ID: "feedf00d", Image: "ghcr.io/org/app:v2", Replicas: 1, Created: t, Started: t, Epoch: t,
		Phase: abPhaseRunning, PhaseAt: t, Config: map[string]string{labelABSplit: "20"}}
}

func wantABCode(t *testing.T, err error, code int) {
	t.Helper()
	if err == nil || abErrStatus(err) != code {
		t.Fatalf("err = %v, want %d", err, code)
	}
}

// ---- start ----

func TestABStartHappyPath(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	rec, err := h.m.Start(context.Background(), "app", abStartRequest{Image: "ghcr.io/org/app:v2", Replicas: 2, Split: abIntP(20)}, false)
	if err != nil {
		t.Fatal(err)
	}
	now := strconv.FormatInt(h.clock.now().Unix(), 10)
	bs := h.bReplicas()
	if len(bs) != 2 {
		t.Fatalf("B replicas = %d, want 2", len(bs))
	}
	for _, b := range bs {
		l := b.Labels
		if b.Image != "ghcr.io/org/app:v2" || l[labelCanary] != "true" || l[labelABVariant] != "B" || l[labelABID] != rec.ID ||
			l[labelABPhase] != abPhaseRunning || l[labelABSplit] != "20" || l[labelABStarted] != now || l[labelABEpoch] != now || l[labelABPhaseAt] != now {
			t.Fatalf("B %s labels = %v", b.name(), l)
		}
		if !strings.HasPrefix(b.name(), "goproxy-app-canary-0000") {
			t.Fatalf("B name %q is not fixed-width", b.name())
		}
	}
	got := h.record(t)
	if got.Op != "" || got.LastError != "" || got.ID != rec.ID || !abIDRe.MatchString(got.ID) {
		t.Fatalf("record after start = %+v", got)
	}
	if h.px.refreshes.Load() == 0 {
		t.Fatal("proxy never refreshed after start")
	}
	// Persisted: a fresh store sees it.
	s2, _ := loadABStore(h.path)
	if r2, ok := s2.get("app"); !ok || r2.ID != rec.ID {
		t.Fatal("start was not persisted")
	}
	// A second start is refused while this one exists.
	_, err = h.m.Start(context.Background(), "app", abStartRequest{Image: "x:3", Replicas: 1}, false)
	wantABCode(t, err, http.StatusConflict)
}

func TestABStartRefusals(t *testing.T) {
	withFastAB(t)
	req := abStartRequest{Image: "ghcr.io/org/app:v2", Replicas: 1}

	h := newABHarness(t, nil, "")
	_, err := h.m.Start(context.Background(), "ghost", req, false)
	wantABCode(t, err, http.StatusNotFound)

	_, err = h.m.Start(context.Background(), "app", abStartRequest{Image: "x", Replicas: 11}, false)
	wantABCode(t, err, http.StatusBadRequest)

	// A plain canary already staged.
	h.f.seedCanary("ghcr.io/org/app:v9", nil)
	_, err = h.m.Start(context.Background(), "app", req, false)
	wantABCode(t, err, http.StatusConflict)

	// The proxy already lists a test for app (B on a peer).
	h2 := newABHarness(t, nil, "")
	h2.px.set(abProxyExperiment{Service: "app", ID: "peer0001"})
	_, err = h2.m.Start(context.Background(), "app", req, false)
	wantABCode(t, err, http.StatusConflict)

	// Onboarded services are out of scope in v1.
	h3 := newABHarness(t, nil, "")
	onb := newTestOnboardedStore(t)
	if err := onb.Put(OnboardedService{Name: "app", Image: "ghcr.io/org/app:v1"}); err != nil {
		t.Fatal(err)
	}
	h3.m.onb = onb
	_, err = h3.m.Start(context.Background(), "app", req, false)
	wantABCode(t, err, http.StatusBadRequest)

	// An active rolling replace.
	h4 := newABHarness(t, nil, "")
	h4.m.rom = newRollingOpManager(h4.dc)
	h4.m.rom.mu.Lock()
	h4.m.rom.ops["app"] = &rollingOpState{Service: "app", Status: rollingOpStatusRunning}
	h4.m.rom.mu.Unlock()
	_, err = h4.m.Start(context.Background(), "app", req, false)
	wantABCode(t, err, http.StatusConflict)

	for _, hh := range []*abHarness{h, h2, h3, h4} {
		if _, ok := hh.m.store.get("app"); ok {
			t.Fatal("a refused start left a record behind")
		}
	}
}

func TestABStartFailedHealthGateRemovesB(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	h.f.setUnhealthy(func(b createBody) bool { return b.Labels[labelABVariant] != "" })
	_, err := h.m.Start(context.Background(), "app", abStartRequest{Image: "ghcr.io/org/app:v2", Replicas: 1}, false)
	if err == nil {
		t.Fatal("start with an unhealthy B succeeded")
	}
	if len(h.bReplicas()) != 0 {
		t.Fatal("unhealthy B left running")
	}
	if _, ok := h.m.store.get("app"); ok {
		t.Fatal("failed start left a record")
	}
	hist := h.m.store.history("app")
	if len(hist) != 1 || hist[0].Outcome != "start_failed" {
		t.Fatalf("history = %+v", hist)
	}
}

// ---- phases ----

func TestABPhaseTransitions(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	ctx := context.Background()
	rec, err := h.m.Start(ctx, "app", abStartRequest{Image: "ghcr.io/org/app:v2", Replicas: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	id := rec.ID
	firstB := h.bReplicas()[0].ID

	h.clock.advance(time.Minute)
	if _, err := h.m.SetSplit("app", 50, false); err != nil {
		t.Fatal(err)
	}
	b := h.bReplicas()
	if len(b) != 1 || b[0].ID == firstB || b[0].Labels[labelABSplit] != "50" || b[0].Labels[labelABID] != id ||
		b[0].Labels[labelABEpoch] != strconv.FormatInt(h.clock.now().Unix(), 10) || b[0].Labels[labelABStarted] != strconv.FormatInt(rec.Started, 10) {
		t.Fatalf("after split: %+v", b)
	}
	if _, err := h.m.SetGroups("app", map[string]string{"staff": "B"}, false); err != nil {
		t.Fatal(err)
	}
	if g := h.bReplicas()[0].Labels[labelABGroups]; g != "staff:B" {
		t.Fatalf("groups label = %q", g)
	}
	_, err = h.m.SetSplit("app", 101, false)
	wantABCode(t, err, http.StatusBadRequest)

	h.clock.advance(time.Minute)
	if _, err := h.m.Abort("app", false); err != nil {
		t.Fatal(err)
	}
	l := h.bReplicas()[0].Labels
	if l[labelABPhase] != abPhaseAborted || l[labelABAbortReason] != "manual" || l[labelABPhaseAt] != strconv.FormatInt(h.clock.now().Unix(), 10) {
		t.Fatalf("after abort: %v", l)
	}
	abortedAt := l[labelABPhaseAt]
	_, err = h.m.Abort("app", false)
	wantABCode(t, err, http.StatusConflict)

	// Promoting an aborted B needs confirm_aborted.
	_, err = h.m.Promote("app", false, false, false)
	wantABCode(t, err, http.StatusConflict)

	// Discard from aborted keeps phase_at: B has been draining since then.
	h.clock.advance(time.Minute)
	if _, err := h.m.Discard("app", false, false); err != nil {
		t.Fatal(err)
	}
	l = h.bReplicas()[0].Labels
	if l[labelABPhase] != abPhaseDiscarding || l[labelABPhaseAt] != abortedAt {
		t.Fatalf("after discard: %v", l)
	}
	if r := h.record(t); r.Pending != abPendingDiscard || r.Op != "" {
		t.Fatalf("record = %+v", r)
	}
	_, err = h.m.Promote("app", false, true, false)
	wantABCode(t, err, http.StatusConflict)
	_, err = h.m.SetSplit("app", 10, false)
	wantABCode(t, err, http.StatusConflict)
	_, err = h.m.Reset(ctx, "app", true, false)
	wantABCode(t, err, http.StatusConflict)
	_, err = h.m.Discard("app", false, false)
	wantABCode(t, err, http.StatusConflict)

	// Force discard finalizes at once.
	if _, err := h.m.Discard("app", true, false); err != nil {
		t.Fatal(err)
	}
	if len(h.bReplicas()) != 0 {
		t.Fatal("force discard left B")
	}
	hist := h.m.store.history("app")
	if _, ok := h.m.store.get("app"); ok || len(hist) != 1 || hist[0].Outcome != "discarded" || hist[0].ID != id {
		t.Fatalf("after force discard: history %+v", hist)
	}
	if live, canary := countLiveCanary(h.f); live != 1 || canary != 0 {
		t.Fatalf("live=%d canary=%d after discard", live, canary)
	}
}

func countLiveCanary(f *cenvFakeDocker) (live, canary int) {
	for _, c := range f.list() {
		if c.Labels[labelService] != "app" {
			continue
		}
		if c.Labels[labelCanary] == "true" {
			canary++
		} else {
			live++
		}
	}
	return
}

func TestABPromoteFromRunningThenDiscardRefused(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	rec := abTestRecord(h.clock.now())
	h.seedB(t, "b1", rec, false)
	h.clock.advance(time.Hour)
	if _, err := h.m.Promote("app", false, false, false); err != nil {
		t.Fatal(err)
	}
	l := h.bReplicas()[0].Labels
	if l[labelABPhase] != abPhasePromoting || l[labelABPhaseAt] != strconv.FormatInt(h.clock.now().Unix(), 10) {
		t.Fatalf("after promote: %v", l)
	}
	_, err := h.m.Discard("app", false, false)
	wantABCode(t, err, http.StatusConflict)
	_, err = h.m.Abort("app", false)
	wantABCode(t, err, http.StatusConflict)
}

// TestABPromoteRefusedWhileProxyLatchedAbort: the proxy latched an
// auto-abort the Run loop hasn't relabelled yet — that B counts as aborted.
func TestABPromoteRefusedWhileProxyLatchedAbort(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	rec := abTestRecord(h.clock.now())
	h.seedB(t, "b1", rec, false)
	e := abExp(rec, 0, 0)
	e.Abort = &abProxyAbort{Reason: "errors", At: rec.PhaseAt + 10, Source: "auto"}
	h.px.set(e)
	_, err := h.m.Promote("app", false, false, false)
	wantABCode(t, err, http.StatusConflict)
	if !strings.Contains(err.Error(), "confirm_aborted") {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.m.Promote("app", false, true, false); err != nil {
		t.Fatalf("confirmed promote: %v", err)
	}
}

func TestABResetNeedsDrainedBOrForce(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	rec := abTestRecord(h.clock.now())
	h.seedB(t, "b1", rec, false)
	now := h.clock.now().Unix()
	h.px.set(abExp(rec, now, now-60)) // B pinned a minute ago
	_, err := h.m.Reset(context.Background(), "app", false, false)
	wantABCode(t, err, http.StatusConflict)
	if h := h.m.store.history("app"); len(h) != 0 {
		t.Fatalf("refused reset wrote history: %+v", h)
	}

	h.clock.advance(5 * time.Minute)
	got, err := h.m.Reset(context.Background(), "app", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == rec.ID || got.Phase != abPhaseRunning || got.Epoch != h.clock.now().Unix() || got.Started != h.clock.now().Unix() {
		t.Fatalf("reset record = %+v", got)
	}
	if l := h.bReplicas()[0].Labels; l[labelABID] != got.ID || l[labelABSplit] != "20" {
		t.Fatalf("B after reset = %v", l)
	}
	hist := h.m.store.history("app")
	if len(hist) != 1 || hist[0].Outcome != "reset" || hist[0].ID != rec.ID {
		t.Fatalf("history = %+v", hist)
	}
}

func TestABOpsWithoutTest(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	for name, err := range map[string]error{
		"abort":   func() error { _, e := h.m.Abort("app", false); return e }(),
		"split":   func() error { _, e := h.m.SetSplit("app", 5, false); return e }(),
		"promote": func() error { _, e := h.m.Promote("app", false, false, false); return e }(),
		"discard": func() error { _, e := h.m.Discard("app", true, false); return e }(),
		"reset":   func() error { _, e := h.m.Reset(context.Background(), "app", true, false); return e }(),
	} {
		if err == nil || abErrStatus(err) != http.StatusNotFound {
			t.Fatalf("%s without a test: %v", name, err)
		}
	}
}

// ---- drain ----

func TestABDrain(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rec := abTestRecord(now.Add(-2 * time.Hour))
	rec.Phase = abPhaseDiscarding
	host := func(id string, lastB int64) abHostReport {
		e := abExp(rec, 0, lastB)
		e.ID = id
		return abHostReport{Host: "h", Reachable: true, Exp: &e}
	}
	idle := int64(abDefaultSessionIdle / time.Second)

	// Idle: last B pin more than session_idle ago, on every host.
	last := now.Unix() - idle - 1
	d := abDrain(rec, "B", []abHostReport{host(rec.ID, last), host(rec.ID, last-100)}, now)
	if !d.Drained || d.LastPinnedAt != last || d.DrainsBy != last+idle || d.ActiveSessionsApprox != 4 || d.DeadlineOnly {
		t.Fatalf("idle drain = %+v", d)
	}
	// One host still has a recent B pin: not drained.
	d = abDrain(rec, "B", []abHostReport{host(rec.ID, last), host(rec.ID, now.Unix()-10)}, now)
	if d.Drained || d.DrainsBy != now.Unix()-10+idle {
		t.Fatalf("recent pin = %+v", d)
	}
	// A host reporting another id (or none — say a proxy still loading
	// after a restart) can't vouch for this id's pins: deadline only, and
	// its pins don't count either.
	for _, other := range []abHostReport{host("otherid1", now.Unix()), {Host: "h", Reachable: true}} {
		d = abDrain(rec, "B", []abHostReport{host(rec.ID, last), other}, now)
		if d.Drained || !d.DeadlineOnly || d.LastPinnedAt != last {
			t.Fatalf("foreign/no id = %+v", d)
		}
	}
	// A proxy that began tracking less than session_idle ago (restarted)
	// can't vouch for the time before: the idle clock runs from then.
	restarted := host(rec.ID, 0)
	restarted.Exp.TrackingSince = now.Unix() - 60
	d = abDrain(rec, "B", []abHostReport{host(rec.ID, last), restarted}, now)
	if d.Drained || d.DeadlineOnly || d.DrainsBy != now.Unix()-60+idle {
		t.Fatalf("recently restarted proxy = %+v", d)
	}
	// No tracking_since at all (an older proxy): deadline only.
	unknown := host(rec.ID, last)
	unknown.Exp.TrackingSince = 0
	if d = abDrain(rec, "B", []abHostReport{unknown}, now); d.Drained || !d.DeadlineOnly {
		t.Fatalf("unknown tracking_since = %+v", d)
	}
	// The persisted max wins over a lower report.
	withMax := rec.clone()
	withMax.PinnedMaxB = now.Unix() - 10
	if d = abDrain(withMax, "B", []abHostReport{host(rec.ID, last)}, now); d.Drained || d.LastPinnedAt != withMax.PinnedMaxB {
		t.Fatalf("persisted max ignored = %+v", d)
	}
	// Deadline: phase_at + max_session has passed, pins don't matter.
	old := rec.clone()
	old.PhaseAt = now.Add(-25 * time.Hour).Unix()
	d = abDrain(old, "B", []abHostReport{host(old.ID, now.Unix())}, now)
	if !d.Drained || d.DrainsBy != old.PhaseAt+int64(abDefaultMaxSession/time.Second) {
		t.Fatalf("deadline drain = %+v", d)
	}
	// A configured max_session moves the deadline.
	old.Config[labelABMaxSession] = "48h0m0s"
	if d = abDrain(old, "B", []abHostReport{host(old.ID, now.Unix())}, now); d.Drained {
		t.Fatalf("48h max_session drained after 25h: %+v", d)
	}
	// A proxy that couldn't be asked: only the deadline counts, even though
	// the reachable host's pins are long idle.
	d = abDrain(rec, "B", []abHostReport{host(rec.ID, last), {Host: "peer", Reachable: false}}, now)
	if d.Drained || !d.DeadlineOnly || d.DrainsBy != rec.PhaseAt+int64(abDefaultMaxSession/time.Second) {
		t.Fatalf("unreachable = %+v", d)
	}
	d = abDrain(old, "B", []abHostReport{{Host: "peer", Reachable: false}}, now.Add(25*time.Hour))
	if !d.Drained {
		t.Fatalf("unreachable past the deadline = %+v", d)
	}
	// Variant A reads A's pins.
	e := abExp(rec, last, now.Unix())
	if d = abDrain(rec, "A", []abHostReport{{Reachable: true, Exp: &e}}, now); !d.Drained || d.Variant != "A" {
		t.Fatalf("A drain = %+v", d)
	}
}

// ---- Run loop ----

func TestABRunFinalizesDiscardOnceDrained(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	rec := abTestRecord(h.clock.now())
	h.seedB(t, "b1", rec, false)
	h.clock.advance(time.Minute)
	if _, err := h.m.Discard("app", false, false); err != nil {
		t.Fatal(err)
	}
	cur := h.record(t)
	now := h.clock.now().Unix()
	h.px.set(abExp(cur, now, now-30))
	h.m.tick(context.Background())
	if len(h.bReplicas()) != 1 {
		t.Fatal("B removed before it drained")
	}
	// Session idle passes with no further B pins.
	h.clock.advance(abDefaultSessionIdle + time.Minute)
	h.m.tick(context.Background())
	if len(h.bReplicas()) != 0 {
		t.Fatal("drained B not removed")
	}
	if _, ok := h.m.store.get("app"); ok {
		t.Fatal("record not cleared")
	}
	if hist := h.m.store.history("app"); len(hist) != 1 || hist[0].Outcome != "discarded" {
		t.Fatalf("history = %+v", hist)
	}
}

func TestABRunFinalizesPromoteByDeadlineWhenProxyDown(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	rec := abTestRecord(h.clock.now())
	h.seedB(t, "b1", rec, false)
	if _, err := h.m.Promote("app", false, false, false); err != nil {
		t.Fatal(err)
	}
	h.px.setDown(true)
	h.clock.advance(abDefaultSessionIdle * 2)
	h.m.tick(context.Background())
	if len(h.bReplicas()) != 1 {
		t.Fatal("promoted with the proxy unreachable before the deadline")
	}
	h.clock.advance(abDefaultMaxSession)
	h.m.tick(context.Background())
	if live, canary := countLiveCanary(h.f); live != 1 || canary != 0 {
		t.Fatalf("after promote: live=%d canary=%d", live, canary)
	}
	for _, c := range h.f.list() {
		if c.Labels[labelService] != "app" {
			continue
		}
		if c.Image != "ghcr.io/org/app:v2" {
			t.Fatalf("live %s runs %s, want B's image", c.name(), c.Image)
		}
		for k := range c.Labels {
			if strings.HasPrefix(k, labelABPrefix) || k == labelCanary {
				t.Fatalf("promoted %s kept %s", c.name(), k)
			}
		}
	}
	if hist := h.m.store.history("app"); len(hist) != 1 || hist[0].Outcome != "promoted" {
		t.Fatalf("history = %+v", hist)
	}
}

func TestABRunPicksUpProxyAutoAbort(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	rec := abTestRecord(h.clock.now())
	h.seedB(t, "b1", rec, false)
	// A label-sourced abort is the dashboard's own echo, never a signal.
	e := abExp(rec, 0, 0)
	e.Abort = &abProxyAbort{Reason: "manual", At: rec.PhaseAt, Source: "label"}
	h.px.set(e)
	h.m.tick(context.Background())
	if h.record(t).Phase != abPhaseRunning || h.bReplicas()[0].ID != "b1" {
		t.Fatal("label-sourced abort was acted on")
	}

	h.clock.advance(10 * time.Minute)
	at := rec.PhaseAt + 300
	e.Abort = &abProxyAbort{Reason: "latency", Detail: "p95 3x", At: at, Source: "auto"}
	h.px.set(e)
	h.m.tick(context.Background())
	bs := h.bReplicas()
	if len(bs) != 1 || bs[0].ID == "b1" {
		t.Fatalf("B not relabelled: %+v", bs)
	}
	l := bs[0].Labels
	if l[labelABPhase] != abPhaseAborted || l[labelABAbortReason] != "latency" || l[labelABPhaseAt] != strconv.FormatInt(at, 10) || l[labelABID] != rec.ID {
		t.Fatalf("B labels after auto-abort = %v", l)
	}
	if r := h.record(t); r.Phase != abPhaseAborted || r.AbortReason != "latency" || r.PhaseAt != at || r.Op != "" {
		t.Fatalf("record = %+v", r)
	}
	// The latch keeps being reported: no second relabel.
	id := bs[0].ID
	h.m.tick(context.Background())
	if h.bReplicas()[0].ID != id {
		t.Fatal("relabelled again on a latch already applied")
	}
	// A peer's latch for a different id (an old cohort) is ignored after a reset.
	if _, err := h.m.Reset(context.Background(), "app", true, false); err != nil {
		t.Fatal(err)
	}
	h.m.tick(context.Background())
	if r := h.record(t); r.Phase != abPhaseRunning {
		t.Fatalf("old id's latch aborted the new cohort: %+v", r)
	}
}

// TestABResumeAfterRestart: the store is the intent; a new manager on the
// same file converges B onto it.
func TestABResumeAfterRestart(t *testing.T) {
	withFastAB(t)
	f := newCenvFakeDocker()
	f.seedTemplate(nil)
	dc := f.client(t)
	path := filepath.Join(t.TempDir(), "abtests.json")

	// 1. A relabel that never ran: B still says running.
	h := newABHarnessOn(t, f, dc, nil, "", path)
	rec := abTestRecord(h.clock.now())
	h.seedB(t, "b1", rec, false)
	want := rec.clone()
	want.Phase, want.AbortReason, want.Op = abPhaseAborted, "manual", abOpRelabel
	if err := h.m.store.put("app", want); err != nil {
		t.Fatal(err)
	}
	h2 := newABHarnessOn(t, f, dc, nil, "", path)
	h2.m.tick(context.Background())
	if l := h2.bReplicas()[0].Labels; l[labelABPhase] != abPhaseAborted || l[labelABAbortReason] != "manual" {
		t.Fatalf("B after resume = %v", l)
	}
	if r := h2.record(t); r.Op != "" {
		t.Fatalf("record after resume = %+v", r)
	}

	// 2. An interrupted start: B removed, record dropped, history says so.
	starting := h2.record(t)
	starting.Op = abOpStart
	h2.m.store.put("app", starting)
	h3 := newABHarnessOn(t, f, dc, nil, "", path)
	h3.m.tick(context.Background())
	if len(h3.bReplicas()) != 0 {
		t.Fatal("interrupted start left B")
	}
	if _, ok := h3.m.store.get("app"); ok {
		t.Fatal("interrupted start left its record")
	}
	if hist := h3.m.store.history("app"); len(hist) != 1 || hist[0].Outcome != "start_failed" || !strings.Contains(hist[0].Detail, "interrupted") {
		t.Fatalf("history = %+v", hist)
	}

	// 3. A lost store: B's labels are adopted back into a record.
	h3.seedB(t, "b9", abTestRecord(h3.clock.now()), true)
	h4 := newABHarnessOn(t, f, dc, nil, "", filepath.Join(t.TempDir(), "fresh.json"))
	h4.m.tick(context.Background())
	if r := h4.record(t); r.ID != "feedf00d" || r.Phase != abPhaseRunning || r.Config[labelABSplit] != "20" || r.Replicas != 1 {
		t.Fatalf("adopted record = %+v", r)
	}
}

func TestABRunRetriesFailedRelabelWithBackoff(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	rec := abTestRecord(h.clock.now())
	h.seedB(t, "b1", rec, false)
	h.f.setUnhealthy(func(b createBody) bool { return true })
	if _, err := h.m.Abort("app", false); err == nil {
		t.Fatal("relabel onto an unhealthy replica succeeded")
	}
	r := h.record(t)
	if r.Op != abOpRelabel || r.LastError == "" || h.bReplicas()[0].ID != "b1" {
		t.Fatalf("after failed relabel: %+v (old B must keep serving)", r)
	}
	h.f.setUnhealthy(nil)
	h.m.tick(context.Background())
	if h.bReplicas()[0].ID != "b1" {
		t.Fatal("retried before the backoff")
	}
	h.clock.advance(abRetryInterval)
	h.m.tick(context.Background())
	if l := h.bReplicas()[0].Labels; l[labelABPhase] != abPhaseAborted {
		t.Fatalf("not retried after the backoff: %v", l)
	}
	if r := h.record(t); r.Op != "" || r.LastError != "" {
		t.Fatalf("record = %+v", r)
	}
}

// TestABAutoAbortReachesLabelsDespitePendingRelabel: a split relabel keeps
// failing; the proxy's latch still lands in the record, and the next
// successful relabel carries both.
func TestABAutoAbortReachesLabelsDespitePendingRelabel(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	rec := abTestRecord(h.clock.now())
	h.seedB(t, "b1", rec, false)
	h.f.setUnhealthy(func(b createBody) bool { return true })
	if _, err := h.m.SetSplit("app", 40, false); err == nil {
		t.Fatal("relabel onto an unhealthy replica succeeded")
	}
	e := abExp(rec, 0, 0)
	e.Abort = &abProxyAbort{Reason: "errors", At: rec.PhaseAt + 60, Source: "peer"}
	h.px.set(e)
	h.f.setUnhealthy(nil)
	h.m.tick(context.Background())
	l := h.bReplicas()[0].Labels
	if l[labelABPhase] != abPhaseAborted || l[labelABAbortReason] != "errors" || l[labelABSplit] != "40" {
		t.Fatalf("B labels = %v", l)
	}
	if r := h.record(t); r.Op != "" || r.Phase != abPhaseAborted {
		t.Fatalf("record = %+v", r)
	}
}

// ---- promoteCanary ----

// fixedCentral is a centralEnvResolver that manages every service at one
// version — just enough for promoteCanary's version check.
type fixedCentral struct{ version uint64 }

func (c fixedCentral) Enabled() bool { return true }
func (c fixedCentral) Resolve(context.Context, string, map[string]string) (centralEnvResult, bool, error) {
	return centralEnvResult{}, false, nil
}
func (c fixedCentral) Managed(string, map[string]string) bool { return true }
func (c fixedCentral) ResolveForPeer(string, string) (centralEnvResult, bool, error) {
	return centralEnvResult{}, false, nil
}
func (c fixedCentral) Accept(string, string, uint64, []string, *healthcheckSpec) error { return nil }
func (c fixedCentral) Version(string) (uint64, bool)                                   { return c.version, true }

func TestPromoteCanaryABStripsLabelsAndSkipsEnvCheck(t *testing.T) {
	withFastAB(t)
	stamp := map[string]string{labelEnvOrigin: "dashboard-a", labelEnvVersion: "3"}

	// Plain canary on an outdated central env version: still refused.
	f := newCenvFakeDocker()
	f.seedTemplate(nil)
	f.seedCanary("ghcr.io/org/app:v2", stamp)
	dc := f.client(t)
	dc.central = fixedCentral{version: 5}
	var managed errEnvCentrallyManaged
	if err := dc.promoteCanary(context.Background(), "app"); !errors.As(err, &managed) {
		t.Fatalf("plain canary promote = %v, want errEnvCentrallyManaged", err)
	}

	// The same canary as an A/B B: promoted on its own env, proxy.ab.* gone,
	// its env stamp kept so the deferred sync sees the lag.
	f2 := newCenvFakeDocker()
	f2.seedTemplate(nil)
	ab := abTestRecord(time.Now()).labels()
	for k, v := range stamp {
		ab[k] = v
	}
	f2.seedCanary("ghcr.io/org/app:v2", ab)
	dc2 := f2.client(t)
	dc2.central = fixedCentral{version: 5}
	if err := dc2.promoteCanary(context.Background(), "app"); err != nil {
		t.Fatalf("A/B promote: %v", err)
	}
	creates := f2.createsSnapshot()
	if len(creates) != 1 {
		t.Fatalf("creates = %d", len(creates))
	}
	c := creates[0]
	for k := range c.body.Labels {
		if strings.HasPrefix(k, labelABPrefix) || k == labelCanary {
			t.Fatalf("promoted replica kept %s: %v", k, c.body.Labels)
		}
	}
	if c.body.Labels[labelEnvVersion] != "3" || strings.Join(c.body.Env, ",") != "A=canary,SECRET=tpl" || c.body.Image != "ghcr.io/org/app:v2" {
		t.Fatalf("promoted replica = %+v", c.body)
	}
	if live, canary := countLiveCanary(f2); live != 1 || canary != 0 {
		t.Fatalf("live=%d canary=%d", live, canary)
	}
}

// ---- scale never clones B ----

func TestScaleWithABTestNeverClonesB(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	h.seedB(t, "b1", abTestRecord(h.clock.now()), false)
	if err := runServiceScale(context.Background(), h.dc, newTestOnboardedStore(t), "", h.px.srv.URL, "app", 3); err != nil {
		t.Fatal(err)
	}
	creates := h.f.createsSnapshot()
	if len(creates) != 2 {
		t.Fatalf("creates = %d, want 2", len(creates))
	}
	for _, c := range creates {
		if c.body.Image != "ghcr.io/org/app:v1" || strings.Contains(strings.Join(c.body.Env, ","), "b-sentinel") {
			t.Fatalf("scale cloned B: %+v", c.body)
		}
		for k := range c.body.Labels {
			if strings.HasPrefix(k, labelABPrefix) || k == labelCanary {
				t.Fatalf("scaled replica has %s", k)
			}
		}
	}
}

// ---- guards ----

func abGuardCases() []struct{ method, path, body string } {
	return []struct{ method, path, body string }{
		{"POST", "/services/app/replace", `{"image":"ghcr.io/org/app:v3"}`},
		{"POST", "/services/app/rolling-replace", `{"image":"ghcr.io/org/app:v3"}`},
		{"POST", "/services/app/spread", `{"host":"dashboard-b"}`},
		{"POST", "/services/app/stage", `{"image":"ghcr.io/org/app:v3"}`},
		{"POST", "/services/app/promote", ``},
		{"DELETE", "/services/app/canary", ``},
	}
}

func TestABGuardsAPI(t *testing.T) {
	withFastAB(t)
	for _, withManager := range []bool{true, false} {
		h := newABHarness(t, nil, "")
		h.seedB(t, "b1", abTestRecord(h.clock.now()), !withManager)
		if !withManager {
			h.dc.ab = nil
		}
		mux := newLocalTestMux(t, h.dc, nil)
		for _, c := range abGuardCases() {
			rec := apiDo(t, mux, c.method, "/api"+c.path, c.body)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "A/B test") {
				t.Fatalf("manager=%v %s %s = %d %s", withManager, c.method, c.path, rec.Code, rec.Body.String())
			}
			if strings.HasSuffix(c.path, "/promote") || strings.HasSuffix(c.path, "/canary") {
				if !strings.Contains(rec.Body.String(), "A/B endpoints") {
					t.Fatalf("%s: %s", c.path, rec.Body.String())
				}
			}
		}
		if len(h.bReplicas()) != 1 || h.bReplicas()[0].ID != "b1" {
			t.Fatal("a guarded action touched B")
		}
		// Scale A stays allowed.
		if rec := apiDo(t, mux, "POST", "/api/services/app/scale", `{"Replicas":2}`); rec.Code != http.StatusOK {
			t.Fatalf("scale = %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestABGuardsPeer(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	h.seedB(t, "b1", abTestRecord(h.clock.now()), false)
	handler := peerServicesMutateHandler("s3cret", "dashboard-b", h.dc, newTestOnboardedStore(t), newImageChecker(h.dc), nil, "", h.px.srv.URL, true, nil)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/peer"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	for _, c := range abGuardCases() {
		if rec := do(c.method, c.path, c.body); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "A/B test") {
			t.Fatalf("peer %s %s = %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	if len(h.bReplicas()) != 1 || h.bReplicas()[0].ID != "b1" {
		t.Fatal("a guarded peer action touched B")
	}
	// The A/B routes themselves work over the peer handler.
	if rec := do("POST", "/services/app/ab/abort", ``); rec.Code != http.StatusAccepted {
		t.Fatalf("peer abort = %d %s", rec.Code, rec.Body.String())
	}
	abWaitIdle(t, h.m, "app")
	if h.record(t).Phase != abPhaseAborted {
		t.Fatal("peer abort not applied")
	}

	// Without -peer-writes: GET ab is still served, POSTs are not.
	ro := peerServicesMutateHandler("s3cret", "dashboard-b", h.dc, newTestOnboardedStore(t), newImageChecker(h.dc), nil, "", h.px.srv.URL, false, nil)
	get := httptest.NewRequest("GET", "/peer/services/app/ab", nil)
	get.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	ro.ServeHTTP(rec, get)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"active"`) {
		t.Fatalf("read-only GET ab = %d %s", rec.Code, rec.Body.String())
	}
	post := httptest.NewRequest("POST", "/peer/services/app/ab/abort", nil)
	post.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	ro.ServeHTTP(rec, post)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("read-only POST = %d", rec.Code)
	}
	bad := httptest.NewRequest("GET", "/peer/services/app/ab", nil)
	bad.Header.Set("Authorization", "Bearer nope")
	rec = httptest.NewRecorder()
	ro.ServeHTTP(rec, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad secret GET ab = %d", rec.Code)
	}
}

func TestABGuardsCentralEnvAdoptRelease(t *testing.T) {
	withFastAB(t)
	h := newSyncHost(t, "dashboard-a", nil, "")
	h.f.seedTemplate(nil)
	ab := newABHarnessOn(t, h.f, h.dc, nil, "", filepath.Join(t.TempDir(), "ab.json"))
	ab.seedB(t, "b1", abTestRecord(ab.clock.now()), false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/services/app/env/adopt", nil)
	serveCentralEnvAdoptLocal(rec, req, h.ce, "app", centralEnvAdoptRequest{DryRun: abBoolP(false)}, "alice")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "A/B test") {
		t.Fatalf("adopt = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := h.ce.store.Get("app"); ok {
		t.Fatal("adopt created a record during an A/B test")
	}

	h.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	_, err := startCentralEnvRelease(httptest.NewRequest("POST", "/", nil), h.ce, "app", "alice")
	var active errABActive
	if !errors.As(err, &active) {
		t.Fatalf("release = %v", err)
	}
	rec = httptest.NewRecorder()
	writeCentralEnvErr(rec, err)
	if rec.Code != http.StatusConflict {
		t.Fatalf("release err maps to %d", rec.Code)
	}
	if r, _, _ := h.ce.store.Get("app"); r.State == centralEnvStateReleasing {
		t.Fatal("release started during an A/B test")
	}
}

// ---- central env: accept + defer ----

func TestABCentralEnvDeferredOnOriginThenSyncedAfterFinalize(t *testing.T) {
	withFastAB(t)
	h := newSyncHost(t, "dashboard-a", nil, "")
	h.ce.store.Create("app", "dashboard-a", map[string]string{"A": "1"}, nil, "test", "")
	h.f.seedMember("m1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)
	ab := newABHarnessOn(t, h.f, h.dc, nil, "", filepath.Join(t.TempDir(), "ab.json"))
	ab.seedB(t, "b1", abTestRecord(ab.clock.now()), false)

	res, err := applyCentralEnvSet(h.ce, "app", centralEnvSetRequest{IfVersion: 1, Set: map[string]string{"A": "2"}}, "alice")
	if err != nil || res.Version != 2 {
		t.Fatalf("set = %+v, %v (edits are accepted during a test)", res, err)
	}
	j := h.waitJob(t, "app")
	if j.Status != envSyncStatusDeferred || !strings.Contains(j.LastError, "applies after the A/B test") {
		t.Fatalf("job = %+v", j)
	}
	if h.m.failure("app") != nil {
		t.Fatal("a deferral was recorded as a failure")
	}
	if mv := h.f.members()["goproxy-app-1"]; mv.version != "1" {
		t.Fatalf("A rolled during the test: %+v", mv)
	}
	view, err := buildCentralEnvView(context.Background(), h.ce, "app")
	if err != nil || !view.ABDeferred || !strings.Contains(strings.Join(view.Warnings, ";"), "applies after the A/B test") {
		t.Fatalf("view = %+v, %v", view, err)
	}
	ab.m.tick(context.Background())
	st := ab.m.Status(context.Background(), "app")
	if st.EnvPendingVersion != 2 || !strings.Contains(st.EnvPendingNote, "applies after the test") {
		t.Fatalf("status env pending = %d %q", st.EnvPendingVersion, st.EnvPendingNote)
	}

	if _, err := ab.m.Discard("app", true, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "post-finalize sync", func() bool {
		j, ok := h.m.get("app")
		return ok && j.Status == envSyncStatusConverged && !h.m.busy("app")
	})
	if mv := h.f.members(); len(mv) != 1 || mv[firstKey(mv)].version != "2" {
		t.Fatalf("members after finalize = %+v", mv)
	}
}

func firstKey(m map[string]memberView) string {
	for k := range m {
		return k
	}
	return ""
}

// TestABCentralEnvDeferredOnPeer: B runs on the non-origin host. The
// origin rolls itself, the peer defers, the origin reports the host as
// deferred (not partial), and finalizing the test there pulls the version.
func TestABCentralEnvDeferredOnPeer(t *testing.T) {
	withFastAB(t)
	a, b, _ := newMesh(t, true)
	seedConverged(a, b)
	ab := newABHarnessOn(t, b.f, b.dc, nil, "", filepath.Join(t.TempDir(), "ab.json"))
	ab.seedB(t, "bb1", abTestRecord(ab.clock.now()), false)

	rec := apiDo(t, a.mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"2"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	ja := a.waitJob(t, "app")
	if ja.Status != envSyncStatusDeferred || hostResult(ja, "dashboard-b").Status != envSyncHostDeferred || hostResult(ja, "dashboard-a").Status != envSyncHostConverged {
		t.Fatalf("origin job = %+v", ja)
	}
	if a.m.failure("app") != nil {
		t.Fatal("deferred peer counted as a failure")
	}
	jb := b.waitJob(t, "app")
	if jb.Status != envSyncStatusDeferred || jb.Target != 2 {
		t.Fatalf("peer job = %+v", jb)
	}
	if mv := b.f.members()["goproxy-app-1"]; mv.version != "1" {
		t.Fatalf("peer A rolled during the test: %+v", mv)
	}
	ab.m.tick(context.Background())
	if st := ab.m.Status(context.Background(), "app"); st.EnvPendingVersion != 2 {
		t.Fatalf("peer env pending = %d", st.EnvPendingVersion)
	}

	if _, err := ab.m.Discard("app", true, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mv := b.f.members()
		j, _ := b.m.get("app")
		if len(mv) == 1 && mv[firstKey(mv)].version == "2" && !b.m.busy("app") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer never pulled the deferred version: members %+v job %+v", mv, j)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- HTTP: async start, status, forwarding, /peer/ab, secrets ----

// abWaitIdle waits until no op runs on svc and its record (if any) has
// no unapplied op.
func abWaitIdle(t *testing.T, m *abManager, svc string) {
	t.Helper()
	waitFor(t, "A/B op to finish", func() bool {
		l := m.lock(svc)
		if !l.TryLock() {
			return false
		}
		l.Unlock()
		rec, ok := m.store.get(svc)
		return !ok || rec.Op == ""
	})
}

func TestABHTTPStartStatusAndNoSecrets(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	h.f.mutateInspect("tpl1", func(in *cenvInspect) { in.env = []string{"A=1", "SECRET=sentinel-do-not-leak"} })
	mux := newLocalTestMux(t, h.dc, nil)

	if rec := apiDo(t, mux, "POST", "/api/services/app/ab", `{"image":"ghcr.io/org/app:v2","replicas":1,"env":{"X":"1"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("start with env = %d", rec.Code)
	}
	rec := apiDo(t, mux, "POST", "/api/services/app/ab", `{"image":"ghcr.io/org/app:v2","replicas":1,"split":30}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	bodies := []string{rec.Body.String()}
	var accepted map[string]any
	json.Unmarshal(rec.Body.Bytes(), &accepted)
	if accepted["op"] != abOpStart || accepted["phase"] != abPhaseRunning {
		t.Fatalf("202 body = %v", accepted)
	}
	abWaitIdle(t, h.m, "app")
	if b := h.bReplicas(); len(b) != 1 || !strings.Contains(strings.Join(h.f.inspect[b[0].ID].env, ","), "sentinel-do-not-leak") {
		t.Fatalf("B = %+v (B must run on A's env)", b)
	}

	id := h.record(t).ID
	hist := make([]uint64, 64)
	hist[20] = 400
	e := abExp(h.record(t), h.clock.now().Unix(), h.clock.now().Unix())
	e.Cumulative = abProxyPair[abProxyCounters]{A: abProxyCounters{Requests: 300, Err5xx: 3, Hist: hist}, B: abProxyCounters{Requests: 100, Err5xx: 1, Hist: hist}}
	e.Windows = []abProxyWindow{{Index: 1, Start: 100}}
	e.Merged = abProxyMerged{Windows: []abProxyWindow{{Index: 7, Start: 700}}}
	h.px.set(e)

	rec = apiDo(t, mux, "GET", "/api/services/app/ab", "")
	bodies = append(bodies, rec.Body.String())
	var st abStatusView
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET ab = %d %s", rec.Code, rec.Body.String())
	}
	if st.State != "active" || st.ID != id || st.Phase != abPhaseRunning || st.Pending != nil || st.Op != "" ||
		st.Stats.A.Requests != 300 || st.Stats.B.Requests != 100 || st.Stats.A.P50Ms == 0 || st.Hint == nil ||
		len(st.PerHost) != 1 || !st.PerHost[0].Reachable || len(st.Windows) != 1 || st.Windows[0].Index != 7 || st.Config[labelABSplit] != "30" {
		t.Fatalf("status = %+v", st)
	}

	// Services list carries the summary.
	rec = apiDo(t, mux, "GET", "/api/services", "")
	bodies = append(bodies, rec.Body.String())
	if !strings.Contains(rec.Body.String(), `"ab_test":{"id":"`+id+`"`) {
		t.Fatalf("services list = %s", rec.Body.String())
	}

	rec = apiDo(t, mux, "POST", "/api/services/app/ab/discard", `{"force":true}`)
	bodies = append(bodies, rec.Body.String())
	if rec.Code != http.StatusAccepted {
		t.Fatalf("discard = %d %s", rec.Code, rec.Body.String())
	}
	abWaitIdle(t, h.m, "app")
	rec = apiDo(t, mux, "GET", "/api/services/app/ab", "")
	bodies = append(bodies, rec.Body.String())
	if !strings.Contains(rec.Body.String(), `"state":"none"`) || !strings.Contains(rec.Body.String(), `"outcome":"discarded"`) {
		t.Fatalf("GET ab after discard = %s", rec.Body.String())
	}

	// /peer/ab relays the proxy report.
	pa := peerABHandler("s3cret", h.px.srv.URL)
	req := httptest.NewRequest("GET", "/peer/ab?service=app", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	prec := httptest.NewRecorder()
	pa.ServeHTTP(prec, req)
	bodies = append(bodies, prec.Body.String())

	for _, b := range bodies {
		if strings.Contains(b, "sentinel-do-not-leak") || strings.Contains(b, "SECRET") {
			t.Fatalf("a response leaked env: %s", b)
		}
	}
}

func TestABHTTPBusyIs409(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	h.seedB(t, "b1", abTestRecord(h.clock.now()), false)
	mux := newLocalTestMux(t, h.dc, nil)
	l := h.m.lock("app")
	l.Lock()
	rec := apiDo(t, mux, "POST", "/api/services/app/ab/abort", "")
	l.Unlock()
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "in progress") {
		t.Fatalf("abort while busy = %d %s", rec.Code, rec.Body.String())
	}
	if rec := apiDo(t, mux, "POST", "/api/services/app/ab/split", `{"bogus":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", rec.Code)
	}
	if rec := apiDo(t, mux, "POST", "/api/services/app/ab/split", ``); rec.Code != http.StatusBadRequest {
		t.Fatalf("split without split = %d", rec.Code)
	}
}

func TestABHTTPForwardsToHost(t *testing.T) {
	withFastAB(t)
	type call struct{ method, path, body string }
	var mu sync.Mutex
	var calls []call
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path, string(b)})
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"state":"none"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"accepted"}`))
	}))
	t.Cleanup(peer.Close)
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")
	reg := newTestPeerRegistryFeatures("dashboard-a", peer.URL, "dashboard-b", true, nil)
	h := newABHarness(t, nil, "")
	mux := newLocalTestMux(t, h.dc, reg)

	for _, c := range []struct{ method, sub, body string }{
		{"POST", "ab", `{"image":"x:2","replicas":1}`},
		{"GET", "ab", ``},
		{"POST", "ab/split", `{"split":40}`},
		{"POST", "ab/groups", `{"groups":{"staff":"B"}}`},
		{"POST", "ab/abort", ``},
		{"POST", "ab/promote", `{"force":true,"confirm_aborted":true}`},
		{"POST", "ab/discard", `{"force":true}`},
		{"POST", "ab/reset", `{"force":false}`},
	} {
		rec := apiDo(t, mux, c.method, "/api/services/app/"+c.sub+"?host=dashboard-b", c.body)
		if rec.Code/100 != 2 {
			t.Fatalf("%s %s?host= = %d %s", c.method, c.sub, rec.Code, rec.Body.String())
		}
		mu.Lock()
		last := calls[len(calls)-1]
		mu.Unlock()
		if last.method != c.method || last.path != "/peer/services/app/"+c.sub || last.body != c.body {
			t.Fatalf("forwarded %+v, want %s /peer/services/app/%s %s", last, c.method, c.sub, c.body)
		}
	}
	if _, ok := h.m.store.get("app"); ok || len(h.bReplicas()) != 0 {
		t.Fatal("a forwarded op ran locally")
	}
	// GET ab is a read: it forwards to a peer without -peer-writes too.
	roMux := newLocalTestMux(t, h.dc, newTestPeerRegistryFeatures("dashboard-a", peer.URL, "dashboard-b", false, nil))
	if rec := apiDo(t, roMux, "GET", "/api/services/app/ab?host=dashboard-b", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"none"`) {
		t.Fatalf("GET ab to a read-only peer = %d %s", rec.Code, rec.Body.String())
	}
}

// TestABGuardsProxyOnlyLeg: B runs on a peer, so this host has no B and
// no record — only its proxy's /ab knows. Replace/spread are refused; a
// plain local canary stays manageable (it isn't B).
func TestABGuardsProxyOnlyLeg(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	h.px.set(abProxyExperiment{Service: "app", ID: "peer0001"})
	h.f.seedCanary("ghcr.io/org/app:v9", nil)
	mux := newLocalTestMux(t, h.dc, nil)
	for _, c := range []struct{ path, body string }{
		{"/api/services/app/replace", `{"image":"ghcr.io/org/app:v3"}`},
		{"/api/services/app/spread", `{"host":"dashboard-b"}`},
	} {
		if rec := apiDo(t, mux, "POST", c.path, c.body); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "A/B test") {
			t.Fatalf("%s = %d %s", c.path, rec.Code, rec.Body.String())
		}
	}
	if rec := apiDo(t, mux, "DELETE", "/api/services/app/canary", ""); rec.Code != http.StatusOK {
		t.Fatalf("discarding a plain local canary = %d %s", rec.Code, rec.Body.String())
	}
	if _, canary := countLiveCanary(h.f); canary != 0 {
		t.Fatal("plain canary not discarded")
	}
}

func TestPeerABHandlerAuth(t *testing.T) {
	px := newABFakeProxy(t)
	px.set(abProxyExperiment{Service: "app", ID: "abcd1234"}, abProxyExperiment{Service: "other", ID: "ffff0000"})
	do := func(h http.Handler, method, auth, q string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/peer/ab"+q, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(peerABHandler("", px.srv.URL), "GET", "Bearer ", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("no secret configured = %d", rec.Code)
	}
	h := peerABHandler("s3cret", px.srv.URL)
	if rec := do(h, "GET", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth = %d", rec.Code)
	}
	if rec := do(h, "GET", "Bearer wrong!", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret = %d", rec.Code)
	}
	if rec := do(h, "POST", "Bearer s3cret", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
	if rec := do(h, "GET", "Bearer s3cret", "?service=../x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad service = %d", rec.Code)
	}
	rec := do(h, "GET", "Bearer s3cret", "?service=app")
	var rep abProxyReport
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &rep) != nil || len(rep.Experiments) != 1 || rep.Experiments[0].ID != "abcd1234" {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	px.setDown(true)
	if rec := do(h, "GET", "Bearer s3cret", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("proxy down = %d", rec.Code)
	}
}

// TestABStatusMergesPeerReports: stats sum each host's OWN cumulative, and
// the peer's pins count toward the drain.
func TestABStatusMergesPeerReports(t *testing.T) {
	withFastAB(t)
	peerProxy := newABFakeProxy(t)
	peerDash := httptest.NewServer(peerABHandler("s3cret", peerProxy.srv.URL))
	t.Cleanup(peerDash.Close)
	reg := newTestPeerRegistryFeatures("dashboard-a", peerDash.URL, "dashboard-b", true, nil)
	h := newABHarness(t, reg, "s3cret")
	rec := abTestRecord(h.clock.now())
	rec.Phase, rec.Pending = abPhaseDiscarding, abPendingDiscard
	h.seedB(t, "b1", rec, false)
	now := h.clock.now().Unix()

	own := abExp(rec, now, now-3600)
	own.Cumulative.A = abProxyCounters{Requests: 100, Err5xx: 1}
	own.Cumulative.B = abProxyCounters{Requests: 50, Transport: 5}
	own.Merged.Cumulative.A = abProxyCounters{Requests: 999999} // never summed
	h.px.set(own)
	peer := abExp(rec, now, now-60)
	peer.Cumulative.A = abProxyCounters{Requests: 200, Err5xx: 2}
	peer.Cumulative.B = abProxyCounters{Requests: 70}
	peerProxy.set(peer)

	st := h.m.Status(context.Background(), "app")
	if st.Stats.A.Requests != 300 || st.Stats.A.Errors != 3 || st.Stats.B.Requests != 125 || st.Stats.B.Errors != 5 {
		t.Fatalf("stats = %+v", st.Stats)
	}
	if len(st.PerHost) != 2 || st.PerHost[1].Host != "dashboard-b" || !st.PerHost[1].Reachable {
		t.Fatalf("per_host = %+v", st.PerHost)
	}
	if st.Drain == nil || st.Drain.Variant != "B" || st.Drain.Drained || st.Drain.LastPinnedAt != now-60 || st.Drain.ActiveSessionsApprox != 4 {
		t.Fatalf("drain = %+v", st.Drain)
	}
	if st.Pending == nil || *st.Pending != abPendingDiscard {
		t.Fatalf("pending = %v", st.Pending)
	}

	// The peer's proxy goes away: deadline only.
	peerProxy.setDown(true)
	st = h.m.Status(context.Background(), "app")
	if !st.Drain.DeadlineOnly || st.PerHost[1].Reachable {
		t.Fatalf("drain with peer down = %+v / %+v", st.Drain, st.PerHost)
	}
	// And the dashboard-level secret never reaches the response.
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "s3cret") {
		t.Fatal("status leaked the peer secret")
	}
}

// ---- drain across proxy / dashboard restarts ----

// TestABDrainProxyRestartMidDrain: a proxy that restarts mid-drain forgets
// its pins; it must not let B look idle before session_idle has passed
// since it started tracking again.
func TestABDrainProxyRestartMidDrain(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	h.seedB(t, "b1", abTestRecord(h.clock.now()), false)
	if _, err := h.m.Discard("app", false, false); err != nil {
		t.Fatal(err)
	}
	t0 := h.clock.now()
	cur := h.record(t)
	h.px.set(abExp(cur, t0.Unix(), t0.Unix()-30))
	h.m.tick(context.Background())
	if got := h.record(t).PinnedMaxB; got != t0.Unix()-30 {
		t.Fatalf("persisted B max = %d", got)
	}
	// The proxy restarts 25 minutes in: no pins, tracking from now.
	h.clock.advance(25 * time.Minute)
	e := abExp(cur, 0, 0)
	e.TrackingSince = h.clock.now().Unix()
	h.px.set(e)
	// Past the last pin + session_idle, but not past restart + session_idle.
	h.clock.advance(6 * time.Minute)
	h.m.tick(context.Background())
	if len(h.bReplicas()) != 1 {
		t.Fatal("B removed on a restarted proxy's empty pin report")
	}
	if st := h.m.Status(context.Background(), "app"); st.Drain == nil || st.Drain.DrainsBy != e.TrackingSince+int64(abDefaultSessionIdle/time.Second) {
		t.Fatalf("drain view = %+v", st.Drain)
	}
	h.clock.advance(25 * time.Minute)
	h.m.tick(context.Background())
	if len(h.bReplicas()) != 0 {
		t.Fatal("B not removed once session_idle passed since the restart")
	}
}

// TestABDrainPersistedMaxSurvivesDashboardRestart: the max pin ever seen
// is in the store, so a restarted dashboard still waits for it even when
// every proxy now reports an older one.
func TestABDrainPersistedMaxSurvivesDashboardRestart(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	h.seedB(t, "b1", abTestRecord(h.clock.now()), false)
	if _, err := h.m.Discard("app", false, false); err != nil {
		t.Fatal(err)
	}
	t0 := h.clock.now()
	cur := h.record(t)
	h.px.set(abExp(cur, 0, t0.Unix()-30))
	h.m.tick(context.Background())

	// A new manager on the same store and containers, 20 minutes later.
	h2 := newABHarnessOn(t, h.f, h.dc, nil, "", h.path)
	h2.clock.t = t0.Add(20 * time.Minute)
	h2.px.set(abExp(cur, 0, t0.Unix()-3600))
	if got := h2.record(t).PinnedMaxB; got != t0.Unix()-30 {
		t.Fatalf("reloaded B max = %d", got)
	}
	h2.m.tick(context.Background())
	if len(h2.bReplicas()) != 1 {
		t.Fatal("B removed before session_idle passed since the persisted last pin")
	}
	h2.clock.advance(10 * time.Minute)
	h2.m.tick(context.Background())
	if len(h2.bReplicas()) != 0 {
		t.Fatal("B not removed once drained")
	}
}

// ---- cross-host promote ----

type abPeerHost struct {
	h      *abHarness
	srv    *httptest.Server
	writes atomic.Bool
	order  *orderLog
}

// newABPromoteMesh: the origin runs B (and an A replica); the peer runs
// two A replicas of "app" and its proxy lists the test via the mesh.
func newABPromoteMesh(t *testing.T) (*abHarness, *abPeerHost, *abRecord) {
	t.Helper()
	withFastAB(t)
	prevPoll := abPeerPromotePoll
	abPeerPromotePoll = 5 * time.Millisecond
	t.Cleanup(func() { abPeerPromotePoll = prevPoll })
	setInternalToken(t)

	p := &abPeerHost{h: newABHarness(t, nil, ""), order: &orderLog{}}
	p.h.f.seedMember("p2", "goproxy-app-2", "", 0, []string{"A=1"}, cenvTemplateHealth)
	p.writes.Store(true)
	rom := newRollingOpManager(p.h.dc)
	onb := newTestOnboardedStore(t)
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/peer/services" {
			json.NewEncoder(w).Encode(peerServicesResp{Identity: "dashboard-b", Services: []Service{{Name: "app", Replicas: 2}}})
			return
		}
		peerServicesMutateHandler("s3cret", "dashboard-b", p.h.dc, onb, nil, nil, "", p.h.px.srv.URL, p.writes.Load(), rom).ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)

	reg := newTestPeerRegistryFeatures("dashboard-a", p.srv.URL, "dashboard-b", true, nil)
	h := newABHarness(t, reg, "s3cret")
	rec := abTestRecord(h.clock.now())
	h.seedB(t, "b1", rec, false)
	h.px.set(abExp(rec, 0, 0))
	p.h.px.set(abExp(rec, 0, 0))
	h.f.onCreate = func(name string) { p.order.add("origin:" + name) }
	p.h.f.onCreate = func(name string) { p.order.add("peer:" + name) }
	return h, p, rec
}

func abAppImages(f *cenvFakeDocker) map[string]string {
	out := map[string]string{}
	for _, c := range f.list() {
		if c.Labels[labelService] == "app" || c.name() == "app" {
			out[c.name()] = c.Image
		}
	}
	return out
}

func TestABPromoteRollsPeersFirst(t *testing.T) {
	h, p, rec := newABPromoteMesh(t)

	// A plain rolling replace is still refused on the peer while the test runs.
	req := httptest.NewRequest(http.MethodPost, "/peer/services/app/rolling-replace", strings.NewReader(`{"image":"ghcr.io/org/app:v2"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	w := httptest.NewRecorder()
	p.srv.Config.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("plain peer rolling-replace = %d %s", w.Code, w.Body.String())
	}

	if _, err := h.m.Promote("app", true, false, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.m.store.get("app"); ok {
		t.Fatalf("record not finalized: %+v", h.record(t))
	}
	for name, img := range abAppImages(p.h.f) {
		if img != rec.Image {
			t.Fatalf("peer %s runs %s after promote", name, img)
		}
	}
	if live, canary := countLiveCanary(h.f); canary != 0 || live == 0 {
		t.Fatalf("origin after promote: live=%d canary=%d", live, canary)
	}
	seq := p.order.snapshot()
	sawOrigin := false
	peerCreates := 0
	for _, s := range seq {
		if strings.HasPrefix(s, "origin:") {
			sawOrigin = true
		} else if sawOrigin {
			t.Fatalf("peer replaced after the local promote started: %v", seq)
		} else {
			peerCreates++
		}
	}
	if peerCreates != 2 || !sawOrigin {
		t.Fatalf("create order = %v", seq)
	}
}

func TestABPromotePeerFailureRetriesNotFinalized(t *testing.T) {
	h, p, rec := newABPromoteMesh(t)
	p.writes.Store(false)

	if _, err := h.m.Promote("app", true, false, false); err == nil {
		t.Fatal("promote finalized with a peer refusing writes")
	}
	cur := h.record(t)
	want := "peer dashboard-b refuses writes — promote there by hand or enable -peer-writes"
	if cur.Op != abOpFinalizePromote || cur.Pending != abPendingPromote || cur.LastError != want {
		t.Fatalf("record after refused peer = op %q pending %q err %q", cur.Op, cur.Pending, cur.LastError)
	}
	if len(h.bReplicas()) != 1 {
		t.Fatal("local B promoted although a peer wasn't")
	}

	// A failing rolling replace on the peer: still not finalized.
	p.writes.Store(true)
	p.h.f.setUnhealthy(func(createBody) bool { return true })
	h.clock.advance(abRetryInterval)
	h.m.tick(context.Background())
	cur = h.record(t)
	if cur.Op != abOpFinalizePromote || !strings.Contains(cur.LastError, "rolling replace to "+rec.Image+" failed") || len(h.bReplicas()) != 1 {
		t.Fatalf("after failing peer replace: op %q err %q B %d", cur.Op, cur.LastError, len(h.bReplicas()))
	}

	// Healthy again: nothing happens before the retry interval, then it finalizes.
	p.h.f.setUnhealthy(nil)
	h.m.tick(context.Background())
	if _, ok := h.m.store.get("app"); !ok || len(h.bReplicas()) != 1 {
		t.Fatal("retried before abRetryInterval")
	}
	h.clock.advance(abRetryInterval)
	h.m.tick(context.Background())
	if _, ok := h.m.store.get("app"); ok {
		t.Fatalf("not finalized on retry: %+v", h.record(t))
	}
	for name, img := range abAppImages(p.h.f) {
		if img != rec.Image {
			t.Fatalf("peer %s runs %s after promote", name, img)
		}
	}
}

// ---- relabel never flaps the proxy's config pick ----

// abProxyPick mirrors cmd/proxy abScan's choice: among running B replicas,
// the smallest id, then the smallest name (string order), supplies the
// config. Returns "id/split", or "" with no B.
func abProxyPick(f *cenvFakeDocker) string {
	var bs []dockerContainer
	for _, c := range f.list() {
		if c.Labels[labelABVariant] == "B" && c.State == "running" {
			bs = append(bs, c)
		}
	}
	if len(bs) == 0 {
		return ""
	}
	sort.Slice(bs, func(i, j int) bool {
		if bs[i].Labels[labelABID] != bs[j].Labels[labelABID] {
			return bs[i].Labels[labelABID] < bs[j].Labels[labelABID]
		}
		return bs[i].name() < bs[j].name()
	})
	return bs[0].Labels[labelABID] + "/" + bs[0].Labels[labelABSplit]
}

func TestABRelabelPickSwitchesOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reset     bool
		unhealthy bool
	}{
		{name: "split"},
		{name: "reset", reset: true},
		{name: "split-gate-fails", unhealthy: true},
		{name: "reset-gate-fails", reset: true, unhealthy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFastAB(t)
			h := newABHarness(t, nil, "")
			old := abTestRecord(h.clock.now())
			// 8 and 9: the next indexes cross into two digits.
			for _, n := range []int{8, 9} {
				labels := map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080", labelCanary: "true"}
				for k, v := range old.labels() {
					labels[k] = v
				}
				id := fmt.Sprintf("b%d", n)
				name := abCanaryName("app", n)
				h.f.seed(dockerContainer{ID: id, Names: []string{"/" + name}, Image: old.Image, State: "running", Status: "Up 1 minute", Labels: labels},
					cenvInspect{env: []string{"A=1"}, health: cenvTemplateHealth, edge: []string{name}, networks: map[string][]string{}})
			}
			next := old.clone()
			next.Config[labelABSplit] = "50"
			if tc.reset {
				next.ID = newABIDAfter(old.ID)
			}
			var mu sync.Mutex
			picks := []string{abProxyPick(h.f)}
			h.f.onMutate = func() {
				p := abProxyPick(h.f)
				mu.Lock()
				if picks[len(picks)-1] != p {
					picks = append(picks, p)
				}
				mu.Unlock()
			}
			if tc.unhealthy {
				h.f.setUnhealthy(func(createBody) bool { return true })
			}
			err := h.dc.relabelABReplicas(context.Background(), "app", next.Image, next.labels())
			mu.Lock()
			defer mu.Unlock()
			want := []string{old.ID + "/20", next.ID + "/50"}
			if tc.unhealthy {
				want = want[:1]
				if err == nil {
					t.Fatal("relabel succeeded with every new replica unhealthy")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(picks, want) {
				t.Fatalf("proxy config pick went %v, want %v", picks, want)
			}
		})
	}
}

func TestABNewIDAfterSortsAfter(t *testing.T) {
	for _, old := range []string{"00000000", "feedf00d", "ffffffff", "zzzz", "zzzzzzzzzzzzzzz", newABID()} {
		for i := 0; i < 20; i++ {
			if id := newABIDAfter(old); id <= old || !abIDRe.MatchString(id) {
				t.Fatalf("newABIDAfter(%q) = %q", old, id)
			}
		}
	}
}

// TestABRelabelNamesStayBounded: many relabels of a full-size B keep
// fixed-width names (no unbounded growth to stay in sort order).
func TestABRelabelNamesStayBounded(t *testing.T) {
	withFastAB(t)
	h := newABHarness(t, nil, "")
	rec := abTestRecord(h.clock.now())
	rec.Replicas = abMaxReplicas
	h.seedB(t, "b1", rec, false)
	for i := 2; i <= abMaxReplicas; i++ {
		labels := map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080", labelCanary: "true"}
		for k, v := range rec.labels() {
			labels[k] = v
		}
		name := abCanaryName("app", i)
		h.f.seed(dockerContainer{ID: fmt.Sprintf("b%d", i), Names: []string{"/" + name}, Image: rec.Image, State: "running", Status: "Up 1 minute", Labels: labels},
			cenvInspect{env: []string{"A=1"}, health: cenvTemplateHealth, edge: []string{name}, networks: map[string][]string{}})
	}
	start := time.Now()
	for i := 0; i < 25; i++ {
		rec.Config[labelABSplit] = strconv.Itoa(10 + i)
		if err := h.dc.relabelABReplicas(context.Background(), "app", rec.Image, rec.labels()); err != nil {
			t.Fatal(err)
		}
	}
	bs := h.bReplicas()
	if len(bs) != abMaxReplicas {
		t.Fatalf("%d B replicas after relabels", len(bs))
	}
	for _, b := range bs {
		if n := b.name(); len(n) != len(abCanaryName("app", 1)) {
			t.Fatalf("B name %q grew", n)
		}
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("25 relabels took %s", d)
	}
}
