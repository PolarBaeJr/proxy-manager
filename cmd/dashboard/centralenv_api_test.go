package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// newAPIHost is a single origin dashboard ("dashboard-a") owning "app" with
// one stamped live replica, plus its full dashboard API mux.
func newAPIHost(t *testing.T, base map[string]string) (*syncHost, http.Handler) {
	t.Helper()
	setInternalToken(t)
	h := newSyncHost(t, "dashboard-a", nil, "")
	if _, err := h.ce.store.Create("app", "dashboard-a", base, nil, "test", ""); err != nil {
		t.Fatal(err)
	}
	h.f.seedMember("m1", "goproxy-app-1", "dashboard-a", 1, envList(base), cenvTemplateHealth)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	mux := newDashboardMux(h.dc, nil, auth, newRateLimiter(), newImageChecker(h.dc), "", nil, newTestOnboardedStore(t), nil, nil, nil, nil, nil, nil, h.rm, nil, h.rom)
	return h, mux
}

func envList(m map[string]string) []string {
	out := []string{}
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func storeVersion(t *testing.T, h *syncHost) uint64 {
	t.Helper()
	rec, _, _ := h.ce.store.Get("app")
	return rec.Version
}

func TestCentralEnvAPIGetIsNamesOnly(t *testing.T) {
	withFastSync(t)
	h, mux := newAPIHost(t, map[string]string{"PLAIN": "value-one-xyz", "TOKEN": "ref:TOKEN"})
	h.ce.secrets = newTestSecrets(t, "app", "TOKEN=secret-two-xyz")

	rec := apiDo(t, mux, "GET", "/api/services/app/env", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "value-one-xyz") || strings.Contains(body, "secret-two-xyz") {
		t.Fatalf("GET leaked a value: %s", body)
	}
	var v centralEnvView
	json.Unmarshal(rec.Body.Bytes(), &v)
	if !v.Managed || v.Role != envSyncRoleOrigin || v.Version != 1 || strings.Join(v.Keys, ",") != "PLAIN,TOKEN" || v.Refs["TOKEN"] != "ref:TOKEN" {
		t.Fatalf("view = %s", body)
	}

	// Unmanaged service: managed=false, not an error.
	rec = apiDo(t, mux, "GET", "/api/services/other/env", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"managed":false`) {
		t.Fatalf("unmanaged GET = %d %s", rec.Code, rec.Body.String())
	}
}

func TestCentralEnvAPIFeatureOffIs404(t *testing.T) {
	setInternalToken(t)
	f := newCenvFakeDocker()
	dc := f.client(t)
	mux := newLocalTestMux(t, dc, nil)
	for _, p := range []string{"/api/services/app/env", "/api/services/app/env/sync"} {
		if rec := apiDo(t, mux, "POST", p, `{}`); rec.Code != http.StatusNotFound {
			t.Fatalf("%s with the feature off = %d", p, rec.Code)
		}
	}
}

func TestCentralEnvAPISetConflictAndSync(t *testing.T) {
	withFastSync(t)
	h, mux := newAPIHost(t, map[string]string{"A": "1"})

	rec := apiDo(t, mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"2","B":"x"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["version"] != float64(2) || out["origin"] != "dashboard-a" || out["request_id"] == "" {
		t.Fatalf("set response = %s", rec.Body.String())
	}
	h.waitJob(t, "app")
	for n, mv := range h.f.members() {
		if mv.version != "2" || !envHas(mv.env, "A=2") || !envHas(mv.env, "B=x") {
			t.Fatalf("%s = %+v", n, mv)
		}
	}

	rec = apiDo(t, mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"3"}}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"current_version":2`) {
		t.Fatalf("stale if_version = %d %s", rec.Code, rec.Body.String())
	}
	if rec := apiDo(t, mux, "POST", "/api/services/app/env", `{"if_version":2,"set":{"BAD=K":"v"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad key = %d %s", rec.Code, rec.Body.String())
	}
	if rec := apiDo(t, mux, "POST", "/api/services/app/env", `{"if_version":2,"set":{"R":"ref:MISSING"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing ref = %d %s", rec.Code, rec.Body.String())
	}
	if rec := apiDo(t, mux, "POST", "/api/services/other/env", `{"if_version":1,"set":{"A":"1"}}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unmanaged set = %d", rec.Code)
	}
	if v := storeVersion(t, h); v != 2 {
		t.Fatalf("store at v%d after rejected edits", v)
	}

	if rec := apiDo(t, mux, "POST", "/api/services/app/env/sync", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("sync = %d %s", rec.Code, rec.Body.String())
	}
	if j := h.waitJob(t, "app"); j.Status != envSyncStatusConverged {
		t.Fatalf("sync job = %+v", j)
	}
}

// TestCentralEnvAPIReplaceConversion: replace / rolling-replace carrying Env
// on a managed service is a central edit when the image doesn't change, and
// refused outright when it does.
func TestCentralEnvAPIReplaceConversion(t *testing.T) {
	for _, path := range []string{"replace", "rolling-replace"} {
		t.Run(path, func(t *testing.T) {
			withFastSync(t)
			h, mux := newAPIHost(t, map[string]string{"A": "1"})

			rec := apiDo(t, mux, "POST", "/api/services/app/"+path, `{"image":"ghcr.io/org/app:v2","env":{"A":"2"}}`)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changes via /env") {
				t.Fatalf("image change + env = %d %s", rec.Code, rec.Body.String())
			}
			if n := len(h.f.createsSnapshot()); n != 0 || storeVersion(t, h) != 1 {
				t.Fatalf("refused request created %d / moved the store", n)
			}

			rec = apiDo(t, mux, "POST", "/api/services/app/"+path, `{"image":"ghcr.io/org/app:v1","env":{"A":"2"}}`)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"env-set"`) || !strings.Contains(rec.Body.String(), `"version":2`) {
				t.Fatalf("same image + env = %d %s", rec.Code, rec.Body.String())
			}
			h.waitJob(t, "app")
			for n, mv := range h.f.members() {
				if mv.version != "2" || !envHas(mv.env, "A=2") {
					t.Fatalf("%s = %+v", n, mv)
				}
			}
		})
	}
}

