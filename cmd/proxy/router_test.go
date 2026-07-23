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
	"path/filepath"
	"strconv"
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

func TestAssembleRateLimitLabel(t *testing.T) {
	dc := fakeDocker(t, dockerJSON(
		// Valid label with explicit burst.
		container("a", "app-a", "running",
			map[string]string{labelHost: "a.example.org", labelPort: "8080", labelRateLimit: "100:250"},
			map[string]string{managedNetwork: "172.20.0.5"}),
		// Invalid label → ignored (unlimited), route still assembled.
		container("b", "app-b", "running",
			map[string]string{labelHost: "b.example.org", labelPort: "8080", labelRateLimit: "abc"},
			map[string]string{managedNetwork: "172.20.0.6"}),
		// First replica with a valid value wins.
		container("c1", "app-c1", "running",
			map[string]string{labelHost: "c.example.org", labelPort: "8080", labelRateLimit: "10"},
			map[string]string{managedNetwork: "172.20.0.7"}),
		container("c2", "app-c2", "running",
			map[string]string{labelHost: "c.example.org", labelPort: "8080", labelRateLimit: "20"},
			map[string]string{managedNetwork: "172.20.0.8"}),
	))

	dir := t.TempDir()
	cfg := filepath.Join(dir, "routes.json")
	if err := os.WriteFile(cfg, []byte(`{"routes":[{"host":"s.example.org","backends":["http://10.0.0.1:80"],"ratelimit":"5:9"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	groups, err := assembleGroups(context.Background(), dc, cfg)
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}

	if g := findGroup(groups, "a.example.org", ""); g == nil || g.RateLimit == nil || g.RateLimit.RPS != 100 || g.RateLimit.Burst != 250 {
		t.Fatalf("a.example.org RateLimit = %+v, want 100:250", g)
	}
	if g := findGroup(groups, "b.example.org", ""); g == nil || g.RateLimit != nil {
		t.Fatalf("b.example.org should have nil RateLimit (bad label ignored), got %+v", g)
	}
	if g := findGroup(groups, "c.example.org", ""); g == nil || g.RateLimit == nil || g.RateLimit.RPS != 10 {
		t.Fatalf("c.example.org RateLimit should be first replica's 10, got %+v", g)
	}
	if g := findGroup(groups, "s.example.org", ""); g == nil || g.RateLimit == nil || g.RateLimit.RPS != 5 || g.RateLimit.Burst != 9 {
		t.Fatalf("static route RateLimit = %+v, want 5:9", g)
	}
}

func TestServeHTTPPerRouteRateLimit(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	limited := mkGroup(t, "a.example.org", "", false, backend.URL)
	limited.RateLimit = &rateSpec{RPS: 1, Burst: 2}
	other := mkGroup(t, "b.example.org", "", false, backend.URL)

	r := &Router{}
	r.Set([]*RouteGroup{limited, other})

	// Burst admits, burst+1 rejects.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		r.ServeHTTP(&accessWriter{ResponseWriter: rec}, httptest.NewRequest("GET", "http://a.example.org/", nil))
		if rec.Code != 200 {
			t.Fatalf("burst request %d code = %d, want 200", i+1, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	aw := &accessWriter{ResponseWriter: rec}
	r.ServeHTTP(aw, httptest.NewRequest("GET", "http://a.example.org/", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("burst+1 code = %d, want 429", rec.Code)
	}
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 {
		t.Fatalf("Retry-After = %q, want integer >= 1", rec.Header().Get("Retry-After"))
	}
	if !aw.ratelimited {
		t.Fatal("MarkRateLimited should have fired")
	}
	if aw.unrouted {
		t.Fatal("known host must not be marked unrouted on a per-route 429")
	}

	// The other host has no limiter and is unaffected.
	rec = httptest.NewRecorder()
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, httptest.NewRequest("GET", "http://b.example.org/", nil))
	if rec.Code != 200 {
		t.Fatalf("unlimited host code = %d, want 200", rec.Code)
	}
}

func TestRateLimitSurvivesSet(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	mk := func(spec rateSpec) *RouteGroup {
		g := mkGroup(t, "a.example.org", "", false, backend.URL)
		g.RateLimit = &spec
		return g
	}

	r := &Router{}
	r.Set([]*RouteGroup{mk(rateSpec{RPS: 1, Burst: 1})})

	// Drain the single token.
	rec := httptest.NewRecorder()
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, httptest.NewRequest("GET", "http://a.example.org/", nil))
	if rec.Code != 200 {
		t.Fatalf("first request code = %d, want 200", rec.Code)
	}

	// Rebuild with an identical config — the drained bucket must survive.
	r.Set([]*RouteGroup{mk(rateSpec{RPS: 1, Burst: 1})})
	rec = httptest.NewRecorder()
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, httptest.NewRequest("GET", "http://a.example.org/", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("post-rebuild code = %d, want 429 (rebuild must not refill)", rec.Code)
	}

	// Rebuild with a changed spec — params update, still no free refill.
	r.Set([]*RouteGroup{mk(rateSpec{RPS: 1, Burst: 5})})
	g := r.Snapshot()[0]
	if g.limiter == nil || g.limiter.burst != 5 {
		t.Fatalf("limiter burst = %+v, want updated to 5", g.limiter)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, httptest.NewRequest("GET", "http://a.example.org/", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("post-spec-change code = %d, want 429 (no free refill)", rec.Code)
	}
}

func TestServeHTTPGlobalRateLimit(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	r := &Router{}
	r.Set([]*RouteGroup{mkGroup(t, "a.example.org", "", false, backend.URL)})
	r.global = newTokenBucket(rateSpec{RPS: 1, Burst: 1}, nil)
	r.global.allow() // drain the ceiling

	// Unknown host: 429 AND collapsed into the unrouted bucket.
	rec := httptest.NewRecorder()
	aw := &accessWriter{ResponseWriter: rec}
	r.ServeHTTP(aw, httptest.NewRequest("GET", "http://unknown.example.org/", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("unknown host code = %d, want 429", rec.Code)
	}
	if !aw.ratelimited || !aw.unrouted {
		t.Fatalf("unknown host: ratelimited=%v unrouted=%v, want both true", aw.ratelimited, aw.unrouted)
	}

	// Known host: 429 without the unrouted collapse.
	rec = httptest.NewRecorder()
	aw = &accessWriter{ResponseWriter: rec}
	r.ServeHTTP(aw, httptest.NewRequest("GET", "http://a.example.org/", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("known host code = %d, want 429", rec.Code)
	}
	if !aw.ratelimited || aw.unrouted {
		t.Fatalf("known host: ratelimited=%v unrouted=%v, want true/false", aw.ratelimited, aw.unrouted)
	}
}

// TestRateLimitedUpgradeAdmission proves admission-only semantics for protocol
// upgrades: a WebSocket costs one token at connect, an over-limit upgrade is
// rejected 429 BEFORE any hijack, and the rejection never trips the reverse
// proxy's ErrorHandler into marking the backend unhealthy. Reuses the real-TCP
// scaffolding from TestServeHTTPWebSocketUpgrade.
func TestRateLimitedUpgradeAdmission(t *testing.T) {
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
	}))
	defer backend.Close()

	r := &Router{}
	group := mkGroup(t, "ws.example.org", "", false, backend.URL)
	group.RateLimit = &rateSpec{RPS: 1, Burst: 1}
	r.Set([]*RouteGroup{group})

	handler := withMetrics(withAccessLog(r, NewAccessLog()), NewMetrics())
	front := httptest.NewServer(handler)
	defer front.Close()

	upgrade := func() string {
		conn, err := net.Dial("tcp", front.Listener.Addr().String())
		if err != nil {
			t.Fatalf("dial front: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: ws.example.org\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		status, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			t.Fatalf("read status line: %v", err)
		}
		return strings.TrimSpace(status)
	}

	if s := upgrade(); !strings.Contains(s, "101") {
		t.Fatalf("first upgrade status = %q, want 101", s)
	}
	if s := upgrade(); !strings.Contains(s, "429") {
		t.Fatalf("second upgrade status = %q, want 429 (admission rejected pre-hijack)", s)
	}
	if !group.Backends[0].healthy() {
		t.Fatal("rate-limited upgrade must not mark the backend unhealthy")
	}
}

// TestRateLimitBeforeAuth pins the middleware ordering: the per-route cap runs
// BEFORE the auth block. With AuthRequired set and no auth gate configured the
// fallback is a fail-closed 503 — so a drained bucket answering 429 proves the
// limiter fired first, without auth machinery ever being invoked.
func TestRateLimitBeforeAuth(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	g := mkGroup(t, "a.example.org", "", false, backend.URL)
	g.AuthRequired = true
	g.RateLimit = &rateSpec{RPS: 1, Burst: 1}
	r := &Router{}
	r.Set([]*RouteGroup{g})

	// Token available → limiter admits, auth (nil gate) fails closed.
	rec := httptest.NewRecorder()
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, httptest.NewRequest("GET", "http://a.example.org/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first request code = %d, want 503 (auth fail-closed)", rec.Code)
	}

	// Drained → 429 wins over the auth 503, proving the ordering.
	rec = httptest.NewRecorder()
	r.ServeHTTP(&accessWriter{ResponseWriter: rec}, httptest.NewRequest("GET", "http://a.example.org/", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request code = %d, want 429 before auth's 503", rec.Code)
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
