// rollout: gradual/staged canary rollouts with an automatic health-gated
// rollback, entirely in-process — no peer/cross-host forwarding, no MCP
// tools, no UI. A rollout ramps a service's canary replica count up through
// a sequence of percentage steps (e.g. 25% -> 50% -> 100%), health-checking
// the canary between steps and automatically rolling back to the
// last-known-good live replica count the moment a canary looks unhealthy.
//
// Concurrency shape: a small mutex (rolloutManager.mu) guards the state map
// itself, but every actual Docker mutation (start/advance/abort, and the
// background ticker's own rollback) is additionally serialized per-service
// by serviceLock — the map mutex alone would only stop concurrent map
// access, not a ticker-driven auto-rollback interleaving with a concurrent
// manual advance for the same service.
//
// Two substrates: label-managed services (docker.go's canary primitives,
// keyed by proxy.* labels) and onboarded services (onboarded.go's
// OnboardedStore-backed services, keyed by container name prefix). The
// dispatch wrapper methods below (checkHealth/scaleCanaryTo/scaleLiveTo/
// promote/discard) pick the right substrate per-service; everything above
// them (startRollout/doAdvance/rollbackContainers/checkOne) is substrate-
// agnostic.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	rolloutStatusAwaitingAdvance = "awaiting-advance"
	rolloutStatusRollingBack     = "rolling-back"
	rolloutStatusRolledBack      = "rolled-back"
	rolloutStatusCompleted       = "completed"
	rolloutStatusFailed          = "failed"

	// canaryHealthProbeTimeout mirrors cmd/proxy/health.go's healthTimeout —
	// the budget for a single HTTP health-path probe against a canary
	// container.
	canaryHealthProbeTimeout = 2 * time.Second

	// canaryMaxRestarts is the restart-count threshold above which a canary
	// is judged unhealthy. Exit code is deliberately NOT checked here — a
	// container that crashed once and is now running cleanly isn't a
	// reliable "currently unhealthy" signal, but a container the daemon has
	// had to restart repeatedly is.
	canaryMaxRestarts = 1
)

// canaryPromoteHealthTimeout/canaryPromoteHealthPoll bound promoteCanary's
// pre-teardown health gate. rolloutCheckInterval paces the background
// ticker. Package vars only so tests can shrink them — same seam as
// replaceSettleDelay.
var (
	canaryPromoteHealthTimeout = 30 * time.Second
	canaryPromoteHealthPoll    = 3 * time.Second
	rolloutCheckInterval       = 15 * time.Second
)

var defaultRolloutSteps = []int{25, 50, 100}

// rolloutState is one service's in-flight (or most-recently-finished)
// staged rollout.
type rolloutState struct {
	Service          string    `json:"service"`
	Image            string    `json:"image"`
	Steps            []int     `json:"steps"`
	StepIdx          int       `json:"step_idx"`
	Status           string    `json:"status"`
	OrigLiveReplicas int       `json:"orig_live_replicas"`
	LastError        string    `json:"last_error,omitempty"`
	StartedAt        time.Time `json:"started_at"`
}

// rolloutActive reports whether a rollout in this status still occupies the
// service's "one canary/rollout at a time" slot. Terminal statuses
// (completed/rolled-back/failed) don't — a fresh rollout can be started
// right over them.
func rolloutActive(status string) bool {
	return status == rolloutStatusAwaitingAdvance || status == rolloutStatusRollingBack
}

// RolloutRequest is the POST /api/services/{name}/rollout body.
type RolloutRequest struct {
	Image  string            `json:"image"`
	Env    map[string]string `json:"env,omitempty"`
	EnvAck []string          `json:"env_ack,omitempty"`
	Steps  []int             `json:"steps,omitempty"`
}

