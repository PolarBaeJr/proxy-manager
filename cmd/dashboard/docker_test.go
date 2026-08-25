package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeDocker(t *testing.T, body string) *dockerClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return &dockerClient{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}}}
}

func ctJSON(cs ...map[string]any) string {
	b, _ := json.Marshal(cs)
	return string(b)
}

func ct(name, image, state string, labels map[string]string) map[string]any {
	return map[string]any{
		"Id":     name + "-id",
		"Names":  []string{"/" + name},
		"Image":  image,
		"State":  state,
		"Labels": labels,
	}
}

func pickService(svcs []Service, name string) *Service {
	for i := range svcs {
		if svcs[i].Name == name {
			return &svcs[i]
		}
	}
	return nil
}

func TestListServicesGrouping(t *testing.T) {
	dc := fakeDocker(t, ctJSON(
		ct("goproxy-web-1", "web:1", "running", map[string]string{labelService: "web", labelHost: "web.example.org", labelPort: "80"}),
		ct("goproxy-web-2", "web:1", "exited", map[string]string{labelService: "web", labelHost: "web.example.org", labelPort: "80"}),
		ct("goproxy-web-canary-1", "web:2", "running", map[string]string{labelService: "web", labelHost: "web.example.org", labelPort: "80", labelCanary: "true"}),
		ct("goproxy-db-1", "db:1", "exited", map[string]string{labelService: "db", labelHost: "db.example.org", labelPort: "5432"}),
		// Invalid service label → dropped.
		ct("rogue", "x:1", "running", map[string]string{labelService: "<script>", labelHost: "x.example.org", labelPort: "80"}),
		// Invalid host label → dropped.
		ct("badhost", "x:1", "running", map[string]string{labelService: "other", labelHost: "bad host", labelPort: "80"}),
	))

	svcs, err := dc.listServices(context.Background())
	if err != nil {
		t.Fatalf("listServices: %v", err)
	}
	if len(svcs) != 2 {
		t.Fatalf("got %d services, want 2 (invalid dropped)", len(svcs))
	}
	// Sorted by name.
	if svcs[0].Name != "db" || svcs[1].Name != "web" {
		t.Fatalf("service order = %q, %q", svcs[0].Name, svcs[1].Name)
	}

	web := pickService(svcs, "web")
	if web.Replicas != 2 {
		t.Fatalf("web.Replicas = %d, want 2", web.Replicas)
	}
	if web.CanaryReplicas != 1 || web.CanaryImage != "web:2" {
		t.Fatalf("web canary = %d %q", web.CanaryReplicas, web.CanaryImage)
	}
	if len(web.MemberSummaries) != 3 {
		t.Fatalf("web member summaries = %d, want 3", len(web.MemberSummaries))
	}
	if web.MemberSummaries[0].Name != "goproxy-web-1" {
		t.Fatalf("member summaries not sorted: first = %q", web.MemberSummaries[0].Name)
	}
	if web.AllStopped {
		t.Fatal("web has a running replica; AllStopped should be false")
	}

	db := pickService(svcs, "db")
	if !db.AllStopped {
		t.Fatal("db's only replica is exited; AllStopped should be true")
	}
}

// TestListServicesSelfHealsBareDigest is the regression test for the
// listServices half of Bug B: /containers/json's Image field can decay to a
// bare "sha256:<digest>" once the tag a container was created from is
// retagged/removed locally. listServices must fall back to the container's
// own inspect Config.Image (fixed at creation time) rather than surfacing
// the bare digest as the service's Image — this is the path that feeds
// shouldAutoUpdate's gate for label-managed services, so a stuck bare digest
// here silently and permanently blocks auto-update.
func TestListServicesSelfHealsBareDigest(t *testing.T) {
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "c1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image:  "sha256:" + strings.Repeat("a", 64),
				Labels: map[string]string{labelService: "app", labelHost: "app.example.org", labelPort: "80"},
			}})
		case strings.HasSuffix(r.URL.Path, "/c1/json"):
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Image": "ghcr.io/org/app:v1"}})
		default:
			w.Write([]byte("{}"))
		}
	}))
	svcs, err := dc.listServices(context.Background())
	if err != nil {
		t.Fatalf("listServices: %v", err)
	}
	app := pickService(svcs, "app")
	if app == nil {
		t.Fatal("app service missing")
	}
	if app.Image != "ghcr.io/org/app:v1" {
		t.Fatalf("Image = %q, want ghcr.io/org/app:v1 (self-healed from inspect)", app.Image)
	}
}

