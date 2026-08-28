package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// newRollingReplaceFailStub mirrors rollout_test.go's newRolloutDockerStub
// (create/start/stop/remove against a stateful rolloutFakeDocker), but can
// fail container creation for one specific new replica name — for testing
// replaceServiceRolling's progress callback against a genuine mid-swap
// failure. failCreateName == "" never fails.
func newRollingReplaceFailStub(t *testing.T, f *rolloutFakeDocker, failCreateName string) *dockerClient {
	t.Helper()
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode(f.list())
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/containers/create"):
			name := r.URL.Query().Get("name")
			if failCreateName != "" && name == failCreateName {
				http.Error(w, "simulated create failure", http.StatusInternalServerError)
				return
			}
			var body createBody
			_ = json.NewDecoder(r.Body).Decode(&body)
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
			fmt.Fprint(w, `{"Image":"sha256:abc","Config":{"Env":[]},"HostConfig":{"Mounts":[]},"NetworkSettings":{"Networks":{"edge":{}}},"RestartCount":0}`)
		case strings.Contains(r.URL.Path, "/images/create"):
			w.WriteHeader(http.StatusOK)
		default:
			w.Write([]byte("{}"))
		}
	}))
}

// TestReplaceServiceRollingTracksPerReplicaProgress proves the progress
// callback's replicaName/verdict, wired through to rollingOpState.Replicas
// by rollingop.go, actually reflects the NEW containers in swap order.
func TestReplaceServiceRollingTracksPerReplicaProgress(t *testing.T) {
	oldSettle := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = oldSettle })

	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 3)
	dc := newRollingReplaceFailStub(t, f, "")

	var got []rollingOpReplica
	progress := func(done, total int, replicaName, verdict string) {
		if replicaName == "" {
			return
		}
		got = append(got, rollingOpReplica{Name: replicaName, Verdict: verdict})
	}
	if err := dc.replaceServiceRolling(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, progress); err != nil {
		t.Fatalf("replaceServiceRolling: %v", err)
	}

	want := []string{"goproxy-app-4", "goproxy-app-5", "goproxy-app-6"}
	if len(got) != len(want) {
		t.Fatalf("progress recorded %+v, want %d entries in swap order", got, len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("Replicas[%d].Name = %q, want %q", i, got[i].Name, name)
		}
		if got[i].Verdict == "" {
			t.Errorf("Replicas[%d].Verdict is empty", i)
		}
	}
}

// TestReplaceServiceRollingProgressStopsAtFailure proves a mid-swap failure
// leaves progress recording only the replicas that actually succeeded
// before it — not the one that failed, and not any after it.
func TestReplaceServiceRollingProgressStopsAtFailure(t *testing.T) {
	oldSettle := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = oldSettle })

	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 3)
	// startIdx is max(existing indices)+1 = 4, so the second replica's new
	// container is goproxy-app-5 — fail its create.
	dc := newRollingReplaceFailStub(t, f, "goproxy-app-5")

	var got []rollingOpReplica
	progress := func(done, total int, replicaName, verdict string) {
		if replicaName == "" {
			return
		}
		got = append(got, rollingOpReplica{Name: replicaName, Verdict: verdict})
	}
	err := dc.replaceServiceRolling(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, progress)
	if err == nil {
		t.Fatal("want an error when a mid-swap create fails")
	}
	if len(got) != 1 || got[0].Name != "goproxy-app-4" {
		t.Fatalf("progress recorded %+v, want exactly [goproxy-app-4] (the failed replica must not appear)", got)
	}
}

// TestRollingOpManagerGetRaceOnReplicas proves get()'s deep copy of
// Replicas lets a concurrent GET read it while the job goroutine is still
// appending to it, cleanly under go test -race.
func TestRollingOpManagerGetRaceOnReplicas(t *testing.T) {
	oldSettle := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = oldSettle })

	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 5)
	dc := newRollingReplaceFailStub(t, f, "")
	rom := newRollingOpManager(dc)

	st, err := rom.start("app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if st.Status != rollingOpStatusRunning {
		t.Fatalf("initial status = %q, want %q", st.Status, rollingOpStatusRunning)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			cur, ok := rom.get("app")
			if !ok {
				continue
			}
			// Touch every element (not just the header) so a race on the
			// backing array would actually be observed.
			for _, r := range cur.Replicas {
				_ = r.Name
			}
			if !rollingOpActive(cur.Status) {
				return
			}
		}
	}()

	final := waitForRollingOpDone(t, rom, "app")
	<-done
	if final.Status != rollingOpStatusCompleted {
		t.Fatalf("status = %q, want %q (last error %q)", final.Status, rollingOpStatusCompleted, final.LastError)
	}
	if len(final.Replicas) != 5 {
		t.Fatalf("Replicas = %d entries, want 5", len(final.Replicas))
	}
}
