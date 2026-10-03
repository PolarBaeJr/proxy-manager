package main

// A/B testing, dashboard side (docs/AB_TESTING_PLAN.md §1.1, §1.10–§1.12,
// §2, §3). This file holds the label names, the start-request validation and
// the JSON store. The bounds below mirror cmd/proxy/abtest.go's
// parseABConfig on purpose — nothing is shared across the two binaries, so
// a change to one side's bounds must be made on the other side too (and
// abtest_test.go pins every one of them).

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	labelABPrefix           = "proxy.ab."
	labelABVariant          = "proxy.ab.variant"
	labelABID               = "proxy.ab.id"
	labelABAssign           = "proxy.ab.assign"
	labelABSplit            = "proxy.ab.split"
	labelABGroups           = "proxy.ab.groups"
	labelABUIDCookie        = "proxy.ab.uid_cookie"
	labelABGroupCookie      = "proxy.ab.group_cookie"
	labelABAnon             = "proxy.ab.anon"
	labelABSessionIdle      = "proxy.ab.session_idle"
	labelABPinRefresh       = "proxy.ab.pin_refresh"
	labelABMaxSession       = "proxy.ab.max_session"
	labelABStarted          = "proxy.ab.started"
	labelABEpoch            = "proxy.ab.epoch"
	labelABPhase            = "proxy.ab.phase"
	labelABPhaseAt          = "proxy.ab.phase_at"
	labelABAbortReason      = "proxy.ab.abort_reason"
	labelABExclude          = "proxy.ab.exclude"
	labelABStatic           = "proxy.ab.static"
	labelABCookieJS         = "proxy.ab.cookie_js"
	labelABOverride         = "proxy.ab.override"
	labelABAutoAbort        = "proxy.ab.autoabort"
	labelABMinSamples       = "proxy.ab.min_samples"
	labelABMinRuntime       = "proxy.ab.min_runtime"
	labelABWarmup           = "proxy.ab.warmup"
	labelABWindow           = "proxy.ab.window"
	labelABWindows          = "proxy.ab.windows"
	labelABWindowMinSamples = "proxy.ab.window_min_samples"
	labelABErrDelta         = "proxy.ab.err_delta"
	labelABErrRatio         = "proxy.ab.err_ratio"
	labelABP95Ratio         = "proxy.ab.p95_ratio"
	labelABP95Slack         = "proxy.ab.p95_slack"

	abPhaseRunning    = "running"
	abPhaseAborted    = "aborted"
	abPhasePromoting  = "promoting"
	abPhaseDiscarding = "discarding"

	abPendingPromote = "promote"
	abPendingDiscard = "discard"

	abDefaultSessionIdle = 30 * time.Minute
	abDefaultMaxSession  = 24 * time.Hour
	abMaxPrefixes        = 32
	abMaxPrefixLen       = 256
	abMaxGroups          = 32
	abMaxTokenLen        = 64
	abMaxReplicas        = 10
	abMaxHistory         = 20
)

var (
	abGroupNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	abIDRe        = regexp.MustCompile(`^[a-z0-9]{4,16}$`)
)

// abError carries the HTTP status an A/B operation's failure maps to.
type abError struct {
	code int
	msg  string
}

func (e abError) Error() string { return e.msg }

func abBadRequest(format string, a ...any) error {
	return abError{code: http.StatusBadRequest, msg: fmt.Sprintf(format, a...)}
}

func abConflict(format string, a ...any) error {
	return abError{code: http.StatusConflict, msg: fmt.Sprintf(format, a...)}
}

// errABActive is a guard refusal (§1.12): the action can't run while svc
// has an A/B test. Always a 409.
type errABActive struct {
	Service string
	Hint    string
}

func (e errABActive) Error() string {
	return fmt.Sprintf("%q has an active A/B test — %s", e.Service, e.Hint)
}

// newABID is the test id: 8 hex chars from crypto/rand, which always
// satisfies the proxy's ^[a-z0-9]{4,16}$.
func newABID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newABIDAfter is a fresh id that sorts after old. The proxy routes only
// the smallest id among a service's B replicas (cmd/proxy abScan), so during
// a reset's surge the old id keeps winning until its last replica is gone
// — one switch, onto replicas that all passed their gate, instead of the
// first not-yet-ready new one taking over (and a gate failure handing it
// back).
func newABIDAfter(old string) string {
	for i := 0; i < 64; i++ {
		if id := newABID(); id > old {
			return id
		}
	}
	if len(old) < 16 {
		// A proper prefix sorts first.
		return old + newABID()[:min(8, 16-len(old))]
	}
	return newABID()
}

