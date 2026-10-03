package main

// A/B testing, proxy side (docs/AB_TESTING_PLAN.md §1.2–§1.8, §1.10, §2,
// §3). Completely inert for a route group whose service has no B replica:
// RouteGroup.ab stays nil and ServeHTTP never enters any code in this file
// beyond the hop-auth header strip.

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- Hop authentication ----

// PeerAuthHeader authenticates a PeerHopHeader for A/B purposes only: a hop
// carrying it with the right value may pass its X-Variant through (§1.3 step
// 2) and is recorded. Every other effect of PeerHopHeader is unchanged and
// still unauthenticated. ServeHTTP strips it from every inbound request.
const PeerAuthHeader = "X-Pmgr-Peer-Auth"

// peerHopAuthToken derives the hop-auth value from PMGR_PEER_SECRET. An empty
// secret yields "", and "" never authenticates (see hopAuthOK).
func peerHopAuthToken(secret string) string {
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("pmgr-ab-hop/v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func (r *Router) hopAuthOK(v string) bool {
	return r.peerHopAuth != "" && hmac.Equal([]byte(v), []byte(r.peerHopAuth))
}

// clock is the Router's time source; tests inject r.now.
func (r *Router) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// ---- Variants ----

const (
	abVariantA = "A"
	abVariantB = "B"

	abPhaseRunning    = "running"
	abPhaseAborted    = "aborted"
	abPhasePromoting  = "promoting"
	abPhaseDiscarding = "discarding"

	abVariantHeader = "X-Variant"
)

// abStampedKey marks a request whose X-Variant was set by abProxyToGroup.
// A peer backend's Director attaches PeerAuthHeader only to such requests,
// so a hop never vouches for a client-supplied X-Variant passed through
// untouched by a route without a test.
type abStampedKey struct{}

func abStamped(req *http.Request) bool {
	v, _ := req.Context().Value(abStampedKey{}).(bool)
	return v
}

func (b *Backend) variantName() string {
	if b.Variant == abVariantB {
		return abVariantB
	}
	return abVariantA
}

// inVariant is the pick-function filter: "" matches everything.
func (b *Backend) inVariant(v string) bool {
	return v == "" || b.variantName() == v
}

func abIdx(v string) int {
	if v == abVariantB {
		return 1
	}
	return 0
}

func abOther(v string) string {
	if v == abVariantB {
		return abVariantA
	}
	return abVariantB
}

// hasVariant reports whether any pickable backend of variant v exists,
// regardless of probe health (DockerUnhealthy ones are never pickable), so
// the static-retry wrapper is skipped when a retry could never go anywhere.
func (g *RouteGroup) hasVariant(v string) bool {
	for _, b := range g.Backends {
		if b.inVariant(v) && !b.DockerUnhealthy {
			return true
		}
	}
	return false
}

// hasBBackend decides whether a test attaches to the group: any B backend,
// healthy or not, so a B that Docker reports unhealthy still routes its
// pins through failover and keeps being judged.
func (g *RouteGroup) hasBBackend() bool {
	for _, b := range g.Backends {
		if b.Variant == abVariantB {
			return true
		}
	}
	return false
}

// ---- Config (§2) ----

type abConfig struct {
	ID               string
	Assign           string // "cookie" | "random" | "header"
	Header           string // canonical header name when Assign == "header"
	Split            int
	Groups           map[string]string
	UIDCookie        string
	GroupCookie      string
	Anon             bool
	SessionIdle      time.Duration
	PinRefresh       time.Duration
	MaxSession       time.Duration
	Started          int64 // 0 = invalid/absent; resolved per run in reconcileAB
	Epoch            int64 // 0 = default (= Started)
	Phase            string
	PhaseAt          int64 // 0 = default (= Started)
	AbortReason      string
	Exclude          []string
	Static           []string
	CookieJS         bool
	Override         bool
	AutoAbort        bool
	MinSamples       int
	MinRuntime       time.Duration
	Warmup           time.Duration
	Window           time.Duration
	Windows          int
	WindowMinSamples int
	ErrDelta         float64
	ErrRatio         float64
	P95Ratio         float64
	P95Slack         time.Duration
}

var (
	abIDRe        = regexp.MustCompile(`^[a-z0-9]{4,16}$`)
	abGroupNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	abUIDRe       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	abAnonRe      = regexp.MustCompile(`^[0-9a-f]{16}$`)
	abPinRe       = regexp.MustCompile(`^([a-z0-9]{4,16})\.([AB])\.([gunq])\.([0-9a-f]{8})\.([0-9]{1,12})\.([0-9]{1,12})$`)
)

const (
	abDefaultStatic = "/_next/static/"
	abMaxPrefixes   = 32
	abMaxPrefixLen  = 256
	abMaxGroups     = 32
	abMaxTokenLen   = 64
	abPinMaxLen     = 96
	abPinSkew       = 2 * time.Minute
	abPinnedCap     = 50000
	abRingSize      = 3
	abHistBuckets   = 64
	abHistRatio     = 1.2
	abEvalInterval  = 10 * time.Second
	abAnonCookie    = "ab_anon"
	abAnonMaxAge    = 30 * 24 * time.Hour
	abQueryOverride = "pm_variant"
)

// isToken reports an RFC 7230 token of at most abMaxTokenLen bytes — the
// same character set is valid for header names and cookie names.
func isToken(s string) bool {
	if s == "" || len(s) > abMaxTokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// parsePrefixes validates an exclude/static list. ok=false means the whole
// label is invalid (caller falls back to the default).
func parsePrefixes(v string) ([]string, bool) {
	parts := splitTrimmed(v)
	if len(parts) > abMaxPrefixes {
		return nil, false
	}
	for _, p := range parts {
		if !strings.HasPrefix(p, "/") || len(p) > abMaxPrefixLen {
			return nil, false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < 0x20 || p[i] == 0x7f {
				return nil, false
			}
		}
	}
	return parts, true
}

func parseABGroups(v string) (map[string]string, bool) {
	parts := splitTrimmed(v)
	if len(parts) > abMaxGroups {
		return nil, false
	}
	out := map[string]string{}
	for _, p := range parts {
		name, variant, ok := strings.Cut(p, ":")
		if !ok || !abGroupNameRe.MatchString(name) || (variant != abVariantA && variant != abVariantB) {
			return nil, false
		}
		out[name] = variant
	}
	return out, true
}

func parseABFloat(v string, lo, hi float64) (float64, bool) {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < lo || f > hi {
		return 0, false
	}
	return f, true
}

func parseABInt(v string, lo, hi int) (int, bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, false
	}
	return n, true
}

func parseABDuration(v string, lo, hi time.Duration) (time.Duration, bool) {
	d, err := time.ParseDuration(v)
	if err != nil || d < lo || d > hi {
		return 0, false
	}
	return d, true
}

func parseABBool(v string) (bool, bool) {
	switch v {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func parseUnix(v string) (int64, bool) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// parseABConfig parses the proxy.ab.* labels of a B replica. The caller has
// already validated proxy.ab.variant and proxy.ab.id. Every other invalid
// value is reported through warn (which dedups) and replaced by its default.
// Peer-received config goes through this same function (abmesh.go).
func parseABConfig(labels map[string]string, now time.Time, warn func(key, val string)) *abConfig {
	c := &abConfig{
		ID:               labels[labelABID],
		Assign:           "cookie",
		Split:            10,
		Groups:           map[string]string{},
		UIDCookie:        "ab_uid",
		GroupCookie:      "ab_group",
		Anon:             true,
		SessionIdle:      30 * time.Minute,
		PinRefresh:       5 * time.Minute,
		MaxSession:       24 * time.Hour,
		Phase:            abPhaseRunning,
		Static:           []string{abDefaultStatic},
		AutoAbort:        true,
		MinSamples:       500,
		MinRuntime:       15 * time.Minute,
		Warmup:           3 * time.Minute,
		Window:           5 * time.Minute,
		Windows:          2,
		WindowMinSamples: 50,
		ErrDelta:         2,
		ErrRatio:         2,
		P95Ratio:         1.5,
		P95Slack:         200 * time.Millisecond,
	}
	get := func(key string) (string, bool) {
		v, ok := labels[key]
		return v, ok && v != ""
	}
	bad := func(key, val string) { warn(key, val) }

	if v, ok := get(labelABAssign); ok {
		switch {
		case v == "cookie", v == "random":
			c.Assign = v
		case strings.HasPrefix(v, "header:"):
			name := strings.TrimPrefix(v, "header:")
			canon := http.CanonicalHeaderKey(name)
			if isToken(name) && canon != "Cookie" && canon != "Host" {
				c.Assign = "header"
				c.Header = canon
			} else {
				bad(labelABAssign, v)
			}
		default:
			bad(labelABAssign, v)
		}
	}
	if v, ok := get(labelABSplit); ok {
		if n, ok := parseABInt(v, 0, 100); ok {
			c.Split = n
		} else {
			bad(labelABSplit, v)
		}
	}
	if v, ok := get(labelABGroups); ok {
		if m, ok := parseABGroups(v); ok {
			c.Groups = m
		} else {
			bad(labelABGroups, v)
		}
	}
	for _, kv := range []struct {
		key string
		dst *string
	}{{labelABUIDCookie, &c.UIDCookie}, {labelABGroupCookie, &c.GroupCookie}} {
		if v, ok := get(kv.key); ok {
			if isToken(v) {
				*kv.dst = v
			} else {
				bad(kv.key, v)
			}
		}
	}
	for _, kv := range []struct {
		key string
		dst *bool
	}{{labelABAnon, &c.Anon}, {labelABCookieJS, &c.CookieJS}, {labelABOverride, &c.Override}, {labelABAutoAbort, &c.AutoAbort}} {
		if v, ok := get(kv.key); ok {
			if b, ok := parseABBool(v); ok {
				*kv.dst = b
			} else {
				bad(kv.key, v)
			}
		}
	}
	for _, kv := range []struct {
		key    string
		dst    *time.Duration
		lo, hi time.Duration
	}{
		{labelABSessionIdle, &c.SessionIdle, 5 * time.Minute, 24 * time.Hour},
		{labelABMaxSession, &c.MaxSession, time.Hour, 7 * 24 * time.Hour},
		{labelABMinRuntime, &c.MinRuntime, 0, 24 * time.Hour},
		{labelABWarmup, &c.Warmup, 0, time.Hour},
		{labelABWindow, &c.Window, time.Minute, time.Hour},
		{labelABP95Slack, &c.P95Slack, 0, 60 * time.Second},
	} {
		if v, ok := get(kv.key); ok {
			if d, ok := parseABDuration(v, kv.lo, kv.hi); ok {
				*kv.dst = d
			} else {
				bad(kv.key, v)
			}
		}
	}
	// pin_refresh is bounded by session_idle, so it is parsed after it, and
	// the default itself is clamped when session_idle is short.
	if c.PinRefresh > c.SessionIdle/2 {
		c.PinRefresh = c.SessionIdle / 2
	}
	if v, ok := get(labelABPinRefresh); ok {
		if d, ok := parseABDuration(v, time.Minute, c.SessionIdle/2); ok {
			c.PinRefresh = d
		} else {
			bad(labelABPinRefresh, v)
		}
	}
	for _, kv := range []struct {
		key    string
		dst    *int
		lo, hi int
	}{
		{labelABMinSamples, &c.MinSamples, 1, 10_000_000},
		{labelABWindows, &c.Windows, 1, 12},
		{labelABWindowMinSamples, &c.WindowMinSamples, 1, 1_000_000},
	} {
		if v, ok := get(kv.key); ok {
			if n, ok := parseABInt(v, kv.lo, kv.hi); ok {
				*kv.dst = n
			} else {
				bad(kv.key, v)
			}
		}
	}
	for _, kv := range []struct {
		key    string
		dst    *float64
		lo, hi float64
	}{
		{labelABErrDelta, &c.ErrDelta, 0.1, 100},
		{labelABErrRatio, &c.ErrRatio, 1, 100},
		{labelABP95Ratio, &c.P95Ratio, 1, 100},
	} {
		if v, ok := get(kv.key); ok {
			if f, ok := parseABFloat(v, kv.lo, kv.hi); ok {
				*kv.dst = f
			} else {
				bad(kv.key, v)
			}
		}
	}
	// Timestamps: positive, at most 1d in the future, at most 90d old, so a
	// multi-day test survives a proxy restart with its windows intact.
	day := int64(24 * time.Hour / time.Second)
	for _, kv := range []struct {
		key string
		dst *int64
	}{{labelABStarted, &c.Started}, {labelABEpoch, &c.Epoch}, {labelABPhaseAt, &c.PhaseAt}} {
		if v, ok := get(kv.key); ok {
			if n, ok := parseUnix(v); ok && n >= now.Unix()-90*day && n <= now.Unix()+day {
				*kv.dst = n
			} else {
				bad(kv.key, v)
			}
		}
	}
	if v, ok := get(labelABPhase); ok {
		switch v {
		case abPhaseRunning, abPhaseAborted, abPhasePromoting, abPhaseDiscarding:
			c.Phase = v
		default:
			bad(labelABPhase, v)
		}
	}
	if v, ok := get(labelABAbortReason); ok {
		switch v {
		case "errors", "latency", "manual":
			c.AbortReason = v
		default:
			bad(labelABAbortReason, v)
		}
	}
	if v, ok := get(labelABExclude); ok {
		if ps, ok := parsePrefixes(v); ok {
			c.Exclude = ps
		} else {
			bad(labelABExclude, v)
		}
	}
	if v, ok := get(labelABStatic); ok {
		if v == "none" {
			c.Static = nil
		} else if ps, ok := parsePrefixes(v); ok {
			c.Static = ps
		} else {
			bad(labelABStatic, v)
		}
	}
	return c
}

// labels renders the normalized config back into label form — the shape
// /ab exposes and the peer payload carries, so a receiver can re-run
// parseABConfig on it unchanged.
func (c *abConfig) labels() map[string]string {
	assign := c.Assign
	if assign == "header" {
		assign = "header:" + c.Header
	}
	groups := make([]string, 0, len(c.Groups))
	for n, v := range c.Groups {
		groups = append(groups, n+":"+v)
	}
	sort.Strings(groups)
	static := strings.Join(c.Static, ",")
	if len(c.Static) == 0 {
		static = "none"
	}
	out := map[string]string{
		labelABVariant:          abVariantB,
		labelABID:               c.ID,
		labelABAssign:           assign,
		labelABSplit:            strconv.Itoa(c.Split),
		labelABGroups:           strings.Join(groups, ","),
		labelABUIDCookie:        c.UIDCookie,
		labelABGroupCookie:      c.GroupCookie,
		labelABAnon:             strconv.FormatBool(c.Anon),
		labelABSessionIdle:      c.SessionIdle.String(),
		labelABPinRefresh:       c.PinRefresh.String(),
		labelABMaxSession:       c.MaxSession.String(),
		labelABStarted:          strconv.FormatInt(c.Started, 10),
		labelABEpoch:            strconv.FormatInt(c.Epoch, 10),
		labelABPhase:            c.Phase,
		labelABPhaseAt:          strconv.FormatInt(c.PhaseAt, 10),
		labelABAbortReason:      c.AbortReason,
		labelABExclude:          strings.Join(c.Exclude, ","),
		labelABStatic:           static,
		labelABCookieJS:         strconv.FormatBool(c.CookieJS),
		labelABOverride:         strconv.FormatBool(c.Override),
		labelABAutoAbort:        strconv.FormatBool(c.AutoAbort),
		labelABMinSamples:       strconv.Itoa(c.MinSamples),
		labelABMinRuntime:       c.MinRuntime.String(),
		labelABWarmup:           c.Warmup.String(),
		labelABWindow:           c.Window.String(),
		labelABWindows:          strconv.Itoa(c.Windows),
		labelABWindowMinSamples: strconv.Itoa(c.WindowMinSamples),
		labelABErrDelta:         strconv.FormatFloat(c.ErrDelta, 'g', -1, 64),
		labelABErrRatio:         strconv.FormatFloat(c.ErrRatio, 'g', -1, 64),
		labelABP95Ratio:         strconv.FormatFloat(c.P95Ratio, 'g', -1, 64),
		labelABP95Slack:         c.P95Slack.String(),
	}
	// Unresolved (0) timestamps mean "default"; omit them rather than emit
	// a value the parser would reject.
	for key, v := range map[string]int64{labelABStarted: c.Started, labelABEpoch: c.Epoch, labelABPhaseAt: c.PhaseAt} {
		if v == 0 {
			delete(out, key)
		}
	}
	return out
}

// abLabelWarnSeen dedups A/B label warnings per container+label, the same
// way cacheLabelWarnSeen does for proxy.cache. Values are logged only for
// labels (never request data), and none of them is a secret.
var (
	abLabelWarnMu   sync.Mutex
	abLabelWarnSeen = map[string]bool{}
)

func abWarnOnce(container, key, msg string) {
	abLabelWarnMu.Lock()
	defer abLabelWarnMu.Unlock()
	k := container + "|" + key
	if abLabelWarnSeen[k] {
		return
	}
	abLabelWarnSeen[k] = true
	log.Printf("%s: %s", container, msg)
}

// abScanResult is assembleGroups' view of the A/B labels across all
// containers: one config per service, the B containers that belong to it,
// and the containers left out of routing entirely (fail closed).
type abScanResult struct {
	configs  map[string]*abConfig
	bNames   map[string]bool
	excluded map[string]bool
}

// abScan implements §1.2: variant=B + valid id makes a backend B; variant=A
// is a no-op; any other variant, a bad id, a B without proxy.service, or a
// second id within the same service (the smallest id wins) is excluded.
// Config is read from the smallest-named B container of the winning id.
func abScan(containers []dockerContainer, now time.Time) abScanResult {
	res := abScanResult{configs: map[string]*abConfig{}, bNames: map[string]bool{}, excluded: map[string]bool{}}
	type cand struct{ name, id string }
	bySvc := map[string][]cand{}
	labelsByName := map[string]map[string]string{}
	for _, c := range containers {
		v, ok := c.Labels[labelABVariant]
		if !ok || v == abVariantA {
			continue
		}
		name := c.name()
		if v != abVariantB {
			abWarnOnce(name, labelABVariant, fmt.Sprintf("bad %s=%q — backend excluded from routing", labelABVariant, v))
			res.excluded[name] = true
			continue
		}
		id := c.Labels[labelABID]
		if !abIDRe.MatchString(id) {
			abWarnOnce(name, labelABID, fmt.Sprintf("bad %s=%q — backend excluded from routing", labelABID, id))
			res.excluded[name] = true
			continue
		}
		svc := c.Labels[labelService]
		if svc == "" {
			abWarnOnce(name, labelService, fmt.Sprintf("%s=B without %s — backend excluded from routing", labelABVariant, labelService))
			res.excluded[name] = true
			continue
		}
		bySvc[svc] = append(bySvc[svc], cand{name, id})
		labelsByName[name] = c.Labels
	}
	for svc, cs := range bySvc {
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].id != cs[j].id {
				return cs[i].id < cs[j].id
			}
			return cs[i].name < cs[j].name
		})
		win := cs[0]
		for _, c := range cs {
			if c.id != win.id {
				abWarnOnce(c.name, labelABID, fmt.Sprintf("service %q already has A/B test %s — backend with %s=%s excluded from routing", svc, win.id, labelABID, c.id))
				res.excluded[c.name] = true
				continue
			}
			res.bNames[c.name] = true
		}
		name := win.name
		res.configs[svc] = parseABConfig(labelsByName[name], now, func(key, val string) {
			abWarnOnce(name, key, fmt.Sprintf("bad %s=%q — using the default", key, val))
		})
	}
	return res
}

