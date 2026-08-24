package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPeerRouteStoreMergeNewHostReportsChanged(t *testing.T) {
	s := newPeerRouteStore(time.Minute)
	payload := peerRoutePayload{
		Peer: "b", Advertise: "http://b:8092",
		Routes: []peerRouteInfo{{Host: "new.example.org", Backends: 2}},
	}
	if changed := s.merge(payload); !changed {
		t.Fatal("merge of a previously-unknown host should report changed=true")
	}
}

// TestPeerRouteStoreOverlayCreatesLearnedOnlyGroup proves a payload for a
// previously-unknown host synthesizes a new learned-only RouteGroup.
func TestPeerRouteStoreOverlayCreatesLearnedOnlyGroup(t *testing.T) {
	s := newPeerRouteStore(time.Minute)
	s.merge(peerRoutePayload{
		Peer: "b", Advertise: "http://b:8092",
		Routes: []peerRouteInfo{{Host: "new.example.org", Backends: 2}},
	})

	groups := s.overlay(nil)
	g := findGroup(groups, "new.example.org", "")
	if g == nil {
		t.Fatal("expected a synthesized group for the learned-only host")
	}
	if len(g.Backends) != 1 || !g.Backends[0].Learned || g.Backends[0].PeerID != "b" {
		t.Fatalf("Backends = %+v, want exactly one learned backend from peer b", g.Backends)
	}
}

// TestPeerRouteStoreOverlayAddsToExistingGroup proves a payload for an
// existing host adds a learned backend alongside the untouched local one.
func TestPeerRouteStoreOverlayAddsToExistingGroup(t *testing.T) {
	s := newPeerRouteStore(time.Minute)
	s.merge(peerRoutePayload{
		Peer: "b", Advertise: "http://b:8092",
		Routes: []peerRouteInfo{{Host: "shared.example.org", Backends: 1}},
	})

	local := &Backend{URL: "http://10.0.0.1:8080"}
	groups := []*RouteGroup{{Host: "shared.example.org", Backends: []*Backend{local}}}
	out := s.overlay(groups)

	g := findGroup(out, "shared.example.org", "")
	if g == nil || len(g.Backends) != 2 {
		t.Fatalf("Backends = %+v, want local + 1 learned", g)
	}
	if g.Backends[0] != local {
		t.Fatal("existing local backend must be untouched (same pointer, unmoved)")
	}
	if g.Backends[1].Learned != true || g.Backends[1].PeerID != "b" {
		t.Fatalf("Backends[1] = %+v, want the learned backend from peer b", g.Backends[1])
	}
}

// TestPeerRouteStoreOverlayIdempotentAcrossFreshSlices proves the
// idempotency property that actually holds: two independent overlay calls
// on two freshly-built groups slices produce the same learned-backend
// count, since assembleGroups() always hands overlay a slice with zero
// pre-existing learned backends.
func TestPeerRouteStoreOverlayIdempotentAcrossFreshSlices(t *testing.T) {
	s := newPeerRouteStore(time.Minute)
	s.merge(peerRoutePayload{
		Peer: "b", Advertise: "http://b:8092",
		Routes: []peerRouteInfo{{Host: "h.example.org", Backends: 1}},
	})

	countLearned := func(groups []*RouteGroup) int {
		n := 0
		for _, g := range groups {
			for _, b := range g.Backends {
				if b.Learned {
					n++
				}
			}
		}
		return n
	}

	c1 := countLearned(s.overlay(nil))
	c2 := countLearned(s.overlay(nil))
	if c1 != 1 || c2 != 1 {
		t.Fatalf("learned backend counts = %d, %d, want 1, 1 (no accumulation across independent freshly-built slices)", c1, c2)
	}
}

