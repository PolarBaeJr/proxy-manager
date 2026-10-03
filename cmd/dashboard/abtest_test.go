package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func abIntP(n int) *int             { return &n }
func abFloatP(f float64) *float64   { return &f }
func abBoolP(b bool) *bool          { return &b }
func abBaseReq() abStartRequest     { return abStartRequest{Image: "ghcr.io/org/app:v2", Replicas: 1} }
func abStatus(err error) (code int) { return abErrStatus(err) }

func TestDecodeABStartRefusesEnvAndUnknownFields(t *testing.T) {
	for _, body := range []string{
		`{"image":"x:1","replicas":1,"env":{"A":"1"}}`,
		`{"image":"x:1","replicas":1,"env":null}`,
		`{"image":"x:1","replicas":1,"env":{}}`,
	} {
		_, err := decodeABStart([]byte(body))
		if err == nil || abStatus(err) != 400 || !strings.Contains(err.Error(), "env") {
			t.Fatalf("%s: err = %v, want a 400 naming env", body, err)
		}
	}
	if _, err := decodeABStart([]byte(`{"image":"x:1","replicas":1,"bogus":1}`)); err == nil || abStatus(err) != 400 {
		t.Fatalf("unknown field: err = %v, want 400", err)
	}
	if _, err := decodeABStart([]byte(`not json`)); err == nil || abStatus(err) != 400 {
		t.Fatalf("bad json: err = %v, want 400", err)
	}
	req, err := decodeABStart([]byte(`{"image":"x:1","replicas":2,"split":20,"thresholds":{"err_delta":0.5}}`))
	if err != nil || req.Replicas != 2 || *req.Split != 20 || *req.Thresholds.ErrDelta != 0.5 {
		t.Fatalf("valid body = %+v, %v", req, err)
	}
}

