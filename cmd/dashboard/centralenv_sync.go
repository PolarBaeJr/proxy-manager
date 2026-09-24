// Central per-service env: PROPAGATION.
//
// When a service's central env changes on its origin, every replica on every
// host has to be recreated from the new version — without an operator
// visiting each host. envSyncManager owns that: one background job per
// service at a time, rolling the origin's own replicas first (health-gated,
// surge-of-one, through the same rollingOpManager every other rolling
// replace goes through), then telling each capable peer in turn to pull the
// new version and roll its own. A peer never receives the env in the
// notification itself — it fetches it from the origin, resolved for its own
// identity.
//
// Failure policy: a new origin replica failing its health gate reverts the
// origin's store (a NEW version with the previous content) and no peer is
// ever told about the bad version; a revert that itself fails leaves the
// service "degraded" and nothing further is attempted automatically. A peer
// that fails rolls back to the env it was running and the job ends
// "partial" — the origin is not reverted for one host's failure.
//
// Nothing here ever carries a value: job state, errors, audit targets and
// logs name services, hosts, keys and versions only (scrubEnvValues is the
// backstop for error strings that come from Docker).
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	envSyncStatusPending          = "pending"
	envSyncStatusRunning          = "running"
	envSyncStatusConverged        = "converged"
	envSyncStatusPartial          = "partial"
	envSyncStatusFailed           = "failed"
	envSyncStatusFailedReverted   = "failed_reverted"
	envSyncStatusFailedRolledBack = "failed_rolled_back"
	envSyncStatusDegraded         = "degraded"

	envSyncHostConverged   = "converged"
	envSyncHostUnsupported = "unsupported"
	envSyncHostUnreachable = "unreachable"
	envSyncHostFailed      = "failed"
	envSyncHostRolledBack  = "failed_rolled_back"

	envSyncRoleOrigin = "origin"
	envSyncRolePeer   = "peer"

	// envSyncAuditUser is the audit "user" for everything the background
	// job does on its own — audit(nil, "", ...) would otherwise fall
	// through to principalFrom(nil).
	envSyncAuditUser = "central-env"
	// envSyncClaimOwner is the propagation job's serviceClaims owner.
	envSyncClaimOwner = "central-env"
)

// Package vars only so tests can shrink them — same seam as
// rollingOpTimeout/replaceSettleDelay.
var (
	// envSyncRetryInterval is how long a job waits before re-checking a
	// service that another rollout / rolling replace currently owns.
	envSyncRetryInterval = 5 * time.Second
	// envSyncPeerPollInterval / envSyncPeerTimeout bound the origin's wait
	// for one peer's own job, polled over /peer/central-env/{svc}/status.
	envSyncPeerPollInterval = 2 * time.Second
	envSyncPeerTimeout      = 20 * time.Minute
	// envSyncReconcileInterval is the reconcile loop's period.
	envSyncReconcileInterval = 60 * time.Second
)

