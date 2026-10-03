package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- two-proxy mesh harness ----

const meshSecret = "s3cret"

// meshNode is one proxy: a Router behind a real listener (its proxy port),
// a PeerRouteStore with the real /peer/routes handler, and local A (and
// optionally B) backends. refresh mirrors main.go: fresh local groups, then
// overlay, then Set.
type meshNode struct {
	t         *testing.T
	id        string
	r         *Router
	store     *PeerRouteStore
	srv       *httptest.Server
	h         http.Handler
	a, b      *abSrv
	cfg       *abConfig
	refreshes int
}

func newMeshNode(t *testing.T, id string, clk *abClock, withB bool, labels map[string]string) *meshNode {
	t.Helper()
	tok := peerHopAuthToken(meshSecret)
	n := &meshNode{t: t, id: id}
	n.r = &Router{now: clk.now, peerHopAuth: tok}
	n.store = newPeerRouteStore(15 * time.Second)
	n.store.now = clk.now
	n.store.hopAuth = tok
	n.store.abApply = n.r.applyPeerAB
	n.a = newABSrv(t, abTestHost, abVariantA)
	if withB {
		n.b = newABSrv(t, abTestHost, abVariantB)
	}
	if labels != nil {
		if _, ok := labels[labelABStarted]; !ok {
			labels[labelABStarted] = strconv.FormatInt(clk.now().Unix(), 10)
		}
		n.cfg = parseABConfig(labels, clk.now(), noWarn)
	}
	n.srv = httptest.NewServer(n.r)
	t.Cleanup(n.srv.Close)
	n.h = peerRoutesHandler(meshSecret, n.store, n.refresh)
	n.refresh()
	return n
}

func (n *meshNode) local() []*RouteGroup {
	g := &RouteGroup{Host: abTestHost, Service: "svc", Backends: []*Backend{n.a.be}}
	if n.b != nil {
		g.Backends = append(g.Backends, n.b.be)
	}
	if n.cfg != nil {
		g.abLocal = n.cfg
		if n.b != nil {
			g.abCfg = n.cfg
		}
	}
	return []*RouteGroup{g}
}

func (n *meshNode) refresh() {
	n.refreshes++
	n.r.Set(n.store.overlay(n.local(), nil))
}

func (n *meshNode) group() *RouteGroup {
	for _, g := range n.r.Snapshot() {
		if g.Host == abTestHost {
			return g
		}
	}
	n.t.Fatal("no group")
	return nil
}

func (n *meshNode) run() *abRun {
	n.r.mu.RLock()
	defer n.r.mu.RUnlock()
	return n.r.abRuns["svc"]
}

