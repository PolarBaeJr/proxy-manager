package main

// abManager owns every A/B test's lifecycle on this host
// (docs/AB_TESTING_PLAN.md §1.1, §1.10, §1.11): start, abort, split/groups,
// reset, and the drain-gated promote/discard. Shaped like rolloutManager —
// a per-service lock, a Run loop — plus persistence: the store holds the
// desired state, written BEFORE any container is touched, so a restarted
// dashboard (or the next Run tick after a failure) converges B onto it.
//
// Docker work runs on a detached context (abOpTimeout): every op recreates
// B, which is slow (health gates, settle delays), and must never be cut
// short by a client hanging up — that would leave B replicas with mixed
// labels. The HTTP layer starts ops asynchronously and answers 202.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	abClaimOwner = "ab-test"
	abAuditUser  = "ab-test"

	abOpStart           = "start"
	abOpRelabel         = "relabel"
	abOpFinalizePromote = "finalize_promote"
	abOpFinalizeDiscard = "finalize_discard"

	abProxyFetchTimeout = 2 * time.Second
	abProxyMaxBody      = 4 << 20
)

// Package vars only so tests can shrink them — same seam as
// rolloutCheckInterval.
var (
	abCheckInterval = 15 * time.Second
	// abRetryInterval spaces out Run's retries of an op that failed, so a B
	// that keeps failing its health gate isn't recreated every tick.
	abRetryInterval = 5 * time.Minute
	abOpTimeout     = rollingOpTimeout
	// abPeerPromoteTimeout bounds the wait for one peer's rolling replace
	// onto B's image (finalizePromote); abPeerPromotePoll is how often its
	// status is read meanwhile.
	abPeerPromoteTimeout = 10 * time.Minute
	abPeerPromotePoll    = 2 * time.Second
)

// ServiceABTest is Service.ABTest: the services list's one-line summary.
type ServiceABTest struct {
	ID      string `json:"id"`
	Image   string `json:"image,omitempty"`
	Phase   string `json:"phase"`
	Pending string `json:"pending,omitempty"`
	Op      string `json:"op,omitempty"`
}

// abTestFromLabels builds the summary from a B container's labels. Values
// reach the UI, so anything outside the proxy's own grammar is replaced.
func abTestFromLabels(ct dockerContainer) *ServiceABTest {
	id := ct.Labels[labelABID]
	if !abIDRe.MatchString(id) {
		id = "invalid"
	}
	phase := ct.Labels[labelABPhase]
	switch phase {
	case abPhaseRunning, abPhaseAborted, abPhasePromoting, abPhaseDiscarding:
	case "":
		phase = abPhaseRunning
	default:
		phase = "invalid"
	}
	return &ServiceABTest{ID: id, Image: ct.Image, Phase: phase}
}

// isABCanary: a canary set is an A/B test's B when any member carries a
// proxy.ab.variant label (the dashboard writes it on every B).
func isABCanary(canary []dockerContainer) bool {
	for _, ct := range canary {
		if ct.Labels[labelABVariant] != "" {
			return true
		}
	}
	return false
}

// cloneOwnEnvAndSpec clones ct from ITSELF — its own env, spec and labels —
// never re-resolving a central env. B must keep exactly the env it is being
// tested (or was tested) on across relabels and its promote. labels is ct's
// shared map: copy before writing.
func (c *dockerClient) cloneOwnEnvAndSpec(ctx context.Context, ct dockerContainer) (templateClone, error) {
	env, err := c.inspectEnv(ctx, ct.ID)
	if err != nil {
		return templateClone{}, fmt.Errorf("inspect %s env: %w", ct.name(), err)
	}
	clone, err := c.inspectCloneSpec(ctx, ct.ID)
	if err != nil {
		return templateClone{}, fmt.Errorf("inspect %s clone spec: %w", ct.name(), err)
	}
	return templateClone{env: env, labels: ct.Labels, clone: clone}, nil
}

func abLabelsMatch(have, want map[string]string) bool {
	for k, v := range have {
		if strings.HasPrefix(k, labelABPrefix) && want[k] != v {
			return false
		}
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// relabelABReplicas gives every B replica of name exactly the proxy.ab.*
// labels in want, by surge recreate (Docker can't edit labels): create the
// new replica, wait for waitReplicaReady, then remove the old one. A replica
// already carrying want is left alone, so this is idempotent — a retry
// after a partial failure only touches what is still behind. A new replica
// that fails its gate is removed; its predecessor keeps serving.
//
// image is the record's image, not the list endpoint's, which can decay to
// a bare digest (looksLikeBareDigest).
//
// The proxy reads a test's config from its B replica with the smallest id,
// then smallest name (cmd/proxy abScan, string order) — a reset's new id
// sorts after the old one (newABIDAfter), so within one relabel this is
// the smallest name. B names are fixed-width (abCanaryName), so
// every new replica sorts after every existing one and the pick stays on
// an old replica (old config) until the last old one is removed, then
// moves once to a new replica that already passed its gate — never back,
// not even when a new replica fails its gate and is removed. The flip
// side: the proxy sees the new config only at the end of the surge.
func (c *dockerClient) relabelABReplicas(ctx context.Context, name, image string, want map[string]string) error {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return err
	}
	bs := canaryOnly(all)
	if len(bs) == 0 {
		return fmt.Errorf("%q has no B replicas", name)
	}
	idx := nextCanaryReplicaIndex(all, name)
	last := ""
	for _, ct := range bs {
		if n := ct.name(); n > last {
			last = n
		}
	}
	for _, old := range bs {
		if abLabelsMatch(old.Labels, want) {
			continue
		}
		tc, err := c.cloneOwnEnvAndSpec(ctx, old)
		if err != nil {
			return err
		}
		labels := map[string]string{}
		for k, v := range tc.labels {
			if strings.HasPrefix(k, labelABPrefix) {
				continue
			}
			labels[k] = v
		}
		for k, v := range want {
			labels[k] = v
		}
		cname := abCanaryName(name, idx)
		idx++
		if cname <= last {
			log.Printf("ab %s: new B %s sorts before existing %s — the proxy may read its config before it is ready", name, cname, last)
		}
		last = cname
		id, err := c.createContainer(ctx, cname, createBody{
			Image: firstNonEmpty(image, old.Image), Labels: labels, Env: tc.env, Healthcheck: tc.clone.Healthcheck, HostConfig: hostConfig{Mounts: tc.clone.Mounts},
			ManagedAliases: tc.clone.ManagedAliases, ExtraNetworks: tc.clone.ExtraNetworks,
		})
		if err != nil {
			return fmt.Errorf("create B %s: %w", cname, err)
		}
		if err := c.startContainer(ctx, id); err != nil {
			_ = c.removeContainer(ctx, id)
			return fmt.Errorf("start B %s: %w", cname, err)
		}
		if err := c.waitReplicaReady(ctx, name, id); err != nil {
			_ = c.stopContainer(ctx, id)
			_ = c.removeContainer(ctx, id)
			return fmt.Errorf("B %s: %w", cname, err)
		}
		_ = c.stopContainer(ctx, old.ID)
		if err := c.removeContainer(ctx, old.ID); err != nil {
			log.Printf("ab %s: failed to remove old B %s: %v (new one is running)", name, old.name(), err)
		}
	}
	return nil
}

// abCanaryName names an A/B B replica. Zero-padded, so string order (the
// proxy's config pick) matches index order.
func abCanaryName(svc string, idx int) string {
	return fmt.Sprintf("goproxy-%s-canary-%06d", svc, idx)
}

// abLocalLabels reports whether any local container of name carries A/B
// labels — the label leg of abActive, which works with no manager at all.
func (c *dockerClient) abLocalLabels(ctx context.Context, name string) bool {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return false
	}
	for _, ct := range all {
		if ct.Labels[labelABVariant] != "" {
			return true
		}
	}
	return false
}

