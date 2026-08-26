package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
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

// containerWithStatus is a sibling to container() that also sets the raw
// docker Status string (e.g. "Up 2 minutes (unhealthy)"), for exercising the
// Docker health-status floor without touching container()'s signature/callers.
func containerWithStatus(id, name, state, status string, labels, networks map[string]string) map[string]any {
	c := container(id, name, state, labels, networks)
	c["Status"] = status
	return c
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

// writeStaticConfig is a small helper for the Service-field tests below.
func writeStaticConfig(t *testing.T, routes ...staticRoute) string {
	t.Helper()
	cfgPath := t.TempDir() + "/routes.json"
	data, err := json.Marshal(staticConfig{Routes: routes})
	if err != nil {
		t.Fatalf("marshal static config: %v", err)
	}
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatalf("write static config: %v", err)
	}
	return cfgPath
}

// TestAssembleGroupsServiceFieldLiteralOnly proves existing literal-backends
// behavior is unaffected by the Service field's addition — no regression.
func TestAssembleGroupsServiceFieldLiteralOnly(t *testing.T) {
	cfgPath := writeStaticConfig(t, staticRoute{Host: "literal.example.com", Backends: []string{"http://10.0.0.9:8080"}})
	dc := fakeDocker(t, dockerJSON())

	groups, err := assembleGroups(context.Background(), dc, cfgPath)
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	g := findGroup(groups, "literal.example.com", "")
	if g == nil || len(g.Backends) != 1 || g.Backends[0].URL != "http://10.0.0.9:8080" {
		t.Fatalf("group = %+v, want exactly the literal backend", g)
	}
}

// TestAssembleGroupsServiceFieldResolvesBackends proves a routes.json entry
// with only a Service field (no literal Backends) picks up backends from
// every running, non-canary, proxy.service-labeled container.
func TestAssembleGroupsServiceFieldResolvesBackends(t *testing.T) {
	cfgPath := writeStaticConfig(t, staticRoute{Host: "svc.example.com", Path: "/admin", Service: "auth"})
	dc := fakeDocker(t, dockerJSON(
		container("a1", "auth-1", "running",
			map[string]string{labelHost: "auth.internal.example.com", labelPort: "9000", labelService: "auth"},
			map[string]string{managedNetwork: "172.20.0.11"}),
		container("a2", "auth-2", "running",
			map[string]string{labelHost: "auth.internal.example.com", labelPort: "9000", labelService: "auth"},
			map[string]string{managedNetwork: "172.20.0.12"}),
	))

	groups, err := assembleGroups(context.Background(), dc, cfgPath)
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	g := findGroup(groups, "svc.example.com", "/admin")
	if g == nil {
		t.Fatal("service-resolved group missing")
	}
	if len(g.Backends) != 2 {
		t.Fatalf("Backends = %+v, want both auth-1 and auth-2", g.Backends)
	}

	// The service-labeled containers must ALSO still appear in their own
	// default label-managed route — service resolution doesn't steal them.
	own := findGroup(groups, "auth.internal.example.com", "")
	if own == nil || len(own.Backends) != 2 {
		t.Fatalf("own label-managed group = %+v, want both replicas present", own)
	}
}

// TestAssembleGroupsServiceFieldCombinedWithLiteral proves a routes.json
// entry can set BOTH Backends and Service — both contribute to the group.
func TestAssembleGroupsServiceFieldCombinedWithLiteral(t *testing.T) {
	cfgPath := writeStaticConfig(t, staticRoute{
		Host: "combo.example.com", Backends: []string{"http://10.0.0.9:8080"}, Service: "auth",
	})
	dc := fakeDocker(t, dockerJSON(
		container("a1", "auth-1", "running",
			map[string]string{labelHost: "auth.internal.example.com", labelPort: "9000", labelService: "auth"},
			map[string]string{managedNetwork: "172.20.0.11"}),
	))

	groups, err := assembleGroups(context.Background(), dc, cfgPath)
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	g := findGroup(groups, "combo.example.com", "")
	if g == nil || len(g.Backends) != 2 {
		t.Fatalf("Backends = %+v, want both the literal backend and the service-resolved one", g)
	}
	var sawLiteral, sawService bool
	for _, b := range g.Backends {
		if b.URL == "http://10.0.0.9:8080" {
			sawLiteral = true
		}
		if b.URL == "http://172.20.0.11:9000" {
			sawService = true
		}
	}
	if !sawLiteral || !sawService {
		t.Fatalf("Backends = %+v, missing literal or service-resolved backend", g.Backends)
	}
}

