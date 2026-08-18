package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
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
	// Legitimate multi-replica case: two label-managed containers sharing the
	// same host+path (no static entry involved) must both end up as backends
	// in the same group.
	if len(g.Backends) != 2 {
		t.Fatalf("Backends = %d, want 2 (both replicas joined the same label-managed group)", len(g.Backends))
	}
}

// TestAssembleGroupsStaticOwnsHost proves a static/onboarded route (from
// routes.json) gets exclusive ownership of its host+path: a still-labeled
// docker container for the same key must not re-join the group as a backend,
// even though the label loop runs after the static-config loop.
func TestAssembleGroupsStaticOwnsHost(t *testing.T) {
	cfgPath := t.TempDir() + "/routes.json"
	cfg := staticConfig{Routes: []staticRoute{
		{Host: "example.com", Backends: []string{"http://10.0.0.9:8080"}},
	}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal static config: %v", err)
	}
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatalf("write static config: %v", err)
	}

	dc := fakeDocker(t, dockerJSON(
		// Original container still carries live proxy.* labels for the same
		// host, e.g. the not-relabeled original of an auto-onboarded service.
		container("orig", "app-orig", "running",
			map[string]string{labelHost: "example.com", labelPort: "8080"},
			map[string]string{managedNetwork: "172.20.0.5"}),
	))

	groups, err := assembleGroups(context.Background(), dc, cfgPath)
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	g := findGroup(groups, "example.com", "")
	if g == nil {
		t.Fatal("group missing")
	}
	if len(g.Backends) != 1 || g.Backends[0].URL != "http://10.0.0.9:8080" {
		t.Fatalf("Backends = %+v, want exactly the static backend http://10.0.0.9:8080", g.Backends)
	}
	for _, b := range g.Backends {
		if strings.Contains(b.URL, "172.20.0.5") {
			t.Fatalf("label-discovered container leaked into a static-owned group: %+v", g.Backends)
		}
	}

	// Regression check: with no static route for a host, label-discovered
	// containers still create/populate a group exactly as before.
	dc2 := fakeDocker(t, dockerJSON(
		container("free", "app-free", "running",
			map[string]string{labelHost: "unowned.example.com", labelPort: "9090"},
			map[string]string{managedNetwork: "172.20.0.6"}),
	))
	groups2, err := assembleGroups(context.Background(), dc2, cfgPath)
	if err != nil {
		t.Fatalf("assembleGroups (unowned): %v", err)
	}
	g2 := findGroup(groups2, "unowned.example.com", "")
	if g2 == nil || len(g2.Backends) != 1 || g2.Backends[0].URL != "http://172.20.0.6:9090" {
		t.Fatalf("unowned host group = %+v, want single label-discovered backend", g2)
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

// TestServeHTTPUnroutedIsThrottled is the regression test for the finding
// that unrouted requests (bad Host/path) bypassed rate limiting entirely —
// exactly the traffic shape a scanner produces. With r.unroutedLimiter set,
// repeated unmatched requests from the same IP must eventually get a 429
// instead of an unthrottled stream of 404s.
func TestServeHTTPUnroutedIsThrottled(t *testing.T) {
	r := &Router{}
	r.Set([]*RouteGroup{mkGroup(t, "a.example.org", "", false, "http://127.0.0.1:1")})
	r.unroutedLimiter = newRateLimiter(3)

	saw404, saw429 := 0, 0
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://unknown.example.org/", nil)
		req.RemoteAddr = "203.0.113.9:1000"
		r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)
		switch rec.Code {
		case 404:
			saw404++
		case 429:
			saw429++
			if ra := rec.Header().Get("Retry-After"); ra == "" {
				t.Error("429 response missing Retry-After header")
			}
		default:
			t.Fatalf("unexpected code %d", rec.Code)
		}
	}
	if saw404 != 3 || saw429 != 2 {
		t.Fatalf("got %d×404 + %d×429, want 3×404 then 2×429 (capacity 3)", saw404, saw429)
	}
}

// TestServeHTTPRetryAfterMatchesRPM: Retry-After must reflect the group's
// actual rpm instead of a fixed guess — a fast group should ask for a much
// shorter wait than a slow one.
func TestServeHTTPRetryAfterMatchesRPM(t *testing.T) {
	fast := mkGroup(t, "fast.example.org", "", false, "http://127.0.0.1:1")
	fast.RateLimit, fast.RateRPM = true, 600 // ceil(60/600) = 1s
	slow := mkGroup(t, "slow.example.org", "", false, "http://127.0.0.1:1")
	slow.RateLimit, slow.RateRPM = true, 6 // ceil(60/6) = 10s

	r := &Router{}
	r.Set([]*RouteGroup{fast, slow})

	// Exhaust the slow group (capacity 6) fully, then check the 7th request.
	var rec *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		rec = httptest.NewRecorder()
		req := httptest.NewRequest("GET", "http://slow.example.org/", nil)
		req.RemoteAddr = "203.0.113.9:1000"
		r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://slow.example.org/", nil)
	req.RemoteAddr = "203.0.113.9:1000"
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)
	if rec.Code != 429 {
		t.Fatalf("slow group: code = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "10" {
		t.Fatalf("slow group (rpm=6): Retry-After = %q, want 10", got)
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

// TestServeHTTPWebSocketUpgrade drives a real protocol upgrade through the full
// production writer chain — withMetrics → withAccessLog → Router → errCatchingWriter.
// Both accessWriter and errCatchingWriter must forward Hijack() or the upgrade
// fails and the reverse proxy's ErrorHandler falsely marks the backend unhealthy.
// httptest.NewRecorder is not a Hijacker, so this test needs real TCP sockets.
func TestServeHTTPWebSocketUpgrade(t *testing.T) {
	// Backend that speaks a minimal WebSocket-style handshake: on an Upgrade
	// request it hijacks the conn, writes 101, then echoes one line back.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Connection"), "Upgrade") {
			http.Error(w, "expected upgrade", http.StatusBadRequest)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("backend hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
		line, _ := buf.ReadString('\n')
		_, _ = buf.WriteString("echo:" + line)
		_ = buf.Flush()
	}))
	defer backend.Close()

	r := &Router{}
	group := mkGroup(t, "ws.example.org", "", false, backend.URL)
	r.Set([]*RouteGroup{group})

	// Front server wearing the exact same middleware stack as production.
	handler := withMetrics(withAccessLog(r, NewAccessLog()), NewMetrics())
	front := httptest.NewServer(handler)
	defer front.Close()

	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: ws.example.org\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status line = %q, want 101 Switching Protocols", strings.TrimSpace(status))
	}
	// Drain the rest of the response headers up to the blank line.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	// Bytes must flow both ways over the hijacked connection.
	fmt.Fprintf(conn, "ping\n")
	echo, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if strings.TrimSpace(echo) != "echo:ping" {
		t.Fatalf("echo = %q, want echo:ping", strings.TrimSpace(echo))
	}

	// The actual bug symptom: a failed upgrade trips ErrorHandler, which marks
	// the backend unhealthy and causes spurious 503s on later requests.
	if !group.Backends[0].healthy() {
		t.Fatal("backend was marked unhealthy — upgrade tripped the ErrorHandler")
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
