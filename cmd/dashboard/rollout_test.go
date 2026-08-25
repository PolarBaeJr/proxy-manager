package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCeilPct(t *testing.T) {
	cases := []struct {
		total, pct, want int
	}{
		{4, 25, 1},
		{3, 25, 1},
		{5, 25, 2},
		{4, 50, 2},
		{4, 100, 4},
		{1, 25, 1}, // minimum-1 floor
		{2, 50, 1},
		{10, 10, 1},
		{10, 33, 4},
	}
	for _, c := range cases {
		if got := ceilPct(c.total, c.pct); got != c.want {
			t.Errorf("ceilPct(%d, %d) = %d, want %d", c.total, c.pct, got, c.want)
		}
	}
}

func TestValidateRolloutSteps(t *testing.T) {
	valid := [][]int{{25, 50, 100}, {100}, {10, 100}}
	for _, steps := range valid {
		if err := validateRolloutSteps(steps); err != nil {
			t.Errorf("validateRolloutSteps(%v) = %v, want nil", steps, err)
		}
	}
	invalid := [][]int{
		{50, 25, 100}, // not ascending
		{25, 25, 100}, // not strictly ascending
		{0, 100},      // out of (0,100]
		{101},         // out of (0,100]
		{25, 50},      // last step not 100
	}
	for _, steps := range invalid {
		if err := validateRolloutSteps(steps); err == nil {
			t.Errorf("validateRolloutSteps(%v) = nil, want an error", steps)
		}
	}
}

// ---- stateful docker fake for rollout manager / promoteCanary tests ----

// rolloutFakeDocker is a small in-memory Docker daemon stand-in that
// actually tracks container lifecycle (create/start/stop/remove), unlike
// this package's other stubs (servicesDockerStub etc.), which answer every
// GET with a fixed, never-changing container list — insufficient here since
// a rollout issues several sequential Docker calls whose later results must
// reflect earlier ones (e.g. a scale-up must be visible to the next
// listAll).
type rolloutFakeDocker struct {
	mu       sync.Mutex
	seq      int
	items    map[string]dockerContainer
	restarts map[string]int
}

func newRolloutFakeDocker() *rolloutFakeDocker {
	return &rolloutFakeDocker{items: map[string]dockerContainer{}, restarts: map[string]int{}}
}

func (f *rolloutFakeDocker) setRestarts(id string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts[id] = n
}

// seed adds a container directly (bypassing the create path) — used to set
// up a rollout's starting live replicas. seedLiveReplicas uses an all-clone
// naming scheme; seedLiveReplicasRealistic mixes in a non-removable
// "original", like every docker-compose-managed service actually looks.
func (f *rolloutFakeDocker) seed(c dockerContainer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[c.ID] = c
}

func (f *rolloutFakeDocker) list() []dockerContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dockerContainer, 0, len(f.items))
	for _, c := range f.items {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name() < out[j].name() })
	return out
}

func (f *rolloutFakeDocker) setStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.items[id]
	c.Status = status
	f.items[id] = c
}

func idFromContainersPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "containers" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func newRolloutDockerStub(t *testing.T, f *rolloutFakeDocker) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode(f.list())
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/containers/create"):
			var body createBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			name := r.URL.Query().Get("name")
			f.mu.Lock()
			f.seq++
			id := fmt.Sprintf("gen-%d", f.seq)
			nc := dockerContainer{ID: id, Names: []string{"/" + name}, Image: body.Image, State: "running", Labels: body.Labels}
			nc.NetworkSettings.Networks = map[string]struct {
				IPAddress string `json:"IPAddress"`
			}{managedNetwork: {IPAddress: fmt.Sprintf("10.10.0.%d", f.seq)}}
			f.items[id] = nc
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"Id": id})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/stop"):
			id := idFromContainersPath(r.URL.Path)
			f.mu.Lock()
			if c, ok := f.items[id]; ok {
				c.State = "exited"
				f.items[id] = c
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/"):
			id := idFromContainersPath(r.URL.Path)
			f.mu.Lock()
			delete(f.items, id)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := idFromContainersPath(r.URL.Path)
			f.mu.Lock()
			restarts := f.restarts[id]
			f.mu.Unlock()
			fmt.Fprintf(w, `{"Image":"sha256:abc","Config":{"Env":[]},"HostConfig":{"Mounts":[]},"NetworkSettings":{"Networks":{"edge":{}}},"RestartCount":%d}`, restarts)
		case strings.Contains(r.URL.Path, "/images/create"):
			w.WriteHeader(http.StatusOK)
		default:
			w.Write([]byte("{}"))
		}
	}))
}

