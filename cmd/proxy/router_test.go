package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// fakeDocker returns a dockerClient whose transport dials an httptest server
// that answers every request with the given body — standing in for the
// /containers/json listing.
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

// dockerJSON marshals containers into the daemon's /containers/json shape,
// matching dockerContainer's struct tags exactly.
func dockerJSON(cs ...map[string]any) string {
	b, _ := json.Marshal(cs)
	return string(b)
}

func container(id, name, state string, labels, networks map[string]string) map[string]any {
	nets := map[string]any{}
	for n, ip := range networks {
		nets[n] = map[string]any{"IPAddress": ip}
	}
	return map[string]any{
		"Id":              id,
		"Names":           []string{"/" + name},
		"State":           state,
		"Labels":          labels,
		"NetworkSettings": map[string]any{"Networks": nets},
	}
}

func findGroup(groups []*RouteGroup, host, path string) *RouteGroup {
	for _, g := range groups {
		if g.Host == host && g.PathPrefix == path {
			return g
		}
	}
	return nil
}

func TestAssembleGroupsLabelParsing(t *testing.T) {
	dc := fakeDocker(t, dockerJSON(
		// Running, dual-homed: edge IP must win over bridge.
		container("a", "app-a", "running",
			map[string]string{labelHost: "a.example.org", labelPort: "8080"},
			map[string]string{"bridge": "172.17.0.9", managedNetwork: "172.20.0.5"}),
		// Missing host label → skipped.
		container("b", "app-b", "running",
			map[string]string{labelPort: "8080"}, nil),
		// Bad port → skipped.
		container("c", "app-c", "running",
			map[string]string{labelHost: "c.example.org", labelPort: "notaport"}, nil),
		// Stopped → group with zero backends.
		container("d", "app-d", "exited",
			map[string]string{labelHost: "d.example.org", labelPort: "9090"}, nil),
	))

	groups, err := assembleGroups(context.Background(), dc, "")
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}

	a := findGroup(groups, "a.example.org", "")
	if a == nil || len(a.Backends) != 1 {
		t.Fatalf("a group = %+v", a)
	}
	if a.Backends[0].URL != "http://172.20.0.5:8080" {
		t.Fatalf("backend URL = %q, want edge IP preferred", a.Backends[0].URL)
	}

	if findGroup(groups, "c.example.org", "") != nil {
		t.Fatal("bad-port container should not produce a group")
	}
	if g := findGroup(groups, "d.example.org", ""); g == nil || len(g.Backends) != 0 {
		t.Fatalf("stopped container should be a group with zero backends, got %+v", g)
	}
}

func TestAssembleAuthMergeRule(t *testing.T) {
	dc := fakeDocker(t, dockerJSON(
		// First replica sets the allowlist.
		container("r1", "app-r1", "running",
			map[string]string{labelHost: "mcp.example.org", labelPort: "8080", labelAuthUsers: "Alice, bob"},
			map[string]string{managedNetwork: "172.20.0.11"}),
		// Second replica flips auth on and to oauth mode.
		container("r2", "app-r2", "running",
			map[string]string{labelHost: "mcp.example.org", labelPort: "8080", labelAuth: "true", labelAuthMode: "oauth"},
			map[string]string{managedNetwork: "172.20.0.12"}),
	))

	groups, err := assembleGroups(context.Background(), dc, "")
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	g := findGroup(groups, "mcp.example.org", "")
	if g == nil {
		t.Fatal("group missing")
	}
	if !g.AuthRequired {
		t.Fatal("AuthRequired should be true (any replica proxy.auth=true)")
	}
	if g.AuthMode != "oauth" {
		t.Fatalf("AuthMode = %q, want oauth", g.AuthMode)
	}
	if len(g.AuthUsers) != 2 || g.AuthUsers[0] != "alice" || g.AuthUsers[1] != "bob" {
		t.Fatalf("AuthUsers = %v, want normalized [alice bob]", g.AuthUsers)
	}
}

func TestNormalizeAuthUsers(t *testing.T) {
	got := normalizeAuthUsers([]string{" Alice ", "", "BOB", "  "})
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("normalizeAuthUsers = %v", got)
	}
	if normalizeAuthUsers(nil) != nil {
		t.Fatal("nil input should yield nil")
	}
}

func mkGroup(t *testing.T, host, prefix string, strip bool, target string) *RouteGroup {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse %q: %v", target, err)
	}
	b := makeBackend(target, 1, "c", "", u, host)
	return &RouteGroup{Host: host, PathPrefix: prefix, StripPrefix: strip, Backends: []*Backend{b}}
}

func TestServeHTTPHostMatch(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	defer backend.Close()

	r := &Router{}
	r.Set([]*RouteGroup{mkGroup(t, "a.example.org", "", false, backend.URL)})

	// Matched host is proxied.
	rec := httptest.NewRecorder()
	aw := &accessWriter{ResponseWriter: rec}
	req := httptest.NewRequest("GET", "http://a.example.org/", nil)
	r.ServeHTTP(aw, req)
	if rec.Code != 200 || rec.Body.String() != "hello" {
		t.Fatalf("matched: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// Unknown host → 404 and MarkUnrouted fired.
	rec = httptest.NewRecorder()
	aw = &accessWriter{ResponseWriter: rec}
	req = httptest.NewRequest("GET", "http://unknown.example.org/", nil)
	r.ServeHTTP(aw, req)
	if rec.Code != 404 {
		t.Fatalf("unknown host code = %d, want 404", rec.Code)
	}
	if !aw.unrouted {
		t.Fatal("MarkUnrouted should have fired for an unrouted host")
	}
}

func TestServeHTTPLongestPrefix(t *testing.T) {
	apiBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("api"))
	}))
	defer apiBackend.Close()
	rootBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("root"))
	}))
	defer rootBackend.Close()

	r := &Router{}
	r.Set([]*RouteGroup{
		mkGroup(t, "a.example.org", "", false, rootBackend.URL),
		mkGroup(t, "a.example.org", "/api", false, apiBackend.URL),
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, httptest.NewRequest("GET", "http://a.example.org/api/x", nil))
	if rec.Body.String() != "api" {
		t.Fatalf("/api/x → %q, want api (longest prefix wins)", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, httptest.NewRequest("GET", "http://a.example.org/other", nil))
	if rec.Body.String() != "root" {
		t.Fatalf("/other → %q, want root", rec.Body.String())
	}
}

func TestServeHTTPStripPrefix(t *testing.T) {
	var seen string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	r := &Router{}
	r.Set([]*RouteGroup{mkGroup(t, "a.example.org", "/api", true, backend.URL)})

	rec := httptest.NewRecorder()
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, httptest.NewRequest("GET", "http://a.example.org/api/foo", nil))
	if seen != "/foo" {
		t.Fatalf("backend saw path %q, want /foo (prefix stripped)", seen)
	}
}
