package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// overlapFake is a stateful fake daemon for the proxy.overlap paths: it
// tracks containers, records every mutating call in order (plus "refresh"
// from the overlapRefresh hook), and serves one configurable inspect body.
type overlapFake struct {
	mu        sync.Mutex
	seq       int
	items     map[string]dockerContainer
	calls     []string
	created   []createBody
	inspect   string
	newStatus string
}

const overlapInspectHealthy = `{"Name":"/app","Image":"sha256:abc","Config":{"Env":["A=1"],"Image":"ghcr.io/org/app:v1","Healthcheck":{"Test":["CMD","true"]}},"HostConfig":{"Mounts":[]},"Mounts":[],"NetworkSettings":{"Networks":{"edge":{}}},"RestartCount":0}`

func overlapLabels(extra map[string]string) map[string]string {
	l := map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "80", labelUnscalable: "true", labelOverlap: "true"}
	for k, v := range extra {
		if v == "" {
			delete(l, k)
			continue
		}
		l[k] = v
	}
	return l
}

func newOverlapFake(t *testing.T, labels map[string]string) (*overlapFake, *dockerClient) {
	t.Helper()
	f := &overlapFake{items: map[string]dockerContainer{}, inspect: overlapInspectHealthy, newStatus: "Up 1 second (healthy)"}
	old := dockerContainer{ID: "old", Names: []string{"/app"}, Image: "ghcr.io/org/app:v1", State: "running", Status: "Up 1 hour (healthy)", Labels: labels}
	old.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{managedNetwork: {IPAddress: "10.0.0.1"}}
	f.items["old"] = old

	oldSettle, oldReady, oldPoll, oldRefresh := replaceSettleDelay, rollingReadyTimeout, canaryPromoteHealthPoll, overlapRefresh
	replaceSettleDelay = 0
	rollingReadyTimeout = 100 * time.Millisecond
	canaryPromoteHealthPoll = 5 * time.Millisecond
	overlapRefresh = func() { f.record("refresh") }
	t.Cleanup(func() {
		replaceSettleDelay, rollingReadyTimeout, canaryPromoteHealthPoll, overlapRefresh = oldSettle, oldReady, oldPoll, oldRefresh
	})
	t.Setenv("PROXY_URL", noopProxyStub(t))

	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := strings.TrimPrefix(r.URL.RequestURI(), "/"+dockerAPI)
		id := idFromContainersPath(r.URL.Path)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode(f.list())
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/containers/"):
			f.mu.Lock()
			body := f.inspect
			f.mu.Unlock()
			w.Write([]byte(body))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/containers/create"):
			var body createBody
			_ = json.NewDecoder(r.Body).Decode(&body)
			name := r.URL.Query().Get("name")
			f.mu.Lock()
			f.seq++
			nid := fmt.Sprintf("gen-%d", f.seq)
			nc := dockerContainer{ID: nid, Names: []string{"/" + name}, Image: body.Image, State: "running", Status: f.newStatus, Labels: body.Labels}
			nc.NetworkSettings.Networks = map[string]struct {
				IPAddress string `json:"IPAddress"`
			}{managedNetwork: {IPAddress: fmt.Sprintf("10.10.0.%d", f.seq)}}
			f.items[nid] = nc
			f.created = append(f.created, body)
			f.calls = append(f.calls, "create "+name)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"Id": nid})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/start"):
			f.record("start " + id)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/stop"):
			f.mu.Lock()
			f.calls = append(f.calls, "stop "+id+"?"+r.URL.RawQuery)
			if c, ok := f.items[id]; ok {
				c.State = "exited"
				f.items[id] = c
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			f.mu.Lock()
			call := "delete " + id
			if r.URL.Query().Get("force") == "true" {
				call += "?force"
			}
			f.calls = append(f.calls, call)
			delete(f.items, id)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(uri, "/images/create"):
			f.record("pull")
			w.WriteHeader(http.StatusOK)
		default:
			w.Write([]byte("{}"))
		}
	}))
	return f, dc
}