// seedLiveReplicas populates n all-removable "goproxy-<name>-<i>" live
// replicas for the given service.
func seedLiveReplicas(f *rolloutFakeDocker, name string, n int) {
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("live-%s-%d", name, i)
		c := dockerContainer{
			ID: id, Names: []string{fmt.Sprintf("/goproxy-%s-%d", name, i)}, Image: "ghcr.io/org/" + name + ":v1", State: "running",
			Labels: map[string]string{labelEnable: "true", labelService: name, labelHost: name + ".example", labelPort: "80"},
		}
		c.NetworkSettings.Networks = map[string]struct {
			IPAddress string `json:"IPAddress"`
		}{managedNetwork: {IPAddress: fmt.Sprintf("10.0.0.%d", i)}}
		f.seed(c)
	}
}

// seedLiveReplicasRealistic mirrors twoReplicaContainers' shape: container 1
// is a non-goproxy-prefixed "original" (never removable by scaleService),
// the rest are goproxy-prefixed clones — the realistic state for any
// docker-compose-managed service, as opposed to seedLiveReplicas' all-clone
// fixture.
func seedLiveReplicasRealistic(f *rolloutFakeDocker, name string, n int) {
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("live-%s-%d", name, i)
		cname := "/" + name
		if i > 1 {
			cname = fmt.Sprintf("/goproxy-%s-%d", name, i)
		}
		c := dockerContainer{
			ID: id, Names: []string{cname}, Image: "ghcr.io/org/" + name + ":v1", State: "running",
			Labels: map[string]string{labelEnable: "true", labelService: name, labelHost: name + ".example", labelPort: "80"},
		}
		c.NetworkSettings.Networks = map[string]struct {
			IPAddress string `json:"IPAddress"`
		}{managedNetwork: {IPAddress: fmt.Sprintf("10.0.0.%d", i)}}
		f.seed(c)
	}
}

func countByRole(f *rolloutFakeDocker, name string) (live, canary int) {
	for _, c := range f.list() {
		if c.Labels[labelService] != name {
			continue
		}
		if c.Labels[labelCanary] == "true" {
			canary++
		} else {
			live++
		}
	}
	return
}

func TestStartRolloutComputesSplit(t *testing.T) {
	cases := []struct {
		orig       int
		steps      []int
		wantCanary int
		wantLive   int
	}{
		{4, []int{25, 50, 100}, 1, 3},
		{3, []int{25, 100}, 1, 2},
		{5, []int{25, 100}, 2, 3},
		{2, []int{50, 100}, 1, 1},
		// orig=1, step0=25 still rounds up to a full canary (ceilPct's
		// minimum-1 floor) — a target live count of 0 is a scaleLiveTo
		// no-op, so live stays at 1 until the eventual promote.
		{1, []int{25, 100}, 1, 1},
	}
	for _, c := range cases {
		f := newRolloutFakeDocker()
		seedLiveReplicas(f, "app", c.orig)
		dc := newRolloutDockerStub(t, f)
		rm := newRolloutManager(dc)

		st, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, c.steps)
		if err != nil {
			t.Fatalf("orig=%d steps=%v: startRollout: %v", c.orig, c.steps, err)
		}
		if st.OrigLiveReplicas != c.orig || st.StepIdx != 0 || st.Status != rolloutStatusAwaitingAdvance {
			t.Fatalf("orig=%d steps=%v: state = %+v", c.orig, c.steps, st)
		}
		live, canary := countByRole(f, "app")
		if live != c.wantLive || canary != c.wantCanary {
			t.Fatalf("orig=%d steps=%v: live=%d canary=%d, want live=%d canary=%d", c.orig, c.steps, live, canary, c.wantLive, c.wantCanary)
		}
	}
}

