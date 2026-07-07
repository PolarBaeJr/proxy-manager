package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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
