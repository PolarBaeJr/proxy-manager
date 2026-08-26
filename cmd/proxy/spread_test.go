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

	groups, err := assembleGroups(context.Background(), dc, "")
	if err != nil {
		t.Fatalf("assembleGroups: %v", err)
	}
	if g := findGroup(groups, "a.example.org", ""); g == nil || !g.Spread {
		t.Errorf("a group = %+v, want Spread from the one labeled replica", g)
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
	groups := s.overlay([]*RouteGroup{g})

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
	groups := s.overlay([]*RouteGroup{g})
	if groups[0].Spread {
		t.Error("Spread turned on by an advertisement that never claimed it")
	}
}

// TestPeerSyncAdvertisesSpread proves the flag reaches the wire even when
// this host only ADOPTED it from another peer rather than carrying the label
// itself — that re-advertisement is what makes the pool symmetric once
// either side of a cross-host scale opts in.
func TestPeerSyncAdvertisesSpread(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- b
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	local := makeBackendForTest("http://172.26.0.5:8080")
	learned := makePeerBackend("http://100.1.1.1:8092", "app.example.org", "", false, "peer-c", 1)
	r := &Router{}
	r.Set([]*RouteGroup{{Host: "app.example.org", Backends: []*Backend{local, learned}, Spread: true}})

	ps := newPeerSync(r, []string{srv.URL}, "s3cret", "peer-a", "http://100.0.0.1:8092", 0)
	ps.tick(context.Background())

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
	if len(payload.Routes) != 1 {
		t.Fatalf("Routes = %+v, want exactly 1", payload.Routes)
	}
	if !payload.Routes[0].Spread {
		t.Error("Spread not advertised")
	}
	if payload.Routes[0].Backends != 1 {
		t.Errorf("advertised backends = %d, want 1 — a learned backend is not local capacity", payload.Routes[0].Backends)
	}
}
