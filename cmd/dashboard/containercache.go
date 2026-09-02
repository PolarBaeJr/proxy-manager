package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// containerCache holds ONE unfiltered GET /containers/json?all=true result
// and answers the read-only dashboard handlers from it, applying their
// (simple) Docker filters in-process. It is invalidated by the daemon's
// event stream (watchEvents), by dashboard writes (invalidateAfterWrite —
// belt and braces for the sub-second gap before the event lands), and by a
// hard max-age in case both miss. Concurrent misses are coalesced into a
// single Docker round-trip (list) so a burst of polls costs one fetch.
//
// Only handlers that opt in via withCachedListing ever see it: mutation
// paths, background loops and anything that inspects the result for
// side-effects keep going straight to Docker.
type containerCache struct {
	dc     *dockerClient
	maxAge time.Duration

	// now, reconnectMin/Max and eventHook exist for tests: now lets a test
	// drive expiry without sleeping, the reconnect bounds keep a test's
	// watchEvents loop fast, and eventHook observes each decoded event
	// (after any invalidation it triggered). Defaults: time.Now, 1s, 30s,
	// nil.
	now                        func() time.Time
	reconnectMin, reconnectMax time.Duration
	eventHook                  func(action string, invalidated bool)

	mu        sync.Mutex
	items     []dockerContainer
	fetchedAt time.Time
	valid     bool
	// gen is bumped by every invalidate so a fetch that was already in
	// flight when an event arrived does not get stored as fresh.
	gen      uint64
	inflight *cacheFetch
}

// cacheFetch is one in-flight Docker list that any number of concurrent
// list callers wait on.
type cacheFetch struct {
	done  chan struct{}
	items []dockerContainer
	err   error
}

func newContainerCache(dc *dockerClient, maxAge time.Duration) *containerCache {
	return &containerCache{
		dc:           dc,
		maxAge:       maxAge,
		now:          time.Now,
		reconnectMin: time.Second,
		reconnectMax: 30 * time.Second,
	}
}

// cachedListingKey flags a request context as "served from the container
// cache is fine" — see (*dockerClient).listAll.
type cachedListingKey struct{}

func withCachedListing(ctx context.Context) context.Context {
	return context.WithValue(ctx, cachedListingKey{}, true)
}

func cachedListing(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(cachedListingKey{}).(bool)
	return v
}

