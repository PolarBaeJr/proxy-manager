package main

import (
	"net/http"
	"sync/atomic"
)

// Global concurrency cap — aggregate load-shedding.
//
// The per-IP token bucket (ratelimit.go) bounds any single client, but many
// clients each under their own cap can still sum to more simultaneous work
// than the host can take. This middleware bounds total in-flight requests
// across ALL clients; excess requests are shed immediately with 503 +
// Retry-After instead of queueing until everything times out.
//
// A concurrency cap (rather than a global req/s rate) self-adapts to slow
// endpoints: when upstream latency grows, the same cap admits fewer new
// requests per second, which is exactly the back-pressure a small host needs.
func withMaxInFlight(next http.Handler, max int) http.Handler {
	var inFlight atomic.Int64
	limit := int64(max)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		if n > limit {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "server busy", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}