func (n *meshNode) cum() [2]abCounters {
	run := n.run()
	if run == nil {
		return [2]abCounters{}
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.cum
}

func (n *meshNode) do(target string, cookies []*http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://"+abTestHost+target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	n.r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)
	return rec
}

func (n *meshNode) pin(variant string, now time.Time) *http.Cookie {
	p := abPin{id: abTestID, variant: variant, src: "u", nonce: "0badc0de", issued: now.Unix(), lastSeen: now.Unix()}
	return &http.Cookie{Name: abPinCookieName("svc"), Value: p.String()}
}

func postPayload(t *testing.T, h http.Handler, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/peer/routes", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+meshSecret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// push sends from's real payload to to's real handler.
func push(t *testing.T, from, to *meshNode) {
	t.Helper()
	body, ok := buildPeerPayload(from.r, from.id, from.srv.URL)
	if !ok {
		t.Fatal("nothing to push")
	}
	if code := postPayload(t, to.h, body); code != http.StatusNoContent {
		t.Fatalf("push %s→%s: %d", from.id, to.id, code)
	}
}

func learnedByVariant(g *RouteGroup, variant string) *Backend {
	for _, b := range g.Backends {
		if b.Learned && b.variantName() == variant {
			return b
		}
	}
	return nil
}

// twoNodes: p1 owns B (labels), p2 has only A. Both pushed both ways.
func twoNodes(t *testing.T, kv ...string) (*abClock, *meshNode, *meshNode) {
	t.Helper()
	clk := newABClock()
	p1 := newMeshNode(t, "p1", clk, true, abLabels(kv...))
	p2 := newMeshNode(t, "p2", clk, false, nil)
	push(t, p1, p2)
	push(t, p2, p1)
	return clk, p1, p2
}

// ---- end-to-end ----

func TestABMeshBOnlyOnPeerRoutesAndRecordsOnce(t *testing.T) {
	clk, p1, p2 := twoNodes(t, labelABSplit, "100")
	g2 := p2.group()
	if g2.ab == nil || g2.abCfg.ID != abTestID {
		t.Fatal("p2 should adopt p1's test")
	}
	if b := learnedByVariant(g2, abVariantB); b == nil || b.Container != "peer:p1:B" || b.stickyID != backendStickyID(p1.srv.URL+"|B") {
		t.Fatalf("p2 B synthetic = %+v", b)
	}
	if learnedByVariant(g2, abVariantA) == nil {
		t.Fatal("p2 should keep p1's A pool as a learned A backend")
	}

	// A new session on p2 is assigned B and served by p1's B.
	rec := p2.do("/", []*http.Cookie{{Name: "ab_uid", Value: "u1"}}, nil)
	if servedBy(rec) != "B" || p1.b.hits.Load() != 1 || p1.b.gotVariant.Load() != "B" {
		t.Fatalf("B session on p2 served by %q (p1 B hits %d)", servedBy(rec), p1.b.hits.Load())
	}
	if p1.b.gotAuth.Load() != "" {
		t.Fatal("hop auth reached a backend")
	}
	if p, ok := pinFromRec(t, rec); !ok || p.variant != "B" {
		t.Fatal("p2 should pin the session to B")
	}
	if c := p1.cum(); c[1].Requests != 1 {
		t.Fatalf("p1 should record the hop once under B, got %+v", c[1])
	}
	if c := p2.cum(); c[0].samples()+c[1].samples() != 0 {
		t.Fatal("the forwarder must not record")
	}
	// An A session stays on p2's local A and is recorded there.
	if servedBy(p2.do("/", []*http.Cookie{p2.pin("A", clk.now())}, nil)) != "A" || p2.a.hits.Load() != 1 {
		t.Fatal("A session on p2 should use the local A")
	}
	if c := p2.cum(); c[0].Requests != 1 {
		t.Fatalf("p2 local A = %+v", c[0])
	}

	// /ab: p2 sees p1's B count and p1 as a fresh peer; merged view sums both.
	push(t, p1, p2)
	push(t, p2, p1)
	rep := p2.r.ABReport("svc").Experiments[0]
	if rep.BBackendsLocal != 0 || rep.BBackendsPeer != 1 || len(rep.Peers) != 1 || rep.Peers[0].Peer != "p1" || !rep.Peers[0].Fresh {
		t.Fatalf("p2 report: local=%d peer=%d peers=%+v", rep.BBackendsLocal, rep.BBackendsPeer, rep.Peers)
	}
	if rep.Merged.Peers != 1 || rep.Merged.Cumulative.A.Requests != 1 || rep.Merged.Cumulative.B.Requests != 1 {
		t.Fatalf("merged = %+v", rep.Merged)
	}
	rep1 := p1.r.ABReport("svc").Experiments[0]
	if rep1.BBackendsLocal != 1 || rep1.BBackendsPeer != 0 || rep1.Merged.Cumulative.A.Requests != 1 {
		t.Fatalf("p1 report: %+v", rep1)
	}
}

func TestABMeshAbortLatchPropagates(t *testing.T) {
	for _, origin := range []string{"p1", "p2"} {
		t.Run("latched on "+origin, func(t *testing.T) {
			clk, p1, p2 := twoNodes(t, labelABSplit, "100")
			src, dst := p1, p2
			if origin == "p2" {
				src, dst = p2, p1
			}
			run := src.run()
			run.mu.Lock()
			run.abort = abAbortInfo{Reason: "errors", Detail: "x", At: clk.now().Unix(), Source: "auto"}
			run.latched.Store(true)
			run.mu.Unlock()
			push(t, src, dst)
			if !dst.run().latched.Load() {
				t.Fatal("latch did not propagate")
			}
			rep := dst.r.ABReport("svc").Experiments[0]
			if rep.Phase != abPhaseAborted || rep.Abort == nil || rep.Abort.Source != "peer" || rep.Abort.Reason != "errors" {
				t.Fatalf("dst report: phase=%s abort=%+v", rep.Phase, rep.Abort)
			}
			for _, n := range []*meshNode{p1, p2} {
				if servedBy(n.do("/", []*http.Cookie{{Name: "ab_uid", Value: "newcomer"}}, nil)) != "A" {
					t.Fatalf("%s: new session after abort should go to A", n.id)
				}
			}
			// The echo back must not flip anything.
			push(t, dst, src)
			if !src.run().latched.Load() || src.r.ABReport("svc").Experiments[0].Abort.Source != "auto" {
				t.Fatal("origin latch changed by the echo")
			}
		})
	}
}

func TestABMeshFailoverCountedOnce(t *testing.T) {
	t.Run("running B to peer A", func(t *testing.T) {
		clk, p1, p2 := twoNodes(t, labelABSplit, "100")
		p2.a.be.DockerUnhealthy = true
		learnedByVariant(p2.group(), abVariantB).markHealthy(false)
		rec := p2.do("/", []*http.Cookie{p2.pin("B", clk.now())}, nil)
		if servedBy(rec) != "A" || p1.a.hits.Load() != 1 || p1.b.hits.Load() != 0 || p1.a.gotVariant.Load() != "A" {
			t.Fatalf("failover hop served by %q (p1 A %d, B %d, X-Variant %v)", servedBy(rec), p1.a.hits.Load(), p1.b.hits.Load(), p1.a.gotVariant.Load())
		}
		c1, c2 := p1.cum(), p2.cum()
		if c1[1].Failover != 1 || c1[0].samples() != 0 || c2[0].samples()+c2[1].samples() != 0 {
			t.Fatalf("failover accounting: p1 A=%+v B=%+v p2 A=%d B=%d", c1[0], c1[1], c2[0].samples(), c2[1].samples())
		}
		if rep := p1.r.ABReport("svc").Experiments[0]; rep.Failover.B != 1 {
			t.Fatalf("p1 failover = %+v", rep.Failover)
		}
	})
	t.Run("promoting A to peer B", func(t *testing.T) {
		clk, p1, p2 := twoNodes(t, labelABPhase, abPhasePromoting)
		p2.a.be.DockerUnhealthy = true
		learnedByVariant(p2.group(), abVariantA).markHealthy(false)
		rec := p2.do("/", []*http.Cookie{p2.pin("A", clk.now())}, nil)
		if servedBy(rec) != "B" || p1.b.hits.Load() != 1 || p1.a.hits.Load() != 0 {
			t.Fatalf("promoting failover served by %q", servedBy(rec))
		}
		c1, c2 := p1.cum(), p2.cum()
		if c1[0].Failover != 1 || c1[1].samples() != 0 || c2[0].samples()+c2[1].samples() != 0 {
			t.Fatalf("failover accounting: p1 A=%+v B=%+v", c1[0], c1[1])
		}
	})
	t.Run("receiver without healthy B fails over locally", func(t *testing.T) {
		clk, p1, p2 := twoNodes(t, labelABSplit, "100")
		p1.b.be.DockerUnhealthy = true
		rec := p2.do("/", []*http.Cookie{p2.pin("B", clk.now())}, nil)
		if servedBy(rec) != "A" || p1.a.hits.Load() != 1 || p2.a.hits.Load() != 0 {
			t.Fatalf("served by %q (p1 A %d, p2 A %d) — receiver must fail over locally, not re-forward", servedBy(rec), p1.a.hits.Load(), p2.a.hits.Load())
		}
		c1, c2 := p1.cum(), p2.cum()
		if c1[1].Failover != 1 || c1[0].samples() != 0 || c2[0].samples()+c2[1].samples() != 0 {
			t.Fatalf("accounting: p1 A=%+v B=%+v", c1[0], c1[1])
		}
	})
	t.Run("transport failure on a failover hop counts under the pinned variant", func(t *testing.T) {
		clk, p1, p2 := twoNodes(t, labelABSplit, "100")
		p2.a.be.DockerUnhealthy = true
		learnedByVariant(p2.group(), abVariantB).markHealthy(false)
		p1.a.srv.Close()
		p2.do("/", []*http.Cookie{p2.pin("B", clk.now())}, nil)
		if c := p1.cum(); c[1].Transport != 1 || c[0].samples() != 0 {
			t.Fatalf("p1 A=%+v B=%+v", c[0], c[1])
		}
	})
}

func TestABMeshFailoverHeaderTrust(t *testing.T) {
	clk, p1, _ := twoNodes(t, labelABSplit, "0")
	// Unauthenticated: stripped, never honoured, never reaches a backend.
	rec := p1.do("/", []*http.Cookie{p1.pin("B", clk.now())}, map[string]string{abFailoverHeader: "1", PeerHopHeader: "1", abVariantHeader: "B"})
	if servedBy(rec) != "B" {
		t.Fatalf("unauthenticated failover flag honoured: served by %q", servedBy(rec))
	}
	if p1.b.gotAuth.Load() != "" || p1.b.gotFailover.Load() != "" {
		t.Fatal("auth or failover header leaked")
	}
	p1.do("/", []*http.Cookie{p1.pin("A", clk.now())}, map[string]string{abFailoverHeader: "1"})
	if p1.a.gotFailover.Load() != "" {
		t.Fatal("failover header reached a backend")
	}
	// Authenticated, but not exactly "1": ignored.
	rec = p1.do("/", nil, map[string]string{abFailoverHeader: "yes", PeerHopHeader: "1", PeerAuthHeader: p1.r.peerHopAuth, abVariantHeader: "B"})
	if servedBy(rec) != "B" {
		t.Fatalf("flag value other than 1 honoured: %q", servedBy(rec))
	}
	// Authenticated "1": the other variant, recorded as failover_B.
	before := p1.cum()
	rec = p1.do("/", nil, map[string]string{abFailoverHeader: "1", PeerHopHeader: "1", PeerAuthHeader: p1.r.peerHopAuth, abVariantHeader: "B"})
	if servedBy(rec) != "A" || p1.a.gotFailover.Load() != "" || p1.a.gotVariant.Load() != "A" {
		t.Fatalf("authenticated failover hop: served by %q, header at backend %q", servedBy(rec), p1.a.gotFailover.Load())
	}
	if c := p1.cum(); c[1].Failover != before[1].Failover+1 || c[0].Requests != before[0].Requests {
		t.Fatalf("failover hop accounting: %+v", c)
	}
}

func TestABMeshHopsLandOnTheRightPool(t *testing.T) {
	clk, p1, p2 := twoNodes(t, labelABSplit, "50")
	// Spread off: p2 prefers its local A for A sessions; only B goes to p1,
	// and only to p1's B.
	for i := 0; i < 20; i++ {
		p2.do("/", []*http.Cookie{p2.pin("A", clk.now())}, nil)
		p2.do("/", []*http.Cookie{p2.pin("B", clk.now())}, nil)
	}
	if p1.a.hits.Load() != 0 || p1.b.hits.Load() != 20 || p2.a.hits.Load() != 20 {
		t.Fatalf("p1 A=%d B=%d p2 A=%d", p1.a.hits.Load(), p1.b.hits.Load(), p2.a.hits.Load())
	}
	// With p2's A down, A sessions hop to p1's A pool, stamped A, no flag.
	p2.a.be.DockerUnhealthy = true
	if servedBy(p2.do("/", []*http.Cookie{p2.pin("A", clk.now())}, nil)) != "A" || p1.a.hits.Load() != 1 {
		t.Fatal("A session should hop to p1's A")
	}
	if p1.a.gotVariant.Load() != "A" || p1.a.gotFailover.Load() != "" {
		t.Fatalf("A hop headers: X-Variant=%v flag=%v", p1.a.gotVariant.Load(), p1.a.gotFailover.Load())
	}
	if c := p1.cum(); c[0].Requests != 1 {
		t.Fatalf("p1 should record the A hop: %+v", c[0])
	}
}

func TestABMeshStalePeerIgnoredByJudge(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%v", stale), func(t *testing.T) {
			f := newABFixture(t, abLabels(labelABWarmup, "0s", labelABMinRuntime, "0s", labelABMinSamples, "10",
				labelABWindow, "1m", labelABWindowMinSamples, "5"), nil)
			bad := abCounts(10, 10, 5)
			good := abCounts(10, 0, 5)
			exp := &abPeerExp{service: "svc", id: abTestID, cfg: f.g.abCfg, phase: abPhaseRunning,
				sum: abSummary{Cumulative: [2]abCounters{abCounts(20, 0, 5), abCounts(20, 20, 5)}, Windows: []abWindow{abWin(0, good, bad), abWin(1, good, bad)}}}
			f.clk.advance(2*time.Minute + 11*time.Second)
			f.r.applyPeerAB("peer1", map[string]*abPeerExp{"svc": exp})
			if stale {
				f.clk.advance(16 * time.Second)
			}
			f.r.abTick(f.clk.now())
			if got := f.g.ab.latched.Load(); got == stale {
				t.Fatalf("latched=%v with stale=%v (judge=%+v)", got, stale, f.g.ab.report().Judge)
			}
			rep := f.g.ab.report()
			if len(rep.Peers) != 1 || rep.Peers[0].Fresh == stale {
				t.Fatalf("peers = %+v", rep.Peers)
			}
		})
	}
}

