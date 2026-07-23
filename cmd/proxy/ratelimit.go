// Aggregate rate limiting: token buckets that cap a route's TOTAL request
// rate — all clients combined — not per-IP (the edge binary owns per-IP
// limits). Buckets use float-token refill so fractional tokens accrue between
// requests instead of rounding sustained rates down to zero. Bucket state
// lives in a limiterRegistry owned by the Router, NOT on the RouteGroups:
// groups are rebuilt from scratch on every Docker event / refresh, and a
// rebuild must not hand a drained route a fresh full bucket.
package main

import (
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxRateRPS   = 1_000_000
	maxRateBurst = 10_000_000
)

type rateSpec struct {
	RPS   int
	Burst int
}

// parseRateSpec parses "N" or "N:M" (rps / rps:burst). Valid iff
// 1 <= rps <= maxRateRPS and rps <= burst <= maxRateBurst; burst defaults to
// rps. Anything else — empty, non-integer, out of range, trailing colon —
// is rejected.
func parseRateSpec(s string) (rateSpec, bool) {
	rpsStr, burstStr := s, ""
	hasBurst := false
	if i := strings.IndexByte(s, ':'); i >= 0 {
		rpsStr, burstStr = s[:i], s[i+1:]
		hasBurst = true
	}
	rps, err := strconv.Atoi(rpsStr)
	if err != nil || rps < 1 || rps > maxRateRPS {
		return rateSpec{}, false
	}
	burst := rps
	if hasBurst {
		b, err := strconv.Atoi(burstStr)
		if err != nil || b < rps || b > maxRateBurst {
			return rateSpec{}, false
		}
		burst = b
	}
	return rateSpec{RPS: rps, Burst: burst}, true
}

type tokenBucket struct {
	mu     sync.Mutex
	rps    float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// newTokenBucket returns a full bucket. now == nil means wall clock; tests
// inject a fake.
func newTokenBucket(spec rateSpec, now func() time.Time) *tokenBucket {
	if now == nil {
		now = time.Now
	}
	return &tokenBucket{
		rps:    float64(spec.RPS),
		burst:  float64(spec.Burst),
		tokens: float64(spec.Burst),
		last:   now(),
		now:    now,
	}
}

// allow refills for elapsed time (capped at burst), then admits if a whole
// token is available. On rejection retryAfter is the ceil-seconds wait until
// one token accrues, minimum 1s.
func (b *tokenBucket) allow() (ok bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	if elapsed := t.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rps)
	}
	b.last = t
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	secs := math.Ceil((1 - b.tokens) / b.rps)
	if secs < 1 {
		secs = 1
	}
	return false, time.Duration(secs) * time.Second
}

// retryAfterSeconds converts allow()'s retryAfter into the integer seconds
// used for the Retry-After header, minimum 1.
func retryAfterSeconds(d time.Duration) int {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return secs
}

// limiterRegistry maps route key (host+"|"+path) → bucket. Its mutex is only
// taken during Router.Set — the hot path holds a direct *tokenBucket on the
// RouteGroup and never touches the map.
type limiterRegistry struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	now     func() time.Time
}

func newLimiterRegistry() *limiterRegistry {
	return &limiterRegistry{buckets: map[string]*tokenBucket{}, now: time.Now}
}

// acquire returns the bucket for key, creating a full one if absent. An
// existing bucket has its rps/burst updated in place (tokens clamped down to
// a shrunken burst) — its drained/filled state is deliberately preserved so a
// route rebuild never resets limiting.
func (r *limiterRegistry) acquire(key string, spec rateSpec) *tokenBucket {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.buckets[key]; ok {
		b.mu.Lock()
		b.rps = float64(spec.RPS)
		b.burst = float64(spec.Burst)
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.mu.Unlock()
		return b
	}
	b := newTokenBucket(spec, r.now)
	r.buckets[key] = b
	return b
}

// prune drops buckets for keys no longer present in the incoming route set so
// removed routes don't leak state forever.
func (r *limiterRegistry) prune(live map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.buckets {
		if !live[k] {
			delete(r.buckets, k)
		}
	}
}
