package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// spreadTestGroup is a route with one local backend and one learned (peer)
// backend, both healthy — the exact shape a cross-host scale produces on the
// origin host once the peer advertises its replicas back.
func spreadTestGroup(t *testing.T, spread bool, peerWeight int) *RouteGroup {
	t.Helper()
	local := makeBackendForTest("http://172.26.0.5:8080")
	peer := makePeerBackend("http://100.83.62.68:8092", "app.example.org", "", false, "peer-b", peerWeight)
	if peer == nil {
		t.Fatal("makePeerBackend returned nil")
	}
	local.markHealthy(true)
	peer.markHealthy(true)
	return &RouteGroup{Host: "app.example.org", Backends: []*Backend{local, peer}, Spread: spread}
}

func makeBackendForTest(raw string) *Backend {
	return &Backend{URL: raw, Weight: 1, Container: "app"}
}

// pickCounts runs pickHealthy n times and tallies which backends came back.
func pickCounts(g *RouteGroup, n int, allowPeer bool) map[string]int {
	out := map[string]int{}
	for i := 0; i < n; i++ {
		b := g.pickHealthy(nil, allowPeer)
		if b == nil {
			out["<nil>"]++
			continue
		}
		out[b.Container]++
	}
	return out
}

// TestSpreadBalancesAcrossLocalAndPeer is the behavior the whole cross-host
// scale feature rests on: with Spread set, a healthy peer backend is a member
// of the same pool as the local one, not a reserve tier.
func TestSpreadBalancesAcrossLocalAndPeer(t *testing.T) {
	g := spreadTestGroup(t, true, 1)
	counts := pickCounts(g, 100, true)
	if counts["app"] == 0 || counts["peer:peer-b"] == 0 {
		t.Fatalf("counts = %v, want both the local and the peer backend selected", counts)
	}
	if counts["app"] != counts["peer:peer-b"] {
		t.Errorf("counts = %v, want an even split at equal weight", counts)
	}
}

// TestNonSpreadKeepsPeerAsFailoverOnly is the regression guard for every
// route that did NOT opt in: default behavior must be byte-for-byte what it
// was — a healthy local backend always wins and the peer is never touched.
func TestNonSpreadKeepsPeerAsFailoverOnly(t *testing.T) {
	g := spreadTestGroup(t, false, 1)
	counts := pickCounts(g, 50, true)
	if counts["peer:peer-b"] != 0 {
		t.Fatalf("counts = %v, want the peer never selected while a local backend is healthy", counts)
	}
	if counts["app"] != 50 {
		t.Errorf("counts = %v, want every pick to be local", counts)
	}
}

// TestSpreadFallsBackToPeerWhenLocalIsDown proves opting into spread does not
// cost the failover behavior it generalizes.
func TestSpreadFallsBackToPeerWhenLocalIsDown(t *testing.T) {
	g := spreadTestGroup(t, true, 1)
	g.Backends[0].markHealthy(false)
	counts := pickCounts(g, 20, true)
	if counts["peer:peer-b"] != 20 {
		t.Errorf("counts = %v, want every pick to fall through to the peer", counts)
	}
}

// TestSpreadIsLocalOnlyForAlreadyHoppedRequest is the loop guard: a request a
// peer already forwarded once (allowPeer=false) must never be handed to a
// learned backend, spread or not, or two spread proxies would bounce it
// between each other.
func TestSpreadIsLocalOnlyForAlreadyHoppedRequest(t *testing.T) {
	g := spreadTestGroup(t, true, 1)
	counts := pickCounts(g, 20, false)
	if counts["peer:peer-b"] != 0 {
		t.Fatalf("counts = %v, want no peer selection for a hopped request", counts)
	}

	g.Backends[0].markHealthy(false)
	if b := g.pickHealthy(nil, false); b != nil {
		t.Errorf("pick = %v, want nil (503) rather than re-forwarding a hopped request", b.Container)
	}
}