// TestABConfigLabelsBounds pins every bound to cmd/proxy/abtest.go's
// parseABConfig (and §3's replicas/env rules): each case is one edge, just
// inside or just outside.
func TestABConfigLabelsBounds(t *testing.T) {
	groups := func(n int) map[string]string {
		m := map[string]string{}
		for i := 0; i < n; i++ {
			m["g"+strconv.Itoa(i)] = "B"
		}
		return m
	}
	prefixes := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "/p" + strconv.Itoa(i)
		}
		return out
	}
	cases := []struct {
		name string
		mod  func(r *abStartRequest)
		ok   bool
	}{
		{"no image", func(r *abStartRequest) { r.Image = "" }, false},
		{"replicas 0", func(r *abStartRequest) { r.Replicas = 0 }, false},
		{"replicas 1", func(r *abStartRequest) { r.Replicas = 1 }, true},
		{"replicas 10", func(r *abStartRequest) { r.Replicas = 10 }, true},
		{"replicas 11", func(r *abStartRequest) { r.Replicas = 11 }, false},
		{"split -1", func(r *abStartRequest) { r.Split = abIntP(-1) }, false},
		{"split 0", func(r *abStartRequest) { r.Split = abIntP(0) }, true},
		{"split 100", func(r *abStartRequest) { r.Split = abIntP(100) }, true},
		{"split 101", func(r *abStartRequest) { r.Split = abIntP(101) }, false},
		{"assign random", func(r *abStartRequest) { r.Assign = "random" }, true},
		{"assign header:X-Variant", func(r *abStartRequest) { r.Assign = "header:X-Variant" }, true},
		{"assign header + header", func(r *abStartRequest) { r.Assign, r.Header = "header", "X-Variant" }, true},
		{"assign header:Cookie", func(r *abStartRequest) { r.Assign = "header:cookie" }, false},
		{"assign header:Host", func(r *abStartRequest) { r.Assign = "header:host" }, false},
		{"assign header bad token", func(r *abStartRequest) { r.Assign = "header:a b" }, false},
		{"assign header 65 bytes", func(r *abStartRequest) { r.Assign = "header:" + strings.Repeat("x", 65) }, false},
		{"assign bogus", func(r *abStartRequest) { r.Assign = "sticky" }, false},
		{"header without assign", func(r *abStartRequest) { r.Header = "X-Variant" }, false},
		{"groups 32", func(r *abStartRequest) { r.Groups = groups(32) }, true},
		{"groups 33", func(r *abStartRequest) { r.Groups = groups(33) }, false},
		{"group bad name", func(r *abStartRequest) { r.Groups = map[string]string{"Beta": "B"} }, false},
		{"group name 33", func(r *abStartRequest) { r.Groups = map[string]string{strings.Repeat("a", 33): "B"} }, false},
		{"group bad variant", func(r *abStartRequest) { r.Groups = map[string]string{"beta": "C"} }, false},
		{"uid_cookie 64", func(r *abStartRequest) { r.UIDCookie = strings.Repeat("c", 64) }, true},
		{"uid_cookie 65", func(r *abStartRequest) { r.UIDCookie = strings.Repeat("c", 65) }, false},
		{"group_cookie not token", func(r *abStartRequest) { r.GroupCookie = "a;b" }, false},
		{"session_idle 4m59s", func(r *abStartRequest) { r.SessionIdle = "4m59s" }, false},
		{"session_idle 5m", func(r *abStartRequest) { r.SessionIdle = "5m" }, true},
		{"session_idle 24h", func(r *abStartRequest) { r.SessionIdle = "24h" }, true},
		{"session_idle 24h1s", func(r *abStartRequest) { r.SessionIdle = "24h0m1s" }, false},
		{"session_idle garbage", func(r *abStartRequest) { r.SessionIdle = "soon" }, false},
		{"pin_refresh 59s", func(r *abStartRequest) { r.PinRefresh = "59s" }, false},
		{"pin_refresh 1m", func(r *abStartRequest) { r.PinRefresh = "1m" }, true},
		{"pin_refresh idle/2 default", func(r *abStartRequest) { r.PinRefresh = "15m" }, true},
		{"pin_refresh over default idle/2", func(r *abStartRequest) { r.PinRefresh = "15m1s" }, false},
		{"pin_refresh idle/2 custom", func(r *abStartRequest) { r.SessionIdle, r.PinRefresh = "10m", "5m" }, true},
		{"pin_refresh over custom idle/2", func(r *abStartRequest) { r.SessionIdle, r.PinRefresh = "10m", "6m" }, false},
		{"max_session 59m", func(r *abStartRequest) { r.MaxSession = "59m" }, false},
		{"max_session 1h", func(r *abStartRequest) { r.MaxSession = "1h" }, true},
		{"max_session 7d", func(r *abStartRequest) { r.MaxSession = "168h" }, true},
		{"max_session 7d+", func(r *abStartRequest) { r.MaxSession = "168h1m" }, false},
		{"exclude 32", func(r *abStartRequest) { r.Exclude = prefixes(32) }, true},
		{"exclude 33", func(r *abStartRequest) { r.Exclude = prefixes(33) }, false},
		{"exclude no slash", func(r *abStartRequest) { r.Exclude = []string{"api"} }, false},
		{"exclude 256 bytes", func(r *abStartRequest) { r.Exclude = []string{"/" + strings.Repeat("a", 255)} }, true},
		{"exclude 257 bytes", func(r *abStartRequest) { r.Exclude = []string{"/" + strings.Repeat("a", 256)} }, false},
		{"exclude control char", func(r *abStartRequest) { r.Exclude = []string{"/a\nb"} }, false},
		{"exclude DEL", func(r *abStartRequest) { r.Exclude = []string{"/a\x7f"} }, false},
		{"exclude comma", func(r *abStartRequest) { r.Exclude = []string{"/a,/b"} }, false},
		{"exclude whitespace", func(r *abStartRequest) { r.Exclude = []string{"/a "} }, false},
		{"static none", func(r *abStartRequest) { r.Static = []string{"none"} }, true},
		{"static prefixes", func(r *abStartRequest) { r.Static = []string{"/assets/", "/static/"} }, true},
		{"static bad", func(r *abStartRequest) { r.Static = []string{"assets"} }, false},
		{"min_samples 0", func(r *abStartRequest) { r.Thresholds.MinSamples = abIntP(0) }, false},
		{"min_samples 1", func(r *abStartRequest) { r.Thresholds.MinSamples = abIntP(1) }, true},
		{"min_samples 1e7", func(r *abStartRequest) { r.Thresholds.MinSamples = abIntP(10_000_000) }, true},
		{"min_samples 1e7+1", func(r *abStartRequest) { r.Thresholds.MinSamples = abIntP(10_000_001) }, false},
		{"windows 0", func(r *abStartRequest) { r.Thresholds.Windows = abIntP(0) }, false},
		{"windows 12", func(r *abStartRequest) { r.Thresholds.Windows = abIntP(12) }, true},
		{"windows 13", func(r *abStartRequest) { r.Thresholds.Windows = abIntP(13) }, false},
		{"window_min_samples 0", func(r *abStartRequest) { r.Thresholds.WindowMinSamples = abIntP(0) }, false},
		{"window_min_samples 1e6", func(r *abStartRequest) { r.Thresholds.WindowMinSamples = abIntP(1_000_000) }, true},
		{"window_min_samples 1e6+1", func(r *abStartRequest) { r.Thresholds.WindowMinSamples = abIntP(1_000_001) }, false},
		{"min_runtime 0", func(r *abStartRequest) { r.Thresholds.MinRuntime = "0s" }, true},
		{"min_runtime 24h", func(r *abStartRequest) { r.Thresholds.MinRuntime = "24h" }, true},
		{"min_runtime 25h", func(r *abStartRequest) { r.Thresholds.MinRuntime = "25h" }, false},
		{"min_runtime negative", func(r *abStartRequest) { r.Thresholds.MinRuntime = "-1s" }, false},
		{"warmup 1h", func(r *abStartRequest) { r.Thresholds.Warmup = "1h" }, true},
		{"warmup 61m", func(r *abStartRequest) { r.Thresholds.Warmup = "61m" }, false},
		{"window 59s", func(r *abStartRequest) { r.Thresholds.Window = "59s" }, false},
		{"window 1m", func(r *abStartRequest) { r.Thresholds.Window = "1m" }, true},
		{"window 1h", func(r *abStartRequest) { r.Thresholds.Window = "1h" }, true},
		{"window 61m", func(r *abStartRequest) { r.Thresholds.Window = "61m" }, false},
		{"p95_slack 60s", func(r *abStartRequest) { r.Thresholds.P95Slack = "60s" }, true},
		{"p95_slack 61s", func(r *abStartRequest) { r.Thresholds.P95Slack = "61s" }, false},
		{"err_delta 0.09", func(r *abStartRequest) { r.Thresholds.ErrDelta = abFloatP(0.09) }, false},
		{"err_delta 0.1", func(r *abStartRequest) { r.Thresholds.ErrDelta = abFloatP(0.1) }, true},
		{"err_delta 100", func(r *abStartRequest) { r.Thresholds.ErrDelta = abFloatP(100) }, true},
		{"err_delta 100.1", func(r *abStartRequest) { r.Thresholds.ErrDelta = abFloatP(100.1) }, false},
		{"err_delta NaN", func(r *abStartRequest) { r.Thresholds.ErrDelta = abFloatP(math.NaN()) }, false},
		{"err_ratio 0.99", func(r *abStartRequest) { r.Thresholds.ErrRatio = abFloatP(0.99) }, false},
		{"err_ratio 1", func(r *abStartRequest) { r.Thresholds.ErrRatio = abFloatP(1) }, true},
		{"err_ratio Inf", func(r *abStartRequest) { r.Thresholds.ErrRatio = abFloatP(math.Inf(1)) }, false},
		{"p95_ratio 100", func(r *abStartRequest) { r.Thresholds.P95Ratio = abFloatP(100) }, true},
		{"p95_ratio 101", func(r *abStartRequest) { r.Thresholds.P95Ratio = abFloatP(101) }, false},
	}
	for _, c := range cases {
		r := abBaseReq()
		c.mod(&r)
		_, err := abConfigLabels(r)
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && (err == nil || abStatus(err) != 400) {
			t.Errorf("%s: err = %v, want a 400", c.name, err)
		}
	}
}

