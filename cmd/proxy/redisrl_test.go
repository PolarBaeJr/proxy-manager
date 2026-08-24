package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakePrimary is an in-process stand-in for redisPrimary, so the circuit
// breaker can be exercised deterministically without a real Redis server.
type fakePrimary struct {
	mu        sync.Mutex
	allowFunc func(key string) (bool, error)
	snapFunc  func() ([]bucketSnapshot, error)
	calls     int
}

func (f *fakePrimary) Allow(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	f.calls++
	fn := f.allowFunc
	f.mu.Unlock()
	return fn(key)
}

func (f *fakePrimary) SetRate(int) {}

func (f *fakePrimary) Snapshot(context.Context) ([]bucketSnapshot, error) {
	if f.snapFunc == nil {
		return nil, nil
	}
	return f.snapFunc()
}

func (f *fakePrimary) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestHybridLimiter(primary redisPrimary, rpm int, now func() time.Time) *hybridLimiter {
	return &hybridLimiter{
		primary:  primary,
		fallback: newRateLimiter(rpm),
		now:      now,
	}
}

// TestHybridLimiterUsesRedisWhenHealthy: a healthy primary answers every call.
func TestHybridLimiterUsesRedisWhenHealthy(t *testing.T) {
	primary := &fakePrimary{allowFunc: func(string) (bool, error) { return true, nil }}
	h := newTestHybridLimiter(primary, 60, time.Now)
	defer h.Stop()

	if !h.Allow("1.2.3.4") {
		t.Fatal("expected Redis-backed Allow to return true")
	}
	if primary.callCount() != 1 {
		t.Fatalf("primary calls = %d, want 1", primary.callCount())
	}
}

// TestHybridLimiterSingleErrorDoesNotOpenBreaker: one Redis error must fail
// open to the local fallback for that call, but not trip the breaker — the
// next call must still try Redis.
func TestHybridLimiterSingleErrorDoesNotOpenBreaker(t *testing.T) {
	failNext := true
	primary := &fakePrimary{allowFunc: func(string) (bool, error) {
		if failNext {
			failNext = false
			return false, errors.New("boom")
		}
		return true, nil
	}}
	h := newTestHybridLimiter(primary, 60, time.Now)
	defer h.Stop()

	if !h.Allow("1.2.3.4") {
		t.Fatal("a single Redis error should fail open to the fallback, not deny")
	}
	if h.breakerOpen() {
		t.Fatal("a single failure should not open the breaker")
	}
	if !h.Allow("1.2.3.4") {
		t.Fatal("next call should succeed via Redis")
	}
	if primary.callCount() != 2 {
		t.Fatalf("primary calls = %d, want 2 (breaker must not have skipped Redis)", primary.callCount())
	}
}

// TestHybridLimiterOpensBreakerAfterConsecutiveFailures: N consecutive Redis
// errors must degrade to fallback-only, and further calls within the
// cooldown window must not touch Redis at all.
func TestHybridLimiterOpensBreakerAfterConsecutiveFailures(t *testing.T) {
	primary := &fakePrimary{allowFunc: func(string) (bool, error) { return false, errors.New("boom") }}
	clock := time.Now()
	h := newTestHybridLimiter(primary, 60, func() time.Time { return clock })
	defer h.Stop()

	for i := 0; i < redisFailureThreshold; i++ {
		h.Allow("1.2.3.4")
	}
	if !h.breakerOpen() {
		t.Fatal("breaker should be open after redisFailureThreshold consecutive failures")
	}
	if got := primary.callCount(); got != redisFailureThreshold {
		t.Fatalf("primary calls = %d, want %d", got, redisFailureThreshold)
	}

	// Further calls while the breaker is open must go straight to fallback —
	// primary call count must not increase.
	h.Allow("5.6.7.8")
	if got := primary.callCount(); got != redisFailureThreshold {
		t.Fatalf("primary calls after breaker open = %d, want unchanged %d (should skip Redis)", got, redisFailureThreshold)
	}
}

