package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fixtures ----

type abClock struct{ ns atomic.Int64 }

func newABClock() *abClock {
	c := &abClock{}
	c.ns.Store(time.Now().Truncate(time.Second).UnixNano())
	return c
}
func (c *abClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *abClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

type abSrv struct {
	srv          *httptest.Server
	be           *Backend
	hits         atomic.Int32
	status       atomic.Int32 // 0 = 200
	staticCode   atomic.Int32 // status for /_next/static/ paths, 0 = status
	gotVariant   atomic.Value
	gotQuery     atomic.Value
	gotAuth      atomic.Value
	gotFailover  atomic.Value
	setAppCookie bool
}

func newABSrv(t *testing.T, host, variant string) *abSrv {
	t.Helper()
	s := &abSrv{}
	s.gotVariant.Store("")
	s.gotQuery.Store("")
	s.gotAuth.Store("")
	s.gotFailover.Store("")
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.gotVariant.Store(r.Header.Get(abVariantHeader))
		s.gotQuery.Store(r.URL.RawQuery)
		s.gotAuth.Store(r.Header.Get(PeerAuthHeader))
		s.gotFailover.Store(r.Header.Get(abFailoverHeader))
		w.Header().Set("X-Served-By", variant)
		if s.setAppCookie {
			w.Header().Add("Set-Cookie", "app_session=xyz; Path=/")
		}
		code := int(s.status.Load())
		if strings.HasPrefix(r.URL.Path, "/_next/static/") {
			if sc := int(s.staticCode.Load()); sc != 0 {
				code = sc
			}
			w.Header().Set("X-Static-From", variant)
		}
		if code == 0 {
			code = 200
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "body-"+variant)
	}))
	t.Cleanup(s.srv.Close)
	s.be = mkBackend(t, host, s.srv)
	if variant == abVariantB {
		s.be.Variant = abVariantB
	}
	return s
}

type abFixture struct {
	t   *testing.T
	clk *abClock
	r   *Router
	g   *RouteGroup
	a   *abSrv
	b   *abSrv
}

const abTestHost = "ab.example.org"
const abTestID = "t0st1234"

func abLabels(kv ...string) map[string]string {
	m := map[string]string{labelABVariant: "B", labelABID: abTestID, labelService: "svc", labelABWarmup: "0s"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func noWarn(string, string) {}

// newABFixture builds a one-A one-B group for service "svc". tweak runs
// before Router.Set so callers can toggle Sticky/CacheTTL/etc.
func newABFixture(t *testing.T, labels map[string]string, tweak func(g *RouteGroup)) *abFixture {
	t.Helper()
	f := &abFixture{t: t, clk: newABClock()}
	if _, ok := labels[labelABStarted]; !ok {
		labels[labelABStarted] = strconv.FormatInt(f.clk.now().Unix(), 10)
	}
	f.a = newABSrv(t, abTestHost, abVariantA)
	f.b = newABSrv(t, abTestHost, abVariantB)
	f.g = &RouteGroup{Host: abTestHost, Service: "svc", Backends: []*Backend{f.a.be, f.b.be}}
	f.g.abCfg = parseABConfig(labels, f.clk.now(), noWarn)
	if tweak != nil {
		tweak(f.g)
	}
	f.r = &Router{now: f.clk.now}
	f.r.Set([]*RouteGroup{f.g})
	return f
}

func (f *abFixture) do(method, target string, cookies []*http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+abTestHost+target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)
	return rec
}

func (f *abFixture) cfg() *abConfig { return f.g.abCfg }

func (f *abFixture) pin(variant, src string, issued, last time.Time) *http.Cookie {
	p := abPin{id: f.cfg().ID, variant: variant, src: src, nonce: "0badc0de", issued: issued.Unix(), lastSeen: last.Unix()}
	return &http.Cookie{Name: abPinCookieName("svc"), Value: p.String()}
}

func (f *abFixture) freshPin(variant string) *http.Cookie {
	return f.pin(variant, "u", f.clk.now(), f.clk.now())
}

func (f *abFixture) cum() [2]abCounters {
	f.g.ab.mu.Lock()
	defer f.g.ab.mu.Unlock()
	return f.g.ab.cum
}

func setCookieNamed(rec *httptest.ResponseRecorder, name string) []string {
	var out []string
	for _, v := range rec.Header().Values("Set-Cookie") {
		if strings.HasPrefix(v, name+"=") {
			out = append(out, v)
		}
	}
	return out
}

func servedBy(rec *httptest.ResponseRecorder) string { return rec.Header().Get("X-Served-By") }

func pinFromRec(t *testing.T, rec *httptest.ResponseRecorder) (abPin, bool) {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == abPinCookieName("svc") {
			m := abPinRe.FindStringSubmatch(c.Value)
			if m == nil {
				t.Fatalf("issued pin %q does not match the strict regex", c.Value)
			}
			is, _ := strconv.ParseInt(m[5], 10, 64)
			ls, _ := strconv.ParseInt(m[6], 10, 64)
			return abPin{id: m[1], variant: m[2], src: m[3], nonce: m[4], issued: is, lastSeen: ls}, true
		}
	}
	return abPin{}, false
}

// ---- label parsing (§2) ----