// rolloutManager tracks every service's in-flight rollout — same
// per-service-map-behind-a-mutex shape as autoUpdater's failures map, plus a
// per-service lock (see package doc comment) since this controller, unlike
// autoUpdater, has two entry points (the ticker AND direct API calls) that
// can race on the same service's containers.
type rolloutManager struct {
	dc         *dockerClient
	onb        *OnboardedStore
	routesPath string
	proxyURL   string

	mu       sync.Mutex
	rollouts map[string]*rolloutState
	locks    map[string]*sync.Mutex
}

func newRolloutManager(dc *dockerClient, onb *OnboardedStore, routesPath, proxyURL string) *rolloutManager {
	return &rolloutManager{
		dc: dc, onb: onb, routesPath: routesPath, proxyURL: proxyURL,
		rollouts: map[string]*rolloutState{}, locks: map[string]*sync.Mutex{},
	}
}

// onboardedSvc is a nil-safe wrapper around onb.Get: some newDashboardMux
// callers (tests exercising unrelated endpoints) construct a rolloutManager
// with a nil onboarded store, and every dispatch method below checks
// onboarded-ness unconditionally, even for label-managed calls.
func (m *rolloutManager) onboardedSvc(name string) (OnboardedService, bool) {
	if m.onb == nil {
		return OnboardedService{}, false
	}
	return m.onb.Get(name)
}

func (m *rolloutManager) serviceLock(name string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[name]
	if !ok {
		l = &sync.Mutex{}
		m.locks[name] = l
	}
	return l
}

// get returns a copy of the tracked state so callers (including a
// concurrent GET while another goroutine is mid-rollback) never race with
// in-place field updates.
func (m *rolloutManager) get(name string) (*rolloutState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rollouts[name]
	if !ok {
		return nil, false
	}
	cp := *r
	return &cp, true
}

