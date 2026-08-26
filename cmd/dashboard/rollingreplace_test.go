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

// TestReplaceServiceRollingNeverDropsCapacityBelowOriginal is the core
// surge-of-one contract: at no point during a rolling replace should the
// live replica count for the service drop below the original count — it may
// briefly rise to N+1 (the new replica created before its predecessor is
// removed), but never fall to N-1. Hooks the fake daemon's create/stop
// handlers to snapshot the live count at the two moments that matter: right
// after a replacement is created (the surge peak) and right before its
// predecessor is stopped (the last moment before capacity would dip if the
// ordering were wrong).
func TestReplaceServiceRollingNeverDropsCapacityBelowOriginal(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicasRealistic(f, "app", 3)

	oldSettle := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = oldSettle })

	var mu sync.Mutex
	var peaks, troughs []int
	countLive := func() int {
		n := 0
		for _, c := range f.list() {
			if c.Labels[labelService] == "app" && c.State == "running" {
				n++
			}
		}
		return n
	}

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			mu.Lock()
			peaks = append(peaks, countLive())
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"Id": id})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/stop"):
			id := idFromContainersPath(r.URL.Path)
			mu.Lock()
			troughs = append(troughs, countLive())
			mu.Unlock()
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

	if err := dc.replaceServiceRolling(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, nil); err != nil {
		t.Fatalf("replaceServiceRolling: %v", err)
	}

	if len(peaks) != 3 || len(troughs) != 3 {
		t.Fatalf("expected 3 create/stop events each, got peaks=%v troughs=%v", peaks, troughs)
	}
	for i, p := range peaks {
		if p != 4 {
			t.Errorf("peak %d (right after creating the replacement) = %d, want 4 (surge to N+1)", i, p)
		}
	}
	for i, tr := range troughs {
		if tr != 4 {
			t.Errorf("trough %d (right before stopping the predecessor) = %d, want 4 (old+new both still counted)", i, tr)
		}
	}
	if final := countLive(); final != 3 {
		t.Errorf("final live count = %d, want 3", final)
	}
}

// TestReplaceServiceRollingMidLoopFailureLeavesFirstSwapped drives a 3-replica
// rolling replace whose SECOND replacement fails to create, and confirms:
// the error names exactly which replica failed and how many completed, the
// first replica's swap (which finished before the failure) is NOT rolled
// back, and the third replica (never reached) is untouched. No rollback is
// correct here — surge-of-one never dropped capacity, so a partial state is
// still fully serving traffic.
func TestReplaceServiceRollingMidLoopFailureLeavesFirstSwapped(t *testing.T) {
	f := newRolloutFakeDocker()
	seedLiveReplicas(f, "app", 3) // goproxy-app-1/2/3, all removable clones

	oldSettle := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = oldSettle })

	var mu sync.Mutex
	createCalls := 0

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode(f.list())
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/containers/create"):
			mu.Lock()
			createCalls++
			n := createCalls
			mu.Unlock()
			if n == 2 {
				http.Error(w, "no space left on device", http.StatusInternalServerError)
				return
			}
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

	// Original replicas occupy indices 1-3, so nextReplicaIndex starts the
	// replacement set at 4: goproxy-app-4 replaces -1 (succeeds), -5 replaces
	// -2 (the injected failure), -6 replaces -3 (never attempted).
	err := dc.replaceServiceRolling(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, nil)
	if err == nil {
		t.Fatal("expected an error from the failed 2nd create")
	}
	if !strings.Contains(err.Error(), "replaced 1/3 replicas") {
		t.Errorf("err = %v, want it to report replaced 1/3", err)
	}
	if !strings.Contains(err.Error(), "goproxy-app-5") {
		t.Errorf("err = %v, want it to name the failed replica goproxy-app-5", err)
	}

	byName := map[string]dockerContainer{}
	for _, c := range f.list() {
		byName[c.name()] = c
	}
	if _, ok := byName["goproxy-app-1"]; ok {
		t.Errorf("old replica 1 should have been removed after its successful swap: %v", byName)
	}
	if c, ok := byName["goproxy-app-4"]; !ok || c.Image != "ghcr.io/org/app:v2" {
		t.Errorf("replica 1's replacement (goproxy-app-4) missing or on the wrong image: %v", byName)
	}
	if _, ok := byName["goproxy-app-2"]; !ok {
		t.Errorf("old replica 2 should still be present — its swap never completed: %v", byName)
	}
	if _, ok := byName["goproxy-app-3"]; !ok {
		t.Errorf("old replica 3 should be untouched: %v", byName)
	}
}