// envSyncHostResult is one host's outcome within a job.
type envSyncHostResult struct {
	Host    string `json:"host"`
	Status  string `json:"status"`
	Version uint64 `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// envSyncJob is one service's in-flight (or most recent) propagation.
type envSyncJob struct {
	Service   string              `json:"service"`
	Role      string              `json:"role"`
	Target    uint64              `json:"target_version"`
	Phase     string              `json:"phase"`
	Status    string              `json:"status"`
	Hosts     []envSyncHostResult `json:"hosts,omitempty"`
	Warnings  []string            `json:"warnings,omitempty"`
	LastError string              `json:"last_error,omitempty"`
	StartedAt time.Time           `json:"started_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// envSyncFailure is the most recent job that did NOT converge, kept apart
// from the job slot so the job that follows (e.g. the one propagating a
// revert) can't make it disappear from view.
type envSyncFailure struct {
	Version   uint64              `json:"version"`
	Status    string              `json:"status"`
	Hosts     []envSyncHostResult `json:"hosts,omitempty"`
	LastError string              `json:"last_error,omitempty"`
	At        time.Time           `json:"at"`
}

func envSyncFailed(status string) bool {
	switch status {
	case envSyncStatusFailed, envSyncStatusFailedReverted, envSyncStatusFailedRolledBack, envSyncStatusPartial, envSyncStatusDegraded:
		return true
	}
	return false
}

func envSyncTerminal(status string) bool {
	switch status {
	case envSyncStatusPending, envSyncStatusRunning:
		return false
	}
	return true
}

func copyEnvSyncJob(j *envSyncJob) *envSyncJob {
	cp := *j
	// Same lesson as rollingOpManager.get: the struct copy alone still
	// shares these slices' backing arrays with the job goroutine's appends.
	cp.Hosts = append([]envSyncHostResult(nil), j.Hosts...)
	cp.Warnings = append([]string(nil), j.Warnings...)
	return &cp
}

// envSyncPeerTarget is what a non-origin host was last asked to converge to.
type envSyncPeerTarget struct {
	origin  string
	version uint64
}

// envSyncResolvedMark is the reconcile loop's secret-rotation detector: a
// hash of svc's fully RESOLVED effective env at one store version. Kept in
// memory only — never persisted, never logged.
type envSyncResolvedMark struct {
	version uint64
	hash    string
}

type envSyncManager struct {
	dc       *dockerClient
	ce       *centralEnv
	rm       *rolloutManager
	rom      *rollingOpManager
	registry *PeerRegistry
	secret   string
	proxyURL string
	client   *http.Client

	mu      sync.Mutex
	jobs    map[string]*envSyncJob
	running map[string]bool
	// dirty coalesces requests that arrive while svc's job runs: however
	// many there were, exactly one more job runs after it, for whatever is
	// newest by then — intermediate versions are never rolled.
	dirty      map[string]bool
	peerTarget map[string]envSyncPeerTarget
	// peerFailed (origin side): svc → peer identity → the version that peer
	// failed (and rolled back) at. Reconcile won't re-notify it until the
	// store moves past that version — otherwise a bad env would spin up a
	// failing replica on that peer every reconcile tick.
	peerFailed map[string]map[string]uint64
	// retry marks an explicit sync (clearFailures): the next origin job
	// asks each peer to forget its own failed version too.
	// (The peer-side twin of peerFailed — a version THIS host failed —
	// lives in the cache, centralEnvCacheEntry.FailedVersion, so it
	// survives a restart and Resolve honors it.)
	retry       map[string]bool
	lastFailure map[string]*envSyncFailure
	resolved    map[string]envSyncResolvedMark
	originSeen  map[string]time.Time
}

func newEnvSyncManager(dc *dockerClient, ce *centralEnv, rm *rolloutManager, rom *rollingOpManager, registry *PeerRegistry, secret, proxyURL string) *envSyncManager {
	return &envSyncManager{
		dc: dc, ce: ce, rm: rm, rom: rom, registry: registry, secret: secret, proxyURL: proxyURL,
		client:      &http.Client{Timeout: 15 * time.Second},
		jobs:        map[string]*envSyncJob{},
		running:     map[string]bool{},
		dirty:       map[string]bool{},
		peerTarget:  map[string]envSyncPeerTarget{},
		peerFailed:  map[string]map[string]uint64{},
		retry:       map[string]bool{},
		lastFailure: map[string]*envSyncFailure{},
		resolved:    map[string]envSyncResolvedMark{},
		originSeen:  map[string]time.Time{},
	}
}

// get returns a deep copy of svc's job.
func (m *envSyncManager) get(svc string) (*envSyncJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[svc]
	if !ok {
		return nil, false
	}
	return copyEnvSyncJob(j), true
}

func (m *envSyncManager) busy(svc string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[svc]
}

func (m *envSyncManager) update(svc string, fn func(*envSyncJob)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[svc]; ok {
		fn(j)
		j.UpdatedAt = time.Now()
	}
}

// request asks for svc (owned by this host's store) to be propagated at its
// current version. Idempotent: while a job runs, any number of requests
// collapse into one follow-up job.
func (m *envSyncManager) request(svc string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startLocked(svc)
}

// requestPeer is request for a service this host does NOT own: converge
// local replicas to at least version from origin.
func (m *envSyncManager) requestPeer(svc, origin string, version uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.peerTarget[svc]; t.origin != origin || version > t.version {
		m.peerTarget[svc] = envSyncPeerTarget{origin: origin, version: version}
	}
	m.startLocked(svc)
}

func (m *envSyncManager) startLocked(svc string) {
	if m.running[svc] {
		m.dirty[svc] = true
		return
	}
	m.running[svc] = true
	go m.loop(svc)
}

func (m *envSyncManager) loop(svc string) {
	for {
		var target uint64
		origin := m.ce.store.Has(svc)
		if origin {
			target = m.runOrigin(svc)
		} else {
			m.runPeer(svc)
		}
		m.mu.Lock()
		again := m.dirty[svc]
		m.dirty[svc] = false
		// Never leave a newer version unrolled (a revert, or an Apply that
		// raced the job's own read): re-read and go once more for the
		// latest. A degraded record stops here — that state is the "don't
		// try again on your own" signal.
		if !again && origin {
			if rec, ok, err := m.ce.store.Get(svc); err == nil && ok && rec.Version > target && rec.State != centralEnvStateDegraded {
				again = true
			}
		}
		if !again {
			delete(m.running, svc)
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
	}
}

func (m *envSyncManager) begin(svc, role string, target uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.jobs[svc] = &envSyncJob{Service: svc, Role: role, Target: target, Phase: role, Status: envSyncStatusPending, StartedAt: now, UpdatedAt: now}
}

func (m *envSyncManager) finish(svc, status, lastError string) {
	m.update(svc, func(j *envSyncJob) {
		j.Status = status
		j.Phase = "done"
		j.LastError = lastError
		if envSyncFailed(status) {
			m.lastFailure[svc] = &envSyncFailure{Version: j.Target, Status: status,
				Hosts: append([]envSyncHostResult(nil), j.Hosts...), LastError: lastError, At: j.UpdatedAt}
		}
	})
}

// failure returns a copy of svc's most recent failed outcome.
func (m *envSyncManager) failure(svc string) *envSyncFailure {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.lastFailure[svc]
	if f == nil {
		return nil
	}
	cp := *f
	cp.Hosts = append([]envSyncHostResult(nil), f.Hosts...)
	return &cp
}

func (m *envSyncManager) addHost(svc string, r envSyncHostResult) {
	m.update(svc, func(j *envSyncJob) { j.Hosts = append(j.Hosts, r) })
}

func (m *envSyncManager) addWarnings(svc string, ws []string) {
	if len(ws) == 0 {
		return
	}
	m.update(svc, func(j *envSyncJob) { j.Warnings = append(j.Warnings, ws...) })
}

// waitIdle blocks while a canary rollout, a rolling replace or an
// auto-update owns svc, leaving the job "pending" — deferring, never
// failing, exactly like the auto-updater does. On true the caller holds
// svc's claim (serviceClaims) and must releaseClaim it; the auto-updater
// defers for as long as it's held. False only if ctx ran out first.
func (m *envSyncManager) waitIdle(ctx context.Context, svc string) bool {
	for {
		busy := ""
		if !m.dc.claims.tryClaim(svc, envSyncClaimOwner) {
			busy = "an auto-update"
		} else {
			if m.rm != nil {
				if st, ok := m.rm.get(svc); ok && rolloutActive(st.Status) {
					busy = "a canary rollout"
				}
			}
			if busy == "" && m.rom != nil {
				if st, ok := m.rom.get(svc); ok && rollingOpActive(st.Status) {
					busy = "a rolling replace"
				}
			}
			if busy != "" {
				m.releaseClaim(svc)
			}
		}
		if busy == "" {
			m.update(svc, func(j *envSyncJob) {
				if j.Status == envSyncStatusPending {
					j.Status = envSyncStatusRunning
				}
			})
			return true
		}
		m.update(svc, func(j *envSyncJob) { j.Status = envSyncStatusPending })
		select {
		case <-ctx.Done():
			return false
		case <-time.After(envSyncRetryInterval):
		}
	}
}

func (m *envSyncManager) releaseClaim(svc string) {
	m.dc.claims.release(svc, envSyncClaimOwner)
}

// runOrigin is one propagation job on svc's origin. Returns the version it
// worked on, so loop can tell whether the store has moved past it.
func (m *envSyncManager) runOrigin(svc string) uint64 {
	ctx, cancel := context.WithTimeout(context.Background(), rollingOpTimeout)
	defer cancel()

	retry := m.takeRetry(svc)
	rec, ok, err := m.ce.store.Get(svc)
	if err != nil || !ok {
		m.begin(svc, envSyncRoleOrigin, 0)
		msg := "no central env record"
		if err != nil {
			msg = err.Error()
		}
		m.finish(svc, envSyncStatusFailed, msg)
		return 0
	}
	target := rec.Version
	m.begin(svc, envSyncRoleOrigin, target)
	if rec.State == centralEnvStateDegraded {
		m.finish(svc, envSyncStatusDegraded, fmt.Sprintf("central env for %q is degraded (a revert failed its health gate) — fix it and set a new version", svc))
		return target
	}
	if !m.waitIdle(ctx, svc) {
		m.finish(svc, envSyncStatusFailed, "timed out waiting for another rollout to finish")
		return target
	}
	// Held for the local roll only — released before the (possibly long)
	// wait on peers, so this host's auto-updater isn't blocked by them.
	claimed := true
	defer func() {
		if claimed {
			m.releaseClaim(svc)
		}
	}()
	env, version, err := m.ce.store.effectiveEnvAt(svc, m.ce.identity, m.ce.secrets)
	if err != nil {
		m.finish(svc, envSyncStatusFailed, err.Error())
		audit(nil, envSyncAuditUser, "service.env_sync_failed", fmt.Sprintf("%s v%d: cannot resolve", svc, target))
		return target
	}
	if version != target {
		// Moved on since the read above — roll the newest, never the
		// intermediate.
		target = version
		if rec, _, err = m.ce.store.Get(svc); err != nil {
			m.finish(svc, envSyncStatusFailed, err.Error())
			return target
		}
		m.update(svc, func(j *envSyncJob) { j.Target = target })
	}
	audit(nil, envSyncAuditUser, "service.env_sync_start", fmt.Sprintf("%s v%d", svc, target))

	m.update(svc, func(j *envSyncJob) { j.Phase = envSyncRoleOrigin })
	warnings, err := m.rollLocal(ctx, svc, env, m.ce.identity, target, nil)
	m.addWarnings(svc, warnings)
	if err != nil {
		msg := scrubEnvValues(err.Error(), env)
		m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostFailed, Version: target, Error: msg})
		var gate errReplicaGateFailed
		if errors.As(err, &gate) {
			m.handleOriginGateFailure(svc, rec, target, msg)
			return target
		}
		m.finish(svc, envSyncStatusFailed, msg)
		audit(nil, envSyncAuditUser, "service.env_sync_failed", fmt.Sprintf("%s v%d on %s", svc, target, m.ce.identity))
		return target
	}
	m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostConverged, Version: target})
	m.releaseClaim(svc)
	claimed = false

	partial := m.syncPeers(ctx, svc, target, retry)
	if partial {
		m.finish(svc, envSyncStatusPartial, "")
		audit(nil, envSyncAuditUser, "service.env_sync_failed", fmt.Sprintf("%s v%d partial", svc, target))
		return target
	}
	m.finish(svc, envSyncStatusConverged, "")
	audit(nil, envSyncAuditUser, "service.env_sync_done", fmt.Sprintf("%s v%d", svc, target))
	return target
}