// TestHybridLimiterRecoversAfterCooldown: once the injected clock passes the
// cooldown window, the next Allow call must retry Redis, and success must
// reset the breaker (failures back to 0, openUntil cleared).
func TestHybridLimiterRecoversAfterCooldown(t *testing.T) {
	failing := true
	primary := &fakePrimary{allowFunc: func(string) (bool, error) {
		if failing {
			return false, errors.New("boom")
		}
		return true, nil
	}}
	clock := time.Now()
	h := newTestHybridLimiter(primary, 60, func() time.Time { return clock })
	defer h.Stop()

	for i := 0; i < redisFailureThreshold; i++ {
		h.Allow("1.2.3.4")
	}
	if !h.breakerOpen() {
		t.Fatal("breaker should be open")
	}

	// Advance the injected clock past the cooldown and let Redis succeed again.
	clock = clock.Add(redisBreakerCooldown + time.Second)
	failing = false
	callsBefore := primary.callCount()
	if !h.Allow("1.2.3.4") {
		t.Fatal("Allow should succeed via recovered Redis")
	}
	if primary.callCount() != callsBefore+1 {
		t.Fatal("expected a retry against Redis once the cooldown elapsed")
	}
	if h.breakerOpen() {
		t.Fatal("a successful call after cooldown should close the breaker")
	}
}

// TestHybridLimiterSnapshotFallsBackWhenDegraded mirrors the Allow-path
// behavior for the dashboard's read view: while degraded, Snapshot must
// report the fallback's state, not attempt Redis.
func TestHybridLimiterSnapshotFallsBackWhenDegraded(t *testing.T) {
	primary := &fakePrimary{
		allowFunc: func(string) (bool, error) { return false, errors.New("boom") },
		snapFunc:  func() ([]bucketSnapshot, error) { return nil, errors.New("should not be called") },
	}
	clock := time.Now()
	h := newTestHybridLimiter(primary, 60, func() time.Time { return clock })
	defer h.Stop()

	for i := 0; i < redisFailureThreshold; i++ {
		h.Allow("1.2.3.4")
	}
	if !h.breakerOpen() {
		t.Fatal("breaker should be open")
	}

	h.fallback.Allow("9.9.9.9") // seed a fallback bucket
	snap := h.Snapshot()
	found := false
	for _, b := range snap {
		if b.Key == "9.9.9.9" {
			found = true
		}
	}
	if !found {
		t.Fatal("Snapshot while degraded should reflect the fallback's buckets")
	}
}

// TestHybridLimiterSnapshotDoesNotTouchBreakerState: Snapshot is a read-only
// dashboard-view path and must never call recordFailure/recordSuccess — a
// slow or failing poll must not be able to trip enforcement into degraded
// mode, and a healthy poll must not be able to mask a genuinely failing
// Allow path by resetting its failure count.
func TestHybridLimiterSnapshotDoesNotTouchBreakerState(t *testing.T) {
	primary := &fakePrimary{
		allowFunc: func(string) (bool, error) { return false, errors.New("boom") },
		snapFunc:  func() ([]bucketSnapshot, error) { return nil, nil },
	}
	h := newTestHybridLimiter(primary, 60, time.Now)
	defer h.Stop()

	for i := 0; i < redisFailureThreshold-1; i++ {
		h.Allow("1.2.3.4")
	}
	h.mu.Lock()
	before := h.failures
	h.mu.Unlock()
	if before != redisFailureThreshold-1 {
		t.Fatalf("failures = %d, want %d before Snapshot", before, redisFailureThreshold-1)
	}

	h.Snapshot() // healthy read must not reset the failure count

	h.mu.Lock()
	after := h.failures
	h.mu.Unlock()
	if after != before {
		t.Fatalf("failures after Snapshot = %d, want unchanged %d (Snapshot must not call recordSuccess)", after, before)
	}
	if h.breakerOpen() {
		t.Fatal("a healthy Snapshot must not open the breaker")
	}
}

// TestRedisLimiterIntegration exercises the real Lua EVAL round trip against
// a live Redis server. Skipped cleanly unless REDIS_TEST_URL is set, so CI
// (which has no Redis) still passes.
func TestRedisLimiterIntegration(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_URL")
	if addr == "" {
		t.Skip("REDIS_TEST_URL not set; skipping real-Redis integration test")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("could not reach Redis at %s: %v", addr, err)
	}

	routeKey := "integration-test.example|"
	rl := newRedisLimiter(client, routeKey, 3, time.Minute)
	defer client.Del(ctx, rl.redisKey("1.2.3.4"))

	for i := 0; i < 3; i++ {
		allowed, err := rl.Allow(ctx, "1.2.3.4")
		if err != nil {
			t.Fatalf("Allow #%d: %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("Allow #%d denied, want allowed (capacity 3)", i+1)
		}
	}
	allowed, err := rl.Allow(ctx, "1.2.3.4")
	if err != nil {
		t.Fatalf("Allow past capacity: %v", err)
	}
	if allowed {
		t.Fatal("Allow past capacity should be denied")
	}

	snap, err := rl.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap) != 1 || snap[0].Key != "1.2.3.4" {
		t.Fatalf("Snapshot = %+v, want one entry for 1.2.3.4", snap)
	}
}
