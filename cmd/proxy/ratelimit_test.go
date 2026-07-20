package main

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newFakeClock() *fakeClock               { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func TestParseRateSpec(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		rps   int
		burst int
	}{
		{"100", true, 100, 100},
		{"100:250", true, 100, 250},
		{"1", true, 1, 1},
		{"1000000", true, 1000000, 1000000},
		{"100:100", true, 100, 100},
		{"0", false, 0, 0},
		{"-1", false, 0, 0},
		{"100:50", false, 0, 0}, // burst < rps
		{"abc", false, 0, 0},
		{"1.5", false, 0, 0},
		{"100:", false, 0, 0}, // trailing colon
		{":100", false, 0, 0},
		{"", false, 0, 0},
		{"1000001", false, 0, 0},      // rps > max
		{"100:10000001", false, 0, 0}, // burst > max
		{"100:250:300", false, 0, 0},
	}
	for _, tc := range cases {
		spec, ok := parseRateSpec(tc.in)
		if ok != tc.ok {
			t.Errorf("parseRateSpec(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && (spec.RPS != tc.rps || spec.Burst != tc.burst) {
			t.Errorf("parseRateSpec(%q) = %+v, want rps=%d burst=%d", tc.in, spec, tc.rps, tc.burst)
		}
	}
}

func TestBucketBurstThenSustained(t *testing.T) {
	clk := newFakeClock()
	b := newTokenBucket(rateSpec{RPS: 10, Burst: 3}, clk.now)

	// New bucket starts full: exactly burst admissions.
	for i := 0; i < 3; i++ {
		if ok, _ := b.allow(); !ok {
			t.Fatalf("burst request %d rejected, want allowed", i+1)
		}
	}
	if ok, _ := b.allow(); ok {
		t.Fatal("request past burst allowed, want rejected")
	}

	// 1/rps later exactly one token has accrued.
	clk.advance(100 * time.Millisecond)
	if ok, _ := b.allow(); !ok {
		t.Fatal("request after 1/rps refill rejected, want allowed")
	}
	if ok, _ := b.allow(); ok {
		t.Fatal("second request after single-token refill allowed, want rejected")
	}

	// A long idle refills to burst, not beyond.
	clk.advance(10 * time.Second)
	for i := 0; i < 3; i++ {
		if ok, _ := b.allow(); !ok {
			t.Fatalf("post-idle request %d rejected, want allowed (refill caps at burst)", i+1)
		}
	}
	if ok, _ := b.allow(); ok {
		t.Fatal("refill exceeded burst")
	}
}

func TestBucketRetryAfter(t *testing.T) {
	clk := newFakeClock()

	// Fast bucket: sub-second wait still rounds up to 1s.
	fast := newTokenBucket(rateSpec{RPS: 100, Burst: 100}, clk.now)
	for i := 0; i < 100; i++ {
		fast.allow()
	}
	ok, retry := fast.allow()
	if ok {
		t.Fatal("drained bucket allowed")
	}
	if retry < time.Second {
		t.Fatalf("retry = %v, want >= 1s", retry)
	}
	if retryAfterSeconds(retry) < 1 {
		t.Fatalf("retryAfterSeconds(%v) < 1", retry)
	}

	// Slow bucket: ceil((1-tokens)/rps) — 1 rps fully drained → 1s.
	slow := newTokenBucket(rateSpec{RPS: 1, Burst: 1}, clk.now)
	slow.allow()
	ok, retry = slow.allow()
	if ok {
		t.Fatal("drained slow bucket allowed")
	}
	if got := retryAfterSeconds(retry); got != 1 {
		t.Fatalf("slow retry seconds = %d, want 1", got)
	}
}

func TestRegistryReconcile(t *testing.T) {
	clk := newFakeClock()
	reg := newLimiterRegistry()
	reg.now = clk.now

	b1 := reg.acquire("h|", rateSpec{RPS: 10, Burst: 3})
	// Drain two of three tokens.
	b1.allow()
	b1.allow()

	// Same key + spec → same bucket, state preserved.
	b2 := reg.acquire("h|", rateSpec{RPS: 10, Burst: 3})
	if b1 != b2 {
		t.Fatal("re-acquire returned a different bucket")
	}
	if ok, _ := b2.allow(); !ok {
		t.Fatal("third token should still be available")
	}
	if ok, _ := b2.allow(); ok {
		t.Fatal("bucket should be drained — re-acquire must not refill")
	}

	// Changed spec updates params in place; tokens clamp down, never up.
	b3 := reg.acquire("h|", rateSpec{RPS: 5, Burst: 2})
	if b3 != b1 {
		t.Fatal("changed spec should update the existing bucket, not replace it")
	}
	if b3.rps != 5 || b3.burst != 2 {
		t.Fatalf("bucket params = %v/%v, want 5/2", b3.rps, b3.burst)
	}
	if ok, _ := b3.allow(); ok {
		t.Fatal("spec change granted a free refill")
	}
	// Idle long enough to refill fully — capped at the NEW burst.
	clk.advance(time.Minute)
	if b3.tokens > 2 { // pre-refill sanity; refill happens in allow
		t.Fatalf("tokens = %v, want <= new burst", b3.tokens)
	}
	if ok, _ := b3.allow(); !ok {
		t.Fatal("refilled bucket rejected")
	}
	if ok, _ := b3.allow(); !ok {
		t.Fatal("refilled bucket rejected second token")
	}
	if ok, _ := b3.allow(); ok {
		t.Fatal("refill exceeded new burst of 2")
	}

	// Prune drops absent keys; re-acquire starts fresh and full.
	reg.prune(map[string]bool{})
	b4 := reg.acquire("h|", rateSpec{RPS: 10, Burst: 3})
	if b4 == b1 {
		t.Fatal("pruned key returned the old bucket")
	}
	for i := 0; i < 3; i++ {
		if ok, _ := b4.allow(); !ok {
			t.Fatalf("fresh bucket rejected request %d", i+1)
		}
	}
}
