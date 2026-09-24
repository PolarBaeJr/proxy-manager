// rollingop: async surge-of-one rolling replace for label-managed services,
// tracked per-service the same way rollout.go tracks staged canary
// rollouts, but structurally simpler — a rolling replace has exactly one
// entry point (the POST handler that calls start) rather than a background
// ticker AND direct API calls racing on the same service, so there's no
// need for rolloutManager's extra per-service sync.Mutex on top of the map
// mutex.
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	rollingOpStatusRunning   = "running"
	rollingOpStatusCompleted = "completed"
	rollingOpStatusFailed    = "failed"
)

// rollingOpReplica is one replica's outcome within a rolling-replace job, in
// swap order.
type rollingOpReplica struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
}

// rollingOpTimeout bounds a single rolling-replace job's background
// context — generous enough to cover many replicas each waiting out
// canaryPromoteHealthTimeout + replaceSettleDelay, but finite so a wedged
// Docker daemon can't leak the goroutine forever. A package var only so
// tests can shrink it — same seam as replaceSettleDelay.
var rollingOpTimeout = 30 * time.Minute

// rollingOpActive reports whether a rolling-replace job in this status
// still occupies the service's "one mutation at a time" slot. The terminal
// statuses (completed/failed) don't — a fresh mutation can start right over
// them.
func rollingOpActive(status string) bool {
	return status == rollingOpStatusRunning
}

// serviceClaims is a per-service "who is recreating this right now" marker
// for the two unattended mutators that would otherwise interleave on the
// same containers: the auto-updater's plain replaceService and the central
// env propagation job. Each claims before touching a service and defers
// (never fails) when the other holds it. Per dockerClient, i.e. per host.
// The zero value is ready to use.
type serviceClaims struct {
	mu    sync.Mutex
	owner map[string]string
}

// tryClaim takes svc for owner. True if it was free or already owner's.
func (c *serviceClaims) tryClaim(svc, owner string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.owner[svc]; ok && cur != owner {
		return false
	}
	if c.owner == nil {
		c.owner = map[string]string{}
	}
	c.owner[svc] = owner
	return true
}

func (c *serviceClaims) release(svc, owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner[svc] == owner {
		delete(c.owner, svc)
	}
}

func (c *serviceClaims) holder(svc string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owner[svc]
}

// rollingOpts adjusts a rolling replace for the central-env propagation job
// (centralenv_sync.go). The zero value is exactly the operator-initiated
// rolling replace.
type rollingOpts struct {
	// pinnedEnv, when non-nil, is the exact env every new replica gets —
	// resolved once by the job rather than re-resolved per create — and
	// pinnedLabels (the pmgr.env.* provenance) are stamped over the
	// template's labels, compose labels dropped. pinnedHealthcheck fills in
	// a template that has none (the origin's, for a spread replica).
	pinnedEnv         []string
	pinnedLabels      map[string]string
	pinnedHealthcheck *healthcheckSpec
	// removeOnGateFailure removes a new replica that fails its health gate
	// instead of leaving it running next to the predecessor it didn't
	// replace — the job then rolls back or reverts deliberately.
	removeOnGateFailure bool
	// skipPull keeps an env-only rollout from pulling (and so silently
	// upgrading to) a newer digest for the service's tag.
	skipPull bool
	// done, if non-nil, receives the job's final error (nil on success)
	// exactly once. Must be buffered: the job never blocks on it.
	done chan error
}

// errReplicaGateFailed is replaceServiceRolling's "a new replica never became
// healthy" failure, distinct from every other error (Docker create/start, no
// live replicas, ...) — only this one is evidence against the env or image
// being rolled, so it's the only one the central-env job reverts on.
type errReplicaGateFailed struct {
	err error
}

func (e errReplicaGateFailed) Error() string { return e.err.Error() }
func (e errReplicaGateFailed) Unwrap() error { return e.err }

// rollingOpState is one service's in-flight (or most-recently-finished)
// rolling replace.
type rollingOpState struct {
	Service   string             `json:"service"`
	Image     string             `json:"image"`
	Done      int                `json:"done"`
	Total     int                `json:"total"`
	Status    string             `json:"status"`
	LastError string             `json:"last_error,omitempty"`
	StartedAt time.Time          `json:"started_at"`
	Replicas  []rollingOpReplica `json:"replicas,omitempty"`
}