// handleOriginGateFailure: a new origin replica at target never became
// healthy. Revert (unless a newer version already superseded target — then
// reverting would undo THAT one, and loop rolls it anyway), and degrade if
// the version that failed was itself a revert or there's nothing to revert
// to. Peers are never told about target either way.
func (m *envSyncManager) handleOriginGateFailure(svc string, rec centralEnvRecord, target uint64, msg string) {
	cur, ok, err := m.ce.store.Get(svc)
	if err != nil || !ok {
		m.finish(svc, envSyncStatusFailed, msg)
		return
	}
	if cur.Version != target {
		m.finish(svc, envSyncStatusFailed, msg+fmt.Sprintf(" (not reverted: version %d already superseded %d)", cur.Version, target))
		audit(nil, envSyncAuditUser, "service.env_sync_failed", fmt.Sprintf("%s v%d superseded by v%d", svc, target, cur.Version))
		return
	}
	degrade := func(why string) {
		if err := m.ce.store.MarkDegraded(svc); err != nil {
			log.Printf("central env: %s: could not mark degraded: %v", svc, err)
		}
		m.finish(svc, envSyncStatusDegraded, msg+" — "+why)
		audit(nil, envSyncAuditUser, "service.env_sync_failed", fmt.Sprintf("%s v%d degraded", svc, target))
		log.Printf("central env: %s v%d degraded (%s) — no further automatic attempts", svc, target, why)
	}
	if rec.State == centralEnvStateReverted {
		degrade("the revert itself failed its health gate")
		return
	}
	v, err := m.ce.store.Revert(svc, fmt.Sprintf("v%d failed its health gate", target))
	if err != nil {
		degrade("could not revert: " + err.Error())
		return
	}
	m.finish(svc, envSyncStatusFailedReverted, msg+fmt.Sprintf(" — reverted to the previous content as v%d", v))
	audit(nil, envSyncAuditUser, "service.env_revert", fmt.Sprintf("%s v%d -> v%d", svc, target, v))
	log.Printf("central env: %s v%d failed its health gate on %s — reverted as v%d", svc, target, m.ce.identity, v)
}

