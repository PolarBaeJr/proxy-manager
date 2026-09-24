package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// seedAdoptee adds one compose-created, unstamped live "app" replica — what
// an adopt starts from. extra overrides labels ("" deletes one).
func (f *cenvFakeDocker) seedAdoptee(id, name string, env []string, health *healthcheckSpec, extra map[string]string) {
	labels := map[string]string{
		labelEnable: "true", labelService: "app", labelHost: "app.example", labelPort: "8080",
		composeProjectLabel: "stack", composeServiceLabel: "app",
		composeConfigLabel: "/srv/stack/docker-compose.yml", composeWorkdirLabel: "/srv/stack",
	}
	for k, v := range extra {
		if v == "" {
			delete(labels, k)
		} else {
			labels[k] = v
		}
	}
	f.seed(dockerContainer{ID: id, Names: []string{"/" + name}, Image: "ghcr.io/org/app:v1", State: "running", Status: "Up 1 hour", Labels: labels},
		cenvInspect{env: env, health: health, edge: []string{name}, networks: map[string][]string{}, restart: "unless-stopped"})
}

func (f *cenvFakeDocker) mutateInspect(id string, fn func(in *cenvInspect)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.inspect[id]
	fn(&in)
	f.inspect[id] = in
}

// appMembers is every live "app" container with its labels and env.
type adoptMember struct {
	labels map[string]string
	env    []string
}

func (f *cenvFakeDocker) appMembers() map[string]adoptMember {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]adoptMember{}
	for id, c := range f.items {
		if c.Labels[labelService] == "app" && c.Labels[labelCanary] != "true" {
			out[c.name()] = adoptMember{labels: c.Labels, env: f.inspect[id].env}
		}
	}
	return out
}

func hasComposeLabel(l map[string]string) bool {
	for k := range l {
		if strings.HasPrefix(k, composeLabelPrefix) {
			return true
		}
	}
	return false
}

// newAdoptHost is a lone dashboard ("dashboard-a", no peers) running one
// compose-created "app" replica with X=x-val on top of its image's PATH.
func newAdoptHost(t *testing.T) (*syncHost, http.Handler) {
	t.Helper()
	setInternalToken(t)
	h := newSyncHost(t, "dashboard-a", nil, "")
	h.f.imageConfig = map[string]any{"Env": []string{"PATH=/usr/bin"}}
	h.f.seedAdoptee("c1", "stack-app-1", []string{"PATH=/usr/bin", "X=x-val"}, cenvTemplateHealth, nil)
	auth, _ := newConfirmedStore(t, "alice", "correct horse")
	mux := newDashboardMux(h.dc, nil, auth, newRateLimiter(), newImageChecker(h.dc), "", nil, newTestOnboardedStore(t), nil, nil, nil, nil, nil, nil, h.rm, nil, h.rom)
	return h, mux
}

func adoptDryRun(t *testing.T, mux http.Handler, svc, body string) centralEnvAdoptReport {
	t.Helper()
	rec := apiDo(t, mux, "POST", "/api/services/"+svc+"/env/adopt", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run = %d %s", rec.Code, rec.Body.String())
	}
	var rep centralEnvAdoptReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestAdoptPreflightBlockers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		svc   string
		setup func(t *testing.T, h *syncHost)
		want  string
	}{
		{name: "infra", svc: "proxy", want: "fixed infrastructure container"},
		{name: "self", setup: func(t *testing.T, h *syncHost) {
			withSelfHostname(t, func() (string, error) { return "c1", nil })
		}, want: "this dashboard's own service"},
		{name: "onboarded", setup: func(t *testing.T, h *syncHost) {
			h.m.onb = newTestOnboardedStore(t)
			if err := h.m.onb.Put(OnboardedService{Name: "app", Host: "app.example"}); err != nil {
				t.Fatal(err)
			}
		}, want: "onboarded record"},
		{name: "no members", setup: func(t *testing.T, h *syncHost) {
			h.f.mu.Lock()
			delete(h.f.items, "c1")
			h.f.mu.Unlock()
		}, want: "no live replica"},
		{name: "unscalable", setup: func(t *testing.T, h *syncHost) {
			h.f.seedAdoptee("c1", "stack-app-1", []string{"X=x-val"}, cenvTemplateHealth, map[string]string{labelUnscalable: "true"})
		}, want: "proxy.unscalable"},
		{name: "no host", setup: func(t *testing.T, h *syncHost) {
			h.f.seedAdoptee("c1", "stack-app-1", []string{"X=x-val"}, cenvTemplateHealth, map[string]string{labelHost: ""})
		}, want: "no proxy.host"},
		{name: "canary", setup: func(t *testing.T, h *syncHost) {
			h.f.seedCanary("ghcr.io/org/app:v2", nil)
		}, want: "staged canary"},
		{name: "claim held", setup: func(t *testing.T, h *syncHost) {
			h.dc.claims.tryClaim("app", autoUpdateClaimOwner)
		}, want: "claimed by " + autoUpdateClaimOwner},
		{name: "rolling replace active", setup: func(t *testing.T, h *syncHost) {
			h.rom.mu.Lock()
			h.rom.ops["app"] = &rollingOpState{Status: rollingOpStatusRunning}
			h.rom.mu.Unlock()
		}, want: "a rolling replace is running"},
		{name: "canary rollout active", setup: func(t *testing.T, h *syncHost) {
			h.rm.mu.Lock()
			h.rm.rollouts["app"] = &rolloutState{Status: rolloutStatusAwaitingAdvance}
			h.rm.mu.Unlock()
		}, want: "a canary rollout is active"},
		{name: "already managed", setup: func(t *testing.T, h *syncHost) {
			h.ce.store.Create("app", "dashboard-a", map[string]string{"X": "x-val"}, nil, "test", "")
		}, want: "already centrally managed"},
		{name: "stamped replica", setup: func(t *testing.T, h *syncHost) {
			h.f.seedMember("m9", "goproxy-app-9", "dashboard-z", 3, []string{"X=x-val"}, cenvTemplateHealth)
		}, want: "already centrally managed"},
		{name: "recreate blocker", setup: func(t *testing.T, h *syncHost) {
			h.f.mutateInspect("c1", func(in *cenvInspect) {
				in.hostConfig = map[string]any{"PortBindings": map[string]any{"80/tcp": []any{map[string]string{"HostPort": "8080"}}}}
			})
		}, want: "cannot recreate stack-app-1: PortBindings"},
		{name: "ref-like value", setup: func(t *testing.T, h *syncHost) {
			h.f.mutateInspect("c1", func(in *cenvInspect) { in.env = append(in.env, "TOK=ref:TOK") })
		}, want: "read as secret references"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFastSync(t)
			h, mux := newAdoptHost(t)
			if tc.setup != nil {
				tc.setup(t, h)
			}
			svc := tc.svc
			if svc == "" {
				svc = "app"
			}
			rep := adoptDryRun(t, mux, svc, `{"ack_compose":true}`)
			if rep.Adoptable || !anyContains(rep.Blockers, tc.want) {
				t.Fatalf("blockers = %q, adoptable=%v; want one containing %q", rep.Blockers, rep.Adoptable, tc.want)
			}
		})
	}
}