// TestSpreadWeightsPeerByAdvertisedReplicaCount proves spread balances per
// REPLICA, not per proxy: a peer running three replicas takes three times the
// share of a single local one, which is what "load balancing across devices"
// has to mean once the replica counts differ.
func TestSpreadWeightsPeerByAdvertisedReplicaCount(t *testing.T) {
	g := spreadTestGroup(t, true, 3)
	counts := pickCounts(g, 100, true)
	if counts["peer:peer-b"] != 75 || counts["app"] != 25 {
		t.Errorf("counts = %v, want a 3:1 split toward the peer's three replicas", counts)
	}
}

// TestAssembleGroupsSpreadLabel covers the label side: one replica carrying
// proxy.spread opts the whole group in (the cross-host scale only ever labels
// the replicas it places, never the originals it found), and a group with no
// such replica stays failover-only.
func TestAssembleGroupsSpreadLabel(t *testing.T) {
	dc := fakeDocker(t, dockerJSON(
		container("a", "app-a", "running",
			map[string]string{labelHost: "a.example.org", labelPort: "8080", labelService: "app"},
			map[string]string{managedNetwork: "172.20.0.5"}),
		container("b", "goproxy-app-1", "running",
			map[string]string{labelHost: "a.example.org", labelPort: "8080", labelService: "app", labelSpread: "true"},
			map[string]string{managedNetwork: "172.20.0.6"}),
		container("c", "other", "running",
			map[string]string{labelHost: "c.example.org", labelPort: "8080"},
			map[string]string{managedNetwork: "172.20.0.7"}),
	))

	groups, _, err := assembleGroups(context.Background(), dc, "")
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	// SpreadLocal too, not just Spread: the label is what this host advertises,
	// and only the locally-derived half may go on the wire.
	if g := findGroup(groups, "a.example.org", ""); g == nil || !g.Spread || !g.SpreadLocal {
		t.Errorf("a group = %+v, want Spread and SpreadLocal from the one labeled replica", g)
	}
	if g := findGroup(groups, "c.example.org", ""); g == nil || g.Spread {
		t.Errorf("c group = %+v, want Spread off — nothing opted it in", g)
	}
}

// TestPeerRouteStoreAdoptsSpreadOntoExistingGroup covers the wire path the
// dashboard's cross-host scale actually uses: the ORIGIN's containers are
// never relabeled, so the only way its group learns to spread is by adopting
// the flag from the peer's advertisement. The advertised backend count must
// come through as the synthetic backend's weight at the same time.
func TestPeerRouteStoreAdoptsSpreadOntoExistingGroup(t *testing.T) {
	s := newPeerRouteStore(time.Minute)
	s.merge(peerRoutePayload{
		Peer:      "peer-b",
		Advertise: "http://100.83.62.68:8092",
		Routes: []peerRouteInfo{{
			Host: "app.example.org", Backends: 2, Spread: true,
		}},
	})

	local := makeBackendForTest("http://172.26.0.5:8080")
	g := &RouteGroup{Host: "app.example.org", Backends: []*Backend{local}}
	groups := s.overlay([]*RouteGroup{g}, nil)

	if len(groups) != 1 {
		t.Fatalf("groups = %d, want the learned route merged into the existing one", len(groups))
	}
	if !groups[0].Spread {
		t.Error("Spread not adopted — the origin would keep the peer's replicas as an unused failover tier")
	}
	if len(groups[0].Backends) != 2 {
		t.Fatalf("backends = %d, want the local one plus the synthetic peer backend", len(groups[0].Backends))
	}
	peer := groups[0].Backends[1]
	if !peer.Learned || peer.Weight != 2 {
		t.Errorf("peer backend learned=%v weight=%d, want learned with weight 2", peer.Learned, peer.Weight)
	}
}

// TestPeerRouteStoreLeavesSpreadOffWhenNotAdvertised pins the default: an
// ordinary peer advertisement must not silently turn an existing route into a
// load-balanced one.
func TestPeerRouteStoreLeavesSpreadOffWhenNotAdvertised(t *testing.T) {
	s := newPeerRouteStore(time.Minute)
	s.merge(peerRoutePayload{
		Peer:      "peer-b",
		Advertise: "http://100.83.62.68:8092",
		Routes:    []peerRouteInfo{{Host: "app.example.org", Backends: 1}},
	})
	g := &RouteGroup{Host: "app.example.org", Backends: []*Backend{makeBackendForTest("http://172.26.0.5:8080")}}
	groups := s.overlay([]*RouteGroup{g}, nil)
	if groups[0].Spread {
		t.Error("Spread turned on by an advertisement that never claimed it")
	}
}

