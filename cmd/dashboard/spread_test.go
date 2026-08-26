package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// spreadDockerStub is replaceDockerStub's counterpart for the spread tests:
// same call recording, but the container inspect answer is configurable so a
// test can hand runServiceSpread a specific Env / Mounts fixture (the two
// inputs every refusal below turns on).
func spreadDockerStub(t *testing.T, calls *svcCallTracker, containers []dockerContainer, env []string, mounts []mountSpec) *dockerClient {
	t.Helper()
	if mounts == nil {
		mounts = []mountSpec{}
	}
	if env == nil {
		env = []string{}
	}
	inspect, err := json.Marshal(map[string]any{
		"Image":           "sha256:abc",
		"Config":          map[string]any{"Env": env},
		"HostConfig":      map[string]any{"Mounts": mounts},
		"NetworkSettings": map[string]any{"Networks": map[string]any{"edge": map[string]any{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			calls.record("list " + r.URL.Path)
			json.NewEncoder(w).Encode(containers)
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			calls.record("inspect " + r.URL.Path)
			w.Write(inspect)
		default:
			calls.record("other " + r.URL.Path)
			w.Write([]byte("{}"))
		}
	}))
}

// spreadAppContainers is the standard origin fixture: one running,
// label-managed "app" replica with a real host+port and no singleton label.
func spreadAppContainers() []dockerContainer {
	return []dockerContainer{{
		ID: "tpl1", Names: []string{"/app"}, State: "running", Image: "ghcr.io/org/app:v1",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080"},
	}}
}

// createCapture records the body of every /containers/create the peer's
// docker daemon stub sees, so a test can assert on the labels and HostConfig
// the spread handler actually built.
type createCapture struct {
	mu     sync.Mutex
	bodies []createBody
	names  []string
}

func (c *createCapture) add(name string, b createBody) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, name)
	c.bodies = append(c.bodies, b)
}

func (c *createCapture) first(t *testing.T) (string, createBody) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("peer never saw /containers/create")
	}
	return c.names[0], c.bodies[0]
}

func (c *createCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

// newSpreadTargetServer is a real /peer/spread handler over a target host
// that starts empty and, unlike the simpler stubs above, REMEMBERS what it
// created — scaleService re-lists after the seed container exists, so a
// replicas > 1 request only converges against a stub that reflects its own
// writes back.
func newSpreadTargetServer(t *testing.T) (*httptest.Server, *createCapture) {
	t.Helper()
	cap := &createCapture{}
	var mu sync.Mutex
	var live []dockerContainer
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/containers/create"):
			var b createBody
			json.NewDecoder(r.Body).Decode(&b)
			name := r.URL.Query().Get("name")
			cap.add(name, b)
			mu.Lock()
			live = append(live, dockerContainer{
				ID: "id-" + name, Names: []string{"/" + name}, State: "running", Image: b.Image, Labels: b.Labels,
			})
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"Id": "id-" + name})
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			mu.Lock()
			snapshot := append([]dockerContainer(nil), live...)
			mu.Unlock()
			json.NewEncoder(w).Encode(snapshot)
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			w.Write([]byte(`{"Image":"sha256:abc","Config":{"Env":[]},"HostConfig":{"Mounts":[]},"NetworkSettings":{"Networks":{"edge":{}}}}`))
		default:
			w.Write([]byte("{}"))
		}
	}))
	srv := httptest.NewServer(peerSpreadHandler("s3cret", "dashboard-b", dc, true))
	t.Cleanup(srv.Close)
	return srv, cap
}