// TestListServicesNormalImageSkipsInspect locks in the "zero extra Docker
// calls in the common case" property: a normal (non-decayed) image must
// never trigger the inspect fallback.
func TestListServicesNormalImageSkipsInspect(t *testing.T) {
	var inspectCalls int
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "c1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image:  "ghcr.io/org/app:v1",
				Labels: map[string]string{labelService: "app", labelHost: "app.example.org", labelPort: "80"},
			}})
		case strings.HasSuffix(r.URL.Path, "/c1/json"):
			inspectCalls++
			json.NewEncoder(w).Encode(map[string]any{"Config": map[string]any{"Image": "ghcr.io/org/app:v1"}})
		default:
			w.Write([]byte("{}"))
		}
	}))
	svcs, err := dc.listServices(context.Background())
	if err != nil {
		t.Fatalf("listServices: %v", err)
	}
	app := pickService(svcs, "app")
	if app == nil || app.Image != "ghcr.io/org/app:v1" {
		t.Fatalf("app = %+v", app)
	}
	if inspectCalls != 0 {
		t.Fatalf("inspectCalls = %d, want 0 (non-decayed image should skip inspect)", inspectCalls)
	}
}

func TestListServicesGroupLabel(t *testing.T) {
	rogue := ct("rogue-group", "x:1", "running", map[string]string{labelService: "roguegroup", labelGroup: "<script>"})
	dc := fakeDocker(t, ctJSON(
		ct("goproxy-web-1", "web:1", "running", map[string]string{labelService: "web", labelHost: "web.example.org", labelPort: "80", labelGroup: "myapp"}),
		ct("goproxy-db-1", "db:1", "running", map[string]string{labelService: "db"}),
		rogue,
	))
	svcs, err := dc.listServices(context.Background())
	if err != nil {
		t.Fatalf("listServices: %v", err)
	}
	web := pickService(svcs, "web")
	if web == nil || web.Group != "myapp" {
		t.Fatalf("web.Group = %+v, want %q", web, "myapp")
	}
	// No proxy.group label → defaults to the service's own name.
	db := pickService(svcs, "db")
	if db == nil || db.Group != "db" {
		t.Fatalf("db.Group = %+v, want defaulted to %q", db, "db")
	}
	// Invalid proxy.group label → the whole container is dropped, same as an
	// invalid proxy.host/proxy.path label.
	if pickService(svcs, "roguegroup") != nil {
		t.Fatal("container with invalid proxy.group label should have been skipped")
	}
}

func TestListServicesMemberHealth(t *testing.T) {
	c := ct("goproxy-web-1", "web:1", "running", map[string]string{labelService: "web", labelHost: "web.example.org", labelPort: "80"})
	c["Status"] = "Up 2 minutes (healthy)"
	dc := fakeDocker(t, ctJSON(c))
	svcs, err := dc.listServices(context.Background())
	if err != nil {
		t.Fatalf("listServices: %v", err)
	}
	web := pickService(svcs, "web")
	if web == nil || len(web.MemberSummaries) != 1 {
		t.Fatalf("web = %+v", web)
	}
	if web.MemberSummaries[0].Health != "healthy" {
		t.Fatalf("Health = %q, want %q", web.MemberSummaries[0].Health, "healthy")
	}
}

func TestParseHealth(t *testing.T) {
	cases := []struct{ status, want string }{
		{"Up 2 minutes (healthy)", "healthy"},
		{"Up 2 minutes (unhealthy)", "unhealthy"},
		{"Up 2 seconds (health: starting)", "starting"},
		{"Up 2 minutes", ""},
		{"Exited (0) 3 minutes ago", ""},
	}
	for _, c := range cases {
		if got := parseHealth(c.status); got != c.want {
			t.Errorf("parseHealth(%q) = %q, want %q", c.status, got, c.want)
		}
	}
}

func TestNextReplicaIndex(t *testing.T) {
	existing := []dockerContainer{
		{Names: []string{"/goproxy-foo-1"}},
		{Names: []string{"/goproxy-foo-3"}},
		{Names: []string{"/unrelated"}},
	}
	if got := nextReplicaIndex(existing, "foo"); got != 4 {
		t.Fatalf("nextReplicaIndex = %d, want 4", got)
	}
	if got := nextReplicaIndex(nil, "foo"); got != 1 {
		t.Fatalf("nextReplicaIndex(empty) = %d, want 1", got)
	}
}

