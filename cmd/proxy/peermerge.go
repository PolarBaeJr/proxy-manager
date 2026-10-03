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
	spread      bool
	// service is the advertising peer's RouteGroup.Service — see overlay's
	// localBackendsByService parameter for what it's used for.
	service  string
	backends int
	weight   int
	// abID/bBackends/bWeight are the B share of backends/weight (which stay
	// totals), validated in merge; zero when the peer runs no test on this
	// route or predates A/B in the mesh.
	abID      string
	bBackends int
	bWeight   int
	lastSeen  time.Time
}

// peerWeight resolves the weight to give a peer's synthetic backend: the
// summed proxy.weight the peer advertised, or its plain backend count when
// it advertised no weight at all (a binary predating the field).
func peerWeight(r peerRouteInfo) int {
	if r.Weight > 0 {
		return r.Weight
	}
	return r.Backends
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
	// hopAuth is sent on every hop to a peer (see peerHopAuthToken); set
	// once by main before the first overlay.
	hopAuth string
	// exps holds each peer's validated A/B experiments by service, and
	// expSeen when they last arrived (same TTL as routes). abApply, set once
	// by main, hands a peer's experiments to Router.applyPeerAB.
	exps    map[string]map[string]*abPeerExp
	expSeen map[string]time.Time
	abApply func(peer string, exps map[string]*abPeerExp)
}