// TestAssembleGroupsServiceFieldZeroMatch proves a Service field that
// matches no container yields an empty (not nil-panicking) group — the
// existing 503-not-404 behavior already covers a group with zero backends.
func TestAssembleGroupsServiceFieldZeroMatch(t *testing.T) {
	cfgPath := writeStaticConfig(t, staticRoute{Host: "nomatch.example.com", Service: "nobody-labels-this"})
	dc := fakeDocker(t, dockerJSON())

	groups, err := assembleGroups(context.Background(), dc, cfgPath)
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	g := findGroup(groups, "nomatch.example.com", "")
	if g == nil {
		t.Fatal("group missing — a zero-match Service should still register the group")
	}
	if len(g.Backends) != 0 {
		t.Fatalf("Backends = %+v, want none", g.Backends)
	}
}

// TestAssembleGroupsServiceFieldExcludesCanary proves a canary-labeled
// container is excluded from Service-field resolution (routing sensitive
// paths to an in-progress canary is worse than the gap), while still
// appearing in its own default label-managed route (proving the exclusion is
// scoped to backendsByService, not global).
func TestAssembleGroupsServiceFieldExcludesCanary(t *testing.T) {
	cfgPath := writeStaticConfig(t, staticRoute{Host: "svc.example.com", Service: "auth"})
	dc := fakeDocker(t, dockerJSON(
		container("live", "auth-1", "running",
			map[string]string{labelHost: "auth.internal.example.com", labelPort: "9000", labelService: "auth"},
			map[string]string{managedNetwork: "172.20.0.11"}),
		container("canary", "auth-canary-1", "running",
			map[string]string{labelHost: "auth.internal.example.com", labelPort: "9000", labelService: "auth", labelCanary: "true"},
			map[string]string{managedNetwork: "172.20.0.12"}),
	))

	groups, err := assembleGroups(context.Background(), dc, cfgPath)
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	g := findGroup(groups, "svc.example.com", "")
	if g == nil || len(g.Backends) != 1 || g.Backends[0].URL != "http://172.20.0.11:9000" {
		t.Fatalf("service-resolved Backends = %+v, want only the live (non-canary) replica", g)
	}

	own := findGroup(groups, "auth.internal.example.com", "")
	if own == nil || len(own.Backends) != 2 {
		t.Fatalf("own label-managed group = %+v, want both live and canary present (canary exclusion is scoped to backendsByService)", own)
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

// TestPickHealthySkipsLearnedUnlessAllowed proves the allowPeer gate on
// pickHealthy: with allowPeer=false a learned backend must never be chosen
// (this is what stops a request that already hopped from a peer being
// forwarded to yet another peer), even when it's the only eligible backend
// in the group (i.e. the local tier is empty).
func TestPickHealthySkipsLearnedUnlessAllowed(t *testing.T) {
	learned := makePeerBackend("http://127.0.0.1:2", "h.example.org", "", false, "peer-b", 1)
	learned.markHealthy(true)
	g := &RouteGroup{Host: "h.example.org", Backends: []*Backend{learned}}

	for i := 0; i < 50; i++ {
		if b := g.pickHealthy(nil, false); b != nil {
			t.Fatal("pickHealthy(nil, false) returned a backend when the only eligible one is learned")
		}
	}
	if b := g.pickHealthy(nil, true); b == nil || !b.Learned {
		t.Fatal("pickHealthy(nil, true) should fall through to the learned tier when no local backend exists")
	}
}

// TestPickHealthyPrefersLocalOverLearned is the fix for the load-balance bug:
// docs/PEER_MESH_PLAN.md specifies a peer is used only "when its own
// backends for a route are unhealthy or absent" (failover), not load-balanced
// alongside a working local backend. With one healthy local and one healthy
// learned backend for the same route, pickHealthy(nil, true) must never
// return the learned one while the local stays healthy; once the local is
// marked unhealthy, it must fail over to the learned backend.
func TestPickHealthyPrefersLocalOverLearned(t *testing.T) {
	u, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	local := makeBackend("http://127.0.0.1:1", 1, "c", "", u, "h.example.org")
	local.markHealthy(true)
	learned := makePeerBackend("http://127.0.0.1:2", "h.example.org", "", false, "peer-b", 1)
	learned.markHealthy(true)
	g := &RouteGroup{Host: "h.example.org", Backends: []*Backend{local, learned}}

	for i := 0; i < 200; i++ {
		if b := g.pickHealthy(nil, true); b == nil || b.Learned {
			t.Fatal("pickHealthy(nil, true) returned the learned backend while a healthy local backend exists")
		}
	}

	local.markHealthy(false)
	for i := 0; i < 50; i++ {
		if b := g.pickHealthy(nil, true); b == nil || !b.Learned {
			t.Fatal("pickHealthy(nil, true) should fail over to the learned backend once the local one is unhealthy")
		}
	}
}

// TestPeerBackendRestoresStrippedPrefix proves the prefix-restoration fix:
// ServeHTTP strips group.PathPrefix from req.URL.Path before pickHealthy
// ever runs, so makePeerBackend's Director must restore it before forwarding
// to the peer — otherwise the receiving peer's own host+path match fails or
// hits the wrong group.
func TestPeerBackendRestoresStrippedPrefix(t *testing.T) {
	var seenPath, seenHost, seenHop string
	peerBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenHost = r.Host
		seenHop = r.Header.Get(PeerHopHeader)
		_, _ = w.Write([]byte("ok"))
	}))
	defer peerBackend.Close()

	b := makePeerBackend(peerBackend.URL, "mcp.example.org", "/mcp/foo", true, "peer-b", 1)
	g := &RouteGroup{Host: "mcp.example.org", PathPrefix: "/mcp/foo", StripPrefix: true, Backends: []*Backend{b}}

	r := &Router{}
	r.Set([]*RouteGroup{g})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://mcp.example.org/mcp/foo/bar", nil)
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)

	if rec.Code != 200 {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if seenPath != "/mcp/foo/bar" {
		t.Fatalf("peer backend saw path %q, want /mcp/foo/bar (stripped prefix must be restored before the peer hop)", seenPath)
	}
	if seenHost != "mcp.example.org" {
		t.Fatalf("peer backend saw Host %q, want mcp.example.org", seenHost)
	}
	if seenHop != "1" {
		t.Fatal("peer backend never saw PeerHopHeader set on the outbound hop")
	}
}

