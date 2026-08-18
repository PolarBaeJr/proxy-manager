package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestManagedOnlyGuards proves the lifecycle actions reject a managed-only
// record (empty Host) BEFORE any Docker IO — the guard sits ahead of listAll,
// so a zero-value *dockerClient (no socket) never gets called.
func TestManagedOnlyGuards(t *testing.T) {
	store, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(OnboardedService{Name: "mo", Host: "", Image: "img", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	dc := &dockerClient{} // zero value: any Docker call would fail/panic
	ctx := context.Background()
	routes := filepath.Join(t.TempDir(), "routes.json")

	check := func(label string, err error) {
		if err == nil || !strings.Contains(err.Error(), "managed-only") {
			t.Fatalf("%s: err = %v, want a managed-only error", label, err)
		}
	}
	check("scale", dc.scaleOnboarded(ctx, "mo", 2, store, routes))
	check("stage", dc.stageOnboarded(ctx, "mo", ReplaceServiceRequest{Image: "x"}, store, routes))
	check("replace", dc.replaceOnboarded(ctx, "mo", ReplaceServiceRequest{Image: "x"}, store, routes))
	check("promote", dc.promoteOnboarded(ctx, "mo", store, routes))
}

func TestNextCloneIndex(t *testing.T) {
	clones := []dockerContainer{
		{Names: []string{"/goproxy-onb-app-1"}},
		{Names: []string{"/goproxy-onb-app-3"}},
		{Names: []string{"/unrelated"}},
	}
	if got := nextCloneIndex(clones, "app"); got != 4 {
		t.Fatalf("nextCloneIndex = %d, want 4", got)
	}
	if got := nextCloneIndex(nil, "app"); got != 1 {
		t.Fatalf("nextCloneIndex(empty) = %d, want 1", got)
	}
}

func TestSortByNameDesc(t *testing.T) {
	in := []dockerContainer{
		{Names: []string{"/goproxy-onb-app-1"}},
		{Names: []string{"/goproxy-onb-app-3"}},
		{Names: []string{"/goproxy-onb-app-2"}},
	}
	sortByNameDesc(in)
	got := []string{in[0].name(), in[1].name(), in[2].name()}
	want := []string{"goproxy-onb-app-3", "goproxy-onb-app-2", "goproxy-onb-app-1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortByNameDesc = %v, want %v", got, want)
		}
	}
}

func TestUpsertOnboardedRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	// Seed a user-curated entry that must survive upserts.
	if err := writeRoutesFile(path, routesFile{Routes: []routesEntry{
		{Name: "curated", Host: "curated.example.org", Backends: []string{"http://c:80"}},
	}}); err != nil {
		t.Fatal(err)
	}

	// Insert with a path prefix + strip.
	if err := upsertOnboardedRoute(path, "myapp", "myapp.example.org", "/admin", true, []string{"http://myapp:3000"}); err != nil {
		t.Fatal(err)
	}
	f, err := readRoutesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Routes) != 2 {
		t.Fatalf("after insert routes = %d, want 2", len(f.Routes))
	}
	for _, r := range f.Routes {
		if r.Onboarded == "myapp" {
			if r.Path != "/admin" || !r.Strip {
				t.Fatalf("inserted entry path/strip = %q/%v, want /admin/true", r.Path, r.Strip)
			}
		}
	}

	// Update in place — count unchanged, host updated, curated preserved.
	// Empty path + strip=false must be omitted from the marshaled JSON.
	if err := upsertOnboardedRoute(path, "myapp", "new.example.org", "", false, []string{"http://myapp:4000"}); err != nil {
		t.Fatal(err)
	}
	f, _ = readRoutesFile(path)
	if len(f.Routes) != 2 {
		t.Fatalf("after update routes = %d, want 2 (in-place)", len(f.Routes))
	}
	var curatedOK, onboardedOK bool
	for _, r := range f.Routes {
		if r.Onboarded == "" && r.Host == "curated.example.org" {
			curatedOK = true
		}
		if r.Onboarded == "myapp" && r.Host == "new.example.org" && r.Backends[0] == "http://myapp:4000" {
			if r.Path != "" || r.Strip {
				t.Fatalf("updated entry path/strip = %q/%v, want empty/false", r.Path, r.Strip)
			}
			onboardedOK = true
		}
	}
	if !curatedOK {
		t.Fatal("curated entry was clobbered")
	}
	if !onboardedOK {
		t.Fatal("onboarded entry not updated in place")
	}
	// The empty path/strip must not appear in the raw file (omitempty).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"path"`) || strings.Contains(string(raw), `"strip"`) {
		t.Fatalf("routes.json should omit empty path/strip, got:\n%s", raw)
	}
}

