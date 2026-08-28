package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// minHealthyReplicasForRollingReplace is the minimum total healthy,
// non-canary replica count a label-managed service must have across the
// local host and every reachable peer BEFORE a rolling-replace is allowed to
// start. A single named constant + single predicate so the policy is a
// one-line change: as shipped, this has NO proxy.unscalable exemption, which
// means a singleton service pinned to exactly 1 replica on a single host can
// never pass this guard (mesh total = 1) unless it's spread across both
// hosts (1+1 = 2). That's an intentional, but reviewable, product decision —
// change this constant or add an exemption in the predicate below if that's
// wrong for some service.
const minHealthyReplicasForRollingReplace = 2

// healthyNonCanaryReplicas is the same healthy-replica predicate
// servicestatus.go's status view uses (kept in exact phrasing there for
// consistency), factored out here so totalHealthyReplicas and any future
// caller share one definition.
func healthyNonCanaryReplicas(svc Service) int {
	healthy := 0
	for _, m := range svc.MemberSummaries {
		if m.IsCanary {
			continue
		}
		if m.State == "running" && m.Health != "unhealthy" && m.Health != "starting" {
			healthy++
		}
	}
	return healthy
}

// peerHealthyReplicas counts name's healthy, non-canary replicas on every
// configured peer, and separately reports how many of those peers actually
// answered — deliberately NOT built on fetchPeerServices, because that
// function only exposes which SERVICES came back (tagged with Machine), not
// which PEERS were reached: a peer that's up but simply doesn't run name
// would be indistinguishable, via fetchPeerServices' output alone, from a
// peer that never answered at all. ensureRollingReplaceCapacity's caveat
// below needs to tell those two apart, so this does its own /peer/services
// round trip.
func peerHealthyReplicas(ctx context.Context, registry *PeerRegistry, secret, name string) (healthy, reachable, configured int) {
	peers := registry.Peers()
	configured = len(peers)
	if configured == 0 || secret == "" {
		return 0, 0, configured
	}
	client := &http.Client{Timeout: 2 * time.Second}
	type result struct {
		ok      bool
		healthy int
	}
	results := make(chan result, len(peers))
	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(peer string) {
			defer wg.Done()
			var body peerServicesResp
			if err := peerGET(ctx, client, peer, secret, "/peer/services", &body); err != nil {
				log.Printf("dashboard peer services (capacity check): %v", err)
				results <- result{}
				return
			}
			h := 0
			for _, s := range body.Services {
				if s.Name == name {
					h += healthyNonCanaryReplicas(s)
				}
			}
			results <- result{ok: true, healthy: h}
		}(peer)
	}
	wg.Wait()
	close(results)
	for r := range results {
		if r.ok {
			reachable++
			healthy += r.healthy
		}
	}
	return healthy, reachable, configured
}

// totalHealthyReplicas counts name's healthy, non-canary replicas on this
// host (local) and across every reachable peer (total = local + peers).
// registry may be nil (no peer mesh configured), in which case total ==
// local and peersConfigured/peersReachable are both 0. peersReachable can be
// less than peersConfigured — see peerHealthyReplicas — so total can
// undercount a mesh with an unreachable peer; callers must not treat total
// as a confirmed mesh-wide count unless peersReachable == peersConfigured.
func totalHealthyReplicas(ctx context.Context, dc *dockerClient, registry *PeerRegistry, peerSecret, name string) (local, total, peersReachable, peersConfigured int, err error) {
	svcs, err := dc.listServices(ctx)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	for _, s := range svcs {
		if s.Name == name {
			local += healthyNonCanaryReplicas(s)
		}
	}
	total = local
	if registry != nil {
		peerHealthy, reachable, configured := peerHealthyReplicas(ctx, registry, peerSecret, name)
		total += peerHealthy
		peersReachable, peersConfigured = reachable, configured
	}
	return local, total, peersReachable, peersConfigured, nil
}

// ensureRollingReplaceCapacity refuses to let a rolling-replace start unless
// name has at least minHealthyReplicasForRollingReplace healthy replicas
// across the mesh. On a Docker/list error from the LOCAL count, it fails
// open (returns nil) — matching this codebase's existing convention of not
// blocking a mutation on a transient inspect error.
func ensureRollingReplaceCapacity(ctx context.Context, dc *dockerClient, registry *PeerRegistry, peerSecret, name string) error {
	local, total, peersReachable, peersConfigured, err := totalHealthyReplicas(ctx, dc, registry, peerSecret, name)
	if err != nil {
		return nil
	}
	if total < minHealthyReplicasForRollingReplace {
		var caveat string
		switch {
		case peersConfigured == 0:
			caveat = "no peers are configured, so this total is confirmed local-only"
		case peersReachable == peersConfigured:
			caveat = fmt.Sprintf("all %d configured peer(s) answered, so this total is confirmed across the mesh", peersConfigured)
		default:
			caveat = fmt.Sprintf("only %d of %d configured peer(s) answered — an unreachable or slow peer would undercount this, so confirm peer connectivity before retrying", peersReachable, peersConfigured)
		}
		return fmt.Errorf("refusing rolling-replace of %q: only %d healthy replica(s) found across the mesh (minimum %d required) — %d locally; %s, or use replace_service if you accept the non-health-gated risk", name, total, minHealthyReplicasForRollingReplace, local, caveat)
	}
	return nil
}