// TestServeHTTPHoppedRequestStillRateLimited proves PeerHopHeader never
// gates the rate-limit decision: it's an unauthenticated, client-settable
// header, so trusting its presence to skip the limiter would let any client
// bypass its own rate limit by setting the header on a direct request. A
// hopped request against an exhausted bucket must still get 429, same as a
// non-hopped one.
func TestServeHTTPHoppedRequestStillRateLimited(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	g := mkGroup(t, "rl.example.org", "", false, backend.URL)
	g.RateLimit, g.RateRPM = true, 1

	r := &Router{}
	r.Set([]*RouteGroup{g})

	// First request consumes the sole token (capacity 1).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://rl.example.org/", nil)
	req.RemoteAddr = "203.0.113.9:1000"
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)
	if rec.Code != 200 {
		t.Fatalf("first request code = %d, want 200", rec.Code)
	}

	// A second, non-hopped request against the same exhausted bucket is throttled.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "http://rl.example.org/", nil)
	req.RemoteAddr = "203.0.113.9:1000"
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)
	if rec.Code != 429 {
		t.Fatalf("non-hopped request code = %d, want 429 (limiter exhausted)", rec.Code)
	}

	// A hopped request against the same exhausted bucket must still be throttled.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "http://rl.example.org/", nil)
	req.RemoteAddr = "203.0.113.9:1000"
	req.Header.Set(PeerHopHeader, "1")
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)
	if rec.Code != 429 {
		t.Fatalf("hopped request code = %d, want 429 (PeerHopHeader must not bypass the limiter)", rec.Code)
	}
}