func TestABParseDefaults(t *testing.T) {
	now := time.Now()
	c := parseABConfig(map[string]string{labelABID: "abcd"}, now, func(k, v string) { t.Errorf("unexpected warn %s=%s", k, v) })
	if c.Assign != "cookie" || c.Split != 10 || !c.Anon || c.SessionIdle != 30*time.Minute ||
		c.PinRefresh != 5*time.Minute || c.MaxSession != 24*time.Hour || c.Phase != abPhaseRunning ||
		len(c.Static) != 1 || c.Static[0] != "/_next/static/" || c.CookieJS || c.Override || !c.AutoAbort ||
		c.MinSamples != 500 || c.MinRuntime != 15*time.Minute || c.Warmup != 3*time.Minute ||
		c.Window != 5*time.Minute || c.Windows != 2 || c.WindowMinSamples != 50 ||
		c.ErrDelta != 2 || c.ErrRatio != 2 || c.P95Ratio != 1.5 || c.P95Slack != 200*time.Millisecond ||
		c.UIDCookie != "ab_uid" || c.GroupCookie != "ab_group" || len(c.Groups) != 0 || c.Started != 0 {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestABParseBounds(t *testing.T) {
	now := time.Now()
	nowS := now.Unix()
	cases := []struct {
		key, val string
		valid    bool
		check    func(c *abConfig) bool
	}{
		{labelABSplit, "0", true, func(c *abConfig) bool { return c.Split == 0 }},
		{labelABSplit, "100", true, func(c *abConfig) bool { return c.Split == 100 }},
		{labelABSplit, "101", false, func(c *abConfig) bool { return c.Split == 10 }},
		{labelABSplit, "-1", false, func(c *abConfig) bool { return c.Split == 10 }},
		{labelABSplit, "1e1", false, func(c *abConfig) bool { return c.Split == 10 }},
		{labelABAssign, "random", true, func(c *abConfig) bool { return c.Assign == "random" }},
		{labelABAssign, "header:x-tenant", true, func(c *abConfig) bool { return c.Assign == "header" && c.Header == "X-Tenant" }},
		{labelABAssign, "header:Cookie", false, func(c *abConfig) bool { return c.Assign == "cookie" }},
		{labelABAssign, "header:host", false, func(c *abConfig) bool { return c.Assign == "cookie" }},
		{labelABAssign, "header:bad name", false, func(c *abConfig) bool { return c.Assign == "cookie" }},
		{labelABAssign, "header:" + strings.Repeat("a", 65), false, func(c *abConfig) bool { return c.Assign == "cookie" }},
		{labelABAssign, "header:", false, func(c *abConfig) bool { return c.Assign == "cookie" }},
		{labelABAssign, "sticky", false, func(c *abConfig) bool { return c.Assign == "cookie" }},
		{labelABGroups, "staff:B, qa:A", true, func(c *abConfig) bool { return c.Groups["staff"] == "B" && c.Groups["qa"] == "A" }},
		{labelABGroups, "Staff:B", false, func(c *abConfig) bool { return len(c.Groups) == 0 }},
		{labelABGroups, "staff:C", false, func(c *abConfig) bool { return len(c.Groups) == 0 }},
		{labelABGroups, "staff", false, func(c *abConfig) bool { return len(c.Groups) == 0 }},
		{labelABGroups, strings.Repeat("g:A,", 33), false, func(c *abConfig) bool { return len(c.Groups) == 0 }},
		{labelABUIDCookie, "my_uid", true, func(c *abConfig) bool { return c.UIDCookie == "my_uid" }},
		{labelABUIDCookie, "bad;name", false, func(c *abConfig) bool { return c.UIDCookie == "ab_uid" }},
		{labelABGroupCookie, strings.Repeat("x", 65), false, func(c *abConfig) bool { return c.GroupCookie == "ab_group" }},
		{labelABAnon, "false", true, func(c *abConfig) bool { return !c.Anon }},
		{labelABAnon, "no", false, func(c *abConfig) bool { return c.Anon }},
		{labelABOverride, "1", false, func(c *abConfig) bool { return !c.Override }},
		{labelABSessionIdle, "4m", false, func(c *abConfig) bool { return c.SessionIdle == 30*time.Minute }},
		{labelABSessionIdle, "25h", false, func(c *abConfig) bool { return c.SessionIdle == 30*time.Minute }},
		{labelABSessionIdle, "5m", true, func(c *abConfig) bool { return c.SessionIdle == 5*time.Minute && c.PinRefresh == 150*time.Second }},
		{labelABPinRefresh, "16m", false, func(c *abConfig) bool { return c.PinRefresh == 5*time.Minute }},
		{labelABPinRefresh, "30s", false, func(c *abConfig) bool { return c.PinRefresh == 5*time.Minute }},
		{labelABPinRefresh, "15m", true, func(c *abConfig) bool { return c.PinRefresh == 15*time.Minute }},
		{labelABMaxSession, "59m", false, func(c *abConfig) bool { return c.MaxSession == 24*time.Hour }},
		{labelABMaxSession, "169h", false, func(c *abConfig) bool { return c.MaxSession == 24*time.Hour }},
		{labelABMinRuntime, "25h", false, func(c *abConfig) bool { return c.MinRuntime == 15*time.Minute }},
		{labelABWarmup, "0s", true, func(c *abConfig) bool { return c.Warmup == 0 }},
		{labelABWarmup, "2h", false, func(c *abConfig) bool { return c.Warmup == 3*time.Minute }},
		{labelABWindow, "30s", false, func(c *abConfig) bool { return c.Window == 5*time.Minute }},
		{labelABWindows, "13", false, func(c *abConfig) bool { return c.Windows == 2 }},
		{labelABWindows, "0", false, func(c *abConfig) bool { return c.Windows == 2 }},
		{labelABMinSamples, "10000001", false, func(c *abConfig) bool { return c.MinSamples == 500 }},
		{labelABWindowMinSamples, "0", false, func(c *abConfig) bool { return c.WindowMinSamples == 50 }},
		{labelABErrDelta, "NaN", false, func(c *abConfig) bool { return c.ErrDelta == 2 }},
		{labelABErrDelta, "Inf", false, func(c *abConfig) bool { return c.ErrDelta == 2 }},
		{labelABErrDelta, "0.05", false, func(c *abConfig) bool { return c.ErrDelta == 2 }},
		{labelABErrDelta, "0.5", true, func(c *abConfig) bool { return c.ErrDelta == 0.5 }},
		{labelABErrRatio, "0.9", false, func(c *abConfig) bool { return c.ErrRatio == 2 }},
		{labelABP95Ratio, "101", false, func(c *abConfig) bool { return c.P95Ratio == 1.5 }},
		{labelABP95Slack, "61s", false, func(c *abConfig) bool { return c.P95Slack == 200*time.Millisecond }},
		{labelABStarted, strconv.FormatInt(nowS-3600, 10), true, func(c *abConfig) bool { return c.Started == nowS-3600 }},
		{labelABStarted, strconv.FormatInt(nowS-2*86400, 10), true, func(c *abConfig) bool { return c.Started == nowS-2*86400 }},
		{labelABStarted, strconv.FormatInt(nowS-91*86400, 10), false, func(c *abConfig) bool { return c.Started == 0 }},
		{labelABEpoch, strconv.FormatInt(nowS+2*86400, 10), false, func(c *abConfig) bool { return c.Epoch == 0 }},
		{labelABStarted, strconv.FormatInt(nowS+2*86400, 10), false, func(c *abConfig) bool { return c.Started == 0 }},
		{labelABEpoch, "abc", false, func(c *abConfig) bool { return c.Epoch == 0 }},
		{labelABPhase, "promoting", true, func(c *abConfig) bool { return c.Phase == abPhasePromoting }},
		{labelABPhase, "paused", false, func(c *abConfig) bool { return c.Phase == abPhaseRunning }},
		{labelABAbortReason, "boredom", false, func(c *abConfig) bool { return c.AbortReason == "" }},
		{labelABExclude, "/api/cron, /api/health", true, func(c *abConfig) bool { return len(c.Exclude) == 2 && c.Exclude[1] == "/api/health" }},
		{labelABExclude, "api/cron", false, func(c *abConfig) bool { return len(c.Exclude) == 0 }},
		{labelABExclude, "/a\x01b", false, func(c *abConfig) bool { return len(c.Exclude) == 0 }},
		{labelABExclude, "/" + strings.Repeat("a", 256), false, func(c *abConfig) bool { return len(c.Exclude) == 0 }},
		{labelABExclude, strings.Repeat("/a,", 33), false, func(c *abConfig) bool { return len(c.Exclude) == 0 }},
		{labelABStatic, "none", true, func(c *abConfig) bool { return len(c.Static) == 0 }},
		{labelABStatic, "/assets/", true, func(c *abConfig) bool { return len(c.Static) == 1 && c.Static[0] == "/assets/" }},
		{labelABStatic, "assets", false, func(c *abConfig) bool { return len(c.Static) == 1 && c.Static[0] == abDefaultStatic }},
	}
	for _, tc := range cases {
		var warned bool
		c := parseABConfig(map[string]string{labelABID: "abcd", tc.key: tc.val}, now, func(string, string) { warned = true })
		if warned == tc.valid {
			t.Errorf("%s=%q: warned=%v, want valid=%v", tc.key, tc.val, warned, tc.valid)
		}
		if !tc.check(c) {
			t.Errorf("%s=%q: unexpected config %+v", tc.key, tc.val, c)
		}
	}
}

func TestABConfigLabelsRoundTrip(t *testing.T) {
	now := time.Now()
	in := parseABConfig(abLabels(labelABAssign, "header:x-tenant", labelABGroups, "b:B,a:A", labelABExclude, "/x,/y",
		labelABStatic, "none", labelABErrDelta, "0.5", labelABStarted, strconv.FormatInt(now.Unix(), 10)), now, noWarn)
	out := parseABConfig(in.labels(), now, func(k, v string) { t.Errorf("re-parse warned on %s=%q", k, v) })
	if fmt.Sprint(in.labels()) != fmt.Sprint(out.labels()) {
		t.Fatalf("round trip differs:\n%v\n%v", in.labels(), out.labels())
	}
}

func TestABScan(t *testing.T) {
	mk := func(name string, labels map[string]string) dockerContainer {
		return dockerContainer{Names: []string{"/" + name}, Labels: labels}
	}
	res := abScan([]dockerContainer{
		mk("scan-a", map[string]string{labelService: "svc"}),
		mk("scan-va", map[string]string{labelService: "svc", labelABVariant: "A", labelABID: "!!"}),
		mk("scan-junk", map[string]string{labelService: "svc", labelABVariant: "C", labelABID: "abcd"}),
		mk("scan-badid", map[string]string{labelService: "svc", labelABVariant: "B", labelABID: "AB"}),
		mk("scan-nosvc", map[string]string{labelABVariant: "B", labelABID: "abcd"}),
		mk("scan-b2", map[string]string{labelService: "svc", labelABVariant: "B", labelABID: "bbbb", labelABSplit: "50"}),
		mk("scan-b1z", map[string]string{labelService: "svc", labelABVariant: "B", labelABID: "aaaa", labelABSplit: "30"}),
		mk("scan-b1a", map[string]string{labelService: "svc", labelABVariant: "B", labelABID: "aaaa", labelABSplit: "20"}),
	}, time.Now())
	for _, n := range []string{"scan-junk", "scan-badid", "scan-nosvc", "scan-b2"} {
		if !res.excluded[n] {
			t.Errorf("%s should be excluded", n)
		}
	}
	for _, n := range []string{"scan-a", "scan-va", "scan-b1z", "scan-b1a"} {
		if res.excluded[n] {
			t.Errorf("%s should not be excluded", n)
		}
	}
	if !res.bNames["scan-b1a"] || !res.bNames["scan-b1z"] || res.bNames["scan-va"] {
		t.Fatalf("bNames = %v", res.bNames)
	}
	cfg := res.configs["svc"]
	if cfg == nil || cfg.ID != "aaaa" || cfg.Split != 20 {
		t.Fatalf("config should come from the smallest-named container of the smallest id, got %+v", cfg)
	}
}

func TestAssembleGroupsAB(t *testing.T) {
	base := map[string]string{labelHost: "asm-ab.example.org", labelPort: "80", labelService: "asmsvc"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	dc := fakeDocker(t, dockerJSON(
		container("1", "asm-a", "running", with(), map[string]string{managedNetwork: "10.0.0.1"}),
		container("2", "asm-b", "running", with(labelCanary, "true", labelABVariant, "B", labelABID, "abcd1234", labelABSplit, "40"), map[string]string{managedNetwork: "10.0.0.2"}),
		container("3", "asm-junk", "running", with(labelABVariant, "X"), map[string]string{managedNetwork: "10.0.0.3"}),
		container("4", "noab-a", "running", map[string]string{labelHost: "noab.example.org", labelPort: "80", labelService: "other"}, map[string]string{managedNetwork: "10.0.0.4"}),
	))
	groups, _, err := assembleGroups(context.Background(), dc, "")
	if err != nil {
		t.Fatal(err)
	}
	g := findGroup(groups, "asm-ab.example.org", "")
	if g == nil || len(g.Backends) != 2 {
		t.Fatalf("group = %+v (bad-variant backend must be excluded)", g)
	}
	if g.abCfg == nil || g.abCfg.ID != "abcd1234" || g.abCfg.Split != 40 {
		t.Fatalf("abCfg = %+v", g.abCfg)
	}
	if g.Backends[0].Variant != "" || g.Backends[1].Variant != abVariantB {
		t.Fatalf("variants = %q %q", g.Backends[0].Variant, g.Backends[1].Variant)
	}
	if n := findGroup(groups, "noab.example.org", ""); n == nil || n.abCfg != nil {
		t.Fatal("a group without B replicas must carry no test")
	}
}

// ---- hashing ----

func TestABHashAgreementSplitAndReshuffle(t *testing.T) {
	cfg := parseABConfig(abLabels(labelABSplit, "30"), time.Now(), noWarn)
	other := *cfg
	other.ID = "zzzz9999"
	b, changed := 0, 0
	for i := 0; i < 10000; i++ {
		uid := fmt.Sprintf("user-%d", i)
		v := abSplitVariant(cfg, uid)
		if v == abVariantB {
			b++
		}
		if abSplitVariant(&other, uid) != v {
			changed++
		}
	}
	if pct := float64(b) / 100; math.Abs(pct-30) > 1 {
		t.Fatalf("B share = %.2f%%, want 30±1", pct)
	}
	if changed < 3000 {
		t.Fatalf("a new id should reshuffle cohorts, only %d of 10000 changed", changed)
	}

	// Two independent Router instances agree for the same uid.
	f1 := newABFixture(t, abLabels(labelABSplit, "50"), nil)
	f2 := newABFixture(t, abLabels(labelABSplit, "50"), nil)
	for i := 0; i < 40; i++ {
		c := []*http.Cookie{{Name: "ab_uid", Value: fmt.Sprintf("u%d", i)}}
		v1, v2 := servedBy(f1.do("GET", "/", c, nil)), servedBy(f2.do("GET", "/", c, nil))
		if v1 != v2 || v1 != abSplitVariant(f1.cfg(), fmt.Sprintf("u%d", i)) {
			t.Fatalf("uid u%d: router1=%s router2=%s", i, v1, v2)
		}
	}
}

// ---- precedence (§1.3) ----

func TestABPrecedenceExcludedPath(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "100", labelABExclude, "/api/cron"), nil)
	rec := f.do("GET", "/api/cron/run", []*http.Cookie{f.freshPin("B")}, map[string]string{abVariantHeader: "B"})
	if servedBy(rec) != "A" || f.a.gotVariant.Load() != "A" {
		t.Fatalf("excluded path served by %s, X-Variant %q", servedBy(rec), f.a.gotVariant.Load())
	}
	if len(rec.Header().Values("Set-Cookie")) != 0 || rec.Header().Get("Vary") != "" {
		t.Fatalf("excluded path must get no cookie/Vary: %v", rec.Header())
	}
	if c := f.cum(); c[0].samples() != 0 || c[1].samples() != 0 {
		t.Fatal("excluded path must not be recorded")
	}
	if f.g.ab.report().Pinned.B.ActiveSessions != 0 {
		t.Fatal("excluded path must not count as pinned activity")
	}
}

func TestABPrecedenceAuthenticatedHop(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "0"), nil)
	f.r.peerHopAuth = peerHopAuthToken("s3cret")
	rec := f.do("GET", "/", nil, map[string]string{PeerHopHeader: "1", PeerAuthHeader: f.r.peerHopAuth, abVariantHeader: "B"})
	if servedBy(rec) != "B" || f.b.gotVariant.Load() != "B" {
		t.Fatalf("authenticated hop should keep X-Variant B, served by %s", servedBy(rec))
	}
	if f.b.gotAuth.Load() != "" {
		t.Fatal("PeerAuthHeader must never reach a backend")
	}
	if len(rec.Header().Values("Set-Cookie")) != 0 {
		t.Fatal("a hop must never issue a pin")
	}
	if c := f.cum(); c[1].Requests != 1 {
		t.Fatalf("authenticated hop should be recorded for B, cum=%+v", c[1])
	}
	// Malformed variant falls through to normal (unrecorded) assignment.
	rec = f.do("GET", "/", nil, map[string]string{PeerHopHeader: "1", PeerAuthHeader: f.r.peerHopAuth, abVariantHeader: "b"})
	if servedBy(rec) != "A" {
		t.Fatalf("malformed X-Variant should fall through, served by %s", servedBy(rec))
	}
	if c := f.cum(); c[0].samples() != 0 {
		t.Fatal("fall-through hop must not be recorded")
	}
}

