package graceful

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type harness struct {
	t      *testing.T
	fire   context.CancelFunc
	addrs  sync.Map // *http.Server -> string
	ready  chan struct{}
	runErr chan error
}

// start runs o with an injected signal; it returns once every server listens.
func start(t *testing.T, o Options) *harness {
	t.Helper()
	sig, fire := context.WithCancel(context.Background())
	h := &harness{t: t, fire: fire, runErr: make(chan error, 1), ready: make(chan struct{})}
	n := len(o.Primary) + len(o.Aux)
	var seen int
	var mu sync.Mutex
	o.notify = func(context.Context) (context.Context, context.CancelFunc) { return sig, func() {} }
	o.onListen = func(s *http.Server, a net.Addr) {
		h.addrs.Store(s, a.String())
		mu.Lock()
		seen++
		if seen == n {
			close(h.ready)
		}
		mu.Unlock()
	}
	for _, s := range append(append([]*http.Server{}, o.Primary...), o.Aux...) {
		s.Addr = "127.0.0.1:0"
	}
	go func() { h.runErr <- Run(o) }()
	select {
	case <-h.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("servers never listened")
	}
	return h
}

func (h *harness) addr(s *http.Server) string {
	v, _ := h.addrs.Load(s)
	return v.(string)
}

func (h *harness) wait(within time.Duration) time.Duration {
	h.t.Helper()
	start := time.Now()
	select {
	case err := <-h.runErr:
		if err != nil {
			h.t.Fatalf("Run: %v", err)
		}
	case <-time.After(within):
		h.t.Fatalf("Run did not return within %s", within)
	}
	return time.Since(start)
}

func get(addr, path string, hdr http.Header) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	return (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
}

func TestInFlightFinishesAndNewConnRefused(t *testing.T) {
	entered := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		time.Sleep(400 * time.Millisecond)
		fmt.Fprint(w, "done")
	})}
	h := start(t, Options{Primary: []*http.Server{srv}, Timeout: 5 * time.Second, StreamGrace: time.Second})
	addr := h.addr(srv)

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := get(addr, "/", nil)
		if err != nil {
			res <- result{err: err}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		res <- result{body: string(b)}
	}()
	<-entered
	h.fire()
	time.Sleep(100 * time.Millisecond)
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("new connection accepted during drain")
	}
	r := <-res
	if r.err != nil || r.body != "done" {
		t.Fatalf("in-flight request: body=%q err=%v", r.body, r.err)
	}
	h.wait(2 * time.Second)
}

func TestHijackedHeldUntilStreamGrace(t *testing.T) {
	hijacked := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: test\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		close(hijacked)
		_, _ = io.Copy(io.Discard, c) // blocks until the conn is closed
	})}
	grace := 300 * time.Millisecond
	h := start(t, Options{Primary: []*http.Server{srv}, Timeout: 5 * time.Second, StreamGrace: grace})

	c, err := net.Dial("tcp", h.addr(srv))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: test\r\nConnection: Upgrade\r\n\r\n")
	br := bufio.NewReader(c)
	if _, err := http.ReadResponse(br, nil); err != nil {
		t.Fatal(err)
	}
	<-hijacked
	fired := time.Now()
	h.fire()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = br.ReadByte()
	held := time.Since(fired)
	if err == nil {
		t.Fatal("expected the hijacked conn to be closed")
	}
	if held < grace-50*time.Millisecond || held > grace+700*time.Millisecond {
		t.Fatalf("hijacked conn closed after %s, want ~%s", held, grace)
	}
	h.wait(2 * time.Second)
}

func TestSSECancelledAtStreamGrace(t *testing.T) {
	entered := make(chan struct{})
	ended := make(chan time.Time, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		ended <- time.Now()
	})}
	grace := 300 * time.Millisecond
	h := start(t, Options{Primary: []*http.Server{srv}, Timeout: 5 * time.Second, StreamGrace: grace})
	go func() {
		resp, err := get(h.addr(srv), "/", http.Header{"Accept": {"text/event-stream"}})
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-entered
	fired := time.Now()
	h.fire()
	at := <-ended
	if d := at.Sub(fired); d < grace-50*time.Millisecond || d > grace+700*time.Millisecond {
		t.Fatalf("SSE cancelled after %s, want ~%s", d, grace)
	}
	h.wait(2 * time.Second)
}

func TestTimeoutForceClosesAndReturns(t *testing.T) {
	entered := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	})}
	timeout := 400 * time.Millisecond
	h := start(t, Options{Primary: []*http.Server{srv}, Timeout: timeout, StreamGrace: 100 * time.Millisecond})
	go func() {
		if resp, err := get(h.addr(srv), "/", nil); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	h.fire()
	if d := h.wait(3 * time.Second); d < timeout-50*time.Millisecond {
		t.Fatalf("Run returned after %s, before the %s timeout", d, timeout)
	}
}

func TestOrderSignalDrainAuxHooks(t *testing.T) {
	var mu sync.Mutex
	var events []string
	add := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	entered := make(chan struct{})
	primary := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		time.Sleep(300 * time.Millisecond)
		add("drained")
	})}
	aux := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "aux") })}
	var h *harness
	h = start(t, Options{
		Primary: []*http.Server{primary}, Aux: []*http.Server{aux},
		Timeout: 5 * time.Second, StreamGrace: time.Second,
		OnSignal: []func(){func() { add("signal") }},
		Hooks: []func(context.Context){
			func(context.Context) {
				if c, err := net.DialTimeout("tcp", h.addr(aux), 200*time.Millisecond); err == nil {
					c.Close()
					add("aux-still-up")
				} else {
					add("hook1")
				}
			},
			func(ctx context.Context) {
				if _, ok := ctx.Deadline(); ok {
					add("hook2")
				}
			},
		},
	})
	go func() {
		if resp, err := get(h.addr(primary), "/", nil); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	h.fire()
	time.Sleep(100 * time.Millisecond)
	resp, err := get(h.addr(aux), "/", nil)
	if err != nil {
		t.Fatalf("aux should serve during the primary drain: %v", err)
	}
	resp.Body.Close()
	h.wait(3 * time.Second)
	want := []string{"signal", "drained", "hook1", "hook2"}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestPrimaryListenErrorReturned(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	o := Options{Primary: []*http.Server{{Addr: ln.Addr().String()}}}
	o.notify = func(ctx context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }
	if err := Run(o); err == nil {
		t.Fatal("expected a listen error")
	}
}

func TestTimeoutFromEnv(t *testing.T) {
	cases := []struct {
		timeout, grace string
		wantT, wantG   time.Duration
	}{
		{"", "", 25 * time.Second, 5 * time.Second},
		{"10s", "2s", 10 * time.Second, 2 * time.Second},
		{"100ms", "", time.Second, time.Second},
		{"10m", "", 120 * time.Second, 5 * time.Second},
		{"bogus", "-1s", 25 * time.Second, 0},
		{"3s", "9s", 3 * time.Second, 3 * time.Second},
	}
	for _, c := range cases {
		t.Setenv("SHUTDOWN_TIMEOUT", c.timeout)
		t.Setenv("SHUTDOWN_STREAM_GRACE", c.grace)
		gotT, gotG := TimeoutFromEnv()
		if gotT != c.wantT || gotG != c.wantG {
			t.Errorf("(%q,%q) = %s,%s want %s,%s", c.timeout, c.grace, gotT, gotG, c.wantT, c.wantG)
		}
	}
}
