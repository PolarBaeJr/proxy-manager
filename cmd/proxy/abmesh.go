package main

// A/B testing across the peer mesh (docs/AB_TESTING_PLAN.md §1.9). The
// sender advertises per-route B counts and one peerABInfo per attached test;
// the receiver re-validates everything (abPeerExpFromWire), splits a peer's
// synthetic backend into A and B (PeerRouteStore.overlay), adopts a test it
// has no labels for, and folds peer stats and abort latches into its own
// runs (Router.applyPeerAB). Recording stays "once, by the proxy whose local
// backend served it": a hop is recorded by the receiver, never the sender.

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// abFailoverHeader marks an authenticated hop the forwarder sent to a peer's
// failover pool: X-Variant carries the session's pinned variant and the
// receiver serves only the other one, recording the request as
// failover_<pinned>. Honoured only on an authenticated hop with the exact
// value "1"; ServeHTTP strips it from every inbound request.
const abFailoverHeader = "X-Pmgr-AB-Failover"

const (
	abMaxPeerExperiments   = 64
	abMaxPeerCount         = 1 << 40
	abMaxPeerConfigEntries = 64
	abMaxPeerValueLen      = 8448 // ≥ 32 prefixes × 256B plus commas
	abMaxPeerServiceLen    = 128
	abMaxPeerDetailLen     = 256
	abMaxPeerBBackends     = 10000
	peerPayloadMaxBytes    = 1 << 20 // peerRoutesHandler's body limit
	abDefaultSyncInterval  = 5 * time.Second
)

// ---- Wire format ----

type abCountersWire struct {
	Requests  uint64   `json:"requests"`
	Err5xx    uint64   `json:"err5xx"`
	Transport uint64   `json:"transport"`
	Failover  uint64   `json:"failover"`
	Hist      []uint64 `json:"hist"`
}

type abWindowWire struct {
	Index int64                  `json:"index"`
	V     abPair[abCountersWire] `json:"variants"`
}

// peerABInfo is one attached test as advertised to peers. Counts, config
// and timestamps only: no pin nonces, request values or secrets (there is
// no per-test key).
type peerABInfo struct {
	Service    string                 `json:"service"`
	ID         string                 `json:"id"`
	Config     map[string]string      `json:"config"`
	Phase      string                 `json:"phase"`
	PhaseAt    int64                  `json:"phase_at"`
	Abort      *abAbortInfo           `json:"abort,omitempty"`
	BBackends  int                    `json:"b_backends"`
	Pinned     abPair[abPinnedReport] `json:"pinned"`
	Cumulative abPair[abCountersWire] `json:"cumulative"`
	Windows    []abWindowWire         `json:"windows"`
}

// peerABList decodes each experiment on its own, so one malformed entry (a
// negative count, a wrong type) is dropped without failing the whole push —
// a bad experiment must never cost a peer its route sync.
type peerABList []peerABInfo

func (l *peerABList) UnmarshalJSON(b []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(b, &raws); err != nil {
		abWarnOnce("peer", "experiments-malformed", "peer push: malformed experiments list — ignored")
		*l = nil
		return nil
	}
	if len(raws) > abMaxPeerExperiments {
		abWarnOnce("peer", "experiments-cap", fmt.Sprintf("peer push: %d experiments over the cap of %d — all ignored", len(raws), abMaxPeerExperiments))
		*l = nil
		return nil
	}
	out := make(peerABList, 0, len(raws))
	for _, raw := range raws {
		var e peerABInfo
		if err := json.Unmarshal(raw, &e); err != nil {
			abWarnOnce("peer", "experiment-decode", "peer push: undecodable experiment — dropped")
			continue
		}
		out = append(out, e)
	}
	*l = out
	return nil
}

func abCountersToWire(c *abCounters) abCountersWire {
	return abCountersWire{Requests: c.Requests, Err5xx: c.Err5xx, Transport: c.Transport, Failover: c.Failover,
		Hist: append([]uint64(nil), c.Hist[:]...)}
}

func abCountersFromWire(w abCountersWire) (abCounters, bool) {
	var c abCounters
	if len(w.Hist) != abHistBuckets {
		return c, false
	}
	for _, n := range []uint64{w.Requests, w.Err5xx, w.Transport, w.Failover} {
		if n > abMaxPeerCount {
			return c, false
		}
	}
	if w.Err5xx > w.Requests {
		return c, false
	}
	var sum uint64
	for i, n := range w.Hist {
		if n > abMaxPeerCount {
			return c, false
		}
		sum += n
		c.Hist[i] = n
	}
	if sum > w.Requests {
		return c, false
	}
	c.Requests, c.Err5xx, c.Transport, c.Failover = w.Requests, w.Err5xx, w.Transport, w.Failover
	return c, true
}