func TestStartRolloutRejectsExistingCanary(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 2)
	f.seed(dockerContainer{
		ID: "canary-1", Names: []string{"/goproxy-app-canary-1"}, Image: "ghcr.io/org/app:v2", State: "running",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80", labelCanary: "true"},
	})
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	if _, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, nil); err == nil {
		t.Fatal("startRollout should reject a service that already has a plain canary")
	} else if !strings.Contains(err.Error(), "already has a canary") {
		t.Fatalf("err = %v, want mention of an existing canary", err)
	}
}

func TestStartRolloutRejectsExistingRollout(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 4)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	if _, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, nil); err != nil {
		t.Fatalf("first startRollout: %v", err)
	}
	if _, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v3"}, nil); err == nil {
		t.Fatal("second startRollout should reject — an active rollout already exists")
	} else if !strings.Contains(err.Error(), "already has an active rollout") {
		t.Fatalf("err = %v, want mention of an active rollout", err)
	}
}

func TestStartRolloutRejectsMalformedSteps(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 4)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	if _, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, []int{50, 25, 100}); err == nil {
		t.Fatal("startRollout should reject non-ascending steps")
	}
}

func TestStartRolloutDefaultsSteps(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 4)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	st, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, nil)
	if err != nil {
		t.Fatalf("startRollout: %v", err)
	}
	if len(st.Steps) != 3 || st.Steps[0] != 25 || st.Steps[1] != 50 || st.Steps[2] != 100 {
		t.Fatalf("Steps = %v, want default [25 50 100]", st.Steps)
	}
}

// TestSingleStepRolloutToCompletion covers steps=[100]: step 0 is already
// the last step, so startRollout's own canary/live split hits the same
// live-target-0 case as an intermediate ramp step, and the very first
// advance immediately promotes.
func TestSingleStepRolloutToCompletion(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicasRealistic(f, "app", 4)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	st, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, []int{100})
	if err != nil {
		t.Fatalf("startRollout: %v", err)
	}
	if st.StepIdx != 0 {
		t.Fatalf("state after start = %+v", st)
	}
	live, canary := countByRole(f, "app")
	if live != 4 || canary != 4 {
		t.Fatalf("after start: live=%d canary=%d, want 4/4 (live untouched)", live, canary)
	}

	st, err = rm.advanceRollout(context.Background(), "app")
	if err != nil {
		t.Fatalf("advance (promote): %v", err)
	}
	if st.Status != rolloutStatusCompleted {
		t.Fatalf("status = %q, want %q (state %+v)", st.Status, rolloutStatusCompleted, st)
	}
	live, canary = countByRole(f, "app")
	if live != 4 || canary != 0 {
		t.Fatalf("after promote: live=%d canary=%d, want 4/0", live, canary)
	}
}

