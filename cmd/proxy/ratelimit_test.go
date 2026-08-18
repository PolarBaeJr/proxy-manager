package main

import (
	"fmt"
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

// TestRateLimitBoundedBucketTable is the regression test for the memory-
// exhaustion finding: an attacker rotating through distinct keys (e.g. an
// IPv6 /64 range's worth of full addresses, or simply many source IPs) must
// not be able to grow the bucket map without limit. Once at maxBuckets,
// unrecognized keys are denied rather than minting a new bucket, and an
// already-tracked key is unaffected.
func TestRateLimitBoundedBucketTable(t *testing.T) {
	rl := newRateLimiter(60)
	for i := 0; i < maxBuckets; i++ {
		if !rl.Allow(fmt.Sprintf("ip-%d", i)) {
			t.Fatalf("Allow for bucket #%d denied, want allowed (table not yet full)", i)
		}
	}
	if len(rl.buckets) != maxBuckets {
		t.Fatalf("len(buckets) = %d, want %d", len(rl.buckets), maxBuckets)
	}
	if rl.Allow("one-too-many") {
		t.Fatal("Allow for a new key past maxBuckets should be denied, not grow the table")
	}
	if len(rl.buckets) != maxBuckets {
		t.Fatalf("len(buckets) after overflow attempt = %d, want unchanged %d", len(rl.buckets), maxBuckets)
	}
	// An already-tracked key must still work — the full table only blocks
	// NEW keys, it doesn't evict or freeze existing ones.
	if !rl.Allow("ip-0") {
		t.Fatal("an existing key should still be served once the table is full")
	}
}

// TestRLKeyIPv6CollapsesToSlash64: two different IPv6 addresses within the
// same /64 must key to the same rate-limit bucket, otherwise a client can
// mint an unbounded number of distinct buckets just by rotating the host
// portion of its own /64 allocation.
func TestRLKeyIPv6CollapsesToSlash64(t *testing.T) {
	req1 := httptest.NewRequest("GET", "http://a/", nil)
	req1.RemoteAddr = "[2001:db8:1234:5678::1]:1000"
	req2 := httptest.NewRequest("GET", "http://a/", nil)
	req2.RemoteAddr = "[2001:db8:1234:5678:ffff:ffff:ffff:ffff]:2000"

	k1 := rlKey(req1, nil)
	k2 := rlKey(req2, nil)
	if k1 != k2 {
		t.Fatalf("two addresses in the same /64 gave different keys: %q vs %q", k1, k2)
	}

	req3 := httptest.NewRequest("GET", "http://a/", nil)
	req3.RemoteAddr = "[2001:db8:1234:5679::1]:1000"
	if k3 := rlKey(req3, nil); k3 == k1 {
		t.Fatalf("a different /64 should not collide: got %q for both", k3)
	}
}