// envMember is one live replica's central-env provenance.
type envMember struct {
	Name       string `json:"name"`
	EnvVersion uint64 `json:"env_version"`
	Foreign    bool   `json:"foreign,omitempty"`
}

// localMembers lists svc's live (non-canary) replicas on this host.
func (m *envSyncManager) localMembers(ctx context.Context, svc string) ([]dockerContainer, []envMember, error) {
	all, err := m.dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, svc))
	if err != nil {
		return nil, nil, err
	}
	live := liveOnly(all)
	out := make([]envMember, 0, len(live))
	for _, ct := range live {
		v, _ := strconv.ParseUint(ct.Labels[labelEnvVersion], 10, 64)
		out = append(out, envMember{Name: ct.name(), EnvVersion: v, Foreign: ct.Labels[labelEnvOrigin] == ""})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return live, out, nil
}

// rollLocal recreates svc's replicas on this host from exactly env/version
// (health-gated, surge-of-one, through rom so every existing guard sees it)
// until every stamped member carries version — one bounded re-run for
// stragglers (e.g. a scale that raced the first pass). Members with no
// pmgr.env.* stamp (created outside central env — a compose recreate, most
// plausibly) are reported, never targeted on their own. No live replicas
// here is not an error: there is nothing to roll.
func (m *envSyncManager) rollLocal(ctx context.Context, svc string, env []string, origin string, version uint64, hc *healthcheckSpec) ([]string, error) {
	var warnings []string
	for rolls := 0; ; {
		live, members, err := m.localMembers(ctx, svc)
		if err != nil {
			return warnings, err
		}
		if len(live) == 0 {
			return warnings, nil
		}
		var stale, foreign []string
		for _, mb := range members {
			switch {
			case mb.Foreign:
				foreign = append(foreign, mb.Name)
			case mb.EnvVersion != version:
				stale = append(stale, mb.Name)
			}
		}
		if len(foreign) > 0 && rolls == 0 {
			warnings = append(warnings, fmt.Sprintf("foreign member(s) on %s not created from central env (left untouched): %s", m.ce.identity, strings.Join(foreign, ", ")))
		}
		if len(stale) == 0 {
			return warnings, nil
		}
		if rolls == 2 {
			warnings = append(warnings, fmt.Sprintf("member(s) on %s still not at v%d after a re-run: %s", m.ce.identity, version, strings.Join(stale, ", ")))
			return warnings, nil
		}
		tpl := preferRunning(live)[0]
		// Config.Image, not the list's Image: the latter decays to a bare
		// digest once the creating tag is retagged or removed.
		image := tpl.Image
		if ref, err := m.dc.inspectConfigImage(ctx, tpl.ID); err == nil && ref != "" && !looksLikeBareDigest(ref) {
			image = ref
		}
		// No pull (skipPull below): the roll recreates from whatever the
		// tag resolves to locally. That is the running image unless a
		// newer one was pulled but never applied — then say so, since this
		// roll applies it too.
		if rolls == 0 && tpl.ImageID != "" {
			if id, err := m.dc.localImageID(ctx, image); err == nil && id != "" && id != tpl.ImageID {
				warnings = append(warnings, fmt.Sprintf("%s on %s: local tag %s points to a newer pulled image than the running replicas — this env roll also moves them onto it", svc, m.ce.identity, image))
			}
		}
		done := make(chan error, 1)
		// No ensureRollingReplaceCapacity here, deliberately: that guard
		// exists because an operator's rolling replace of a 1-replica
		// service would briefly be its only copy; surge-of-one never drops
		// THIS host below its current count either way, and an env change
		// must reach singleton services too.
		_, err = m.rom.startWith(svc, ReplaceServiceRequest{Image: image}, rollingOpts{
			pinnedEnv:           env,
			pinnedLabels:        map[string]string{labelEnvOrigin: origin, labelEnvVersion: strconv.FormatUint(version, 10)},
			pinnedHealthcheck:   hc,
			removeOnGateFailure: true,
			skipPull:            true,
			done:                done,
		})
		if err != nil {
			// Something else claimed svc between waitIdle and here — defer
			// again rather than fail.
			if !m.waitIdle(ctx, svc) {
				return warnings, ctx.Err()
			}
			continue
		}
		select {
		case err = <-done:
		case <-ctx.Done():
			return warnings, ctx.Err()
		}
		if err != nil {
			return warnings, err
		}
		// A freshly labeled container can 503 until the proxy re-reads.
		proxyRefresh(m.proxyURL)
		rolls++
	}
}

// ---- peers (origin side) ----

// peerCentralEnvStatus is the wire shape of GET /peer/central-env/{svc}/status
// — names and versions only. Keys/Refs/Overrides are filled only when the
// answering host is svc's origin.
type peerCentralEnvStatus struct {
	Service   string      `json:"service"`
	Identity  string      `json:"identity"`
	Role      string      `json:"role"`
	Origin    string      `json:"origin,omitempty"`
	Version   uint64      `json:"version"`
	State     string      `json:"state,omitempty"`
	FetchedAt int64       `json:"fetched_at,omitempty"`
	Stale     bool        `json:"stale,omitempty"`
	Members   []envMember `json:"members"`
	Job       *envSyncJob `json:"job,omitempty"`
	// LastFailure is this host's most recent non-converged job;
	// FailedVersion a version it failed and rolled back from (peers).
	LastFailure   *envSyncFailure     `json:"last_failure,omitempty"`
	FailedVersion uint64              `json:"failed_version,omitempty"`
	Keys          []string            `json:"keys,omitempty"`
	Refs          map[string]string   `json:"refs,omitempty"`
	Overrides     map[string][]string `json:"overrides,omitempty"`
}

