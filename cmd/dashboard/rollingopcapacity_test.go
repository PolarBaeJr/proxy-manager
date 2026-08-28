package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oneReplicaApp/twoReplicaApp are minimal "app" fixtures for the capacity
// guard — labelEnable/labelService/labelHost/labelPort only, no docker
// healthcheck (so parseHealth("") counts as healthy, same convention as
// twoReplicaContainers elsewhere in this package).
func nReplicaApp(n int) []dockerContainer {
	labels := map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80"}
	var out []dockerContainer
	for i := 1; i <= n; i++ {
		name := "/app"
		if i > 1 {
			name = "/goproxy-app-" + string(rune('0'+i))
		}
		out = append(out, dockerContainer{ID: "c" + string(rune('0'+i)), Names: []string{name}, State: "running", Image: "ghcr.io/org/app:v1", Labels: labels})
	}
	return out
}

// TestEnsureRollingReplaceCapacityLocalOnly covers the no-peer-mesh case:
// refuse at 1 healthy replica total, allow at 2+.
func TestEnsureRollingReplaceCapacityLocalOnly(t *testing.T) {
	calls := &svcCallTracker{}
	dc1 := servicesDockerStub(t, calls, nReplicaApp(1))
	if err := ensureRollingReplaceCapacity(context.Background(), dc1, nil, "", "app"); err == nil {
		t.Fatal("want refusal at 1 healthy replica total")
	} else if !strings.Contains(err.Error(), "only 1 healthy replica") {
		t.Errorf("err = %v, want it to state the count", err)
	}

	dc2 := servicesDockerStub(t, calls, nReplicaApp(2))
	if err := ensureRollingReplaceCapacity(context.Background(), dc2, nil, "", "app"); err != nil {
		t.Errorf("want success at 2 healthy replicas, got %v", err)
	}
}

// TestEnsureRollingReplaceCapacityHandlerRefusesAt400 exercises the guard
// through the actual POST /api/services/{name}/rolling-replace handler, not
// just the free function.
func TestEnsureRollingReplaceCapacityHandlerRefusesAt400(t *testing.T) {
	calls := &svcCallTracker{}
	dc := servicesDockerStub(t, calls, nReplicaApp(1))
	onb := newTestOnboardedStore(t)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)
	rom := newRollingOpManager(dc)
	mux := newDashboardMux(dc, nil, auth, newRateLimiter(), newImageChecker(dc), "", nil, onb, nil, nil, nil, nil, nil, nil, nil, nil, rom)

	body := `{"image":"ghcr.io/org/app:v2"}`
	req := httptest.NewRequest("POST", "/api/services/app/rolling-replace", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s, want %d", rec.Code, rec.Body.String(), http.StatusBadRequest)
	}
}

// TestEnsureRollingReplaceCapacityPeerMesh covers the peer-forwarded case:
// local 1 + an unreachable peer must still refuse, and must say so in a way
// that doesn't look like a confirmed zero-redundancy service; local 1 + a
// reachable peer that simply doesn't run "app" must refuse too, but say so
// as a CONFIRMED total, not an undercount caveat (the peer did answer — it
// just has nothing matching); local 1 + peer 1 (both healthy) must succeed.
func TestEnsureRollingReplaceCapacityPeerMesh(t *testing.T) {
	localCalls := &svcCallTracker{}
	localDC := servicesDockerStub(t, localCalls, nReplicaApp(1))

	t.Run("peer unreachable undercounts but is not treated as a confirmed zero", func(t *testing.T) {
		deadPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		t.Cleanup(deadPeer.Close)
		reg := newPeerRegistry([]string{deadPeer.URL}, "s3cret", "dashboard-a", "dev", 0, nil)

		err := ensureRollingReplaceCapacity(context.Background(), localDC, reg, "s3cret", "app")
		if err == nil {
			t.Fatal("want refusal when the mesh total is still only 1")
		}
		if !strings.Contains(err.Error(), "1 locally") {
			t.Errorf("err = %v, want it to break out the local count", err)
		}
		if !strings.Contains(err.Error(), "unreachable") {
			t.Errorf("err = %v, want it to flag that an unreachable peer would undercount this", err)
		}
	})

	t.Run("peer reachable but running a different service is a confirmed total, not an undercount caveat", func(t *testing.T) {
		peerCalls := &svcCallTracker{}
		otherSvc := nReplicaApp(1)
		otherSvc[0].Labels = map[string]string{labelEnable: "true", labelService: "other", labelHost: "other.example", labelPort: "80"}
		peerDC := servicesDockerStub(t, peerCalls, otherSvc)
		peerOnb := newTestOnboardedStore(t)
		peerSrv := httptest.NewServer(peerServicesHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil))
		t.Cleanup(peerSrv.Close)
		reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)

		err := ensureRollingReplaceCapacity(context.Background(), localDC, reg, "s3cret", "app")
		if err == nil {
			t.Fatal("want refusal when the mesh total is still only 1 (the peer doesn't run app)")
		}
		if strings.Contains(err.Error(), "unreachable") {
			t.Errorf("err = %v, a reachable-but-non-matching peer must not read like an undercount caveat", err)
		}
		if !strings.Contains(err.Error(), "answered") {
			t.Errorf("err = %v, want it to state the total is confirmed (all peers answered)", err)
		}
	})

	t.Run("peer reachable with a healthy replica reaches the minimum", func(t *testing.T) {
		peerCalls := &svcCallTracker{}
		peerDC := servicesDockerStub(t, peerCalls, nReplicaApp(1))
		peerOnb := newTestOnboardedStore(t)
		peerSrv := httptest.NewServer(peerServicesHandler("s3cret", "dashboard-b", peerDC, peerOnb, newImageChecker(peerDC), nil))
		t.Cleanup(peerSrv.Close)
		reg := newPeerRegistry([]string{peerSrv.URL}, "s3cret", "dashboard-a", "dev", 0, nil)

		if err := ensureRollingReplaceCapacity(context.Background(), localDC, reg, "s3cret", "app"); err != nil {
			t.Errorf("want success at 1 local + 1 peer = 2, got %v", err)
		}
	})
}

// TestEnsureRollingReplaceCapacityNoPeersConfiguredMessage covers the
// no-peer-mesh case explicitly at the message level: the caveat must not
// mention peers at all when none are configured.
func TestEnsureRollingReplaceCapacityNoPeersConfiguredMessage(t *testing.T) {
	calls := &svcCallTracker{}
	dc := servicesDockerStub(t, calls, nReplicaApp(1))
	err := ensureRollingReplaceCapacity(context.Background(), dc, nil, "", "app")
	if err == nil {
		t.Fatal("want refusal at 1 healthy replica total")
	}
	if !strings.Contains(err.Error(), "no peers are configured") {
		t.Errorf("err = %v, want it to say plainly that no peers are configured", err)
	}
	if strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "answered") {
		t.Errorf("err = %v, no peers configured — must not use undercount/confirmed-mesh language", err)
	}
}

// sanity-check that nReplicaApp/servicesDockerStub actually decode into
// listServices the way the guard assumes, independent of the guard itself.
func TestNReplicaAppFixtureSanity(t *testing.T) {
	calls := &svcCallTracker{}
	dc := servicesDockerStub(t, calls, nReplicaApp(2))
	svcs, err := dc.listServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var b []byte
	b, _ = json.Marshal(svcs)
	if len(svcs) != 1 || svcs[0].Name != "app" || svcs[0].Replicas != 2 {
		t.Fatalf("unexpected services: %s", b)
	}
}