func TestAdvanceRolloutToCompletion(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 4)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	st, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, []int{25, 50, 100})
	if err != nil {
		t.Fatalf("startRollout: %v", err)
	}
	live, canary := countByRole(f, "app")
	if live != 3 || canary != 1 {
		t.Fatalf("after start: live=%d canary=%d, want 3/1", live, canary)
	}

	st, err = rm.advanceRollout(context.Background(), "app")
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if st.Status != rolloutStatusAwaitingAdvance || st.StepIdx != 1 {
		t.Fatalf("advance 1: state = %+v", st)
	}
	live, canary = countByRole(f, "app")
	if live != 2 || canary != 2 {
		t.Fatalf("after advance 1: live=%d canary=%d, want 2/2", live, canary)
	}

	st, err = rm.advanceRollout(context.Background(), "app")
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if st.Status != rolloutStatusAwaitingAdvance || st.StepIdx != 2 {
		t.Fatalf("advance 2: state = %+v", st)
	}
	// Live stays at 2 (unchanged), not 0: scaleLiveTo treats a target of 0 as
	// a no-op, since only promoteCanary may remove the last live replica.
	live, canary = countByRole(f, "app")
	if live != 2 || canary != 4 {
		t.Fatalf("after advance 2: live=%d canary=%d, want 2/4", live, canary)
	}

	st, err = rm.advanceRollout(context.Background(), "app")
	if err != nil {
		t.Fatalf("advance 3 (promote): %v", err)
	}
	if st.Status != rolloutStatusCompleted {
		t.Fatalf("advance 3: status = %q, want %q (state %+v)", st.Status, rolloutStatusCompleted, st)
	}
	live, canary = countByRole(f, "app")
	if live != 4 || canary != 0 {
		t.Fatalf("after promote: live=%d canary=%d, want 4/0", live, canary)
	}
}

// TestAdvanceRolloutToCompletionRealisticNaming is the same 3-step ramp as
// TestAdvanceRolloutToCompletion, but against a fixture with a real
// non-removable "original" container (like every docker-compose-managed
// service on the Pi) rather than all-goproxy-prefixed replicas. It proves
// the 100% ramp step no longer routes through scaleService (which would
// reject scaling live down to 0 whenever an original is present) and that
// promoteCanary still correctly removes the leftover live containers at
// the final advance.
func TestAdvanceRolloutToCompletionRealisticNaming(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicasRealistic(f, "app", 4)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	if _, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, []int{25, 50, 100}); err != nil {
		t.Fatalf("startRollout: %v", err)
	}
	live, canary := countByRole(f, "app")
	if live != 3 || canary != 1 {
		t.Fatalf("after start: live=%d canary=%d, want 3/1", live, canary)
	}

	st, err := rm.advanceRollout(context.Background(), "app")
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if st.StepIdx != 1 {
		t.Fatalf("advance 1: state = %+v", st)
	}
	live, canary = countByRole(f, "app")
	if live != 2 || canary != 2 {
		t.Fatalf("after advance 1: live=%d canary=%d, want 2/2", live, canary)
	}

	st, err = rm.advanceRollout(context.Background(), "app")
	if err != nil {
		t.Fatalf("advance 2 (100%% step, must not call scaleService(0)): %v", err)
	}
	if st.StepIdx != 2 {
		t.Fatalf("advance 2: state = %+v", st)
	}
	live, canary = countByRole(f, "app")
	if live != 2 || canary != 4 {
		t.Fatalf("after advance 2: live=%d canary=%d, want 2/4 (live untouched, incl. the original)", live, canary)
	}

	st, err = rm.advanceRollout(context.Background(), "app")
	if err != nil {
		t.Fatalf("advance 3 (promote): %v", err)
	}
	if st.Status != rolloutStatusCompleted {
		t.Fatalf("advance 3: status = %q, want %q (state %+v)", st.Status, rolloutStatusCompleted, st)
	}
	live, canary = countByRole(f, "app")
	if live != 4 || canary != 0 {
		t.Fatalf("after promote: live=%d canary=%d, want 4/0", live, canary)
	}
}

func TestAdvanceRolloutAutoRollbackOnUnhealthy(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 2)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	if _, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, []int{50, 100}); err != nil {
		t.Fatalf("startRollout: %v", err)
	}

	// Mark the (only) canary container unhealthy.
	var canaryID string
	for _, c := range f.list() {
		if c.Labels[labelCanary] == "true" {
			canaryID = c.ID
		}
	}
	if canaryID == "" {
		t.Fatal("no canary container found after startRollout")
	}
	f.setStatus(canaryID, "Up 1 minute (unhealthy)")

	st, err := rm.advanceRollout(context.Background(), "app")
	if err != nil {
		t.Fatalf("advanceRollout: %v", err)
	}
	if st.Status != rolloutStatusFailed {
		t.Fatalf("status = %q, want %q", st.Status, rolloutStatusFailed)
	}
	if !strings.Contains(st.LastError, "unhealthy") {
		t.Fatalf("LastError = %q, want mention of unhealthy", st.LastError)
	}
	live, canary := countByRole(f, "app")
	if live != 2 || canary != 0 {
		t.Fatalf("after auto-rollback: live=%d canary=%d, want 2/0 (restored to OrigLiveReplicas)", live, canary)
	}
}