func TestABMeshJudgeGraceWaitsForPeerWindow(t *testing.T) {
	f := newABFixture(t, abLabels(labelABWarmup, "0s", labelABMinRuntime, "0s", labelABMinSamples, "1",
		labelABWindow, "1m", labelABWindowMinSamples, "5"), nil)
	exp := &abPeerExp{service: "svc", id: abTestID, cfg: f.g.abCfg, phase: abPhaseRunning,
		sum: abSummary{Cumulative: [2]abCounters{abCounts(10, 0, 5), abCounts(10, 0, 5)}, Windows: []abWindow{abWin(0, abCounts(10, 0, 5), abCounts(10, 0, 5))}}}
	f.clk.advance(time.Minute + time.Second) // window 0 just closed
	f.r.applyPeerAB("peer1", map[string]*abPeerExp{"svc": exp})
	f.r.abTick(f.clk.now())
	run := f.g.ab
	run.mu.Lock()
	seen := run.js.Seen
	run.mu.Unlock()
	if seen {
		t.Fatal("window 0 judged before the peer's last push for it could arrive")
	}
	f.clk.advance(10 * time.Second)
	f.r.applyPeerAB("peer1", map[string]*abPeerExp{"svc": exp})
	f.r.abTick(f.clk.now())
	run.mu.Lock()
	seen = run.js.Seen
	run.mu.Unlock()
	if !seen {
		t.Fatal("window 0 should be judged after the grace")
	}
}

