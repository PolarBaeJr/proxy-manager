package main

import (
	"testing"
	"time"
)

func TestAllowConsumesTokens(t *testing.T) {
	rl := &rateLimiter{buckets: map[string]*bucket{}, rate: 10, capacity: 3}
	// Fresh bucket starts full at capacity=3.
	for i := 0; i < 3; i++ {
		if !rl.Allow("k") {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	if rl.Allow("k") {
		t.Fatal("4th request should be denied (bucket empty)")
	}
	// Simulate a second passing: 10 tokens/s refills the bucket.
	rl.buckets["k"].lastRefill = time.Now().Add(-time.Second)
	if !rl.Allow("k") {
		t.Fatal("request after refill should be allowed")
	}
}

func TestPushDeltas(t *testing.T) {
	rl := &rateLimiter{buckets: map[string]*bucket{}, rate: 10, capacity: 10}
	rl.Allow("k")
	rl.Allow("k")
	d := rl.PushDeltas()
	if d["k"] != 2 {
		t.Fatalf("delta = %d, want 2", d["k"])
	}
	// Watermark advanced: nothing new to push.
	if len(rl.PushDeltas()) != 0 {
		t.Fatal("second PushDeltas should be empty")
	}
}

func TestApplyRemote(t *testing.T) {
	rl := &rateLimiter{buckets: map[string]*bucket{}, rate: 0, capacity: 5}
	rl.buckets["k"] = &bucket{tokens: 3, lastRefill: time.Now()}

	rl.ApplyRemote(map[string]uint64{"k": 2})
	if got := rl.buckets["k"].tokens; got != 1 {
		t.Fatalf("tokens after drain = %v, want 1", got)
	}
	if rl.buckets["k"].consumed != 0 {
		t.Fatal("ApplyRemote must not touch consumed")
	}

	// Draining below zero floors at zero.
	rl.ApplyRemote(map[string]uint64{"k": 10})
	if got := rl.buckets["k"].tokens; got != 0 {
		t.Fatalf("tokens = %v, want 0 floor", got)
	}

	// Empty deltas is a no-op.
	rl.ApplyRemote(nil)
	if got := rl.buckets["k"].tokens; got != 0 {
		t.Fatalf("tokens = %v, want unchanged", got)
	}
}
