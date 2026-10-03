package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMainHandlerLoopbackHealthzSkipsMetricsAndAccessLog(t *testing.T) {
	for _, remote := range []string{"127.0.0.1:40000", "[::1]:40000"} {
		metrics, access := NewMetrics(), NewAccessLog()
		h := mainHandler(&Router{unroutedLimiter: newRateLimiter(defaultRateRPM)}, metrics, access)
		req := httptest.NewRequest("GET", "http://127.0.0.1:8092/healthz", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("%s: got %d %q, want 200 ok", remote, rec.Code, rec.Body.String())
		}
		if n := metrics.Total.Load(); n != 0 {
			t.Fatalf("%s: metrics recorded %d request(s), want 0", remote, n)
		}
		if n := len(access.Snapshot(0, 0)); n != 0 {
			t.Fatalf("%s: access log has %d entr(ies), want 0", remote, n)
		}
	}
}

// Control: the same request from a non-loopback peer falls through to the
// router and is recorded as unrouted — proving the test above discriminates.
func TestMainHandlerNonLoopbackHealthzIsUnrouted(t *testing.T) {
	metrics, access := NewMetrics(), NewAccessLog()
	h := mainHandler(&Router{unroutedLimiter: newRateLimiter(defaultRateRPM)}, metrics, access)
	req := httptest.NewRequest("GET", "http://127.0.0.1:8092/healthz", nil)
	req.RemoteAddr = "172.18.0.5:40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 from the router", rec.Code)
	}
	if n := metrics.Snapshot()["by_host"].(map[string]uint64)[unroutedHost]; n != 1 {
		t.Fatalf("(unrouted) count = %d, want 1", n)
	}
	if n := len(access.Snapshot(0, 0)); n != 1 {
		t.Fatalf("access log has %d entr(ies), want 1", n)
	}
}