// TestPeerRouteStoreOverlayDropsExpiredEntries proves an entry past TTL is
// dropped from the next overlay call and purged from the store itself.
func TestPeerRouteStoreOverlayDropsExpiredEntries(t *testing.T) {
	base := time.Now()
	s := newPeerRouteStore(time.Minute)
	s.now = func() time.Time { return base }
	s.merge(peerRoutePayload{
		Peer: "b", Advertise: "http://b:8092",
		Routes: []peerRouteInfo{{Host: "exp.example.org", Backends: 1}},
	})

	s.now = func() time.Time { return base.Add(2 * time.Minute) }
	groups := s.overlay(nil)
	if g := findGroup(groups, "exp.example.org", ""); g != nil {
		t.Fatalf("expired route should have been dropped, got %+v", g)
	}

	s.mu.Lock()
	_, ok := s.routes[routeKey("exp.example.org", "")]
	s.mu.Unlock()
	if ok {
		t.Fatal("expired key should have been purged from the store, not just skipped")
	}
}

// TestPeerRouteStoreMergeTwiceNoDuplicate proves merging twice for the same
// peer/route updates lastSeen without creating a duplicate entry.
func TestPeerRouteStoreMergeTwiceNoDuplicate(t *testing.T) {
	base := time.Now()
	s := newPeerRouteStore(time.Minute)
	s.now = func() time.Time { return base }
	payload := peerRoutePayload{
		Peer: "b", Advertise: "http://b:8092",
		Routes: []peerRouteInfo{{Host: "dup.example.org", Backends: 1}},
	}
	if changed := s.merge(payload); !changed {
		t.Fatal("first merge should report changed=true")
	}

	s.now = func() time.Time { return base.Add(time.Second) }
	if changed := s.merge(payload); changed {
		t.Fatal("second merge of the same peer/route should report changed=false")
	}

	groups := s.overlay(nil)
	g := findGroup(groups, "dup.example.org", "")
	if g == nil || len(g.Backends) != 1 {
		t.Fatalf("Backends = %+v, want exactly 1 (no duplicate from merging twice)", g)
	}

	s.mu.Lock()
	lr := s.routes[routeKey("dup.example.org", "")]["b"]
	s.mu.Unlock()
	if !lr.lastSeen.Equal(base.Add(time.Second)) {
		t.Fatalf("lastSeen = %v, want updated to %v", lr.lastSeen, base.Add(time.Second))
	}
}

// TestPeerRouteStoreOverlaySynthesizedGroupCarriesRateLimit proves a
// synthesized learned-only group adopts the pushed RateLimit/RateRPM — the
// fix for the double-skip rate-limit bypass: without this, the group
// defaults to RateLimit=false and never charges the shared limiter at all,
// while a peer forwarding a hopped request to us assumes we already did.
func TestPeerRouteStoreOverlaySynthesizedGroupCarriesRateLimit(t *testing.T) {
	s := newPeerRouteStore(time.Minute)
	s.merge(peerRoutePayload{
		Peer: "b", Advertise: "http://b:8092",
		Routes: []peerRouteInfo{{Host: "rl.example.org", Backends: 1, RateLimit: true, RateRPM: 10}},
	})

	groups := s.overlay(nil)
	g := findGroup(groups, "rl.example.org", "")
	if g == nil {
		t.Fatal("expected a synthesized group")
	}
	if !g.RateLimit || g.RateRPM != 10 {
		t.Fatalf("group = %+v, want RateLimit=true RateRPM=10 adopted from the pushed route", g)
	}
}

// TestPeerRouteStoreOverlayExistingGroupKeepsOwnRateLimit proves an
// existing local group's own RateLimit config is left untouched by overlay
// — local config always wins, same as every other label/static merge rule.
func TestPeerRouteStoreOverlayExistingGroupKeepsOwnRateLimit(t *testing.T) {
	s := newPeerRouteStore(time.Minute)
	s.merge(peerRoutePayload{
		Peer: "b", Advertise: "http://b:8092",
		Routes: []peerRouteInfo{{Host: "shared.example.org", Backends: 1, RateLimit: true, RateRPM: 999}},
	})

	local := &Backend{URL: "http://10.0.0.1:8080"}
	groups := []*RouteGroup{{Host: "shared.example.org", Backends: []*Backend{local}, RateLimit: false}}
	out := s.overlay(groups)

	g := findGroup(out, "shared.example.org", "")
	if g == nil || g.RateLimit {
		t.Fatalf("group = %+v, want RateLimit still false (local config, not the peer's, wins)", g)
	}
}

