package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowCreateStub answers a single-replica "app" service, blocking the
// container create call on release so a test can deterministically observe
// a rollingOpManager job still "running" without racing a real sleep. Once
// released, the new replica joins subsequent /containers/json listings (with
// no docker healthcheck, so it clears waitReplicaReady's gate instantly) so
// the job actually reaches "completed" rather than "container disappeared".
func slowCreateStub(t *testing.T, release <-chan struct{}) *dockerClient {
	t.Helper()
	old := dockerContainer{
		ID: "old1", Names: []string{"/goproxy-app-1"}, State: "running", Image: "ghcr.io/org/app:v1",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
	}
	var mu sync.Mutex
	var created bool
	var newName string
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			list := []dockerContainer{old}
			mu.Lock()
			if created {
				list = append(list, dockerContainer{
					ID: "new1", Names: []string{"/" + newName}, State: "running", Image: "ghcr.io/org/app:v2",
					Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
				})
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(list)
		case strings.HasSuffix(r.URL.Path, "/old1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{}}, "HostConfig": map[string]any{}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			<-release
			mu.Lock()
			created = true
			newName = r.URL.Query().Get("name")
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"Id": "new1"})
		default:
			w.Write([]byte("{}"))
		}
	}))
}

func waitForRollingOpDone(t *testing.T, rom *rollingOpManager, name string) *rollingOpState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := rom.get(name); ok && !rollingOpActive(st.Status) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("rolling op %q did not finish within timeout", name)
	return nil
}

// TestRollingOpManagerRejectsConcurrentStart proves the "one mutation in
// flight per service" invariant the rest of the feature depends on: a second
// start while the first job is genuinely still running (blocked on its
// first Docker call, not just "recently started") must be rejected, but a
// start after the first job reaches a terminal state must succeed — a
// completed/failed job must not permanently occupy the slot.
func TestRollingOpManagerRejectsConcurrentStart(t *testing.T) {
	release := make(chan struct{})
	dc := slowCreateStub(t, release)
	rom := newRollingOpManager(dc)

	oldSettle := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = oldSettle })

	st, err := rom.start("app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"})
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if st.Status != rollingOpStatusRunning {
		t.Fatalf("initial status = %q, want %q", st.Status, rollingOpStatusRunning)
	}

	if _, err := rom.start("app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v3"}); err == nil {
		t.Fatal("second start while the first is still running should be rejected")
	} else if !strings.Contains(err.Error(), "already has an active rolling replace") {
		t.Errorf("err = %v, want mention of an active rolling replace", err)
	}

	close(release)
	final := waitForRollingOpDone(t, rom, "app")
	if final.Status != rollingOpStatusCompleted {
		t.Fatalf("after release: status = %q, want %q (last error %q)", final.Status, rollingOpStatusCompleted, final.LastError)
	}

	if _, err := rom.start("app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v4"}); err != nil {
		t.Errorf("start after the prior job completed should be allowed: %v", err)
	}
}