// TestCentralEnvAPIStageRolloutPromoteRefusals: per-replica env edits on a
// managed service are 409s, and so is promoting a canary staged on a
// different central version.
func TestCentralEnvAPIStageRolloutPromoteRefusals(t *testing.T) {
	withFastSync(t)
	h, mux := newAPIHost(t, map[string]string{"A": "1"})

	if rec := apiDo(t, mux, "POST", "/api/services/app/stage", `{"image":"ghcr.io/org/app:v2","env":{"A":"x"}}`); rec.Code != http.StatusConflict {
		t.Fatalf("stage with env = %d %s", rec.Code, rec.Body.String())
	}
	if rec := apiDo(t, mux, "POST", "/api/services/app/rollout", `{"image":"ghcr.io/org/app:v2","env":{"A":"x"},"steps":[50,100]}`); rec.Code != http.StatusConflict {
		t.Fatalf("rollout with env = %d %s", rec.Code, rec.Body.String())
	}
	if n := len(h.f.createsSnapshot()); n != 0 {
		t.Fatalf("refused requests created %d containers", n)
	}

	h.f.seedCanary("ghcr.io/org/app:v2", map[string]string{labelEnvOrigin: "dashboard-a", labelEnvVersion: "1"})
	if _, err := h.ce.store.Apply("app", centralEnvChange{IfVersion: 1, Base: map[string]string{"A": "2"}}, "test"); err != nil {
		t.Fatal(err)
	}
	rec := apiDo(t, mux, "POST", "/api/services/app/promote", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("promote across a version change = %d %s", rec.Code, rec.Body.String())
	}
	if n := len(h.f.createsSnapshot()); n != 0 {
		t.Fatalf("refused promote created %d containers", n)
	}
}

