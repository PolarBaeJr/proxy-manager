package main

import (
	"sync"
	"time"
)

// Per-IP token bucket for proxy.ratelimit hosts. In-memory; restart wipes state.
// Capacity = burst, refill = `rate` tokens per second.
//
// When -redis-addr is unset (the default), the proxy is single-instance in
// prod and this in-memory limiter is the whole story: no cross-instance
// gossip, per-instance by design. When -redis-addr IS set, RouteGroup.limiter
// is instead a hybridLimiter (see redisrl.go) backed by a shared Redis
// instance, so the same per-IP cap applies across every proxy in the mesh —
// with automatic fail-open-to-this-type on a Redis outage. Note the auth
// gate's -auth-trusted-cidrs LAN bypass intentionally does NOT apply here —
// LAN clients are still throttled.

// limiter is the interface RouteGroup.limiter and Router.limiters hold, so
// the in-memory rateLimiter and the Redis-backed hybridLimiter (redisrl.go)
// are interchangeable at every call site.
type limiter interface {
	Allow(key string) bool
	SetRate(rpm int)
	Stop()
	// Snapshot returns a read-only view of currently tracked buckets, for the
	// dashboard's rate-limit view. Never mutates state (does not consume a
	// token or apply a refill write) — RECOMPUTED refill is fine to report,
	// STORED tokens must not change as a side effect of viewing them.
	Snapshot() []bucketSnapshot
}

// bucketSnapshot is one client's current rate-limit bucket state, as reported
// by Snapshot(). Tokens is refill-adjusted to "now" for display, without
// writing that adjustment back.
type bucketSnapshot struct {
	Key      string  `json:"key"`
	Tokens   float64 `json:"tokens"`
	Capacity float64 `json:"capacity"`
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
}

type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     float64 // tokens/sec
	capacity float64 // max tokens
	stop     chan struct{}
}

const idleEvict = 30 * time.Minute

// maxBuckets bounds per-limiter memory under a distinct-key flood (e.g. an
// attacker rotating through a /64 of IPv6 addresses). Once full, new keys are
// denied rather than evicting existing buckets, so an active flood can't push
// out legitimate, already-tracked clients; gc() reclaims space as buckets go
// idle.
const maxBuckets = 50_000

func newRateLimiter(rpm int) *rateLimiter {
	rl := &rateLimiter{
		buckets:  map[string]*bucket{},
		rate:     float64(rpm) / 60.0,
		capacity: float64(rpm),
		stop:     make(chan struct{}),
	}
	go rl.gc()
	return rl
}

// Stop halts the gc goroutine. Called when a limiter is dropped on route
// removal so it doesn't outlive its route (goroutine + ticker leak).
func (rl *rateLimiter) Stop() { close(rl.stop) }

// SetRate updates the refill rate and capacity in place without resetting
// existing buckets — a config edit (new rpm) must not hand every client a
// fresh full bucket. The refill logic converges tokens toward the new
// capacity naturally.
func (rl *rateLimiter) SetRate(rpm int) {
	rl.mu.Lock()
	rl.rate = float64(rpm) / 60.0
	rl.capacity = float64(rpm)
	rl.mu.Unlock()
}

func (rl *rateLimiter) getOrCreate(key string, now time.Time) (*bucket, bool) {
	b, ok := rl.buckets[key]
	if ok {
		return b, true
	}
	if len(rl.buckets) >= maxBuckets {
		return nil, false
	}
	b = &bucket{tokens: rl.capacity, lastRefill: now, lastSeen: now}
	rl.buckets[key] = b
	return b, true
}

func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.getOrCreate(key, now)
	if !ok {
		return false
	}
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens = min(rl.capacity, b.tokens+elapsed*rl.rate)
	b.lastRefill = now
	b.lastSeen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Snapshot reports each tracked bucket's current key and refill-adjusted
// token count without mutating rl.buckets — a read for the dashboard's
// rate-limit view, not a consuming Allow call.
func (rl *rateLimiter) Snapshot() []bucketSnapshot {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	out := make([]bucketSnapshot, 0, len(rl.buckets))
	for key, b := range rl.buckets {
		elapsed := now.Sub(b.lastRefill).Seconds()
		tokens := min(rl.capacity, b.tokens+elapsed*rl.rate)
		out = append(out, bucketSnapshot{Key: key, Tokens: tokens, Capacity: rl.capacity})
	}
	return out
}

var _ limiter = (*rateLimiter)(nil)

func (rl *rateLimiter) gc() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-rl.stop:
			return
		case <-t.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-idleEvict)
			for k, v := range rl.buckets {
				if v.lastSeen.Before(cutoff) {
					delete(rl.buckets, k)
				}
			}
			rl.mu.Unlock()
		}
	}
}
