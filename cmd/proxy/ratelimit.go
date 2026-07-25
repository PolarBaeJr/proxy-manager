package main

import (
	"sync"
	"time"
)

// Per-IP token bucket for proxy.ratelimit hosts. In-memory; restart wipes state.
// Capacity = burst, refill = `rate` tokens per second.
//
// The proxy is single-instance in prod, so there's no cross-instance gossip
// (unlike cmd/edge, which ships per-IP usage to peers). The rate limit is
// therefore per-instance by design. Note the auth gate's -auth-trusted-cidrs
// LAN bypass intentionally does NOT apply here — LAN clients are still throttled.

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

func (rl *rateLimiter) getOrCreate(key string, now time.Time) *bucket {
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.capacity, lastRefill: now, lastSeen: now}
		rl.buckets[key] = b
	}
	return b
}

func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b := rl.getOrCreate(key, now)
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