// abCanaryLocal: name's canary (if any) is this host's A/B B — a record
// here, or B labels on a local container.
func (c *dockerClient) abCanaryLocal(ctx context.Context, name string) bool {
	if c.ab != nil {
		if _, ok := c.ab.store.get(name); ok {
			return true
		}
	}
	return c.abLocalLabels(ctx, name)
}

// abActive is §1.12's "a test is active for name": a record here, B labels
// on a local container, or the local proxy's /ab listing the test (which
// also covers a test whose B runs on a peer). Fails open on errors — every
// guarded action re-reads live state before it mutates.
func (c *dockerClient) abActive(ctx context.Context, name string) bool {
	if c.abCanaryLocal(ctx, name) {
		return true
	}
	return c.ab != nil && c.ab.proxyListsTest(ctx, name)
}

// ---- Proxy /ab (cmd/proxy/abtest.go abReport), read side ----

type abProxyCounters struct {
	Requests  uint64   `json:"requests"`
	Err5xx    uint64   `json:"err5xx"`
	Transport uint64   `json:"transport"`
	Failover  uint64   `json:"failover"`
	ErrorRate float64  `json:"error_rate"`
	P50Ms     float64  `json:"p50_ms"`
	P95Ms     float64  `json:"p95_ms"`
	Hist      []uint64 `json:"hist"`
}

type abProxyPair[T any] struct {
	A T `json:"A"`
	B T `json:"B"`
}

type abProxyPinned struct {
	ActiveSessions int   `json:"active_sessions"`
	LastSeen       int64 `json:"last_seen"`
}

type abProxyAbort struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
	At     int64  `json:"at"`
	Source string `json:"source"`
}

type abProxyWindow struct {
	Index int64                        `json:"index"`
	Start int64                        `json:"start"`
	V     abProxyPair[abProxyCounters] `json:"variants"`
}

type abProxyJudge struct {
	Status string `json:"status"`
	Streak int    `json:"streak"`
}

type abProxyMerged struct {
	Peers      int                          `json:"peers"`
	Cumulative abProxyPair[abProxyCounters] `json:"cumulative"`
	Windows    []abProxyWindow              `json:"windows"`
}

type abProxyExperiment struct {
	Service        string                       `json:"service"`
	ID             string                       `json:"id"`
	Phase          string                       `json:"phase"`
	PhaseAt        int64                        `json:"phase_at"`
	Config         map[string]string            `json:"config"`
	Started        int64                        `json:"started"`
	Epoch          int64                        `json:"epoch"`
	Abort          *abProxyAbort                `json:"abort,omitempty"`
	BBackendsLocal int                          `json:"b_backends_local"`
	BBackendsPeer  int                          `json:"b_backends_peer"`
	Cumulative     abProxyPair[abProxyCounters] `json:"cumulative"`
	Windows        []abProxyWindow              `json:"windows"`
	Frozen         bool                         `json:"frozen"`
	Pinned         abProxyPair[abProxyPinned]   `json:"pinned"`
	Failover       abProxyPair[uint64]          `json:"failover"`
	Draining       abProxyPair[uint64]          `json:"draining"`
	Judge          abProxyJudge                 `json:"judge"`
	Merged         abProxyMerged                `json:"merged"`
	TrackingSince  int64                        `json:"tracking_since"`
}

type abProxyReport struct {
	HistBoundsMs []float64           `json:"hist_bounds_ms"`
	Experiments  []abProxyExperiment `json:"experiments"`
}

func (r abProxyReport) experiment(svc string) *abProxyExperiment {
	for i := range r.Experiments {
		if r.Experiments[i].Service == svc {
			return &r.Experiments[i]
		}
	}
	return nil
}