// ---- Start request (§3) ----

type abThresholds struct {
	AutoAbort        *bool    `json:"autoabort,omitempty"`
	MinSamples       *int     `json:"min_samples,omitempty"`
	MinRuntime       string   `json:"min_runtime,omitempty"`
	Warmup           string   `json:"warmup,omitempty"`
	Window           string   `json:"window,omitempty"`
	Windows          *int     `json:"windows,omitempty"`
	WindowMinSamples *int     `json:"window_min_samples,omitempty"`
	ErrDelta         *float64 `json:"err_delta,omitempty"`
	ErrRatio         *float64 `json:"err_ratio,omitempty"`
	P95Ratio         *float64 `json:"p95_ratio,omitempty"`
	P95Slack         string   `json:"p95_slack,omitempty"`
}

// abStartRequest is the POST /api/services/{name}/ab body. Env exists only
// so that its presence can be refused (§0.1: B is image-only in v1).
type abStartRequest struct {
	Image       string            `json:"image"`
	Split       *int              `json:"split,omitempty"`
	Replicas    int               `json:"replicas"`
	Assign      string            `json:"assign,omitempty"`
	Header      string            `json:"header,omitempty"`
	Groups      map[string]string `json:"groups,omitempty"`
	UIDCookie   string            `json:"uid_cookie,omitempty"`
	GroupCookie string            `json:"group_cookie,omitempty"`
	Anon        *bool             `json:"anon,omitempty"`
	SessionIdle string            `json:"session_idle,omitempty"`
	PinRefresh  string            `json:"pin_refresh,omitempty"`
	MaxSession  string            `json:"max_session,omitempty"`
	Exclude     []string          `json:"exclude,omitempty"`
	Static      []string          `json:"static,omitempty"`
	CookieJS    bool              `json:"cookie_js,omitempty"`
	Override    bool              `json:"override,omitempty"`
	Thresholds  abThresholds      `json:"thresholds"`
	Env         json.RawMessage   `json:"env,omitempty"`
}

// decodeABStart parses a start body strictly: unknown fields are refused,
// and an "env" key of any value (even null or {}) is a 400.
func decodeABStart(body []byte) (abStartRequest, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return abStartRequest{}, abBadRequest("invalid request body")
	}
	if _, ok := probe["env"]; ok {
		return abStartRequest{}, abBadRequest("env is not supported in an A/B test (B is image-only in v1) — build a different image for B")
	}
	var req abStartRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return abStartRequest{}, abBadRequest("invalid request body: %v", err)
	}
	return req, nil
}