func (f *overlapFake) record(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *overlapFake) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *overlapFake) list() []dockerContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dockerContainer, 0, len(f.items))
	for _, c := range f.items {
		out = append(out, c)
	}
	return out
}

func (f *overlapFake) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.items[id]
	return ok
}

func (f *overlapFake) setInspect(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspect = s
}

func (f *overlapFake) setNewStatus(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newStatus = s
}

func (f *overlapFake) createdBodies() []createBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]createBody(nil), f.created...)
}

func (f *overlapFake) assertNoCreate(t *testing.T) {
	t.Helper()
	for _, c := range f.got() {
		if strings.HasPrefix(c, "create ") || strings.HasPrefix(c, "stop old") || strings.HasPrefix(c, "delete old") {
			t.Fatalf("calls = %v, want no create and the old container untouched", f.got())
		}
	}
}

func overlapMux(t *testing.T, dc *dockerClient, rom *rollingOpManager) http.Handler {
	t.Helper()
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	setInternalToken(t)
	return newDashboardMux(dc, nil, auth, newRateLimiter(), newImageChecker(dc), "", nil, newTestOnboardedStore(t), nil, nil, nil, nil, nil, nil, nil, nil, rom)
}

func waitRollingTerminal(t *testing.T, rom *rollingOpManager, name string) *rollingOpState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := rom.get(name); ok && !rollingOpActive(st.Status) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("rolling op never reached a terminal state")
	return nil
}

func TestOverlapEnabled(t *testing.T) {
	for _, c := range []struct {
		labels map[string]string
		want   bool
	}{
		{nil, false},
		{map[string]string{labelOverlap: "true"}, false},
		{map[string]string{labelUnscalable: "true"}, false},
		{map[string]string{labelUnscalable: "true", labelOverlap: "true"}, true},
		{map[string]string{labelUnscalable: "true", labelOverlap: " TRUE "}, true},
		{map[string]string{labelUnscalable: "true", labelOverlap: "yes"}, false},
		{map[string]string{labelUnscalable: "TRUE", labelOverlap: "true"}, false},
	} {
		if got := overlapEnabled(c.labels); got != c.want {
			t.Errorf("overlapEnabled(%v) = %v, want %v", c.labels, got, c.want)
		}
	}
}

func TestAnonymousVolumesAndHealthSignal(t *testing.T) {
	clone := cloneSpec{
		Mounts:      []mountSpec{{Type: "volume", Source: "appdata", Target: "/data"}, {Type: "bind", Source: "/srv/x", Target: "/x"}, {Type: "volume", Target: "/anon"}},
		VolumeDests: []string{"/data", "/anon", "/var/lib/image-volume"},
	}
	if got, want := anonymousVolumes(clone), []string{"/anon", "/var/lib/image-volume"}; !reflect.DeepEqual(got, want) {
		t.Errorf("anonymousVolumes = %v, want %v", got, want)
	}
	if got := anonymousVolumes(cloneSpec{Mounts: []mountSpec{{Type: "volume", Source: "appdata", Target: "/data"}}, VolumeDests: []string{"/data"}}); len(got) != 0 {
		t.Errorf("named volume reported anonymous: %v", got)
	}
	if hasHealthSignal(nil, nil) || hasHealthSignal(&healthcheckSpec{Test: []string{"NONE"}}, nil) {
		t.Error("no healthcheck / NONE counted as a health signal")
	}
	if !hasHealthSignal(&healthcheckSpec{Test: []string{"CMD", "true"}}, nil) || !hasHealthSignal(nil, map[string]string{labelHealth: "/healthz"}) {
		t.Error("HEALTHCHECK or proxy.health not counted as a health signal")
	}
}