func TestABMeshMisalignedPeerNotMerged(t *testing.T) {
	f := newABFixture(t, abLabels(labelABWarmup, "0s"), nil)
	other := *f.g.abCfg
	other.Epoch += 60
	exp := &abPeerExp{service: "svc", id: abTestID, cfg: &other, phase: abPhaseRunning,
		sum: abSummary{Cumulative: [2]abCounters{abCounts(7, 0, 5), {}}}}
	f.r.applyPeerAB("peer1", map[string]*abPeerExp{"svc": exp})
	rep := f.g.ab.report()
	if rep.Merged.Peers != 0 || rep.Merged.Cumulative.A.Requests != 0 || rep.Peers[0].Aligned {
		t.Fatalf("misaligned peer merged: %+v", rep.Merged)
	}
}

func TestABMeshConfigConflictLocalWins(t *testing.T) {
	clk := newABClock()
	p1 := newMeshNode(t, "p1", clk, true, abLabels())
	p2 := newMeshNode(t, "p2", clk, true, abLabels(labelABID, "zzzz9999"))
	push(t, p2, p1)
	g := p1.group()
	if g.abCfg.ID != abTestID || g.ab.id != abTestID {
		t.Fatalf("local test lost: %s", g.abCfg.ID)
	}
	if learnedByVariant(g, abVariantB) != nil {
		t.Fatal("the peer's B of another test must not be routed to")
	}
	a := learnedByVariant(g, abVariantA)
	if a == nil || a.Weight != 1 {
		t.Fatalf("peer A synthetic = %+v (weight must exclude B)", a)
	}
	// Local B also wins while the local B is stopped (labels still present).
	p1.b = nil
	p1.refresh()
	if g := p1.group(); g.abCfg != nil {
		t.Fatalf("a peer with a different id must not take over: %+v", g.abCfg.ID)
	}
	// A peer whose B has the same id lets the local labels attach.
	p3 := newMeshNode(t, "p3", clk, true, abLabels())
	push(t, p3, p1)
	if g := p1.group(); g.ab == nil || g.abCfg.ID != abTestID || learnedByVariant(g, abVariantB) == nil {
		t.Fatal("local labels should attach through the same-id peer B")
	}
}

func TestABMeshInvalidPeerConfigDropped(t *testing.T) {
	clk := newABClock()
	p1 := newMeshNode(t, "p1", clk, true, abLabels())
	p2 := newMeshNode(t, "p2", clk, false, nil)
	body, _ := buildPeerPayload(p1.r, "p1", p1.srv.URL)
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	exp := raw["experiments"].([]any)[0].(map[string]any)
	exp["config"].(map[string]any)[labelABSplit] = "101"
	body, _ = json.Marshal(raw)
	if code := postPayload(t, p2.h, body); code != http.StatusNoContent {
		t.Fatalf("code = %d", code)
	}
	g := p2.group()
	if g.ab != nil || len(p2.store.exps) != 0 {
		t.Fatal("invalid config must not be adopted")
	}
	if len(g.Backends) != 2 || learnedByVariant(g, abVariantB) != nil {
		t.Fatalf("backends = %d — want local A plus the legacy single peer backend", len(g.Backends))
	}
	if l := learnedByVariant(g, abVariantA); l == nil || l.Weight != 2 {
		t.Fatal("legacy peer backend must carry the totals")
	}
}