// abIsToken mirrors the proxy's isToken: an RFC 7230 token of at most
// abMaxTokenLen bytes (valid for header and cookie names alike).
func abIsToken(s string) bool {
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

// abValidatePrefixes mirrors the proxy's parsePrefixes, plus the two rules
// the label round-trip needs: the proxy splits on "," and trims, so an
// entry with a comma or surrounding whitespace would come back different.
func abValidatePrefixes(field string, ps []string) error {
	if len(ps) > abMaxPrefixes {
		return abBadRequest("%s: at most %d prefixes", field, abMaxPrefixes)
	}
	for _, p := range ps {
		if !strings.HasPrefix(p, "/") || len(p) > abMaxPrefixLen {
			return abBadRequest("%s: each prefix must start with / and be at most %d bytes", field, abMaxPrefixLen)
		}
		if strings.ContainsRune(p, ',') || strings.TrimSpace(p) != p {
			return abBadRequest("%s: prefixes may not contain commas or surrounding whitespace", field)
		}
		for i := 0; i < len(p); i++ {
			if p[i] < 0x20 || p[i] == 0x7f {
				return abBadRequest("%s: prefixes may not contain control characters", field)
			}
		}
	}
	return nil
}

func abValidateGroups(groups map[string]string) error {
	if len(groups) > abMaxGroups {
		return abBadRequest("groups: at most %d entries", abMaxGroups)
	}
	for name, v := range groups {
		if !abGroupNameRe.MatchString(name) {
			return abBadRequest("groups: invalid group name %q (allowed: a-z 0-9 _ -, 1..32)", name)
		}
		if v != "A" && v != "B" {
			return abBadRequest("groups: %q must map to A or B", name)
		}
	}
	return nil
}

// abGroupsLabel renders groups in the proxy's "name:V,name:V" form, sorted
// so identical maps always produce identical labels.
func abGroupsLabel(groups map[string]string) string {
	out := make([]string, 0, len(groups))
	for n, v := range groups {
		out = append(out, n+":"+v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func abParseDuration(field, v string, lo, hi time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d < lo || d > hi {
		return 0, abBadRequest("%s: must be a duration in %s..%s", field, lo, hi)
	}
	return d, nil
}

func abCheckFloat(field string, f, lo, hi float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < lo || f > hi {
		return abBadRequest("%s: must be in %g..%g", field, lo, hi)
	}
	return nil
}

func abCheckInt(field string, n, lo, hi int) error {
	if n < lo || n > hi {
		return abBadRequest("%s: must be in %d..%d", field, lo, hi)
	}
	return nil
}

// abConfigLabels validates req and returns the static part of the B label
// set: only values the caller set (anything left out is the proxy's
// default). The dynamic labels (variant/id/started/epoch/phase/phase_at/
// abort_reason) are added by abRecord.labels.
func abConfigLabels(req abStartRequest) (map[string]string, error) {
	out := map[string]string{}
	if req.Image == "" {
		return nil, abBadRequest("image is required")
	}
	if err := abCheckInt("replicas", req.Replicas, 1, abMaxReplicas); err != nil {
		return nil, err
	}
	if req.Split != nil {
		if err := abCheckInt("split", *req.Split, 0, 100); err != nil {
			return nil, err
		}
		out[labelABSplit] = strconv.Itoa(*req.Split)
	}
	assign := req.Assign
	header := req.Header
	if h, ok := strings.CutPrefix(assign, "header:"); ok {
		assign, header = "header", h
	}
	switch assign {
	case "", "cookie":
		if header != "" {
			return nil, abBadRequest("header is only valid with assign=header")
		}
	case "random":
		if header != "" {
			return nil, abBadRequest("header is only valid with assign=header")
		}
		out[labelABAssign] = "random"
	case "header":
		canon := http.CanonicalHeaderKey(header)
		if !abIsToken(header) || canon == "Cookie" || canon == "Host" {
			return nil, abBadRequest("header: must be a header name token of at most %d bytes, not Cookie or Host", abMaxTokenLen)
		}
		out[labelABAssign] = "header:" + header
	default:
		return nil, abBadRequest("assign: must be cookie, random or header")
	}
	if len(req.Groups) > 0 {
		if err := abValidateGroups(req.Groups); err != nil {
			return nil, err
		}
		out[labelABGroups] = abGroupsLabel(req.Groups)
	}
	for _, kv := range []struct{ field, label, v string }{
		{"uid_cookie", labelABUIDCookie, req.UIDCookie},
		{"group_cookie", labelABGroupCookie, req.GroupCookie},
	} {
		if kv.v == "" {
			continue
		}
		if !abIsToken(kv.v) {
			return nil, abBadRequest("%s: must be a cookie name token of at most %d bytes", kv.field, abMaxTokenLen)
		}
		out[kv.label] = kv.v
	}
	if req.Anon != nil {
		out[labelABAnon] = strconv.FormatBool(*req.Anon)
	}
	idle := abDefaultSessionIdle
	if req.SessionIdle != "" {
		d, err := abParseDuration("session_idle", req.SessionIdle, 5*time.Minute, 24*time.Hour)
		if err != nil {
			return nil, err
		}
		idle = d
		out[labelABSessionIdle] = d.String()
	}
	if req.PinRefresh != "" {
		d, err := abParseDuration("pin_refresh", req.PinRefresh, time.Minute, idle/2)
		if err != nil {
			return nil, err
		}
		out[labelABPinRefresh] = d.String()
	}
	if req.MaxSession != "" {
		d, err := abParseDuration("max_session", req.MaxSession, time.Hour, 7*24*time.Hour)
		if err != nil {
			return nil, err
		}
		out[labelABMaxSession] = d.String()
	}
	if len(req.Exclude) > 0 {
		if err := abValidatePrefixes("exclude", req.Exclude); err != nil {
			return nil, err
		}
		out[labelABExclude] = strings.Join(req.Exclude, ",")
	}
	if len(req.Static) == 1 && req.Static[0] == "none" {
		out[labelABStatic] = "none"
	} else if len(req.Static) > 0 {
		if err := abValidatePrefixes("static", req.Static); err != nil {
			return nil, err
		}
		out[labelABStatic] = strings.Join(req.Static, ",")
	}
	if req.CookieJS {
		out[labelABCookieJS] = "true"
	}
	if req.Override {
		out[labelABOverride] = "true"
	}
	t := req.Thresholds
	if t.AutoAbort != nil {
		out[labelABAutoAbort] = strconv.FormatBool(*t.AutoAbort)
	}
	for _, kv := range []struct {
		field, label string
		v            *int
		lo, hi       int
	}{
		{"thresholds.min_samples", labelABMinSamples, t.MinSamples, 1, 10_000_000},
		{"thresholds.windows", labelABWindows, t.Windows, 1, 12},
		{"thresholds.window_min_samples", labelABWindowMinSamples, t.WindowMinSamples, 1, 1_000_000},
	} {
		if kv.v == nil {
			continue
		}
		if err := abCheckInt(kv.field, *kv.v, kv.lo, kv.hi); err != nil {
			return nil, err
		}
		out[kv.label] = strconv.Itoa(*kv.v)
	}
	for _, kv := range []struct {
		field, label, v string
		lo, hi          time.Duration
	}{
		{"thresholds.min_runtime", labelABMinRuntime, t.MinRuntime, 0, 24 * time.Hour},
		{"thresholds.warmup", labelABWarmup, t.Warmup, 0, time.Hour},
		{"thresholds.window", labelABWindow, t.Window, time.Minute, time.Hour},
		{"thresholds.p95_slack", labelABP95Slack, t.P95Slack, 0, 60 * time.Second},
	} {
		if kv.v == "" {
			continue
		}
		d, err := abParseDuration(kv.field, kv.v, kv.lo, kv.hi)
		if err != nil {
			return nil, err
		}
		out[kv.label] = d.String()
	}
	for _, kv := range []struct {
		field, label string
		v            *float64
		lo, hi       float64
	}{
		{"thresholds.err_delta", labelABErrDelta, t.ErrDelta, 0.1, 100},
		{"thresholds.err_ratio", labelABErrRatio, t.ErrRatio, 1, 100},
		{"thresholds.p95_ratio", labelABP95Ratio, t.P95Ratio, 1, 100},
	} {
		if kv.v == nil {
			continue
		}
		if err := abCheckFloat(kv.field, *kv.v, kv.lo, kv.hi); err != nil {
			return nil, err
		}
		out[kv.label] = strconv.FormatFloat(*kv.v, 'g', -1, 64)
	}
	return out, nil
}

// ---- Store ----

// abRecord is one service's test as the dashboard drives it — the desired
// state. B's labels are derived from it (labels), and Op names an operation
// whose label/container changes are not yet known to be applied, so a
// restarted dashboard (or the next Run tick after a failure) converges.
type abRecord struct {
	ID          string            `json:"id"`
	Image       string            `json:"image"`
	Replicas    int               `json:"replicas"`
	Created     int64             `json:"created"`
	Started     int64             `json:"started"`
	Epoch       int64             `json:"epoch"`
	Phase       string            `json:"phase"`
	PhaseAt     int64             `json:"phase_at"`
	AbortReason string            `json:"abort_reason,omitempty"`
	Config      map[string]string `json:"config"`
	// Pending is the resolution waiting on a drain: promote | discard | "".
	Pending           string `json:"pending,omitempty"`
	EnvPendingVersion uint64 `json:"env_pending_version,omitempty"`
	// PinnedMaxA/B are the latest pin of each variant ever reported for this
	// id by any proxy (abDrain) — monotonic, so a proxy restart that zeroes
	// its own last_seen can't make a variant look idle.
	PinnedMaxA int64 `json:"pinned_max_a,omitempty"`
	PinnedMaxB int64 `json:"pinned_max_b,omitempty"`
	// PeersPromoted are the peers already rolled onto Image by an
	// unfinished finalize_promote (promotePeers), so a retry skips them.
	PeersPromoted []string `json:"peers_promoted,omitempty"`
	// Op is "start", "relabel", "finalize_promote" or "finalize_discard"
	// while that operation hasn't completed; "" once B matches the record.
	Op          string `json:"op,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	LastAttempt int64  `json:"last_attempt,omitempty"`
}

// abHistoryEntry is one finished test (or a reset's previous id).
type abHistoryEntry struct {
	ID      string `json:"id"`
	Image   string `json:"image"`
	Started int64  `json:"started"`
	Ended   int64  `json:"ended"`
	Outcome string `json:"outcome"` // promoted | discarded | reset | start_failed | lost
	Detail  string `json:"detail,omitempty"`
}

type abServiceState struct {
	Current *abRecord        `json:"current,omitempty"`
	History []abHistoryEntry `json:"history,omitempty"`
}

func (r *abRecord) clone() *abRecord {
	cp := *r
	cp.PeersPromoted = append([]string(nil), r.PeersPromoted...)
	cp.Config = make(map[string]string, len(r.Config))
	for k, v := range r.Config {
		cp.Config[k] = v
	}
	return &cp
}

// labels is the complete proxy.ab.* set every B replica carries.
func (r *abRecord) labels() map[string]string {
	out := make(map[string]string, len(r.Config)+8)
	for k, v := range r.Config {
		out[k] = v
	}
	out[labelABVariant] = "B"
	out[labelABID] = r.ID
	out[labelABStarted] = strconv.FormatInt(r.Started, 10)
	out[labelABEpoch] = strconv.FormatInt(r.Epoch, 10)
	out[labelABPhase] = r.Phase
	out[labelABPhaseAt] = strconv.FormatInt(r.PhaseAt, 10)
	if r.AbortReason != "" {
		out[labelABAbortReason] = r.AbortReason
	}
	return out
}

func (r *abRecord) duration(label string, def time.Duration) time.Duration {
	if v, ok := r.Config[label]; ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func (r *abRecord) pinnedMax(v string) int64 {
	if v == "B" {
		return r.PinnedMaxB
	}
	return r.PinnedMaxA
}

func (r *abRecord) sessionIdle() time.Duration {
	return r.duration(labelABSessionIdle, abDefaultSessionIdle)
}

func (r *abRecord) maxSession() time.Duration {
	return r.duration(labelABMaxSession, abDefaultMaxSession)
}

// abStore persists every service's A/B state to one JSON file, written
// atomically (writeFileAtomic) after every change.
type abStore struct {
	mu   sync.Mutex
	path string
	data map[string]*abServiceState
}

func loadABStore(path string) (*abStore, error) {
	s := &abStore{path: path, data: map[string]*abServiceState{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.data == nil {
		s.data = map[string]*abServiceState{}
	}
	return s, nil
}

func (s *abStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0o600)
}

// get returns a copy of svc's current record.
func (s *abStore) get(svc string) (*abRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.data[svc]
	if st == nil || st.Current == nil {
		return nil, false
	}
	return st.Current.clone(), true
}

func (s *abStore) history(svc string) []abHistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.data[svc]
	if st == nil {
		return []abHistoryEntry{}
	}
	return append([]abHistoryEntry{}, st.History...)
}

func (s *abStore) put(svc string, rec *abRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.data[svc]
	if st == nil {
		st = &abServiceState{}
		s.data[svc] = st
	}
	st.Current = rec.clone()
	return s.saveLocked()
}

// finish clears svc's record and appends h to its history in one write.
func (s *abStore) finish(svc string, h abHistoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.data[svc]
	if st == nil {
		st = &abServiceState{}
		s.data[svc] = st
	}
	st.Current = nil
	s.appendHistoryLocked(st, h)
	return s.saveLocked()
}

func (s *abStore) appendHistory(svc string, h abHistoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.data[svc]
	if st == nil {
		st = &abServiceState{}
		s.data[svc] = st
	}
	s.appendHistoryLocked(st, h)
	return s.saveLocked()
}

func (s *abStore) appendHistoryLocked(st *abServiceState, h abHistoryEntry) {
	st.History = append(st.History, h)
	if len(st.History) > abMaxHistory {
		st.History = st.History[len(st.History)-abMaxHistory:]
	}
}

// services lists every service with a current record, sorted.
func (s *abStore) services() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for svc, st := range s.data {
		if st != nil && st.Current != nil {
			out = append(out, svc)
		}
	}
	sort.Strings(out)
	return out
}
