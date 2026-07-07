package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimiter — per-IP token bucket for the passkey login endpoints. In-memory
// only; restarts wipe state. Duplicated from the dashboard deliberately to keep
// the blast radius of the opt-in passkey feature inside cmd/auth.
//
// Defaults: 5 attempts allowed, refill 1 per minute.

const (
	rlCapacity    = 5
	rlRefillEvery = time.Minute
	rlIdleEvict   = 30 * time.Minute
)

type bucket struct {
	tokens     int
	lastRefill time.Time
	lastSeen   time.Time
}

type rateLimiter struct {
	mu sync.Mutex
	b  map[string]*bucket
}

func newRateLimiter() *rateLimiter {
	rl := &rateLimiter{b: map[string]*bucket{}}
	go rl.gc()
	return rl
}

// Allow returns true if the request from `key` should proceed.
func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.b[key]
	if !ok {
		b = &bucket{tokens: rlCapacity, lastRefill: now, lastSeen: now}
		rl.b[key] = b
	}
	if elapsed := now.Sub(b.lastRefill); elapsed >= rlRefillEvery {
		add := int(elapsed / rlRefillEvery)
		if add > 0 {
			b.tokens += add
			if b.tokens > rlCapacity {
				b.tokens = rlCapacity
			}
			b.lastRefill = b.lastRefill.Add(time.Duration(add) * rlRefillEvery)
		}
	}
	b.lastSeen = now
	if b.tokens <= 0 {
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
		cutoff := time.Now().Add(-rlIdleEvict)
		for k, v := range rl.b {
			if v.lastSeen.Before(cutoff) {
				delete(rl.b, k)
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

// limit wraps a handler with rate limiting. Returns 429 on limit hit.
func (rl *rateLimiter) limit(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(clientIP(r)) {
			http.Error(w, "too many attempts — wait a few minutes", http.StatusTooManyRequests)
			return
		}
		h(w, r)
	}
}