// TestABConfigLabelsExactTable: what the dashboard writes, label for label
// — and nothing for a value the caller left out (the proxy's default).
func TestABConfigLabelsExactTable(t *testing.T) {
	got, err := abConfigLabels(abBaseReq())
	if err != nil || len(got) != 0 {
		t.Fatalf("minimal request labels = %v, %v; want none", got, err)
	}
	r := abBaseReq()
	r.Split = abIntP(25)
	r.Assign = "header:x-ab"
	r.Groups = map[string]string{"staff": "B", "beta": "B", "control": "A"}
	r.UIDCookie, r.GroupCookie = "uid", "grp"
	r.Anon = abBoolP(false)
	r.SessionIdle, r.PinRefresh, r.MaxSession = "45m", "10m", "48h"
	r.Exclude = []string{"/api/admin", "/healthz"}
	r.Static = []string{"none"}
	r.CookieJS, r.Override = true, true
	r.Thresholds = abThresholds{
		AutoAbort: abBoolP(false), MinSamples: abIntP(1000), MinRuntime: "30m", Warmup: "5m", Window: "10m",
		Windows: abIntP(3), WindowMinSamples: abIntP(100), ErrDelta: abFloatP(1.5), ErrRatio: abFloatP(3), P95Ratio: abFloatP(1.25), P95Slack: "250ms",
	}
	got, err = abConfigLabels(r)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"proxy.ab.split":              "25",
		"proxy.ab.assign":             "header:x-ab",
		"proxy.ab.groups":             "beta:B,control:A,staff:B",
		"proxy.ab.uid_cookie":         "uid",
		"proxy.ab.group_cookie":       "grp",
		"proxy.ab.anon":               "false",
		"proxy.ab.session_idle":       "45m0s",
		"proxy.ab.pin_refresh":        "10m0s",
		"proxy.ab.max_session":        "48h0m0s",
		"proxy.ab.exclude":            "/api/admin,/healthz",
		"proxy.ab.static":             "none",
		"proxy.ab.cookie_js":          "true",
		"proxy.ab.override":           "true",
		"proxy.ab.autoabort":          "false",
		"proxy.ab.min_samples":        "1000",
		"proxy.ab.min_runtime":        "30m0s",
		"proxy.ab.warmup":             "5m0s",
		"proxy.ab.window":             "10m0s",
		"proxy.ab.windows":            "3",
		"proxy.ab.window_min_samples": "100",
		"proxy.ab.err_delta":          "1.5",
		"proxy.ab.err_ratio":          "3",
		"proxy.ab.p95_ratio":          "1.25",
		"proxy.ab.p95_slack":          "250ms",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("labels =\n%v\nwant\n%v", got, want)
	}
	// Every value is in the primitive grammar the proxy parses it with
	// (time.ParseDuration, strconv.Atoi/ParseFloat, "true"/"false").
	for k, v := range got {
		switch k {
		case labelABSessionIdle, labelABPinRefresh, labelABMaxSession, labelABMinRuntime, labelABWarmup, labelABWindow, labelABP95Slack:
			if _, err := time.ParseDuration(v); err != nil {
				t.Errorf("%s=%q is not a Go duration", k, v)
			}
		case labelABSplit, labelABMinSamples, labelABWindows, labelABWindowMinSamples:
			if _, err := strconv.Atoi(v); err != nil {
				t.Errorf("%s=%q is not an int", k, v)
			}
		case labelABErrDelta, labelABErrRatio, labelABP95Ratio:
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				t.Errorf("%s=%q is not a float", k, v)
			}
		case labelABAnon, labelABCookieJS, labelABOverride, labelABAutoAbort:
			if v != "true" && v != "false" {
				t.Errorf("%s=%q is not a bool", k, v)
			}
		}
	}
}