func (m *rolloutManager) update(name string, fn func(*rolloutState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rollouts[name]; ok {
		fn(r)
	}
}

// validateRolloutSteps requires strictly ascending percentages, each in
// (0,100], with the last step always 100 — a rollout that never reaches
// full traffic isn't a completable rollout.
func validateRolloutSteps(steps []int) error {
	prev := 0
	for _, s := range steps {
		if s <= 0 || s > 100 {
			return fmt.Errorf("invalid rollout step %d: must be in (0,100]", s)
		}
		if s <= prev {
			return fmt.Errorf("rollout steps must be strictly ascending, got %v", steps)
		}
		prev = s
	}
	if steps[len(steps)-1] != 100 {
		return fmt.Errorf("rollout steps must end at 100, got %v", steps)
	}
	return nil
}

// ceilPct returns ceil(total*pct/100), minimum 1 — the canary replica count
// for a given ramp-step percentage of the original live replica count.
func ceilPct(total, pct int) int {
	n := (total*pct + 99) / 100
	if n < 1 {
		n = 1
	}
	return n
}

// startRollout stages the first ramp step's canary count and scales live
// down to match. Rejects if the service already has a plain canary or
// another active rollout — a service may only be in one of {no canary,
// plain-staged canary, active rollout} at a time.
func (m *rolloutManager) startRollout(ctx context.Context, name string, req ReplaceServiceRequest, steps []int) (*rolloutState, error) {
	if req.Image == "" {
		return nil, fmt.Errorf("image is required")
	}
	if len(steps) == 0 {
		steps = append([]int(nil), defaultRolloutSteps...)
	}
	if err := validateRolloutSteps(steps); err != nil {
		return nil, err
	}

	lock := m.serviceLock(name)
	lock.Lock()
	defer lock.Unlock()

	if existing, ok := m.get(name); ok && rolloutActive(existing.Status) {
		return nil, fmt.Errorf("%q already has an active rollout — advance or abort it first", name)
	}

	var orig int
	if svc, ok := m.onboardedSvc(name); ok {
		if svc.Host == "" {
			return nil, fmt.Errorf("%q is managed-only (no route) — manage it via docker compose", name)
		}
		if svc.CanaryImage != "" {
			return nil, fmt.Errorf("%q already has a canary — promote or discard it first", name)
		}
		orig = svc.Replicas
		if orig == 0 {
			return nil, fmt.Errorf("service %q has no live replicas", name)
		}

		canaryCount := ceilPct(orig, steps[0])
		if err := m.dc.stageOnboardedCanary(ctx, name, req, canaryCount, m.onb, m.routesPath); err != nil {
			return nil, err
		}
		proxyRefresh(m.proxyURL)
		// canaryCount can reach or exceed orig at step 0 already (small orig,
		// ceilPct's floor-of-1) — nothing to scale down in that case; the
		// first advance detects target >= orig and promotes directly rather
		// than trying to scale live to zero via scaleOnboarded (which
		// refuses desired < 1).
		if canaryCount < orig {
			if err := m.dc.scaleOnboarded(ctx, name, orig-canaryCount, m.onb, m.routesPath); err != nil {
				return nil, err
			}
			proxyRefresh(m.proxyURL)
		}
	} else {
		all, err := m.dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
		if err != nil {
			return nil, err
		}
		if len(canaryOnly(all)) > 0 {
			return nil, fmt.Errorf("%q already has a canary — promote or discard it first", name)
		}
		orig = len(liveOnly(all))
		if orig == 0 {
			return nil, fmt.Errorf("service %q has no live replicas", name)
		}

		canaryCount := ceilPct(orig, steps[0])
		if err := m.dc.createCanaryReplicas(ctx, name, req, canaryCount); err != nil {
			return nil, err
		}
		if err := m.scaleLiveTo(ctx, name, orig-canaryCount); err != nil {
			return nil, err
		}
	}

	st := &rolloutState{
		Service: name, Image: req.Image, Steps: steps, StepIdx: 0,
		Status: rolloutStatusAwaitingAdvance, OrigLiveReplicas: orig, StartedAt: time.Now(),
	}
	m.mu.Lock()
	m.rollouts[name] = st
	m.mu.Unlock()
	cp := *st
	return &cp, nil
}

// advanceRollout is a thin locking wrapper around doAdvance — acquire the
// service lock, look up the active state, and hand off.
func (m *rolloutManager) advanceRollout(ctx context.Context, name string) (*rolloutState, error) {
	lock := m.serviceLock(name)
	lock.Lock()
	defer lock.Unlock()

	st, ok := m.get(name)
	if !ok || !rolloutActive(st.Status) {
		return nil, fmt.Errorf("%q has no active rollout", name)
	}
	return m.doAdvance(ctx, st)
}

// doAdvance health-gates the current canary, then either auto-rolls-back
// (unhealthy), promotes (healthy and either at the last step, or — for an
// onboarded service — the next step's target already reaches full live
// capacity), or scales up to the next ramp step.
//
// Caller must already hold the service's lock: both advanceRollout and
// checkOne call this after acquiring the lock themselves — locking here too
// would deadlock checkOne's call path.
func (m *rolloutManager) doAdvance(ctx context.Context, st *rolloutState) (*rolloutState, error) {
	healthy, reason, err := m.checkHealth(ctx, st.Service)
	if err != nil {
		return nil, err
	}
	if !healthy {
		return m.autoRollback(ctx, st, reason), nil
	}

	_, onboarded := m.onboardedSvc(st.Service)
	atLastStep := st.StepIdx == len(st.Steps)-1

	targetPct := st.Steps[st.StepIdx]
	if !atLastStep {
		targetPct = st.Steps[st.StepIdx+1]
	}
	target := ceilPct(st.OrigLiveReplicas, targetPct)

	// Onboarded has no valid "live==0, not-yet-promoted" intermediate state
	// (scaleOnboarded refuses desired < 1, unlike scaleService) — so the
	// moment the next step's target reaches full live capacity, cut straight
	// to promote instead of scaling live down. Always true at the actual
	// last step (ceilPct(orig,100) == orig), and can also be true one or
	// more steps early when a small OrigLiveReplicas makes ceilPct's
	// floor-of-1 reach full capacity ahead of schedule.
	if onboarded && target >= st.OrigLiveReplicas {
		if err := m.scaleCanaryTo(ctx, st.Service, target); err != nil {
			m.update(st.Service, func(r *rolloutState) { r.Status = rolloutStatusFailed; r.LastError = err.Error() })
			res, _ := m.get(st.Service)
			return res, nil
		}
		if err := m.promote(ctx, st.Service); err != nil {
			m.update(st.Service, func(r *rolloutState) { r.Status = rolloutStatusFailed; r.LastError = err.Error() })
			res, _ := m.get(st.Service)
			return res, nil
		}
		m.update(st.Service, func(r *rolloutState) { r.Status = rolloutStatusCompleted; r.LastError = "" })
		res, _ := m.get(st.Service)
		return res, nil
	}

	if atLastStep {
		if err := m.promote(ctx, st.Service); err != nil {
			m.update(st.Service, func(r *rolloutState) { r.Status = rolloutStatusFailed; r.LastError = err.Error() })
			res, _ := m.get(st.Service)
			return res, nil
		}
		m.update(st.Service, func(r *rolloutState) { r.Status = rolloutStatusCompleted; r.LastError = "" })
		res, _ := m.get(st.Service)
		return res, nil
	}

	if err := m.scaleCanaryTo(ctx, st.Service, target); err != nil {
		return nil, err
	}
	if err := m.scaleLiveTo(ctx, st.Service, st.OrigLiveReplicas-target); err != nil {
		return nil, err
	}
	m.update(st.Service, func(r *rolloutState) { r.StepIdx++ })
	res, _ := m.get(st.Service)
	return res, nil
}

// checkHealth dispatches a canary health check to the substrate-appropriate
// implementation — onboarded containers carry none of the proxy.* labels
// checkCanaryHealth keys off.
func (m *rolloutManager) checkHealth(ctx context.Context, name string) (bool, string, error) {
	if svc, ok := m.onboardedSvc(name); ok {
		return m.dc.checkOnboardedCanaryHealth(ctx, name, svc.Port)
	}
	return m.dc.checkCanaryHealth(ctx, name)
}

// scaleCanaryTo dispatches a canary scale to the substrate-appropriate
// implementation, refreshing the proxy afterward for the onboarded path
// (scaleCanary's label-managed callers are picked up by docker discovery on
// the next render; onboarded's static routes.json entry needs an explicit
// refresh).
func (m *rolloutManager) scaleCanaryTo(ctx context.Context, name string, target int) error {
	if _, ok := m.onboardedSvc(name); ok {
		if err := m.dc.scaleOnboardedCanary(ctx, name, target, m.onb, m.routesPath); err != nil {
			return err
		}
		proxyRefresh(m.proxyURL)
		return nil
	}
	return m.dc.scaleCanary(ctx, name, target)
}

// scaleLiveTo scales live replicas down to target, except when target is 0:
// neither scaleService nor scaleOnboarded may legitimately reach 0 (each
// refuses to remove a service's last non-removable "original" container), so
// a target of 0 here is a no-op, leaving live as-is until the rollout's
// final advance calls promote (which tears down live containers directly,
// with no such guard).
func (m *rolloutManager) scaleLiveTo(ctx context.Context, name string, target int) error {
	if target == 0 {
		return nil
	}
	if _, ok := m.onboardedSvc(name); ok {
		if err := m.dc.scaleOnboarded(ctx, name, target, m.onb, m.routesPath); err != nil {
			return err
		}
		proxyRefresh(m.proxyURL)
		return nil
	}
	return m.dc.scaleService(ctx, name, target)
}

// promote dispatches the final "canary becomes live" step. For onboarded,
// promoteOnboarded doesn't update svc.Replicas itself (only CanaryImage/
// CanaryReplicas), so a completed ramp would otherwise leave Replicas stuck
// at its pre-ramp value — SetReplicas corrects that after promoteOnboarded
// clears CanaryReplicas.
func (m *rolloutManager) promote(ctx context.Context, name string) error {
	if svc, ok := m.onboardedSvc(name); ok {
		finalCanaryCount := svc.CanaryReplicas
		if err := m.dc.promoteOnboarded(ctx, name, m.onb, m.routesPath); err != nil {
			return err
		}
		proxyRefresh(m.proxyURL)
		return m.onb.SetReplicas(name, finalCanaryCount)
	}
	return m.dc.promoteCanary(ctx, name)
}

// discard dispatches a canary discard to the substrate-appropriate
// implementation.
func (m *rolloutManager) discard(ctx context.Context, name string) error {
	if _, ok := m.onboardedSvc(name); ok {
		if err := m.dc.discardOnboarded(ctx, name, m.onb, m.routesPath); err != nil {
			return err
		}
		proxyRefresh(m.proxyURL)
		return nil
	}
	return m.dc.discardCanary(ctx, name)
}

// abortRollout rolls back immediately regardless of current canary health.
func (m *rolloutManager) abortRollout(ctx context.Context, name string) (*rolloutState, error) {
	lock := m.serviceLock(name)
	lock.Lock()
	defer lock.Unlock()

	st, ok := m.get(name)
	if !ok || !rolloutActive(st.Status) {
		return nil, fmt.Errorf("%q has no active rollout", name)
	}

	m.update(name, func(r *rolloutState) { r.Status = rolloutStatusRollingBack })
	if err := m.rollbackContainers(ctx, st); err != nil {
		return nil, err
	}
	m.update(name, func(r *rolloutState) { r.Status = rolloutStatusRolledBack; r.LastError = "" })
	res, _ := m.get(name)
	return res, nil
}

// rollbackContainers removes every canary container for the service and
// scales live back up to the rollout's original replica count — the shared
// mechanics behind both an auto-rollback (health gate failure) and a manual
// abort. Caller must already hold the service's lock.
func (m *rolloutManager) rollbackContainers(ctx context.Context, st *rolloutState) error {
	if svc, ok := m.onboardedSvc(st.Service); ok {
		if svc.CanaryImage != "" {
			if err := m.discard(ctx, st.Service); err != nil {
				return err
			}
		}
		return m.scaleLiveTo(ctx, st.Service, st.OrigLiveReplicas)
	}
	all, err := m.dc.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, st.Service))
	if err != nil {
		return err
	}
	if len(canaryOnly(all)) > 0 {
		if err := m.dc.discardCanary(ctx, st.Service); err != nil {
			return err
		}
	}
	return m.dc.scaleService(ctx, st.Service, st.OrigLiveReplicas)
}