// fetchProxyAB reads the proxy's /ab. Decoding into the known shape (rather
// than relaying bytes) is what keeps /peer/ab from passing anything else on.
func fetchProxyAB(ctx context.Context, client *http.Client, proxyURL, svc string) (abProxyReport, error) {
	if proxyURL == "" {
		proxyURL = defaultProxyURL
	}
	u := strings.TrimRight(proxyURL, "/") + "/ab"
	if svc != "" {
		u += "?service=" + url.QueryEscape(svc)
	}
	rctx, cancel := context.WithTimeout(ctx, abProxyFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return abProxyReport{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return abProxyReport{}, fmt.Errorf("proxy unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return abProxyReport{}, fmt.Errorf("proxy /ab: %s", resp.Status)
	}
	var rep abProxyReport
	if err := json.NewDecoder(io.LimitReader(resp.Body, abProxyMaxBody)).Decode(&rep); err != nil {
		return abProxyReport{}, fmt.Errorf("proxy /ab: bad response")
	}
	if rep.Experiments == nil {
		rep.Experiments = []abProxyExperiment{}
	}
	return rep, nil
}

// abHostReport is one proxy's view of a service's test: this host's own
// proxy first, then each configured peer's (via its dashboard's /peer/ab).
type abHostReport struct {
	Host      string
	Reachable bool
	Error     string
	Bounds    []float64
	Exp       *abProxyExperiment
}

// ---- Manager ----

type abManager struct {
	dc       *dockerClient
	store    *abStore
	onb      *OnboardedStore
	rm       *rolloutManager
	rom      *rollingOpManager
	registry *PeerRegistry
	secret   string
	proxyURL string
	client   *http.Client
	now      func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newABManager(dc *dockerClient, store *abStore, onb *OnboardedStore, rm *rolloutManager, rom *rollingOpManager, registry *PeerRegistry, secret, proxyURL string) *abManager {
	if proxyURL == "" {
		proxyURL = defaultProxyURL
	}
	return &abManager{
		dc: dc, store: store, onb: onb, rm: rm, rom: rom, registry: registry, secret: secret, proxyURL: proxyURL,
		client: &http.Client{Timeout: abProxyFetchTimeout},
		now:    time.Now,
		locks:  map[string]*sync.Mutex{},
	}
}

func (m *abManager) lock(svc string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[svc]
	if !ok {
		l = &sync.Mutex{}
		m.locks[svc] = l
	}
	return l
}

func (m *abManager) identity() string {
	if m.registry != nil && m.registry.Identity() != "" {
		return m.registry.Identity()
	}
	return "local"
}

func (m *abManager) proxyListsTest(ctx context.Context, svc string) bool {
	rep, err := fetchProxyAB(ctx, m.client, m.proxyURL, svc)
	return err == nil && rep.experiment(svc) != nil
}

// gather asks this host's proxy and every configured peer for svc's test.
func (m *abManager) gather(ctx context.Context, svc string) []abHostReport {
	out := []abHostReport{{Host: m.identity()}}
	if rep, err := fetchProxyAB(ctx, m.client, m.proxyURL, svc); err != nil {
		out[0].Error = err.Error()
	} else {
		out[0].Reachable, out[0].Bounds, out[0].Exp = true, rep.HistBoundsMs, rep.experiment(svc)
	}
	if m.registry == nil {
		return out
	}
	peers := m.registry.Peers()
	status := m.registry.Status()
	res := make([]abHostReport, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			h := abHostReport{Host: firstNonEmpty(status[p].Identity, p)}
			var rep abProxyReport
			if m.secret == "" {
				h.Error = "no peer secret"
			} else if err := peerGET(ctx, m.client, p, m.secret, "/peer/ab?service="+url.QueryEscape(svc), &rep); err != nil {
				h.Error = "unreachable"
			} else {
				h.Reachable, h.Bounds, h.Exp = true, rep.HistBoundsMs, rep.experiment(svc)
			}
			res[i] = h
		}(i, p)
	}
	wg.Wait()
	return append(out, res...)
}

// abDrainView is GET ab's drain block (§1.10).
type abDrainView struct {
	Variant              string `json:"variant"`
	ActiveSessionsApprox int    `json:"active_sessions_approx"`
	LastPinnedAt         int64  `json:"last_pinned_at"`
	Drained              bool   `json:"drained"`
	DrainsBy             int64  `json:"drains_by"`
	// DeadlineOnly: some proxy couldn't be asked, so only the server-side
	// deadline (phase_at + max_session) can end the drain.
	DeadlineOnly bool `json:"deadline_only,omitempty"`
}

// abDrain decides whether variant v of rec's test has drained: no pinned
// traffic for v anywhere in the mesh for session_idle, or the deadline
// phase_at + max_session has passed.
//
// The idle path is only as good as the pin data behind it, so it counts
// only when every proxy answered, every one reports this test id, and each
// says since when it has tracked pins (tracking_since). A proxy that
// restarted has no memory of earlier pins: its "no pins" is only proof for
// the time since it started tracking, so the idle clock runs from
// max(last pin ever seen, latest tracking_since). The last pin is also the
// max ever observed for this id (rec.PinnedMax*, persisted by Run), never
// only what the proxies report now. Anything less trustworthy leaves only
// the deadline.
func abDrain(rec *abRecord, v string, hosts []abHostReport, now time.Time) abDrainView {
	deadline := rec.PhaseAt + int64(rec.maxSession()/time.Second)
	d := abDrainView{Variant: v, DrainsBy: deadline, LastPinnedAt: rec.pinnedMax(v)}
	trusted := true
	var tracking int64
	for _, h := range hosts {
		if !h.Reachable || h.Exp == nil || h.Exp.ID != rec.ID || h.Exp.TrackingSince <= 0 {
			trusted = false
			continue
		}
		if h.Exp.TrackingSince > tracking {
			tracking = h.Exp.TrackingSince
		}
		p := h.Exp.Pinned.A
		if v == "B" {
			p = h.Exp.Pinned.B
		}
		d.ActiveSessionsApprox += p.ActiveSessions
		if p.LastSeen > d.LastPinnedAt {
			d.LastPinnedAt = p.LastSeen
		}
	}
	d.DeadlineOnly = !trusted
	if trusted {
		quiet := d.LastPinnedAt
		if tracking > quiet {
			quiet = tracking
		}
		if idleEnd := quiet + int64(rec.sessionIdle()/time.Second); idleEnd < d.DrainsBy {
			d.DrainsBy = idleEnd
		}
	}
	d.Drained = now.Unix() >= deadline || (trusted && d.DrainsBy < now.Unix())
	return d
}

// observePins raises rec's persisted per-variant last-pin maxima from the
// reports of hosts for this id. Reports whether anything rose.
func observePins(rec *abRecord, hosts []abHostReport) bool {
	changed := false
	for _, h := range hosts {
		if !h.Reachable || h.Exp == nil || h.Exp.ID != rec.ID {
			continue
		}
		if a := h.Exp.Pinned.A.LastSeen; a > rec.PinnedMaxA {
			rec.PinnedMaxA, changed = a, true
		}
		if b := h.Exp.Pinned.B.LastSeen; b > rec.PinnedMaxB {
			rec.PinnedMaxB, changed = b, true
		}
	}
	return changed
}

// drainVariant is the variant whose pinned sessions a phase waits out.
func drainVariant(phase string) string {
	switch phase {
	case abPhaseAborted, abPhaseDiscarding:
		return "B"
	case abPhasePromoting:
		return "A"
	}
	return ""
}

// ---- Operations ----

// mutate is the shape of every operation: under svc's lock (never waited
// on — a second op while one runs is a 409), prep validates against the
// current record and returns the next one, which is persisted BEFORE exec
// touches any container. exec then runs on a detached context, in the
// background when async; it releases the lock and the service claim when
// done. A nil exec just persists next.
func (m *abManager) mutate(svc string, async bool, prep func(cur *abRecord, now time.Time) (*abRecord, error), exec func(ctx context.Context, rec *abRecord) error) (*abRecord, error) {
	l := m.lock(svc)
	if !l.TryLock() {
		return nil, abConflict("an A/B operation on %q is already in progress", svc)
	}
	cur, _ := m.store.get(svc)
	next, err := prep(cur, m.now())
	if err != nil {
		l.Unlock()
		return nil, err
	}
	if !m.dc.claims.tryClaim(svc, abClaimOwner) {
		l.Unlock()
		return nil, abConflict("%q is being changed by %s — try again when it finishes", svc, m.dc.claims.holder(svc))
	}
	// A new op starts clean: an earlier failure's error would otherwise
	// show as this op's until exec finishes.
	next.LastAttempt, next.LastError = m.now().Unix(), ""
	if err := m.store.put(svc, next); err != nil {
		m.dc.claims.release(svc, abClaimOwner)
		l.Unlock()
		return nil, err
	}
	run := func() error {
		defer l.Unlock()
		defer m.dc.claims.release(svc, abClaimOwner)
		if exec == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), abOpTimeout)
		defer cancel()
		return exec(ctx, next.clone())
	}
	if async {
		go func() { _ = run() }()
		return next.clone(), nil
	}
	err = run()
	if rec, ok := m.store.get(svc); ok {
		return rec, err
	}
	return next.clone(), err
}

// startPreflight refuses a start the §1.12 / §3 rules don't allow. Runs
// under svc's lock.
func (m *abManager) startPreflight(ctx context.Context, svc string) error {
	if m.onb != nil {
		if _, ok := m.onb.Get(svc); ok {
			return abBadRequest("%q is an onboarded service — A/B tests support label-managed services only (v1)", svc)
		}
	}
	all, err := m.dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, svc))
	if err != nil {
		return err
	}
	if len(liveOnly(all)) == 0 {
		return abError{code: http.StatusNotFound, msg: fmt.Sprintf("service %q not found (no live replicas)", svc)}
	}
	if len(canaryOnly(all)) > 0 {
		return abConflict("%q already has a canary — promote or discard it first", svc)
	}
	if m.rm != nil {
		if st, ok := m.rm.get(svc); ok && rolloutActive(st.Status) {
			return abConflict("%q has an active rollout", svc)
		}
	}
	if m.rom != nil {
		if st, ok := m.rom.get(svc); ok && rollingOpActive(st.Status) {
			return abConflict("%q has an active rolling replace", svc)
		}
	}
	if ce := centralEnvOf(m.dc); ce != nil && ce.sync.busy(svc) {
		return abConflict("%q has a central env propagation in progress", svc)
	}
	if m.dc.abLocalLabels(ctx, svc) || m.proxyListsTest(ctx, svc) {
		return abConflict("%q already has an A/B test (possibly on a peer)", svc)
	}
	return nil
}