// TestWaitReplicaReadyTimesOutOnStuckHealthStarting is the regression for the
// live badminton-admin healthcheck timing gap: a freshly created replica
// whose Docker healthcheck never leaves "(health: starting)" (as can happen
// past canaryPromoteHealthTimeout depending on the image's configured
// start_period/interval) must fail the health gate — naming the stuck
// replica — rather than hanging forever or, worse, being treated as healthy.
// It must also leave the predecessor completely untouched: surge-of-one's
// safety property only holds if a failed health gate blocks the teardown
// step, not just the create step.
func TestWaitReplicaReadyTimesOutOnStuckHealthStarting(t *testing.T) {
	oldTimeout, oldPoll, oldSettle := rollingReadyTimeout, canaryPromoteHealthPoll, replaceSettleDelay
	rollingReadyTimeout = 30 * time.Millisecond
	canaryPromoteHealthPoll = 5 * time.Millisecond
	replaceSettleDelay = 0
	t.Cleanup(func() {
		rollingReadyTimeout = oldTimeout
		canaryPromoteHealthPoll = oldPoll
		replaceSettleDelay = oldSettle
	})

	var mu sync.Mutex
	var created bool
	var newName string
	var oldTouched bool

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			list := []dockerContainer{{
				ID: "old1", Names: []string{"/goproxy-app-1"}, State: "running", Image: "ghcr.io/org/app:v1",
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
			}}
			mu.Lock()
			if created {
				list = append(list, dockerContainer{
					ID: "new1", Names: []string{"/" + newName}, State: "running",
					Status: "Up 1 second (health: starting)", Image: "ghcr.io/org/app:v2",
					Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
				})
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(list)
		case strings.HasSuffix(r.URL.Path, "/old1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{}}, "HostConfig": map[string]any{}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			mu.Lock()
			created = true
			newName = r.URL.Query().Get("name")
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"Id": "new1"})
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/old1"):
			// stopContainer/removeContainer against the predecessor — must
			// never be reached since waitReplicaReady fails first.
			mu.Lock()
			oldTouched = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.Write([]byte("{}"))
		}
	}))

	err := dc.replaceServiceRolling(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, nil)
	if err == nil {
		t.Fatal("expected the stuck health-starting replica to fail the gate")
	}
	if !strings.Contains(err.Error(), "healthcheck still starting") {
		t.Errorf("err = %v, want it to mention the stuck healthcheck", err)
	}
	if !strings.Contains(err.Error(), "replaced 0/1 replicas") {
		t.Errorf("err = %v, want it to report replaced 0/1", err)
	}
	mu.Lock()
	touched := oldTouched
	mu.Unlock()
	if touched {
		t.Error("predecessor was stopped/removed despite the replacement never becoming healthy")
	}
}

// TestWaitReplicaReadyPassesOnceHealthy is the positive counterpart to
// TestWaitReplicaReadyTimesOutOnStuckHealthStarting: a replica that reports
// "(health: starting)" on its first few polls and then "(healthy)" must let
// the gate proceed — the predecessor removed, the swap counted as a success —
// rather than only ever being exercised through the failure path. This is
// also the case that would silently degrade to a no-op gate if a container's
// Docker healthcheck were ever dropped on recreate (see
// TestReplaceServiceCarriesHealthcheckForward in replace_env_test.go): with
// no healthcheck, Status never reports "(health: starting)" at all and
// checkContainerHealthy passes on the very first poll instead of waiting for
// the transition asserted here.
func TestWaitReplicaReadyPassesOnceHealthy(t *testing.T) {
	oldTimeout, oldPoll, oldSettle := rollingReadyTimeout, canaryPromoteHealthPoll, replaceSettleDelay
	rollingReadyTimeout = time.Second
	canaryPromoteHealthPoll = 5 * time.Millisecond
	replaceSettleDelay = 0
	t.Cleanup(func() {
		rollingReadyTimeout = oldTimeout
		canaryPromoteHealthPoll = oldPoll
		replaceSettleDelay = oldSettle
	})

	var mu sync.Mutex
	var created bool
	var newName string
	var polls int
	var oldRemoved bool

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			list := []dockerContainer{{
				ID: "old1", Names: []string{"/goproxy-app-1"}, State: "running", Image: "ghcr.io/org/app:v1",
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
			}}
			mu.Lock()
			if created {
				polls++
				status := "Up 1 second (health: starting)"
				if polls >= 3 {
					status = "Up 3 seconds (healthy)"
				}
				list = append(list, dockerContainer{
					ID: "new1", Names: []string{"/" + newName}, State: "running",
					Status: status, Image: "ghcr.io/org/app:v2",
					Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"},
				})
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(list)
		case strings.HasSuffix(r.URL.Path, "/old1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": []string{}}, "HostConfig": map[string]any{}})
		case strings.Contains(r.URL.Path, "/containers/create"):
			mu.Lock()
			created = true
			newName = r.URL.Query().Get("name")
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"Id": "new1"})
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/old1"):
			mu.Lock()
			oldRemoved = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.Write([]byte("{}"))
		}
	}))

	if err := dc.replaceServiceRolling(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}, nil); err != nil {
		t.Fatalf("replaceServiceRolling: %v", err)
	}
	mu.Lock()
	removed := oldRemoved
	seenPolls := polls
	mu.Unlock()
	if !removed {
		t.Error("predecessor was never removed despite the replacement becoming healthy")
	}
	if seenPolls < 3 {
		t.Errorf("gate returned after %d poll(s), want it to have actually waited for the healthy transition", seenPolls)
	}
}