func TestABForgedHop(t *testing.T) {
	for _, tc := range []struct {
		name, secret, auth string
	}{
		{"no auth header", "s3cret", ""},
		{"wrong auth", "s3cret", "deadbeef"},
		{"empty secret never authenticates", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newABFixture(t, abLabels(labelABSplit, "0"), nil)
			f.r.peerHopAuth = peerHopAuthToken(tc.secret)
			hdr := map[string]string{PeerHopHeader: "1", abVariantHeader: "B"}
			if tc.auth != "" {
				hdr[PeerAuthHeader] = tc.auth
			}
			rec := f.do("GET", "/", []*http.Cookie{{Name: "ab_uid", Value: "u1"}}, hdr)
			if servedBy(rec) != "A" || f.a.gotVariant.Load() != "A" {
				t.Fatalf("forged hop chose its variant: served by %s", servedBy(rec))
			}
			if f.a.gotAuth.Load() != "" {
				t.Fatal("PeerAuthHeader leaked to backend")
			}
			if len(rec.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("unauthenticated hop must not get a pin or anon cookie")
			}
			if c := f.cum(); c[0].samples() != 0 || c[1].samples() != 0 {
				t.Fatal("unauthenticated hop must not be recorded")
			}
		})
	}
}

func TestABPeerAuthStrippedOnUnlabeledRoute(t *testing.T) {
	var got atomic.Value
	got.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get(PeerAuthHeader))
	}))
	defer srv.Close()
	r := &Router{peerHopAuth: peerHopAuthToken("s")}
	r.Set([]*RouteGroup{mkGroup(t, "plain.example.org", "", false, srv.URL)})
	req := httptest.NewRequest("GET", "http://plain.example.org/", nil)
	req.Header.Set(PeerAuthHeader, r.peerHopAuth)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if got.Load() != "" {
		t.Fatal("PeerAuthHeader must be stripped on every route")
	}
}

func TestABPeerBackendSendsHopAuthOnlyWhenStamped(t *testing.T) {
	var gotHop, gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHop.Store(r.Header.Get(PeerHopHeader))
		gotAuth.Store(r.Header.Get(PeerAuthHeader))
	}))
	defer srv.Close()
	tok := peerHopAuthToken("s")
	b := makePeerBackendAuth(srv.URL, "h.example.org", "", false, "p", 1, tok)
	tryProxy(httptest.NewRecorder(), httptest.NewRequest("GET", "http://h.example.org/", nil), b)
	if gotHop.Load() != "1" || gotAuth.Load() != "" {
		t.Fatalf("unstamped hop: hop=%v auth=%v (auth must be absent)", gotHop.Load(), gotAuth.Load())
	}
	req := httptest.NewRequest("GET", "http://h.example.org/", nil)
	req = req.WithContext(context.WithValue(req.Context(), abStampedKey{}, true))
	tryProxy(httptest.NewRecorder(), req, b)
	if gotAuth.Load() != tok {
		t.Fatalf("stamped hop auth = %v", gotAuth.Load())
	}
	b = makePeerBackend(srv.URL, "h.example.org", "", false, "p", 1)
	tryProxy(httptest.NewRecorder(), req, b)
	if gotAuth.Load() != "" {
		t.Fatal("no secret → no auth header")
	}
}

// abReceiver serves an A/B fixture (split 100: an untrusted request with a
// uid lands on B) behind a real listener, as the peer proxy.
func abReceiver(t *testing.T, tok string) (*abFixture, *httptest.Server) {
	t.Helper()
	f := newABFixture(t, abLabels(labelABSplit, "100"), nil)
	f.r.peerHopAuth = tok
	srv := httptest.NewServer(f.r)
	t.Cleanup(srv.Close)
	return f, srv
}

// A route WITHOUT a test passes the client's X-Variant through (no
// regression) but must not vouch for it: the hop carries no auth, so the
// receiver neither trusts nor records the client-chosen variant.
func TestABHopFromUnlabeledRouteNotTrusted(t *testing.T) {
	tok := peerHopAuthToken("s")
	recv, recvSrv := abReceiver(t, tok)
	peer := makePeerBackendAuth(recvSrv.URL, abTestHost, "", false, "p", 1, tok)
	fwd := &Router{peerHopAuth: tok}
	fwd.Set([]*RouteGroup{{Host: abTestHost, Backends: []*Backend{peer}}})
	req := httptest.NewRequest("GET", "http://"+abTestHost+"/", nil)
	req.Header.Set(abVariantHeader, "A")
	req.AddCookie(&http.Cookie{Name: "ab_uid", Value: "u1"})
	rec := httptest.NewRecorder()
	fwd.ServeHTTP(rec, req)
	if servedBy(rec) != "B" || recv.b.gotVariant.Load() != "B" {
		t.Fatalf("client X-Variant A was trusted: served by %s", servedBy(rec))
	}
	if recv.b.gotAuth.Load() != "" {
		t.Fatal("auth header reached a backend")
	}
	if c := recv.cum(); c[0].samples() != 0 || c[1].samples() != 0 {
		t.Fatalf("untrusted hop recorded: A=%d B=%d", c[0].samples(), c[1].samples())
	}
}

// An A/B-path hop (X-Variant stamped by abProxyToGroup) carries auth and
// the receiver trusts and records it.
func TestABHopFromABPathTrusted(t *testing.T) {
	tok := peerHopAuthToken("s")
	recv, recvSrv := abReceiver(t, tok)
	peer := makePeerBackendAuth(recvSrv.URL, abTestHost, "", false, "p", 1, tok)
	peer.Learned = true
	localB := newABSrv(t, abTestHost, abVariantB)
	fwdClk := newABClock()
	fwd := &Router{peerHopAuth: tok, now: fwdClk.now}
	g := &RouteGroup{Host: abTestHost, Service: "fwdsvc", Backends: []*Backend{localB.be, peer},
		abCfg: parseABConfig(abLabels(labelABID, "fwd12345", labelABSplit, "0", labelABStarted, strconv.FormatInt(fwdClk.now().Unix(), 10)), fwdClk.now(), noWarn)}
	fwd.Set([]*RouteGroup{g})
	req := httptest.NewRequest("GET", "http://"+abTestHost+"/", nil)
	req.Header.Set(abVariantHeader, "B")
	req.AddCookie(&http.Cookie{Name: "ab_uid", Value: "u1"})
	rec := httptest.NewRecorder()
	fwd.ServeHTTP(rec, req)
	if servedBy(rec) != "A" || recv.a.gotVariant.Load() != "A" || localB.hits.Load() != 0 {
		t.Fatalf("stamped A hop not trusted: served by %s", servedBy(rec))
	}
	if recv.a.gotAuth.Load() != "" {
		t.Fatal("auth header reached a backend")
	}
	if c := recv.cum(); c[0].Requests != 1 || c[1].samples() != 0 {
		t.Fatalf("trusted hop should be recorded under A: A=%d B=%d", c[0].Requests, c[1].samples())
	}
	g.ab.mu.Lock()
	fwdA := g.ab.cum[0].samples()
	g.ab.mu.Unlock()
	if fwdA != 0 {
		t.Fatal("the forwarding proxy must not record a request a peer served")
	}
}

func TestABStartedSurvivesRestart(t *testing.T) {
	clk := newABClock()
	started := clk.now().Add(-72 * time.Hour).Unix()
	epoch := clk.now().Add(-48 * time.Hour).Unix()
	labels := abLabels(labelABStarted, strconv.FormatInt(started, 10), labelABEpoch, strconv.FormatInt(epoch, 10),
		labelABPhaseAt, strconv.FormatInt(started, 10))
	cfg := parseABConfig(labels, clk.now(), func(k, v string) { t.Errorf("3-day-old %s=%s rejected", k, v) })
	// A fresh Router stands in for a restarted proxy.
	r := &Router{now: clk.now}
	g := &RouteGroup{Host: abTestHost, Service: "svc", Backends: []*Backend{newABSrv(t, abTestHost, "A").be, newABSrv(t, abTestHost, "B").be}, abCfg: cfg}
	r.Set([]*RouteGroup{g})
	if g.abCfg.Started != started || g.abCfg.Epoch != epoch || g.abCfg.PhaseAt != started {
		t.Fatalf("after restart started=%d epoch=%d phase_at=%d", g.abCfg.Started, g.abCfg.Epoch, g.abCfg.PhaseAt)
	}
	if abWindowIndex(g.abCfg, clk.now()) < 0 {
		t.Fatal("a 2-day-old epoch must be past warm-up")
	}
	old := clk.now().Add(-91 * 24 * time.Hour).Unix()
	for _, k := range []string{labelABStarted, labelABEpoch, labelABPhaseAt} {
		var warned bool
		c := parseABConfig(abLabels(k, strconv.FormatInt(old, 10)), clk.now(), func(string, string) { warned = true })
		if !warned || c.Started != 0 || c.Epoch != 0 || c.PhaseAt != 0 {
			t.Errorf("%s older than 90d must be rejected", k)
		}
	}
}