// Start validates req, persists a new test and creates B (§1.1).
func (m *abManager) Start(ctx context.Context, svc string, req abStartRequest, async bool) (*abRecord, error) {
	cfg, err := abConfigLabels(req)
	if err != nil {
		return nil, err
	}
	return m.mutate(svc, async, func(cur *abRecord, now time.Time) (*abRecord, error) {
		if cur != nil {
			return nil, abConflict("%q already has an A/B test (%s)", svc, cur.ID)
		}
		if err := m.startPreflight(ctx, svc); err != nil {
			return nil, err
		}
		t := now.Unix()
		return &abRecord{
			ID: newABID(), Image: req.Image, Replicas: req.Replicas, Created: t, Started: t, Epoch: t,
			Phase: abPhaseRunning, PhaseAt: t, Config: cfg, Op: abOpStart,
		}, nil
	}, func(ctx context.Context, rec *abRecord) error { return m.execStart(ctx, svc, rec) })
}

func (m *abManager) execStart(ctx context.Context, svc string, rec *abRecord) error {
	err := m.dc.createCanaryReplicas(ctx, svc, ReplaceServiceRequest{Image: rec.Image}, rec.Replicas, rec.labels())
	if err == nil {
		err = m.dc.waitForCanaryHealthy(ctx, svc)
	}
	if err != nil {
		m.abandonStart(svc, rec, err.Error())
		return err
	}
	proxyRefresh(m.proxyURL)
	rec.Op, rec.LastError = "", ""
	if perr := m.store.put(svc, rec); perr != nil {
		log.Printf("ab %s: started %s but could not persist it: %v", svc, rec.ID, perr)
	}
	audit(nil, abAuditUser, "service.ab_started", fmt.Sprintf("%s id=%s image=%s", svc, rec.ID, rec.Image))
	return nil
}

// abandonStart removes whatever B a failed (or interrupted) start created
// and records why.
//
// Runs on its own context: the caller's may be the very one that just
// expired. The record is only dropped once B is confirmed gone — otherwise
// it is turned into a pending discard that Run retries, because a B left
// behind without a record would be re-adopted as a running test.
func (m *abManager) abandonStart(svc string, rec *abRecord, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), abOpTimeout)
	defer cancel()
	all, err := m.dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, svc))
	if err == nil && len(canaryOnly(all)) > 0 {
		err = m.dc.discardCanary(ctx, svc)
		proxyRefresh(m.proxyURL)
	}
	if err != nil {
		log.Printf("ab %s: could not remove B after a failed start: %v", svc, err)
		rec.Op, rec.Pending = abOpFinalizeDiscard, abPendingDiscard
		rec.LastError = "start failed (" + why + "); removing B failed: " + err.Error()
		rec.LastAttempt = m.now().Unix()
		if perr := m.store.put(svc, rec); perr != nil {
			log.Printf("ab %s: could not persist the failed start: %v", svc, perr)
		}
		audit(nil, abAuditUser, "service.ab_start_failed", fmt.Sprintf("%s id=%s: %s (B not yet removed)", svc, rec.ID, why))
		return
	}
	if err := m.store.finish(svc, abHistoryEntry{ID: rec.ID, Image: rec.Image, Started: rec.Started, Ended: m.now().Unix(), Outcome: "start_failed", Detail: why}); err != nil {
		log.Printf("ab %s: could not persist the failed start: %v", svc, err)
	}
	audit(nil, abAuditUser, "service.ab_start_failed", fmt.Sprintf("%s id=%s: %s", svc, rec.ID, why))
}

// execRelabel converges B onto rec's labels, then refreshes the proxy.
func (m *abManager) execRelabel(ctx context.Context, svc string, rec *abRecord) error {
	err := m.dc.relabelABReplicas(ctx, svc, rec.Image, rec.labels())
	proxyRefresh(m.proxyURL)
	if err != nil {
		rec.LastError = err.Error()
		rec.LastAttempt = m.now().Unix()
		if perr := m.store.put(svc, rec); perr != nil {
			log.Printf("ab %s: could not persist a relabel failure: %v", svc, perr)
		}
		audit(nil, abAuditUser, "service.ab_relabel_failed", fmt.Sprintf("%s id=%s: %s", svc, rec.ID, err.Error()))
		return err
	}
	rec.Op, rec.LastError = "", ""
	return m.store.put(svc, rec)
}

func (m *abManager) relabelExec(svc string) func(context.Context, *abRecord) error {
	return func(ctx context.Context, rec *abRecord) error { return m.execRelabel(ctx, svc, rec) }
}

// requireRecord is the common prep check of every op on an existing test.
func requireRecord(svc string, cur *abRecord) error {
	if cur == nil {
		return abError{code: http.StatusNotFound, msg: fmt.Sprintf("%q has no A/B test", svc)}
	}
	switch cur.Op {
	case abOpStart:
		return abConflict("%q's A/B test is still starting", svc)
	case abOpFinalizePromote, abOpFinalizeDiscard:
		return abConflict("%q's A/B test is being finalized (%s)", svc, cur.Pending)
	}
	return nil
}

// Abort stops sending new sessions to B (§1.10). Pins keep their variant.
func (m *abManager) Abort(svc string, async bool) (*abRecord, error) {
	return m.mutate(svc, async, func(cur *abRecord, now time.Time) (*abRecord, error) {
		if err := requireRecord(svc, cur); err != nil {
			return nil, err
		}
		if cur.Phase != abPhaseRunning {
			return nil, abConflict("%q's A/B test is %s, not running", svc, cur.Phase)
		}
		next := cur.clone()
		next.Phase, next.AbortReason, next.PhaseAt, next.Op = abPhaseAborted, "manual", now.Unix(), abOpRelabel
		return next, nil
	}, m.relabelExec(svc))
}

// SetSplit / SetGroups relabel B with the same id and a new epoch (§1.11).
func (m *abManager) SetSplit(svc string, split int, async bool) (*abRecord, error) {
	if err := abCheckInt("split", split, 0, 100); err != nil {
		return nil, err
	}
	return m.reconfigure(svc, async, func(cfg map[string]string) { cfg[labelABSplit] = strconv.Itoa(split) })
}

func (m *abManager) SetGroups(svc string, groups map[string]string, async bool) (*abRecord, error) {
	if err := abValidateGroups(groups); err != nil {
		return nil, err
	}
	return m.reconfigure(svc, async, func(cfg map[string]string) {
		if len(groups) == 0 {
			delete(cfg, labelABGroups)
		} else {
			cfg[labelABGroups] = abGroupsLabel(groups)
		}
	})
}

func (m *abManager) reconfigure(svc string, async bool, edit func(map[string]string)) (*abRecord, error) {
	return m.mutate(svc, async, func(cur *abRecord, now time.Time) (*abRecord, error) {
		if err := requireRecord(svc, cur); err != nil {
			return nil, err
		}
		if cur.Phase != abPhaseRunning && cur.Phase != abPhaseAborted {
			return nil, abConflict("%q's A/B test is %s — split and groups can't change now", svc, cur.Phase)
		}
		next := cur.clone()
		edit(next.Config)
		next.Epoch, next.Op = now.Unix(), abOpRelabel
		return next, nil
	}, m.relabelExec(svc))
}

