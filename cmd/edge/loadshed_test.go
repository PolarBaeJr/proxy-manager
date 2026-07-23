package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestMaxInFlightShedsExcess saturates the concurrency cap with blocked
// requests, then asserts every *further* request is shed while the cap is held.
// The requests are sent synchronously on the test goroutine so there is no race
// between arrivals and the release of the held slots — the previous version
// released the held requests first, freeing slots that late arrivals slipped
// through, so its "exactly cap OK, rest shed" assertion was never reliable.
func TestMaxInFlightShedsExcess(t *testing.T) {
	const cap = 4

	release := make(chan struct{})
	started := make(chan struct{}, cap)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	h := withMaxInFlight(slow, cap)

	// Occupy every slot with a request blocked inside the handler.
	var held sync.WaitGroup
	for i := 0; i < cap; i++ {
		held.Add(1)
		go func() {
			defer held.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		}()
	}
	for i := 0; i < cap; i++ {
		<-started // all cap slots now occupied and blocked
	}

	// With the cap saturated, every further request must be shed.
	const extra = 16
	for i := 0; i < extra; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("request %d: expected 503 while saturated, got %d", i, rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Errorf("request %d: shed response missing Retry-After", i)
		}
	}

	// Release the held requests; they should all drain successfully.
	close(release)
	held.Wait()
}

func TestMaxInFlightRecovers(t *testing.T) {
	h := withMaxInFlight(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), 1)

	// Sequential requests must all pass — slots are released.
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d", i, rec.Code)
		}
	}
}