func TestAbortRollout(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 4)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	if _, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, []int{25, 50, 100}); err != nil {
		t.Fatalf("startRollout: %v", err)
	}

	st, err := rm.abortRollout(context.Background(), "app")
	if err != nil {
		t.Fatalf("abortRollout: %v", err)
	}
	if st.Status != rolloutStatusRolledBack {
		t.Fatalf("status = %q, want %q", st.Status, rolloutStatusRolledBack)
	}
	live, canary := countByRole(f, "app")
	if live != 4 || canary != 0 {
		t.Fatalf("after abort: live=%d canary=%d, want 4/0", live, canary)
	}
}

func TestAbortRolloutIgnoresHealth(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 2)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	if _, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, []int{50, 100}); err != nil {
		t.Fatalf("startRollout: %v", err)
	}
	// Canary is perfectly healthy — abort must still roll back.
	st, err := rm.abortRollout(context.Background(), "app")
	if err != nil {
		t.Fatalf("abortRollout: %v", err)
	}
	if st.Status != rolloutStatusRolledBack {
		t.Fatalf("status = %q, want %q even though the canary was healthy", st.Status, rolloutStatusRolledBack)
	}
}

func TestAdvanceRolloutNoActiveRollout(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 2)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	if _, err := rm.advanceRollout(context.Background(), "app"); err == nil {
		t.Fatal("advanceRollout should error when no rollout is active")
	}
	if _, err := rm.abortRollout(context.Background(), "app"); err == nil {
		t.Fatal("abortRollout should error when no rollout is active")
	}
}

// TestCheckOneAutoRollback exercises the background ticker's own path
// (checkOne), distinct from advanceRollout's — proving auto-rollback also
// fires between manual advances, not just at the moment of one.
func TestCheckOneAutoRollback(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 2)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)

	if _, err := rm.startRollout(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, []int{50, 100}); err != nil {
		t.Fatalf("startRollout: %v", err)
	}
	var canaryID string
	for _, c := range f.list() {
		if c.Labels[labelCanary] == "true" {
			canaryID = c.ID
		}
	}
	if canaryID == "" {
		t.Fatal("no canary container found after startRollout")
	}
	f.setStatus(canaryID, "Up 1 minute (unhealthy)")

	rm.checkOne(context.Background(), "app")

	st, ok := rm.get("app")
	if !ok {
		t.Fatal("rollout state missing after checkOne")
	}
	if st.Status != rolloutStatusFailed {
		t.Fatalf("status = %q, want %q", st.Status, rolloutStatusFailed)
	}
	if !strings.Contains(st.LastError, "unhealthy") {
		t.Fatalf("LastError = %q, want mention of unhealthy", st.LastError)
	}
	live, canary := countByRole(f, "app")
	if live != 2 || canary != 0 {
		t.Fatalf("after checkOne auto-rollback: live=%d canary=%d, want 2/0", live, canary)
	}
}

// TestCheckCanaryHealthRestartCount proves a crash-looping canary (restarts
// above canaryMaxRestarts) is flagged unhealthy even though its reported
// docker status string looks fine.
func TestCheckCanaryHealthRestartCount(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 1)
	canaryID := "canary-1"
	f.seed(dockerContainer{
		ID: canaryID, Names: []string{"/goproxy-app-canary-1"}, Image: "ghcr.io/org/app:v2", State: "running",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80", labelCanary: "true"},
	})
	f.setRestarts(canaryID, canaryMaxRestarts+1)
	dc := newRolloutDockerStub(t, f)

	healthy, reason, err := dc.checkCanaryHealth(context.Background(), "app")
	if err != nil {
		t.Fatalf("checkCanaryHealth: %v", err)
	}
	if healthy {
		t.Fatal("checkCanaryHealth() = healthy, want unhealthy due to restart count")
	}
	if !strings.Contains(reason, "restarted") {
		t.Fatalf("reason = %q, want mention of restarts", reason)
	}
}

