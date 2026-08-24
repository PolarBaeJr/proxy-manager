package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis-backed shared rate limiter. Preserves the same continuous-refill
// token-bucket semantics as ratelimit.go's in-memory rateLimiter, but the
// bucket lives in Redis so every proxy instance in the mesh enforces one
// shared cap per client IP per route instead of one cap per instance.
//
// Key scheme: pmgr:rl:{host}|{path}:{clientIP} — routeKey (host+"|"+path) is
// baked in at construction (it matches Router.limiters' map key), so the
// suffix is just the rlKey() the caller already computed.
//
// Unlike the in-memory limiter, this keyspace has no maxBuckets-equivalent
// count bound — Redis is shared across the mesh, so an IP-rotating flood
// grows it without limit in the count dimension. It IS bounded in time: every
// key carries a TTL equal to idleEvict (the same idle-eviction window the
// in-memory limiter uses), so an abandoned bucket self-expires. A determined
// distinct-key flood could still inflate Redis memory faster than TTLs
// reclaim it; that tradeoff is accepted for v1 rather than adding a second
// bounding mechanism (e.g. Redis-side key-count sampling) that the in-memory
// limiter doesn't need. Revisit if this proves exploitable in practice.

const redisCallTimeout = 2 * time.Second

// tokenBucketScript performs the entire get-refill-decrement as one atomic
// EVAL so concurrent proxy instances hitting the same key can't race each
// other into over-granting tokens. now is passed in as ARGV rather than
// calling redis.call("TIME") so callers control the clock (and so a clock-
// skewed peer's stored ts can't make elapsed go negative — clamped below).
var tokenBucketScript = redis.NewScript(`
local key = KEYS[1]
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local data = redis.call("HMGET", key, "tokens", "ts")
local tokens
local ts
if data[1] then
  tokens = tonumber(data[1])
  ts = tonumber(data[2])
else
  tokens = capacity
  ts = now
end

local elapsed = now - ts
if elapsed < 0 then elapsed = 0 end
tokens = math.min(capacity, tokens + elapsed * rate)

local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end

redis.call("HMSET", key, "tokens", tostring(tokens), "ts", tostring(now))
redis.call("EXPIRE", key, ttl)

return allowed
`)

// redisPrimary is the minimal surface hybridLimiter needs from the
// Redis-backed path, kept narrow and swappable so redisrl_test.go can inject
// a fake in-process responder instead of a real Redis server. Allow/Snapshot
// return an error (unlike the limiter interface's Allow) so hybridLimiter's
// circuit breaker can distinguish "Redis correctly denied" from
// "Redis is unreachable/erroring".
type redisPrimary interface {
	Allow(ctx context.Context, key string) (bool, error)
	SetRate(rpm int)
	Snapshot(ctx context.Context) ([]bucketSnapshot, error)
}

// redisLimiter implements redisPrimary against a real *redis.Client. The
// client is shared across every route group's limiter (constructed once in
// main.go), so redisLimiter never owns or closes it.
type redisLimiter struct {
	client   *redis.Client
	routeKey string // "{host}|{path}"

	mu       sync.Mutex
	rate     float64
	capacity float64
	ttl      time.Duration
}

func newRedisLimiter(client *redis.Client, routeKey string, rpm int, ttl time.Duration) *redisLimiter {
	return &redisLimiter{
		client:   client,
		routeKey: routeKey,
		rate:     float64(rpm) / 60.0,
		capacity: float64(rpm),
		ttl:      ttl,
	}
}

func (rl *redisLimiter) SetRate(rpm int) {
	rl.mu.Lock()
	rl.rate = float64(rpm) / 60.0
	rl.capacity = float64(rpm)
	rl.mu.Unlock()
}

func (rl *redisLimiter) redisKey(key string) string {
	return "pmgr:rl:" + rl.routeKey + ":" + key
}