// ---- Runtime state (§1.8) ----

// abHistBounds are the histogram's upper bucket edges in ms: 1.2^(i+1),
// ~1.2ms to ~117s. Anything above the last edge lands in the last bucket.
var abHistBounds = func() []float64 {
	out := make([]float64, abHistBuckets)
	v := 1.0
	for i := range out {
		v *= abHistRatio
		out[i] = v
	}
	return out
}()

func abHistBucket(ms float64) int {
	i := sort.SearchFloat64s(abHistBounds, ms)
	if i >= abHistBuckets {
		i = abHistBuckets - 1
	}
	return i
}

type abCounters struct {
	Requests  uint64
	Err5xx    uint64
	Transport uint64
	Failover  uint64
	Hist      [abHistBuckets]uint64
}

func (c *abCounters) add(o *abCounters) {
	c.Requests += o.Requests
	c.Err5xx += o.Err5xx
	c.Transport += o.Transport
	c.Failover += o.Failover
	for i := range c.Hist {
		c.Hist[i] += o.Hist[i]
	}
}

// samples is the denominator of the error rate and what min_samples counts.
func (c *abCounters) samples() uint64 { return c.Requests + c.Transport + c.Failover }

func (c *abCounters) errRate() float64 {
	n := c.samples()
	if n == 0 {
		return 0
	}
	return float64(c.Err5xx+c.Transport+c.Failover) / float64(n)
}