func newPeerRouteStore(ttl time.Duration) *PeerRouteStore {
	return &PeerRouteStore{
		routes:  map[string]map[string]learnedRoute{},
		ttl:     ttl,
		now:     time.Now,
		exps:    map[string]map[string]*abPeerExp{},
		expSeen: map[string]time.Time{},
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
// merge introduced a new host|path key, a new peer under an existing key, or
// a changed spread/weight or A/B state (see the experiments block and
// peerRouteAB) — a bare lastSeen refresh for an already-known
// peer/route returns false, so the caller (peerRoutesHandler) can skip an
// unnecessary refresh()
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
	// Experiments are replaced wholesale per peer; any change routing
	// depends on (a test appearing or going, its config, phase, latch or
	// B ownership) refreshes. Stats-only changes do not.
	exps := validatePeerExperiments(payload.Peer, payload.Experiments, now())
	prevExps := s.exps[payload.Peer]
	if len(prevExps) != len(exps) {
		changed = true
	}
	for svc, e := range exps {
		if p, ok := prevExps[svc]; !ok || p.fingerprint() != e.fingerprint() {
			changed = true
		}
	}
	if len(exps) > 0 {
		s.exps[payload.Peer] = exps
		s.expSeen[payload.Peer] = now()
	} else {
		delete(s.exps, payload.Peer)
		delete(s.expSeen, payload.Peer)
	}
	for _, r := range payload.Routes {
		if r.Backends <= 0 {
			continue
		}
		abID, bBackends, bWeight := peerRouteAB(r)
		key := routeKey(r.Host, r.PathPrefix)
		peersForKey, ok := s.routes[key]
		if !ok {
			peersForKey = map[string]learnedRoute{}
			s.routes[key] = peersForKey
			changed = true
		}
		// A flipped spread flag or an edited weight counts as a change even
		// though the peer and route are both already known: unlike the other
		// advertised fields these two decide how traffic is split, and the
		// periodic tick below only calls refresh() when something has
		// EXPIRED. Without this, turning spread off (or retuning the weight)
		// on the peer would sit unapplied here until some unrelated Docker
		// event happened to trigger a rebuild.
		// The B split is the same kind of field; with one, the total count
		// matters too (an A share dropping to zero removes the A backend).
		if prev, ok := peersForKey[payload.Peer]; !ok || prev.spread != r.Spread || prev.weight != peerWeight(r) ||
			prev.abID != abID || prev.bBackends != bBackends || prev.bWeight != bWeight ||
			(abID != "" && prev.backends != r.Backends) {
			changed = true
		}
		peersForKey[payload.Peer] = learnedRoute{
			peerID:      payload.Peer,
			advertise:   payload.Advertise,
			name:        r.Name,
			stripPrefix: r.StripPrefix,
			rateLimit:   r.RateLimit,
			rateRPM:     r.RateRPM,
			spread:      r.Spread,
			service:     r.Service,
			backends:    r.Backends,
			// A peer running a binary from before proxy.weight was advertised
			// sends no weight at all. Falling back to the count keeps its
			// share of the pool exactly what it was, instead of letting
			// makePeerBackend's floor collapse a 3-replica peer to weight 1
			// for the length of a rolling deploy.
			weight:    peerWeight(r),
			abID:      abID,
			bBackends: bBackends,
			bWeight:   bWeight,
			lastSeen:  now(),
		}
	}
	return changed
}

// peerRouteAB validates a route's B fields: a well-formed id, a B count
// within the total, and a B weight within the total weight (defaulting to
// the B count, as peerWeight does for the total). Anything else means "no
// B here", which overlay turns into the legacy single backend.
func peerRouteAB(r peerRouteInfo) (string, int, int) {
	if r.ABID == "" || !abIDRe.MatchString(r.ABID) || r.BBackends <= 0 || r.BBackends > r.Backends || r.BWeight < 0 {
		return "", 0, 0
	}
	bw := r.BWeight
	if bw == 0 {
		bw = r.BBackends
	}
	if bw > peerWeight(r) {
		return "", 0, 0
	}
	return r.ABID, r.BBackends, bw
}

// applyAB hands a peer's current experiments to the router (abApply). The
// handler calls it on every push, after any refresh the push caused, so a
// newly adopted test already has a run to write into.
func (s *PeerRouteStore) applyAB(peer string) {
	s.mu.Lock()
	apply := s.abApply
	exps := map[string]*abPeerExp{}
	for svc, e := range s.exps[peer] {
		exps[svc] = e
	}
	s.mu.Unlock()
	if apply != nil {
		apply(peer, exps)
	}
}

// overlay appends a synthetic learned backend (via makePeerBackend) for
// every stored, non-expired peer route onto groups — either onto an existing
// group matching the same host+path, or a newly synthesized group appended
// to the slice if no local group exists for that route. Expired entries are
// dropped lazily while iterating; there is no background sweep goroutine.
//
// localBackendsByService is this host's OWN backendsByService map, as
// returned by assembleGroups() in the same refresh() cycle — used to backfill
// a brand-new synthesized group from this host's own Service-labeled
// containers, exactly the way assembleGroups' own static-route backfill
// (router.go: `if g.static && g.Service != ""`) does for a route this host
// already knows about via routes.json. Without this, a `service:`-resolved
// routes.json entry that exists on only ONE host is invisible everywhere
// else: a peer running the same proxy.service-labeled containers has no
// local group for the route at all (no routes.json entry, no proxy.host/path
// label on those containers), so its own healthy replicas sit unused while
// every request routes back over the network to the advertising peer — and,
// since a group with zero local backends is never re-advertised outward
// (see peersync.go's tick() anti-recycling filter), the origin can't fail
// over to THIS peer either. Deliberately scoped to the brand-new-group
// branch only (never an already-existing local group): a directly-labeled
// local container is already in that group's Backends by the time overlay
// runs, and is also already counted in localBackendsByService under the
// same service name — backfilling onto an existing group would append the
// same *Backend twice. See project memory
// project_routes_json_service_backend_gap.md for how this was found.
//
// Because assembleGroups() runs fresh every refresh(), the groups slice
// passed in always starts with zero pre-existing learned backends — so two
// independent calls with two freshly-built slices produce the same
// learned-backend count, rather than accumulating duplicates the way
// repeated calls against the same slice would.
func (s *PeerRouteStore) overlay(groups []*RouteGroup, localBackendsByService map[string][]*Backend) []*RouteGroup {
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
	for peer, seen := range s.expSeen {
		if now().Sub(seen) > s.ttl {
			delete(s.exps, peer)
			delete(s.expSeen, peer)
		}
	}
	testIDs, adopt := s.abTestIDs(groups)

	for key, peersForKey := range s.routes {
		host, path := splitRouteKey(key)
		for peerID, lr := range peersForKey {
			if now().Sub(lr.lastSeen) > s.ttl {
				delete(peersForKey, peerID)
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
					RateLimit: lr.rateLimit, RateRPM: lr.rateRPM, Service: lr.service,
				}
				// Real, local, non-Learned backends -- pickHealthy already prefers
				// these over the synthetic peer backend added unconditionally
				// below, without any change needed there: it's the same
				// local-tier preference every other local backend already gets.
				if lr.service != "" {
					if bs, ok := localBackendsByService[lr.service]; ok {
						g.Backends = append(g.Backends, bs...)
					}
				}
				byKey[key] = g
				groups = append(groups, g)
			}
			testID := testIDs[g.Service]
			bs := s.peerBackendsFor(lr, host, path, s.abSplitFor(g, testID, lr))
			if len(bs) == 0 {
				continue
			}
			if e := s.exps[peerID][g.Service]; testID != "" && !g.static && e != nil && e.id == testID {
				for _, b := range bs {
					b.peerTestID = testID
				}
			}
			// Spread is the one advertised field adopted onto an EXISTING
			// local group, unlike RateLimit/RateRPM above — it has to be, or
			// the cross-host scale that set proxy.spread on the peer's
			// replicas could never reach the origin, whose own containers
			// deliberately keep their labels untouched. Adoption is one-way
			// (see peersync.go: we advertise SpreadLocal, never this) and
			// non-sticky — overlay runs on a freshly assembled group set every
			// refresh, so a peer that stops advertising spread clears it here
			// on the next one, without waiting for TTL expiry.
			if lr.spread {
				g.Spread = true
			}
			g.Backends = append(g.Backends, bs...)
		}
		if len(peersForKey) == 0 {
			delete(s.routes, key)
		}
	}
	abAttach(groups, adopt)
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
	for _, seen := range s.expSeen {
		if now().Sub(seen) > s.ttl {
			return true
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
		// The body limit bounds everything a peer can make us decode; a body
		// over it is truncated and fails to decode, so it is rejected whole.
		var payload peerRoutePayload
		if err := json.NewDecoder(io.LimitReader(r.Body, peerPayloadMaxBytes)).Decode(&payload); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if store.merge(payload) && refresh != nil {
			refresh()
		}
		store.applyAB(payload.Peer)
		w.WriteHeader(http.StatusNoContent)
	})
}