// atLeast reports whether a peer's replicas (and its cache) are all at or
// past version. At-or-past, not equal: a peer fetches the NEWEST version
// when told to sync, so it can legitimately be ahead of a job's target.
func (ps peerCentralEnvStatus) atLeast(version uint64) bool {
	if ps.Version < version {
		return false
	}
	for _, mb := range ps.Members {
		if !mb.Foreign && mb.EnvVersion < version {
			return false
		}
	}
	return true
}

func (ps peerCentralEnvStatus) runsOrCaches() bool {
	return len(ps.Members) > 0 || ps.Version > 0
}

func hasFeature(features []string, f string) bool {
	for _, x := range features {
		if x == f {
			return true
		}
	}
	return false
}

// envSyncPeer is one configured peer as the job sees it.
type envSyncPeer struct {
	url      string
	identity string
	capable  bool
}

// peers lists every peer with a known identity, sorted so notification
// order is stable. A peer that hasn't completed a handshake yet has no
// identity and is skipped — the next reconcile tick picks it up.
func (m *envSyncManager) peers() []envSyncPeer {
	if m.registry == nil {
		return nil
	}
	var out []envSyncPeer
	for u, st := range m.registry.Status() {
		if st.Identity == "" || st.Identity == m.ce.identity {
			continue
		}
		out = append(out, envSyncPeer{url: u, identity: st.Identity, capable: hasFeature(st.Features, centralEnvFeature)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].identity < out[j].identity })
	return out
}

func (m *envSyncManager) peerStatus(ctx context.Context, peerURL, svc string) (peerCentralEnvStatus, int, error) {
	var ps peerCentralEnvStatus
	code, body, err := peerMutate(ctx, m.client, peerURL, m.secret, http.MethodGet, "/peer/central-env/"+url.PathEscape(svc)+"/status", 5*time.Second, nil, nil, "")
	if err != nil {
		return ps, 0, err
	}
	if code != http.StatusOK {
		return ps, code, fmt.Errorf("status %d", code)
	}
	if err := json.Unmarshal(body, &ps); err != nil {
		return ps, code, errors.New("bad status response")
	}
	return ps, code, nil
}

// peerRunsService asks a pre-central-env peer (plain /peer/services) whether
// it runs svc at all, so an old peer that does is reported "unsupported"
// instead of silently left behind.
func (m *envSyncManager) peerRunsService(ctx context.Context, peerURL, svc string) bool {
	var body peerServicesResp
	if err := peerGET(ctx, m.client, peerURL, m.secret, "/peer/services", &body); err != nil {
		return false
	}
	for _, s := range body.Services {
		if s.Name == svc {
			return true
		}
	}
	return false
}

// syncPeers notifies each capable peer that runs or caches svc, strictly one
// at a time, waiting for each to finish before the next. Reports whether
// any peer ended up not converged.
func (m *envSyncManager) syncPeers(ctx context.Context, svc string, target uint64, retry bool) (partial bool) {
	if m.secret == "" {
		return false
	}
	for _, p := range m.peers() {
		if ctx.Err() != nil {
			return true
		}
		m.update(svc, func(j *envSyncJob) { j.Phase = "peer:" + p.identity })
		if !p.capable {
			if m.peerRunsService(ctx, p.url, svc) {
				m.addHost(svc, envSyncHostResult{Host: p.identity, Status: envSyncHostUnsupported})
				m.addWarnings(svc, []string{fmt.Sprintf("%s runs %s but does not support central env — its replicas were not updated", p.identity, svc)})
			}
			continue
		}
		// A peer that already failed (and rolled back) this exact version
		// isn't asked again by an automatic job — it would only start
		// another failing replica. A manual sync clears this first.
		m.mu.Lock()
		failedAt, failed := m.peerFailed[svc][p.identity]
		m.mu.Unlock()
		if failed && failedAt >= target {
			m.addHost(svc, envSyncHostResult{Host: p.identity, Status: envSyncHostRolledBack, Version: target, Error: "failed this version before — not retried automatically (use sync to retry)"})
			partial = true
			continue
		}
		r := m.syncOnePeer(ctx, p, svc, target, retry)
		if r.Status == "" {
			continue
		}
		m.addHost(svc, r)
		switch r.Status {
		case envSyncHostConverged, envSyncHostUnsupported:
		case envSyncHostRolledBack:
			partial = true
			m.mu.Lock()
			if m.peerFailed[svc] == nil {
				m.peerFailed[svc] = map[string]uint64{}
			}
			m.peerFailed[svc][p.identity] = target
			m.mu.Unlock()
		default:
			partial = true
		}
	}
	return partial
}