// abConfigLabelKeys is every key abConfig.labels() can emit — the only keys
// a peer's config map may carry.
var abConfigLabelKeys = func() map[string]bool {
	c := parseABConfig(map[string]string{labelABID: "abcd"}, time.Now(), func(string, string) {})
	c.Started, c.Epoch, c.PhaseAt = 1, 1, 1
	m := map[string]bool{}
	for k := range c.labels() {
		m[k] = true
	}
	return m
}()

// ---- Validated receive-side state ----

// abPeerExp is a peer's experiment after re-validation. cfg went through
// the same parseABConfig as local labels, with zero tolerance for warnings.
type abPeerExp struct {
	service   string
	id        string
	cfg       *abConfig
	phase     string // effective phase on the peer
	phaseAt   int64
	abort     *abAbortInfo
	bBackends int
	pinned    [2]abPinnedReport
	sum       abSummary
}

// latched reports whether the peer's own runtime holds an abort latch (as
// opposed to a phase=aborted label).
func (e *abPeerExp) latched() bool {
	return e.abort != nil && (e.abort.Source == "auto" || e.abort.Source == "peer")
}

// fingerprint is what PeerRouteStore.merge compares to decide whether a
// push changed anything routing depends on. Stats, pinned counts and abort
// detail are deliberately left out so steady-state pushes never refresh.
func (e *abPeerExp) fingerprint() string {
	labels := e.cfg.labels()
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(e.id + "|" + e.phase + "|" + fmt.Sprint(e.latched(), e.bBackends > 0))
	for _, k := range keys {
		sb.WriteString("|" + k + "=" + labels[k])
	}
	return sb.String()
}

func abPrintable(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}

// abPeerExpFromWire re-validates one advertised experiment. Anything out of
// bounds drops the whole experiment (the caller logs once).
func abPeerExpFromWire(e *peerABInfo, now time.Time) (*abPeerExp, string) {
	if e.Service == "" || !abPrintable(e.Service, abMaxPeerServiceLen) {
		return nil, "bad service"
	}
	if !abIDRe.MatchString(e.ID) {
		return nil, "bad id"
	}
	if len(e.Config) == 0 || len(e.Config) > abMaxPeerConfigEntries {
		return nil, "bad config size"
	}
	for k, v := range e.Config {
		if !abConfigLabelKeys[k] || len(v) > abMaxPeerValueLen {
			return nil, "bad config key " + fmt.Sprintf("%q", k)
		}
	}
	if e.Config[labelABVariant] != abVariantB || e.Config[labelABID] != e.ID {
		return nil, "config variant/id mismatch"
	}
	var bad string
	cfg := parseABConfig(e.Config, now, func(key, _ string) {
		if bad == "" {
			bad = "invalid config " + key
		}
	})
	if bad != "" {
		return nil, bad
	}
	maxTS := now.Add(24 * time.Hour).Unix()
	switch e.Phase {
	case abPhaseRunning, abPhaseAborted, abPhasePromoting, abPhaseDiscarding:
	default:
		return nil, "bad phase"
	}
	if e.Phase != cfg.Phase && !(cfg.Phase == abPhaseRunning && e.Phase == abPhaseAborted) {
		return nil, "phase inconsistent with config"
	}
	if e.PhaseAt < 0 || e.PhaseAt > maxTS {
		return nil, "bad phase_at"
	}
	if a := e.Abort; a != nil {
		switch a.Reason {
		case "errors", "latency", "manual", "":
		default:
			return nil, "bad abort reason"
		}
		switch a.Source {
		case "auto", "peer", "label":
		default:
			return nil, "bad abort source"
		}
		if !abPrintable(a.Detail, abMaxPeerDetailLen) || a.At < 0 || a.At > maxTS {
			return nil, "bad abort"
		}
	}
	if e.BBackends < 0 || e.BBackends > abMaxPeerBBackends {
		return nil, "bad b_backends"
	}
	out := &abPeerExp{service: e.Service, id: e.ID, cfg: cfg, phase: e.Phase, phaseAt: e.PhaseAt, bBackends: e.BBackends}
	if e.Abort != nil {
		a := *e.Abort
		out.abort = &a
	}
	for i, p := range []abPinnedReport{e.Pinned.A, e.Pinned.B} {
		if p.ActiveSessions < 0 || p.ActiveSessions > abPinnedCap || p.LastSeen < 0 || p.LastSeen > maxTS {
			return nil, "bad pinned"
		}
		out.pinned[i] = p
	}
	var ok bool
	if out.sum.Cumulative[0], ok = abCountersFromWire(e.Cumulative.A); !ok {
		return nil, "bad cumulative"
	}
	if out.sum.Cumulative[1], ok = abCountersFromWire(e.Cumulative.B); !ok {
		return nil, "bad cumulative"
	}
	if len(e.Windows) > abRingSize {
		return nil, "too many windows"
	}
	seen := map[int64]bool{}
	for _, w := range e.Windows {
		if w.Index < 0 || w.Index > abMaxPeerCount || seen[w.Index] {
			return nil, "bad window index"
		}
		seen[w.Index] = true
		win := abWindow{Index: w.Index}
		if win.V[0], ok = abCountersFromWire(w.V.A); !ok {
			return nil, "bad window"
		}
		if win.V[1], ok = abCountersFromWire(w.V.B); !ok {
			return nil, "bad window"
		}
		out.sum.Windows = append(out.sum.Windows, win)
	}
	sort.Slice(out.sum.Windows, func(i, j int) bool { return out.sum.Windows[i].Index < out.sum.Windows[j].Index })
	return out, ""
}

