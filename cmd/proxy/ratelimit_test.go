package main

import (
	"net/http/httptest"
	"testing"
)

// TestRateLimitCap: with rpm modest enough that refill can't cross a token
// within the microsecond-scale loop, the first rpm calls succeed and the
// next is denied — deterministic, no sleeps.
func TestRateLimitCap(t *testing.T) {
	rl := newRateLimiter(60)
	for i := 0; i < 60; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("Allow #%d denied, want allowed (capacity 60)", i+1)
		}
	}
	if rl.Allow("1.2.3.4") {
		t.Fatal("Allow past capacity should be denied")
	}
}

// TestRateLimitSpoofedXFFUntrustedPeer: two requests from the same untrusted
// peer with different spoofed X-Forwarded-For headers must key to the same
// bucket — rlKey returns the untrusted peer IP, ignoring the header.
func TestRateLimitSpoofedXFFUntrustedPeer(t *testing.T) {
	trusted := parseCIDRList("127.0.0.0/8")

	req1 := httptest.NewRequest("GET", "http://a/", nil)
	req1.RemoteAddr = "203.0.113.9:1000"
	req1.Header.Set("X-Forwarded-For", "1.1.1.1")

	req2 := httptest.NewRequest("GET", "http://a/", nil)
	req2.RemoteAddr = "203.0.113.9:2000"
	req2.Header.Set("X-Forwarded-For", "2.2.2.2")

	k1 := rlKey(req1, trusted)
	k2 := rlKey(req2, trusted)
	if k1 != k2 {
		t.Fatalf("spoofed XFF from same peer gave different keys: %q vs %q", k1, k2)
	}
	if k1 != "203.0.113.9" {
		t.Fatalf("rlKey = %q, want 203.0.113.9 (untrusted peer IP)", k1)
	}
}

// TestRateLimitPerServiceIndependent: exhausting one limiter must not affect
// another.
func TestRateLimitPerServiceIndependent(t *testing.T) {
	a := newRateLimiter(10)
	b := newRateLimiter(10)
	for i := 0; i < 10; i++ {
		if !a.Allow("ip") {
			t.Fatalf("limiter a Allow #%d denied", i+1)
		}
	}
	if a.Allow("ip") {
		t.Fatal("limiter a should be exhausted")
	}
	if !b.Allow("ip") {
		t.Fatal("limiter b must be unaffected by a's exhaustion")
	}
}

// TestRateLimitSurvivesRefresh: a refresh (second Set with an equivalent
// group) must reuse the SAME limiter instance and preserve drained tokens —
// not hand every client a fresh full bucket. A group dropped from the second
// Set has its limiter removed from r.limiters.
func TestRateLimitSurvivesRefresh(t *testing.T) {
	r := &Router{}
	g1 := &RouteGroup{Host: "a.example.org", RateLimit: true, RateRPM: 10}
	g2 := &RouteGroup{Host: "b.example.org", RateLimit: true, RateRPM: 10}
	r.Set([]*RouteGroup{g1, g2})

	limiterBefore := g1.limiter
	if limiterBefore == nil {
		t.Fatal("group a should have a limiter after Set")
	}

	// Drain a.example.org's limiter to empty via the public API.
	for i := 0; i < 10; i++ {
		if !limiterBefore.Allow("9.9.9.9") {
			t.Fatalf("drain Allow #%d denied", i+1)
		}
	}
	if limiterBefore.Allow("9.9.9.9") {
		t.Fatal("limiter should be drained before refresh")
	}

	// Refresh: same host/path, RateLimit on; drop b.example.org entirely.
	g1b := &RouteGroup{Host: "a.example.org", RateLimit: true, RateRPM: 10}
	r.Set([]*RouteGroup{g1b})

	if g1b.limiter != limiterBefore {
		t.Fatal("refresh should reuse the same *rateLimiter instance")
	}
	// State preserved: a reset would flip this back to true.
	if g1b.limiter.Allow("9.9.9.9") {
		t.Fatal("drained token count should survive refresh (bucket not reset)")
	}

	// The dropped group's limiter must be evicted from the map.
	if _, ok := r.limiters["b.example.org|"]; ok {
		t.Fatal("dropped group's limiter should be removed from r.limiters")
	}
	if _, ok := r.limiters["a.example.org|"]; !ok {
		t.Fatal("live group's limiter should remain in r.limiters")
	}
}
