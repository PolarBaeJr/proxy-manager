// Central per-service env: the NON-ORIGIN side.
//
// A host that runs replicas of a service adopted elsewhere keeps the last
// env it was handed for that service (already resolved for THIS host's
// identity by the origin) so it can still create replicas while the origin
// is unreachable — flagged stale, never silently. One file per service,
// written atomically, versions only ever move forward.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultCentralEnvCacheDir = "/data/central-env-cache"

type centralEnvCacheEntry struct {
	Service     string   `json:"service"`
	Origin      string   `json:"origin"`
	Version     uint64   `json:"version"`
	Env         []string `json:"env"`
	PrevEnv     []string `json:"prev_env,omitempty"`
	PrevVersion uint64   `json:"prev_version,omitempty"`
	FetchedAt   int64    `json:"fetched_at"`
	// Healthcheck is the origin template's Config.Healthcheck, carried so a
	// replica recreated here keeps a health gate even when this host's own
	// template never had one (spread replicas created before the field
	// existed). Not versioned with Env: it describes the service, not a
	// value, and a nil offer keeps whatever was cached before.
	Healthcheck *healthcheckSpec `json:"healthcheck,omitempty"`
	// FailedVersion is a version that failed its health gate on THIS host
	// and was rolled back from. Put refuses it (and anything older) until
	// the origin publishes something newer, so a later scale here can
	// never bring it back. When it equals Version the current Env itself is
	// the failed one (nothing to roll back to) and must not be used.
	FailedVersion uint64 `json:"failed_version,omitempty"`
}

// usable reports whether Env may be used to create replicas.
func (e centralEnvCacheEntry) usable() bool {
	return e.FailedVersion != e.Version
}

// errCentralEnvCacheOlder is Put refusing to move a cached entry backwards.
type errCentralEnvCacheOlder struct {
	Service         string
	Cached, Offered uint64
}

func (e errCentralEnvCacheOlder) Error() string {
	return fmt.Sprintf("central env for %q: offered version %d is older than cached version %d", e.Service, e.Offered, e.Cached)
}

// errCentralEnvCacheFailed is Put refusing a version that already failed its
// health gate on this host.
type errCentralEnvCacheFailed struct {
	Service         string
	Failed, Offered uint64
}

func (e errCentralEnvCacheFailed) Error() string {
	return fmt.Sprintf("central env for %q: version %d failed its health gate on this host (failed: v%d) — not re-applied until the origin publishes a newer version or a sync retries it", e.Service, e.Offered, e.Failed)
}

type centralEnvCache struct {
	dir   string
	mu    sync.Mutex
	items map[string]*centralEnvCacheEntry
}

// loadCentralEnvCache creates dir (0700) if needed and loads every
// <service>.json in it. Unlike the origin store, a corrupt cache file is
// simply dropped: the cache is never authoritative, and a service whose
// replicas carry pmgr.env.origin with no usable cache already fails closed in
// Resolve (errCentralEnvUnavailable).
func loadCentralEnvCache(dir string) (*centralEnvCache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &centralEnvCache{dir: dir, items: map[string]*centralEnvCacheEntry{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		svc := strings.TrimSuffix(name, ".json")
		if !validServiceName(svc) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			log.Printf("central env cache: %s unreadable — ignored", name)
			continue
		}
		var ent centralEnvCacheEntry
		if err := json.Unmarshal(data, &ent); err != nil || ent.Service != svc {
			log.Printf("central env cache: %s is corrupt — ignored", name)
			continue
		}
		c.items[svc] = &ent
	}
	return c, nil
}

func (c *centralEnvCache) path(svc string) string {
	return filepath.Join(c.dir, svc+".json")
}

func (c *centralEnvCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func (c *centralEnvCache) Get(svc string) (centralEnvCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ent := c.items[svc]
	if ent == nil {
		return centralEnvCacheEntry{}, false
	}
	return copyCacheEntry(ent), true
}

// Services lists every cached service name — for the reconcile loop, which
// asks each one's origin whether it has moved on.
func (c *centralEnvCache) Services() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.items))
	for svc := range c.items {
		out = append(out, svc)
	}
	sort.Strings(out)
	return out
}