// globEscape escapes SCAN MATCH's glob metacharacters (*, ?, [, ], \) in a
// literal prefix. routeKey is built from proxy.host/proxy.path Docker
// labels, which aren't restricted from containing these characters — without
// this, a route whose host or path happens to contain e.g. "*" could have
// its Snapshot's MATCH pattern glob across other routes' keys and surface
// another route's client IPs.
func globEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`)
	return r.Replace(s)
}

func (rl *redisLimiter) Allow(ctx context.Context, key string) (bool, error) {
	rl.mu.Lock()
	capacity, rate, ttl := rl.capacity, rl.rate, rl.ttl
	rl.mu.Unlock()

	now := float64(time.Now().UnixNano()) / 1e9
	res, err := tokenBucketScript.Run(ctx, rl.client, []string{rl.redisKey(key)}, capacity, rate, now, int64(ttl.Seconds())).Result()
	if err != nil {
		return false, err
	}
	allowed, ok := res.(int64)
	if !ok {
		return false, errors.New("redisrl: unexpected script result type")
	}
	return allowed == 1, nil
}

// Snapshot reads current bucket state for every client key under this
// route's prefix, without going through the (mutating) Lua script. Refill is
// recomputed here in Go for display only — the stored value in Redis is left
// untouched, matching the read-only contract of limiter.Snapshot.
func (rl *redisLimiter) Snapshot(ctx context.Context) ([]bucketSnapshot, error) {
	rl.mu.Lock()
	capacity, rate := rl.capacity, rl.rate
	rl.mu.Unlock()

	prefix := "pmgr:rl:" + rl.routeKey + ":"
	var out []bucketSnapshot
	iter := rl.client.Scan(ctx, 0, globEscape(prefix)+"*", 100).Iterator()
	now := float64(time.Now().UnixNano()) / 1e9
	for iter.Next(ctx) {
		fullKey := iter.Val()
		vals, err := rl.client.HMGet(ctx, fullKey, "tokens", "ts").Result()
		if err != nil || len(vals) < 2 || vals[0] == nil || vals[1] == nil {
			continue
		}
		tokensStr, ok1 := vals[0].(string)
		tsStr, ok2 := vals[1].(string)
		if !ok1 || !ok2 {
			continue
		}
		tokens, err1 := strconv.ParseFloat(tokensStr, 64)
		ts, err2 := strconv.ParseFloat(tsStr, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		elapsed := now - ts
		if elapsed < 0 {
			elapsed = 0
		}
		tokens = min(capacity, tokens+elapsed*rate)
		out = append(out, bucketSnapshot{
			Key:      strings.TrimPrefix(fullKey, prefix),
			Tokens:   tokens,
			Capacity: capacity,
		})
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// hybridLimiter pairs the Redis-backed primary with an in-memory fallback,
// behind a circuit breaker: after redisFailureThreshold consecutive Redis
// errors, it degrades to the fallback exclusively for redisBreakerCooldown,
// then transparently retries Redis on the next call. Deliberately
// fail-open-to-local-fallback, never fail-open-to-unlimited and never
// fail-closed: rate limiting is abuse prevention, not authn, so a Redis
// outage should degrade enforcement to per-instance rather than take
// protected routes down or lift the cap entirely.
//
// The fallback's buckets sit untouched while the breaker is closed (every
// Allow goes to Redis), so at the instant of failover every client starts
// from full local capacity — a Redis blip grants a brief burst amnesty, and
// the fallback's state is correspondingly stale once Redis recovers. Accepted
// as part of "degrade to per-instance enforcement", not a bug to fix here.
type hybridLimiter struct {
	primary  redisPrimary
	fallback *rateLimiter

	mu        sync.Mutex
	failures  int
	openUntil time.Time
	now       func() time.Time // injectable for tests; defaults to time.Now
}

const (
	redisFailureThreshold = 3
	redisBreakerCooldown  = 30 * time.Second
)

func newHybridLimiter(client *redis.Client, routeKey string, rpm int, ttl time.Duration) *hybridLimiter {
	return &hybridLimiter{
		primary:  newRedisLimiter(client, routeKey, rpm, ttl),
		fallback: newRateLimiter(rpm),
		now:      time.Now,
	}
}

var _ limiter = (*hybridLimiter)(nil)

func (h *hybridLimiter) breakerOpen() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now().Before(h.openUntil)
}

func (h *hybridLimiter) recordFailure() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures++
	if h.failures >= redisFailureThreshold {
		h.openUntil = h.now().Add(redisBreakerCooldown)
	}
}

func (h *hybridLimiter) recordSuccess() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures = 0
	h.openUntil = time.Time{}
}

func (h *hybridLimiter) Allow(key string) bool {
	if h.breakerOpen() {
		return h.fallback.Allow(key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisCallTimeout)
	defer cancel()
	allowed, err := h.primary.Allow(ctx, key)
	if err != nil {
		h.recordFailure()
		return h.fallback.Allow(key)
	}
	h.recordSuccess()
	return allowed
}

func (h *hybridLimiter) SetRate(rpm int) {
	h.primary.SetRate(rpm)
	h.fallback.SetRate(rpm)
}

// Stop halts the local fallback's gc goroutine. The shared *redis.Client is
// owned by main.go, not by any one hybridLimiter, so it is never closed here.
func (h *hybridLimiter) Stop() { h.fallback.Stop() }

// Snapshot reads from whichever backend is currently authoritative: Redis
// when the breaker is closed (it holds the real cross-peer state), the local
// fallback when degraded (it's what's actually being enforced right now).
// Deliberately does NOT call recordFailure/recordSuccess — this is a
// read-only dashboard-view path, and letting it touch breaker state would
// let a slow poll trip enforcement into degraded mode, or let a healthy poll
// mask a genuinely failing Allow path by resetting its failure count.
func (h *hybridLimiter) Snapshot() []bucketSnapshot {
	if h.breakerOpen() {
		return h.fallback.Snapshot()
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisCallTimeout)
	defer cancel()
	snap, err := h.primary.Snapshot(ctx)
	if err != nil {
		return h.fallback.Snapshot()
	}
	return snap
}