func TestABRecordLabelsExact(t *testing.T) {
	rec := &abRecord{ID: "deadbeef", Started: 1000, Epoch: 2000, Phase: abPhaseAborted, PhaseAt: 3000, AbortReason: "manual",
		Config: map[string]string{labelABSplit: "30"}}
	want := map[string]string{
		"proxy.ab.split": "30", "proxy.ab.variant": "B", "proxy.ab.id": "deadbeef", "proxy.ab.started": "1000",
		"proxy.ab.epoch": "2000", "proxy.ab.phase": "aborted", "proxy.ab.phase_at": "3000", "proxy.ab.abort_reason": "manual",
	}
	if got := rec.labels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v\nwant %v", got, want)
	}
	rec.AbortReason, rec.Phase = "", abPhaseRunning
	if _, ok := rec.labels()[labelABAbortReason]; ok {
		t.Fatal("abort_reason written with no reason")
	}
	if !abIDRe.MatchString(newABID()) || len(newABID()) != 8 {
		t.Fatal("newABID outside the proxy's id grammar")
	}
}

// abDashboardLabels is every proxy.ab.* key the dashboard writes or reads.
var abDashboardLabels = []string{
	labelABVariant, labelABID, labelABAssign, labelABSplit, labelABGroups, labelABUIDCookie, labelABGroupCookie,
	labelABAnon, labelABSessionIdle, labelABPinRefresh, labelABMaxSession, labelABStarted, labelABEpoch, labelABPhase,
	labelABPhaseAt, labelABAbortReason, labelABExclude, labelABStatic, labelABCookieJS, labelABOverride, labelABAutoAbort,
	labelABMinSamples, labelABMinRuntime, labelABWarmup, labelABWindow, labelABWindows, labelABWindowMinSamples,
	labelABErrDelta, labelABErrRatio, labelABP95Ratio, labelABP95Slack,
}

