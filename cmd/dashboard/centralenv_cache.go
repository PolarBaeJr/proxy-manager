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
}

// errCentralEnvCacheOlder is Put refusing to move a cached entry backwards.
type errCentralEnvCacheOlder struct {
	Service         string
	Cached, Offered uint64
}

func (e errCentralEnvCacheOlder) Error() string {
	return fmt.Sprintf("central env for %q: offered version %d is older than cached version %d", e.Service, e.Offered, e.Cached)
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

// Put records env as svc's current central env. An older version than the
// one cached is refused (a delayed response must not roll replicas back); a
// newer one rotates the current entry into prev_*; the same version just
// refreshes it.
func (c *centralEnvCache) Put(svc, origin string, version uint64, env []string) error {
	if !validServiceName(svc) {
		return fmt.Errorf("invalid service name %q", svc)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := centralEnvCacheEntry{
		Service:   svc,
		Origin:    origin,
		Version:   version,
		Env:       append([]string(nil), env...),
		FetchedAt: time.Now().Unix(),
	}
	if cur := c.items[svc]; cur != nil {
		if version < cur.Version {
			return errCentralEnvCacheOlder{Service: svc, Cached: cur.Version, Offered: version}
		}
		if version == cur.Version {
			next.PrevEnv = append([]string(nil), cur.PrevEnv...)
			next.PrevVersion = cur.PrevVersion
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
	return out
}