// percentile interpolates linearly inside the bucket where rank q falls.
func (c *abCounters) percentile(q float64) float64 {
	var total uint64
	for _, n := range c.Hist {
		total += n
	}
	if total == 0 {
		return 0
	}
	target := q * float64(total)
	var cum float64
	for i, n := range c.Hist {
		if n == 0 {
			continue
		}
		if cum+float64(n) >= target {
			lo := 0.0
			if i > 0 {
				lo = abHistBounds[i-1]
			}
			frac := (target - cum) / float64(n)
			return lo + frac*(abHistBounds[i]-lo)
		}
		cum += float64(n)
	}
	return abHistBounds[abHistBuckets-1]
}

type abWindow struct {
	Index int64
	V     [2]abCounters
}

// abSummary is what judge() consumes: one proxy's cumulative counters and
// its ring of recent windows. Fresh peers' summaries are fed in alongside.
type abSummary struct {
	Cumulative [2]abCounters
	Windows    []abWindow
}

type abJudgeState struct {
	Seen    bool  // LastIdx is meaningful
	LastIdx int64 // newest window index already judged (or skipped as neutral)
	Streak  int   // consecutive bad judged windows
	// Reason/Detail describe the newest bad window, so an abort that only
	// becomes possible on a later call (min_runtime) keeps the right reason.
	Reason string
	Detail string
}