// postSpread drives the local /api/services/app/spread endpoint end to end
// through a real dashboard mux whose registry resolves "dashboard-b" to
// target.
func postSpread(t *testing.T, dc *dockerClient, target *httptest.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	reg := newTestPeerRegistry(target.URL, true)
	mux := newLocalTestMux(t, dc, reg)
	req := httptest.NewRequest(http.MethodPost, "/api/services/app/spread", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestServicesSpreadCreatesSameLabelReplicaOnPeer is the core claim of the
// cross-host scale path: the container created on the peer joins the SAME
// logical service (same proxy.service / proxy.host / proxy.path), carries
// proxy.spread so the route mesh load-balances into it instead of holding it
// in reserve, publishes NO host port, and mounts nothing.
func TestServicesSpreadCreatesSameLabelReplicaOnPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	target, cap := newSpreadTargetServer(t)
	calls := &svcCallTracker{}
	dc := spreadDockerStub(t, calls, spreadAppContainers(), []string{"DATABASE_URL=postgres://u@db.example.org:5432/app"}, nil)

	rec := postSpread(t, dc, target, `{"target":"dashboard-b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	name, body := cap.first(t)
	if name != "goproxy-app-1" {
		t.Errorf("container name = %q, want goproxy-app-1", name)
	}
	if got := body.Labels[labelService]; got != "app" {
		t.Errorf("%s = %q, want app — the replica must join the origin's service identity, not get its own", labelService, got)
	}
	if got := body.Labels[labelHost]; got != "app.example" {
		t.Errorf("%s = %q, want app.example", labelHost, got)
	}
	if got := body.Labels[labelPort]; got != "8080" {
		t.Errorf("%s = %q, want 8080", labelPort, got)
	}
	if body.Labels[labelSpread] != "true" {
		t.Errorf("%s missing — without it the peer's proxy advertises the route as failover-only and the replica gets no traffic", labelSpread)
	}
	if len(body.HostConfig.PortBindings) != 0 {
		t.Errorf("PortBindings = %v, want none — a spread replica is reached over the edge network, not a published host port", body.HostConfig.PortBindings)
	}
	if len(body.HostConfig.Mounts) != 0 {
		t.Errorf("Mounts = %v, want none", body.HostConfig.Mounts)
	}
}

// TestServicesSpreadForwardsGroupLabel is the regression for the bug where a
// spread replica silently landed in its own default-to-service-name group
// instead of the origin's real proxy.group — which fragmented a spread
// service's Status-tab/statusbot entry across two differently-named groups
// on two hosts (see mergeServiceStatusGroups in servicestatus.go, which
// depends on both hosts agreeing on the group name to combine them at all).
func TestServicesSpreadForwardsGroupLabel(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	target, cap := newSpreadTargetServer(t)
	calls := &svcCallTracker{}
	containers := []dockerContainer{{
		ID: "tpl1", Names: []string{"/app"}, State: "running", Image: "ghcr.io/org/app:v1",
		Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080", labelGroup: "badminton"},
	}}
	dc := spreadDockerStub(t, calls, containers, nil, nil)

	rec := postSpread(t, dc, target, `{"target":"dashboard-b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	_, body := cap.first(t)
	if got := body.Labels[labelGroup]; got != "badminton" {
		t.Errorf("%s = %q, want badminton — the replica must join the origin's Status-view group, not default to its own service name", labelGroup, got)
	}
}

// Unlike proxy.name (free text, never validated on ingest), an invalid
// proxy.group is rejected by docker.go at container-listing time — the
// container is dropped before it ever becomes part of a Service, so
// findService can't find it and spread.go's own group sanitization (mirrored
// after Display's, for defense in depth) has no reachable path to exercise
// via this handler. No test for that branch; the guarantee comes from
// docker.go's ingest-time validation instead.

// TestServicesSpreadForwardsAutoUpdateAndWeight is the regression for the bug
// where a spread replica silently lost proxy.autoupdate and proxy.weight: the
// peer replica would never auto-update and would always route at the
// default weight, no matter what the origin's live container was set to.
func TestServicesSpreadForwardsAutoUpdateAndWeight(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	target, cap := newSpreadTargetServer(t)
	calls := &svcCallTracker{}
	containers := []dockerContainer{{
		ID: "tpl1", Names: []string{"/app"}, State: "running", Image: "ghcr.io/org/app:v1",
		Labels: map[string]string{
			labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080",
			labelAutoUpdate: "true", labelWeight: "5",
		},
	}}
	dc := spreadDockerStub(t, calls, containers, nil, nil)

	rec := postSpread(t, dc, target, `{"target":"dashboard-b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	_, body := cap.first(t)
	if got := body.Labels[labelAutoUpdate]; got != "true" {
		t.Errorf("%s = %q, want true — the replica must inherit the origin's auto-update opt-in", labelAutoUpdate, got)
	}
	if got := body.Labels[labelWeight]; got != "5" {
		t.Errorf("%s = %q, want 5 — the replica must inherit the origin's routing weight", labelWeight, got)
	}
}

// TestServicesSpreadOmitsDefaultAutoUpdateAndWeight proves the origin's
// defaults are not written out as labels: an origin with proxy.autoupdate
// absent and no proxy.weight label must produce a replica with neither
// label set, the same "absent means default" convention setWeightLabel uses.
func TestServicesSpreadOmitsDefaultAutoUpdateAndWeight(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	target, cap := newSpreadTargetServer(t)
	calls := &svcCallTracker{}
	dc := spreadDockerStub(t, calls, spreadAppContainers(), nil, nil)

	rec := postSpread(t, dc, target, `{"target":"dashboard-b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	_, body := cap.first(t)
	if _, ok := body.Labels[labelAutoUpdate]; ok {
		t.Errorf("%s present = %q, want absent — the origin never opted in", labelAutoUpdate, body.Labels[labelAutoUpdate])
	}
	if _, ok := body.Labels[labelWeight]; ok {
		t.Errorf("%s present = %q, want absent — the origin never set a weight", labelWeight, body.Labels[labelWeight])
	}
}

// TestServicesSpreadDropsOutOfRangeWeight proves an origin whose proxy.weight
// exceeds this codebase's normal range (e.g. hand-authored in a compose file,
// never run through the dashboard's own weight API) doesn't take the whole
// spread down with it: peerSpreadHandler validates Weight with the same
// validWeight bound the /weight endpoint uses, so shipping an out-of-range
// value verbatim would make the peer hard-refuse the entire request over a
// number that only affects routing share.
func TestServicesSpreadDropsOutOfRangeWeight(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	target, cap := newSpreadTargetServer(t)
	calls := &svcCallTracker{}
	containers := []dockerContainer{{
		ID: "tpl1", Names: []string{"/app"}, State: "running", Image: "ghcr.io/org/app:v1",
		Labels: map[string]string{
			labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080",
			labelWeight: "250",
		},
	}}
	dc := spreadDockerStub(t, calls, containers, nil, nil)

	rec := postSpread(t, dc, target, `{"target":"dashboard-b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var resp SpreadServiceResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(resp.Warnings, "\n")
	if !strings.Contains(joined, "weight") {
		t.Errorf("warnings = %q, want the dropped weight named", joined)
	}

	_, body := cap.first(t)
	if _, ok := body.Labels[labelWeight]; ok {
		t.Errorf("%s present = %q, want absent — an out-of-range weight must fall back to the default, not reach the peer", labelWeight, body.Labels[labelWeight])
	}
}

// TestServicesSpreadWritesNoRoutesEntry is the direct regression against the
// incident shape: duplicate.go appends a SECOND, competing routes.json entry
// for the same host+path. Spread must add none at all — the peer's own proxy
// advertises its backends into the origin's existing route.
func TestServicesSpreadWritesNoRoutesEntry(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	target, _ := newSpreadTargetServer(t)
	calls := &svcCallTracker{}
	dc := spreadDockerStub(t, calls, spreadAppContainers(), nil, nil)

	routesPath := filepath.Join(t.TempDir(), "routes.json")
	if err := writeRoutesFile(routesPath, routesFile{Routes: []routesEntry{
		{Name: "app", Host: "app.example", Backends: []string{"http://172.26.0.5:8080"}},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(routesPath)
	if err != nil {
		t.Fatal(err)
	}

	if rec := postSpread(t, dc, target, `{"target":"dashboard-b"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	after, err := os.ReadFile(routesPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("routes.json changed:\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestServicesSpreadPlacesRequestedReplicaCount proves the count is
// converged, not accumulated: the seed container is created from the shipped
// spec and scaleService fills in the rest under the same service label.
func TestServicesSpreadPlacesRequestedReplicaCount(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	target, cap := newSpreadTargetServer(t)
	calls := &svcCallTracker{}
	dc := spreadDockerStub(t, calls, spreadAppContainers(), nil, nil)

	rec := postSpread(t, dc, target, `{"target":"dashboard-b","replicas":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if cap.count() != 3 {
		t.Fatalf("peer created %d containers, want 3", cap.count())
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	for i, b := range cap.bodies {
		if b.Labels[labelService] != "app" || b.Labels[labelSpread] != "true" {
			t.Errorf("replica %d labels = %v, want the shared service identity plus %s", i, b.Labels, labelSpread)
		}
	}
}

// TestServicesSpreadRefusals covers every guard that must fire BEFORE the
// peer is contacted — a refusal that still created a container on the other
// host would be worse than no guard at all.
func TestServicesSpreadRefusals(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	singleton := spreadAppContainers()
	singleton[0].Labels[labelUnscalable] = "true"

	hostless := spreadAppContainers()
	delete(hostless[0].Labels, labelHost)

	cases := []struct {
		name       string
		containers []dockerContainer
		env        []string
		mounts     []mountSpec
		body       string
		wantErr    string
	}{
		{
			name:       "singleton",
			containers: singleton,
			body:       `{"target":"dashboard-b"}`,
			wantErr:    "singleton service",
		},
		{
			name:       "bind mount",
			containers: spreadAppContainers(),
			mounts:     []mountSpec{{Type: "bind", Source: "/srv/data", Target: "/data"}},
			body:       `{"target":"dashboard-b"}`,
			wantErr:    "bind mount",
		},
		{
			name:       "named volume",
			containers: spreadAppContainers(),
			mounts:     []mountSpec{{Type: "volume", Source: "appdata", Target: "/data"}},
			body:       `{"target":"dashboard-b"}`,
			wantErr:    "named volume",
		},
		{
			name:       "host-local db address",
			containers: spreadAppContainers(),
			env:        []string{"DB_HOST=postgres", "LOG_LEVEL=info"},
			body:       `{"target":"dashboard-b"}`,
			wantErr:    "DB_HOST",
		},
		{
			name:       "no proxy.host",
			containers: hostless,
			body:       `{"target":"dashboard-b"}`,
			wantErr:    labelHost,
		},
		{
			name:       "unknown target",
			containers: spreadAppContainers(),
			body:       `{"target":"nowhere"}`,
			wantErr:    "unknown target host",
		},
		{
			name:       "replica count out of range",
			containers: spreadAppContainers(),
			body:       `{"target":"dashboard-b","replicas":99}`,
			wantErr:    "replicas must be between",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, cap := newSpreadTargetServer(t)
			calls := &svcCallTracker{}
			dc := spreadDockerStub(t, calls, tc.containers, tc.env, tc.mounts)

			rec := postSpread(t, dc, target, tc.body)
			if rec.Code == http.StatusOK {
				t.Fatalf("status = 200, want a refusal (body %s)", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantErr) {
				t.Errorf("body = %q, want it to mention %q", rec.Body.String(), tc.wantErr)
			}
			if cap.count() != 0 {
				t.Error("peer created a container despite the refusal")
			}
		})
	}
}

// TestServicesSpreadEnvGuardIsOverridableAndLeaksNoValues proves the
// host-local-env refusal can be acknowledged, and that neither the refusal
// nor the resulting warning ever echoes an env VALUE — Env routinely holds
// credentials, and both of these land in HTTP responses and the audit log.
func TestServicesSpreadEnvGuardIsOverridableAndLeaksNoValues(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	env := []string{"DB_HOST=postgres", "BOT_TOKEN=hunter2-super-secret", "LOG_LEVEL=info"}

	target, cap := newSpreadTargetServer(t)
	calls := &svcCallTracker{}
	dc := spreadDockerStub(t, calls, spreadAppContainers(), env, nil)

	rec := postSpread(t, dc, target, `{"target":"dashboard-b","allow_unreachable_env":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s — allow_unreachable_env must lift the refusal", rec.Code, rec.Body.String())
	}
	if cap.count() != 1 {
		t.Fatalf("peer created %d containers, want 1", cap.count())
	}

	var resp SpreadServiceResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(resp.Warnings, "\n")
	if !strings.Contains(joined, "DB_HOST") {
		t.Errorf("warnings = %q, want the overridden host-local key named", joined)
	}
	if !strings.Contains(joined, "BOT_TOKEN") {
		t.Errorf("warnings = %q, want the credential-shaped key flagged — this is the duplicated-bot-token risk", joined)
	}
	if strings.Contains(joined, "hunter2") || strings.Contains(joined, "postgres") {
		t.Errorf("warnings leaked an env VALUE: %q", joined)
	}
}

// TestServicesSpreadForwardsToOwningPeer mirrors
// TestServicesDuplicateForwardsToOwningPeer: spreading a FOREIGN service must
// relay to the owning peer, whose own runServiceSpread reads ITS OWN docker
// state and reaches a third host — the local daemon is never touched.
func TestServicesSpreadForwardsToOwningPeer(t *testing.T) {
	t.Setenv("DASHBOARD_PEER_SECRET", "s3cret")

	finalSrv, cap := newSpreadTargetServer(t)

	calls := &svcCallTracker{}
	ownerDC := spreadDockerStub(t, calls, spreadAppContainers(), nil, nil)
	ownerOnb := newTestOnboardedStore(t)
	ownerReg := newPeerRegistry([]string{finalSrv.URL}, "s3cret", "dashboard-b", "dev", 0, nil)
	ownerReg.recordResult(finalSrv.URL, true, "dashboard-c", "dev", true)
	ownerSrv := httptest.NewServer(peerServicesMutateHandler("s3cret", "dashboard-b", ownerDC, ownerOnb, newImageChecker(ownerDC), ownerReg, filepath.Join(t.TempDir(), "routes.json"), noopProxyStub(t), true, nil))
	t.Cleanup(ownerSrv.Close)

	var localHit atomic.Bool
	localDC := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit.Store(true)
		json.NewEncoder(w).Encode([]dockerContainer{})
	}))
	reg := newTestPeerRegistry(ownerSrv.URL, true)
	mux := newLocalTestMux(t, localDC, reg)

	req := httptest.NewRequest(http.MethodPost, "/api/services/app/spread?host=dashboard-b",
		strings.NewReader(`{"target":"dashboard-c"}`))
	req.Header.Set("Authorization", "Bearer "+internalToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if localHit.Load() {
		t.Error("local docker stub was hit — a foreign service's spread must never touch the local daemon")
	}
	if cap.count() != 1 {
		t.Errorf("final target saw %d creates, want 1 — the second hop never happened", cap.count())
	}
}

// TestServicesSpreadPeerHandlerRefusesWithoutWrites proves /peer/spread is
// gated on -peer-writes exactly like every other write-mesh handler, rather
// than riding in on DASHBOARD_PEER_SECRET alone.
func TestServicesSpreadPeerHandlerRefusesWithoutWrites(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("docker was touched with writes disabled")
	}))
	srv := httptest.NewServer(peerSpreadHandler("s3cret", "dashboard-b", dc, false))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/", strings.NewReader(`{"service":"app"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestUnreachableEnvKeysScoping pins the heuristic's two failure modes: it
// must catch a bare Docker network alias on a connection-shaped key, and must
// NOT fire on ordinary non-address config, or the acknowledgement flag
// becomes a reflex checkbox that guards nothing.
func TestUnreachableEnvKeysScoping(t *testing.T) {
	env := []string{
		"DB_HOST=postgres",
		"REDIS_URL=redis://cache:6379",
		"DATABASE_URL=postgres://u:p@db:5432/app",
		"API_URL=https://api.example.org/v1",
		"MONITOR_ADDR=100.83.62.68:9000",
		"LOG_LEVEL=info",
		"ENV=production",
		"DEBUG=true",
		"WORKERS=4",
		"HEALTH_PATH=/healthz",
	}
	got := unreachableEnvKeys(env)
	want := map[string]bool{"DB_HOST": true, "REDIS_URL": true, "DATABASE_URL": true}
	for _, k := range got {
		if !want[k] {
			t.Errorf("flagged %q, which is routable or not an address at all", k)
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("missed %q — a replica on another host cannot resolve it", k)
	}
}

// TestPeerSpreadRefusesForeignContainers guards the invisible-success case: if
// the target already runs containers for the service that spread did not place
// — a leftover from "Duplicate to host…", say — scaleService would clone THEIR
// labels, so every replica would come up without proxy.spread and the pool
// would never activate. The service would run on both hosts and take no
// cross-host traffic, looking healthy at every layer. Must refuse instead.
func TestPeerSpreadRefusesForeignContainers(t *testing.T) {
	cap := &createCapture{}
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/containers/create"):
			var b createBody
			json.NewDecoder(r.Body).Decode(&b)
			cap.add(r.URL.Query().Get("name"), b)
			json.NewEncoder(w).Encode(map[string]string{"Id": "id-x"})
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			// Named "app", not "goproxy-app-N", and carrying no proxy.spread.
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "dup1", Names: []string{"/app"}, State: "running", Image: "app:1",
				Labels: map[string]string{labelService: "app", labelHost: "app.example.org", labelPort: "8080"},
			}})
		default:
			w.Write([]byte("{}"))
		}
	}))
	srv := httptest.NewServer(peerSpreadHandler("s3cret", "dashboard-b", dc, true))
	defer srv.Close()

	body := `{"service":"app","image":"app:1","host":"app.example.org","port":8080,"replicas":2}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 — spread must not adopt containers it did not place", resp.StatusCode)
	}
	if cap.count() != 0 {
		t.Errorf("created %d container(s) on a refusal", cap.count())
	}
}