// rollingOpManager tracks every service's in-flight rolling replace — a
// small mutex guards the map itself, and get returns a copy so a concurrent
// GET never races with the goroutine's in-place field updates.
type rollingOpManager struct {
	dc *dockerClient

	mu  sync.Mutex
	ops map[string]*rollingOpState
}

func newRollingOpManager(dc *dockerClient) *rollingOpManager {
	return &rollingOpManager{dc: dc, ops: map[string]*rollingOpState{}}
}

// get returns a copy of the tracked state so callers (including a
// concurrent GET while the job is mid-flight) never race with in-place
// field updates.
func (m *rollingOpManager) get(name string) (*rollingOpState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.ops[name]
	if !ok {
		return nil, false
	}
	cp := *r
	// r.Replicas is a slice header sharing r's backing array with the job
	// goroutine's still-in-progress update() appends — copying just the
	// struct above leaves cp.Replicas pointing at that same backing array,
	// so a concurrent append (and any resulting reallocation/overwrite) is a
	// real data race with a caller reading cp.Replicas afterward. Deep-copy
	// it before returning.
	cp.Replicas = append([]rollingOpReplica(nil), r.Replicas...)
	return &cp, true
}

func (m *rollingOpManager) update(name string, fn func(*rollingOpState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.ops[name]; ok {
		fn(r)
	}
}

// start launches a rolling replace in the background and returns its
// initial state. The already-active check and the map insert happen under
// the same mutex hold so two concurrent POSTs for the same service can't
// both pass the guard and race each other onto the same containers.
func (m *rollingOpManager) start(name string, req ReplaceServiceRequest) (*rollingOpState, error) {
	return m.startWith(name, req, rollingOpts{})
}

// startWith is start with rollingOpts. Going through the same map (rather
// than calling replaceServiceRolling directly) is what makes a central-env
// propagation visible to every existing "one mutation at a time" guard —
// the API's, the auto-updater's, and a second propagation's.
func (m *rollingOpManager) startWith(name string, req ReplaceServiceRequest, opts rollingOpts) (*rollingOpState, error) {
	m.mu.Lock()
	if existing, ok := m.ops[name]; ok && rollingOpActive(existing.Status) {
		m.mu.Unlock()
		return nil, fmt.Errorf("%q already has an active rolling replace", name)
	}
	st := &rollingOpState{
		Service:   name,
		Image:     req.Image,
		Status:    rollingOpStatusRunning,
		StartedAt: time.Now(),
	}
	m.ops[name] = st
	// Snapshot the initial state to return WHILE the lock is still held: the
	// goroutine below can start calling update() (a write to *st) the moment
	// it's launched, so reading st's fields after unlocking races it.
	cp := *st
	m.mu.Unlock()

	go func() {
		// The HTTP handler returns immediately (202 Accepted) and its
		// request context is canceled the instant it does — a background
		// context (bounded by rollingOpTimeout, not the request's) is
		// required or every job would die on its first Docker call.
		ctx, cancel := context.WithTimeout(context.Background(), rollingOpTimeout)
		defer cancel()
		err := m.dc.replaceServiceRolling(ctx, name, req, func(done, total int, replicaName, verdict string) {
			m.update(name, func(s *rollingOpState) {
				s.Done = done
				s.Total = total
				// The initial progress(0, total, "", "") call up front (see
				// replaceServiceRolling's doc comment) carries no replica
				// yet — guard on replicaName so it doesn't append an empty
				// entry to Replicas.
				if replicaName != "" {
					s.Replicas = append(s.Replicas, rollingOpReplica{Name: replicaName, Verdict: verdict})
				}
			})
		}, opts)
		m.update(name, func(s *rollingOpState) {
			if err != nil {
				s.Status = rollingOpStatusFailed
				// Values of the env this job was given never reach the
				// status (API, MCP, UI) even if a Docker error echoed one.
				s.LastError = scrubEnvValues(err.Error(), opts.pinnedEnv, envMapToSlice(req.Env))
				return
			}
			s.Status = rollingOpStatusCompleted
		})
		if opts.done != nil {
			opts.done <- err
		}
	}()

	return &cp, nil
}