func TestABMeshPeerExpValidationBounds(t *testing.T) {
	clk := newABClock()
	f := newABFixture(t, abLabels(), nil)
	good := f.g.ab.peerInfo()
	now := clk.now()
	if e, why := abPeerExpFromWire(&good, now); e == nil {
		t.Fatalf("own info rejected: %s", why)
	}
	clone := func() peerABInfo {
		var c peerABInfo
		b, _ := json.Marshal(good)
		_ = json.Unmarshal(b, &c)
		return c
	}
	cases := map[string]func(e *peerABInfo){
		"empty service":      func(e *peerABInfo) { e.Service = "" },
		"service control":    func(e *peerABInfo) { e.Service = "a\nb" },
		"long service":       func(e *peerABInfo) { e.Service = strings.Repeat("s", 129) },
		"bad id":             func(e *peerABInfo) { e.ID = "AB" },
		"id mismatch":        func(e *peerABInfo) { e.Config[labelABID] = "other123" },
		"variant mismatch":   func(e *peerABInfo) { e.Config[labelABVariant] = "A" },
		"unknown key":        func(e *peerABInfo) { e.Config["proxy.host"] = "x" },
		"long value":         func(e *peerABInfo) { e.Config[labelABExclude] = "/" + strings.Repeat("a", abMaxPeerValueLen) },
		"invalid value":      func(e *peerABInfo) { e.Config[labelABErrDelta] = "NaN" },
		"bad phase":          func(e *peerABInfo) { e.Phase = "paused" },
		"inconsistent phase": func(e *peerABInfo) { e.Phase = abPhasePromoting },
		"future phase_at":    func(e *peerABInfo) { e.PhaseAt = now.Add(48 * time.Hour).Unix() },
		"bad abort source":   func(e *peerABInfo) { e.Abort = &abAbortInfo{Reason: "errors", Source: "dashboard"} },
		"bad abort reason":   func(e *peerABInfo) { e.Abort = &abAbortInfo{Reason: "boredom", Source: "auto"} },
		"long abort detail": func(e *peerABInfo) {
			e.Abort = &abAbortInfo{Reason: "errors", Source: "auto", Detail: strings.Repeat("d", 257)}
		},
		"negative b":          func(e *peerABInfo) { e.BBackends = -1 },
		"negative pinned":     func(e *peerABInfo) { e.Pinned.A.ActiveSessions = -1 },
		"pinned over cap":     func(e *peerABInfo) { e.Pinned.B.ActiveSessions = abPinnedCap + 1 },
		"short hist":          func(e *peerABInfo) { e.Cumulative.A.Hist = e.Cumulative.A.Hist[:63] },
		"err5xx > requests":   func(e *peerABInfo) { e.Cumulative.B.Err5xx = 1 },
		"hist sum > requests": func(e *peerABInfo) { e.Cumulative.B.Hist[3] = 1 },
		"count over bound":    func(e *peerABInfo) { e.Cumulative.A.Transport = abMaxPeerCount + 1 },
		"four windows": func(e *peerABInfo) {
			w := abWindowWire{V: abPair[abCountersWire]{abCountersToWire(&abCounters{}), abCountersToWire(&abCounters{})}}
			for i := 0; i < 4; i++ {
				w.Index = int64(i)
				e.Windows = append(e.Windows, w)
			}
		},
		"duplicate window": func(e *peerABInfo) {
			w := abWindowWire{Index: 1, V: abPair[abCountersWire]{abCountersToWire(&abCounters{}), abCountersToWire(&abCounters{})}}
			e.Windows = append(e.Windows, w, w)
		},
		"negative window": func(e *peerABInfo) {
			e.Windows = append(e.Windows, abWindowWire{Index: -1, V: abPair[abCountersWire]{abCountersToWire(&abCounters{}), abCountersToWire(&abCounters{})}})
		},
	}
	for name, mut := range cases {
		e := clone()
		mut(&e)
		if got, _ := abPeerExpFromWire(&e, now); got != nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Latched running → aborted is the one allowed phase difference.
	e := clone()
	e.Phase = abPhaseAborted
	e.Abort = &abAbortInfo{Reason: "latency", Source: "auto", At: now.Unix()}
	if got, why := abPeerExpFromWire(&e, now); got == nil || !got.latched() {
		t.Fatalf("latched experiment rejected: %s", why)
	}
}

func TestABMeshMalformedAndOversizedPayloads(t *testing.T) {
	store := newPeerRouteStore(time.Minute)
	refreshed := 0
	h := peerRoutesHandler(meshSecret, store, func() { refreshed++ })

	// A bad experiment (negative count) is dropped; the routes still merge.
	body := `{"peer":"p","advertise":"http://p:8092","routes":[{"host":"x.example.org","backends":1}],
		"experiments":[{"service":"svc","id":"abcd1234","cumulative":{"A":{"requests":-1}}}]}`
	if code := postPayload(t, h, []byte(body)); code != http.StatusNoContent {
		t.Fatalf("bad experiment failed the push: %d", code)
	}
	if findGroup(store.overlay(nil, nil), "x.example.org", "") == nil || len(store.exps) != 0 {
		t.Fatal("routes must merge, the experiment must be dropped")
	}
	// A non-list experiments field is ignored too.
	body = `{"peer":"p","advertise":"http://p:8092","routes":[{"host":"y.example.org","backends":1}],"experiments":5}`
	if code := postPayload(t, h, []byte(body)); code != http.StatusNoContent {
		t.Fatalf("code = %d", code)
	}
	// Over the experiment cap: all experiments ignored, routes kept.
	var exps []string
	for i := 0; i <= abMaxPeerExperiments; i++ {
		exps = append(exps, `{}`)
	}
	body = `{"peer":"p","advertise":"http://p:8092","routes":[{"host":"z.example.org","backends":1}],"experiments":[` + strings.Join(exps, ",") + `]}`
	if code := postPayload(t, h, []byte(body)); code != http.StatusNoContent {
		t.Fatalf("code = %d", code)
	}
	if findGroup(store.overlay(nil, nil), "z.example.org", "") == nil {
		t.Fatal("routes must survive an over-cap experiments list")
	}
	// Over the body limit: rejected whole.
	big := `{"peer":"p","advertise":"http://p:8092","routes":[{"host":"big.example.org","backends":1,"name":"` +
		strings.Repeat("n", peerPayloadMaxBytes) + `"}]}`
	if code := postPayload(t, h, []byte(big)); code != http.StatusBadRequest {
		t.Fatalf("oversized payload: %d", code)
	}
	// Malformed JSON: rejected.
	if code := postPayload(t, h, []byte(`{"peer":`)); code != http.StatusBadRequest {
		t.Fatalf("malformed payload: %d", code)
	}
}

func TestABMeshMaxSizePayloadAccepted(t *testing.T) {
	clk := newABClock()
	f := newABFixture(t, abLabels(labelABExclude, strings.TrimSuffix(strings.Repeat("/"+strings.Repeat("e", 100)+",", 32), ",")), nil)
	base := f.g.ab.peerInfo()
	big := abCountersWire{Requests: abMaxPeerCount, Err5xx: abMaxPeerCount, Transport: abMaxPeerCount, Failover: abMaxPeerCount, Hist: make([]uint64, abHistBuckets)}
	for i := range big.Hist {
		big.Hist[i] = abMaxPeerCount / abHistBuckets
	}
	var list peerABList
	for i := 0; i < abMaxPeerExperiments; i++ {
		e := base
		e.Service = fmt.Sprintf("svc-%02d", i)
		e.Cumulative = abPair[abCountersWire]{big, big}
		for w := 0; w < abRingSize; w++ {
			e.Windows = append(e.Windows, abWindowWire{Index: int64(w), V: abPair[abCountersWire]{big, big}})
		}
		list = append(list, e)
	}
	body, _ := json.Marshal(peerRoutePayload{Peer: "p", Advertise: "http://p:8092", Routes: []peerRouteInfo{{Host: "x.example.org", Backends: 1}}, Experiments: list})
	if len(body) > peerPayloadMaxBytes {
		t.Fatalf("large payload %d bytes is over the limit", len(body))
	}
	store := newPeerRouteStore(time.Minute)
	store.now = clk.now
	if code := postPayload(t, peerRoutesHandler(meshSecret, store, nil), body); code != http.StatusNoContent {
		t.Fatalf("code = %d", code)
	}
	if len(store.exps["p"]) != abMaxPeerExperiments {
		t.Fatalf("validated %d of %d experiments", len(store.exps["p"]), abMaxPeerExperiments)
	}
}

func TestABMeshSenderDropsExperimentsOverLimit(t *testing.T) {
	srv := newABSrv(t, "x.example.org", abVariantA)
	f := newABFixture(t, abLabels(), nil)
	body, ok := buildPeerPayload(f.r, "p", "http://p")
	if !ok || !strings.Contains(string(body), `"experiments"`) {
		t.Fatal("normal payload should carry experiments")
	}
	// Big route name + a test: experiments dropped, routes kept.
	f.r.Set([]*RouteGroup{f.g, {Host: "x.example.org", Name: strings.Repeat("n", peerPayloadMaxBytes-1000), Backends: []*Backend{srv.be}}})
	body, ok = buildPeerPayload(f.r, "p", "http://p")
	if !ok || strings.Contains(string(body), `"experiments"`) {
		t.Fatal("oversized payload should drop experiments")
	}
}

// ---- compatibility ----

// oldPeerRouteInfo/oldPeerRoutePayload are the pre-A/B wire shapes.
type oldPeerRouteInfo struct {
	Host        string `json:"host"`
	PathPrefix  string `json:"path,omitempty"`
	StripPrefix bool   `json:"strip,omitempty"`
	Name        string `json:"name,omitempty"`
	Service     string `json:"service,omitempty"`
	Backends    int    `json:"backends"`
	Weight      int    `json:"weight,omitempty"`
	RateLimit   bool   `json:"ratelimit,omitempty"`
	RateRPM     int    `json:"ratelimit_rpm,omitempty"`
	Spread      bool   `json:"spread,omitempty"`
}

type oldPeerRoutePayload struct {
	Peer      string             `json:"peer"`
	Advertise string             `json:"advertise"`
	Routes    []oldPeerRouteInfo `json:"routes"`
}

func TestABMeshCompatNewSenderOldReceiver(t *testing.T) {
	clk := newABClock()
	p1 := newMeshNode(t, "p1", clk, true, abLabels())
	body, _ := buildPeerPayload(p1.r, "p1", p1.srv.URL)
	var old oldPeerRoutePayload
	if err := json.Unmarshal(body, &old); err != nil {
		t.Fatal(err)
	}
	if len(old.Routes) != 1 || old.Routes[0].Backends != 2 || old.Routes[0].Weight != 2 {
		t.Fatalf("old receiver must see totals: %+v", old.Routes)
	}
	var probe struct {
		Routes []map[string]any `json:"routes"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Routes[0]["ab_id"] != abTestID || probe.Routes[0]["b_backends"] != float64(1) || probe.Routes[0]["b_weight"] != float64(1) {
		t.Fatalf("new fields = %v", probe.Routes[0])
	}
}

func TestABMeshCompatOldSenderNewReceiver(t *testing.T) {
	clk := newABClock()
	p1 := newMeshNode(t, "p1", clk, true, abLabels())
	body, _ := json.Marshal(oldPeerRoutePayload{Peer: "old", Advertise: "http://old:8092",
		Routes: []oldPeerRouteInfo{{Host: abTestHost, Service: "svc", Backends: 3, Weight: 3}}})
	if code := postPayload(t, p1.h, body); code != http.StatusNoContent {
		t.Fatal(code)
	}
	g := p1.group()
	var learned []*Backend
	for _, b := range g.Backends {
		if b.Learned {
			learned = append(learned, b)
		}
	}
	if len(learned) != 1 || learned[0].Variant != "" || learned[0].Weight != 3 || learned[0].Container != "peer:old" {
		t.Fatalf("old sender should yield one A backend with its totals: %+v", learned)
	}
	if g.abCfg == nil || g.abCfg.ID != abTestID {
		t.Fatal("local test must stay attached")
	}
	// A receiver without a local test never adopts from an old sender.
	p2 := newMeshNode(t, "p2", clk, false, nil)
	if code := postPayload(t, p2.h, body); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if p2.group().ab != nil {
		t.Fatal("no test without experiments")
	}
}

func TestABMeshNoRegressionWithoutTests(t *testing.T) {
	srv := newABSrv(t, "plain.example.org", abVariantA)
	r := &Router{}
	r.Set([]*RouteGroup{{Host: "plain.example.org", Service: "plain", Backends: []*Backend{srv.be}, RateLimit: true, RateRPM: 5}})
	body, ok := buildPeerPayload(r, "a", "http://a:8092")
	if !ok {
		t.Fatal("no payload")
	}
	old, _ := json.Marshal(oldPeerRoutePayload{Peer: "a", Advertise: "http://a:8092",
		Routes: []oldPeerRouteInfo{{Host: "plain.example.org", Service: "plain", Backends: 1, Weight: 1, RateLimit: true, RateRPM: 5}}})
	if string(body) != string(old) {
		t.Fatalf("payload without tests changed:\n%s\n%s", body, old)
	}

	// Overlay of a route without B fields: the same single backend as before.
	s := newPeerRouteStore(time.Minute)
	var p peerRoutePayload
	_ = json.Unmarshal(body, &p)
	if !s.merge(p) {
		t.Fatal("first merge should report a change")
	}
	g := findGroup(s.overlay(nil, nil), "plain.example.org", "")
	if len(g.Backends) != 1 {
		t.Fatalf("backends = %d", len(g.Backends))
	}
	b := g.Backends[0]
	if !b.Learned || b.Variant != "" || b.Weight != 1 || b.Container != "peer:a" || b.stickyID != backendStickyID("http://a:8092") || g.abCfg != nil {
		t.Fatalf("legacy backend = %+v", b)
	}
	if s.merge(p) {
		t.Fatal("steady-state merge reported a change")
	}
}

// ---- store behaviour ----

func TestABMeshMergeChangeDetection(t *testing.T) {
	clk, p1, p2 := twoNodes(t)
	base := p2.refreshes
	// Stats-only change: no refresh.
	p1.do("/", []*http.Cookie{p1.pin("B", clk.now())}, nil)
	push(t, p1, p2)
	if p2.refreshes != base {
		t.Fatal("a stats-only push must not refresh")
	}
	// Latch: refresh (phase changed).
	run := p1.run()
	run.mu.Lock()
	run.abort = abAbortInfo{Reason: "errors", At: clk.now().Unix(), Source: "auto"}
	run.latched.Store(true)
	run.mu.Unlock()
	push(t, p1, p2)
	if p2.refreshes != base+1 {
		t.Fatalf("a phase change must refresh once (%d)", p2.refreshes-base)
	}
	// B count change: refresh.
	b2 := newABSrv(t, abTestHost, abVariantB)
	p1.r.Set([]*RouteGroup{{Host: abTestHost, Service: "svc", Backends: []*Backend{p1.a.be, p1.b.be, b2.be}, abCfg: p1.cfg}})
	push(t, p1, p2)
	if p2.refreshes != base+2 {
		t.Fatal("a B count change must refresh")
	}
	if b := learnedByVariant(p2.group(), abVariantB); b == nil || b.Weight != 2 {
		t.Fatalf("B weight = %+v", b)
	}
}

func TestABMeshHealthCarryOverDistinct(t *testing.T) {
	_, _, p2 := twoNodes(t)
	learnedByVariant(p2.group(), abVariantB).markHealthy(false)
	p2.refresh()
	g := p2.group()
	if learnedByVariant(g, abVariantB).healthyFlag.Load() || !learnedByVariant(g, abVariantA).healthyFlag.Load() {
		t.Fatal("A and B synthetic backends to the same peer must carry health separately")
	}
}

func TestABMeshAdoptionEndsWhenOwnerRemovesB(t *testing.T) {
	_, p1, p2 := twoNodes(t)
	if p2.group().ab == nil {
		t.Fatal("adopted")
	}
	p1.b = nil
	p1.refresh()
	push(t, p1, p2)
	if p2.group().ab != nil || len(p2.r.ABReport("").Experiments) != 0 {
		t.Fatal("p2 must detach once p1 advertises no B")
	}
	push(t, p2, p1)
	if p1.group().ab != nil {
		t.Fatal("p2's former adoption must not keep a test alive on p1")
	}
}

func TestABMeshExperimentsExpireWithTTL(t *testing.T) {
	clk, _, p2 := twoNodes(t)
	clk.advance(16 * time.Second)
	if !p2.store.hasExpired() {
		t.Fatal("expired entries should be reported")
	}
	p2.refresh()
	if len(p2.store.exps) != 0 || p2.group().ab != nil {
		t.Fatal("a silent peer's experiment must expire with the TTL")
	}
}

func TestABMeshPhaseAdoptedFromPeerLabels(t *testing.T) {
	clk, p1, p2 := twoNodes(t, labelABSplit, "100")
	p1.cfg = parseABConfig(abLabels(labelABSplit, "100", labelABPhase, abPhaseDiscarding,
		labelABStarted, p1.cfg.labels()[labelABStarted], labelABPhaseAt, strconv.FormatInt(clk.now().Unix(), 10)), clk.now(), noWarn)
	p1.refresh()
	push(t, p1, p2)
	if rep := p2.r.ABReport("svc").Experiments[0]; rep.Phase != abPhaseDiscarding {
		t.Fatalf("p2 phase = %s", rep.Phase)
	}
	if servedBy(p2.do("/", []*http.Cookie{{Name: "ab_uid", Value: "n"}}, nil)) != "A" {
		t.Fatal("discarding: new sessions on p2 go to A")
	}
	if servedBy(p2.do("/", []*http.Cookie{p2.pin("B", clk.now())}, nil)) != "B" {
		t.Fatal("discarding: B pins drain on p1's B")
	}
}

func TestABMeshNoSecretsInPayloadOrReport(t *testing.T) {
	_, p1, p2 := twoNodes(t, labelABSplit, "100")
	rec := p2.do("/", []*http.Cookie{{Name: "ab_uid", Value: "secret-uid"}}, map[string]string{"Authorization": "Bearer tok"})
	p, _ := pinFromRec(t, rec)
	p2.do("/", []*http.Cookie{{Name: abPinCookieName("svc"), Value: p.String()}}, nil)
	body, _ := buildPeerPayload(p2.r, "p2", p2.srv.URL)
	rep, _ := json.Marshal(p1.r.ABReport(""))
	for _, s := range []string{string(body), string(rep)} {
		for _, leak := range []string{p1.r.peerHopAuth, meshSecret, "secret-uid", "Bearer", p.nonce} {
			if strings.Contains(s, leak) {
				t.Fatalf("leaks %q", leak)
			}
		}
	}
}

// A peer that does not run this test (older binary, or no test) gets the
// PR1 hop: the served variant, no failover flag, and the forwarder records
// the failover itself — the receiver cannot.
func TestABMeshFailoverToPeerWithoutTest(t *testing.T) {
	clk := newABClock()
	p1 := newMeshNode(t, "p1", clk, true, abLabels(labelABSplit, "100"))
	plainA := newABSrv(t, abTestHost, abVariantA)
	plain := &Router{peerHopAuth: peerHopAuthToken(meshSecret)}
	plain.Set([]*RouteGroup{{Host: abTestHost, Service: "svc", Backends: []*Backend{plainA.be}}})
	plainSrv := httptest.NewServer(plain)
	t.Cleanup(plainSrv.Close)
	body, _ := json.Marshal(oldPeerRoutePayload{Peer: "old", Advertise: plainSrv.URL,
		Routes: []oldPeerRouteInfo{{Host: abTestHost, Service: "svc", Backends: 1}}})
	if code := postPayload(t, p1.h, body); code != http.StatusNoContent {
		t.Fatal(code)
	}
	p1.a.be.DockerUnhealthy = true
	p1.b.be.DockerUnhealthy = true
	rec := p1.do("/", []*http.Cookie{p1.pin("B", clk.now())}, nil)
	if servedBy(rec) != "A" || plainA.hits.Load() != 1 {
		t.Fatalf("served by %q", servedBy(rec))
	}
	if plainA.gotVariant.Load() != "A" || plainA.gotFailover.Load() != "" {
		t.Fatalf("backend saw X-Variant=%v flag=%v", plainA.gotVariant.Load(), plainA.gotFailover.Load())
	}
	if c := p1.cum(); c[1].Failover != 1 || c[0].samples() != 0 {
		t.Fatalf("forwarder must record the failover: A=%+v B=%+v", c[0], c[1])
	}
}

// The adopter advertises only its A count (no ab_id), yet runs the test:
// the owner's failover to it uses the hop protocol and the adopter records.
func TestABMeshFailoverToAdopterRecordedThere(t *testing.T) {
	clk, p1, p2 := twoNodes(t, labelABSplit, "100")
	learned := learnedByVariant(p1.group(), abVariantA)
	if learned == nil || learned.peerTestID != abTestID {
		t.Fatalf("adopter's backend should be marked as running the test: %+v", learned)
	}
	p1.a.be.DockerUnhealthy = true
	p1.b.be.DockerUnhealthy = true
	rec := p1.do("/", []*http.Cookie{p1.pin("B", clk.now())}, nil)
	if servedBy(rec) != "A" || p2.a.hits.Load() != 1 || p2.a.gotVariant.Load() != "A" || p2.a.gotFailover.Load() != "" {
		t.Fatalf("served by %q", servedBy(rec))
	}
	c1, c2 := p1.cum(), p2.cum()
	if c2[1].Failover != 1 || c2[0].samples() != 0 || c1[0].samples()+c1[1].samples() != 0 {
		t.Fatalf("p1 A=%d B=%d, p2 A=%+v B=%+v", c1[0].samples(), c1[1].samples(), c2[0], c2[1])
	}
}