// syncOnePeer notifies one capable peer and waits for it. An empty Status
// means the peer neither runs nor caches svc — nothing to do there.
func (m *envSyncManager) syncOnePeer(ctx context.Context, p envSyncPeer, svc string, target uint64, retry bool) envSyncHostResult {
	res := envSyncHostResult{Host: p.identity, Version: target}
	ps, code, err := m.peerStatus(ctx, p.url, svc)
	switch {
	case code == http.StatusNotFound:
		res.Status = envSyncHostUnsupported
		return res
	case err != nil:
		res.Status, res.Error = envSyncHostUnreachable, "status: "+err.Error()
		return res
	case !ps.runsOrCaches():
		return envSyncHostResult{}
	case ps.atLeast(target):
		res.Status = envSyncHostConverged
		return res
	}
	notice := map[string]any{"origin": m.ce.identity, "version": target}
	if retry {
		notice["retry"] = true
	}
	body, _ := json.Marshal(notice)
	code, _, err = peerMutate(ctx, m.client, p.url, m.secret, http.MethodPost, "/peer/central-env/"+url.PathEscape(svc)+"/notify", 10*time.Second, bytes.NewReader(body), nil, "")
	switch {
	case err != nil:
		res.Status, res.Error = envSyncHostUnreachable, "notify: "+err.Error()
		return res
	case code == http.StatusNotFound:
		res.Status = envSyncHostUnsupported
		return res
	case code == http.StatusOK:
		res.Status = envSyncHostConverged
		return res
	case code != http.StatusAccepted:
		res.Status, res.Error = envSyncHostFailed, fmt.Sprintf("notify: status %d", code)
		return res
	}
	deadline := time.Now().Add(envSyncPeerTimeout)
	for {
		select {
		case <-ctx.Done():
			res.Status, res.Error = envSyncHostFailed, "timed out waiting for the peer"
			return res
		case <-time.After(envSyncPeerPollInterval):
		}
		ps, _, err := m.peerStatus(ctx, p.url, svc)
		if err == nil {
			// Only a job for (at least) this version counts — right after
			// the 202 the peer may still show its PREVIOUS job's terminal
			// state.
			if j := ps.Job; j != nil && j.Target >= target && envSyncTerminal(j.Status) {
				res.Version = j.Target
				switch j.Status {
				case envSyncStatusConverged:
					res.Status = envSyncHostConverged
				case envSyncStatusFailedRolledBack:
					res.Status, res.Error = envSyncHostRolledBack, j.LastError
				default:
					res.Status, res.Error = envSyncHostFailed, j.LastError
				}
				return res
			}
			if (ps.Job == nil || envSyncTerminal(ps.Job.Status)) && ps.atLeast(target) {
				res.Status = envSyncHostConverged
				return res
			}
		}
		if time.Now().After(deadline) {
			res.Status, res.Error = envSyncHostFailed, "timed out waiting for the peer"
			return res
		}
	}
}

// ---- peer side ----

// runPeer is one local job on a host that is NOT svc's origin: fetch the
// newest env from the origin, roll local replicas onto it, and on a health
// gate failure roll them back to what they were running.
func (m *envSyncManager) runPeer(svc string) {
	ctx, cancel := context.WithTimeout(context.Background(), rollingOpTimeout)
	defer cancel()
	m.mu.Lock()
	t := m.peerTarget[svc]
	m.mu.Unlock()
	m.begin(svc, envSyncRolePeer, t.version)
	if t.origin == "" {
		m.finish(svc, envSyncStatusFailed, "no origin known")
		return
	}
	if !m.waitIdle(ctx, svc) {
		m.finish(svc, envSyncStatusFailed, "timed out waiting for another rollout to finish")
		return
	}
	defer m.releaseClaim(svc)
	if m.ce.fetchFromOrigin == nil {
		m.finish(svc, envSyncStatusFailed, "no origin fetcher configured")
		return
	}
	// The rollback target is captured BEFORE the fetch rotates the cache:
	// what the replicas here are running now.
	before, hadBefore := m.ce.cache.Get(svc)
	got, err := m.ce.fetchFromOrigin(ctx, t.origin, svc, m.ce.identity)
	if err != nil {
		m.finish(svc, envSyncStatusFailed, fmt.Sprintf("origin %s unreachable", t.origin))
		return
	}
	m.markOriginSeen(svc)
	env, version, hc := got.Env, got.Version, got.Healthcheck
	var refused uint64 // the origin's version, already failed here
	if err := m.ce.cache.PutWithHealthcheck(svc, t.origin, got.Version, got.Env, got.Healthcheck); err != nil {
		var older errCentralEnvCacheOlder
		var failed errCentralEnvCacheFailed
		switch {
		case errors.As(err, &failed):
			refused = got.Version
		case !errors.As(err, &older):
			m.finish(svc, envSyncStatusFailed, scrubEnvValues(err.Error(), got.Env))
			return
		}
		cached, _ := m.ce.cache.Get(svc)
		if !cached.usable() {
			m.finish(svc, envSyncStatusFailed, errCentralEnvFailedHere{Service: svc, Version: cached.Version}.Error())
			return
		}
		env, version = cached.Env, cached.Version
	}
	if cached, ok := m.ce.cache.Get(svc); ok && hc == nil {
		hc = cached.Healthcheck
	}
	var prevEnv []string
	var prevVersion uint64
	if hadBefore && before.Origin == t.origin {
		if before.Version < version {
			prevEnv, prevVersion = before.Env, before.Version
		} else if before.PrevVersion > 0 {
			prevEnv, prevVersion = before.PrevEnv, before.PrevVersion
		}
	}
	m.update(svc, func(j *envSyncJob) { j.Target = version })
	audit(nil, envSyncAuditUser, "service.env_sync_start", fmt.Sprintf("%s v%d from %s", svc, version, t.origin))

	warnings, err := m.rollLocal(ctx, svc, env, t.origin, version, hc)
	m.addWarnings(svc, warnings)
	if err == nil && refused > 0 {
		// Still on the rolled-back version, as it should be — but the
		// origin's asked-for version is not running here.
		msg := fmt.Sprintf("v%d failed its health gate here before — staying on v%d (sync to retry)", refused, version)
		m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostRolledBack, Version: version, Error: msg})
		m.update(svc, func(j *envSyncJob) { j.Target = refused })
		m.finish(svc, envSyncStatusFailedRolledBack, msg)
		return
	}
	if err == nil {
		m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostConverged, Version: version})
		m.finish(svc, envSyncStatusConverged, "")
		audit(nil, envSyncAuditUser, "service.env_sync_done", fmt.Sprintf("%s v%d", svc, version))
		return
	}
	msg := scrubEnvValues(err.Error(), env, prevEnv)
	var gate errReplicaGateFailed
	if !errors.As(err, &gate) {
		m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostFailed, Version: version, Error: msg})
		m.finish(svc, envSyncStatusFailed, msg)
		audit(nil, envSyncAuditUser, "service.env_sync_failed", fmt.Sprintf("%s v%d on %s", svc, version, m.ce.identity))
		return
	}
	if prevEnv == nil {
		if err := m.ce.cache.RollBack(svc, version, nil, 0); err != nil {
			log.Printf("central env: %s: could not record v%d as failed: %v", svc, version, scrubEnvValues(err.Error(), env))
		}
		m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostFailed, Version: version, Error: msg})
		m.finish(svc, envSyncStatusFailed, msg+" — no previous env to roll back to")
		audit(nil, envSyncAuditUser, "service.env_sync_failed", fmt.Sprintf("%s v%d on %s", svc, version, m.ce.identity))
		return
	}
	m.update(svc, func(j *envSyncJob) { j.Phase = "rollback" })
	rbWarnings, rbErr := m.rollLocal(ctx, svc, prevEnv, t.origin, prevVersion, hc)
	m.addWarnings(svc, rbWarnings)
	// Whatever the rollback's outcome, future creates here (scale, a
	// label flip) must never use the version that just failed.
	if err := m.ce.cache.RollBack(svc, version, prevEnv, prevVersion); err != nil {
		log.Printf("central env: %s: could not restore v%d in the cache: %v", svc, prevVersion, scrubEnvValues(err.Error(), env, prevEnv))
	}
	if rbErr != nil {
		msg += " — rollback to v" + strconv.FormatUint(prevVersion, 10) + " also failed: " + scrubEnvValues(rbErr.Error(), env, prevEnv)
		m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostFailed, Version: version, Error: msg})
		m.finish(svc, envSyncStatusFailed, msg)
		audit(nil, envSyncAuditUser, "service.env_sync_failed", fmt.Sprintf("%s v%d on %s, rollback failed", svc, version, m.ce.identity))
		return
	}
	m.addHost(svc, envSyncHostResult{Host: m.ce.identity, Status: envSyncHostRolledBack, Version: prevVersion, Error: msg})
	m.finish(svc, envSyncStatusFailedRolledBack, msg+fmt.Sprintf(" — rolled back to v%d", prevVersion))
	audit(nil, envSyncAuditUser, "service.env_sync_failed", fmt.Sprintf("%s v%d on %s, rolled back to v%d", svc, version, m.ce.identity, prevVersion))
}