type abVerdict struct {
	Abort  bool
	Reason string // "errors" | "latency"
	Detail string
	Status string // warmup | insufficient_samples | min_runtime | ok | abort
}

type abAbortInfo struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
	At     int64  `json:"at"`
	Source string `json:"source"` // "auto" (this proxy's latch) | "peer" (a peer's latch) | "label"
}

// abRun is one test's runtime on this proxy, keyed by service in
// Router.abRuns and replaced only when a different test id shows up — so a
// B restart that briefly detaches the test keeps its stats and abort latch.
type abRun struct {
	service   string
	id        string
	firstSeen int64
	latched   atomic.Bool

	mu              sync.Mutex
	cfg             *abConfig // resolved; replaced on every Router.Set
	attached        bool
	bLocal          int
	abort           abAbortInfo
	cum             [2]abCounters
	warm            [2]abCounters
	ring            [abRingSize]abWindow
	js              abJudgeState
	verdict         abVerdict
	failover        [2]uint64
	draining        [2]uint64
	hopErrors       uint64
	staticFallbacks uint64
	pinnedLast      [2]int64
	pinnedNonces    [2]map[string]int64
	// peers holds each peer's latest validated experiment for this test id
	// (abmesh.go applyPeerAB); syncInterval is Router.peerSyncInterval.
	peers        map[string]*abPeerSnap
	syncInterval time.Duration
	clock        func() time.Time
}

func newABRun(service, id string, now time.Time) *abRun {
	run := &abRun{service: service, id: id, firstSeen: now.Unix(), peers: map[string]*abPeerSnap{}}
	run.resetRing()
	for i := range run.pinnedNonces {
		run.pinnedNonces[i] = map[string]int64{}
	}
	return run
}

func (run *abRun) resetRing() {
	for i := range run.ring {
		run.ring[i] = abWindow{Index: -1}
	}
	run.js = abJudgeState{}
}

// phase is the effective phase: the label's, except that the in-memory
// auto-abort latch turns running into aborted.
func (run *abRun) phase(cfg *abConfig) string {
	if cfg.Phase == abPhaseRunning && run.latched.Load() {
		return abPhaseAborted
	}
	return cfg.Phase
}

func abFrozen(phase string) bool { return phase == abPhaseAborted || phase == abPhaseDiscarding }

// abWindowIndex returns the tumbling window now falls in, or -1 during
// warm-up. Anchored at epoch+warmup so every proxy judges the same windows.
func abWindowIndex(cfg *abConfig, now time.Time) int64 {
	anchor := time.Unix(cfg.Epoch, 0).Add(cfg.Warmup)
	if now.Before(anchor) {
		return -1
	}
	return int64(now.Sub(anchor) / cfg.Window)
}

type abOutcome int

const (
	abServed abOutcome = iota
	abTransport
	abFailover
)

// record adds one judged request. Callers have already applied every
// "not recorded" rule; a frozen phase is re-checked here under the lock.
func (run *abRun) record(v int, out abOutcome, status int, latency time.Duration, now time.Time) {
	run.mu.Lock()
	defer run.mu.Unlock()
	cfg := run.cfg
	if abFrozen(run.phase(cfg)) {
		return
	}
	var c abCounters
	switch out {
	case abServed:
		c.Requests = 1
		if status >= 500 {
			c.Err5xx = 1
		}
		c.Hist[abHistBucket(float64(latency)/float64(time.Millisecond))] = 1
	case abTransport:
		c.Transport = 1
	case abFailover:
		c.Failover = 1
		run.failover[v]++
	}
	idx := abWindowIndex(cfg, now)
	if idx < 0 {
		run.warm[v].add(&c)
		return
	}
	slot := &run.ring[idx%abRingSize]
	if slot.Index != idx {
		*slot = abWindow{Index: idx}
	}
	slot.V[v].add(&c)
	run.cum[v].add(&c)
}

// notePinned tracks a non-hopped request carrying a valid pin. While frozen
// it also feeds the display-only draining counter.
func (run *abRun) notePinned(v int, nonce string, now time.Time, frozen bool) {
	run.mu.Lock()
	defer run.mu.Unlock()
	t := now.Unix()
	run.pinnedLast[v] = t
	m := run.pinnedNonces[v]
	if _, ok := m[nonce]; ok || len(m) < abPinnedCap {
		m[nonce] = t
	}
	if frozen {
		run.draining[v]++
	}
}

func (run *abRun) addHopError() {
	run.mu.Lock()
	run.hopErrors++
	run.mu.Unlock()
}

func (run *abRun) addStaticFallback() {
	run.mu.Lock()
	run.staticFallbacks++
	run.mu.Unlock()
}

func (run *abRun) summaryLocked() abSummary {
	s := abSummary{Cumulative: run.cum}
	for _, w := range run.ring {
		if w.Index >= 0 {
			s.Windows = append(s.Windows, w)
		}
	}
	sort.Slice(s.Windows, func(i, j int) bool { return s.Windows[i].Index < s.Windows[j].Index })
	return s
}

// evaluate is one evaluator tick for this run: prune idle pins, then judge
// while running with autoabort on and no latch yet, over this proxy's
// summary plus every fresh, aligned peer's. With peers in the mesh a window
// is judged only two sync intervals after it closes, so a peer's final push
// for it has arrived.
func (run *abRun) evaluate(now time.Time) {
	run.mu.Lock()
	defer run.mu.Unlock()
	cfg := run.cfg
	cutoff := now.Add(-cfg.SessionIdle).Unix()
	for _, m := range run.pinnedNonces {
		for k, t := range m {
			if t < cutoff {
				delete(m, k)
			}
		}
	}
	if run.phase(cfg) != abPhaseRunning || !cfg.AutoAbort {
		return
	}
	judgeNow := now
	if len(run.peers) > 0 {
		judgeNow = now.Add(-2 * run.syncEvery())
	}
	st, v := judge(cfg, run.js, run.summaryLocked(), run.peerSummariesLocked(now), judgeNow)
	run.js, run.verdict = st, v
	if v.Abort {
		run.latched.Store(true)
		run.abort = abAbortInfo{Reason: v.Reason, Detail: v.Detail, At: now.Unix(), Source: "auto"}
		log.Printf("ab: service %q test %s auto-aborted (%s): %s — new sessions go to A", run.service, run.id, v.Reason, v.Detail)
	}
}

// ---- judge (§1.8) ----