// TestCheckCanaryHealthHTTPProbe proves the health probe targets the
// canary CONTAINER's own edge-network address + proxy.port, never the
// public proxy.host — a probe through the public hostname would round-robin
// across live and canary and could mask a broken canary.
func TestCheckCanaryHealthHTTPProbe(t *testing.T) {
	var gotHost string
	var healthy200 = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		if healthy200 {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}

	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 1)
	canaryID := "canary-1"
	c := dockerContainer{
		ID: canaryID, Names: []string{"/goproxy-app-canary-1"}, Image: "ghcr.io/org/app:v2", State: "running",
		Labels: map[string]string{
			labelEnable: "true", labelService: "app", labelHost: "app.polardev.org", labelPort: port,
			labelCanary: "true", labelHealth: "/healthz",
		},
	}
	c.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{managedNetwork: {IPAddress: host}}
	f.seed(c)
	dc := newRolloutDockerStub(t, f)

	healthy, reason, err := dc.checkCanaryHealth(context.Background(), "app")
	if err != nil {
		t.Fatalf("checkCanaryHealth: %v", err)
	}
	if !healthy {
		t.Fatalf("checkCanaryHealth() = unhealthy (%s), want healthy on 200", reason)
	}
	if gotHost != host+":"+port {
		t.Fatalf("probe hit Host %q, want the container's own address %q (not proxy.host %q)", gotHost, host+":"+port, c.Labels[labelHost])
	}

	healthy200 = false
	healthy, reason, err = dc.checkCanaryHealth(context.Background(), "app")
	if err != nil {
		t.Fatalf("checkCanaryHealth: %v", err)
	}
	if healthy {
		t.Fatal("checkCanaryHealth() = healthy, want unhealthy on 500")
	}
	if !strings.Contains(reason, "500") {
		t.Fatalf("reason = %q, want mention of the 500 status", reason)
	}
}

// ---- promoteCanary health-gate fix ----

func withShortPromoteHealthGate(t *testing.T) {
	t.Helper()
	oldTimeout, oldPoll := canaryPromoteHealthTimeout, canaryPromoteHealthPoll
	canaryPromoteHealthTimeout = 20 * time.Millisecond
	canaryPromoteHealthPoll = 5 * time.Millisecond
	t.Cleanup(func() {
		canaryPromoteHealthTimeout = oldTimeout
		canaryPromoteHealthPoll = oldPoll
	})
}

func TestPromoteCanaryFailsHealthGateLeavesStateIntact(t *testing.T) {
	withShortPromoteHealthGate(t)

	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 1)
	var canaryID = "canary-1"
	c := dockerContainer{
		ID: canaryID, Names: []string{"/goproxy-app-canary-1"}, Image: "ghcr.io/org/app:v2", State: "running",
		Status: "Up 1 minute (unhealthy)",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80", labelCanary: "true"},
	}
	f.seed(c)
	dc := newRolloutDockerStub(t, f)

	if err := dc.promoteCanary(context.Background(), "app"); err == nil {
		t.Fatal("promoteCanary should fail when the canary never becomes healthy")
	}
	live, canary := countByRole(f, "app")
	if live != 1 || canary != 1 {
		t.Fatalf("after failed promote: live=%d canary=%d, want 1/1 (nothing torn down)", live, canary)
	}
}