// validatePeerExperiments turns a push's experiments into a per-service
// map. Invalid entries are dropped and logged once per peer+service+reason;
// a duplicated service keeps the smallest id.
func validatePeerExperiments(peer string, list peerABList, now time.Time) map[string]*abPeerExp {
	out := map[string]*abPeerExp{}
	for i := range list {
		e, why := abPeerExpFromWire(&list[i], now)
		if e == nil {
			svc := list[i].Service
			if !abPrintable(svc, abMaxPeerServiceLen) {
				svc = "?"
			}
			abWarnOnce("peer:"+peer, "exp:"+svc+":"+why, fmt.Sprintf("peer push: experiment for service %q dropped: %s", svc, why))
			continue
		}
		if prev, ok := out[e.service]; !ok || e.id < prev.id {
			out[e.service] = e
		}
	}
	return out
}

// ---- Sender ----

// abortLocked returns the abort info and phase_at to expose for this run: its own
// latch first, else a phase=aborted label. Caller holds run.mu.
func (run *abRun) abortLocked() (*abAbortInfo, int64) {
	cfg := run.cfg
	if run.latched.Load() {
		a := run.abort
		at := cfg.PhaseAt
		if cfg.Phase == abPhaseRunning {
			at = a.At
		}
		return &a, at
	}
	if cfg.Phase == abPhaseAborted {
		return &abAbortInfo{Reason: cfg.AbortReason, At: cfg.PhaseAt, Source: "label"}, cfg.PhaseAt
	}
	return nil, cfg.PhaseAt
}

func (run *abRun) peerInfo() peerABInfo {
	run.mu.Lock()
	defer run.mu.Unlock()
	cfg := run.cfg
	abort, phaseAt := run.abortLocked()
	info := peerABInfo{
		Service: run.service, ID: run.id, Config: cfg.labels(), Phase: run.phase(cfg), PhaseAt: phaseAt,
		Abort: abort, BBackends: run.bLocal,
		Pinned: abPair[abPinnedReport]{
			A: abPinnedReport{len(run.pinnedNonces[0]), run.pinnedLast[0]},
			B: abPinnedReport{len(run.pinnedNonces[1]), run.pinnedLast[1]},
		},
		Cumulative: abPair[abCountersWire]{abCountersToWire(&run.cum[0]), abCountersToWire(&run.cum[1])},
		Windows:    []abWindowWire{},
	}
	for _, w := range run.summaryLocked().Windows {
		info.Windows = append(info.Windows, abWindowWire{Index: w.Index, V: abPair[abCountersWire]{abCountersToWire(&w.V[0]), abCountersToWire(&w.V[1])}})
	}
	return info
}

// peerABInfos lists every attached test (attachedRuns is sorted by
// service), capped at abMaxPeerExperiments. Adopted tests are included too:
// this proxy records what its local backends serve, and merged judging
// needs those counts.
func (r *Router) peerABInfos() []peerABInfo {
	var out []peerABInfo
	for _, run := range r.attachedRuns() {
		if len(out) == abMaxPeerExperiments {
			abWarnOnce("peer", "send-cap", fmt.Sprintf("more than %d A/B tests — the rest are not advertised to peers", abMaxPeerExperiments))
			break
		}
		out = append(out, run.peerInfo())
	}
	return out
}

// ---- Receiver: runtime state ----