// TestPeerRouteStoreHasExpired proves hasExpired is a cheap, pure in-memory
// signal the periodic ticker in main.go can poll to decide whether the
// expensive, Docker-listing refresh() is actually worth calling: false
// before TTL, true once a fake clock advances past it, and false again once
// a subsequent overlay() call has lazily evicted the stale entry (overlay
// still owns the actual eviction — this must not duplicate or race it).
func TestPeerRouteStoreHasExpired(t *testing.T) {
	base := time.Now()
	s := newPeerRouteStore(time.Minute)
	s.now = func() time.Time { return base }
	s.merge(peerRoutePayload{
		Peer: "b", Advertise: "http://b:8092",
		Routes: []peerRouteInfo{{Host: "exp.example.org", Backends: 1}},
	})

	if s.hasExpired() {
		t.Fatal("hasExpired() = true before TTL, want false")
	}

	s.now = func() time.Time { return base.Add(2 * time.Minute) }
	if !s.hasExpired() {
		t.Fatal("hasExpired() = false after TTL, want true")
	}

	s.overlay(nil)
	if s.hasExpired() {
		t.Fatal("hasExpired() = true after overlay() evicted the stale entry, want false")
	}
}

func TestPeerRoutesHandlerValidSecretMergesAndRefreshes(t *testing.T) {
	store := newPeerRouteStore(time.Minute)
	refreshed := 0
	h := peerRoutesHandler("s3cret", store, func() { refreshed++ })

	body, err := json.Marshal(peerRoutePayload{
		Peer: "b", Advertise: "http://b:8092",
		Routes: []peerRouteInfo{{Host: "x.example.org", Backends: 1}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/peer/routes", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if refreshed != 1 {
		t.Fatalf("refresh called %d time(s), want 1", refreshed)
	}

	groups := store.overlay(nil)
	if findGroup(groups, "x.example.org", "") == nil {
		t.Fatal("merged route should be visible via overlay")
	}
}

// TestPeerRoutesHandlerSteadyStatePushSkipsRefresh proves a repeat push for
// an already-known peer/route does not fire refresh a second time — that's
// what keeps steady-state pushes from Docker-listing on every tick from
// every peer.
func TestPeerRoutesHandlerSteadyStatePushSkipsRefresh(t *testing.T) {
	store := newPeerRouteStore(time.Minute)
	refreshed := 0
	h := peerRoutesHandler("s3cret", store, func() { refreshed++ })

	body, err := json.Marshal(peerRoutePayload{
		Peer: "b", Advertise: "http://b:8092",
		Routes: []peerRouteInfo{{Host: "x.example.org", Backends: 1}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/peer/routes", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("push %d: status = %d, want %d", i, rec.Code, http.StatusNoContent)
		}
	}
	if refreshed != 1 {
		t.Fatalf("refresh called %d time(s) across 2 identical pushes, want 1", refreshed)
	}
}

func TestPeerRoutesHandlerWrongSecret(t *testing.T) {
	store := newPeerRouteStore(time.Minute)
	h := peerRoutesHandler("s3cret", store, func() {})
	req := httptest.NewRequest(http.MethodPost, "/peer/routes", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestPeerRoutesHandlerEmptySecretDisabled(t *testing.T) {
	store := newPeerRouteStore(time.Minute)
	h := peerRoutesHandler("", store, func() {})
	req := httptest.NewRequest(http.MethodPost, "/peer/routes", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestPeerRoutesHandlerWrongMethod(t *testing.T) {
	store := newPeerRouteStore(time.Minute)
	h := peerRoutesHandler("s3cret", store, func() {})
	req := httptest.NewRequest(http.MethodGet, "/peer/routes", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