// Reset starts a fresh cohort under a new id (§1.11). Every existing pin
// becomes invalid, so B must have drained first unless force.
func (m *abManager) Reset(ctx context.Context, svc string, force, async bool) (*abRecord, error) {
	var old abHistoryEntry
	return m.mutate(svc, async, func(cur *abRecord, now time.Time) (*abRecord, error) {
		if err := requireRecord(svc, cur); err != nil {
			return nil, err
		}
		if cur.Phase != abPhaseRunning && cur.Phase != abPhaseAborted {
			return nil, abConflict("%q's A/B test is %s — it can't be reset now", svc, cur.Phase)
		}
		if !force {
			if d := abDrain(cur, "B", m.gather(ctx, svc), now); !d.Drained {
				return nil, abConflict("B still has pinned sessions (drains by %s) — wait, or reset with force", time.Unix(d.DrainsBy, 0).UTC().Format(time.RFC3339))
			}
		}
		old = abHistoryEntry{ID: cur.ID, Image: cur.Image, Started: cur.Started, Ended: now.Unix(), Outcome: "reset"}
		next := cur.clone()
		t := now.Unix()
		next.ID, next.Started, next.Epoch, next.PhaseAt = newABIDAfter(cur.ID), t, t, t
		next.PinnedMaxA, next.PinnedMaxB, next.PeersPromoted = 0, 0, nil
		next.Phase, next.AbortReason, next.Pending, next.Op = abPhaseRunning, "", "", abOpRelabel
		return next, nil
	}, func(ctx context.Context, rec *abRecord) error {
		if err := m.store.appendHistory(svc, old); err != nil {
			log.Printf("ab %s: could not record the reset of %s: %v", svc, old.ID, err)
		}
		return m.execRelabel(ctx, svc, rec)
	})
}

// Discard moves the test to discarding (new sessions → A) and removes B
// once B has drained — or right away with force.
func (m *abManager) Discard(svc string, force, async bool) (*abRecord, error) {
	var finalizeNow bool
	return m.mutate(svc, async, func(cur *abRecord, now time.Time) (*abRecord, error) {
		if err := requireRecord(svc, cur); err != nil {
			return nil, err
		}
		if cur.Phase == abPhasePromoting {
			return nil, abConflict("%q is promoting B — wait for it, or promote with force", svc)
		}
		next := cur.clone()
		next.Pending = abPendingDiscard
		if force {
			finalizeNow = true
			next.Op = abOpFinalizeDiscard
			return next, nil
		}
		if cur.Phase == abPhaseDiscarding {
			return nil, abConflict("%q is already discarding B (waiting for its sessions to drain) — use force to skip the wait", svc)
		}
		// From aborted, new sessions already go to A: B has been draining
		// since phase_at, so the deadline stays where it is.
		if cur.Phase != abPhaseAborted {
			next.PhaseAt = now.Unix()
		}
		next.Phase, next.Op = abPhaseDiscarding, abOpRelabel
		return next, nil
	}, func(ctx context.Context, rec *abRecord) error {
		if finalizeNow {
			return m.finalizeDiscard(ctx, svc, rec)
		}
		return m.execRelabel(ctx, svc, rec)
	})
}

// Promote moves the test to promoting (new sessions → B) and makes B the
// live set once A has drained — or right away with force. Promoting an
// aborted B needs confirm_aborted.
func (m *abManager) Promote(svc string, force, confirmAborted, async bool) (*abRecord, error) {
	var finalizeNow bool
	return m.mutate(svc, async, func(cur *abRecord, now time.Time) (*abRecord, error) {
		if err := requireRecord(svc, cur); err != nil {
			return nil, err
		}
		if cur.Phase == abPhaseDiscarding {
			return nil, abConflict("%q is discarding B — wait for it, or discard with force", svc)
		}
		aborted, reason := cur.Phase == abPhaseAborted, cur.AbortReason
		if cur.Phase == abPhaseRunning && !confirmAborted {
			// The proxy may have latched an auto-abort the next tick hasn't
			// relabelled yet — that is an aborted B too.
			if a := abAutoAbort(cur, m.gather(context.Background(), svc)); a != nil {
				aborted, reason = true, a.reason
			}
		}
		if aborted && !confirmAborted {
			return nil, abConflict("B was aborted (reason: %s) — pass confirm_aborted to promote it anyway", firstNonEmpty(reason, "unknown"))
		}
		next := cur.clone()
		next.Pending = abPendingPromote
		if force {
			finalizeNow = true
			next.Op = abOpFinalizePromote
			return next, nil
		}
		if cur.Phase == abPhasePromoting {
			return nil, abConflict("%q is already promoting B (waiting for A's sessions to drain) — use force to skip the wait", svc)
		}
		next.Phase, next.PhaseAt, next.Op = abPhasePromoting, now.Unix(), abOpRelabel
		return next, nil
	}, func(ctx context.Context, rec *abRecord) error {
		if finalizeNow {
			return m.finalizePromote(ctx, svc, rec)
		}
		return m.execRelabel(ctx, svc, rec)
	})
}

func (m *abManager) finalizeFailed(svc string, rec *abRecord, err error) error {
	rec.LastError = err.Error()
	rec.LastAttempt = m.now().Unix()
	if perr := m.store.put(svc, rec); perr != nil {
		log.Printf("ab %s: could not persist a finalize failure: %v", svc, perr)
	}
	audit(nil, abAuditUser, "service.ab_finalize_failed", fmt.Sprintf("%s id=%s %s: %s", svc, rec.ID, rec.Op, err.Error()))
	return err
}

// finalizePromote makes B live (promoteCanary strips proxy.ab.*). A
// resumed finalize whose B is already gone counts as done.
//
// Every peer running svc is moved onto B's image first (promotePeers), so
// the old version can't come back from a peer's A replicas once the test
// is gone. Any peer failure leaves the record pending (finalizeFailed) for
// Run to retry — never a half-done finalize.
func (m *abManager) finalizePromote(ctx context.Context, svc string, rec *abRecord) error {
	if err := m.promotePeers(ctx, svc, rec); err != nil {
		return m.finalizeFailed(svc, rec, err)
	}
	if m.dc.abLocalLabels(ctx, svc) {
		if err := m.dc.promoteCanary(ctx, svc); err != nil {
			proxyRefresh(m.proxyURL)
			return m.finalizeFailed(svc, rec, err)
		}
	}
	proxyRefresh(m.proxyURL)
	return m.finalize(svc, rec, "promoted")
}

// abPeerPromoteRequest is POST /peer/services/{name}/ab/promote-replace's
// body (servePeerABPromoteReplace).
type abPeerPromoteRequest struct {
	ID    string `json:"id"`
	Image string `json:"image"`
}

// promotePeers rolls every peer that runs svc onto rec.Image, one peer at a
// time, each through its own health-gated rolling replace (rollingop.go),
// waiting for it to finish. A peer that can't be asked (unreachable,
// listing failed) is an error, not "doesn't run it". Peers already done for
// this promote (rec.PeersPromoted) are skipped, so a retry only redoes what
// failed.
func (m *abManager) promotePeers(ctx context.Context, svc string, rec *abRecord) error {
	if m.registry == nil {
		return nil
	}
	peers := m.registry.Peers()
	if len(peers) == 0 {
		return nil
	}
	if m.secret == "" {
		return fmt.Errorf("peers are configured but this host has no peer secret — promote %s on them by hand", svc)
	}
	status := m.registry.Status()
	for _, p := range peers {
		name := firstNonEmpty(status[p].Identity, p)
		if containsString(rec.PeersPromoted, name) {
			continue
		}
		var list peerServicesResp
		if err := peerGET(ctx, m.client, p, m.secret, "/peer/services", &list); err != nil {
			return fmt.Errorf("peer %s: can't list its services: %v", name, err)
		}
		runs := false
		for _, s := range list.Services {
			if s.Name == svc && (s.Replicas > 0 || len(s.MemberSummaries) > 0) {
				runs = true
			}
		}
		if runs {
			if status[p].OK && !status[p].Writes {
				return abPeerRefusesWrites(name)
			}
			if err := m.promotePeer(ctx, p, name, svc, rec); err != nil {
				return err
			}
			log.Printf("ab %s: peer %s now runs %s", svc, name, rec.Image)
		}
		rec.PeersPromoted = append(rec.PeersPromoted, name)
		if err := m.store.put(svc, rec); err != nil {
			return err
		}
	}
	return nil
}