func TestPromoteCanarySucceedsWhenHealthy(t *testing.T) {
	withShortPromoteHealthGate(t)

	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 1)
	f.seed(dockerContainer{
		ID: "canary-1", Names: []string{"/goproxy-app-canary-1"}, Image: "ghcr.io/org/app:v2", State: "running",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80", labelCanary: "true"},
	})
	dc := newRolloutDockerStub(t, f)

	if err := dc.promoteCanary(context.Background(), "app"); err != nil {
		t.Fatalf("promoteCanary: %v", err)
	}
	live, canary := countByRole(f, "app")
	if live != 1 || canary != 0 {
		t.Fatalf("after promote: live=%d canary=%d, want 1/0", live, canary)
	}
}

// ---- HTTP endpoint tests ----

func newRolloutTestMux(t *testing.T, dc *dockerClient, rm *rolloutManager) http.Handler {
	t.Helper()
	onb := newTestOnboardedStore(t)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)
	return newDashboardMux(dc, nil, auth, newRateLimiter(), newImageChecker(dc), "", nil, onb, nil, nil, nil, nil, nil, nil, rm)
}

func doJSONReq(mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestRolloutEndpointsHappyPath(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 4)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)
	mux := newRolloutTestMux(t, dc, rm)

	rec := doJSONReq(mux, http.MethodGet, "/api/services/app/rollout", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET before start: status = %d, want 404", rec.Code)
	}

	rec = doJSONReq(mux, http.MethodPost, "/api/services/app/rollout", `{"image":"ghcr.io/org/app:v2","steps":[25,50,100]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST rollout: status = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = doJSONReq(mux, http.MethodGet, "/api/services/app/rollout", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after start: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var st rolloutState
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Status != rolloutStatusAwaitingAdvance || st.StepIdx != 0 {
		t.Fatalf("state after start = %+v", st)
	}

	rec = doJSONReq(mux, http.MethodPost, "/api/services/app/rollout/advance", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("advance: status = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = doJSONReq(mux, http.MethodPost, "/api/services/app/rollout/abort", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("abort: status = %d, body %s", rec.Code, rec.Body.String())
	}
	live, canary := countByRole(f, "app")
	if live != 4 || canary != 0 {
		t.Fatalf("after abort via HTTP: live=%d canary=%d, want 4/0", live, canary)
	}
}

func TestRolloutEndpointsAdvanceAbortNoActiveRollout(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 2)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)
	mux := newRolloutTestMux(t, dc, rm)

	rec := doJSONReq(mux, http.MethodPost, "/api/services/app/rollout/advance", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("advance with no active rollout: status = %d, want 400", rec.Code)
	}
	rec = doJSONReq(mux, http.MethodPost, "/api/services/app/rollout/abort", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("abort with no active rollout: status = %d, want 400", rec.Code)
	}
}

func TestRolloutEndpointsRequireAuth(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 2)
	dc := newRolloutDockerStub(t, f)
	rm := newRolloutManager(dc)
	mux := newRolloutTestMux(t, dc, rm)

	paths := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/services/app/rollout"},
		{http.MethodGet, "/api/services/app/rollout"},
		{http.MethodPost, "/api/services/app/rollout/advance"},
		{http.MethodPost, "/api/services/app/rollout/abort"},
	}
	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without auth: status = %d, want 401", p.method, p.path, rec.Code)
		}
	}
}

// TestRolloutStartSelfGuardRejects proves the shared self-guard (already
// applied to every /api/services/{name}/* mutation) also covers the new
// rollout-start route — a rollout must not be startable against the
// dashboard's own container.
func TestRolloutStartSelfGuardRejects(t *testing.T) {
	withSelfHostname(t, func() (string, error) { return "abc123def456", nil })
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"Id":"abc123def456","Names":["/dashboard"],"State":"running","Labels":{"proxy.service":"dashboard","proxy.host":"dashboard.example","proxy.port":"8093"}}]`))
	}))
	rm := newRolloutManager(dc)
	mux := newRolloutTestMux(t, dc, rm)

	rec := doJSONReq(mux, http.MethodPost, "/api/services/dashboard/rollout", `{"image":"ghcr.io/org/dashboard:v2"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
}