// An unscalable service without proxy.overlap keeps the old restart:
// drain-stop then start the same container, nothing created.
func TestRestartUnlabelledUnscalableStopsThenStarts(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(map[string]string{labelOverlap: ""}))
	mux := overlapMux(t, dc, newRollingOpManager(dc))
	rec := doJSONReq(mux, "POST", "/api/services/app/replicas/app/restart", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"stop-start"`) {
		t.Fatalf("restart = %d %s, want 200 stop-start", rec.Code, rec.Body)
	}
	if got, want := f.got(), []string{"stop old?t=30", "start old"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// proxy.overlap without proxy.unscalable is a no-op.
func TestOverlapLabelOnScalableServiceIsNoop(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(map[string]string{labelUnscalable: ""}))
	svcs, err := dc.listServices(context.Background())
	if err != nil || len(svcs) != 1 || svcs[0].Overlap {
		t.Fatalf("listServices = %+v, %v; want one service with Overlap false", svcs, err)
	}
	mux := overlapMux(t, dc, newRollingOpManager(dc))
	rec := doJSONReq(mux, "POST", "/api/services/app/replicas/app/restart", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"stop-start"`) {
		t.Fatalf("restart = %d %s, want 200 stop-start", rec.Code, rec.Body)
	}
	if got, want := f.got(), []string{"stop old?t=30", "start old"}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// The overlap restart's whole contract: create and start the new copy,
// refresh the proxy once it is healthy, and only then drain the old one —
// from the creating reference (not the decayed list digest), without a pull
// and without stamping proxy.previous_image.
func TestOverlapRestartSurgeOrder(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(map[string]string{labelPrevImage: "ghcr.io/org/app:v0"}))
	f.mu.Lock()
	c := f.items["old"]
	c.Image = "sha256:deadbeef"
	f.items["old"] = c
	f.mu.Unlock()
	svcs, _ := dc.listServices(context.Background())
	if len(svcs) != 1 || !svcs[0].Overlap {
		t.Fatalf("listServices = %+v, want Overlap true", svcs)
	}

	rom := newRollingOpManager(dc)
	mux := overlapMux(t, dc, rom)
	rec := doJSONReq(mux, "POST", "/api/services/app/replicas/app/restart", "")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"mode":"overlap"`) || !strings.Contains(rec.Body.String(), "currently points to") {
		t.Fatalf("restart = %d %s, want 202 overlap with the image note", rec.Code, rec.Body)
	}
	st := waitRollingTerminal(t, rom, "app")
	if st.Status != rollingOpStatusCompleted || st.Kind != rollingKindRestart || !strings.Contains(st.Note, "not re-pulled") {
		t.Fatalf("state = %+v, want completed restart with the image note", st)
	}
	want := []string{"create goproxy-app-1", "start gen-1", "refresh", "stop old?t=30", "delete old"}
	if got := f.got(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	created := f.createdBodies()
	if len(created) != 1 || created[0].Image != "ghcr.io/org/app:v1" {
		t.Fatalf("created = %+v, want one copy from ghcr.io/org/app:v1", created)
	}
	if got := created[0].Labels[labelPrevImage]; got != "ghcr.io/org/app:v0" {
		t.Errorf("proxy.previous_image = %q, want the template's own v0 carried, not a restart stamp", got)
	}
}

// A new copy that never becomes healthy is stopped and removed; the old one
// is never touched and the job says so.
func TestOverlapRestartGateFailureRollsBack(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(nil))
	f.setNewStatus("Up 1 second (unhealthy)")
	rom := newRollingOpManager(dc)
	rec := doJSONReq(overlapMux(t, dc, rom), "POST", "/api/services/app/replicas/app/restart", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restart = %d %s, want 202", rec.Code, rec.Body)
	}
	st := waitRollingTerminal(t, rom, "app")
	if st.Status != rollingOpStatusFailed || st.Kind != rollingKindRestart || !strings.Contains(st.LastError, "new replica never became healthy — removed; old replica still serving") {
		t.Fatalf("state = %+v, want failed restart with the rollback message", st)
	}
	want := []string{"create goproxy-app-1", "start gen-1", "stop gen-1?t=5", "delete gen-1?force"}
	if got := f.got(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if !f.has("old") || f.has("gen-1") {
		t.Errorf("containers = %v, want only old", f.list())
	}
}

func TestOverlapRestartRefusesWhileClaimed(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(nil))
	dc.claims.tryClaim("app", autoUpdateClaimOwner)
	rec := doJSONReq(overlapMux(t, dc, newRollingOpManager(dc)), "POST", "/api/services/app/replicas/app/restart", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("restart = %d %s, want 409", rec.Code, rec.Body)
	}
	if got := f.got(); len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}

func TestOverlapRestartRefusesAnonymousVolumes(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(nil))
	f.setInspect(strings.Replace(overlapInspectHealthy, `},"Mounts":[]`, `},"Mounts":[{"Type":"volume","Name":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","Destination":"/data"}]`, 1))
	rom := newRollingOpManager(dc)
	rec := doJSONReq(overlapMux(t, dc, rom), "POST", "/api/services/app/replicas/app/restart", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restart = %d %s, want 202", rec.Code, rec.Body)
	}
	st := waitRollingTerminal(t, rom, "app")
	if st.Status != rollingOpStatusFailed || !strings.Contains(st.LastError, "proxy.overlap: app has anonymous volumes (/data)") {
		t.Fatalf("state = %+v, want the anonymous-volume refusal", st)
	}
	f.assertNoCreate(t)

	if err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}); err == nil || !strings.Contains(err.Error(), "anonymous volumes") {
		t.Fatalf("replaceService err = %v, want the anonymous-volume refusal", err)
	}
	f.assertNoCreate(t)
}

