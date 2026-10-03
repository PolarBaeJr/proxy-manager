package selfcheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoopbackURL(t *testing.T) {
	cases := []struct{ addr, path, want string }{
		{":8093", "/healthz", "http://127.0.0.1:8093/healthz"},
		{"0.0.0.0:8093", "/healthz", "http://127.0.0.1:8093/healthz"},
		{"[::]:8093", "/healthz", "http://127.0.0.1:8093/healthz"},
		{"127.0.0.1:8094", "/healthz", "http://127.0.0.1:8094/healthz"},
		{"10.0.0.5:8092", "/x", "http://10.0.0.5:8092/x"},
		{"[::1]:8092", "/healthz", "http://[::1]:8092/healthz"},
		{"localhost:9000", "", "http://localhost:9000"},
	}
	for _, c := range cases {
		if got := LoopbackURL(c.addr, c.path); got != c.want {
			t.Errorf("LoopbackURL(%q, %q) = %q, want %q", c.addr, c.path, got, c.want)
		}
	}
}

func clearEnv(t *testing.T) {
	for _, k := range []string{"SELFCHECK", "SELFCHECK_INTERVAL", "SELFCHECK_TIMEOUT", "SELFCHECK_FAILURES", "SELFCHECK_GRACE", "SELFCHECK_URLS"} {
		t.Setenv(k, "")
	}
}

func TestFromEnvDefaults(t *testing.T) {
	clearEnv(t)
	c, on := FromEnv("svc", "http://127.0.0.1:1/healthz")
	if !on {
		t.Fatal("watchdog should default on")
	}
	if c.Name != "svc" || c.Interval != 15*time.Second || c.Timeout != 5*time.Second || c.Grace != 30*time.Second || c.Failures != 4 {
		t.Fatalf("defaults = %+v", c)
	}
	if len(c.URLs) != 1 || c.URLs[0] != "http://127.0.0.1:1/healthz" {
		t.Fatalf("URLs = %v", c.URLs)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("SELFCHECK_INTERVAL", "3s")
	t.Setenv("SELFCHECK_TIMEOUT", "2s")
	t.Setenv("SELFCHECK_FAILURES", "7")
	t.Setenv("SELFCHECK_GRACE", "0s")
	t.Setenv("SELFCHECK_URLS", " http://a/healthz , ,http://b/healthz")
	c, on := FromEnv("svc", "http://default/healthz")
	if !on {
		t.Fatal("expected on")
	}
	if c.Interval != 3*time.Second || c.Timeout != 2*time.Second || c.Failures != 7 || c.Grace != 0 {
		t.Fatalf("overrides = %+v", c)
	}
	if strings.Join(c.URLs, "|") != "http://a/healthz|http://b/healthz" {
		t.Fatalf("URLs = %v", c.URLs)
	}
}

func TestFromEnvInvalid(t *testing.T) {
	clearEnv(t)
	t.Setenv("SELFCHECK_INTERVAL", "soon")
	t.Setenv("SELFCHECK_TIMEOUT", "0s")
	t.Setenv("SELFCHECK_FAILURES", "many")
	t.Setenv("SELFCHECK_GRACE", "-5s")
	c, _ := FromEnv("svc")
	if c.Interval != defaultInterval || c.Timeout != defaultTimeout || c.Failures != defaultFailures || c.Grace != defaultGrace {
		t.Fatalf("invalid values should fall back to defaults, got %+v", c)
	}
}

func TestFromEnvClamps(t *testing.T) {
	clearEnv(t)
	t.Setenv("SELFCHECK_INTERVAL", "10ms")
	t.Setenv("SELFCHECK_FAILURES", "0")
	c, _ := FromEnv("svc")
	if c.Interval != time.Second {
		t.Fatalf("Interval = %s, want clamped to 1s", c.Interval)
	}
	if c.Failures != 1 {
		t.Fatalf("Failures = %d, want clamped to 1", c.Failures)
	}
	t.Setenv("SELFCHECK_FAILURES", "-3")
	if c, _ = FromEnv("svc"); c.Failures != 1 {
		t.Fatalf("Failures = %d, want clamped to 1", c.Failures)
	}
}

func TestFromEnvDisabledKeepsURLs(t *testing.T) {
	for _, v := range []string{"0", "false", "off", "OFF", "False"} {
		clearEnv(t)
		t.Setenv("SELFCHECK", v)
		t.Setenv("SELFCHECK_URLS", "http://override/healthz")
		c, on := FromEnv("svc", "http://default/healthz")
		if on {
			t.Errorf("SELFCHECK=%q should disable the watchdog", v)
		}
		if len(c.URLs) != 1 || c.URLs[0] != "http://override/healthz" {
			t.Errorf("SELFCHECK=%q: URLs = %v, want override kept for -healthcheck", v, c.URLs)
		}
	}
	clearEnv(t)
	t.Setenv("SELFCHECK", "1")
	if _, on := FromEnv("svc"); !on {
		t.Error("SELFCHECK=1 should keep the watchdog on")
	}
}

func statusServer(t *testing.T, code int) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbe(t *testing.T) {
	ctx := context.Background()
	if err := Probe(ctx, statusServer(t, 200).URL, time.Second); err != nil {
		t.Fatalf("200: %v", err)
	}
	if err := Probe(ctx, statusServer(t, 204).URL, time.Second); err != nil {
		t.Fatalf("204: %v", err)
	}
	if err := Probe(ctx, statusServer(t, 500).URL, time.Second); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("500: err = %v", err)
	}

	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-block }))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(block) })
	start := time.Now()
	if err := Probe(ctx, slow.URL, 50*time.Millisecond); err == nil {
		t.Fatal("timeout: expected error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not honoured")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + ln.Addr().String() + "/healthz"
	ln.Close()
	if err := Probe(ctx, refused, time.Second); err == nil {
		t.Fatal("refused: expected error")
	}
}

func TestProbeIgnoresHTTPProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	if err := Probe(context.Background(), statusServer(t, 200).URL, time.Second); err != nil {
		t.Fatalf("bogus HTTP_PROXY should be ignored: %v", err)
	}
}

func TestRunHealthcheck(t *testing.T) {
	var buf bytes.Buffer
	healthOut = &buf
	t.Cleanup(func() { healthOut = defaultHealthOut })
	ok := statusServer(t, 200).URL
	bad := statusServer(t, 503).URL
	if code := RunHealthcheck(ok, ok); code != 0 {
		t.Fatalf("all ok: code %d (%s)", code, buf.String())
	}
	if code := RunHealthcheck(ok, bad); code != 1 {
		t.Fatalf("one bad: code %d", code)
	}
	if !strings.Contains(buf.String(), "503") {
		t.Fatalf("reason not reported: %q", buf.String())
	}
	if code := RunHealthcheck(); code != 1 {
		t.Fatalf("no urls: code %d", code)
	}
}

// toggleServer is healthy while ok is true, and counts every probe.
type toggleServer struct {
	ok     atomic.Bool
	probes atomic.Int64
	srv    *httptest.Server
}

func newToggleServer(t *testing.T, ok bool) *toggleServer {
	ts := &toggleServer{}
	ts.ok.Store(ok)
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ts.probes.Add(1)
		if !ts.ok.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

// stubExit swaps exitFn for one that records the code and the probe count at
// the moment of exit. The caller must wait for run to return before the
// cleanup restores exitFn.
func stubExit(t *testing.T, ts *toggleServer) (codes chan int, probesAtExit *atomic.Int64) {
	codes = make(chan int, 4)
	probesAtExit = &atomic.Int64{}
	exitFn = func(code int) {
		if ts != nil {
			probesAtExit.Store(ts.probes.Load())
		}
		codes <- code
	}
	dumpOut = io.Discard
	t.Cleanup(func() { exitFn = defaultExitFn; dumpOut = defaultDumpOut })
	return codes, probesAtExit
}

var (
	defaultExitFn    = exitFn
	defaultDumpOut   = dumpOut
	defaultHealthOut = healthOut
)

func runAsync(ctx context.Context, c Config) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		run(ctx, c)
		close(done)
	}()
	return done
}

func cfg(urls ...string) Config {
	return Config{Name: "test", URLs: urls, Interval: 5 * time.Millisecond, Timeout: time.Second, Failures: 3}
}

func TestWatchdogHealthyNoExit(t *testing.T) {
	ts := newToggleServer(t, true)
	codes, _ := stubExit(t, ts)
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, cfg(ts.srv.URL))
	deadline := time.Now().Add(5 * time.Second)
	for ts.probes.Load() < 20 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if ts.probes.Load() < 20 {
		t.Fatalf("only %d probes ran", ts.probes.Load())
	}
	select {
	case c := <-codes:
		t.Fatalf("healthy server triggered exit(%d)", c)
	default:
	}
}