// TestPeerSyncAdvertisesOnlyLocalSpread pins the anti-latch rule: a host
// advertises the spread it derives from its OWN labels and never the spread it
// adopted from a peer. Re-advertising the adopted value would make the flag
// self-sustaining — each host adopting its own claim back through the other,
// with no way to clear it while the route kept being advertised.
func TestPeerSyncAdvertisesOnlyLocalSpread(t *testing.T) {
	adopted := &RouteGroup{
		Host:     "adopted.example.org",
		Backends: []*Backend{makeBackendForTest("http://172.26.0.5:8080")},
		Spread:   true, // adopted from a peer; SpreadLocal deliberately false
	}
	routes := advertisedRoutes(t, adopted)
	if routes[0].Spread {
		t.Error("re-advertised a spread flag this host only adopted — that latches the flag on permanently")
	}
}

// TestPeerSyncAdvertisesSpread covers the other half: a host that does carry
// proxy.spread on its own containers must advertise it, or the peer would
// never join the pool.
func TestPeerSyncAdvertisesSpread(t *testing.T) {
	local := makeBackendForTest("http://172.26.0.5:8080")
	learned := makePeerBackend("http://100.1.1.1:8092", "app.example.org", "", false, "peer-c", 1)
	routes := advertisedRoutes(t, &RouteGroup{
		Host:     "app.example.org",
		Backends: []*Backend{local, learned},
		Spread:   true, SpreadLocal: true,
	})
	if !routes[0].Spread {
		t.Error("Spread not advertised")
	}
	if routes[0].Backends != 1 {
		t.Errorf("advertised backends = %d, want 1 — a learned backend is not local capacity", routes[0].Backends)
	}
}

// advertisedRoutes runs one real PeerSync tick against a stub peer and returns
// the routes it put on the wire.
func advertisedRoutes(t *testing.T, groups ...*RouteGroup) []peerRouteInfo {
	t.Helper()
	bodyCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- b
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := &Router{}
	r.Set(groups)
	newPeerSync(r, []string{srv.URL}, "s3cret", "peer-a", "http://100.0.0.1:8092", 0).tick(context.Background())

	var body []byte
	select {
	case body = <-bodyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tick to POST to the peer")
	}
	var payload peerRoutePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Routes) != len(groups) {
		t.Fatalf("Routes = %+v, want %d", payload.Routes, len(groups))
	}
	return payload.Routes
}

// TestPeerRouteStoreClearsAdoptedSpread is the off-switch: once the peer stops
// advertising spread (its replicas lost the label), the route must go back to
// failover-only WITHOUT waiting for TTL expiry — the route itself is still
// being advertised, so TTL never fires and would never clear it.
func TestPeerRouteStoreClearsAdoptedSpread(t *testing.T) {
	s := newPeerRouteStore(time.Minute)
	adv := func(spread bool) peerRoutePayload {
		return peerRoutePayload{
			Peer:      "peer-b",
			Advertise: "http://100.83.62.68:8092",
			Routes:    []peerRouteInfo{{Host: "app.example.org", Backends: 2, Spread: spread}},
		}
	}
	// Rebuilt each time, mirroring main.go's refresh(): assembleGroups then
	// overlay, so an adopted flag never survives on its own.
	fresh := func() *RouteGroup {
		return &RouteGroup{Host: "app.example.org", Backends: []*Backend{makeBackendForTest("http://172.26.0.5:8080")}}
	}

	s.merge(adv(true))
	if !s.overlay([]*RouteGroup{fresh()}, nil)[0].Spread {
		t.Fatal("Spread not adopted on the first advertisement")
	}

	if !s.merge(adv(false)) {
		t.Error("merge reported no change on a flipped spread flag — refresh() would not run, leaving the stale flag live")
	}
	if s.overlay([]*RouteGroup{fresh()}, nil)[0].Spread {
		t.Error("Spread still on after the peer stopped advertising it — the flag has no off-switch")
	}
}