func TestABPrecedencePinBeatsGroupAndUID(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "100", labelABGroups, "staff:B"), nil)
	rec := f.do("GET", "/", []*http.Cookie{f.freshPin("A"), {Name: "ab_group", Value: "staff"}, {Name: "ab_uid", Value: "u"}}, nil)
	if servedBy(rec) != "A" {
		t.Fatalf("valid pin must win, served by %s", servedBy(rec))
	}
	if c := f.cum(); c[0].Requests != 1 {
		t.Fatal("pinned request should be recorded")
	}
}

func TestABPrecedenceQAOverride(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "0", labelABOverride, "true"), nil)
	rec := f.do("GET", "/p?x=1&pm_variant=B&y=%20z", nil, nil)
	if servedBy(rec) != "B" {
		t.Fatalf("override should route to B, got %s", servedBy(rec))
	}
	if q := f.b.gotQuery.Load(); q != "x=1&y=%20z" {
		t.Fatalf("backend query = %q, want only pm_variant stripped byte-for-byte", q)
	}
	p, ok := pinFromRec(t, rec)
	if !ok || p.src != "q" || p.variant != "B" {
		t.Fatalf("override pin = %+v ok=%v", p, ok)
	}
	if c := f.cum(); c[1].samples() != 0 {
		t.Fatal("override request must not be recorded")
	}
	// A src=q pin is routed but never counted.
	rec = f.do("GET", "/", []*http.Cookie{{Name: abPinCookieName("svc"), Value: p.String()}}, nil)
	if servedBy(rec) != "B" || f.cum()[1].Requests != 0 {
		t.Fatal("src=q pin should route to B uncounted")
	}

	off := newABFixture(t, abLabels(labelABSplit, "0"), nil)
	rec = off.do("GET", "/p?pm_variant=B", nil, nil)
	if servedBy(rec) != "A" || off.a.gotQuery.Load() != "pm_variant=B" {
		t.Fatalf("override off: served by %s, query %q", servedBy(rec), off.a.gotQuery.Load())
	}
}

func TestABHeaderMode(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "50", labelABAssign, "header:X-Tenant"), nil)
	for i := 0; i < 20; i++ {
		v := fmt.Sprintf("tenant-%d", i)
		want := abSplitVariant(f.cfg(), v)
		for j := 0; j < 2; j++ {
			rec := f.do("GET", "/", nil, map[string]string{"X-Tenant": v})
			if servedBy(rec) != want {
				t.Fatalf("header %s: served by %s, want %s", v, servedBy(rec), want)
			}
			if len(rec.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("header mode must not set cookies")
			}
			if rec.Header().Get("Vary") != "X-Tenant" {
				t.Fatalf("Vary = %q", rec.Header().Get("Vary"))
			}
		}
	}
	for i := 0; i < 10; i++ {
		if servedBy(f.do("GET", "/", nil, nil)) != "A" {
			t.Fatal("missing header must go to A")
		}
	}
}

func TestABRandomMode(t *testing.T) {
	for _, tc := range []struct{ split, want string }{{"100", "B"}, {"0", "A"}} {
		f := newABFixture(t, abLabels(labelABSplit, tc.split, labelABAssign, "random"), nil)
		for i := 0; i < 10; i++ {
			rec := f.do("GET", "/", nil, nil)
			if servedBy(rec) != tc.want || len(rec.Header().Values("Set-Cookie")) != 0 || rec.Header().Get("Vary") != "" {
				t.Fatalf("random split=%s: served by %s, headers %v", tc.split, servedBy(rec), rec.Header())
			}
		}
	}
	f := newABFixture(t, abLabels(labelABSplit, "50", labelABAssign, "random"), nil)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		seen[servedBy(f.do("GET", "/", nil, nil))] = true
	}
	if !seen["A"] || !seen["B"] {
		t.Fatal("random 50% should hit both variants")
	}
}

func TestABGroupsAndUID(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "0", labelABGroups, "staff:B,ctl:A"), nil)
	rec := f.do("GET", "/", []*http.Cookie{{Name: "ab_group", Value: "staff"}}, nil)
	if p, _ := pinFromRec(t, rec); servedBy(rec) != "B" || p.src != "g" {
		t.Fatalf("mapped group: served by %s pin %+v", servedBy(rec), p)
	}
	// Unmapped valid group = ungrouped → uid split (0% → A).
	rec = f.do("GET", "/", []*http.Cookie{{Name: "ab_group", Value: "nobody"}, {Name: "ab_uid", Value: "u1"}}, nil)
	if p, _ := pinFromRec(t, rec); servedBy(rec) != "A" || p.src != "u" {
		t.Fatalf("unmapped group: served by %s pin %+v", servedBy(rec), p)
	}
	// Invalid group/uid values count as absent → anon.
	rec = f.do("GET", "/", []*http.Cookie{{Name: "ab_group", Value: "STAFF"}, {Name: "ab_uid", Value: "has space!"}}, nil)
	if p, _ := pinFromRec(t, rec); p.src != "n" {
		t.Fatalf("invalid values should be absent, pin %+v", p)
	}
	all := newABFixture(t, abLabels(labelABSplit, "100", labelABGroups, "ctl:A"), nil)
	if servedBy(all.do("GET", "/", []*http.Cookie{{Name: "ab_group", Value: "ctl"}}, nil)) != "A" {
		t.Fatal("group mapped to A must override the split")
	}
	if servedBy(all.do("GET", "/", []*http.Cookie{{Name: "ab_uid", Value: "x"}}, nil)) != "B" {
		t.Fatal("uid at 100% split must go to B")
	}
	custom := newABFixture(t, abLabels(labelABSplit, "100", labelABUIDCookie, "my_uid"), nil)
	if p, _ := pinFromRec(t, custom.do("GET", "/", []*http.Cookie{{Name: "my_uid", Value: "x"}}, nil)); p.src != "u" {
		t.Fatal("uid_cookie label should rename the uid cookie")
	}
}

func TestABAnonCookie(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "50"), nil)
	rec := f.do("GET", "/", nil, nil)
	anons := setCookieNamed(rec, abAnonCookie)
	if len(anons) != 1 {
		t.Fatalf("first visit should set ab_anon once, got %v", anons)
	}
	var anon *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == abAnonCookie {
			anon = c
		}
	}
	if !abAnonRe.MatchString(anon.Value) || anon.MaxAge != 30*24*3600 || !anon.HttpOnly || !anon.Secure || anon.SameSite != http.SameSiteLaxMode || anon.Path != "/" {
		t.Fatalf("anon cookie attributes = %+v", anon)
	}
	want := abSplitVariant(f.cfg(), anon.Value)
	if servedBy(rec) != want {
		t.Fatalf("anon split: served by %s, want %s", servedBy(rec), want)
	}
	rec = f.do("GET", "/", []*http.Cookie{anon}, nil)
	if len(setCookieNamed(rec, abAnonCookie)) != 0 {
		t.Fatal("ab_anon must be set only once")
	}
	if servedBy(rec) != want {
		t.Fatal("same anon id must land in the same variant")
	}

	off := newABFixture(t, abLabels(labelABAnon, "false"), nil)
	rec = off.do("GET", "/", nil, nil)
	if len(setCookieNamed(rec, abAnonCookie)) != 0 {
		t.Fatal("anon=false must not set ab_anon")
	}
	if _, ok := pinFromRec(t, rec); !ok {
		t.Fatal("anon=false still pins the session")
	}
}

// ---- pins ----

func TestABPinValidity(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "0"), nil)
	now := f.clk.now()
	name := abPinCookieName("svc")
	bad := map[string]string{
		"wrong id":      abPin{id: "otherid1", variant: "B", src: "u", nonce: "0badc0de", issued: now.Unix(), lastSeen: now.Unix()}.String(),
		"idle":          f.pin("B", "u", now.Add(-time.Hour), now.Add(-31*time.Minute)).Value,
		"max_session":   f.pin("B", "u", now.Add(-25*time.Hour), now).Value,
		"future":        f.pin("B", "u", now, now.Add(3*time.Minute)).Value,
		"issued>last":   f.pin("B", "u", now, now.Add(-time.Minute)).Value,
		"malformed":     abTestID + ".B.u.0badc0de." + strconv.FormatInt(now.Unix(), 10),
		"bad variant":   abTestID + ".C.u.0badc0de.1.1",
		"bad src":       abTestID + ".B.x.0badc0de.1.1",
		"uppercase hex": abTestID + ".B.u.0BADC0DE." + strconv.FormatInt(now.Unix(), 10) + "." + strconv.FormatInt(now.Unix(), 10),
		"oversized":     abTestID + ".B.u.0badc0de." + strings.Repeat("1", 60) + ".1",
	}
	for desc, v := range bad {
		rec := f.do("GET", "/", []*http.Cookie{{Name: name, Value: v}}, nil)
		if servedBy(rec) != "A" {
			t.Errorf("%s: invalid pin honored (served by %s)", desc, servedBy(rec))
		}
		if p, ok := pinFromRec(t, rec); !ok || p.variant != "A" || p.nonce == "0badc0de" {
			t.Errorf("%s: expected a fresh A pin, got %+v ok=%v", desc, p, ok)
		}
	}
	// Within skew is fine.
	if servedBy(f.do("GET", "/", []*http.Cookie{f.pin("B", "u", now, now.Add(time.Minute))}, nil)) != "B" {
		t.Fatal("lastSeen within 2m skew should be valid")
	}
}

func TestABPinRefreshCadence(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "100"), nil)
	rec := f.do("GET", "/", []*http.Cookie{{Name: "ab_uid", Value: "u"}}, nil)
	p, ok := pinFromRec(t, rec)
	if !ok || p.variant != "B" {
		t.Fatalf("first visit pin %+v", p)
	}
	cookie := &http.Cookie{Name: abPinCookieName("svc"), Value: p.String()}
	f.clk.advance(4 * time.Minute)
	if rec := f.do("GET", "/", []*http.Cookie{cookie}, nil); len(setCookieNamed(rec, cookie.Name)) != 0 {
		t.Fatal("pin refreshed before pin_refresh elapsed")
	}
	f.clk.advance(time.Minute)
	if rec := f.do("GET", "/_next/static/a.js", []*http.Cookie{cookie}, nil); len(rec.Header().Values("Set-Cookie")) != 0 {
		t.Fatal("static requests must never refresh the pin")
	}
	rec = f.do("GET", "/", []*http.Cookie{cookie}, nil)
	np, ok := pinFromRec(t, rec)
	if !ok || np.lastSeen != f.clk.now().Unix() || np.issued != p.issued || np.nonce != p.nonce || np.variant != "B" {
		t.Fatalf("refreshed pin = %+v (orig %+v)", np, p)
	}
	f.r.peerHopAuth = peerHopAuthToken("s")
	f.clk.advance(10 * time.Minute)
	rec = f.do("GET", "/", []*http.Cookie{{Name: cookie.Name, Value: np.String()}}, map[string]string{PeerHopHeader: "1"})
	if len(rec.Header().Values("Set-Cookie")) != 0 {
		t.Fatal("hopped requests must never refresh the pin")
	}
}