func TestOverlapRefusesWithoutHealthSignal(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(nil))
	f.setInspect(strings.Replace(overlapInspectHealthy, `,"Healthcheck":{"Test":["CMD","true"]}`, "", 1))
	err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"})
	if err == nil || !strings.Contains(err.Error(), "proxy.overlap requires a HEALTHCHECK or proxy.health so the new copy is proven ready") {
		t.Fatalf("replaceService err = %v, want the health-signal refusal", err)
	}
	if err := dc.setWeightLabel(context.Background(), "app", 3); err == nil || !strings.Contains(err.Error(), "requires a HEALTHCHECK") {
		t.Fatalf("setWeightLabel err = %v, want the health-signal refusal", err)
	}
	f.assertNoCreate(t)
}

func TestOverlapReplaceServiceIsHealthGated(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		f, dc := newOverlapFake(t, overlapLabels(nil))
		if err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}); err != nil {
			t.Fatalf("replaceService: %v", err)
		}
		want := []string{"pull", "create goproxy-app-1", "start gen-1", "refresh", "stop old?t=30", "delete old", "refresh"}
		if got := f.got(); !reflect.DeepEqual(got, want) {
			t.Errorf("calls = %v, want %v", got, want)
		}
		if got := f.createdBodies()[0].Labels[labelPrevImage]; got != "ghcr.io/org/app:v1" {
			t.Errorf("proxy.previous_image = %q, want v1 stamped by a replace", got)
		}
	})
	t.Run("unhealthy", func(t *testing.T) {
		f, dc := newOverlapFake(t, overlapLabels(nil))
		f.setNewStatus("Up 1 second (unhealthy)")
		err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"})
		var gate errReplicaGateFailed
		if !errors.As(err, &gate) {
			t.Fatalf("replaceService err = %v, want errReplicaGateFailed", err)
		}
		if !f.has("old") || f.has("gen-1") {
			t.Errorf("containers = %v, want only old", f.list())
		}
	})
}

// An operator rolling replace of an overlap service (exempt from the
// capacity guard) also removes a new copy that fails its gate.
func TestOverlapOperatorRollingReplaceRemovesFailedCopy(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(nil))
	f.setNewStatus("Up 1 second (unhealthy)")
	rom := newRollingOpManager(dc)
	if _, err := rom.start("app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if st := waitRollingTerminal(t, rom, "app"); st.Status != rollingOpStatusFailed {
		t.Fatalf("state = %+v, want failed", st)
	}
	if !f.has("old") || f.has("gen-1") {
		t.Errorf("containers = %v, want only old", f.list())
	}
}