// abPeerSnap is the latest validated experiment a peer pushed for a run,
// stamped with this proxy's clock when it arrived.
type abPeerSnap struct {
	exp *abPeerExp
	at  time.Time
}

func (r *Router) abSyncInterval() time.Duration {
	if r.peerSyncInterval > 0 {
		return r.peerSyncInterval
	}
	return abDefaultSyncInterval
}

// applyPeerAB writes one peer's validated experiments into the matching
// runs: the snapshot used for merged judging and /ab, and the abort latch
// (union by test id). Called on every push, after any refresh the push
// caused, so a latch propagates within one sync interval without a refresh.
func (r *Router) applyPeerAB(peer string, exps map[string]*abPeerExp) {
	now := r.clock()
	r.mu.RLock()
	runs := make([]*abRun, 0, len(r.abRuns))
	for _, run := range r.abRuns {
		runs = append(runs, run)
	}
	r.mu.RUnlock()
	for _, run := range runs {
		e := exps[run.service]
		run.mu.Lock()
		if e == nil || e.id != run.id {
			delete(run.peers, peer)
			run.mu.Unlock()
			continue
		}
		run.peers[peer] = &abPeerSnap{exp: e, at: now}
		if cfg := run.cfg; cfg != nil && cfg.Phase == abPhaseRunning && e.phase == abPhaseAborted && !run.latched.Load() {
			a := abAbortInfo{Reason: "manual", At: e.phaseAt, Source: "peer"}
			if e.abort != nil {
				a.Reason, a.Detail = e.abort.Reason, e.abort.Detail
				if e.abort.At > 0 {
					a.At = e.abort.At
				}
				if a.Reason == "" {
					a.Reason = "manual"
				}
			}
			run.abort = a
			run.latched.Store(true)
			log.Printf("ab: service %q test %s aborted on peer %s (%s) — latched here too, new sessions go to A", run.service, run.id, peer, a.Reason)
		}
		run.mu.Unlock()
	}
}

// freshPeersLocked returns the peer snapshots younger than three sync
// intervals, sorted by peer id. Caller holds run.mu.
func (run *abRun) freshPeersLocked(now time.Time) (fresh []string, all []string) {
	stale := 3 * run.syncEvery()
	for p, s := range run.peers {
		all = append(all, p)
		if now.Sub(s.at) <= stale {
			fresh = append(fresh, p)
		}
	}
	sort.Strings(fresh)
	sort.Strings(all)
	return fresh, all
}

func (run *abRun) syncEvery() time.Duration {
	if run.syncInterval > 0 {
		return run.syncInterval
	}
	return abDefaultSyncInterval
}

// peerSummariesLocked is what merged judging adds to this proxy's own
// summary: fresh peers whose windows line up with ours (same epoch, warm-up
// and window), so equal indices mean equal time ranges. Caller holds run.mu.
func (run *abRun) peerSummariesLocked(now time.Time) []abSummary {
	fresh, _ := run.freshPeersLocked(now)
	var out []abSummary
	for _, p := range fresh {
		e := run.peers[p].exp
		if run.abAligned(e) {
			out = append(out, e.sum)
		}
	}
	return out
}

func (run *abRun) abAligned(e *abPeerExp) bool {
	c := run.cfg
	return e.cfg.Epoch == c.Epoch && e.cfg.Warmup == c.Warmup && e.cfg.Window == c.Window
}

// abMergeSummaries sums summaries the way judge does: cumulative counters
// and windows by index.
func abMergeSummaries(all []abSummary) abSummary {
	var out abSummary
	byIdx := map[int64]*abWindow{}
	for _, s := range all {
		for v := range out.Cumulative {
			out.Cumulative[v].add(&s.Cumulative[v])
		}
		for _, w := range s.Windows {
			m, ok := byIdx[w.Index]
			if !ok {
				m = &abWindow{Index: w.Index}
				byIdx[w.Index] = m
			}
			for v := range m.V {
				m.V[v].add(&w.V[v])
			}
		}
	}
	for _, w := range byIdx {
		out.Windows = append(out.Windows, *w)
	}
	sort.Slice(out.Windows, func(i, j int) bool { return out.Windows[i].Index < out.Windows[j].Index })
	return out
}

// abPeerReport is one peer's view of a test in /ab.
type abPeerReport struct {
	Peer      string `json:"peer"`
	Fresh     bool   `json:"fresh"`
	AgeS      int64  `json:"age_s"`
	Phase     string `json:"phase"`
	Latched   bool   `json:"latched"`
	BBackends int    `json:"b_backends"`
	Aligned   bool   `json:"aligned"`
}

