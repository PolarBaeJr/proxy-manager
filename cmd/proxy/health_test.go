package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDispatchHealthChecksCapsConcurrency proves that dispatchHealthChecks
// never has more than healthCheckConcurrency dials in flight at once, even
// when a single tick fans out to far more backends than that.
func TestDispatchHealthChecksCapsConcurrency(t *testing.T) {
	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const n = healthCheckConcurrency * 4
	backends := make([]*Backend, n)
	for i := range backends {
		backends[i] = &Backend{URL: srv.URL, HealthPath: "/", Weight: 1}
	}

	sem := make(chan struct{}, healthCheckConcurrency)
	var wg sync.WaitGroup
	dispatchHealthChecks(context.Background(), []*RouteGroup{{Host: "h.example.org", Backends: backends}}, sem, &wg)
	wg.Wait()

	if peak.Load() > healthCheckConcurrency {
		t.Fatalf("peak concurrent health checks = %d, want <= %d", peak.Load(), healthCheckConcurrency)
	}
	if peak.Load() < 2 {
		t.Fatalf("peak concurrent health checks = %d, want > 1 (checks should still run in parallel, just capped)", peak.Load())
	}
}