// TestABLabelNamesMirrorProxy reads cmd/proxy/docker.go: the two binaries
// share no code, so the label names must be kept equal by hand — this is
// the tripwire.
func TestABLabelNamesMirrorProxy(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "proxy", "docker.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	proxy := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if v, err := strconv.Unquote(lit.Value); err == nil && strings.HasPrefix(v, labelABPrefix) {
				proxy[v] = true
			}
		}
		return true
	})
	ours := map[string]bool{}
	for _, l := range abDashboardLabels {
		ours[l] = true
		if !proxy[l] {
			t.Errorf("dashboard label %q is not a proxy label", l)
		}
	}
	for l := range proxy {
		if !ours[l] {
			t.Errorf("proxy label %q has no dashboard counterpart", l)
		}
	}
}

// abStructTags returns json tag -> type expression for every struct type in
// files, keyed by type name.
func abStructTags(t *testing.T, files ...string) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, file := range files {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			fields := map[string]string{}
			for _, fl := range st.Fields.List {
				if fl.Tag == nil {
					continue
				}
				tag, _ := strconv.Unquote(fl.Tag.Value)
				name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
				if name != "" && name != "-" {
					fields[name] = types.ExprString(fl.Type)
				}
			}
			out[ts.Name.Name] = fields
			return true
		})
	}
	return out
}

// TestABProxyReportShapeMirrorsProxy: the dashboard decodes the proxy's
// /ab strictly typed, so one field of the wrong shape would make every
// report undecodable — the proxy would look unreachable (no auto-abort
// pickup, deadline-only drain). Every field we decode must exist in the
// proxy's report with the same type.
func TestABProxyReportShapeMirrorsProxy(t *testing.T) {
	proxy := abStructTags(t, filepath.Join("..", "proxy", "abtest.go"), filepath.Join("..", "proxy", "abmesh.go"))
	ours := abStructTags(t, "abmanager.go")
	rename := strings.NewReplacer(
		"abProxyPair", "abPair", "abProxyCounters", "abCountersReport", "abProxyPinned", "abPinnedReport",
		"abProxyWindow", "abWindowReport", "abProxyJudge", "abJudgeReport", "abProxyMerged", "abMergedReport",
		"abProxyAbort", "abAbortInfo", "abProxyExperiment", "abExperimentReport", "abProxyReport", "abReport",
	)
	pairs := map[string]string{
		"abProxyCounters": "abCountersReport", "abProxyPair": "abPair", "abProxyPinned": "abPinnedReport",
		"abProxyAbort": "abAbortInfo", "abProxyWindow": "abWindowReport", "abProxyJudge": "abJudgeReport",
		"abProxyMerged": "abMergedReport", "abProxyExperiment": "abExperimentReport", "abProxyReport": "abReport",
	}
	for mine, theirs := range pairs {
		mf, pf := ours[mine], proxy[theirs]
		if len(mf) == 0 || len(pf) == 0 {
			t.Fatalf("struct %s (%d fields) / %s (%d fields) not found", mine, len(mf), theirs, len(pf))
		}
		for tag, typ := range mf {
			pt, ok := pf[tag]
			if !ok {
				t.Errorf("%s.%s: no such field in proxy %s", mine, tag, theirs)
				continue
			}
			if rename.Replace(typ) != pt {
				t.Errorf("%s.%s: type %s, proxy has %s", mine, tag, typ, pt)
			}
		}
	}
}

func TestABStorePersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "abtests.json")
	s, err := loadABStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := &abRecord{ID: "abcd1234", Image: "x:2", Replicas: 1, Phase: abPhaseRunning, Config: map[string]string{labelABSplit: "5"}, Op: abOpRelabel}
	if err := s.put("app", rec); err != nil {
		t.Fatal(err)
	}
	rec.Config[labelABSplit] = "99"
	if got, _ := s.get("app"); got.Config[labelABSplit] != "5" {
		t.Fatal("put kept a reference to the caller's map")
	}
	got, _ := s.get("app")
	got.Config[labelABSplit] = "77"
	if again, _ := s.get("app"); again.Config[labelABSplit] != "5" {
		t.Fatal("get returned a shared map")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("store file = %v, %v; want mode 0600", fi, err)
	}
	r2, err := loadABStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r2.get("app"); !ok || got.ID != "abcd1234" || got.Op != abOpRelabel || got.Config[labelABSplit] != "5" {
		t.Fatalf("reloaded = %+v, %v", got, ok)
	}
	for i := 0; i < abMaxHistory+5; i++ {
		r2.appendHistory("app", abHistoryEntry{ID: strconv.Itoa(i)})
	}
	if err := r2.finish("app", abHistoryEntry{ID: "last", Outcome: "discarded"}); err != nil {
		t.Fatal(err)
	}
	r3, _ := loadABStore(path)
	h := r3.history("app")
	if _, ok := r3.get("app"); ok || len(h) != abMaxHistory || h[len(h)-1].ID != "last" {
		t.Fatalf("after finish: history %d entries (last %+v)", len(h), h[len(h)-1])
	}
	if svcs := r3.services(); len(svcs) != 0 {
		t.Fatalf("services = %v", svcs)
	}
}

func TestABStatsAndPercentile(t *testing.T) {
	hist := make([]uint64, 64)
	hist[10] = 100
	b := abHistBoundsDefault
	p50 := abPercentile(hist, b, 0.5)
	if p50 <= b[9] || p50 > b[10] {
		t.Fatalf("p50 = %v, want inside bucket 10 (%v..%v]", p50, b[9], b[10])
	}
	if abPercentile(make([]uint64, 64), b, 0.5) != 0 {
		t.Fatal("empty histogram has a percentile")
	}
	// Error rate = (5xx + transport + failover) / (requests + transport + failover).
	st := abStats(abProxyCounters{Requests: 90, Err5xx: 5, Transport: 6, Failover: 4, Hist: hist}, b)
	if st.Requests != 100 || st.Errors != 15 || st.ErrorRate != 0.15 {
		t.Fatalf("stats = %+v", st)
	}
	if math.Abs(abHistBoundsDefault[0]-1.2) > 1e-9 || math.Abs(abHistBoundsDefault[1]-1.44) > 1e-9 {
		t.Fatalf("default bounds = %v", abHistBoundsDefault[:2])
	}
}

func TestABZHint(t *testing.T) {
	if h := abZHint(abStatsView{Requests: 10}, abStatsView{Requests: 1000}); h.Z != 0 || !strings.Contains(h.Text, "insufficient") {
		t.Fatalf("few samples: %+v", h)
	}
	a := abStatsView{Requests: 10000, Errors: 100, ErrorRate: 0.01}
	worse := abStatsView{Requests: 10000, Errors: 300, ErrorRate: 0.03}
	if h := abZHint(a, worse); h.Z < 1.96 || !strings.Contains(h.Text, "higher") {
		t.Fatalf("worse B: %+v", h)
	}
	if h := abZHint(worse, a); h.Z > -1.96 || !strings.Contains(h.Text, "lower") {
		t.Fatalf("better B: %+v", h)
	}
	same := abStatsView{Requests: 10000, Errors: 102, ErrorRate: 0.0102}
	if h := abZHint(a, same); math.Abs(h.Z) >= 1.96 || !strings.Contains(h.Text, "no significant") {
		t.Fatalf("same: %+v", h)
	}
}

func TestAbGroupsLabelSorted(t *testing.T) {
	got := abGroupsLabel(map[string]string{"b": "B", "a": "A"})
	parts := strings.Split(got, ",")
	if !sort.StringsAreSorted(parts) || got != "a:A,b:B" {
		t.Fatalf("groups label = %q", got)
	}
}