func abPeerRefusesWrites(peer string) error {
	return fmt.Errorf("peer %s refuses writes — promote there by hand or enable -peer-writes", peer)
}

// promotePeer starts one peer's rolling replace onto rec.Image and waits
// (bounded by abPeerPromoteTimeout) for it to complete. A 409 is only
// accepted when the peer is already running exactly this replace — an
// earlier attempt of ours that outlived its wait.
func (m *abManager) promotePeer(ctx context.Context, peerURL, peer, svc string, rec *abRecord) error {
	body, err := json.Marshal(abPeerPromoteRequest{ID: rec.ID, Image: rec.Image})
	if err != nil {
		return err
	}
	base := "/peer/services/" + url.PathEscape(svc)
	code, resp, err := peerMutate(ctx, &http.Client{}, peerURL, m.secret, http.MethodPost, base+"/ab/promote-replace", 30*time.Second, strings.NewReader(string(body)), nil, "")
	if err != nil {
		return fmt.Errorf("peer %s: %v", peer, err)
	}
	// poll reads the peer's rolling-replace status; nil on a transient read
	// failure (retried until the deadline).
	poll := func() *rollingOpState {
		var st rollingOpState
		if err := peerGET(ctx, m.client, peerURL, m.secret, base+"/rolling-replace", &st); err != nil {
			return nil
		}
		return &st
	}
	switch code {
	case http.StatusAccepted:
	case http.StatusNotFound:
		return abPeerRefusesWrites(peer)
	case http.StatusConflict:
		if st := poll(); st == nil || st.Status != rollingOpStatusRunning || st.Image != rec.Image {
			return fmt.Errorf("peer %s refused the rolling replace to %s: %s", peer, rec.Image, strings.TrimSpace(string(resp)))
		}
	default:
		return fmt.Errorf("peer %s refused the rolling replace to %s (%d): %s", peer, rec.Image, code, strings.TrimSpace(string(resp)))
	}
	deadline := time.Now().Add(abPeerPromoteTimeout)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("peer %s: rolling replace to %s: %w", peer, rec.Image, ctx.Err())
		case <-time.After(abPeerPromotePoll):
		}
		if st := poll(); st != nil {
			if st.Image != rec.Image {
				return fmt.Errorf("peer %s's rolling replace is now for %s, not %s", peer, st.Image, rec.Image)
			}
			switch st.Status {
			case rollingOpStatusCompleted:
				return nil
			case rollingOpStatusFailed:
				return fmt.Errorf("peer %s: rolling replace to %s failed: %s", peer, rec.Image, st.LastError)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("peer %s: rolling replace to %s not done after %s", peer, rec.Image, abPeerPromoteTimeout)
		}
	}
}

func (m *abManager) finalizeDiscard(ctx context.Context, svc string, rec *abRecord) error {
	if m.dc.abLocalLabels(ctx, svc) {
		if err := m.dc.discardCanary(ctx, svc); err != nil {
			proxyRefresh(m.proxyURL)
			return m.finalizeFailed(svc, rec, err)
		}
	}
	proxyRefresh(m.proxyURL)
	return m.finalize(svc, rec, "discarded")
}

func (m *abManager) finalize(svc string, rec *abRecord, outcome string) error {
	err := m.store.finish(svc, abHistoryEntry{ID: rec.ID, Image: rec.Image, Started: rec.Started, Ended: m.now().Unix(), Outcome: outcome})
	audit(nil, abAuditUser, "service.ab_"+outcome, fmt.Sprintf("%s id=%s image=%s", svc, rec.ID, rec.Image))
	m.afterFinalize(svc)
	return err
}

// afterFinalize runs the central env propagation deferred while the test
// ran (§0.8): the remaining replicas move to the current version through
// the normal health-gated job.
func (m *abManager) afterFinalize(svc string) {
	ce := centralEnvOf(m.dc)
	if ce == nil {
		return
	}
	if ce.store.Has(svc) {
		ce.sync.request(svc)
		return
	}
	if cached, ok := ce.cache.Get(svc); ok {
		ce.sync.requestPeer(svc, cached.Origin, cached.Version)
	}
}

// envPending is the central env version a test is holding back: the
// current version when some local replica of svc runs an older one.
func (m *abManager) envPending(ctx context.Context, svc string) uint64 {
	ce := centralEnvOf(m.dc)
	if ce == nil {
		return 0
	}
	var pending uint64
	if cur, ok := ce.Version(svc); ok {
		if all, err := m.dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, svc)); err == nil {
			want := strconv.FormatUint(cur, 10)
			for _, ct := range all {
				if v := ct.Labels[labelEnvVersion]; v != "" && v != want {
					pending = cur
					break
				}
			}
		}
	}
	// A non-origin host's deferred job never fetched the origin's newer
	// version, so its own version still matches its replicas — the job's
	// target is the only place that version shows up here.
	if j, ok := ce.sync.get(svc); ok && j.Status == envSyncStatusDeferred && j.Target > pending {
		pending = j.Target
	}
	return pending
}

// ---- Run loop ----

// Run converges every test every abCheckInterval: resumes interrupted ops,
// makes a proxy auto-abort durable, and finalizes a pending promote or
// discard once the draining variant has drained.
func (m *abManager) Run(ctx context.Context) {
	m.tick(ctx)
	t := time.NewTicker(abCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick(ctx)
		}
	}
}

func (m *abManager) tick(ctx context.Context) {
	m.adoptOrphans(ctx)
	for _, svc := range m.store.services() {
		if ctx.Err() != nil {
			return
		}
		m.checkOne(ctx, svc)
	}
}

// adoptOrphans rebuilds a record from B's labels for a test this host runs
// but has no record of (a lost store) — so it stays manageable and guarded.
func (m *abManager) adoptOrphans(ctx context.Context) {
	all, err := m.dc.listAll(ctx, fmt.Sprintf(`{"label":["%s"]}`, labelABVariant))
	if err != nil {
		return
	}
	bySvc := map[string][]dockerContainer{}
	for _, ct := range all {
		svc := ct.Labels[labelService]
		if ct.Labels[labelCanary] == "true" && validServiceName(svc) && abIDRe.MatchString(ct.Labels[labelABID]) {
			bySvc[svc] = append(bySvc[svc], ct)
		}
	}
	for svc, bs := range bySvc {
		l := m.lock(svc)
		if !l.TryLock() {
			continue
		}
		if _, ok := m.store.get(svc); !ok {
			sort.Slice(bs, func(i, j int) bool { return bs[i].name() < bs[j].name() })
			rec := abRecordFromLabels(bs[0], len(bs))
			if err := m.store.put(svc, rec); err == nil {
				log.Printf("ab %s: adopted A/B test %s from B's labels (no record here)", svc, rec.ID)
				audit(nil, abAuditUser, "service.ab_adopted", fmt.Sprintf("%s id=%s", svc, rec.ID))
			}
		}
		l.Unlock()
	}
}