func TestABCookieAttributesAndJSMirror(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "100", labelABCookieJS, "true"), nil)
	rec := f.do("GET", "/", []*http.Cookie{{Name: "ab_uid", Value: "u"}}, nil)
	var pin, js *http.Cookie
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case abPinCookieName("svc"):
			pin = c
		case abJSCookieName("svc"):
			js = c
		}
	}
	if pin == nil || !pin.HttpOnly || !pin.Secure || pin.SameSite != http.SameSiteLaxMode || pin.Path != "/" || pin.MaxAge != 0 || !pin.Expires.IsZero() {
		t.Fatalf("pin cookie attributes = %+v", pin)
	}
	if len(pin.Value) > abPinMaxLen {
		t.Fatal("pin exceeds 96 bytes")
	}
	if !strings.HasPrefix(pin.Name, "ab_v_") || len(pin.Name) != len("ab_v_")+8 {
		t.Fatalf("pin name %q", pin.Name)
	}
	if js == nil || js.Value != "B" || js.HttpOnly || !js.Secure || js.MaxAge != 0 {
		t.Fatalf("js mirror = %+v", js)
	}
	off := newABFixture(t, abLabels(), nil)
	if len(setCookieNamed(off.do("GET", "/", nil, nil), abJSCookieName("svc"))) != 0 {
		t.Fatal("js mirror must be off by default")
	}
}

func TestABOtherSetCookiePreserved(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "0"), func(g *RouteGroup) { g.Sticky = true })
	f.a.setAppCookie = true
	rec := f.do("GET", "/", nil, nil)
	var names []string
	for _, c := range rec.Result().Cookies() {
		names = append(names, c.Name)
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"app_session", abPinCookieName("svc"), abAnonCookie, stickyCookieName(f.g)} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Set-Cookie %q missing from %v", want, names)
		}
	}
	if len(setCookieNamed(rec, abPinCookieName("svc"))) != 1 {
		t.Fatal("exactly one pin cookie expected")
	}
}

// ---- phases (§1.10) ----

func TestABPhaseTable(t *testing.T) {
	for _, tc := range []struct {
		phase, newVariant string
	}{
		{abPhaseRunning, "B"}, {abPhaseAborted, "A"}, {abPhaseDiscarding, "A"}, {abPhasePromoting, "B"},
	} {
		split := "100"
		if tc.phase == abPhasePromoting {
			split = "0"
		}
		f := newABFixture(t, abLabels(labelABSplit, split, labelABPhase, tc.phase), nil)
		if got := servedBy(f.do("GET", "/", []*http.Cookie{{Name: "ab_uid", Value: "u"}}, nil)); got != tc.newVariant {
			t.Errorf("%s: new session → %s, want %s", tc.phase, got, tc.newVariant)
		}
		for _, v := range []string{"A", "B"} {
			if got := servedBy(f.do("GET", "/", []*http.Cookie{f.freshPin(v)}, nil)); got != v {
				t.Errorf("%s: %s pin served by %s", tc.phase, v, got)
			}
		}
	}
	for _, mode := range []string{"random", "header:X-T"} {
		f := newABFixture(t, abLabels(labelABSplit, "100", labelABAssign, mode, labelABPhase, abPhaseAborted), nil)
		if servedBy(f.do("GET", "/", nil, map[string]string{"X-T": "x"})) != "A" {
			t.Errorf("%s aborted should go to A", mode)
		}
		f = newABFixture(t, abLabels(labelABSplit, "0", labelABAssign, mode, labelABPhase, abPhasePromoting), nil)
		if servedBy(f.do("GET", "/", nil, map[string]string{"X-T": "x"})) != "B" {
			t.Errorf("%s promoting should go to B", mode)
		}
	}
}

func TestABFailover(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "100"), nil)
	f.b.be.DockerUnhealthy = true
	pin := f.freshPin("B")
	rec := f.do("GET", "/", []*http.Cookie{pin}, map[string]string{abVariantHeader: "B"})
	if servedBy(rec) != "A" || f.a.gotVariant.Load() != "A" {
		t.Fatalf("B down should fail over to A with X-Variant A, served by %s got %v", servedBy(rec), f.a.gotVariant.Load())
	}
	if len(setCookieNamed(rec, pin.Name)) != 0 {
		t.Fatal("failover must keep (not rewrite) the pin")
	}
	c := f.cum()
	if c[1].Failover != 1 || c[0].Requests != 0 || c[1].errRate() != 1 {
		t.Fatalf("failover accounting: A=%+v B=%+v", c[0], c[1])
	}
	if rep := f.g.ab.report(); rep.Failover.B != 1 {
		t.Fatalf("report failover = %+v", rep.Failover)
	}
	// Pin kept: once B recovers the session returns to B.
	f.b.be.DockerUnhealthy = false
	if servedBy(f.do("GET", "/", []*http.Cookie{pin}, nil)) != "B" {
		t.Fatal("session should return to B after recovery")
	}

	// Running: A never fails over to B, even in panic mode.
	g := newABFixture(t, abLabels(labelABSplit, "0"), nil)
	g.a.be.DockerUnhealthy = true
	if rec := g.do("GET", "/", []*http.Cookie{g.freshPin("A")}, nil); rec.Code != 503 || g.b.hits.Load() != 0 {
		t.Fatalf("A pinned with A down must 503, got %d (B hits %d)", rec.Code, g.b.hits.Load())
	}
	g.a.be.DockerUnhealthy = false
	g.a.be.markHealthy(false)
	if servedBy(g.do("GET", "/", []*http.Cookie{g.freshPin("A")}, nil)) != "A" || g.b.hits.Load() != 0 {
		t.Fatal("panic mode must stay within A")
	}

	// Promoting: A-pinned with no healthy A → B.
	p := newABFixture(t, abLabels(labelABPhase, abPhasePromoting), nil)
	p.a.be.DockerUnhealthy = true
	if servedBy(p.do("GET", "/", []*http.Cookie{p.freshPin("A")}, nil)) != "B" {
		t.Fatal("promoting: A down should fail over to B")
	}
}

func TestABTransportFailureRecorded(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "0"), nil)
	f.a.srv.Close()
	rec := f.do("GET", "/", []*http.Cookie{f.freshPin("A")}, nil)
	if rec.Code != 503 {
		t.Fatalf("code = %d", rec.Code)
	}
	if c := f.cum(); c[0].Transport != 1 {
		t.Fatalf("transport not recorded: %+v", c[0])
	}
}

func TestABFreezeOnAbortAndDraining(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "100", labelABWarmup, "0s"), nil)
	f.do("GET", "/", []*http.Cookie{f.freshPin("B")}, nil)
	f.g.ab.latched.Store(true)
	for i := 0; i < 3; i++ {
		f.do("GET", "/", []*http.Cookie{f.freshPin("B")}, nil)
	}
	f.do("GET", "/", []*http.Cookie{f.freshPin("A")}, nil)
	if servedBy(f.do("GET", "/", []*http.Cookie{{Name: "ab_uid", Value: "new"}}, nil)) != "A" {
		t.Fatal("latched: new sessions must go to A")
	}
	c := f.cum()
	if c[1].Requests != 1 || c[0].Requests != 0 {
		t.Fatalf("stats must freeze on abort: A=%d B=%d", c[0].Requests, c[1].Requests)
	}
	rep := f.g.ab.report()
	if rep.Draining.B != 3 || rep.Draining.A != 1 || !rep.Frozen || rep.Phase != abPhaseAborted {
		t.Fatalf("draining=%+v frozen=%v phase=%s", rep.Draining, rep.Frozen, rep.Phase)
	}
}

func TestABPinnedTracking(t *testing.T) {
	f := newABFixture(t, abLabels(), nil)
	now := f.clk.now()
	for i := 0; i < 3; i++ {
		c := f.freshPin("B")
		c.Value = strings.Replace(c.Value, "0badc0de", fmt.Sprintf("0000000%d", i), 1)
		f.do("GET", "/", []*http.Cookie{c}, nil)
	}
	f.do("GET", "/", []*http.Cookie{f.freshPin("B")}, nil)
	f.do("GET", "/", []*http.Cookie{f.freshPin("B")}, nil) // same nonce again
	rep := f.g.ab.report()
	if rep.Pinned.B.ActiveSessions != 4 || rep.Pinned.B.LastSeen != now.Unix() || rep.Pinned.A.ActiveSessions != 0 || rep.Pinned.A.LastSeen != 0 {
		t.Fatalf("pinned = %+v", rep.Pinned)
	}
	f.clk.advance(31 * time.Minute)
	f.r.abTick(f.clk.now())
	if rep := f.g.ab.report(); rep.Pinned.B.ActiveSessions != 0 || rep.Pinned.B.LastSeen != now.Unix() {
		t.Fatalf("after prune pinned = %+v", rep.Pinned)
	}
	run := f.g.ab
	run.mu.Lock()
	for i := 0; i < abPinnedCap; i++ {
		run.pinnedNonces[0][strconv.Itoa(i)] = now.Unix()
	}
	run.mu.Unlock()
	run.notePinned(0, "brandnew", f.clk.now(), false)
	run.notePinned(0, "0", f.clk.now(), false)
	run.mu.Lock()
	n := len(run.pinnedNonces[0])
	_, hasNew := run.pinnedNonces[0]["brandnew"]
	updated := run.pinnedNonces[0]["0"] == f.clk.now().Unix()
	run.mu.Unlock()
	if n != abPinnedCap || hasNew || !updated {
		t.Fatalf("cap: n=%d hasNew=%v updated=%v", n, hasNew, updated)
	}
}

// ---- static retry (§1.7) ----