// Put records env as svc's current central env. An older version than the
// one cached is refused (a delayed response must not roll replicas back); a
// newer one rotates the current entry into prev_*; the same version just
// refreshes it. Any cached healthcheck is kept.
func (c *centralEnvCache) Put(svc, origin string, version uint64, env []string) error {
	return c.PutWithHealthcheck(svc, origin, version, env, nil)
}

// PutWithHealthcheck is Put that also records the origin template's
// healthcheck; nil keeps the one already cached.
func (c *centralEnvCache) PutWithHealthcheck(svc, origin string, version uint64, env []string, hc *healthcheckSpec) error {
	if !validServiceName(svc) {
		return fmt.Errorf("invalid service name %q", svc)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := centralEnvCacheEntry{
		Service:     svc,
		Origin:      origin,
		Version:     version,
		Env:         append([]string(nil), env...),
		FetchedAt:   time.Now().Unix(),
		Healthcheck: copyHealthcheck(hc),
	}
	if cur := c.items[svc]; cur != nil {
		if next.Healthcheck == nil {
			next.Healthcheck = copyHealthcheck(cur.Healthcheck)
		}
		if version < cur.Version {
			return errCentralEnvCacheOlder{Service: svc, Cached: cur.Version, Offered: version}
		}
		if version <= cur.FailedVersion && (version > cur.Version || !cur.usable()) {
			return errCentralEnvCacheFailed{Service: svc, Failed: cur.FailedVersion, Offered: version}
		}
		if version == cur.Version {
			next.PrevEnv = append([]string(nil), cur.PrevEnv...)
			next.PrevVersion = cur.PrevVersion
			next.FailedVersion = cur.FailedVersion
		} else {
			next.PrevEnv = append([]string(nil), cur.Env...)
			next.PrevVersion = cur.Version
		}
	}
	data, err := json.MarshalIndent(&next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(c.path(svc), data, 0o600); err != nil {
		return err
	}
	c.items[svc] = &next
	return nil
}

// RollBack records that failed failed its health gate here. With env
// non-nil, env/version (what the replicas were rolled back to) becomes
// current again; with env nil there was nothing to roll back to and the
// current entry is marked unusable. The one deliberate backwards move.
func (c *centralEnvCache) RollBack(svc string, failed uint64, env []string, version uint64) error {
	return c.mutate(svc, func(e *centralEnvCacheEntry) {
		if env != nil {
			e.Env, e.Version = append([]string(nil), env...), version
			e.PrevEnv, e.PrevVersion = nil, 0
		}
		e.FailedVersion = failed
	})
}

// ClearFailed forgets svc's failed version — an explicit retry.
func (c *centralEnvCache) ClearFailed(svc string) error {
	return c.mutate(svc, func(e *centralEnvCacheEntry) { e.FailedVersion = 0 })
}

func (c *centralEnvCache) mutate(svc string, fn func(*centralEnvCacheEntry)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.items[svc]
	if cur == nil {
		return nil
	}
	next := copyCacheEntry(cur)
	fn(&next)
	data, err := json.MarshalIndent(&next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(c.path(svc), data, 0o600); err != nil {
		return err
	}
	c.items[svc] = &next
	return nil
}

func (c *centralEnvCache) Delete(svc string) error {
	if !validServiceName(svc) {
		return fmt.Errorf("invalid service name %q", svc)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Remove(c.path(svc)); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(c.items, svc)
	return nil
}

func copyCacheEntry(e *centralEnvCacheEntry) centralEnvCacheEntry {
	out := *e
	out.Env = append([]string(nil), e.Env...)
	out.PrevEnv = append([]string(nil), e.PrevEnv...)
	out.Healthcheck = copyHealthcheck(e.Healthcheck)
	return out
}

func copyHealthcheck(h *healthcheckSpec) *healthcheckSpec {
	if h == nil {
		return nil
	}
	cp := *h
	cp.Test = append([]string(nil), h.Test...)
	return &cp
}