func abRecordFromLabels(ct dockerContainer, replicas int) *abRecord {
	unix := func(k string) int64 {
		n, _ := strconv.ParseInt(ct.Labels[k], 10, 64)
		return n
	}
	rec := &abRecord{
		ID: ct.Labels[labelABID], Image: ct.Image, Replicas: replicas,
		Started: unix(labelABStarted), Epoch: unix(labelABEpoch), PhaseAt: unix(labelABPhaseAt),
		Phase: firstNonEmpty(ct.Labels[labelABPhase], abPhaseRunning), AbortReason: ct.Labels[labelABAbortReason],
		Config: map[string]string{},
	}
	rec.Created = rec.Started
	switch rec.Phase {
	case abPhaseDiscarding:
		rec.Pending = abPendingDiscard
	case abPhasePromoting:
		rec.Pending = abPendingPromote
	}
	for k, v := range ct.Labels {
		switch k {
		case labelABVariant, labelABID, labelABStarted, labelABEpoch, labelABPhase, labelABPhaseAt, labelABAbortReason:
			continue
		}
		if strings.HasPrefix(k, labelABPrefix) {
			rec.Config[k] = v
		}
	}
	return rec
}

func (m *abManager) checkOne(parent context.Context, svc string) {
	if _, ok := m.store.get(svc); !ok {
		return
	}
	// Network reads first, outside the lock, so a user op landing during
	// them isn't refused as "already in progress".
	hosts := m.gather(parent, svc)
	pending := m.envPending(parent, svc)
	l := m.lock(svc)
	if !l.TryLock() {
		return
	}
	defer l.Unlock()
	ctx, cancel := context.WithTimeout(parent, abOpTimeout)
	defer cancel()
	rec, ok := m.store.get(svc)
	if !ok {
		return
	}
	now := m.now()
	if observePins(rec, hosts) {
		if err := m.store.put(svc, rec); err != nil {
			log.Printf("ab %s: %v", svc, err)
		}
	}
	if rec.Op == abOpStart {
		// We hold the lock, so no start is in flight: a dashboard restart
		// interrupted this one.
		m.abandonStart(svc, rec, "start interrupted (dashboard restarted)")
		return
	}
	if !m.dc.claims.tryClaim(svc, abClaimOwner) {
		return
	}
	defer m.dc.claims.release(svc, abClaimOwner)

	if pending != rec.EnvPendingVersion {
		rec.EnvPendingVersion = pending
		if err := m.store.put(svc, rec); err != nil {
			log.Printf("ab %s: %v", svc, err)
		}
	}

	// A pending relabel (say, a split that keeps failing) must not keep a
	// latched abort out of the labels: the record is the desired state, so
	// the relabel below converges both changes at once.
	if rec.Phase == abPhaseRunning && (rec.Op == "" || rec.Op == abOpRelabel) {
		if a := abAutoAbort(rec, hosts); a != nil {
			rec.Phase, rec.AbortReason, rec.Op = abPhaseAborted, a.reason, abOpRelabel
			rec.PhaseAt = now.Unix()
			if a.at > 0 && a.at <= now.Unix() {
				rec.PhaseAt = a.at
			}
			if err := m.store.put(svc, rec); err != nil {
				log.Printf("ab %s: could not persist the auto-abort: %v", svc, err)
				return
			}
			log.Printf("ab %s: proxy auto-aborted %s (%s, %s) — relabelling B phase=aborted", svc, rec.ID, a.reason, a.source)
			audit(nil, abAuditUser, "service.ab_auto_abort", fmt.Sprintf("%s id=%s reason=%s source=%s", svc, rec.ID, a.reason, a.source))
			_ = m.execRelabel(ctx, svc, rec)
			return
		}
	}

	retryDue := rec.LastError == "" || now.Unix()-rec.LastAttempt >= int64(abRetryInterval/time.Second)
	switch rec.Op {
	case abOpRelabel:
		if retryDue {
			rec.LastAttempt = now.Unix()
			_ = m.execRelabel(ctx, svc, rec)
		}
		return
	case abOpFinalizePromote:
		if retryDue {
			_ = m.finalizePromote(ctx, svc, rec)
		}
		return
	case abOpFinalizeDiscard:
		if retryDue {
			_ = m.finalizeDiscard(ctx, svc, rec)
		}
		return
	}

	switch rec.Pending {
	case abPendingDiscard:
		if abDrain(rec, "B", hosts, now).Drained {
			rec.Op = abOpFinalizeDiscard
			if err := m.store.put(svc, rec); err == nil {
				_ = m.finalizeDiscard(ctx, svc, rec)
			}
		}
		return
	case abPendingPromote:
		if abDrain(rec, "A", hosts, now).Drained {
			rec.Op = abOpFinalizePromote
			if err := m.store.put(svc, rec); err == nil {
				_ = m.finalizePromote(ctx, svc, rec)
			}
		}
		return
	}

	// B gone with nothing pending (removed by hand): the test is over.
	if all, err := m.dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, svc)); err == nil && !isABCanary(canaryOnly(all)) {
		if err := m.store.finish(svc, abHistoryEntry{ID: rec.ID, Image: rec.Image, Started: rec.Started, Ended: now.Unix(), Outcome: "lost", Detail: "B replicas disappeared"}); err == nil {
			audit(nil, abAuditUser, "service.ab_lost", fmt.Sprintf("%s id=%s", svc, rec.ID))
			m.afterFinalize(svc)
		}
	}
}

type abAbortSignal struct {
	reason, source string
	at             int64
}

// abAutoAbort finds a proxy-latched abort for rec's test (source auto on
// the proxy that judged it, peer on one that learned it). A label-sourced
// abort is the dashboard's own relabel, never a new signal.
func abAutoAbort(rec *abRecord, hosts []abHostReport) *abAbortSignal {
	for _, h := range hosts {
		e := h.Exp
		if e == nil || e.ID != rec.ID || e.Abort == nil {
			continue
		}
		if e.Abort.Source != "auto" && e.Abort.Source != "peer" {
			continue
		}
		reason := e.Abort.Reason
		if reason != "errors" && reason != "latency" {
			reason = "errors"
		}
		return &abAbortSignal{reason: reason, source: e.Abort.Source, at: e.Abort.At}
	}
	return nil
}

// ---- Status (GET ab) ----