type abMergedReport struct {
	Peers      int                      `json:"peers"`
	Cumulative abPair[abCountersReport] `json:"cumulative"`
	Windows    []abWindowReport         `json:"windows"`
}

// ---- Receiver: route overlay ----

// learnedABSplit decides how overlay turns one peer route into synthetic
// backends for group g. testID is the test the group runs on this proxy
// ("" = none). The result is either the legacy single backend with the
// advertised totals (no test here, or the peer sent no B fields), an A-only
// backend (the peer's B belongs to another test, or its experiment did not
// validate), or separate A and B backends.
type learnedABSplit int

const (
	abSplitLegacy learnedABSplit = iota
	abSplitAOnly
	abSplitBoth
)

func (s *PeerRouteStore) abSplitFor(g *RouteGroup, testID string, lr learnedRoute) learnedABSplit {
	if testID == "" || g.static || lr.abID == "" || lr.bBackends <= 0 {
		return abSplitLegacy
	}
	if lr.abID != testID {
		abWarnOnce("peer:"+lr.peerID, "conflict:"+g.Service+":"+lr.abID+":"+testID,
			fmt.Sprintf("peer %s runs A/B test %s for service %q but this proxy runs %s — local wins, the peer's B is not routed to", lr.peerID, lr.abID, g.Service, testID))
		return abSplitAOnly
	}
	if e := s.exps[lr.peerID][g.Service]; e == nil || e.id != testID {
		return abSplitAOnly
	}
	return abSplitBoth
}

// peerBackendsFor builds the synthetic backends for one peer route.
func (s *PeerRouteStore) peerBackendsFor(lr learnedRoute, host, path string, split learnedABSplit) []*Backend {
	if split == abSplitLegacy {
		if b := makePeerBackendAuth(lr.advertise, host, path, lr.stripPrefix, lr.peerID, lr.weight, s.hopAuth); b != nil {
			return []*Backend{b}
		}
		return nil
	}
	var out []*Backend
	if lr.backends-lr.bBackends > 0 {
		w := lr.weight - lr.bWeight
		if b := makePeerBackendAuth(lr.advertise, host, path, lr.stripPrefix, lr.peerID, w, s.hopAuth); b != nil {
			out = append(out, b)
		}
	}
	if split == abSplitBoth {
		if b := makePeerBackendAuth(lr.advertise, host, path, lr.stripPrefix, lr.peerID, lr.bWeight, s.hopAuth); b != nil {
			b.Variant = abVariantB
			b.Container = "peer:" + lr.peerID + ":B"
			b.stickyID = backendStickyID(lr.advertise + "|B")
			out = append(out, b)
		}
	}
	return out
}

// abTestIDs resolves, per service, the test this proxy runs: local labels
// (abCfg, or abLocal while the local B is briefly stopped) win; otherwise
// the smallest id any fresh peer that owns B replicas advertises, newest
// phase_at first among equal ids. Caller holds s.mu.
func (s *PeerRouteStore) abTestIDs(groups []*RouteGroup) (map[string]string, map[string]*abPeerExp) {
	ids := map[string]string{}
	for _, g := range groups {
		if g.static || g.Service == "" {
			continue
		}
		if c := g.abCfg; c != nil {
			ids[g.Service] = c.ID
		} else if c := g.abLocal; c != nil {
			ids[g.Service] = c.ID
		}
	}
	adopt := map[string]*abPeerExp{}
	peers := make([]string, 0, len(s.exps))
	for p := range s.exps {
		peers = append(peers, p)
	}
	sort.Strings(peers)
	for _, p := range peers {
		for svc, e := range s.exps[p] {
			if _, local := ids[svc]; local || e.bBackends <= 0 {
				continue
			}
			if cur := adopt[svc]; cur == nil || e.id < cur.id || (e.id == cur.id && e.phaseAt > cur.phaseAt) {
				adopt[svc] = e
			}
		}
	}
	for svc, e := range adopt {
		ids[svc] = e.id
	}
	return ids, adopt
}

// abAttach runs after every peer backend is in place: a group without an
// attached test gets its local labels' config (local B stopped, peer B
// alive) or the adopted peer config — only if it now has a B backend.
func abAttach(groups []*RouteGroup, adopt map[string]*abPeerExp) {
	for _, g := range groups {
		if g.abCfg != nil || g.static || !g.hasBBackend() {
			continue
		}
		if g.abLocal != nil {
			g.abCfg = g.abLocal
		} else if e := adopt[g.Service]; e != nil {
			c := *e.cfg
			g.abCfg = &c
		}
	}
}