// TestPeerRouteStoreOverlayRateLimitReachesRouterSet proves the full
// overlay -> router.Set() pipeline: a synthesized learned-only group that
// carries RateLimit/RateRPM from a peer push actually gets a working
// limiter wired up by Set()'s existing reconciliation, exactly like a
// locally-discovered rate-limited group would. Without this, a route
// reached purely through a peer hop would never have its shared bucket
// charged by either side (see peersync.go's peerRouteInfo doc comment).
func TestPeerRouteStoreOverlayRateLimitReachesRouterSet(t *testing.T) {
	store := newPeerRouteStore(time.Minute)
	store.merge(peerRoutePayload{
		Peer: "b", Advertise: "http://127.0.0.1:1",
		Routes: []peerRouteInfo{{Host: "rl.example.org", Backends: 1, RateLimit: true, RateRPM: 1}},
	})

	r := &Router{}
	r.Set(store.overlay(nil))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://rl.example.org/", nil)
	req.RemoteAddr = "203.0.113.9:1000"
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)
	// First request against the learned backend fails (nothing listening at
	// 127.0.0.1:1, connection refused immediately — no DNS involved) but must
	// still have consumed the sole token.
	if rec.Code == 429 {
		t.Fatalf("first request unexpectedly rate-limited already: code = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "http://rl.example.org/", nil)
	req.RemoteAddr = "203.0.113.9:1000"
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, req)
	if rec.Code != 429 {
		t.Fatalf("second request code = %d, want 429 (limiter must be wired up on the overlay-synthesized group)", rec.Code)
	}
}

// TestPeerMeshLoopPreventionTwoInstances is the single most important test
// in the peer mesh: two independent Routers, each with, for the same host,
// ONLY a learned backend pointing at the other (mutual, no local backend on
// either side). A request to A must forward to B exactly once — B sees
// PeerHopHeader set, pickHealthy(_, false) finds no eligible backend since
// B's only backend for that route is also learned, and B returns 503 —
// rather than A and B bouncing the request back and forth forever.
func TestPeerMeshLoopPreventionTwoInstances(t *testing.T) {
	const host = "shared.example.org"

	rA := &Router{}
	rB := &Router{}

	var hitsB atomic.Int32
	var srvA, srvB *httptest.Server
	srvA = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rA.ServeHTTP(w, r)
	}))
	defer srvA.Close()
	srvB = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		rB.ServeHTTP(w, r)
	}))
	defer srvB.Close()

	bA := makePeerBackend(srvB.URL, host, "", false, "b", 1)
	rA.Set([]*RouteGroup{{Host: host, Backends: []*Backend{bA}}})

	bB := makePeerBackend(srvA.URL, host, "", false, "a", 1)
	rB.Set([]*RouteGroup{{Host: host, Backends: []*Backend{bB}}})

	req, err := http.NewRequest("GET", srvA.URL+"/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request to A: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (B has no eligible backend for an already-hopped request)", resp.StatusCode)
	}
	if got := hitsB.Load(); got != 1 {
		t.Fatalf("B was hit %d time(s), want exactly 1 (no forwarding loop)", got)
	}
}

// TestDockerHealthFloorIndependentOfProbe proves the Docker health-status
// floor (step A) works even when the TCP/HTTP probe hasn't run yet: a fresh
// Router.Set() defaults healthyFlag to true, but a container whose Status
// reports "(unhealthy)" must still be excluded from routing.
func TestDockerHealthFloorIndependentOfProbe(t *testing.T) {
	dc := fakeDocker(t, dockerJSON(
		containerWithStatus("u", "app-u", "running", "Up 2 minutes (unhealthy)",
			map[string]string{labelHost: "u.example.org", labelPort: "8080"},
			map[string]string{managedNetwork: "172.20.0.9"}),
	))

	groups, err := assembleGroups(context.Background(), dc, "")
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	g := findGroup(groups, "u.example.org", "")
	if g == nil || len(g.Backends) != 1 {
		t.Fatalf("u group = %+v", g)
	}
	b := g.Backends[0]
	if !b.DockerUnhealthy {
		t.Fatal("DockerUnhealthy should be true for a container reporting (unhealthy)")
	}

	r := &Router{}
	r.Set(groups)
	if !b.healthyFlag.Load() {
		t.Fatal("healthyFlag should default true on a fresh Set()")
	}
	if b.healthy() {
		t.Fatal("healthy() should be false — DockerUnhealthy floor must apply even though healthyFlag is true")
	}
}

