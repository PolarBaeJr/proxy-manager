package main

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// learnedRoute is one peer's advertisement of one route (host+path), kept
// until it ages out. Overlaid onto freshly-assembled route groups on every
// refresh() — NOT mutated directly into a live *RouteGroup.Backends slice,
// because router.go's assembleGroups()/router.Set() rebuild groups from
// scratch on every refresh and would silently wipe any backend appended in
// place.
type learnedRoute struct {
	peerID      string
	advertise   string
	name        string
	stripPrefix bool
	rateLimit   bool
	rateRPM     int
	lastSeen    time.Time
}

// PeerRouteStore is the durable, receiving-side record of routes pushed by
// peers. Keyed by "host|path" -> peerID -> learnedRoute, so multiple peers
// can independently advertise the same route without clobbering each other.
// A route a peer stops advertising ages out via ttl in overlay(), not
// diff-deleted on the next push.
type PeerRouteStore struct {
	mu     sync.Mutex
	routes map[string]map[string]learnedRoute
	ttl    time.Duration
	now    func() time.Time // injectable for tests
}

func newPeerRouteStore(ttl time.Duration) *PeerRouteStore {
	return &PeerRouteStore{
		routes: map[string]map[string]learnedRoute{},
		ttl:    ttl,
		now:    time.Now,
	}
}

func routeKey(host, path string) string { return host + "|" + path }

func splitRouteKey(key string) (host, path string) {
	parts := strings.SplitN(key, "|", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

// merge upserts every advertised route (with Backends > 0) into the store,
// keyed by payload.Peer under its host|path. Returns true only when this
// merge introduced a new host|path key or a new peer under an existing
// key — a bare lastSeen refresh for an already-known peer/route returns
// false, so the caller (peerRoutesHandler) can skip an unnecessary refresh()
// on steady-state pushes and rely on the periodic resync ticker for TTL
// eviction instead.
func (s *PeerRouteStore) merge(payload peerRoutePayload) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now
	if now == nil {
		now = time.Now
	}
	changed := false
	for _, r := range payload.Routes {
		if r.Backends <= 0 {
			continue
		}
		key := routeKey(r.Host, r.PathPrefix)
		peersForKey, ok := s.routes[key]
		if !ok {
			peersForKey = map[string]learnedRoute{}
			s.routes[key] = peersForKey
			changed = true
		}
		if _, ok := peersForKey[payload.Peer]; !ok {
			changed = true
		}
		peersForKey[payload.Peer] = learnedRoute{
			peerID:      payload.Peer,
			advertise:   payload.Advertise,
			name:        r.Name,
			stripPrefix: r.StripPrefix,
			rateLimit:   r.RateLimit,
			rateRPM:     r.RateRPM,
			lastSeen:    now(),
		}
	}
	return changed
}

// overlay appends a synthetic learned backend (via makePeerBackend) for
// every stored, non-expired peer route onto groups — either onto an existing
// group matching the same host+path, or a newly synthesized group appended
// to the slice if no local group exists for that route. Expired entries are
// dropped lazily while iterating; there is no background sweep goroutine.
//
// Because assembleGroups() runs fresh every refresh(), the groups slice
// passed in always starts with zero pre-existing learned backends — so two
// independent calls with two freshly-built slices produce the same
// learned-backend count, rather than accumulating duplicates the way
// repeated calls against the same slice would.
func (s *PeerRouteStore) overlay(groups []*RouteGroup) []*RouteGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now
	if now == nil {
		now = time.Now
	}

	byKey := map[string]*RouteGroup{}
	for _, g := range groups {
		byKey[routeKey(g.Host, g.PathPrefix)] = g
	}

	for key, peersForKey := range s.routes {
		host, path := splitRouteKey(key)
		for peerID, lr := range peersForKey {
			if now().Sub(lr.lastSeen) > s.ttl {
				delete(peersForKey, peerID)
				continue
			}
			b := makePeerBackend(lr.advertise, host, path, lr.stripPrefix, peerID)
			if b == nil {
				continue
			}
			g, ok := byKey[key]
			if !ok {
				// RateLimit/RateRPM are only adopted here, when synthesizing a
				// brand-new (learned-only) group with no local knowledge of
				// its own — an existing local group's own RateLimit config
				// always wins (unchanged, same as every other label/static
				// merge rule in this codebase). Without this, a learned-only
				// group here would default to RateLimit=false and never
				// charge the shared limiter for a request routed directly
				// into it.
				g = &RouteGroup{
					Host: host, PathPrefix: path, StripPrefix: lr.stripPrefix, Name: lr.name,
					RateLimit: lr.rateLimit, RateRPM: lr.rateRPM,
				}
				byKey[key] = g
				groups = append(groups, g)
			}
			g.Backends = append(g.Backends, b)
		}
		if len(peersForKey) == 0 {
			delete(s.routes, key)
		}
	}
	return groups
}

// hasExpired reports whether any stored entry is currently past its TTL,
// without mutating the store or touching Docker — a cheap, in-memory,
// mutex-guarded check the caller can run on every tick of a cheap ticker to
// decide whether the expensive, Docker-listing refresh() is actually worth
// calling. The predicate mirrors overlay()'s own expiry check exactly
// (same now() with the same nil fallback, same strict ">") so the two can
// never disagree — if hasExpired() said true, overlay() must evict on the
// very next call, and if it said false, overlay() must not have anything to
// evict either. Eviction itself stays owned by overlay()'s existing
// lazy-delete-on-read; this is read-only.
func (s *PeerRouteStore) hasExpired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now
	if now == nil {
		now = time.Now
	}
	for _, peersForKey := range s.routes {
		for _, lr := range peersForKey {
			if now().Sub(lr.lastSeen) > s.ttl {
				return true
			}
		}
	}
	return false
}

// peerRoutesHandler returns the HTTP handler for POST /peer/routes on the
// internal metrics port. Same bearer-auth shape as peerHandshakeHandler in
// peers.go: empty secret disables the endpoint (404), wrong method 405s,
// constant-time compare on the bearer token. A valid push is merged into
// store, and — only when the merge actually changed something (a new route
// or a new peer for an existing route) — refresh is invoked so the new
// route becomes live immediately, at the same latency as a local Docker
// event, rather than waiting for the next periodic resync tick.
func peerRoutesHandler(secret string, store *PeerRouteStore, refresh func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		want := []byte("Bearer " + secret)
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var payload peerRoutePayload
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if store.merge(payload) && refresh != nil {
			refresh()
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
