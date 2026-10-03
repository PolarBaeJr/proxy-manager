// Package graceful runs a binary's HTTP servers until SIGTERM/SIGINT, then
// drains them instead of dying mid-request: stop accepting, let in-flight
// requests finish, cut long-lived streams after a short grace, force-close
// whatever is left at a hard deadline, and only then run the binary's
// persistence hooks and return.
//
// Order on a signal:
//
//  1. OnSignal callbacks (fast, e.g. stop a watchdog, take an early save).
//  2. Primary servers: Shutdown in parallel under Timeout. After StreamGrace
//     long-lived requests (WebSocket upgrades, SSE) get their context
//     cancelled and hijacked connections are closed — Shutdown never waits
//     for those on its own. At Timeout everything left is force-closed.
//  3. Aux servers (internal metrics/peer endpoints that should keep
//     answering while the main listener drains): Shutdown under AuxTimeout.
//  4. Hooks, in order, under one shared 5s context.
//
// A second signal during the drain kills the process the normal way.
package graceful

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	DefaultTimeout     = 25 * time.Second
	DefaultStreamGrace = 5 * time.Second
	DefaultAuxTimeout  = 3 * time.Second
	minTimeout         = time.Second
	maxTimeout         = 120 * time.Second
	hookTimeout        = 5 * time.Second
)

type Options struct {
	Primary     []*http.Server
	Aux         []*http.Server
	Timeout     time.Duration
	StreamGrace time.Duration
	AuxTimeout  time.Duration
	OnSignal    []func()
	Hooks       []func(context.Context)

	// notify replaces signal.NotifyContext in tests; onListen reports each
	// bound address (tests listen on :0).
	notify   func(context.Context) (context.Context, context.CancelFunc)
	onListen func(*http.Server, net.Addr)
}

// TimeoutFromEnv reads SHUTDOWN_TIMEOUT (default 25s, clamped to 1s–120s)
// and SHUTDOWN_STREAM_GRACE (default 5s, clamped to 0–timeout).
func TimeoutFromEnv() (timeout, streamGrace time.Duration) {
	timeout = envDuration("SHUTDOWN_TIMEOUT", DefaultTimeout)
	timeout = min(max(timeout, minTimeout), maxTimeout)
	streamGrace = envDuration("SHUTDOWN_STREAM_GRACE", DefaultStreamGrace)
	streamGrace = min(max(streamGrace, 0), timeout)
	return timeout, streamGrace
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("graceful: invalid %s=%q, using %s", key, v, def)
		return def
	}
	return d
}