// clearFailures forgets every "failed at version N" guard for svc — what an
// operator's explicit sync means: try the failed hosts again.
func (m *envSyncManager) clearFailures(svc string) {
	if err := m.ce.cache.ClearFailed(svc); err != nil {
		log.Printf("central env: %s: could not clear the failed-version marker: %v", svc, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.peerFailed, svc)
	if m.ce.store.Has(svc) {
		m.retry[svc] = true
	}
}

// takeRetry reports (once) whether svc's next origin job follows an
// explicit sync — it then asks each peer to clear its own failed marker.
func (m *envSyncManager) takeRetry(svc string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.retry[svc]
	delete(m.retry, svc)
	return r
}

func (m *envSyncManager) markOriginSeen(svc string) {
	m.mu.Lock()
	m.originSeen[svc] = time.Now()
	m.mu.Unlock()
}

// stale reports whether this (non-origin) host has lost touch with svc's
// origin: no successful fetch or reconcile contact for three reconcile
// periods.
func (m *envSyncManager) stale(svc string, fetchedAt int64) bool {
	m.mu.Lock()
	seen := m.originSeen[svc]
	m.mu.Unlock()
	if f := time.Unix(fetchedAt, 0); f.After(seen) {
		seen = f
	}
	return time.Since(seen) > 3*envSyncReconcileInterval
}

// status is this host's answer to GET /peer/central-env/{svc}/status.
func (m *envSyncManager) status(ctx context.Context, svc string) (peerCentralEnvStatus, error) {
	ps := peerCentralEnvStatus{Service: svc, Identity: m.ce.identity, Role: "none", Members: []envMember{}}
	_, members, err := m.localMembers(ctx, svc)
	if err != nil {
		return ps, err
	}
	ps.Members = members
	if j, ok := m.get(svc); ok {
		ps.Job = j
	}
	ps.LastFailure = m.failure(svc)
	if m.ce.store.Has(svc) {
		ps.Role, ps.Origin = envSyncRoleOrigin, m.ce.identity
		rec, _, err := m.ce.store.Get(svc)
		if err != nil {
			ps.State = "broken"
			return ps, nil
		}
		ps.Version, ps.State = rec.Version, rec.State
		ps.Keys, ps.Refs, ps.Overrides = centralEnvNames(rec)
		return ps, nil
	}
	if cached, ok := m.ce.cache.Get(svc); ok {
		ps.Role, ps.Origin, ps.Version, ps.FetchedAt = envSyncRolePeer, cached.Origin, cached.Version, cached.FetchedAt
		ps.FailedVersion = cached.FailedVersion
		ps.Stale = m.stale(svc, cached.FetchedAt)
	}
	return ps, nil
}

// centralEnvNames is a record reduced to names: base keys, which of them
// are "ref:NAME" references (the ref string is a name, not a value), and
// which keys each host overrides.
func centralEnvNames(rec centralEnvRecord) (keys []string, refs map[string]string, overrides map[string][]string) {
	keys = make([]string, 0, len(rec.Base))
	for k, v := range rec.Base {
		keys = append(keys, k)
		if strings.HasPrefix(v, secretRefPrefix) {
			if refs == nil {
				refs = map[string]string{}
			}
			refs[k] = v
		}
	}
	sort.Strings(keys)
	for host, kv := range rec.Overrides {
		if len(kv) == 0 {
			continue
		}
		if overrides == nil {
			overrides = map[string][]string{}
		}
		ks := make([]string, 0, len(kv))
		for k := range kv {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		overrides[host] = ks
	}
	return keys, refs, overrides
}

// ---- reconcile ----

// reconcileLoop runs reconcileOnce at startup and every
// envSyncReconcileInterval until ctx ends.
func (m *envSyncManager) reconcileLoop(ctx context.Context) {
	m.reconcileOnce(ctx)
	t := time.NewTicker(envSyncReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce is the safety net under notify: on the origin, it detects a
// rotated secret behind an unchanged record, and re-requests propagation for
// any service whose replicas here or on a capable peer lag the store; on a
// non-origin host, it asks each cached service's origin for its version and
// pulls a newer one.
func (m *envSyncManager) reconcileOnce(ctx context.Context) {
	for _, svc := range m.ce.store.Services() {
		if ctx.Err() != nil {
			return
		}
		m.reconcileOrigin(ctx, svc)
	}
	for _, svc := range m.ce.cache.Services() {
		if ctx.Err() != nil {
			return
		}
		if !m.ce.store.Has(svc) {
			m.reconcilePeer(ctx, svc)
		}
	}
}

func (m *envSyncManager) reconcileOrigin(ctx context.Context, svc string) {
	rec, ok, err := m.ce.store.Get(svc)
	if err != nil || !ok || rec.State == centralEnvStateDegraded || m.busy(svc) {
		return
	}
	if h, err := m.resolvedHash(svc, rec); err == nil {
		m.mu.Lock()
		mark, seen := m.resolved[svc]
		rotated := seen && mark.version == rec.Version && mark.hash != h
		m.resolved[svc] = envSyncResolvedMark{version: rec.Version, hash: h}
		m.mu.Unlock()
		if rotated {
			v, err := m.ce.store.Touch(svc, "secret-rotation")
			if err != nil {
				log.Printf("central env: %s: a referenced secret changed but the version bump failed: %v", svc, err)
				return
			}
			m.mu.Lock()
			m.resolved[svc] = envSyncResolvedMark{version: v, hash: h}
			m.mu.Unlock()
			audit(nil, envSyncAuditUser, "service.env_secret_rotated", fmt.Sprintf("%s v%d -> v%d", svc, rec.Version, v))
			log.Printf("central env: %s: a referenced secret changed — bumped to v%d", svc, v)
			m.request(svc)
			return
		}
	}
	_, members, err := m.localMembers(ctx, svc)
	if err == nil {
		for _, mb := range members {
			if !mb.Foreign && mb.EnvVersion != rec.Version {
				m.request(svc)
				return
			}
		}
	}
	if m.secret == "" {
		return
	}
	for _, p := range m.peers() {
		if !p.capable {
			continue
		}
		m.mu.Lock()
		failedAt, failed := m.peerFailed[svc][p.identity]
		m.mu.Unlock()
		if failed && failedAt >= rec.Version {
			continue
		}
		ps, _, err := m.peerStatus(ctx, p.url, svc)
		if err != nil || !ps.runsOrCaches() || ps.Role == envSyncRoleOrigin {
			continue
		}
		if !ps.atLeast(rec.Version) {
			m.request(svc)
			return
		}
	}
}

func (m *envSyncManager) reconcilePeer(ctx context.Context, svc string) {
	cached, ok := m.ce.cache.Get(svc)
	if !ok || m.registry == nil || m.secret == "" || m.busy(svc) {
		return
	}
	peerURL, ok := m.registry.URLForIdentity(cached.Origin)
	if !ok {
		// Not handshaken yet (startup) or not configured: next tick.
		return
	}
	ps, _, err := m.peerStatus(ctx, peerURL, svc)
	if err != nil || ps.Role != envSyncRoleOrigin {
		return
	}
	m.markOriginSeen(svc)
	if ps.Version > cached.Version && ps.Version > cached.FailedVersion {
		m.requestPeer(svc, cached.Origin, ps.Version)
		return
	}
	if !cached.usable() {
		return
	}
	_, members, err := m.localMembers(ctx, svc)
	if err != nil {
		return
	}
	for _, mb := range members {
		if !mb.Foreign && mb.EnvVersion < cached.Version {
			m.requestPeer(svc, cached.Origin, cached.Version)
			return
		}
	}
}

// resolvedHash hashes svc's RESOLVED effective env for every host it can
// name (this host, each override host, and base-only), so a rotated
// "ref:NAME" secret shows up as a changed hash under an unchanged version.
// In memory only.
func (m *envSyncManager) resolvedHash(svc string, rec centralEnvRecord) (string, error) {
	hosts := []string{"", m.ce.identity}
	for h := range rec.Overrides {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	resolved := make(map[string][]string, len(hosts))
	for _, h := range hosts {
		merged := make(map[string]string, len(rec.Base))
		for k, v := range rec.Base {
			merged[k] = v
		}
		for k, v := range rec.Overrides[h] {
			merged[k] = v
		}
		r, _, err := resolveSecretRefs(svc, merged, m.ce.secrets)
		if err != nil {
			return "", err
		}
		resolved[h] = envMapToSlice(r)
	}
	data, _ := json.Marshal(resolved)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// scrubEnvValues replaces every env value (of any of envs) found in msg —
// the backstop for error text that originates in Docker and could echo a
// create body. Short values are left alone: they'd mangle ordinary words
// and are not plausibly secrets.
func scrubEnvValues(msg string, envs ...[]string) string {
	var vals []string
	for _, env := range envs {
		for _, e := range env {
			if _, v, ok := strings.Cut(e, "="); ok && len(v) >= 6 {
				vals = append(vals, v)
			}
		}
	}
	// Longest first: a value that is a prefix of another must not be
	// redacted out of it first, leaving the longer one's tail behind.
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		msg = strings.ReplaceAll(msg, v, "[redacted]")
	}
	return msg
}

// localImageID is the image ID ref resolves to locally ("" if not present).
func (c *dockerClient) localImageID(ctx context.Context, ref string) (string, error) {
	body, err := c.get(ctx, "/images/"+url.PathEscape(ref)+"/json")
	if err != nil {
		return "", err
	}
	defer body.Close()
	var resp struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return "", err
	}
	return resp.ID, nil
}