func TestLiveOnly(t *testing.T) {
	in := []dockerContainer{
		{ID: "a", Labels: map[string]string{}},
		{ID: "b", Labels: map[string]string{labelCanary: "true"}},
		{ID: "c", Labels: map[string]string{}},
	}
	live := liveOnly(in)
	if len(live) != 2 || live[0].ID != "a" || live[1].ID != "c" {
		t.Fatalf("liveOnly = %+v", live)
	}
}

func TestCanaryOnly(t *testing.T) {
	in := []dockerContainer{
		{ID: "a", Labels: map[string]string{}},
		{ID: "b", Labels: map[string]string{labelCanary: "true"}},
	}
	canary := canaryOnly(in)
	if len(canary) != 1 || canary[0].ID != "b" {
		t.Fatalf("canaryOnly = %+v", canary)
	}
}

func TestGuardUnscalable(t *testing.T) {
	dc := fakeDocker(t, ctJSON(
		ct("goproxy-web-1", "web:1", "running", map[string]string{labelService: "web", labelUnscalable: "true"}),
	))
	if err := dc.guardUnscalable(context.Background(), "web", 2); err == nil {
		t.Fatal("scaling an unscalable service past 1 should error")
	}
	if err := dc.guardUnscalable(context.Background(), "web", 1); err != nil {
		t.Fatalf("desired=1 should be allowed: %v", err)
	}

	empty := fakeDocker(t, "[]")
	if err := empty.guardUnscalable(context.Background(), "web", 5); err != nil {
		t.Fatalf("no containers should allow scaling: %v", err)
	}
}

// TestReplaceServiceRefusesPortBindings is the regression test for the
// self-inflicted-outage bug: replaceService used to happily recreate a
// container that published host ports, silently dropping the bindings.
func TestReplaceServiceRefusesPortBindings(t *testing.T) {
	var sawCreate bool
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image:  "ghcr.io/org/app:v1",
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			w.Write([]byte(`{
				"Image": "sha256:abc",
				"HostConfig": {"PortBindings": {"80/tcp": [{"HostPort": "8080"}]}},
				"Config": {},
				"NetworkSettings": {"Networks": {"edge": {}}}
			}`))
		case strings.Contains(r.URL.Path, "/containers/create"):
			sawCreate = true
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		default:
			w.Write([]byte("{}"))
		}
	}))

	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	if err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}); err == nil {
		t.Fatal("replaceService should refuse a template with PortBindings")
	}
	if sawCreate {
		t.Fatal("no container should have been created")
	}
}

// TestReplaceServiceProceedsWithoutHostPorts confirms the new guard doesn't
// block the common case: a template with no unreproducible HostConfig
// fields still gets replaced.
func TestReplaceServiceProceedsWithoutHostPorts(t *testing.T) {
	var sawCreate bool
	dc := dockerStub(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			json.NewEncoder(w).Encode([]dockerContainer{{
				ID: "tpl1", Names: []string{"/goproxy-app-1"}, State: "running",
				Image:  "ghcr.io/org/app:v1",
				Labels: map[string]string{labelEnable: "true", labelService: "app", labelHost: "app.example"},
			}})
		case strings.HasSuffix(r.URL.Path, "/tpl1/json"):
			w.Write([]byte(`{
				"Image": "sha256:abc",
				"Config": {"Env": []},
				"HostConfig": {"Mounts": []},
				"NetworkSettings": {"Networks": {"edge": {}}}
			}`))
		case strings.Contains(r.URL.Path, "/containers/create"):
			sawCreate = true
			json.NewEncoder(w).Encode(map[string]any{"Id": "new1"})
		default:
			w.Write([]byte("{}"))
		}
	}))

	old := replaceSettleDelay
	replaceSettleDelay = 0
	t.Cleanup(func() { replaceSettleDelay = old })

	if err := dc.replaceService(context.Background(), "app", ReplaceServiceRequest{Image: "ghcr.io/org/app:v2"}); err != nil {
		t.Fatalf("replaceService: %v", err)
	}
	if !sawCreate {
		t.Fatal("no container was created")
	}
}