// Run serves every Primary and Aux server, blocks until a signal, drains,
// runs the hooks and returns nil. A Primary listen/serve failure is returned
// immediately (callers log.Fatal it); an Aux failure is only logged — those
// listeners were always non-fatal.
func Run(o Options) error {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	o.StreamGrace = min(max(o.StreamGrace, 0), o.Timeout)
	if o.AuxTimeout <= 0 {
		o.AuxTimeout = DefaultAuxTimeout
	}
	notify := o.notify
	if notify == nil {
		notify = func(ctx context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
		}
	}
	sigCtx, stop := notify(context.Background())
	defer stop()

	tr := newTracker()
	for _, srv := range o.Primary {
		tr.track(srv)
	}

	var lns []net.Listener
	closeAll := func() {
		for _, ln := range lns {
			_ = ln.Close()
		}
	}
	listen := func(srv *http.Server) (net.Listener, error) {
		addr := srv.Addr
		if addr == "" {
			addr = ":http"
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		if o.onListen != nil {
			o.onListen(srv, ln.Addr())
		}
		return ln, nil
	}

	errCh := make(chan error, len(o.Primary))
	for _, srv := range o.Primary {
		ln, err := listen(srv)
		if err != nil {
			closeAll()
			return err
		}
		lns = append(lns, ln)
		go func(srv *http.Server, ln net.Listener) {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(srv, ln)
	}
	for _, srv := range o.Aux {
		ln, err := listen(srv)
		if err != nil {
			log.Printf("graceful: listen %s: %v", srv.Addr, err)
			continue
		}
		go func(srv *http.Server, ln net.Listener) {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				log.Printf("graceful: serve %s: %v", srv.Addr, err)
			}
		}(srv, ln)
	}

	select {
	case <-sigCtx.Done():
	case err := <-errCh:
		return err
	}
	// Restore default signal handling: a second SIGTERM/SIGINT now kills.
	stop()
	log.Printf("graceful: signal received — draining (timeout %s, stream grace %s)", o.Timeout, o.StreamGrace)

	for _, f := range o.OnSignal {
		f()
	}

	start := time.Now()
	drainCtx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	defer cancel()
	streamTimer := time.AfterFunc(o.StreamGrace, tr.cutStreams)

	var wg sync.WaitGroup
	for _, srv := range o.Primary {
		wg.Add(1)
		go func(srv *http.Server) {
			defer wg.Done()
			_ = srv.Shutdown(drainCtx)
		}(srv)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		tr.waitIdle(drainCtx)
		close(done)
	}()
	select {
	case <-done:
		log.Printf("graceful: drained in %s", time.Since(start).Round(time.Millisecond))
	case <-drainCtx.Done():
		log.Printf("graceful: drain timeout after %s — force-closing %d active request(s)", o.Timeout, tr.activeCount())
	}
	streamTimer.Stop()
	tr.cancelAll()
	tr.closeHijacked()
	for _, srv := range o.Primary {
		_ = srv.Close()
	}

	auxCtx, auxCancel := context.WithTimeout(context.Background(), o.AuxTimeout)
	var awg sync.WaitGroup
	for _, srv := range o.Aux {
		awg.Add(1)
		go func(srv *http.Server) {
			defer awg.Done()
			if err := srv.Shutdown(auxCtx); err != nil {
				_ = srv.Close()
			}
		}(srv)
	}
	awg.Wait()
	auxCancel()

	hookCtx, hookCancel := context.WithTimeout(context.Background(), hookTimeout)
	defer hookCancel()
	for _, h := range o.Hooks {
		h(hookCtx)
	}
	return nil
}

type connKey struct{}

// tracker counts in-flight requests across the Primary servers. Shutdown
// alone doesn't wait for hijacked connections, so a drain is only complete
// once Shutdown has returned AND no handler is still running.
type tracker struct {
	mu       sync.Mutex
	active   int
	changed  chan struct{}
	reqs     map[*trackedReq]struct{}
	hijacked map[net.Conn]struct{}
	cut      bool
}

type trackedReq struct {
	cancel context.CancelFunc
	long   bool
}

func newTracker() *tracker {
	return &tracker{
		changed:  make(chan struct{}, 1),
		reqs:     map[*trackedReq]struct{}{},
		hijacked: map[net.Conn]struct{}{},
	}
}

// track wraps srv's handler and chains its ConnContext/ConnState hooks.
func (t *tracker) track(srv *http.Server) {
	next := srv.Handler
	if next == nil {
		next = http.DefaultServeMux
	}
	prevCC := srv.ConnContext
	srv.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		if prevCC != nil {
			ctx = prevCC(ctx, c)
		}
		return context.WithValue(ctx, connKey{}, c)
	}
	prevCS := srv.ConnState
	srv.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateHijacked {
			t.mu.Lock()
			if t.cut {
				_ = c.Close()
			} else {
				t.hijacked[c] = struct{}{}
			}
			t.mu.Unlock()
		}
		if prevCS != nil {
			prevCS(c, s)
		}
	}
	srv.Handler = t.wrap(next)
}

func isLongLived(r *http.Request) bool {
	return r.Header.Get("Upgrade") != "" || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// wrap counts the request and gives it a cancelable context. A hijacked
// connection is forgotten when its handler returns: ReverseProxy (the only
// hijacker here) blocks for the whole upgraded session, so a handler that
// hijacks and returns early would not be force-closed at StreamGrace.
func (t *tracker) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		tr := &trackedReq{cancel: cancel, long: isLongLived(r)}
		t.mu.Lock()
		t.active++
		t.reqs[tr] = struct{}{}
		if tr.long && t.cut {
			cancel()
		}
		t.mu.Unlock()
		defer func() {
			cancel()
			t.mu.Lock()
			t.active--
			delete(t.reqs, tr)
			if c, ok := r.Context().Value(connKey{}).(net.Conn); ok {
				delete(t.hijacked, c)
			}
			t.mu.Unlock()
			select {
			case t.changed <- struct{}{}:
			default:
			}
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// cutStreams ends long-lived requests and hijacked connections at
// StreamGrace; anything that arrives later is cut on arrival.
func (t *tracker) cutStreams() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cut = true
	for tr := range t.reqs {
		if tr.long {
			tr.cancel()
		}
	}
	for c := range t.hijacked {
		_ = c.Close()
	}
}

func (t *tracker) cancelAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for tr := range t.reqs {
		tr.cancel()
	}
}

func (t *tracker) closeHijacked() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cut = true
	for c := range t.hijacked {
		_ = c.Close()
	}
}

func (t *tracker) activeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active
}

func (t *tracker) waitIdle(ctx context.Context) {
	for {
		if t.activeCount() == 0 {
			return
		}
		select {
		case <-t.changed:
		case <-ctx.Done():
			return
		}
	}
}