// judge is pure: given the config, the previous judging state, this
// proxy's summary and any peers' summaries, it returns the next state and a
// verdict. Callers only invoke it while the effective phase is running.
func judge(cfg *abConfig, st abJudgeState, own abSummary, peers []abSummary, now time.Time) (abJudgeState, abVerdict) {
	merged := abMergeSummaries(append([]abSummary{own}, peers...))
	cum := merged.Cumulative
	byIdx := map[int64]*abWindow{}
	for i := range merged.Windows {
		byIdx[merged.Windows[i].Index] = &merged.Windows[i]
	}
	cur := abWindowIndex(cfg, now)
	if cur < 0 {
		return st, abVerdict{Status: "warmup"}
	}
	var idxs []int64
	for idx := range byIdx {
		if idx < cur && (!st.Seen || idx > st.LastIdx) {
			idxs = append(idxs, idx)
		}
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
	for _, idx := range idxs {
		w := byIdx[idx]
		st.Seen, st.LastIdx = true, idx
		a, b := &w.V[0], &w.V[1]
		if a.samples() < uint64(cfg.WindowMinSamples) || b.samples() < uint64(cfg.WindowMinSamples) {
			continue // inconclusive: streak unchanged
		}
		r, d := abWindowBad(cfg, a, b)
		if r == "" {
			st.Streak = 0
			continue
		}
		st.Streak++
		st.Reason, st.Detail = r, d
	}
	if cum[0].samples() < uint64(cfg.MinSamples) || cum[1].samples() < uint64(cfg.MinSamples) {
		return st, abVerdict{Status: "insufficient_samples"}
	}
	if now.Before(time.Unix(cfg.Started, 0).Add(cfg.MinRuntime)) {
		return st, abVerdict{Status: "min_runtime"}
	}
	if st.Streak >= cfg.Windows {
		return st, abVerdict{Abort: true, Reason: st.Reason, Detail: st.Detail, Status: "abort"}
	}
	return st, abVerdict{Status: "ok"}
}

// abWindowBad applies the two §1.8 rules to one judged window and returns
// the rule that fired ("" if neither).
func abWindowBad(cfg *abConfig, a, b *abCounters) (string, string) {
	errA, errB := a.errRate(), b.errRate()
	if errB > errA+cfg.ErrDelta/100 && errB >= cfg.ErrRatio*errA {
		return "errors", fmt.Sprintf("error rate B %.2f%% vs A %.2f%%", errB*100, errA*100)
	}
	p95A, p95B := a.percentile(0.95), b.percentile(0.95)
	slack := float64(cfg.P95Slack) / float64(time.Millisecond)
	if p95B > cfg.P95Ratio*p95A+slack {
		return "latency", fmt.Sprintf("p95 B %.0fms vs A %.0fms", p95B, p95A)
	}
	return "", ""
}

// runABEvaluator ticks every abEvalInterval until ctx is done.
func (r *Router) runABEvaluator(ctx context.Context) {
	t := time.NewTicker(abEvalInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.abTick(r.clock())
		}
	}
}

func (r *Router) abTick(now time.Time) {
	for _, run := range r.attachedRuns() {
		run.evaluate(now)
	}
}

func (r *Router) attachedRuns() []*abRun {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*abRun
	for _, run := range r.abRuns {
		run.mu.Lock()
		if run.attached {
			out = append(out, run)
		}
		run.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].service < out[j].service })
	return out
}

// reconcileAB wires each test-bearing group to its persistent run. Called
// from Router.Set with r.mu held, before r.groups is published.
func (r *Router) reconcileAB(groups []*RouteGroup) {
	if r.abRuns == nil {
		r.abRuns = map[string]*abRun{}
	}
	now := r.clock()
	resolved := map[string]*abConfig{}
	bLocal := map[string]int{}
	for _, g := range groups {
		if g.abCfg == nil {
			continue
		}
		for _, b := range g.Backends {
			if b.Variant == abVariantB && !b.Learned {
				bLocal[g.Service]++
			}
		}
	}
	for _, run := range r.abRuns {
		run.mu.Lock()
		run.attached = false
		run.mu.Unlock()
	}
	for _, g := range groups {
		if g.abCfg == nil {
			g.ab = nil
			continue
		}
		svc := g.Service
		run := r.abRuns[svc]
		if run == nil || run.id != g.abCfg.ID {
			run = newABRun(svc, g.abCfg.ID, now)
			r.abRuns[svc] = run
		}
		cfg, ok := resolved[svc]
		if !ok {
			c := *g.abCfg
			if c.Started == 0 {
				// An invalid started label (e.g. past ±1d) keeps whatever this
				// run already resolved, so epoch — which defaults to it — and
				// the window ring stay put; firstSeen only for a fresh run.
				c.Started = run.firstSeen
				run.mu.Lock()
				if run.cfg != nil {
					c.Started = run.cfg.Started
				}
				run.mu.Unlock()
			}
			if c.Epoch == 0 {
				c.Epoch = c.Started
			}
			if c.PhaseAt == 0 {
				c.PhaseAt = c.Started
			}
			cfg = &c
			resolved[svc] = cfg
			run.mu.Lock()
			if run.cfg != nil && run.cfg.Epoch != cfg.Epoch {
				// A split/groups change bumps epoch: warm-up and the window
				// ring restart; cumulative counters and pins carry on.
				run.resetRing()
				run.warm = [2]abCounters{}
			}
			run.cfg = cfg
			run.attached = true
			run.bLocal = bLocal[svc]
			run.syncInterval = r.peerSyncInterval
			run.clock = r.clock
			run.mu.Unlock()
		}
		g.abCfg = cfg
		g.ab = run
	}
}

// ---- Assignment (§1.3) ----

// abDecision is ServeHTTP's per-request A/B outcome, consumed by
// abProxyToGroup.
type abDecision struct {
	run      *abRun
	cfg      *abConfig
	variant  string // "A" or "B": the session's variant
	failover string // variant to fail over to, or "" for none
	// failoverFrom is set on a peer's authenticated failover hop: the
	// pinned variant the forwarder failed over from. variant is then the
	// target, and the request is recorded as failover_<failoverFrom>.
	failoverFrom string
	excluded     bool
	static       bool
	record       bool
}

func abHash(id, value string) uint64 {
	sum := sha256.Sum256([]byte(id + "\x00" + value))
	return binary.BigEndian.Uint64(sum[:8])
}

func abSplitVariant(cfg *abConfig, value string) string {
	if abHash(cfg.ID, value)%10000 < uint64(cfg.Split)*100 {
		return abVariantB
	}
	return abVariantA
}

func abRandomVariant(cfg *abConfig) string {
	if mrand.IntN(10000) < cfg.Split*100 {
		return abVariantB
	}
	return abVariantA
}

func abCookieSuffix(service string) string {
	sum := sha256.Sum256([]byte(service))
	return hex.EncodeToString(sum[:])[:8]
}

func abPinCookieName(service string) string { return "ab_v_" + abCookieSuffix(service) }
func abJSCookieName(service string) string  { return "ab_vjs_" + abCookieSuffix(service) }

type abPin struct {
	id, variant, src, nonce string
	issued, lastSeen        int64
}

func (p abPin) String() string {
	return p.id + "." + p.variant + "." + p.src + "." + p.nonce + "." + strconv.FormatInt(p.issued, 10) + "." + strconv.FormatInt(p.lastSeen, 10)
}