func TestABStatic404Retry(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "100"), nil)
	f.b.staticCode.Store(404)
	rec := f.do("GET", "/_next/static/chunk.js", []*http.Cookie{f.freshPin("B")}, nil)
	if rec.Code != 200 || servedBy(rec) != "A" || rec.Body.String() != "body-A" {
		t.Fatalf("static retry: code=%d served=%s body=%q", rec.Code, servedBy(rec), rec.Body.String())
	}
	if rec.Header().Get("X-Static-From") != "A" || len(rec.Header().Values("X-Served-By")) != 1 {
		t.Fatalf("404 attempt's headers leaked: %v", rec.Header())
	}
	if f.a.gotVariant.Load() != "A" {
		t.Fatal("retry should carry X-Variant of the retry target")
	}
	if rec.Header().Get("Vary") != "" || len(rec.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("static responses get no Vary/cookies: %v", rec.Header())
	}
	rep := f.g.ab.report()
	if rep.StaticFallbacks != 1 || rep.Cumulative.A.Requests != 0 || rep.Cumulative.B.Requests != 0 {
		t.Fatalf("static retry must not be counted: %+v", rep)
	}
	// POST is never retried.
	rec = f.do("POST", "/_next/static/chunk.js", []*http.Cookie{f.freshPin("B")}, nil)
	if rec.Code != 404 || servedBy(rec) != "B" {
		t.Fatalf("POST: code=%d served=%s", rec.Code, servedBy(rec))
	}
	// Non-404 passes through unchanged.
	f.b.staticCode.Store(0)
	if rec := f.do("GET", "/_next/static/chunk.js", []*http.Cookie{f.freshPin("B")}, nil); servedBy(rec) != "B" || rec.Body.String() != "body-B" {
		t.Fatal("200 static should come straight from B")
	}
	// Both 404 → client gets a 404.
	f.b.staticCode.Store(404)
	f.a.staticCode.Store(404)
	if rec := f.do("GET", "/_next/static/gone.js", []*http.Cookie{f.freshPin("B")}, nil); rec.Code != 404 {
		t.Fatalf("both 404 → code %d", rec.Code)
	}
	// static=none disables the retry.
	n := newABFixture(t, abLabels(labelABSplit, "100", labelABStatic, "none"), nil)
	n.b.staticCode.Store(404)
	if rec := n.do("GET", "/_next/static/chunk.js", []*http.Cookie{n.freshPin("B")}, nil); rec.Code != 404 || n.a.hits.Load() != 0 {
		t.Fatal("static=none must not retry")
	}
}

func TestABStaticRetryWithCache(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "100"), func(g *RouteGroup) { g.CacheTTL = time.Minute })
	f.b.staticCode.Store(404)
	rec := f.do("GET", "/_next/static/c.js", nil, nil)
	if rec.Code != 200 || rec.Header().Get("X-Cache") != "MISS" || servedBy(rec) != "A" {
		t.Fatalf("first: code=%d cache=%q served=%s", rec.Code, rec.Header().Get("X-Cache"), servedBy(rec))
	}
	rec = f.do("GET", "/_next/static/c.js", nil, nil)
	if rec.Header().Get("X-Cache") != "HIT" || rec.Body.String() != "body-A" {
		t.Fatalf("static should stay cacheable: %q %q", rec.Header().Get("X-Cache"), rec.Body.String())
	}
}

// ---- headers, sticky, Vary, cache ----

func TestABXVariantStripAndSet(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "0"), nil)
	f.do("GET", "/", nil, map[string]string{abVariantHeader: "B"})
	if f.a.gotVariant.Load() != "A" || f.b.hits.Load() != 0 {
		t.Fatalf("client X-Variant must be replaced, backend saw %v", f.a.gotVariant.Load())
	}
	f.do("GET", "/", nil, map[string]string{abVariantHeader: "<script>"})
	if f.a.gotVariant.Load() != "A" {
		t.Fatal("X-Variant must only be written from constants")
	}
}

func TestABStickyWithinVariant(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "0"), func(g *RouteGroup) { g.Sticky = true })
	pinToB := &http.Cookie{Name: stickyCookieName(f.g), Value: f.b.be.stickyID}
	rec := f.do("GET", "/", []*http.Cookie{f.freshPin("A"), pinToB}, nil)
	if servedBy(rec) != "A" {
		t.Fatal("a sticky pin to a B backend must not pull an A session to B")
	}
	sc := stickyCookies(rec, f.g)
	if len(sc) != 1 || sc[0].Value != f.a.be.stickyID {
		t.Fatalf("sticky cookie should be re-issued for the A backend: %+v", sc)
	}
	pinToB2 := &http.Cookie{Name: stickyCookieName(f.g), Value: f.b.be.stickyID}
	if servedBy(f.do("GET", "/", []*http.Cookie{f.freshPin("B"), pinToB2}, nil)) != "B" {
		t.Fatal("matching-variant sticky pin should be honored")
	}
}

func TestABVaryRules(t *testing.T) {
	f := newABFixture(t, abLabels(labelABExclude, "/api/cron"), nil)
	if v := f.do("GET", "/page", nil, nil).Header().Values("Vary"); len(v) == 0 || v[0] != "Cookie" {
		t.Fatalf("cookie mode Vary = %v", v)
	}
	if v := f.do("GET", "/_next/static/x.js", nil, nil).Header().Get("Vary"); v != "" {
		t.Fatalf("static Vary = %q", v)
	}
	if v := f.do("GET", "/api/cron", nil, nil).Header().Get("Vary"); v != "" {
		t.Fatalf("excluded Vary = %q", v)
	}
	ab := newABFixture(t, abLabels(labelABPhase, abPhaseAborted), nil)
	if v := ab.do("GET", "/page", nil, nil).Header().Get("Vary"); v != "Cookie" {
		t.Fatalf("aborted test still varies, got %q", v)
	}
}

func TestABCacheRules(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "0"), func(g *RouteGroup) { g.CacheTTL = time.Minute })
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "http://"+abTestHost+"/page", nil) // zero cookies
		rec := httptest.NewRecorder()
		f.r.ServeHTTP(rec, req)
		if rec.Header().Get("X-Cache") != "BYPASS" {
			t.Fatalf("visit %d: non-static on an active test must BYPASS, got %q", i, rec.Header().Get("X-Cache"))
		}
	}
	if f.a.hits.Load() != 2 {
		t.Fatalf("backend hits = %d, want 2", f.a.hits.Load())
	}
	f.do("GET", "/_next/static/s.js", nil, nil)
	if rec := f.do("GET", "/_next/static/s.js", nil, nil); rec.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("static should be cacheable, got %q", rec.Header().Get("X-Cache"))
	}
}

// ---- no regression ----

func TestABNoRegressionUnlabeledRoute(t *testing.T) {
	var gotVariant atomic.Value
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotVariant.Store(r.Header.Get(abVariantHeader))
		w.Header().Set("X-App", "1")
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	g := mkGroup(t, "plain.example.org", "", false, srv.URL)
	g.Service = "svc"
	g.CacheTTL = time.Minute
	g.Sticky = true
	r := &Router{}
	r.Set([]*RouteGroup{g})
	if g.ab != nil || g.abCfg != nil {
		t.Fatal("unlabeled route must carry no test")
	}
	req := httptest.NewRequest("GET", "http://plain.example.org/page?pm_variant=B", nil)
	req.Header.Set(abVariantHeader, "client-value")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if gotVariant.Load() != "client-value" {
		t.Fatalf("client X-Variant must pass untouched, got %v", gotVariant.Load())
	}
	if rec.Header().Get("Vary") != "" {
		t.Fatal("no Vary on unlabeled routes")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name != stickyCookieName(g) {
			t.Fatalf("unexpected cookie %q on unlabeled route", c.Name)
		}
	}
	if rec.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("X-Cache = %q", rec.Header().Get("X-Cache"))
	}
	// Sticky routes never store (unchanged behavior); a non-sticky one HITs.
	g2 := mkGroup(t, "plain2.example.org", "", false, srv.URL)
	g2.CacheTTL = time.Minute
	r.Set([]*RouteGroup{g, g2})
	for i := 0; i < 2; i++ {
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("GET", "http://plain2.example.org/x", nil))
	}
	if rec.Header().Get("X-Cache") != "HIT" || len(rec.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("plain cached route: X-Cache=%q cookies=%v", rec.Header().Get("X-Cache"), rec.Header().Values("Set-Cookie"))
	}
	if rep := r.ABReport(""); len(rep.Experiments) != 0 {
		t.Fatal("/ab must list nothing without labels")
	}
}

// ---- judge (§1.8) ----

func abCounts(req, e5 uint64, latMs float64) abCounters {
	c := abCounters{Requests: req, Err5xx: e5}
	c.Hist[abHistBucket(latMs)] = req
	return c
}

func abWin(idx int64, a, b abCounters) abWindow { return abWindow{Index: idx, V: [2]abCounters{a, b}} }

func judgeCfg() (*abConfig, time.Time) {
	now := time.Unix(1_800_000_000, 0)
	cfg := parseABConfig(abLabels(), now, noWarn)
	cfg.Started = now.Unix()
	cfg.Epoch = now.Unix()
	cfg.Warmup = 0
	cfg.MinRuntime = 0
	cfg.MinSamples = 100
	cfg.Window = time.Minute
	return cfg, now
}