// list returns a fresh copy of the cached container list, fetching from
// Docker on a miss. Concurrent misses share one fetch. An error is never
// cached — the next caller retries.
func (c *containerCache) list(ctx context.Context) ([]dockerContainer, error) {
	c.mu.Lock()
	if c.valid && c.now().Sub(c.fetchedAt) < c.maxAge {
		out := append([]dockerContainer(nil), c.items...)
		c.mu.Unlock()
		return out, nil
	}
	if f := c.inflight; f != nil {
		c.mu.Unlock()
		select {
		case <-f.done:
			return append([]dockerContainer(nil), f.items...), f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &cacheFetch{done: make(chan struct{})}
	c.inflight = f
	startGen := c.gen
	c.mu.Unlock()

	// Detached from the caller's context: one poller disconnecting must not
	// fail every coalesced waiter. Bounded by our own timeout instead.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	items, err := c.dc.listAllDirect(fctx, "")

	c.mu.Lock()
	if err == nil {
		c.items = items
		c.fetchedAt = c.now()
		// An invalidate that landed mid-fetch means this snapshot may
		// already be stale; keep it for this round of waiters but don't
		// mark it valid.
		c.valid = c.gen == startGen
	} else {
		c.valid = false
	}
	c.inflight = nil
	c.mu.Unlock()

	f.items, f.err = items, err
	close(f.done)
	return append([]dockerContainer(nil), items...), err
}

func (c *containerCache) invalidate() {
	c.mu.Lock()
	c.valid = false
	c.gen++
	c.mu.Unlock()
}

// parsedFilter is a decoded Docker `filters` query value.
type parsedFilter map[string][]string

// filtered answers a listAll/listRunning call from the cache when the filter
// is one it can apply in-process; ok == false means "not handled, go to
// Docker" and is decided BEFORE touching the cache, so an unsupported
// filter never triggers a fetch.
func (c *containerCache) filtered(ctx context.Context, filter string, runningOnly bool) ([]dockerContainer, bool, error) {
	f := parsedFilter{}
	if filter != "" {
		if err := json.Unmarshal([]byte(filter), &f); err != nil {
			return nil, false, nil
		}
	}
	// Deliberately narrower than applyContainerFilter can handle: the read
	// path only ever sends a single label OR name key, so anything else is
	// treated as unsupported rather than trusting an untested combination.
	if len(f) > 1 {
		return nil, false, nil
	}
	for k := range f {
		if k != "label" && k != "name" {
			return nil, false, nil
		}
	}
	items, err := c.list(ctx)
	if err != nil {
		return nil, true, err
	}
	return applyContainerFilter(items, f, runningOnly), true, nil
}

// applyContainerFilter mirrors the subset of Docker's container filters the
// read path uses. Always returns a NEW slice — callers on the mutation
// paths sort what they get, and the cache's own slice must never leak.
//
//   - label: multiple values AND together. "k=v" needs Labels[k] == v, bare
//     "k" needs the key to exist.
//   - name: multiple values OR together. Docker treats each value as an
//     unanchored regex against every entry of Names (leading "/" kept); the
//     read path only ever passes literal prefixes, so a substring test is
//     equivalent (a "." in a service name is the one divergence, and it
//     would only over-match).
func applyContainerFilter(items []dockerContainer, f parsedFilter, runningOnly bool) []dockerContainer {
	out := make([]dockerContainer, 0, len(items))
	for _, ct := range items {
		if runningOnly && ct.State != "running" {
			continue
		}
		if !matchLabelFilter(ct, f["label"]) || !matchNameFilter(ct, f["name"]) {
			continue
		}
		out = append(out, ct)
	}
	return out
}

func matchLabelFilter(ct dockerContainer, values []string) bool {
	for _, v := range values {
		k, want, hasEq := strings.Cut(v, "=")
		got, ok := ct.Labels[k]
		if !ok || (hasEq && got != want) {
			return false
		}
	}
	return true
}

func matchNameFilter(ct dockerContainer, values []string) bool {
	if len(values) == 0 {
		return true
	}
	for _, v := range values {
		for _, n := range ct.Names {
			if strings.Contains(n, v) {
				return true
			}
		}
	}
	return false
}

// cacheInvalidatingActions is the exact set of container event actions that
// can change what /containers/json returns. Exact match (plus the
// "health_status: ..." prefix) means exec_create/exec_start/exec_die/
// exec_detach/top/attach/copy/export — which fire constantly and change
// nothing we list — never invalidate.
var cacheInvalidatingActions = map[string]bool{
	"create": true, "start": true, "stop": true, "die": true, "destroy": true,
	"rename": true, "restart": true, "kill": true, "pause": true, "unpause": true,
	"update": true, "oom": true,
}

func (c *containerCache) handleEvent(action string) {
	invalidated := cacheInvalidatingActions[action] || strings.HasPrefix(action, "health_status")
	if invalidated {
		c.invalidate()
	}
	if c.eventHook != nil {
		c.eventHook(action, invalidated)
	}
}

// watchEvents follows the daemon's container event stream and invalidates
// the cache on anything that changes the list. Same shape as cmd/proxy's
// streamEvents; the differences are the exponential backoff and that every
// (re)connect invalidates once, since events were missed while we were
// away. Returns when ctx is done.
func (c *containerCache) watchEvents(ctx context.Context) {
	backoff := c.reconnectMin
	loggedOutage := false
	wait := func() {
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > c.reconnectMax {
			backoff = c.reconnectMax
		}
	}
	for ctx.Err() == nil {
		body, err := c.dc.get(ctx, "/events?filters="+url.QueryEscape(`{"type":["container"]}`))
		if err != nil {
			c.invalidate()
			if !loggedOutage && ctx.Err() == nil {
				log.Printf("container cache: event stream open: %v — retrying with backoff", err)
				loggedOutage = true
			}
			wait()
			continue
		}
		backoff = c.reconnectMin
		loggedOutage = false
		c.invalidate()
		dec := json.NewDecoder(body)
		for {
			var ev struct{ Type, Action string }
			if err := dec.Decode(&ev); err != nil {
				body.Close()
				c.invalidate()
				if ctx.Err() == nil {
					log.Printf("container cache: event stream: %v — reconnecting", err)
				}
				break
			}
			c.handleEvent(ev.Action)
		}
		wait()
	}
}

// invalidateContainerCache is the nil-safe hook for write paths —
// newDashboardMux is built with dc == nil in tests, and the cache is nil
// until main.go attaches it.
func (c *dockerClient) invalidateContainerCache() {
	if c == nil || c.cache == nil {
		return
	}
	c.cache.invalidate()
}

// invalidateAfterWrite drops the container cache after any non-read request
// handled by next. Belt and braces: the events stream is the primary
// invalidation, this closes the sub-second window between a dashboard write
// returning and the daemon's event arriving, so the UI's immediate re-poll
// sees the change. Deferred so it runs even if next panics or bails early.
func invalidateAfterWrite(dc *dockerClient, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			defer dc.invalidateContainerCache()
		}
		next.ServeHTTP(w, req)
	}
}