// parseABPin strictly parses a pin and applies every §1.3 validity rule.
func parseABPin(v string, cfg *abConfig, now time.Time) (abPin, bool) {
	if len(v) > abPinMaxLen {
		return abPin{}, false
	}
	m := abPinRe.FindStringSubmatch(v)
	if m == nil {
		return abPin{}, false
	}
	issued, err1 := strconv.ParseInt(m[5], 10, 64)
	last, err2 := strconv.ParseInt(m[6], 10, 64)
	if err1 != nil || err2 != nil {
		return abPin{}, false
	}
	p := abPin{id: m[1], variant: m[2], src: m[3], nonce: m[4], issued: issued, lastSeen: last}
	n := now.Unix()
	if p.id != cfg.ID ||
		issued > last ||
		last > n+int64(abPinSkew/time.Second) ||
		n-last >= int64(cfg.SessionIdle/time.Second) ||
		n-issued >= int64(cfg.MaxSession/time.Second) {
		return abPin{}, false
	}
	return p, true
}

func randHex(nbytes int) string {
	b := make([]byte, nbytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// setABCookie queues one of the proxy's own A/B cookies, replacing any
// earlier value for the same name queued in this response and leaving every
// other Set-Cookie untouched — the setStickyCookie pattern.
func setABCookie(w http.ResponseWriter, c *http.Cookie) {
	existing := w.Header()["Set-Cookie"]
	w.Header().Del("Set-Cookie")
	for _, v := range existing {
		if !strings.HasPrefix(v, c.Name+"=") {
			w.Header().Add("Set-Cookie", v)
		}
	}
	http.SetCookie(w, c)
}

func (d *abDecision) setPin(w http.ResponseWriter, p abPin) {
	setABCookie(w, &http.Cookie{
		Name: abPinCookieName(d.run.service), Value: p.String(), Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	if d.cfg.CookieJS {
		setABCookie(w, &http.Cookie{
			Name: abJSCookieName(d.run.service), Value: p.variant, Path: "/",
			Secure: true, SameSite: http.SameSiteLaxMode,
		})
	}
}

func hasPrefixAny(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// cutQueryOverride removes every pm_variant segment from a raw query,
// leaving all other segments byte-for-byte, and returns the first value.
func cutQueryOverride(raw string) (val string, rest string, found bool) {
	if raw == "" {
		return "", raw, false
	}
	segs := strings.Split(raw, "&")
	kept := segs[:0]
	for _, s := range segs {
		k, v, _ := strings.Cut(s, "=")
		if k == abQueryOverride {
			if !found {
				val, found = v, true
			}
			continue
		}
		kept = append(kept, s)
	}
	return val, strings.Join(kept, "&"), found
}

func abValidVariant(v string) (string, bool) {
	switch v {
	case abVariantA:
		return abVariantA, true
	case abVariantB:
		return abVariantB, true
	}
	return "", false
}

// abAssign runs the §1.3 precedence for a request to a group with a test.
// It strips the client's X-Variant, queues the proxy's own cookies and the
// Vary header, and returns the decision. Never logs request values.
func (r *Router) abAssign(w http.ResponseWriter, req *http.Request, g *RouteGroup, origPath string, hopped, authHop, failoverHop bool) *abDecision {
	cfg, run := g.abCfg, g.ab
	now := r.clock()
	phase := run.phase(cfg)
	frozen := abFrozen(phase)
	incoming := req.Header.Get(abVariantHeader)
	req.Header.Del(abVariantHeader)

	d := &abDecision{run: run, cfg: cfg, static: hasPrefixAny(origPath, cfg.Static)}
	finish := func(v string) *abDecision {
		d.variant = v
		if v == abVariantB {
			d.failover = abVariantA
		} else if phase == abPhasePromoting {
			d.failover = abVariantB
		}
		if !d.static && !d.excluded {
			switch cfg.Assign {
			case "cookie":
				w.Header().Add("Vary", "Cookie")
			case "header":
				w.Header().Add("Vary", cfg.Header)
			}
		}
		return d
	}

	// 1. Excluded path.
	if hasPrefixAny(origPath, cfg.Exclude) {
		d.excluded = true
		return finish(abVariantA)
	}
	// 2. Authenticated peer hop with a well-formed variant. A failover hop
	// serves only the other variant (the forwarder already found none of
	// the pinned one): no second failover, and hopped means no re-forward.
	if authHop {
		if v, ok := abValidVariant(incoming); ok {
			d.record = !frozen
			if failoverHop {
				finish(abOther(v))
				d.failover, d.failoverFrom = "", v
				return d
			}
			return finish(v)
		}
	}
	canSetCookie := !hopped && !d.static
	// 3. Valid pin for this test.
	pinName := abPinCookieName(run.service)
	if c, err := req.Cookie(pinName); err == nil {
		if p, ok := parseABPin(c.Value, cfg, now); ok {
			// src=q (QA override) pins are routed but never counted (§1.3).
			d.record = !frozen && !hopped && p.src != "q"
			if !hopped {
				run.notePinned(abIdx(p.variant), p.nonce, now, frozen)
			}
			if canSetCookie && now.Unix()-p.lastSeen >= int64(cfg.PinRefresh/time.Second) {
				p.lastSeen = now.Unix()
				d.setPin(w, p)
			}
			return finish(p.variant)
		}
	}
	newPin := func(v, src string) *abDecision {
		if cfg.Assign == "cookie" && canSetCookie {
			d.setPin(w, abPin{id: cfg.ID, variant: v, src: src, nonce: randHex(4), issued: now.Unix(), lastSeen: now.Unix()})
		}
		return finish(v)
	}
	// 4. QA override, opt-in by label.
	if cfg.Override {
		if val, rest, found := cutQueryOverride(req.URL.RawQuery); found {
			req.URL.RawQuery = rest
			if v, ok := abValidVariant(val); ok {
				return newPin(v, "q")
			}
		}
	}
	d.record = !frozen && !hopped
	// Phase overrides steps 5-9 for new sessions only; a valid pin (step 3)
	// already kept its variant above (§1.10).
	switch phase {
	case abPhaseAborted, abPhaseDiscarding:
		return newPin(abVariantA, "n")
	case abPhasePromoting:
		return newPin(abVariantB, "n")
	}
	switch cfg.Assign {
	case "header":
		// 5. Header mode: missing header → A.
		if v := req.Header.Get(cfg.Header); v != "" {
			return finish(abSplitVariant(cfg, v))
		}
		return finish(abVariantA)
	case "random":
		// 6. Per request.
		return finish(abRandomVariant(cfg))
	}
	// 7. Mapped group.
	if c, err := req.Cookie(cfg.GroupCookie); err == nil && abGroupNameRe.MatchString(c.Value) {
		if v, ok := cfg.Groups[c.Value]; ok {
			return newPin(v, "g")
		}
	}
	// 8. Account id.
	if c, err := req.Cookie(cfg.UIDCookie); err == nil && abUIDRe.MatchString(c.Value) {
		return newPin(abSplitVariant(cfg, c.Value), "u")
	}
	// 9. Signed out.
	if !cfg.Anon {
		return newPin(abRandomVariant(cfg), "n")
	}
	anon := ""
	if c, err := req.Cookie(abAnonCookie); err == nil && abAnonRe.MatchString(c.Value) {
		anon = c.Value
	} else if canSetCookie {
		anon = randHex(8)
		setABCookie(w, &http.Cookie{
			Name: abAnonCookie, Value: anon, Path: "/", MaxAge: int(abAnonMaxAge / time.Second),
			HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		})
	}
	if anon == "" {
		return newPin(abRandomVariant(cfg), "n")
	}
	return newPin(abSplitVariant(cfg, anon), "n")
}

// ---- Dispatch (§1.4, §1.5, §1.7) ----

// abStatusWriter captures the final status the client got, for recording.
type abStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *abStatusWriter) WriteHeader(code int) {
	if code >= 200 && w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *abStatusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *abStatusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *abStatusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("proxy: underlying ResponseWriter does not support hijacking")
}

func (w *abStatusWriter) SetBackend(s string) {
	if setter, ok := w.ResponseWriter.(interface{ SetBackend(string) }); ok {
		setter.SetBackend(s)
	}
}

// abStaticWriter wraps a static asset's first attempt with its own header
// map so a 404 can be swallowed without anything reaching the client (or a
// cacheRecorder underneath); any other status is copied through.
type abStaticWriter struct {
	http.ResponseWriter
	h         http.Header
	forwarded bool
	swallowed bool
}

func (w *abStaticWriter) Header() http.Header { return w.h }

func (w *abStaticWriter) WriteHeader(code int) {
	if w.forwarded || w.swallowed {
		return
	}
	if code < 200 {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if code == http.StatusNotFound {
		w.swallowed = true
		return
	}
	dst := w.ResponseWriter.Header()
	for k, vs := range w.h {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	w.forwarded = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *abStaticWriter) Write(p []byte) (int, error) {
	if !w.forwarded && !w.swallowed {
		w.WriteHeader(http.StatusOK)
	}
	if w.swallowed {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

func (w *abStaticWriter) Flush() {
	if !w.forwarded {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *abStaticWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("proxy: underlying ResponseWriter does not support hijacking")
}

func (w *abStaticWriter) SetBackend(s string) {
	if setter, ok := w.ResponseWriter.(interface{ SetBackend(string) }); ok {
		setter.SetBackend(s)
	}
}

// abPick is pickHealthy/pickAny restricted to the session's variant, with
// the §1.7 failover target tried before panic mode: healthy own, healthy
// failover, any own, any failover.
func (g *RouteGroup) abPick(tried map[*Backend]bool, allowPeer bool, own, failover, reqHost string) *Backend {
	if b := g.pickHealthy(tried, allowPeer, own); b != nil {
		return b
	}
	if failover != "" {
		if b := g.pickHealthy(tried, allowPeer, failover); b != nil {
			return b
		}
	}
	if b := g.pickAny(tried, allowPeer, own); b != nil {
		g.logPanicOnce(reqHost)
		return b
	}
	if failover != "" {
		if b := g.pickAny(tried, allowPeer, failover); b != nil {
			g.logPanicOnce(reqHost)
			return b
		}
	}
	return nil
}

// abProxyToGroup is proxyToGroup for a group with a test: variant-filtered
// picks, sticky only within the variant, X-Variant from constants before
// every attempt, the static 404 retry, and recording.
func (r *Router) abProxyToGroup(w http.ResponseWriter, req *http.Request, group *RouteGroup, reqHost string, hopped bool, stickyPin string, d *abDecision) {
	start := time.Now()
	req = req.WithContext(context.WithValue(req.Context(), abStampedKey{}, true))
	sw := &abStatusWriter{ResponseWriter: w}
	isGetHead := req.Method == http.MethodGet || req.Method == http.MethodHead
	staticRetry := d.static && isGetHead && group.hasVariant(abOther(d.variant))
	tried := map[*Backend]bool{}
	var last *Backend
	for attempt := 0; attempt < maxRetries; attempt++ {
		var b *Backend
		if stickyPin != "" {
			if pinned := group.backendByStickyID(stickyPin); pinned != nil && pinned.healthy() && (!pinned.Learned || group.Spread) && pinned.variantName() == d.variant {
				b = pinned
			}
			stickyPin = ""
		}
		if b == nil {
			b = group.abPick(tried, !hopped, d.variant, d.failover, reqHost)
		}
		if b == nil {
			break
		}
		tried[b] = true
		last = b
		if setter, ok := w.(interface{ SetBackend(string) }); ok {
			setter.SetBackend(b.URL)
		}
		if group.Sticky && !hopped {
			setStickyCookie(w, group, b.stickyID)
		}
		// A hop to a peer running this test carries the session's variant.
		// Sent to the peer's other pool (failover), it also carries
		// abFailoverHeader so the peer serves that pool and records the
		// request once, as failover. Any other peer (older binary, no or a
		// different test) gets the served variant and no flag, as in PR1.
		req.Header.Set(abVariantHeader, b.variantName())
		req.Header.Del(abFailoverHeader)
		if d.peerRuns(b) && b.variantName() != d.variant {
			req.Header.Set(abVariantHeader, d.variant)
			req.Header.Set(abFailoverHeader, "1")
		}
		if staticRetry {
			stw := &abStaticWriter{ResponseWriter: sw, h: http.Header{}}
			if !tryProxy(stw, req, b) {
				if b.Learned {
					d.run.addHopError()
				}
				continue
			}
			if stw.swallowed {
				r.abStaticFallback(sw, req, group, reqHost, hopped, d, tried, stw.h)
				return
			}
		} else if !tryProxy(sw, req, b) {
			if b.Learned {
				d.run.addHopError()
			}
			continue
		}
		r.abRecordServed(d, b, sw.status, time.Since(start))
		return
	}
	if d.record && last != nil && !last.Learned {
		d.run.record(abIdx(d.recordVariant()), abTransport, 0, 0, r.clock())
	}
	log.Printf("proxy: group %q (host %s) has no healthy backends — serving 503", group.Service, reqHost)
	serveUnavailable(w, http.StatusServiceUnavailable, reqHost, "Service unavailable at this time, try again later.")
}

// recordVariant is the variant a request is counted under: the pinned one,
// which on a failover hop is failoverFrom rather than the served variant.
func (d *abDecision) recordVariant() string {
	if d.failoverFrom != "" {
		return d.failoverFrom
	}
	return d.variant
}

// peerRuns reports whether b is a peer verified to run this same test, and
// so records what it serves (see Backend.peerTestID).
func (d *abDecision) peerRuns(b *Backend) bool {
	return b.Learned && b.peerTestID == d.run.id
}

// abRecordServed records a request served by a LOCAL backend. One served by
// a peer running the same test is recorded by that peer — including a
// failover hop, which the peer counts as failover_<pinned> — so each request
// is counted exactly once mesh-wide. A failover to any other peer cannot be
// recorded there, so it is counted here, as in PR1.
func (r *Router) abRecordServed(d *abDecision, b *Backend, status int, latency time.Duration) {
	if b.Learned && status >= 500 {
		d.run.addHopError()
	}
	if d.peerRuns(b) || !d.record || status == 0 {
		return
	}
	now := r.clock()
	if d.failoverFrom != "" || b.variantName() != d.variant {
		d.run.record(abIdx(d.recordVariant()), abFailover, status, latency, now)
		return
	}
	if b.Learned {
		return
	}
	d.run.record(abIdx(d.variant), abServed, status, latency, now)
}

// abStaticFallback retries a swallowed static 404 once on the other
// variant, unwrapped and never recorded. With nowhere to retry it replays
// the 404 itself.
func (r *Router) abStaticFallback(w http.ResponseWriter, req *http.Request, group *RouteGroup, reqHost string, hopped bool, d *abDecision, tried map[*Backend]bool, h404 http.Header) {
	other := abOther(d.variant)
	b := group.pickHealthy(tried, !hopped, other)
	if b == nil {
		b = group.pickAny(tried, !hopped, other)
	}
	if b != nil {
		d.run.addStaticFallback()
		if setter, ok := w.(interface{ SetBackend(string) }); ok {
			setter.SetBackend(b.URL)
		}
		req.Header.Set(abVariantHeader, b.variantName())
		req.Header.Del(abFailoverHeader)
		if tryProxy(w, req, b) {
			return
		}
	}
	dst := w.Header()
	for k, vs := range h404 {
		if k == "Content-Length" {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
	w.WriteHeader(http.StatusNotFound)
}

// ---- GET /ab (§3) ----

type abCountersReport struct {
	Requests  uint64   `json:"requests"`
	Err5xx    uint64   `json:"err5xx"`
	Transport uint64   `json:"transport"`
	Failover  uint64   `json:"failover"`
	ErrorRate float64  `json:"error_rate"`
	P50Ms     float64  `json:"p50_ms"`
	P95Ms     float64  `json:"p95_ms"`
	Hist      []uint64 `json:"hist"`
}

func abCountersOut(c *abCounters) abCountersReport {
	return abCountersReport{
		Requests: c.Requests, Err5xx: c.Err5xx, Transport: c.Transport, Failover: c.Failover,
		ErrorRate: c.errRate(), P50Ms: c.percentile(0.5), P95Ms: c.percentile(0.95),
		Hist: append([]uint64(nil), c.Hist[:]...),
	}
}

type abPair[T any] struct {
	A T `json:"A"`
	B T `json:"B"`
}

type abPinnedReport struct {
	ActiveSessions int   `json:"active_sessions"`
	LastSeen       int64 `json:"last_seen"`
}

type abWindowReport struct {
	Index int64                    `json:"index"`
	Start int64                    `json:"start"`
	V     abPair[abCountersReport] `json:"variants"`
}

type abJudgeReport struct {
	Status string `json:"status"`
	Streak int    `json:"streak"`
}

type abExperimentReport struct {
	Service         string                   `json:"service"`
	ID              string                   `json:"id"`
	Phase           string                   `json:"phase"`
	PhaseAt         int64                    `json:"phase_at"`
	Config          map[string]string        `json:"config"`
	Started         int64                    `json:"started"`
	Epoch           int64                    `json:"epoch"`
	Abort           *abAbortInfo             `json:"abort,omitempty"`
	BBackendsLocal  int                      `json:"b_backends_local"`
	BBackendsPeer   int                      `json:"b_backends_peer"`
	Cumulative      abPair[abCountersReport] `json:"cumulative"`
	Windows         []abWindowReport         `json:"windows"`
	Frozen          bool                     `json:"frozen"`
	Warmup          abPair[abCountersReport] `json:"warmup"`
	Pinned          abPair[abPinnedReport]   `json:"pinned"`
	Failover        abPair[uint64]           `json:"failover"`
	Draining        abPair[uint64]           `json:"draining"`
	HopErrors       uint64                   `json:"hop_errors"`
	StaticFallbacks uint64                   `json:"static_fallbacks"`
	Judge           abJudgeReport            `json:"judge"`
	Peers           []abPeerReport           `json:"peers"`
	// Merged is what judge sees: this proxy plus every fresh, aligned peer.
	Merged abMergedReport `json:"merged"`
}

type abReport struct {
	HistBoundsMs []float64            `json:"hist_bounds_ms"`
	Experiments  []abExperimentReport `json:"experiments"`
}

// ABReport is the read-only /ab payload; service "" lists every attached
// test. Exposes counts only — never pin nonces or any request value.
func (r *Router) ABReport(service string) abReport {
	out := abReport{HistBoundsMs: abHistBounds, Experiments: []abExperimentReport{}}
	for _, run := range r.attachedRuns() {
		if service != "" && run.service != service {
			continue
		}
		out.Experiments = append(out.Experiments, run.report())
	}
	return out
}

func (run *abRun) report() abExperimentReport {
	run.mu.Lock()
	defer run.mu.Unlock()
	cfg := run.cfg
	phase := run.phase(cfg)
	abort, phaseAt := run.abortLocked()
	rep := abExperimentReport{
		Service: run.service, ID: run.id, Phase: phase, PhaseAt: phaseAt, Abort: abort,
		Config: cfg.labels(), Started: cfg.Started, Epoch: cfg.Epoch,
		BBackendsLocal: run.bLocal,
		Cumulative:     abPair[abCountersReport]{abCountersOut(&run.cum[0]), abCountersOut(&run.cum[1])},
		Windows:        []abWindowReport{},
		Frozen:         abFrozen(phase),
		Warmup:         abPair[abCountersReport]{abCountersOut(&run.warm[0]), abCountersOut(&run.warm[1])},
		Pinned: abPair[abPinnedReport]{
			A: abPinnedReport{len(run.pinnedNonces[0]), run.pinnedLast[0]},
			B: abPinnedReport{len(run.pinnedNonces[1]), run.pinnedLast[1]},
		},
		Failover:        abPair[uint64]{run.failover[0], run.failover[1]},
		Draining:        abPair[uint64]{run.draining[0], run.draining[1]},
		HopErrors:       run.hopErrors,
		StaticFallbacks: run.staticFallbacks,
		Judge:           abJudgeReport{Status: run.verdict.Status, Streak: run.js.Streak},
		Peers:           []abPeerReport{},
	}
	windowsOut := func(ws []abWindow) []abWindowReport {
		out := []abWindowReport{}
		for _, w := range ws {
			start := time.Unix(cfg.Epoch, 0).Add(cfg.Warmup).Add(time.Duration(w.Index) * cfg.Window).Unix()
			out = append(out, abWindowReport{
				Index: w.Index, Start: start,
				V: abPair[abCountersReport]{abCountersOut(&w.V[0]), abCountersOut(&w.V[1])},
			})
		}
		return out
	}
	own := run.summaryLocked()
	rep.Windows = windowsOut(own.Windows)
	now := time.Now()
	if run.clock != nil {
		now = run.clock()
	}
	fresh, all := run.freshPeersLocked(now)
	isFresh := map[string]bool{}
	for _, p := range fresh {
		isFresh[p] = true
	}
	for _, p := range all {
		s := run.peers[p]
		aligned := run.abAligned(s.exp)
		rep.Peers = append(rep.Peers, abPeerReport{
			Peer: p, Fresh: isFresh[p], AgeS: int64(now.Sub(s.at) / time.Second), Phase: s.exp.phase,
			Latched: s.exp.latched(), BBackends: s.exp.bBackends, Aligned: aligned,
		})
		if isFresh[p] {
			rep.BBackendsPeer += s.exp.bBackends
		}
	}
	peerSums := run.peerSummariesLocked(now)
	merged := abMergeSummaries(append([]abSummary{own}, peerSums...))
	rep.Merged = abMergedReport{
		Peers:      len(peerSums),
		Cumulative: abPair[abCountersReport]{abCountersOut(&merged.Cumulative[0]), abCountersOut(&merged.Cumulative[1])},
		Windows:    windowsOut(merged.Windows),
	}
	return rep
}

// abHandler serves GET /ab[?service=] on the metrics port. Read-only.
func abHandler(report func(service string) abReport) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report(req.URL.Query().Get("service")))
	}
}