// A caller giving up mid-gate must not leave the new copy behind.
func TestOverlapCancelledMidGateLeavesNoOrphan(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(nil))
	f.setNewStatus("Up 1 second (health: starting)")
	rollingReadyTimeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for !f.has("gen-1") {
			time.Sleep(2 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := dc.replaceServiceRolling(ctx, "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v1"}, nil, rollingOpts{removeOnGateFailure: true, skipPull: true, kind: rollingKindRestart})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !f.has("old") || f.has("gen-1") {
		t.Errorf("containers = %v, want only old", f.list())
	}
}

// A label flip on an overlap service is gated too: an unhealthy new copy
// leaves the old one serving with its old labels.
func TestOverlapLabelSetterKeepsOldOnGateFailure(t *testing.T) {
	f, dc := newOverlapFake(t, overlapLabels(nil))
	f.setNewStatus("Up 1 second (unhealthy)")
	err := dc.setAutoUpdateLabel(context.Background(), "app", true)
	var gate errReplicaGateFailed
	if !errors.As(err, &gate) {
		t.Fatalf("setAutoUpdateLabel err = %v, want errReplicaGateFailed", err)
	}
	if !f.has("old") || f.has("gen-1") {
		t.Errorf("containers = %v, want only old", f.list())
	}
	for _, c := range f.got() {
		if strings.HasPrefix(c, "stop old") || strings.HasPrefix(c, "delete old") {
			t.Errorf("old container touched: %v", f.got())
		}
	}
}

func TestOverlapExemptFromRollingReplaceCapacity(t *testing.T) {
	_, dc := newOverlapFake(t, overlapLabels(nil))
	if err := ensureRollingReplaceCapacity(context.Background(), dc, nil, "", "app"); err != nil {
		t.Errorf("overlap singleton refused: %v", err)
	}
	_, dc = newOverlapFake(t, overlapLabels(map[string]string{labelOverlap: ""}))
	if err := ensureRollingReplaceCapacity(context.Background(), dc, nil, "", "app"); err == nil || !strings.Contains(err.Error(), "proxy.overlap") {
		t.Errorf("plain singleton err = %v, want a refusal mentioning proxy.overlap", err)
	}
}

// An auto-update of an overlap service whose new copy fails the gate counts
// against the failure budget, like any other failed replace.
func TestAutoUpdateOverlapGateFailureCountsAsFailure(t *testing.T) {
	old := autoUpdateGap
	autoUpdateGap = 0
	t.Cleanup(func() { autoUpdateGap = old })
	f, dc := newOverlapFake(t, overlapLabels(map[string]string{labelAutoUpdate: "true"}))
	f.setNewStatus("Up 1 second (unhealthy)")
	ic := newImageChecker(dc)
	ic.statuses["ghcr.io/org/app:v1"] = &imageStatus{Image: "ghcr.io/org/app:v1", UpdateAvailable: true}
	au := newAutoUpdater(dc, ic, newTestOnboardedStore(t), "", "", newAutoUpdateBlockStore(), nil, nil)
	au.runOnce(context.Background())
	if au.failures["app"] != 1 {
		t.Errorf("failures = %d, want 1", au.failures["app"])
	}
	if !f.has("old") || f.has("gen-1") {
		t.Errorf("containers = %v, want only old", f.list())
	}
}