type abStatsView struct {
	Requests  uint64  `json:"requests"`
	Errors    uint64  `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
}

type abHostView struct {
	Host      string                      `json:"host"`
	Reachable bool                        `json:"reachable"`
	Error     string                      `json:"error,omitempty"`
	ID        string                      `json:"id,omitempty"`
	Phase     string                      `json:"phase,omitempty"`
	Stats     *abProxyPair[abStatsView]   `json:"stats,omitempty"`
	Pinned    *abProxyPair[abProxyPinned] `json:"pinned,omitempty"`
	Judge     *abProxyJudge               `json:"judge,omitempty"`
}

type abHint struct {
	Z    float64 `json:"z"`
	Text string  `json:"text"`
}

type abStatusView struct {
	State             string                   `json:"state"` // active | none
	Service           string                   `json:"service"`
	ID                string                   `json:"id,omitempty"`
	Image             string                   `json:"image,omitempty"`
	Replicas          int                      `json:"replicas,omitempty"`
	Phase             string                   `json:"phase,omitempty"`
	PhaseAt           int64                    `json:"phase_at,omitempty"`
	Started           int64                    `json:"started,omitempty"`
	Epoch             int64                    `json:"epoch,omitempty"`
	AbortReason       string                   `json:"abort_reason,omitempty"`
	Pending           *string                  `json:"pending"`
	Op                string                   `json:"op,omitempty"`
	LastError         string                   `json:"last_error,omitempty"`
	Config            map[string]string        `json:"config,omitempty"`
	Drain             *abDrainView             `json:"drain,omitempty"`
	Stats             abProxyPair[abStatsView] `json:"stats"`
	Windows           []abProxyWindow          `json:"windows"`
	PerHost           []abHostView             `json:"per_host"`
	Abort             *abProxyAbort            `json:"abort,omitempty"`
	Hint              *abHint                  `json:"hint,omitempty"`
	EnvPendingVersion uint64                   `json:"env_pending_version,omitempty"`
	EnvPendingNote    string                   `json:"env_pending_note,omitempty"`
	History           []abHistoryEntry         `json:"history"`
}

// abHistBoundsDefault mirrors the proxy's bucket edges (1.2^(i+1) ms), used
// when a report carries none.
var abHistBoundsDefault = func() []float64 {
	out := make([]float64, 64)
	v := 1.0
	for i := range out {
		v *= 1.2
		out[i] = v
	}
	return out
}()

// abPercentile mirrors the proxy's abCounters.percentile: linear
// interpolation inside the bucket where rank q falls.
func abPercentile(hist []uint64, bounds []float64, q float64) float64 {
	if len(bounds) == 0 {
		bounds = abHistBoundsDefault
	}
	var total uint64
	for _, n := range hist {
		total += n
	}
	if total == 0 {
		return 0
	}
	target := q * float64(total)
	var cum float64
	for i, n := range hist {
		if n == 0 || i >= len(bounds) {
			continue
		}
		if cum+float64(n) >= target {
			lo := 0.0
			if i > 0 {
				lo = bounds[i-1]
			}
			frac := (target - cum) / float64(n)
			return lo + frac*(bounds[i]-lo)
		}
		cum += float64(n)
	}
	return bounds[len(bounds)-1]
}

func abAddCounters(dst *abProxyCounters, src abProxyCounters) {
	dst.Requests += src.Requests
	dst.Err5xx += src.Err5xx
	dst.Transport += src.Transport
	dst.Failover += src.Failover
	if len(dst.Hist) < len(src.Hist) {
		dst.Hist = append(dst.Hist, make([]uint64, len(src.Hist)-len(dst.Hist))...)
	}
	for i, n := range src.Hist {
		dst.Hist[i] += n
	}
}

// abStats is the §1.8 error rate: (5xx + transport + failover) over
// (requests + transport + failover), with percentiles from the histogram.
func abStats(c abProxyCounters, bounds []float64) abStatsView {
	n := c.Requests + c.Transport + c.Failover
	errs := c.Err5xx + c.Transport + c.Failover
	v := abStatsView{Requests: n, Errors: errs, P50Ms: abPercentile(c.Hist, bounds, 0.5), P95Ms: abPercentile(c.Hist, bounds, 0.95)}
	if n > 0 {
		v.ErrorRate = float64(errs) / float64(n)
	}
	return v
}

// abZHint is a two-proportion z-test on the error rates: a number and a
// sentence for the operator, never an automatic action.
func abZHint(a, b abStatsView) *abHint {
	if a.Requests < 30 || b.Requests < 30 {
		return &abHint{Text: "insufficient samples for a comparison (need at least 30 requests per variant)"}
	}
	pa, pb := a.ErrorRate, b.ErrorRate
	p := float64(a.Errors+b.Errors) / float64(a.Requests+b.Requests)
	se := math.Sqrt(p * (1 - p) * (1/float64(a.Requests) + 1/float64(b.Requests)))
	if se == 0 {
		return &abHint{Text: "no difference in error rate"}
	}
	z := (pb - pa) / se
	h := &abHint{Z: math.Round(z*100) / 100}
	switch {
	case z >= 1.96:
		h.Text = fmt.Sprintf("B's error rate is significantly higher than A's (%.2f%% vs %.2f%%, ~95%% confidence)", pb*100, pa*100)
	case z <= -1.96:
		h.Text = fmt.Sprintf("B's error rate is significantly lower than A's (%.2f%% vs %.2f%%, ~95%% confidence)", pb*100, pa*100)
	default:
		h.Text = fmt.Sprintf("no significant difference in error rate (%.2f%% vs %.2f%%)", pb*100, pa*100)
	}
	return h
}

// Status assembles GET ab.
func (m *abManager) Status(ctx context.Context, svc string) abStatusView {
	view := abStatusView{State: "none", Service: svc, Windows: []abProxyWindow{}, PerHost: []abHostView{}, History: m.store.history(svc)}
	rec, ok := m.store.get(svc)
	if !ok {
		return view
	}
	now := m.now()
	view.State = "active"
	view.ID, view.Image, view.Replicas = rec.ID, rec.Image, rec.Replicas
	view.Phase, view.PhaseAt, view.Started, view.Epoch, view.AbortReason = rec.Phase, rec.PhaseAt, rec.Started, rec.Epoch, rec.AbortReason
	view.Op, view.LastError, view.Config = rec.Op, rec.LastError, rec.Config
	if rec.Pending != "" {
		p := rec.Pending
		view.Pending = &p
	}
	hosts := m.gather(ctx, svc)
	if v := drainVariant(rec.Phase); v != "" {
		d := abDrain(rec, v, hosts, now)
		view.Drain = &d
	}
	var sum abProxyPair[abProxyCounters]
	var bounds []float64
	for i, h := range hosts {
		hv := abHostView{Host: h.Host, Reachable: h.Reachable, Error: h.Error}
		if e := h.Exp; e != nil {
			if bounds == nil {
				bounds = h.Bounds
			}
			hv.ID, hv.Phase, hv.Judge = e.ID, e.Phase, &e.Judge
			hv.Stats = &abProxyPair[abStatsView]{A: abStats(e.Cumulative.A, h.Bounds), B: abStats(e.Cumulative.B, h.Bounds)}
			pinned := e.Pinned
			hv.Pinned = &pinned
			if e.ID == rec.ID {
				// Each proxy records only what its own backends served, so
				// summing every host's own cumulative counts each request
				// exactly once (never add a proxy's "merged" to a peer's).
				abAddCounters(&sum.A, e.Cumulative.A)
				abAddCounters(&sum.B, e.Cumulative.B)
				if view.Abort == nil && e.Abort != nil {
					a := *e.Abort
					view.Abort = &a
				}
			}
			if i == 0 && e.ID == rec.ID && e.Merged.Windows != nil {
				view.Windows = e.Merged.Windows
			}
		}
		view.PerHost = append(view.PerHost, hv)
	}
	view.Stats = abProxyPair[abStatsView]{A: abStats(sum.A, bounds), B: abStats(sum.B, bounds)}
	view.Hint = abZHint(view.Stats.A, view.Stats.B)
	if rec.EnvPendingVersion > 0 {
		view.EnvPendingVersion = rec.EnvPendingVersion
		view.EnvPendingNote = fmt.Sprintf("env v%d pending — applies after the test", rec.EnvPendingVersion)
	}
	return view
}

// overlaySummaries fills Service.ABTest from this host's records: the
// record is authoritative for phase/pending/op (B's labels lag a relabel
// in flight, and a starting test has no B yet). Nil-safe.
func (m *abManager) overlaySummaries(svcs []Service) {
	if m == nil {
		return
	}
	for i := range svcs {
		rec, ok := m.store.get(svcs[i].Name)
		if !ok {
			continue
		}
		svcs[i].ABTest = &ServiceABTest{ID: rec.ID, Image: rec.Image, Phase: rec.Phase, Pending: rec.Pending, Op: rec.Op}
	}
}

// abErrStatus maps an op error to its HTTP status.
func abErrStatus(err error) int {
	var ae abError
	if errors.As(err, &ae) {
		return ae.code
	}
	var active errABActive
	if errors.As(err, &active) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}