func TestWatchdogExitsAfterExactlyN(t *testing.T) {
	ts := newToggleServer(t, false)
	codes, probesAtExit := stubExit(t, ts)
	done := runAsync(context.Background(), cfg(ts.srv.URL))
	select {
	case c := <-codes:
		if c != 1 {
			t.Fatalf("exit code %d, want 1", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog never exited")
	}
	<-done
	if n := probesAtExit.Load(); n != 3 {
		t.Fatalf("exited after %d probes, want exactly 3", n)
	}
	if len(codes) != 0 {
		t.Fatal("exitFn called more than once")
	}
}

func TestWatchdogSuccessResetsCount(t *testing.T) {
	var calls atomic.Int64
	// Fail, fail, ok, repeating: never 3 in a row.
	check := func(context.Context) error {
		if calls.Add(1)%3 == 0 {
			return nil
		}
		return errors.New("down")
	}
	codes, _ := stubExit(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	c := cfg()
	c.Check = check
	done := runAsync(ctx, c)
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 30 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	select {
	case code := <-codes:
		t.Fatalf("exit(%d) despite successes resetting the count", code)
	default:
	}
}

func TestWatchdogGraceIgnored(t *testing.T) {
	ts := newToggleServer(t, false)
	codes, _ := stubExit(t, ts)
	ctx, cancel := context.WithCancel(context.Background())
	c := cfg(ts.srv.URL)
	c.Grace = 200 * time.Millisecond
	done := runAsync(ctx, c)
	time.Sleep(100 * time.Millisecond)
	if n := ts.probes.Load(); n != 0 {
		t.Fatalf("%d probes during grace, want 0", n)
	}
	select {
	case <-codes:
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog never exited after grace")
	}
	cancel()
	<-done
}

func TestWatchdogCtxCancelStops(t *testing.T) {
	ts := newToggleServer(t, false)
	codes, _ := stubExit(t, ts)
	c := cfg(ts.srv.URL)
	c.Grace = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, c)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel during grace did not stop the watchdog")
	}

	c.Grace = 0
	c.Failures = 1 << 30
	ctx, cancel = context.WithCancel(context.Background())
	done = runAsync(ctx, c)
	for ts.probes.Load() < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel while ticking did not stop the watchdog")
	}
	if len(codes) != 0 {
		t.Fatal("unexpected exit")
	}
}

func TestHeartbeat(t *testing.T) {
	h := NewHeartbeat()
	check := h.Stale(50 * time.Millisecond)
	if err := check(context.Background()); err != nil {
		t.Fatalf("fresh heartbeat stale: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := check(context.Background()); err == nil {
		t.Fatal("expected stale after maxAge")
	}
	h.Beat()
	if err := check(context.Background()); err != nil {
		t.Fatalf("after Beat: %v", err)
	}
}

func TestWatchdogHeartbeatExits(t *testing.T) {
	h := NewHeartbeat()
	codes, _ := stubExit(t, nil)
	c := cfg()
	c.Check = h.Stale(20 * time.Millisecond)
	done := runAsync(context.Background(), c)
	select {
	case <-codes:
	case <-time.After(5 * time.Second):
		t.Fatal("stale heartbeat never triggered exit")
	}
	<-done
}

func TestHandler(t *testing.T) {
	var nextCalls int
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusTeapot)
	})
	h := Handler(next)
	cases := []struct {
		method, path, remote string
		wantCode             int
		wantNext             bool
	}{
		{"GET", "/healthz", "127.0.0.1:5555", 200, false},
		{"HEAD", "/healthz", "127.0.0.1:5555", 200, false},
		{"GET", "/healthz", "[::1]:5555", 200, false},
		{"GET", "/healthz", "172.18.0.5:5555", http.StatusTeapot, true},
		{"POST", "/healthz", "127.0.0.1:5555", http.StatusTeapot, true},
		{"GET", "/other", "127.0.0.1:5555", http.StatusTeapot, true},
	}
	for _, c := range cases {
		nextCalls = 0
		req := httptest.NewRequest(c.method, c.path, nil)
		req.RemoteAddr = c.remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.wantCode || (nextCalls == 1) != c.wantNext {
			t.Errorf("%s %s from %s: code %d next=%d, want %d next=%v", c.method, c.path, c.remote, rec.Code, nextCalls, c.wantCode, c.wantNext)
		}
		if c.method == "GET" && !c.wantNext && rec.Body.String() != "ok" {
			t.Errorf("%s %s from %s: body %q", c.method, c.path, c.remote, rec.Body.String())
		}
	}
}