// autoRollback is the shared failure path for both doAdvance's health check
// and the background ticker's between-steps check. Caller must already hold
// the service's lock.
func (m *rolloutManager) autoRollback(ctx context.Context, st *rolloutState, reason string) *rolloutState {
	m.update(st.Service, func(r *rolloutState) { r.Status = rolloutStatusRollingBack })
	if err := m.rollbackContainers(ctx, st); err != nil {
		log.Printf("rollout %s: auto-rollback failed: %v", st.Service, err)
		m.update(st.Service, func(r *rolloutState) {
			r.Status = rolloutStatusFailed
			r.LastError = fmt.Sprintf("auto-rollback failed after unhealthy canary (%s): %v", reason, err)
		})
	} else {
		m.update(st.Service, func(r *rolloutState) { r.Status = rolloutStatusFailed; r.LastError = reason })
	}
	res, _ := m.get(st.Service)
	return res
}

// Run polls every rolloutCheckInterval for services currently
// awaiting-advance and auto-rolls-back any whose canary has gone unhealthy
// since the last check — mirrors autoUpdater's ticker-driven Run(ctx) shape.
func (m *rolloutManager) Run(ctx context.Context) {
	t := time.NewTicker(rolloutCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.checkAll(ctx)
		}
	}
}

func (m *rolloutManager) checkAll(ctx context.Context) {
	m.mu.Lock()
	names := make([]string, 0, len(m.rollouts))
	for name, r := range m.rollouts {
		if r.Status == rolloutStatusAwaitingAdvance {
			names = append(names, name)
		}
	}
	m.mu.Unlock()
	sort.Strings(names)

	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		m.checkOne(ctx, name)
	}
}