// TestDockerHealthFloorRecoversAcrossRefresh is the regression test for step
// 6: prevHealth carry-forward must read the raw healthyFlag, not healthy(),
// or a floored backend's false gets laundered forward and the backend stays
// stuck unhealthy on the next refresh even after Docker reports it healthy
// again.
func TestDockerHealthFloorRecoversAcrossRefresh(t *testing.T) {
	dcUnhealthy := fakeDocker(t, dockerJSON(
		containerWithStatus("f", "app-f", "running", "Up 2 minutes (unhealthy)",
			map[string]string{labelHost: "f.example.org", labelPort: "8080"},
			map[string]string{managedNetwork: "172.20.0.9"}),
	))
	groups1, err := assembleGroups(context.Background(), dcUnhealthy, "")
	if err != nil {
		t.Fatalf("assembleGroups (unhealthy): %v", err)
	}
	r := &Router{}
	r.Set(groups1)
	g1 := findGroup(groups1, "f.example.org", "")
	if g1 == nil || len(g1.Backends) != 1 || g1.Backends[0].healthy() {
		t.Fatalf("first pass: backend should be unhealthy, got %+v", g1)
	}

	dcHealthy := fakeDocker(t, dockerJSON(
		containerWithStatus("f", "app-f", "running", "Up 5 minutes (healthy)",
			map[string]string{labelHost: "f.example.org", labelPort: "8080"},
			map[string]string{managedNetwork: "172.20.0.9"}),
	))
	groups2, err := assembleGroups(context.Background(), dcHealthy, "")
	if err != nil {
		t.Fatalf("assembleGroups (healthy): %v", err)
	}
	r.Set(groups2)
	g2 := findGroup(groups2, "f.example.org", "")
	if g2 == nil || len(g2.Backends) != 1 {
		t.Fatalf("second pass: group = %+v", g2)
	}
	if !g2.Backends[0].healthy() {
		t.Fatal("backend should be healthy immediately after Docker reports recovery, not stuck unhealthy from carry-forward")
	}
}

// TestMissingHealthLabelWarningDedup proves the missing-proxy.health warning
// (steps 8-9) only LOGS once per container name for the process lifetime,
// even across repeated assembleGroups() calls (which now happen far more
// often thanks to health_status event refreshes) — not just that the dedup
// map gets set (which would pass even if the log call were unconditional).
// healthLabelWarnSeen is package-level and persists across tests in this
// binary, so this test uses a container name no other test in the package
// reuses, and resets its own entry rather than the whole map. No test in
// this package logs from a goroutine or runs t.Parallel(), so redirecting
// the shared log.Writer() for the duration of this test is safe.
func TestMissingHealthLabelWarningDedup(t *testing.T) {
	const name = "app-warn-dedup-test-only"
	healthLabelWarnMu.Lock()
	delete(healthLabelWarnSeen, name)
	healthLabelWarnMu.Unlock()

	dc := fakeDocker(t, dockerJSON(
		container("w", name, "running",
			map[string]string{labelHost: "w.example.org", labelPort: "8080"},
			map[string]string{managedNetwork: "172.20.0.9"}),
	))

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	if _, err := assembleGroups(context.Background(), dc, ""); err != nil {
		t.Fatalf("assembleGroups (1st): %v", err)
	}
	if _, err := assembleGroups(context.Background(), dc, ""); err != nil {
		t.Fatalf("assembleGroups (2nd): %v", err)
	}
	if got := strings.Count(buf.String(), name); got != 1 {
		t.Fatalf("warning logged %d time(s) across two assembleGroups() calls, want exactly 1 (dedup)", got)
	}
}