func TestABJudgeRules(t *testing.T) {
	cfg, t0 := judgeCfg()
	good := abCounts(1000, 10, 100) // 1% errors, ~100ms
	cases := []struct {
		name string
		b    abCounters
		bad  bool
		rsn  string
	}{
		{"same as A", abCounts(1000, 10, 100), false, ""},
		{"errors over delta and ratio", abCounts(1000, 50, 100), true, "errors"},
		{"errors over ratio but under delta", abCounts(1000, 25, 100), false, ""},
		{"latency over ratio+slack", abCounts(1000, 10, 400), true, "latency"},
		{"latency within slack", abCounts(1000, 10, 300), false, ""},
	}
	for _, tc := range cases {
		r, _ := abWindowBad(cfg, &good, &tc.b)
		if (r != "") != tc.bad || r != tc.rsn {
			t.Errorf("%s: rule=%q", tc.name, r)
		}
	}
	// Ratio blocks a delta-only regression: A 10%, B 13%.
	a10, b13 := abCounts(1000, 100, 100), abCounts(1000, 130, 100)
	if r, _ := abWindowBad(cfg, &a10, &b13); r != "" {
		t.Fatal("err_ratio should block 13% vs 10%")
	}
	// Transport and failover count as errors.
	bt := abCounts(900, 0, 100)
	bt.Transport, bt.Failover = 50, 50
	if r, _ := abWindowBad(cfg, &good, &bt); r != "errors" {
		t.Fatal("transport+failover should count as B errors")
	}

	bad := abCounts(1000, 100, 100)
	sum := func(ws ...abWindow) abSummary {
		s := abSummary{Windows: ws}
		for _, w := range ws {
			for v := range w.V {
				s.Cumulative[v].add(&w.V[v])
			}
		}
		return s
	}
	run := func(cfg *abConfig, now time.Time, s abSummary) abVerdict {
		_, v := judge(cfg, abJudgeState{}, s, nil, now)
		return v
	}
	at := t0.Add(5 * time.Minute) // windows 0..4 complete
	if v := run(cfg, at, sum(abWin(0, good, bad), abWin(1, good, bad))); !v.Abort || v.Reason != "errors" {
		t.Fatalf("two bad windows should abort: %+v", v)
	}
	if v := run(cfg, at, sum(abWin(0, good, bad), abWin(1, good, good), abWin(2, good, bad))); v.Abort {
		t.Fatal("bad, good, bad must not abort")
	}
	thin := abCounts(10, 10, 100)
	if v := run(cfg, at, sum(abWin(0, good, bad), abWin(1, good, thin), abWin(2, good, bad))); !v.Abort {
		t.Fatal("an inconclusive window must leave the streak unchanged")
	}
	// The current (incomplete) window is never judged.
	if v := run(cfg, t0.Add(90*time.Second), sum(abWin(0, good, bad), abWin(1, good, bad))); v.Abort {
		t.Fatal("window 1 is still open and must not be judged")
	}
	// Min samples.
	c2 := *cfg
	c2.MinSamples = 5000
	if v := run(&c2, at, sum(abWin(0, good, bad), abWin(1, good, bad))); v.Abort || v.Status != "insufficient_samples" {
		t.Fatalf("min_samples: %+v", v)
	}
	// Min runtime.
	c3 := *cfg
	c3.MinRuntime = time.Hour
	if v := run(&c3, at, sum(abWin(0, good, bad), abWin(1, good, bad))); v.Abort || v.Status != "min_runtime" {
		t.Fatalf("min_runtime: %+v", v)
	}
	// Warm-up.
	c4 := *cfg
	c4.Warmup = time.Hour
	if v := run(&c4, at, sum()); v.Status != "warmup" {
		t.Fatalf("warmup: %+v", v)
	}
	// windows=3 needs three.
	c5 := *cfg
	c5.Windows = 3
	if v := run(&c5, at, sum(abWin(0, good, bad), abWin(1, good, bad))); v.Abort {
		t.Fatal("windows=3 must not abort on two")
	}
	// Peers merge into the same windows.
	half := abCounts(30, 0, 100)
	halfBad := abCounts(30, 10, 100)
	own := sum(abWin(0, half, halfBad), abWin(1, half, halfBad))
	c6 := *cfg
	c6.MinSamples = 1
	if v := run(&c6, at, own); v.Abort {
		t.Fatal("30 samples per window is below window_min_samples alone")
	}
	if _, v := judge(&c6, abJudgeState{}, own, []abSummary{own}, at); !v.Abort {
		t.Fatal("own+peer reach window_min_samples together and should abort")
	}
	// State carries the streak across calls without re-judging old windows.
	st, _ := judge(cfg, abJudgeState{}, sum(abWin(0, good, bad)), nil, t0.Add(time.Minute+time.Second))
	if st.Streak != 1 || st.LastIdx != 0 {
		t.Fatalf("state after one bad window = %+v", st)
	}
	_, v := judge(cfg, st, sum(abWin(0, good, bad), abWin(1, good, bad)), nil, t0.Add(2*time.Minute+time.Second))
	if !v.Abort {
		t.Fatal("streak should carry across calls")
	}
}

func TestABHistogramPercentiles(t *testing.T) {
	if len(abHistBounds) != 64 || math.Abs(abHistBounds[0]-1.2) > 1e-9 || abHistBounds[63] < 100_000 || abHistBounds[63] > 130_000 {
		t.Fatalf("bounds: first %v last %v", abHistBounds[0], abHistBounds[63])
	}
	var c abCounters
	for i := 1; i <= 1000; i++ {
		c.Hist[abHistBucket(float64(i))]++
	}
	p50, p95 := c.percentile(0.5), c.percentile(0.95)
	if math.Abs(p50-500)/500 > 0.1 || math.Abs(p95-950)/950 > 0.1 {
		t.Fatalf("p50=%v p95=%v", p50, p95)
	}
	if abHistBucket(1e9) != 63 || abHistBucket(0) != 0 {
		t.Fatal("bucket clamping")
	}
}

// ---- evaluator, latch ----

func TestABEvaluatorAutoAbortLatch(t *testing.T) {
	f := newABFixture(t, abLabels(labelABWarmup, "0s", labelABMinRuntime, "0s", labelABMinSamples, "10",
		labelABWindow, "1m", labelABWindowMinSamples, "5"), nil)
	f.b.status.Store(500)
	traffic := func() {
		for i := 0; i < 10; i++ {
			f.do("GET", "/", []*http.Cookie{f.freshPin("A")}, nil)
			f.do("GET", "/", []*http.Cookie{f.freshPin("B")}, nil)
		}
	}
	traffic()
	f.clk.advance(time.Minute)
	f.r.abTick(f.clk.now())
	if f.g.ab.latched.Load() {
		t.Fatal("one bad window must not abort")
	}
	traffic()
	f.clk.advance(time.Minute)
	f.r.abTick(f.clk.now())
	if !f.g.ab.latched.Load() {
		t.Fatalf("two bad windows should latch, judge=%+v", f.g.ab.report().Judge)
	}
	rep := f.g.ab.report()
	if rep.Phase != abPhaseAborted || rep.Abort == nil || rep.Abort.Reason != "errors" || rep.Abort.Source != "auto" || rep.Abort.At != f.clk.now().Unix() {
		t.Fatalf("report after latch: phase=%s abort=%+v", rep.Phase, rep.Abort)
	}
	if servedBy(f.do("GET", "/", []*http.Cookie{{Name: "ab_uid", Value: "newcomer"}}, nil)) != "A" {
		t.Fatal("latched: new sessions → A")
	}
	if servedBy(f.do("GET", "/", []*http.Cookie{f.freshPin("B")}, nil)) != "B" {
		t.Fatal("latched: B pins stay on B")
	}

	// No flap: B recovers, good windows, still latched.
	f.b.status.Store(200)
	for i := 0; i < 3; i++ {
		traffic()
		f.clk.advance(time.Minute)
		f.r.abTick(f.clk.now())
	}
	if !f.g.ab.latched.Load() {
		t.Fatal("latch must not flap back")
	}

	// Detach (B gone for one refresh) and re-attach: latch survives.
	run := f.g.ab
	gNoB := &RouteGroup{Host: abTestHost, Service: "svc", Backends: []*Backend{f.a.be}}
	f.r.Set([]*RouteGroup{gNoB})
	if gNoB.ab != nil || len(f.r.ABReport("").Experiments) != 0 {
		t.Fatal("no B backend → no test attached")
	}
	g2 := &RouteGroup{Host: abTestHost, Service: "svc", Backends: []*Backend{f.a.be, f.b.be}, abCfg: parseABConfig(abLabels(labelABStarted, strconv.FormatInt(f.clk.now().Unix(), 10)), f.clk.now(), noWarn)}
	f.r.Set([]*RouteGroup{g2})
	if g2.ab != run || !g2.ab.latched.Load() {
		t.Fatal("same id after a detach must keep the latch")
	}

	// New id clears it.
	g3 := &RouteGroup{Host: abTestHost, Service: "svc", Backends: []*Backend{f.a.be, f.b.be}, abCfg: parseABConfig(abLabels(labelABID, "newid123"), f.clk.now(), noWarn)}
	f.r.Set([]*RouteGroup{g3})
	if g3.ab == run || g3.ab.latched.Load() {
		t.Fatal("a new test id must start unlatched")
	}
}

func TestABAutoAbortOffAndLabelAbort(t *testing.T) {
	f := newABFixture(t, abLabels(labelABAutoAbort, "false", labelABWarmup, "0s", labelABMinRuntime, "0s",
		labelABMinSamples, "1", labelABWindow, "1m", labelABWindowMinSamples, "1"), nil)
	f.b.status.Store(500)
	for w := 0; w < 3; w++ {
		f.do("GET", "/", []*http.Cookie{f.freshPin("A")}, nil)
		f.do("GET", "/", []*http.Cookie{f.freshPin("B")}, nil)
		f.clk.advance(time.Minute)
		f.r.abTick(f.clk.now())
	}
	if f.g.ab.latched.Load() {
		t.Fatal("autoabort=false must never latch")
	}
	phaseAt := time.Now().Add(-time.Hour).Unix()
	l := newABFixture(t, abLabels(labelABPhase, abPhaseAborted, labelABAbortReason, "manual", labelABPhaseAt, strconv.FormatInt(phaseAt, 10)), nil)
	rep := l.g.ab.report()
	if rep.Abort == nil || rep.Abort.Source != "label" || rep.Abort.Reason != "manual" || rep.PhaseAt != phaseAt {
		t.Fatalf("label abort report = %+v / %d", rep.Abort, rep.PhaseAt)
	}
}

func TestABEpochBumpResetsWindows(t *testing.T) {
	f := newABFixture(t, abLabels(labelABWarmup, "0s"), nil)
	f.do("GET", "/", []*http.Cookie{f.freshPin("B")}, nil)
	g2 := &RouteGroup{Host: abTestHost, Service: "svc", Backends: f.g.Backends,
		abCfg: parseABConfig(abLabels(labelABStarted, f.g.abCfg.labels()[labelABStarted], labelABEpoch, strconv.FormatInt(f.clk.now().Unix()+60, 10)), f.clk.now(), noWarn)}
	f.r.Set([]*RouteGroup{g2})
	rep := g2.ab.report()
	if len(rep.Windows) != 0 || rep.Cumulative.B.Requests != 1 || g2.ab != f.g.ab {
		t.Fatalf("epoch bump: windows=%d cum=%d", len(rep.Windows), rep.Cumulative.B.Requests)
	}
	g2.ab.record(1, abServed, 200, time.Millisecond, f.clk.now())
	if rep := g2.ab.report(); rep.Warmup.B.Requests != 1 {
		t.Fatal("traffic before epoch+warmup is warm-up traffic")
	}
}

// ---- GET /ab ----