func (m *rolloutManager) checkOne(ctx context.Context, name string) {
	lock := m.serviceLock(name)
	lock.Lock()
	defer lock.Unlock()

	st, ok := m.get(name)
	if !ok || st.Status != rolloutStatusAwaitingAdvance {
		return
	}

	healthy, reason, err := m.checkHealth(ctx, name)
	if err != nil {
		log.Printf("rollout %s: health check error: %v", name, err)
		return
	}
	if healthy {
		return
	}
	log.Printf("rollout %s: canary unhealthy (%s) — auto-rolling back", name, reason)
	m.autoRollback(ctx, st, reason)
	audit(nil, "rollout", "service.rollout_autorollback", name+": "+reason)
}

// containerIP returns a container's address on the managed ("edge") Docker
// network, falling back to any other network it's attached to — same
// resolution order docker.go's listRoutes/serviceBackends use.
func containerIP(ct dockerContainer) string {
	if n, ok := ct.NetworkSettings.Networks[managedNetwork]; ok && n.IPAddress != "" {
		return n.IPAddress
	}
	for _, n := range ct.NetworkSettings.Networks {
		if n.IPAddress != "" {
			return n.IPAddress
		}
	}
	return ""
}

// checkCanaryHealth reports whether every current canary container of a
// service looks healthy: no docker-reported "(unhealthy)" status, no more
// than canaryMaxRestarts restarts since creation, and (only if the
// container carries a proxy.health label) a 2xx from an HTTP probe against
// that CONTAINER's own edge-network address — deliberately not the
// service's public proxy.host, since a request to that hostname goes
// through the proxy itself, which round-robins across both live and canary
// and so could randomly hit a healthy live container and mask a broken
// canary. Returns false with a specific reason the moment any signal fails
// for any canary container — it does not need to check every container
// once one has failed.
func (c *dockerClient) checkCanaryHealth(ctx context.Context, name string) (bool, string, error) {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"label":["%s=%s"]}`, labelService, name))
	if err != nil {
		return false, "", err
	}
	canary := canaryOnly(all)
	if len(canary) == 0 {
		return false, fmt.Sprintf("%q has no canary containers", name), nil
	}
	for _, ct := range canary {
		healthy, reason, err := c.checkContainerHealthy(ctx, ct)
		if err != nil {
			return false, "", err
		}
		if !healthy {
			return false, reason, nil
		}
	}
	return true, "", nil
}

// checkContainerHealthy is checkCanaryHealth's per-container check, factored
// out so replaceServiceRolling's surge-of-one swap (docker.go) can gate each
// new replica on the same signals a canary rollout does: no docker-reported
// "(unhealthy)" status, no more than canaryMaxRestarts restarts since
// creation, and (only if the container carries a proxy.health label) a 2xx
// from an HTTP probe against that CONTAINER's own edge-network address —
// deliberately not the service's public proxy.host, since a request to that
// hostname goes through the proxy itself, which round-robins across live
// (and canary) containers and so could randomly hit a healthy sibling and
// mask a broken one.
func (c *dockerClient) checkContainerHealthy(ctx context.Context, ct dockerContainer) (bool, string, error) {
	if parseHealth(ct.Status) == "unhealthy" {
		return false, fmt.Sprintf("%s: docker healthcheck reports unhealthy", ct.name()), nil
	}
	restarts, err := c.inspectRestartCount(ctx, ct.ID)
	if err != nil {
		return false, "", fmt.Errorf("inspect %s: %w", ct.name(), err)
	}
	if restarts > canaryMaxRestarts {
		return false, fmt.Sprintf("%s: restarted %d times since creation", ct.name(), restarts), nil
	}

	healthPath := ct.Labels[labelHealth]
	if healthPath == "" {
		return true, "", nil
	}
	port := ct.Labels[labelPort]
	ip := containerIP(ct)
	if ip == "" || port == "" {
		return false, fmt.Sprintf("%s: no reachable address for HTTP health probe", ct.name()), nil
	}
	url := fmt.Sprintf("http://%s:%s%s", ip, port, healthPath)
	hctx, cancel := context.WithTimeout(ctx, canaryHealthProbeTimeout)
	req, _ := http.NewRequestWithContext(hctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	cancel()
	if err != nil {
		return false, fmt.Sprintf("%s: health probe %s failed: %v", ct.name(), url, err), nil
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return false, fmt.Sprintf("%s: health probe %s returned %d", ct.name(), url, resp.StatusCode), nil
	}
	return true, "", nil
}

// checkOnboardedCanaryHealth is checkCanaryHealth's onboarded-substrate
// counterpart. Onboarded containers carry none of the proxy.* labels
// checkCanaryHealth keys off, so canary discovery goes by name prefix
// instead (same idiom discardOnboarded uses), and the health probe is a
// bare TCP dial rather than an HTTP GET requiring a 2xx — OnboardedService
// has no health-path field/convention today, and requiring a 2xx from "/"
// would false-positive-rollback apps that redirect or 404 there. A real
// HTTP-path probe is a natural follow-up once OnboardedService gains a
// health-path field; not built here.
func (c *dockerClient) checkOnboardedCanaryHealth(ctx context.Context, name string, port int) (bool, string, error) {
	all, err := c.listAll(ctx, fmt.Sprintf(`{"name":["goproxy-onb-%s-c"]}`, name))
	if err != nil {
		return false, "", err
	}
	prefix := fmt.Sprintf("goproxy-onb-%s-c", name)
	var canary []dockerContainer
	for _, cl := range all {
		if strings.HasPrefix(cl.name(), prefix) {
			canary = append(canary, cl)
		}
	}
	if len(canary) == 0 {
		return false, fmt.Sprintf("%q has no canary containers", name), nil
	}
	for _, ct := range canary {
		if parseHealth(ct.Status) == "unhealthy" {
			return false, fmt.Sprintf("%s: docker healthcheck reports unhealthy", ct.name()), nil
		}
		restarts, err := c.inspectRestartCount(ctx, ct.ID)
		if err != nil {
			return false, "", fmt.Errorf("inspect %s: %w", ct.name(), err)
		}
		if restarts > canaryMaxRestarts {
			return false, fmt.Sprintf("%s: restarted %d times since creation", ct.name(), restarts), nil
		}

		addr := containerIP(ct)
		if addr == "" {
			addr = ct.name()
		}
		dialer := net.Dialer{Timeout: canaryHealthProbeTimeout}
		conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", addr, port))
		if err != nil {
			return false, fmt.Sprintf("%s: dial %s:%d failed: %v", ct.name(), addr, port, err), nil
		}
		conn.Close()
	}
	return true, "", nil
}

// waitForCanaryHealthy polls checkCanaryHealth every canaryPromoteHealthPoll
// up to canaryPromoteHealthTimeout — the pre-teardown gate promoteCanary
// must pass before it recreates the canary set as live and removes the old
// live containers.
func (c *dockerClient) waitForCanaryHealthy(ctx context.Context, name string) error {
	deadline := time.Now().Add(canaryPromoteHealthTimeout)
	var lastReason string
	for {
		healthy, reason, err := c.checkCanaryHealth(ctx, name)
		if err != nil {
			return fmt.Errorf("health check: %w", err)
		}
		if healthy {
			return nil
		}
		lastReason = reason
		if !time.Now().Before(deadline) {
			return fmt.Errorf("canary failed health gate: %s", lastReason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(canaryPromoteHealthPoll):
		}
	}
}
