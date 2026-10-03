// Package selfcheck makes a service restart itself when it is hung or
// wedged: "exit so Docker restarts us". Every service runs with
// `restart: unless-stopped`, so a process that exits non-zero comes back
// fresh within seconds — the cheapest possible recovery from a deadlock, a
// leak that has slowed the process to a crawl, or a listener that stopped
// accepting.
//
// Two independent mechanisms, both deliberately trivial:
//
//   - An in-process watchdog (Start) that probes the binary's own /healthz
//     (or a Heartbeat for binaries with no listener) and, after N
//     consecutive failures, dumps goroutines to stderr and exits 1. No
//     shutdown hooks run — the point is that the process may be wedged.
//   - A `-healthcheck` mode (RunHealthcheck) for Docker's HEALTHCHECK. Plain
//     Docker only marks a container unhealthy; it never restarts one. The
//     restart comes from the watchdog exiting.
//
// /healthz must stay trivial: it proves the HTTP server is accepting and
// serving, nothing more. Never make it depend on Docker, Redis or disk — a
// flaky dependency would then crash-loop a healthy process.
//
// Configuration is env-only, because Docker's HEALTHCHECK exec inherits the
// container's environment but not its compose `command:` flags.
package selfcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultInterval = 15 * time.Second
	defaultTimeout  = 5 * time.Second
	defaultFailures = 4
	defaultGrace    = 30 * time.Second
)

// Overridden in tests: os.Exit would kill the test binary, and a full
// goroutine dump per exit test is just noise.
var (
	exitFn              = os.Exit
	dumpOut   io.Writer = os.Stderr
	healthOut io.Writer = os.Stderr
)

// Config describes one watchdog. Check, when set, replaces probing URLs.
type Config struct {
	Name     string
	URLs     []string
	Check    func(ctx context.Context) error
	Interval time.Duration
	Timeout  time.Duration
	Grace    time.Duration
	Failures int
}

// LoopbackURL turns a listen address into a URL that reaches it from inside
// the same container. A wildcard or empty host (":8093", "0.0.0.0:8093",
// "[::]:8093") becomes 127.0.0.1; an explicit host is kept.
func LoopbackURL(addr, path string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + path
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + path
}

// Probe GETs url and returns nil on a 2xx. A fresh client per call, never
// proxied and never reusing a connection, so a wedged pool or an inherited
// HTTP_PROXY can't mask (or fake) the result.
func Probe(ctx context.Context, url string, timeout time.Duration) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return nil
}

// RunHealthcheck probes every url and returns a process exit code: 0 if all
// pass, 1 otherwise (with the reason on stderr). For `-healthcheck`.
func RunHealthcheck(urls ...string) int {
	if len(urls) == 0 {
		fmt.Fprintln(healthOut, "healthcheck: no URLs to probe")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	for _, u := range urls {
		if err := Probe(ctx, u, defaultTimeout); err != nil {
			fmt.Fprintf(healthOut, "healthcheck: %v\n", err)
			return 1
		}
	}
	return 0
}

// FromEnv builds a watchdog Config from SELFCHECK_* env vars. urls are the
// binary's defaults, replaced wholesale by SELFCHECK_URLS when set. The bool
// is false when SELFCHECK disables the watchdog — URLs are still filled in,
// because -healthcheck keeps working either way.
func FromEnv(name string, urls ...string) (Config, bool) {
	c := Config{
		Name:     name,
		URLs:     urls,
		Interval: envDuration("SELFCHECK_INTERVAL", defaultInterval, false),
		Timeout:  envDuration("SELFCHECK_TIMEOUT", defaultTimeout, false),
		Grace:    envDuration("SELFCHECK_GRACE", defaultGrace, true),
		Failures: defaultFailures,
	}
	if c.Interval < time.Second {
		c.Interval = time.Second
	}
	if v := strings.TrimSpace(os.Getenv("SELFCHECK_FAILURES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Printf("selfcheck: invalid SELFCHECK_FAILURES=%q, using %d", v, defaultFailures)
		} else {
			c.Failures = max(n, 1)
		}
	}
	if v := strings.TrimSpace(os.Getenv("SELFCHECK_URLS")); v != "" {
		var override []string
		for _, u := range strings.Split(v, ",") {
			if u = strings.TrimSpace(u); u != "" {
				override = append(override, u)
			}
		}
		if len(override) > 0 {
			c.URLs = override
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SELFCHECK"))) {
	case "0", "false", "off", "no":
		return c, false
	}
	return c, true
}

func envDuration(key string, def time.Duration, allowZero bool) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 || (d == 0 && !allowZero) {
		log.Printf("selfcheck: invalid %s=%q, using %s", key, v, def)
		return def
	}
	return d
}

// Start runs the watchdog in the background until ctx is cancelled or it
// exits the process. c is used as given; FromEnv is where clamping happens.
func Start(ctx context.Context, c Config) {
	log.Printf("selfcheck[%s]: watchdog probing %s every %s (grace %s, exit after %d consecutive failures)",
		c.Name, c.target(), c.Interval, c.Grace, c.Failures)
	go run(ctx, c)
}

func (c Config) target() string {
	if c.Check != nil {
		return "self-check"
	}
	return strings.Join(c.URLs, ",")
}

func (c Config) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	if c.Check != nil {
		return c.Check(ctx)
	}
	if len(c.URLs) == 0 {
		return errors.New("no URLs configured")
	}
	for _, u := range c.URLs {
		if err := Probe(ctx, u, c.Timeout); err != nil {
			return err
		}
	}
	return nil
}

func run(ctx context.Context, c Config) {
	if c.Grace > 0 {
		t := time.NewTimer(c.Grace)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
	tick := time.NewTicker(c.Interval)
	defer tick.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		err := c.check(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if fails > 0 {
				log.Printf("selfcheck[%s]: probe recovered after %d failure(s)", c.Name, fails)
			}
			fails = 0
			continue
		}
		fails++
		if fails < c.Failures {
			log.Printf("selfcheck[%s]: probe %d/%d of %s failed: %v", c.Name, fails, c.Failures, c.target(), err)
			continue
		}
		log.Printf("selfcheck[%s]: %d consecutive failed probes of %s (last: %v) — exiting so Docker restarts the container",
			c.Name, fails, c.target(), err)
		_ = pprof.Lookup("goroutine").WriteTo(dumpOut, 1)
		exitFn(1)
		return
	}
}

// Handler answers GET/HEAD /healthz from a loopback client with 200 "ok"
// before next ever sees the request, so self-probes skip rate limiting,
// metrics and access logging. Everything else goes to next unchanged.
func Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && isLoopback(r.RemoteAddr) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte("ok"))
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Heartbeat is a liveness signal for binaries with no listener: the main
// loop calls Beat, and the watchdog checks Stale.
type Heartbeat struct {
	last atomic.Int64
}

// NewHeartbeat returns a Heartbeat that has just beaten, so a slow startup
// isn't mistaken for a hang.
func NewHeartbeat() *Heartbeat {
	h := &Heartbeat{}
	h.Beat()
	return h
}

func (h *Heartbeat) Beat() { h.last.Store(time.Now().UnixNano()) }

// Stale returns a Check that fails once the last Beat is older than maxAge.
func (h *Heartbeat) Stale(maxAge time.Duration) func(context.Context) error {
	return func(context.Context) error {
		age := time.Since(time.Unix(0, h.last.Load()))
		if age > maxAge {
			return fmt.Errorf("no heartbeat for %s (max %s)", age.Round(time.Second), maxAge)
		}
		return nil
	}
}
