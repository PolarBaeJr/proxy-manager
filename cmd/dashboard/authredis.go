// Redis adapter for AuthStore's shared-user-identity path (see txRunner in
// auth.go). Only this file and main.go's wiring know about *redis.Client —
// everything else in the dashboard talks to AuthStore through mutateUsers,
// which is a no-op passthrough to the local file when txRunner is nil.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisTxRunner implements txRunner against a real *redis.Client. The
// client is shared with whatever else in the process uses Redis (none,
// today), so redisTxRunner never owns or closes it.
type redisTxRunner struct{ client *redis.Client }

func (r *redisTxRunner) RunTx(ctx context.Context, key string, fn func(current []byte) (newVal []byte, err error)) error {
	err := r.client.Watch(ctx, func(tx *redis.Tx) error {
		cur, gerr := tx.Get(ctx, key).Bytes()
		if gerr != nil {
			if !errors.Is(gerr, redis.Nil) {
				return gerr
			}
			cur = nil
		}
		newVal, ferr := fn(cur)
		if ferr != nil {
			return ferr
		}
		_, perr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, key, newVal, 0)
			return nil
		})
		return perr
	}, key)
	if errors.Is(err, redis.TxFailedErr) {
		return errTxConflict
	}
	return err
}

func (r *redisTxRunner) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return b, err
}

// syncFromRedisOrImport runs once at startup, after loadAuthStore has
// populated s.data.Users from the local file (post-legacy-migration).
//
// If Redis already has state for authRedisKey (from this or a peer
// instance's prior run), that state is adopted, discarding whatever the
// local file just loaded. If the key is absent, Redis is seeded from the
// local file's Users — race-safe against a peer doing the same thing
// concurrently via RunTx's normal WATCH/EXEC semantics.
//
// Idempotency is keyed purely on "does the Redis key exist" — every restart
// re-runs this; no marker flag needed.
func (s *AuthStore) syncFromRedisOrImport(ctx context.Context) error {
	cur, err := s.txRunner.Get(ctx, authRedisKey)
	if err != nil {
		return err // connectivity error — caller logs a warning, keeps local file's Users as-is
	}
	if cur != nil {
		var users []User
		if jerr := json.Unmarshal(cur, &users); jerr != nil {
			log.Printf("dashboard auth: redis auth state is corrupt, keeping local file's Users: %v", jerr)
			return nil
		}
		s.mu.Lock()
		s.data.Users = users
		s.mu.Unlock()
		return nil
	}
	seedErr := s.txRunner.RunTx(ctx, authRedisKey, func(existing []byte) ([]byte, error) {
		if existing != nil {
			return nil, errTxConflict // someone beat us to it — bail, caller re-adopts below
		}
		s.mu.RLock()
		users := s.data.Users
		s.mu.RUnlock()
		if users == nil {
			users = []User{}
		}
		return json.Marshal(users)
	})
	if seedErr != nil && errors.Is(seedErr, errTxConflict) {
		// Lost the seed race — re-fetch and adopt the winner's state instead.
		// Bounded: the recursive call finds cur != nil (the winner's write)
		// and takes the adopt branch above, not this one, so it cannot loop.
		return s.syncFromRedisOrImport(ctx)
	}
	return seedErr
}

const redisUsersRefreshInterval = 5 * time.Second

// refreshLoop keeps s.data.Users in sync with Redis for reads that never
// go through mutateUsers (every findUser-based read in auth.go/passkey.go).
// Only writes s.save() on an observed change (dedup via lastRaw), not on
// every tick, to avoid needless disk writes.
func (s *AuthStore) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(redisUsersRefreshInterval)
	defer ticker.Stop()
	var lastRaw string
	healthy := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			raw, err := s.txRunner.Get(ctx, authRedisKey)
			if err != nil {
				if healthy {
					log.Printf("dashboard auth: redis refresh lost connectivity: %v", err)
					healthy = false
				}
				continue
			}
			if !healthy {
				log.Printf("dashboard auth: redis refresh connectivity restored")
				healthy = true
			}
			if string(raw) == lastRaw {
				continue
			}
			var users []User
			if json.Unmarshal(raw, &users) != nil {
				continue
			}
			s.mu.Lock()
			s.data.Users = users
			_ = s.save()
			s.mu.Unlock()
			lastRaw = string(raw)
		}
	}
}