// TestCentralEnvAPINonOriginUnreachableWritesNothing: a non-origin host whose
// origin refuses connections answers 503 and writes nothing, anywhere.
func TestCentralEnvAPINonOriginUnreachableWritesNothing(t *testing.T) {
	withFastSync(t)
	setInternalToken(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + ln.Addr().String()
	ln.Close()

	reg := newTestPeerRegistryFeatures("dashboard-b", deadURL, "dashboard-a", true, []string{centralEnvFeature})
	h := newSyncHost(t, "dashboard-b", reg, "s3cret")
	h.ce.cache.PutWithHealthcheck("app", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)
	h.f.seedMember("b1", "goproxy-app-1", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	mux := newDashboardMux(h.dc, nil, auth, newRateLimiter(), newImageChecker(h.dc), "", nil, newTestOnboardedStore(t), nil, nil, nil, nil, nil, reg, h.rm, nil, h.rom)

	rec := apiDo(t, mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"2"}}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("set with origin down = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := h.ce.store.Get("app"); ok {
		t.Fatal("non-origin wrote a store record")
	}
	if c, _ := h.ce.cache.Get("app"); c.Version != 1 {
		t.Fatalf("cache moved to v%d", c.Version)
	}
	if n := len(h.f.createsSnapshot()); n != 0 || h.m.busy("app") {
		t.Fatal("an unreachable-origin set started work")
	}
	// A converted replace fails the same way, before touching anything.
	rec = apiDo(t, mux, "POST", "/api/services/app/replace", `{"image":"ghcr.io/org/app:v1","env":{"A":"2"}}`)
	if rec.Code != http.StatusServiceUnavailable || len(h.f.createsSnapshot()) != 0 {
		t.Fatalf("converted replace with origin down = %d %s", rec.Code, rec.Body.String())
	}
}

// TestCentralEnvConcurrentSetsReadsAndRollingReplace: many edits based on
// the same version race — exactly one lands, the rest are conflicts —
// while readers hammer the view/job/status and a rolling-replace of the
// same service is already running (the job must wait for it, then roll).
// Meant for -race.
func TestCentralEnvConcurrentSetsReadsAndRollingReplace(t *testing.T) {
	withFastSync(t)
	h, mux := newAPIHost(t, map[string]string{"A": "1"})
	h.f.seedMember("m2", "goproxy-app-2", "dashboard-a", 1, []string{"A=1"}, cenvTemplateHealth)

	// Hold the rolling-replace mid-create until the env job has seen it and
	// parked itself pending — so the job's wait-for-idle path is what runs.
	created, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.f.onCreate = func(string) {
		once.Do(func() {
			close(created)
			<-release
		})
	}
	if _, err := h.rom.start("app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v1"}); err != nil {
		t.Fatal(err)
	}
	<-created

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				apiDo(t, mux, "GET", "/api/services/app/env", "")
				h.m.get("app")
				h.m.status(context.Background(), "app")
				h.m.request("app")
			}
		}()
	}

	const n = 16
	codes := make([]int, n)
	bodies := make([]string, n)
	var writers sync.WaitGroup
	for i := 0; i < n; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			rec := apiDo(t, mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"`+strconv.Itoa(i+2)+`"}}`)
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}(i)
	}
	writers.Wait()
	ok := 0
	for i := range codes {
		switch codes[i] {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			if !strings.Contains(bodies[i], `"current_version":2`) {
				t.Errorf("conflict body = %s", bodies[i])
			}
		default:
			t.Errorf("set %d = %d %s", i, codes[i], bodies[i])
		}
	}
	if ok != 1 || storeVersion(t, h) != 2 {
		t.Fatalf("%d sets succeeded, store at v%d — want exactly one bump", ok, storeVersion(t, h))
	}

	waitFor(t, "the env job to wait for the rolling replace", func() bool {
		j, ok := h.m.get("app")
		return ok && j.Status == envSyncStatusPending
	})
	close(release)
	j := h.waitJob(t, "app")
	close(stop)
	readers.Wait()
	j = h.waitJob(t, "app")
	if j.Status != envSyncStatusConverged || j.Target != 2 {
		t.Fatalf("job = %+v", j)
	}
	rec, _, _ := h.ce.store.Get("app")
	want := "A=" + rec.Base["A"]
	ms := h.f.members()
	if len(ms) != 2 {
		t.Fatalf("members = %+v, want 2", ms)
	}
	for name, mv := range ms {
		if mv.version != "2" || !envHas(mv.env, want) {
			t.Fatalf("%s = %+v, want v2 with %s", name, mv, want)
		}
	}
}