func TestReplicaRestartRouting(t *testing.T) {
	_, dc := newOverlapFake(t, overlapLabels(map[string]string{labelOverlap: ""}))
	mux := overlapMux(t, dc, newRollingOpManager(dc))
	if rec := doJSONReq(mux, "POST", "/api/services/app/replicas/app/bogus", ""); rec.Code != http.StatusNotFound {
		t.Errorf("bogus action = %d, want 404", rec.Code)
	}
	if rec := doJSONReq(mux, "POST", "/api/services/app/replicas/app/restart", ""); rec.Code != http.StatusOK {
		t.Errorf("local restart = %d %s, want 200", rec.Code, rec.Body)
	}

	peer := peerServicesMutateHandler("s3cret", "peer-b", dc, newTestOnboardedStore(t), newImageChecker(dc), nil, "", os.Getenv("PROXY_URL"), true, nil)
	do := func(path string) int {
		req := httptest.NewRequest("POST", path, nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		peer.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do("/peer/services/app/replicas/app/restart"); code != http.StatusOK {
		t.Errorf("peer restart = %d, want 200", code)
	}
	if code := do("/peer/services/app/replicas/app/bogus"); code != http.StatusNotFound {
		t.Errorf("peer bogus = %d, want 404", code)
	}

	// forwardServiceMutation: restart gets past the route switch (and fails
	// on the missing registry); a bogus action never does.
	fwd := func(action string) string {
		rec := httptest.NewRecorder()
		forwardServiceMutation(rec, httptest.NewRequest("POST", "/api/services/app/replicas/app/"+action, nil), "peer-b", nil, "app", []string{"app", "replicas/app/" + action}, "alice")
		return strings.TrimSpace(rec.Body.String())
	}
	if got := fwd("restart"); got != "unknown host" {
		t.Errorf("forward restart = %q, want it routed (unknown host)", got)
	}
	if got := fwd("bogus"); got != "not found" {
		t.Errorf("forward bogus = %q, want not found", got)
	}
}

// restart_replica blocks on an overlap restart's job until it is terminal,
// and a non-route 404 ("replica not found") is not mistaken for an older
// peer.
func TestMCPRestartReplicaOverlap(t *testing.T) {
	prev, prevPoll := internalToken, rollingReplaceToolPollInterval
	internalToken = "pmt_internal_test"
	rollingReplaceToolPollInterval = time.Millisecond
	t.Cleanup(func() { internalToken, rollingReplaceToolPollInterval = prev, prevPoll })

	t.Run("polls on overlap", func(t *testing.T) {
		var calls []string
		polls := 0
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.Method+" "+r.URL.RequestURI())
			if r.Method == http.MethodGet {
				polls++
				if polls < 2 {
					w.Write([]byte(`{"service":"app","status":"running","kind":"restart"}`))
					return
				}
				w.Write([]byte(`{"service":"app","status":"completed","kind":"restart"}`))
				return
			}
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"service":"app","status":"running","kind":"restart","mode":"overlap"}`))
		})
		s := NewServer("t", "v")
		registerMCPTools(s, &apiCaller{mux: h}, true, true)
		res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"restart_replica","arguments":{"service":"app","member":"m1","action":"restart","host":"peer-b"}}}`)
		r := res["result"].(map[string]any)
		if r["isError"] == true {
			t.Fatalf("tool errored: %v", r["content"])
		}
		if text := r["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, "completed") {
			t.Errorf("result = %q, want the completed state", text)
		}
		want := []string{"POST /api/services/app/replicas/m1/restart?host=peer-b", "GET /api/services/app/rolling-replace?host=peer-b", "GET /api/services/app/rolling-replace?host=peer-b"}
		if !reflect.DeepEqual(calls, want) {
			t.Errorf("calls = %v, want %v", calls, want)
		}
	})
	t.Run("replica not found is not a fallback", func(t *testing.T) {
		var calls []string
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.Method+" "+r.URL.RequestURI())
			http.Error(w, "replica not found", http.StatusNotFound)
		})
		s := NewServer("t", "v")
		registerMCPTools(s, &apiCaller{mux: h}, true, false)
		res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"restart_replica","arguments":{"service":"app","member":"m1","action":"restart"}}}`)
		if r := res["result"].(map[string]any); r["isError"] != true {
			t.Fatalf("expected an error, got %v", res)
		}
		if len(calls) != 1 {
			t.Errorf("calls = %v, want only the restart attempt", calls)
		}
	})
}
