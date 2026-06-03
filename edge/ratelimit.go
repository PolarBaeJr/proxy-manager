package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Per-IP token bucket. In-memory; restart wipes state.
// Capacity = burst, refill = `rate` tokens per second.

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
}

const idleEvict = 30 * time.Minute

func newRateLimiter(perSec, burst int) *rateLimiter {
	rl := &rateLimiter{
		buckets:  map[string]*bucket{},
		rate:     float64(perSec),
		capacity: float64(burst),
	}
	go rl.gc()
	return rl
}

func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.capacity, lastRefill: now, lastSeen: now}
		rl.buckets[key] = b
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

func (rl *rateLimiter) gc() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
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

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		for i := 0; i < len(fwd); i++ {
			if fwd[i] == ',' {
				return fwd[:i]
			}
		}
		return fwd
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func withRateLimit(next http.Handler, perSec, burst int) http.Handler {
	rl := newRateLimiter(perSec, burst)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(clientIP(r)) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