// TestPeerSyncAdvertisesSummedWeight proves the wire carries the SUM of the
// local backends' proxy.weight, not a replica count — that sum is what gives
// the receiving side's single synthetic backend the right share. Backends
// stays a plain count alongside it, and the two are meant to diverge as soon
// as an operator edits the label.
func TestPeerSyncAdvertisesSummedWeight(t *testing.T) {
	heavy := makeBackendForTest("http://172.26.0.5:8080")
	heavy.Weight = 3
	light := makeBackendForTest("http://172.26.0.6:8080")
	light.Weight = 1
	// A learned backend must contribute to neither number: it is another
	// host's capacity, and counting it would inflate our own advertisement.
	learned := makePeerBackend("http://100.1.1.1:8092", "app.example.org", "", false, "peer-c", 9)

	routes := advertisedRoutes(t, &RouteGroup{
		Host:     "app.example.org",
		Backends: []*Backend{heavy, light, learned},
	})
	if routes[0].Weight != 4 {
		t.Errorf("Weight = %d, want 4 (3+1 local, learned excluded)", routes[0].Weight)
	}
	if routes[0].Backends != 2 {
		t.Errorf("Backends = %d, want 2 — the count must stay a count", routes[0].Backends)
	}
}

// TestOverlayWeightsPeerByAdvertisedWeight is the end of the same path: an
// advertised weight of 4 against one local backend of weight 1 must actually
// come out of pickHealthy as a 4:1 split, which is the whole point of letting
// an operator edit proxy.weight from the dashboard.
func TestOverlayWeightsPeerByAdvertisedWeight(t *testing.T) {
	s := newPeerRouteStore(time.Minute)
	s.merge(peerRoutePayload{
		Peer:      "peer-b",
		Advertise: "http://100.83.62.68:8092",
		Routes:    []peerRouteInfo{{Host: "app.example.org", Backends: 2, Weight: 4, Spread: true}},
	})

	local := makeBackendForTest("http://172.26.0.5:8080")
	g := s.overlay([]*RouteGroup{{Host: "app.example.org", Backends: []*Backend{local}}}, nil)[0]
	for _, b := range g.Backends {
		b.markHealthy(true)
	}
	counts := pickCounts(g, 100, true)
	if counts["peer:peer-b"] != 80 || counts["app"] != 20 {
		t.Errorf("counts = %v, want a 4:1 split from the advertised weight (not 2:1 from the count)", counts)
	}
}

// TestPeerWeightFallsBackToCount is the rolling-deploy case. A peer still
// running a binary from before proxy.weight was advertised sends no weight at
// all; without the fallback makePeerBackend's floor would collapse it to
// weight 1, quietly starving a multi-replica peer of its share for as long as
// the two hosts run different builds.
func TestPeerWeightFallsBackToCount(t *testing.T) {
	if got := peerWeight(peerRouteInfo{Backends: 3}); got != 3 {
		t.Errorf("weight for a peer advertising none = %d, want its backend count 3", got)
	}
	if got := peerWeight(peerRouteInfo{Backends: 3, Weight: 7}); got != 7 {
		t.Errorf("weight = %d, want the advertised 7 to win over the count", got)
	}
}

// TestPeerRouteStoreAppliesRetunedWeight: an edited weight has to reach the
// router, and merge() is the only thing that can say so — peerRoutesHandler
// refreshes only on a changed merge, and the periodic tick only on TTL expiry.
// Without this the new split would sit unapplied until an unrelated Docker
// event rebuilt the groups.
func TestPeerRouteStoreAppliesRetunedWeight(t *testing.T) {
	s := newPeerRouteStore(time.Minute)
	adv := func(w int) peerRoutePayload {
		return peerRoutePayload{
			Peer:      "peer-b",
			Advertise: "http://100.83.62.68:8092",
			Routes:    []peerRouteInfo{{Host: "app.example.org", Backends: 2, Weight: w}},
		}
	}
	s.merge(adv(2))
	if !s.merge(adv(5)) {
		t.Error("merge reported no change on a retuned weight — refresh() would not run")
	}
	// The other half: a re-push of the SAME weight is just a keepalive and
	// must not churn a Docker-listing refresh every sync interval.
	if s.merge(adv(5)) {
		t.Error("merge reported a change on an unchanged re-advertisement")
	}
}