// TestAdoptPreflightStrictFields: every field a recreate would silently drop
// blocks the adopt; the same values inherited from the image (or at the
// daemon default) don't.
func TestAdoptPreflightStrictFields(t *testing.T) {
	for _, tc := range []struct {
		field      string
		config     map[string]any
		hostConfig map[string]any
	}{
		{field: "Config.User", config: map[string]any{"User": "1000"}},
		{field: "Config.WorkingDir", config: map[string]any{"WorkingDir": "/work"}},
		{field: "Config.StopSignal", config: map[string]any{"StopSignal": "SIGINT"}},
		{field: "Config.StopTimeout", config: map[string]any{"StopTimeout": 30}},
		{field: "HostConfig.LogConfig", hostConfig: map[string]any{"LogConfig": map[string]any{"Type": "syslog"}}},
		{field: "HostConfig.LogConfig", hostConfig: map[string]any{"LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "10m"}}}},
		{field: "HostConfig.Tmpfs", hostConfig: map[string]any{"Tmpfs": map[string]string{"/tmp": ""}}},
		{field: "HostConfig.Ulimits", hostConfig: map[string]any{"Ulimits": []any{map[string]any{"Name": "nofile", "Soft": 1024, "Hard": 2048}}}},
		{field: "HostConfig.Init", hostConfig: map[string]any{"Init": true}},
		{field: "HostConfig.SecurityOpt", hostConfig: map[string]any{"SecurityOpt": []string{"no-new-privileges"}}},
		{field: "HostConfig.ReadonlyRootfs", hostConfig: map[string]any{"ReadonlyRootfs": true}},
		{field: "HostConfig.Sysctls", hostConfig: map[string]any{"Sysctls": map[string]string{"net.core.somaxconn": "1024"}}},
		{field: "HostConfig.GroupAdd", hostConfig: map[string]any{"GroupAdd": []string{"docker"}}},
		{field: "HostConfig.ShmSize", hostConfig: map[string]any{"ShmSize": 128 << 20}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			withFastSync(t)
			h, mux := newAdoptHost(t)
			h.f.mutateInspect("c1", func(in *cenvInspect) { in.config, in.hostConfig = tc.config, tc.hostConfig })
			rep := adoptDryRun(t, mux, "app", `{"ack_compose":true}`)
			if rep.Adoptable || !anyContains(rep.Blockers, "stack-app-1: "+tc.field) {
				t.Fatalf("blockers = %q; want %s", rep.Blockers, tc.field)
			}
		})
	}

	t.Run("inherited or default", func(t *testing.T) {
		withFastSync(t)
		h, mux := newAdoptHost(t)
		h.f.imageConfig = map[string]any{"Env": []string{"PATH=/usr/bin"}, "User": "app", "WorkingDir": "/srv", "StopSignal": "SIGTERM"}
		h.f.mutateInspect("c1", func(in *cenvInspect) {
			in.config = map[string]any{"User": "app", "WorkingDir": "/srv", "StopSignal": "SIGTERM"}
			in.hostConfig = map[string]any{"LogConfig": map[string]any{"Type": "json-file", "Config": map[string]string{}}, "ShmSize": 64 << 20, "Init": false}
		})
		rep := adoptDryRun(t, mux, "app", `{"ack_compose":true}`)
		if !rep.Adoptable || len(rep.Blockers) != 0 {
			t.Fatalf("inherited values blocked the adopt: %q", rep.Blockers)
		}
	})
}

// TestAdoptPreflightAcksAndCompose: compose ownership, restart policy, a
// missing healthcheck and a newer pulled image each need their own ack, and
// the compose warning says exactly what not to do afterwards.
func TestAdoptPreflightAcksAndCompose(t *testing.T) {
	withFastSync(t)
	h, mux := newAdoptHost(t)
	rep := adoptDryRun(t, mux, "app", `{}`)
	if rep.Adoptable || !reflect.DeepEqual(rep.RequiredAcks, []string{adoptAckCompose}) || !reflect.DeepEqual(rep.MissingAcks, []string{adoptAckCompose}) {
		t.Fatalf("acks = %v missing %v adoptable=%v", rep.RequiredAcks, rep.MissingAcks, rep.Adoptable)
	}
	if !reflect.DeepEqual(rep.Env.OriginKeys, []string{"X"}) {
		t.Fatalf("origin_keys = %v, want the image's PATH subtracted", rep.Env.OriginKeys)
	}
	c := rep.Compose["dashboard-a"]
	if c == nil || c.Project != "stack" || c.ConfigFiles != "/srv/stack/docker-compose.yml" || c.WorkingDir != "/srv/stack" || !reflect.DeepEqual(c.Services, []string{"app"}) {
		t.Fatalf("compose = %+v", c)
	}
	for _, want := range []string{"compose_owned on dashboard-a", "Adopt strips the compose labels", "NEVER run `docker compose up`, `down` or `pull`",
		"Retire only the service block(s) for app", "do NOT delete the project directory"} {
		if !anyContains(rep.Warnings, want) {
			t.Errorf("warnings %q missing %q", rep.Warnings, want)
		}
	}
	if len(rep.RetireSteps) == 0 || !reflect.DeepEqual(rep.Members["dashboard-a"], []string{"stack-app-1"}) {
		t.Fatalf("retire_steps=%v members=%v", rep.RetireSteps, rep.Members)
	}
	if rep := adoptDryRun(t, mux, "app", `{"ack_compose":true}`); !rep.Adoptable {
		t.Fatalf("acked dry run not adoptable: %+v", rep)
	}

	h.f.seedAdoptee("c1", "stack-app-1", []string{"X=x-val"}, nil, map[string]string{composeDependsLabel: "db:service_started:false"})
	h.f.mutateInspect("c1", func(in *cenvInspect) { in.restart = "always" })
	h.f.mu.Lock()
	ct := h.f.items["c1"]
	ct.ImageID = "sha256:old"
	h.f.items["c1"] = ct
	h.f.imageID = "sha256:new"
	h.f.mu.Unlock()
	rep = adoptDryRun(t, mux, "app", `{"ack_compose":true}`)
	want := []string{adoptAckCompose, adoptAckImageUpdate, adoptAckNoHealth, adoptAckRestart}
	sort.Strings(want)
	if !reflect.DeepEqual(rep.RequiredAcks, want) || rep.Adoptable {
		t.Fatalf("acks = %v, want %v", rep.RequiredAcks, want)
	}
	for _, w := range []string{"depends_on ordering (db:service_started:false) is lost", "restart policy stack-app-1=always is normalized", "points to a newer pulled image", "no Docker healthcheck and no proxy.health"} {
		if !anyContains(rep.Warnings, w) {
			t.Errorf("warnings %q missing %q", rep.Warnings, w)
		}
	}
	rep = adoptDryRun(t, mux, "app", `{"ack_compose":true,"ack_no_health":true,"ack_restart_policy":true,"ack_image_update":true}`)
	if !rep.Adoptable {
		t.Fatalf("fully acked = %+v", rep)
	}

	// proxy.health counts as a health gate: no ack needed.
	h.f.seedAdoptee("c1", "stack-app-1", []string{"X=x-val"}, nil, map[string]string{labelHealth: "/healthz"})
	h.f.mu.Lock()
	h.f.imageID = ""
	h.f.mu.Unlock()
	if rep := adoptDryRun(t, mux, "app", `{}`); containsString(rep.RequiredAcks, adoptAckNoHealth) {
		t.Fatalf("proxy.health still required ack_no_health: %v", rep.RequiredAcks)
	}
}

func TestAdoptExecuteRefusals(t *testing.T) {
	withFastSync(t)
	h, mux := newAdoptHost(t)
	rep := adoptDryRun(t, mux, "app", `{}`)
	for _, tc := range []struct{ name, body, want string }{
		{"no fingerprint", `{"dry_run":false,"ack_compose":true}`, "fingerprint"},
		{"missing ack", `{"dry_run":false,"fingerprint":"` + rep.Fingerprint + `"}`, adoptAckCompose},
		{"stale fingerprint", `{"dry_run":false,"ack_compose":true,"fingerprint":"0000"}`, "changed"},
	} {
		rec := apiDo(t, mux, "POST", "/api/services/app/env/adopt", tc.body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s = %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if h.ce.store.Has("app") || len(h.f.createsSnapshot()) != 0 || h.dc.claims.holder("app") != "" {
		t.Fatal("a refused execute changed something")
	}
}

// TestAdoptExecuteSingleHost: the happy path on a lone origin — the compose
// replica is replaced by a stamped, compose-free one carrying exactly the
// record's env, the record holds the origin env minus image ENV, the claim
// is held (autoupdate would defer) for the whole roll, and replaying the
// request id changes nothing.
func TestAdoptExecuteSingleHost(t *testing.T) {
	withFastSync(t)
	readAudit := withAuditFile(t)
	h, mux := newAdoptHost(t)
	var autoGotIn atomic.Int32
	h.f.onCreate = func(string) {
		if h.dc.claims.tryClaim("app", autoUpdateClaimOwner) {
			autoGotIn.Add(1)
			h.dc.claims.release("app", autoUpdateClaimOwner)
		}
	}
	rep := adoptDryRun(t, mux, "app", `{}`)
	body := `{"dry_run":false,"ack_compose":true,"request_id":"req-1","fingerprint":"` + rep.Fingerprint + `"}`
	rec := apiDo(t, mux, "POST", "/api/services/app/env/adopt", body)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"base_keys":["X"]`) {
		t.Fatalf("execute = %d %s", rec.Code, rec.Body.String())
	}
	j := h.waitJob(t, "app")
	if j.Status != envSyncStatusConverged {
		t.Fatalf("job = %+v", j)
	}
	if autoGotIn.Load() != 0 {
		t.Fatal("the auto-updater could claim the service mid-adopt")
	}
	if h.dc.claims.holder("app") != "" {
		t.Fatalf("claim still held by %s", h.dc.claims.holder("app"))
	}
	r, ok, _ := h.ce.store.Get("app")
	if !ok || r.Adopting || r.Version != 1 || !reflect.DeepEqual(r.Base, map[string]string{"X": "x-val"}) || !strings.HasPrefix(r.AdoptedFrom, "compose:stack") {
		t.Fatalf("record = %+v", r)
	}
	members := h.f.appMembers()
	if _, old := members["stack-app-1"]; old || len(members) != 1 {
		t.Fatalf("members = %v", members)
	}
	for name, mb := range members {
		if mb.labels[labelEnvOrigin] != "dashboard-a" || mb.labels[labelEnvVersion] != "1" || hasComposeLabel(mb.labels) || !reflect.DeepEqual(mb.env, []string{"X=x-val"}) {
			t.Fatalf("%s = %+v", name, mb)
		}
	}

	creates := len(h.f.createsSnapshot())
	rec = apiDo(t, mux, "POST", "/api/services/app/env/adopt", body)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"replayed":true`) {
		t.Fatalf("replay = %d %s", rec.Code, rec.Body.String())
	}
	h.waitJob(t, "app")
	if len(h.f.createsSnapshot()) != creates {
		t.Fatal("a replayed adopt rolled again")
	}
	actions := map[string]bool{}
	for _, e := range readAudit() {
		actions[e["action"].(string)] = true
	}
	for _, a := range []string{"service.env_adopt", "service.env_adopt_done"} {
		if !actions[a] {
			t.Errorf("audit missing %s: %v", a, actions)
		}
	}
}

// TestAdoptOriginGateFailureUnAdopts: nothing was swapped, so the record
// simply goes and the compose replica stays exactly as it was.
func TestAdoptOriginGateFailureUnAdopts(t *testing.T) {
	withFastSync(t)
	readAudit := withAuditFile(t)
	h, mux := newAdoptHost(t)
	h.f.setUnhealthy(func(createBody) bool { return true })
	rep := adoptDryRun(t, mux, "app", `{}`)
	rec := apiDo(t, mux, "POST", "/api/services/app/env/adopt", `{"dry_run":false,"ack_compose":true,"fingerprint":"`+rep.Fingerprint+`"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("execute = %d %s", rec.Code, rec.Body.String())
	}
	if j := h.waitJob(t, "app"); j.Status != envSyncStatusAdoptUndone {
		t.Fatalf("job = %+v", j)
	}
	if h.ce.store.Has("app") {
		t.Fatal("record left behind")
	}
	mb, ok := h.f.appMembers()["stack-app-1"]
	if !ok || len(h.f.appMembers()) != 1 || mb.labels[labelEnvOrigin] != "" || !hasComposeLabel(mb.labels) {
		t.Fatalf("members = %+v", h.f.appMembers())
	}
	var view centralEnvView
	json.Unmarshal(apiDo(t, mux, "GET", "/api/services/app/env", "").Body.Bytes(), &view)
	if view.Managed || view.LastFailure == nil || view.LastFailure.Status != envSyncStatusAdoptUndone || view.LastFailure.LastError == "" {
		t.Fatalf("view = %+v last_failure = %+v", view, view.LastFailure)
	}
	found := false
	for _, e := range readAudit() {
		found = found || e["action"] == "service.env_adopt_failed"
	}
	if !found {
		t.Fatal("no service.env_adopt_failed audit")
	}
}

// TestAdoptOriginPartialFailureUnstampsFirst: one replica was already
// swapped (stamped) when the second failed its gate — deleting the record
// under it would leave it failing closed, so it's re-rolled unstamped first.
func TestAdoptOriginPartialFailureUnstampsFirst(t *testing.T) {
	withFastSync(t)
	h, mux := newAdoptHost(t)
	h.f.seedAdoptee("c2", "stack-app-2", []string{"PATH=/usr/bin", "X=x-val"}, cenvTemplateHealth, nil)
	var stamped atomic.Int32
	h.f.setUnhealthy(func(b createBody) bool {
		return b.Labels[labelEnvOrigin] != "" && stamped.Add(1) == 2
	})
	rep := adoptDryRun(t, mux, "app", `{}`)
	apiDo(t, mux, "POST", "/api/services/app/env/adopt", `{"dry_run":false,"ack_compose":true,"fingerprint":"`+rep.Fingerprint+`"}`)
	if j := h.waitJob(t, "app"); j.Status != envSyncStatusAdoptUndone {
		t.Fatalf("job = %+v", j)
	}
	if h.ce.store.Has("app") {
		t.Fatal("record left behind")
	}
	members := h.f.appMembers()
	if len(members) != 2 {
		t.Fatalf("members = %v", members)
	}
	for n, mb := range members {
		if mb.labels[labelEnvOrigin] != "" || mb.labels[labelEnvVersion] != "" {
			t.Fatalf("%s still stamped after un-adopt: %v", n, mb.labels)
		}
	}
}

// adoptMesh: A runs X (plus SHARED and DIFF), B runs SHARED, a different
// DIFF, Y and Z with no healthcheck; both compose-created.
func adoptMesh(t *testing.T) (a, b *meshNode) {
	t.Helper()
	a, b, _ = newMesh(t, true)
	a.f.seedAdoptee("a1", "stack-app-1", []string{"X=x-val", "SHARED=shared-val", "DIFF=from-a"}, cenvTemplateHealth, nil)
	b.f.seedAdoptee("b1", "stack-app-1", []string{"SHARED=shared-val", "DIFF=from-b", "Y=y-val", "Z=z-val"}, nil, nil)
	return a, b
}

func adoptMeshExecute(t *testing.T, a *meshNode, extra string) {
	t.Helper()
	rep := adoptDryRun(t, a.mux, "app", `{}`)
	rec := apiDo(t, a.mux, "POST", "/api/services/app/env/adopt", `{"dry_run":false,"ack_compose":true,"fingerprint":"`+rep.Fingerprint+`"`+extra+`}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("execute = %d %s", rec.Code, rec.Body.String())
	}
}

// TestMeshAdoptImportOverrideAndAcceptDropped: Y stays B's alone (override),
// Z is dropped as accepted.
func TestMeshAdoptImportOverrideAndAcceptDropped(t *testing.T) {
	withFastSync(t)
	a, b := adoptMesh(t)
	adoptMeshExecute(t, a, `,"import":{"dashboard-b":{"keys":["Y","DIFF"],"as":"override"}},"accept_dropped":["Z"]`)
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	r, _, _ := a.ce.store.Get("app")
	if !reflect.DeepEqual(sortedKeys(r.Base), []string{"DIFF", "SHARED", "X"}) || !reflect.DeepEqual(r.Overrides["dashboard-b"], map[string]string{"DIFF": "from-b", "Y": "y-val"}) {
		t.Fatalf("record base=%v overrides=%v", sortedKeys(r.Base), r.Overrides)
	}
	for n, want := range map[*meshNode][]string{
		a: {"DIFF=from-a", "SHARED=shared-val", "X=x-val"},
		b: {"DIFF=from-b", "SHARED=shared-val", "X=x-val", "Y=y-val"},
	} {
		for name, mb := range n.f.appMembers() {
			env := append([]string(nil), mb.env...)
			sort.Strings(env)
			if !reflect.DeepEqual(env, want) || mb.labels[labelEnvOrigin] != "dashboard-a" {
				t.Fatalf("%s %s env=%v labels=%v", n.identity, name, env, mb.labels)
			}
		}
	}
}

func TestMeshAdoptPeerSupportBlocks(t *testing.T) {
	withFastSync(t)
	t.Run("no central env", func(t *testing.T) {
		a, b, _ := newMesh(t, false)
		a.f.seedAdoptee("a1", "stack-app-1", []string{"X=1"}, cenvTemplateHealth, nil)
		b.f.seedAdoptee("b1", "stack-app-1", []string{"X=1"}, cenvTemplateHealth, nil)
		if rep := adoptDryRun(t, a.mux, "app", `{"ack_compose":true}`); !anyContains(rep.Blockers, "does not advertise "+centralEnvFeature) {
			t.Fatalf("blockers = %q", rep.Blockers)
		}
	})
	t.Run("no adopt support", func(t *testing.T) {
		a, b := adoptMesh(t)
		a.m.registry.recordResult(b.url, true, "dashboard-b", "dev", true, []string{centralEnvFeature})
		if rep := adoptDryRun(t, a.mux, "app", `{"ack_compose":true}`); !anyContains(rep.Blockers, "cannot take part in an adopt") {
			t.Fatalf("blockers = %q", rep.Blockers)
		}
	})
	t.Run("no handshake", func(t *testing.T) {
		a, b := adoptMesh(t)
		reg := newPeerRegistry([]string{b.url, "http://127.0.0.1:1"}, "s3cret", "dashboard-a", "dev", 0, nil)
		reg.recordResult(b.url, true, "dashboard-b", "dev", true, []string{centralEnvFeature, centralEnvAdoptFeature})
		a.m.registry = reg
		if rep := adoptDryRun(t, a.mux, "app", `{"ack_compose":true}`); !anyContains(rep.Blockers, "has not completed a handshake") {
			t.Fatalf("blockers = %q", rep.Blockers)
		}
	})
	t.Run("peer already managed", func(t *testing.T) {
		a, b := adoptMesh(t)
		b.ce.cache.PutWithHealthcheck("app", "dashboard-z", 1, []string{"Q=1"}, nil)
		if rep := adoptDryRun(t, a.mux, "app", `{"ack_compose":true}`); !anyContains(rep.Blockers, "dashboard-b already has a central env") {
			t.Fatalf("blockers = %q", rep.Blockers)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		a, b := adoptMesh(t)
		b.stop()
		defer b.start(t)
		if rep := adoptDryRun(t, a.mux, "app", `{"ack_compose":true}`); !anyContains(rep.Blockers, "dashboard-b is unreachable") {
			t.Fatalf("blockers = %q", rep.Blockers)
		}
	})
}

// TestMeshAdoptRefusesUnresolvedPeerKeys: every peer-only key must be
// imported or accepted as dropped before anything moves.
func TestMeshAdoptRefusesUnresolvedPeerKeys(t *testing.T) {
	withFastSync(t)
	a, b := adoptMesh(t)
	rep := adoptDryRun(t, a.mux, "app", `{}`)
	for _, extra := range []string{"", `,"accept_dropped":["Y"]`, `,"import":{"dashboard-b":{"keys":["Z"],"as":"base"}}`} {
		rec := apiDo(t, a.mux, "POST", "/api/services/app/env/adopt", `{"dry_run":false,"ack_compose":true,"fingerprint":"`+rep.Fingerprint+`"`+extra+`}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"unresolved":{"dashboard-b":[`) {
			t.Fatalf("execute%s = %d %s", extra, rec.Code, rec.Body.String())
		}
	}
	if a.ce.store.Has("app") || len(a.f.createsSnapshot()) != 0 || len(b.f.createsSnapshot()) != 0 {
		t.Fatal("a refused execute changed something")
	}
}

// TestMeshReleaseUnstampsEverywhere: release rolls A then B without the
// stamp, B drops its cache, and the record goes.
func TestMeshReleaseUnstampsEverywhere(t *testing.T) {
	withFastSync(t)
	readAudit := withAuditFile(t)
	a, b, _ := newMesh(t, true)
	seedConverged(a, b)

	if rec := apiDo(t, b.mux, "POST", "/api/services/app/env/release", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "on its origin dashboard-a") {
		t.Fatalf("release on B = %d %s", rec.Code, rec.Body.String())
	}
	rec := apiDo(t, a.mux, "POST", "/api/services/app/env/release", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("release = %d %s", rec.Code, rec.Body.String())
	}
	if j := a.waitJob(t, "app"); j.Status != envSyncStatusConverged {
		t.Fatalf("A release job = %+v", j)
	}
	b.waitJob(t, "app")
	if a.ce.store.Has("app") {
		t.Fatal("record not deleted")
	}
	if _, ok := b.ce.cache.Get("app"); ok {
		t.Fatal("B kept its cache")
	}
	for _, n := range []*meshNode{a, b} {
		members := n.f.appMembers()
		if len(members) == 0 {
			t.Fatalf("%s has no replicas", n.identity)
		}
		for name, mb := range members {
			if mb.labels[labelEnvOrigin] != "" || mb.labels[labelEnvVersion] != "" || !envHas(mb.env, "A=1") {
				t.Fatalf("%s %s = %+v", n.identity, name, mb)
			}
		}
	}
	// Now plain per-host: scaling resolves nothing centrally.
	if err := b.dc.scaleService(context.Background(), "app", 2); err != nil {
		t.Fatalf("scale after release: %v", err)
	}
	found := false
	for _, e := range readAudit() {
		found = found || e["action"] == "service.env_release"
	}
	if !found {
		t.Fatal("no service.env_release audit")
	}
}

// TestMeshPeerReleaseNeedsOriginReleasing: a direct /release on a peer
// touches nothing unless the origin confirms it is releasing — 409 while its
// record is active, 503 when it can't be asked.
func TestMeshPeerReleaseNeedsOriginReleasing(t *testing.T) {
	withFastSync(t)
	a, b, _ := newMesh(t, true)
	seedConverged(a, b)
	unchanged := func(what string) {
		t.Helper()
		if j, ok := b.m.get("app"); ok || b.m.busy("app") {
			t.Fatalf("%s: B started a job: %+v", what, j)
		}
		if _, ok := b.ce.cache.Get("app"); !ok {
			t.Fatalf("%s: B dropped its cache", what)
		}
		members := b.f.appMembers()
		if len(members) != 1 || members["goproxy-app-1"].labels[labelEnvOrigin] != "dashboard-a" || len(b.f.createsSnapshot()) != 0 {
			t.Fatalf("%s: B's replicas changed: %v", what, members)
		}
	}
	for _, body := range []string{"", `{"origin":"dashboard-a"}`} {
		rec := peerDo(t, b.peerMux, "POST", "/peer/central-env/app/release", "s3cret", body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "origin dashboard-a is not releasing app") {
			t.Fatalf("release with the origin active = %d %s", rec.Code, rec.Body.String())
		}
		unchanged("origin active")
	}

	a.stop()
	rec := peerDo(t, b.peerMux, "POST", "/peer/central-env/app/release", "s3cret", "")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "cannot confirm with origin dashboard-a") {
		t.Fatalf("release with the origin down = %d %s", rec.Code, rec.Body.String())
	}
	unchanged("origin down")
	a.start(t)

	// Marked releasing on the origin: the same call is now accepted.
	if err := a.ce.store.SetState("app", centralEnvStateReleasing); err != nil {
		t.Fatal(err)
	}
	rec = peerDo(t, b.peerMux, "POST", "/peer/central-env/app/release", "s3cret", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("release with the origin releasing = %d %s", rec.Code, rec.Body.String())
	}
	b.waitJob(t, "app")
	if _, ok := b.ce.cache.Get("app"); ok {
		t.Fatal("B kept its cache")
	}
}

// TestMeshReleaseFailureStaysReleasingAndResumes: B fails its unstamped
// roll, so the record stays releasing (Resolve keeps working on both hosts,
// edits are refused); releasing again finishes it.
func TestMeshReleaseFailureStaysReleasingAndResumes(t *testing.T) {
	withFastSync(t)
	a, b, _ := newMesh(t, true)
	seedConverged(a, b)
	b.f.setUnhealthy(func(createBody) bool { return true })

	apiDo(t, a.mux, "POST", "/api/services/app/env/release", "")
	if j := a.waitJob(t, "app"); j.Status != envSyncStatusPartial {
		t.Fatalf("A job = %+v", j)
	}
	b.waitJob(t, "app")
	r, ok, _ := a.ce.store.Get("app")
	if !ok || r.State != centralEnvStateReleasing {
		t.Fatalf("record = %+v", r)
	}
	for name, mb := range a.f.appMembers() {
		if mb.labels[labelEnvOrigin] != "" {
			t.Fatalf("A %s still stamped", name)
		}
	}
	for name, mb := range b.f.appMembers() {
		if mb.labels[labelEnvOrigin] != "dashboard-a" {
			t.Fatalf("B %s lost its stamp on a failed release", name)
		}
	}
	if _, ok := b.ce.cache.Get("app"); !ok {
		t.Fatal("B dropped its cache on a failed release")
	}
	ctx := context.Background()
	if res, managed, err := a.ce.Resolve(ctx, "app", map[string]string{labelEnvOrigin: "dashboard-a"}); err != nil || !managed || !envHas(res.Env, "A=1") {
		t.Fatalf("origin Resolve while releasing: managed=%v err=%v", managed, err)
	}
	if res, managed, err := b.ce.Resolve(ctx, "app", map[string]string{labelEnvOrigin: "dashboard-a"}); err != nil || !managed || !envHas(res.Env, "A=1") {
		t.Fatalf("peer Resolve while releasing: managed=%v err=%v", managed, err)
	}
	if rec := apiDo(t, a.mux, "POST", "/api/services/app/env", `{"if_version":1,"set":{"A":"2"}}`); rec.Code != http.StatusConflict {
		t.Fatalf("edit while releasing = %d %s", rec.Code, rec.Body.String())
	}

	b.f.setUnhealthy(nil)
	if rec := apiDo(t, a.mux, "POST", "/api/services/app/env/release", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("retry = %d %s", rec.Code, rec.Body.String())
	}
	if j := a.waitJob(t, "app"); j.Status != envSyncStatusConverged {
		t.Fatalf("retry job = %+v", j)
	}
	b.waitJob(t, "app")
	if a.ce.store.Has("app") {
		t.Fatal("record not deleted after the retry")
	}
	for name, mb := range b.f.appMembers() {
		if mb.labels[labelEnvOrigin] != "" {
			t.Fatalf("B %s still stamped after the retry", name)
		}
	}
	if _, ok := b.ce.cache.Get("app"); ok {
		t.Fatal("B kept its cache after the retry")
	}
}

// TestMeshAdoptThenReleaseRoundTrip: an adopted service released again ends
// per-host, unstamped, with every host's replicas still running the env it
// had centrally.
func TestMeshAdoptThenReleaseRoundTrip(t *testing.T) {
	withFastSync(t)
	a, b := adoptMesh(t)
	adoptMeshExecute(t, a, `,"import":{"dashboard-b":{"keys":["Y","Z"],"as":"base"}}`)
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	apiDo(t, a.mux, "POST", "/api/services/app/env/release", "")
	if j := a.waitJob(t, "app"); j.Status != envSyncStatusConverged {
		t.Fatalf("release job = %+v", j)
	}
	b.waitJob(t, "app")
	for _, n := range []*meshNode{a, b} {
		for name, mb := range n.f.appMembers() {
			if mb.labels[labelEnvOrigin] != "" || !envHas(mb.env, "Y=y-val") || !envHas(mb.env, "X=x-val") {
				t.Fatalf("%s %s = %+v", n.identity, name, mb)
			}
		}
	}
	if a.ce.store.Has("app") {
		t.Fatal("record left")
	}
}

// centralEnvAdoptOutputs drives every PR-C surface over a mesh whose env is
// all sentinel — dry run, execute with an import, the live-keys peer
// endpoint, a gate failure whose Docker error echoes env (un-adopt), release,
// and the MCP adopt/release tools — and returns every response, job state
// and the audit log for TestCentralEnvNeverLeaksValues.
func centralEnvAdoptOutputs(t *testing.T, sentinel string) []string {
	withFastSync(t)
	readAudit := withAuditFile(t)
	a, b, _ := newMesh(t, true)
	a.f.seedAdoptee("a1", "stack-app-1", []string{"X=" + sentinel, "DIFF=" + sentinel + "-a"}, cenvTemplateHealth, nil)
	b.f.seedAdoptee("b1", "stack-app-1", []string{"DIFF=" + sentinel + "-b", "Y=" + sentinel, "Z=" + sentinel + "-z"}, nil, nil)
	var out []string
	add := func(what, s string) { out = append(out, what+": "+s) }
	addJSON := func(what string, v any) {
		j, _ := json.Marshal(v)
		add(what, string(j))
	}
	api := func(n *meshNode, method, path, body string) string {
		rec := apiDo(t, n.mux, method, path, body)
		add(n.identity+" "+method+" "+path, rec.Body.String())
		return rec.Body.String()
	}
	jobs := func(what string) {
		for _, n := range []*meshNode{a, b} {
			j, _ := n.m.get("app")
			addJSON(what+" job "+n.identity, j)
			st, _ := n.rom.get("app")
			addJSON(what+" rolling "+n.identity, st)
			addJSON(what+" last_failure "+n.identity, n.m.failure("app"))
		}
	}
	s := NewServer("t", "v")
	registerMCPTools(s, &apiCaller{mux: a.mux}, true, true)
	mcp := func(tool, args string) {
		res, _ := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`)
		addJSON("mcp "+tool, res)
	}

	// Gate failure with a Docker error echoing env -> un-adopt.
	a.f.mu.Lock()
	a.f.createErr = func(c createBody) string { return "invalid container config: " + strings.Join(c.Env, " ") }
	a.f.mu.Unlock()
	var rep centralEnvAdoptReport
	json.Unmarshal([]byte(api(a, "POST", "/api/services/app/env/adopt", `{}`)), &rep)
	api(a, "POST", "/api/services/app/env/adopt", `{"dry_run":false,"ack_compose":true,"fingerprint":"`+rep.Fingerprint+`","import":{"dashboard-b":{"keys":["Y","Z"],"as":"base"}}}`)
	a.waitJob(t, "app")
	jobs("unadopt")
	api(a, "GET", "/api/services/app/env", "")
	a.f.mu.Lock()
	a.f.createErr = nil
	a.f.mu.Unlock()

	// Refusals, then a real execute through MCP (dry run first).
	api(a, "POST", "/api/services/app/env/adopt", `{"dry_run":false,"ack_compose":true,"fingerprint":"nope"}`)
	api(a, "POST", "/api/services/app/env/adopt", `{"dry_run":false,"fingerprint":"`+rep.Fingerprint+`"}`)
	mcp("adopt_service_env", `{"service":"app"}`)
	json.Unmarshal([]byte(api(a, "POST", "/api/services/app/env/adopt", `{}`)), &rep)
	mcp("adopt_service_env", `{"service":"app","dry_run":false,"ack_compose":true,"fingerprint":"`+rep.Fingerprint+`","import":{"dashboard-b":{"keys":["Y","DIFF"],"as":"override"}},"accept_dropped":["Z"]}`)
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	if !a.ce.store.Has("app") {
		t.Error("the MCP adopt never landed — the leak check would miss the execute path")
	}
	jobs("adopted")
	api(a, "GET", "/api/services/app/env", "")
	api(b, "GET", "/api/services/app/env", "")
	for _, n := range []*meshNode{a, b} {
		for _, p := range []struct{ method, path string }{
			{"GET", "/peer/central-env/app/live-keys?nonce=" + strings.Repeat("ab", 32)},
			{"GET", "/peer/central-env/app/live-keys?nonce=short"},
			{"GET", "/peer/central-env/app/status"},
		} {
			rec := peerDo(t, n.peerMux, p.method, p.path, "s3cret", "")
			add(n.identity+" peer "+p.method+" "+p.path, rec.Body.String())
		}
		// Refused (managed now): must not answer with values.
		rec := peerDo(t, n.peerMux, "POST", "/peer/central-env/app/live-env", "s3cret", `{"keys":["X","Y"]}`)
		add(n.identity+" peer live-env on managed", rec.Body.String())
	}

	// Release through MCP.
	mcp("release_service_env", `{"service":"app"}`)
	a.waitJob(t, "app")
	b.waitJob(t, "app")
	if a.ce.store.Has("app") {
		t.Error("the MCP release never finished — the leak check would miss the release path")
	}
	jobs("released")
	api(a, "GET", "/api/services/app/env", "")

	entries := readAudit()
	actions := map[string]bool{}
	for _, e := range entries {
		actions[e["action"].(string)] = true
	}
	for _, want := range []string{"service.env_adopt", "service.env_adopt_failed", "service.env_adopt_done", "service.env_adopt_fetch", "service.env_release"} {
		if !actions[want] {
			t.Errorf("audit never recorded %s (got %v)", want, actions)
		}
	}
	addJSON("audit", entries)
	return out
}

func TestCentralEnvAdoptOutputsAreNotEmpty(t *testing.T) {
	outs := strings.Join(centralEnvAdoptOutputs(t, "SENTINEL-S3CRET-9f2a"), "\n")
	for _, want := range []string{"[redacted]", envSyncStatusAdoptUndone, `"peer_only"`, `"keys"`, `"base_keys"`, "fingerprint"} {
		if !strings.Contains(outs, want) {
			t.Errorf("outputs never contained %q", want)
		}
	}
}
