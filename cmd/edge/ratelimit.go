package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Per-IP token bucket. In-memory; restart wipes state.
// Capacity = burst, refill = `rate` tokens per second.
//
// Consumption is tracked so an edge can gossip per-IP usage to peer edges
// (see peers.go). A peer's usage is applied via ApplyRemote, which drains
// tokens locally — this makes the cap cluster-wide, not per-instance.

type bucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time

	// consumed = total allowed requests since bucket creation.
	// lastPushed = value at last gossip push. Delta gets shipped to peers.
	consumed   uint64
	lastPushed uint64
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
	b.consumed++
	return true
}

// PushDeltas returns per-key allowed-request counts since the last push
// and advances the watermark. Returned map is safe to mutate.
func (rl *rateLimiter) PushDeltas() map[string]uint64 {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	out := map[string]uint64{}
	for k, b := range rl.buckets {
		if b.consumed > b.lastPushed {
			out[k] = b.consumed - b.lastPushed
			b.lastPushed = b.consumed
		}
	}
	return out
}

// ApplyRemote drains tokens locally to account for peer-side consumption.
// Does NOT touch consumed/lastPushed — remote deltas must not feed back.
func (rl *rateLimiter) ApplyRemote(deltas map[string]uint64) {
	if len(deltas) == 0 {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	for k, d := range deltas {
		b := rl.getOrCreate(k, now)
		elapsed := now.Sub(b.lastRefill).Seconds()
		b.tokens = min(rl.capacity, b.tokens+elapsed*rl.rate)
		b.lastRefill = now
		b.lastSeen = now
		b.tokens -= float64(d)
		if b.tokens < 0 {
			b.tokens = 0
		}
	}
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

// remoteIP is the peer address of the immediate TCP connection.
// The edge is the outermost network hop, so inbound X-Forwarded-* headers
// are attacker-controlled and MUST NOT be trusted here (spoofing them would
// otherwise mint an infinite pool of fresh rate-limit buckets).
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func withRateLimit(next http.Handler, rl *rateLimiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(remoteIP(r)) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