func TestABHandlerShapeAndReadOnly(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "100", labelABWarmup, "0s"), nil)
	rec := f.do("GET", "/", []*http.Cookie{{Name: "ab_uid", Value: "secret-uid-value"}}, map[string]string{"Authorization": "Bearer tok"})
	p, _ := pinFromRec(t, rec)
	f.do("GET", "/", []*http.Cookie{{Name: abPinCookieName("svc"), Value: p.String()}}, nil)

	h := abHandler(f.r.ABReport)
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/ab?service=svc", nil))
	body := w.Body.String()
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET /ab: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, leak := range []string{"secret-uid-value", "Bearer", p.nonce, p.String()} {
		if strings.Contains(body, leak) {
			t.Fatalf("/ab leaks %q", leak)
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if b, ok := out["hist_bounds_ms"].([]any); !ok || len(b) != 64 {
		t.Fatalf("hist_bounds_ms = %v", out["hist_bounds_ms"])
	}
	exps := out["experiments"].([]any)
	if len(exps) != 1 {
		t.Fatalf("experiments = %d", len(exps))
	}
	e := exps[0].(map[string]any)
	for _, k := range []string{"service", "id", "phase", "phase_at", "config", "started", "epoch", "b_backends_local", "b_backends_peer",
		"cumulative", "windows", "frozen", "warmup", "pinned", "failover", "draining", "hop_errors", "static_fallbacks", "peers"} {
		if _, ok := e[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if _, ok := e["abort"]; ok {
		t.Error("abort must be omitted while running")
	}
	if e["b_backends_local"].(float64) != 1 {
		t.Errorf("b_backends_local = %v", e["b_backends_local"])
	}
	pinned := e["pinned"].(map[string]any)["B"].(map[string]any)
	if pinned["active_sessions"].(float64) != 1 || pinned["last_seen"].(float64) == 0 {
		t.Errorf("pinned B = %v", pinned)
	}
	cum := e["cumulative"].(map[string]any)["B"].(map[string]any)
	if cum["requests"].(float64) != 2 || len(cum["hist"].([]any)) != 64 {
		t.Errorf("cumulative B = %v", cum)
	}

	w = httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/ab?service=other", nil))
	if !strings.Contains(w.Body.String(), `"experiments":[]`) {
		t.Fatalf("service filter: %s", w.Body.String())
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		w = httptest.NewRecorder()
		h(w, httptest.NewRequest(m, "/ab", strings.NewReader("{}")))
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") == "" {
			t.Fatalf("%s /ab: %d allow=%q", m, w.Code, w.Header().Get("Allow"))
		}
	}
}

// ---- concurrency ----

func TestABConcurrentTrafficWithEvaluator(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "50", labelABWarmup, "0s", labelABMinRuntime, "0s",
		labelABMinSamples, "10", labelABWindow, "1m", labelABWindowMinSamples, "5", labelABExclude, "/api/cron"),
		func(g *RouteGroup) { g.Sticky = true; g.CacheTTL = time.Second })
	f.b.status.Store(500)
	stop := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(1)
	go func() {
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			f.clk.advance(5 * time.Second)
			f.r.abTick(f.clk.now())
			_ = f.r.ABReport("")
			f.r.Set([]*RouteGroup{{Host: abTestHost, Service: "svc", Backends: []*Backend{f.a.be, f.b.be}, Sticky: true, CacheTTL: time.Second,
				abCfg: parseABConfig(abLabels(labelABSplit, "50", labelABWarmup, "0s", labelABMinRuntime, "0s", labelABMinSamples, "10",
					labelABWindow, "1m", labelABWindowMinSamples, "5", labelABStarted, f.g.abCfg.labels()[labelABStarted]), f.clk.now(), noWarn)}})
			time.Sleep(time.Millisecond)
		}
	}()
	paths := []string{"/", "/page", "/_next/static/a.js", "/api/cron", "/x?pm_variant=B"}
	var wg sync.WaitGroup
	for gi := 0; gi < 50; gi++ {
		wg.Add(1)
		go func(gi int) {
			defer wg.Done()
			var jar []*http.Cookie
			for i := 0; i < 200; i++ {
				req := httptest.NewRequest("GET", "http://"+abTestHost+paths[(gi+i)%len(paths)], nil)
				if gi%3 == 0 {
					req.AddCookie(&http.Cookie{Name: "ab_uid", Value: "u" + strconv.Itoa(gi)})
				}
				for _, c := range jar {
					req.AddCookie(c)
				}
				if gi%7 == 0 {
					req.Header.Set(PeerHopHeader, "1")
				}
				rec := httptest.NewRecorder()
				f.r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)
				if rec.Code == 0 {
					t.Errorf("no response")
				}
				if cs := rec.Result().Cookies(); len(cs) > 0 {
					jar = cs
				}
			}
		}(gi)
	}
	wg.Wait()
	close(stop)
	bg.Wait()
	rep := f.r.ABReport("svc")
	if len(rep.Experiments) != 1 {
		t.Fatal("experiment should still be attached")
	}
}

func TestABCutQueryOverride(t *testing.T) {
	for _, tc := range []struct{ in, val, rest string }{
		{"pm_variant=B", "B", ""},
		{"a=1&pm_variant=A&b=%2F", "A", "a=1&b=%2F"},
		{"pm_variant=B&pm_variant=A", "B", ""},
		{"a=1&pm_variantx=B", "", "a=1&pm_variantx=B"},
	} {
		val, rest, _ := cutQueryOverride(tc.in)
		if val != tc.val || rest != tc.rest {
			t.Errorf("%q → %q %q", tc.in, val, rest)
		}
	}

}

func TestABUnhealthyBStaysAttached(t *testing.T) {
	var aHits, bHits atomic.Int32
	var aVariant atomic.Value
	aSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits.Add(1)
		aVariant.Store(r.Header.Get(abVariantHeader))
	}))
	defer aSrv.Close()
	bSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bHits.Add(1) }))
	defer bSrv.Close()
	ip := func(s *httptest.Server) (string, string) {
		u, _ := url.Parse(s.URL)
		return u.Hostname(), u.Port()
	}
	aIP, aPort := ip(aSrv)
	bIP, bPort := ip(bSrv)
	now := time.Now()
	labels := func(port string, kv ...string) map[string]string {
		m := map[string]string{labelHost: "unh.example.org", labelPort: port, labelService: "unhsvc"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	dc := fakeDocker(t, dockerJSON(
		container("1", "unh-a", "running", labels(aPort), map[string]string{managedNetwork: aIP}),
		containerWithStatus("2", "unh-b", "running", "Up 1 minute (unhealthy)",
			labels(bPort, labelCanary, "true", labelABVariant, "B", labelABID, "unh12345", labelABWarmup, "0s",
				labelABStarted, strconv.FormatInt(now.Unix(), 10)),
			map[string]string{managedNetwork: bIP}),
	))
	groups, _, err := assembleGroups(context.Background(), dc, "")
	if err != nil {
		t.Fatal(err)
	}
	g := findGroup(groups, "unh.example.org", "")
	if g == nil || g.abCfg == nil {
		t.Fatal("a Docker-unhealthy B must keep the test attached")
	}
	r := &Router{}
	r.Set(groups)
	pin := abPin{id: "unh12345", variant: "B", src: "u", nonce: "0badc0de", issued: now.Unix(), lastSeen: now.Unix()}
	req := httptest.NewRequest("GET", "http://unh.example.org/", nil)
	req.AddCookie(&http.Cookie{Name: abPinCookieName("unhsvc"), Value: pin.String()})
	req.Header.Set(abVariantHeader, "B")
	r.ServeHTTP(httptest.NewRecorder(), req)
	if aHits.Load() != 1 || bHits.Load() != 0 || aVariant.Load() != "A" {
		t.Fatalf("B pin with unhealthy B: aHits=%d bHits=%d X-Variant=%v", aHits.Load(), bHits.Load(), aVariant.Load())
	}
	g.ab.mu.Lock()
	fo := g.ab.cum[1].Failover
	g.ab.mu.Unlock()
	if fo != 1 {
		t.Fatalf("B failover = %d, want 1", fo)
	}
}

func TestABJudgeKeepsReasonAcrossCalls(t *testing.T) {
	cfg, t0 := judgeCfg()
	cfg.MinRuntime = 15 * time.Minute
	good, slow := abCounts(1000, 10, 100), abCounts(1000, 10, 900)
	s := abSummary{Windows: []abWindow{abWin(0, good, slow), abWin(1, good, slow)}}
	for _, w := range s.Windows {
		for v := range w.V {
			s.Cumulative[v].add(&w.V[v])
		}
	}
	st, v := judge(cfg, abJudgeState{}, s, nil, t0.Add(2*time.Minute+time.Second))
	if v.Abort || v.Status != "min_runtime" || st.Streak != 2 {
		t.Fatalf("before min_runtime: %+v %+v", st, v)
	}
	_, v = judge(cfg, st, s, nil, t0.Add(15*time.Minute))
	if !v.Abort || v.Reason != "latency" || !strings.Contains(v.Detail, "p95") {
		t.Fatalf("later call should abort with the latency reason: %+v", v)
	}
}

func TestABStartedFallbackStable(t *testing.T) {
	f := newABFixture(t, abLabels(), nil)
	run := f.g.ab
	f.r.abTick(f.clk.now())
	f.do("GET", "/", []*http.Cookie{f.freshPin("B")}, nil)
	started := f.g.abCfg.Started
	f.clk.advance(91 * 24 * time.Hour)
	// Same labels 91 days later: started now fails the 90d bound.
	g2 := &RouteGroup{Host: abTestHost, Service: "svc", Backends: f.g.Backends,
		abCfg: parseABConfig(abLabels(labelABStarted, strconv.FormatInt(started, 10)), f.clk.now(), noWarn)}
	if g2.abCfg.Started != 0 {
		t.Fatal("precondition: started should be out of bounds now")
	}
	f.r.Set([]*RouteGroup{g2})
	if g2.ab != run || g2.abCfg.Started != started || g2.abCfg.Epoch != started {
		t.Fatalf("started/epoch moved: started=%d epoch=%d want %d", g2.abCfg.Started, g2.abCfg.Epoch, started)
	}
	if rep := run.report(); len(rep.Windows) != 1 {
		t.Fatalf("window ring reset by the fallback: %d windows", len(rep.Windows))
	}
}

func TestABNoRegressionMixedRouter(t *testing.T) {
	f := newABFixture(t, abLabels(labelABSplit, "100", labelABOverride, "true"), nil)
	var gotVariant, gotQuery atomic.Value
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVariant.Store(r.Header.Get(abVariantHeader))
		gotQuery.Store(r.URL.RawQuery)
	}))
	defer plain.Close()
	// Same service, other path, no B backend → no test.
	other := mkGroup(t, abTestHost, "/plain", false, plain.URL)
	other.Service = "svc"
	f.g.abCfg = parseABConfig(abLabels(labelABSplit, "100", labelABOverride, "true", labelABStarted, strconv.FormatInt(f.clk.now().Unix(), 10)), f.clk.now(), noWarn)
	f.r.Set([]*RouteGroup{f.g, other})
	if other.ab != nil {
		t.Fatal("group without a B backend must not carry the test")
	}
	rec := f.do("GET", "/plain/x?pm_variant=B&a=1", []*http.Cookie{{Name: "ab_uid", Value: "u"}}, map[string]string{abVariantHeader: "client"})
	if gotVariant.Load() != "client" || gotQuery.Load() != "pm_variant=B&a=1" {
		t.Fatalf("unlabeled group touched: X-Variant=%v query=%v", gotVariant.Load(), gotQuery.Load())
	}
	if rec.Header().Get("Vary") != "" || len(rec.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("unlabeled group got A/B headers: %v", rec.Header())
	}
	if servedBy(f.do("GET", "/", []*http.Cookie{{Name: "ab_uid", Value: "u"}}, nil)) != "B" {
		t.Fatal("the test group itself still runs")
	}
}