func TestRemoveOnboardedRoute(t *testing.T) {
	// Missing file → no-op (no error).
	missing := filepath.Join(t.TempDir(), "routes.json")
	if err := removeOnboardedRoute(missing, "myapp"); err != nil {
		t.Fatalf("remove on missing file = %v, want nil", err)
	}

	// Existing: onboarded entry removed, curated kept.
	path := filepath.Join(t.TempDir(), "routes.json")
	if err := writeRoutesFile(path, routesFile{Routes: []routesEntry{
		{Name: "curated", Host: "curated.example.org", Backends: []string{"http://c:80"}},
		{Name: "onboarded: myapp", Host: "myapp.example.org", Backends: []string{"http://myapp:3000"}, Onboarded: "myapp"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := removeOnboardedRoute(path, "myapp"); err != nil {
		t.Fatal(err)
	}
	f, _ := readRoutesFile(path)
	if len(f.Routes) != 1 || f.Routes[0].Host != "curated.example.org" {
		t.Fatalf("after remove routes = %+v, want only curated", f.Routes)
	}
}

// ---- Stateful fake Docker daemon (create/start/stop/remove/inspect) ----
//
// Unlike fakeDocker in docker_test.go (a single canned GET response), promote/
// stage flows need a daemon that remembers containers across several calls
// (listAll → create → start → listAll → inspect → stop → remove), so tests can
// assert on env that only ever reached a live container, never the store.

type fakeContainer struct {
	id, name, image, state string
	env                    []string
}

type fakeDockerState struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer // keyed by ID
}

func newFakeDockerServer(t *testing.T, seed ...*fakeContainer) *dockerClient {
	t.Helper()
	st := &fakeDockerState{containers: map[string]*fakeContainer{}}
	for _, c := range seed {
		st.containers[c.id] = c
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/"+dockerAPI)
		switch {
		case r.Method == "GET" && path == "/containers/json":
			var nameSubstr string
			if fq := r.URL.Query().Get("filters"); fq != "" {
				var f struct {
					Name []string `json:"name"`
				}
				_ = json.Unmarshal([]byte(fq), &f)
				if len(f.Name) > 0 {
					nameSubstr = f.Name[0]
				}
			}
			var out []map[string]any
			for _, c := range st.containers {
				if nameSubstr != "" && !strings.Contains(c.name, nameSubstr) {
					continue
				}
				out = append(out, map[string]any{
					"Id": c.id, "Names": []string{"/" + c.name}, "Image": c.image, "State": c.state,
				})
			}
			w.Header().Set("Content-Type", "application/json")
			b, _ := json.Marshal(out)
			_, _ = w.Write(b)
		case r.Method == "POST" && path == "/containers/create":
			name := r.URL.Query().Get("name")
			var body struct {
				Image string
				Env   []string
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := name + "-id"
			st.containers[id] = &fakeContainer{id: id, name: name, image: body.Image, env: body.Env, state: "created"}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case r.Method == "POST" && strings.HasSuffix(path, "/start"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/start")
			if c, ok := st.containers[id]; ok {
				c.state = "running"
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "POST" && strings.HasSuffix(path, "/stop"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/stop")
			if c, ok := st.containers[id]; ok {
				c.state = "exited"
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
			c, ok := st.containers[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Env": c.env}})
		case r.Method == "DELETE" && strings.HasPrefix(path, "/containers/"):
			id := strings.TrimPrefix(path, "/containers/")
			delete(st.containers, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return &dockerClient{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}}}
}

// TestPromoteOnboardedPersistsEnv is the regression test for bug #2: an env
// edit made via stage_canary must survive promote into OnboardedService.Env,
// not just live on in the (now-live) canary container. Before the fix,
// promoteOnboarded flipped svc.Image but left svc.Env untouched, so a later
// scale-up would clone the STALE pre-edit env.
func TestPromoteOnboardedPersistsEnv(t *testing.T) {
	dc := newFakeDockerServer(t,
		&fakeContainer{
			id: "goproxy-onb-app-c1-id", name: "goproxy-onb-app-c1",
			image: "img:v2", env: []string{"FOO=new", "BAR=1"}, state: "running",
		},
	)
	store, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	routes := filepath.Join(t.TempDir(), "routes.json")
	svc := OnboardedService{
		Name: "app", Host: "app.example.org", Port: 8080,
		Image: "img:v1", Env: []string{"FOO=old"},
		Replicas: 1, OriginalRouted: true,
		CanaryImage: "img:v2", CanaryReplicas: 1,
	}
	if err := store.Put(svc); err != nil {
		t.Fatal(err)
	}

	if err := dc.promoteOnboarded(context.Background(), "app", store, routes); err != nil {
		t.Fatalf("promoteOnboarded: %v", err)
	}

	got, ok := store.Get("app")
	if !ok {
		t.Fatal("app missing from store after promote")
	}
	want := []string{"FOO=new", "BAR=1"}
	if len(got.Env) != len(want) || got.Env[0] != want[0] || got.Env[1] != want[1] {
		t.Fatalf("Env after promote = %v, want %v (the edited canary env, not the stale pre-edit env)", got.Env, want)
	}
	if got.Image != "img:v2" {
		t.Fatalf("Image after promote = %q, want img:v2", got.Image)
	}
	if got.CanaryImage != "" {
		t.Fatalf("CanaryImage after promote = %q, want empty", got.CanaryImage)
	}
}

// TestOnboardedBaseEnvUsesPromotedContainer is the regression test for bug
// #3: once a canary is promoted (CanaryImage cleared), the sole surviving
// c-prefixed container IS the live service, and onboardedBaseEnv must read
// its env — not unconditionally skip it and fall through to the stale
// original container.
func TestOnboardedBaseEnvUsesPromotedContainer(t *testing.T) {
	dc := newFakeDockerServer(t,
		// Original container: still running, but no longer routed — its env
		// is the stale pre-promote value.
		&fakeContainer{id: "app-id", name: "app", image: "img:v1", env: []string{"FOO=stale"}, state: "running"},
		// The promoted (now-live) container — this is what onboardedBaseEnv
		// must return.
		&fakeContainer{id: "goproxy-onb-app-c1-id", name: "goproxy-onb-app-c1", image: "img:v2", env: []string{"FOO=live", "BAR=2"}, state: "running"},
	)
	svc := OnboardedService{
		Name: "app", Host: "app.example.org", Port: 8080,
		Image: "img:v2", Env: []string{"FOO=snapshot"}, // stale store snapshot too
		Replicas: 1, OriginalRouted: false,
		CanaryImage: "", // promote already ran
	}

	got := dc.onboardedBaseEnv(context.Background(), "app", svc)
	want := []string{"FOO=live", "BAR=2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("onboardedBaseEnv = %v, want %v (the promoted container's live env)", got, want)
	}
}

// TestPromoteToOnboardedCapturesPathAndStrip is the regression test for the
// sfubadminton.com incident: a label-managed container with proxy.path=/admin
// gets auto-onboarded (first stop/start), but promoteToOnboarded dropped Path
// and Strip when building the OnboardedService record. The stored route then
// keyed as host|"" instead of host|/admin, so router.go's static-vs-label
// dedup (keyed on host+path) never matched the original's label route and a
// stray onboarded backend silently took over every non-/admin request to the
// host.
func TestPromoteToOnboardedCapturesPathAndStrip(t *testing.T) {
	dc := newFakeDockerServer(t,
		&fakeContainer{id: "admin-id", name: "admin", image: "img:v1", env: []string{"FOO=1"}, state: "running"},
	)
	store, err := loadOnboardedStore(filepath.Join(t.TempDir(), "onboarded.json"))
	if err != nil {
		t.Fatal(err)
	}
	svc := Service{
		Name: "admin", Host: "sfubadminton.com", Port: 3001, Path: "/admin",
		Labels: map[string]string{"proxy.path": "/admin", "proxy.strip": "true"},
		Replicas: 1,
		Members:  []dockerContainer{{ID: "admin-id", Labels: map[string]string{}}},
	}
	if err := promoteToOnboarded(context.Background(), dc, store, svc); err != nil {
		t.Fatal(err)
	}
	got, ok := store.Get("admin")
	if !ok {
		t.Fatal("onboarded record not persisted")
	}
	if got.Path != "/admin" {
		t.Errorf("Path = %q, want /admin", got.Path)
	}
	if !got.Strip {
		t.Error("Strip = false, want true")
	}
}
