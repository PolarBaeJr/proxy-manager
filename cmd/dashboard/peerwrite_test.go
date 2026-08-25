package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestPeerMutateSingleAttemptOnTimeout proves peerMutate makes exactly ONE
// HTTP request even when the target hangs past the given timeout, and
// returns promptly once the timeout fires — never blocking for the
// handler's full duration and never retrying. Mutations aren't idempotent;
// a retry here would be unsafe (see the doc comment on peerMutate).
func TestPeerMutateSingleAttemptOnTimeout(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 2 * time.Second}
	start := time.Now()
	code, body, err := peerMutate(context.Background(), client, srv.URL, "s3cret", http.MethodPost, "/anything", 50*time.Millisecond, nil, nil, "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("want an error for a handler slower than the timeout, got nil (code=%d body=%s)", code, body)
	}
	if elapsed > time.Second {
		t.Errorf("elapsed = %s, want it bounded near the 50ms timeout, not the handler's 300ms sleep", elapsed)
	}

	// The handler increments attempts before it sleeps, so it's already
	// observable by the time peerMutate's 50ms deadline fires; a small
	// buffer just protects against scheduling jitter.
	time.Sleep(400 * time.Millisecond)
	if n := attempts.Load(); n != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no retry on timeout)", n)
	}
}

// TestPeerMutateDecodesOnSuccess proves a 2xx response is decoded into
// respOut while the raw status+body are still returned.
func TestPeerMutateDecodesOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 2 * time.Second}
	var out struct {
		Status string `json:"status"`
	}
	code, body, err := peerMutate(context.Background(), client, srv.URL, "s3cret", http.MethodPost, "/anything", time.Second, nil, &out, "")
	if err != nil {
		t.Fatalf("want success, got: %v", err)
	}
	if code != http.StatusOK {
		t.Errorf("code = %d, want 200", code)
	}
	if string(body) != `{"status":"ok"}` {
		t.Errorf("body = %q, want the raw response body", body)
	}
	if out.Status != "ok" {
		t.Errorf("out.Status = %q, want %q", out.Status, "ok")
	}
}

// TestPeerMutateNonSuccessDoesNotDecode proves a non-2xx response's raw
// status+body are still returned (for mapPeerMutationErr's benefit), without
// erroring on a respOut that doesn't match the error shape.
func TestPeerMutateNonSuccessDoesNotDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"conflict"}`, http.StatusConflict)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 2 * time.Second}
	var out struct {
		Status string `json:"status"`
	}
	code, body, err := peerMutate(context.Background(), client, srv.URL, "s3cret", http.MethodPost, "/anything", time.Second, nil, &out, "")
	if err != nil {
		t.Fatalf("want no transport-level error for a non-2xx response, got: %v", err)
	}
	if code != http.StatusConflict {
		t.Errorf("code = %d, want %d", code, http.StatusConflict)
	}
	if !strings.Contains(string(body), "conflict") {
		t.Errorf("body = %q, want the peer's raw body", body)
	}
}

// TestMapPeerMutationErr table-tests every branch of the relay rule.
func TestMapPeerMutationErr(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       []byte
		wantStatus int
		wantBody   string // only checked when non-empty
	}{
		{"400 relayed verbatim", http.StatusBadRequest, []byte(`{"error":"bad input"}`), http.StatusBadRequest, `{"error":"bad input"}`},
		{"404 relayed verbatim", http.StatusNotFound, []byte(`{"error":"not found"}`), http.StatusNotFound, `{"error":"not found"}`},
		{"409 relayed verbatim", http.StatusConflict, []byte(`{"error":"conflict"}`), http.StatusConflict, `{"error":"conflict"}`},
		{"401 becomes 502", http.StatusUnauthorized, []byte(`{"error":"unauthorized"}`), http.StatusBadGateway, ""},
		{"403 becomes 502", http.StatusForbidden, []byte(`{"error":"forbidden"}`), http.StatusBadGateway, ""},
		{"500 becomes 502", http.StatusInternalServerError, []byte(`{"error":"boom"}`), http.StatusBadGateway, ""},
		{"transport error becomes 502", 0, []byte("dial tcp: connection refused"), http.StatusBadGateway, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mapPeerMutationErr(rec, tc.statusCode, tc.body)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
				t.Errorf("body = %q, want %q (exact verbatim relay)", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestMapPeerMutationErrNeverRelaysAuthFailureVerbatim guards the specific
// reasoning documented on mapPeerMutationErr: a peer's own 401/403 body must
// never reach the caller's response, since a 401/403 status here would pop
// the dashboard's own "session expired" handling in the requesting browser.
func TestMapPeerMutationErrNeverRelaysAuthFailureVerbatim(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		rec := httptest.NewRecorder()
		mapPeerMutationErr(rec, code, []byte("peer's own auth error body"))
		if rec.Code == code {
			t.Fatalf("status = %d, a peer's %d must never be relayed verbatim", rec.Code, code)
		}
		if strings.Contains(rec.Body.String(), "peer's own auth error body") {
			t.Errorf("body = %q, must not contain the peer's own auth-failure body", rec.Body.String())
		}
	}
}

// TestHostForReq covers the shared ?host= resolution logic extracted from
// the /api/access and /api/images handlers.
func TestHostForReq(t *testing.T) {
	reg := newPeerRegistry([]string{"http://peer-b:8098"}, "s3cret", "dashboard-a", "dev", 0, nil)
	reg.recordResult("http://peer-b:8098", true, "dashboard-b", "dev", false)

	cases := []struct {
		name       string
		url        string
		registry   *PeerRegistry
		wantHost   string
		wantIsPeer bool
	}{
		{"no host param, with registry", "/x", reg, "", false},
		{"no host param, nil registry", "/x", nil, "", false},
		{"host equals self identity", "/x?host=dashboard-a", reg, "dashboard-a", false},
		{"host is a known peer identity", "/x?host=dashboard-b", reg, "dashboard-b", true},
		{"host is an unknown identity, with registry", "/x?host=dashboard-ghost", reg, "dashboard-ghost", true},
		{"host given, nil registry", "/x?host=anything", nil, "anything", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			host, isPeer := hostForReq(req, tc.registry)
			if host != tc.wantHost || isPeer != tc.wantIsPeer {
				t.Errorf("hostForReq = (%q, %v), want (%q, %v)", host, isPeer, tc.wantHost, tc.wantIsPeer)
			}
		})
	}
}

// TestPeerWritesFlagDefaultsFalse is a light smoke test — there's no
// behavior to gate on -peer-writes yet in this phase, so this just confirms
// it parses and defaults false, mirroring the declaration in main.go.
func TestPeerWritesFlagDefaultsFalse(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	pw := fs.Bool("peer-writes", false, "enable write-capable /peer/* handlers on top of the read-only peer mesh (requires DASHBOARD_PEER_SECRET too)")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *pw {
		t.Error("want -peer-writes to default to false")
	}

	fs2 := flag.NewFlagSet("test2", flag.ContinueOnError)
	pw2 := fs2.Bool("peer-writes", false, "enable write-capable /peer/* handlers on top of the read-only peer mesh (requires DASHBOARD_PEER_SECRET too)")
	if err := fs2.Parse([]string{"-peer-writes"}); err != nil {
		t.Fatal(err)
	}
	if !*pw2 {
		t.Error("want -peer-writes=true when passed")
	}

	if peerWritesEnabled {
		t.Error("want the package var peerWritesEnabled to default to false before main() sets it")
	}
}
