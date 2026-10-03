package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// PeerSync pushes this proxy's locally-owned routes to every configured
// peer, modeled on cmd/edge/peers.go's PeerSync (which gossips rate-limit
// deltas the same way). Every `interval`, it snapshots the router, filters
// out anything that isn't a locally-owned route, and POSTs the result to
// each peer's /peer/routes endpoint on the internal metrics port.
//
// Only backend COUNTS are sent, never backend URLs/topology — a peer only
// needs to know "route X exists over here and has N healthy-looking local
// backends", not how to reach them directly (see peermerge.go, which turns
// a pushed route into a single synthetic backend pointed at the sender's
// own -peer-advertise-url).

// peerRouteInfo describes one locally-owned route group, as advertised to
// peers. Backends is a count only — no backend URLs/topology leaked.
//
// RateLimit/RateRPM are carried across too — NOT part of the plan as
// written, added because omitting them opens a rate-limit bypass: a
// synthesized learned-only group on the receiving side would otherwise
// default to RateLimit=false, so a request routed directly into it (this
// host has no local backend for the route at all) would never be charged
// against the shared limiter.
type peerRouteInfo struct {
	Host        string `json:"host"`
	PathPrefix  string `json:"path,omitempty"`
	StripPrefix bool   `json:"strip,omitempty"`
	Name        string `json:"name,omitempty"`
	// Service carries RouteGroup.Service — the proxy.service label name this
	// route's backends resolve by, static-config or label-managed alike. Lets
	// the receiving side backfill from ITS OWN backendsByService[Service]
	// when it has no local group for this route at all (see peermerge.go's
	// overlay): the same containers that already feed this host's own
	// Service-resolved static routes are otherwise invisible to a route the
	// mesh only knows about via this push, so the peer replicas get bypassed
	// even when healthy and local. See project memory
	// project_routes_json_service_backend_gap.md for how this was found.
	Service  string `json:"service,omitempty"`
	Backends int    `json:"backends"`
	// Weight is the SUM of the locally-owned backends' proxy.weight values
	// (each defaulting to 1), which is what the receiving side gives the
	// single synthetic backend it creates for this peer. Kept separate from
	// Backends rather than folded into it: Backends stays a plain count and
	// remains the validity gate in peermerge.merge, and the two diverge as
	// soon as an operator edits proxy.weight. Zero means "peer didn't send
	// one" — an older binary — and merge falls back to Backends there.
	Weight    int  `json:"weight,omitempty"`
	RateLimit bool `json:"ratelimit,omitempty"`
	RateRPM   int  `json:"ratelimit_rpm,omitempty"`
	// Spread carries RouteGroup.SpreadLocal — this host's OWN proxy.spread
	// labels — across the wire so the receiving side load-balances into this
	// peer instead of holding it in reserve as a failover tier. Deliberately
	// not the adopted RouteGroup.Spread: re-advertising what we learned would
	// latch the flag on forever, each host adopting its own claim back through
	// the other, with no way to clear it short of restarting both proxies
	// inside one TTL window. Removing the label from the replicas that carry
	// it is the off-switch, and it only works if adoption stays one-way.
	Spread bool `json:"spread,omitempty"`
	// ABID/BBackends/BWeight describe the B share of this route's local
	// backends when it runs an A/B test with B replicas here. Backends and
	// Weight stay TOTALS, so a receiver predating A/B in the mesh still sees
	// one backend with today's weight and simply forwards everything (its
	// hops carry no X-Variant, and this proxy assigns them itself, unpinned
	// and unrecorded). A current receiver splits the totals into separate A
	// and B synthetic backends (peermerge.go's overlay). All omitempty, so a
	// route without a test serializes exactly as before.
	ABID      string `json:"ab_id,omitempty"`
	BBackends int    `json:"b_backends,omitempty"`
	BWeight   int    `json:"b_weight,omitempty"`
}

// peerRoutePayload is the body POSTed to a peer's /peer/routes endpoint.
type peerRoutePayload struct {
	Peer      string          `json:"peer"`
	Advertise string          `json:"advertise"`
	Routes    []peerRouteInfo `json:"routes"`
	// Experiments carries every attached A/B test (abmesh.go), at most
	// abMaxPeerExperiments. Omitted when there are none.
	Experiments peerABList `json:"experiments,omitempty"`
}

type PeerSync struct {
	router    *Router
	peers     []string
	secret    string
	identity  string
	advertise string
	interval  time.Duration
	client    *http.Client
}

func newPeerSync(router *Router, peers []string, secret, identity, advertise string, interval time.Duration) *PeerSync {
	return &PeerSync{
		router:    router,
		peers:     peers,
		secret:    secret,
		identity:  identity,
		advertise: advertise,
		interval:  interval,
		client: &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        len(peers) * 2,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (p *PeerSync) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}

// tick snapshots the router and builds the route list to advertise. A group
// is skipped entirely if it has zero locally-owned (!Learned) backends —
// this is the anti-recycling filter that stops a learned-only group (one
// this host only knows about because ANOTHER peer pushed it) from being
// re-advertised back out, which would otherwise let a route bounce around
// the mesh indefinitely.
func (p *PeerSync) tick(ctx context.Context) {
	body, ok := buildPeerPayload(p.router, p.identity, p.advertise)
	if !ok {
		return
	}
	for _, peer := range p.peers {
		go p.send(ctx, peer, body)
	}
}

// buildPeerPayload is tick's payload, as the JSON body to POST; ok=false
// means there is nothing to advertise.
func buildPeerPayload(router *Router, identity, advertise string) ([]byte, bool) {
	var routes []peerRouteInfo
	for _, g := range router.Snapshot() {
		localCount, localWeight, bCount, bWeight := 0, 0, 0, 0
		for _, b := range g.Backends {
			if !b.Learned {
				localCount++
				localWeight += b.Weight
				if b.Variant == abVariantB {
					bCount++
					bWeight += b.Weight
				}
			}
		}
		if localCount == 0 {
			continue
		}
		ri := peerRouteInfo{
			Host: g.Host, PathPrefix: g.PathPrefix, StripPrefix: g.StripPrefix,
			Name: g.Name, Backends: localCount, Weight: localWeight,
			RateLimit: g.RateLimit, RateRPM: g.RateRPM, Spread: g.SpreadLocal,
			Service: g.Service,
		}
		// B fields only for a group running a test with local B replicas:
		// an adopted test (B only on a peer) advertises its A count alone.
		if g.abCfg != nil && bCount > 0 {
			ri.ABID, ri.BBackends, ri.BWeight = g.abCfg.ID, bCount, bWeight
		}
		routes = append(routes, ri)
	}
	if len(routes) == 0 {
		return nil, false
	}
	payload := peerRoutePayload{Peer: identity, Advertise: advertise, Routes: routes, Experiments: router.peerABInfos()}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	if len(body) > peerPayloadMaxBytes {
		// The receiver would reject the whole push; keep route sync alive.
		log.Printf("proxy peer route push: payload %d bytes over the %d limit — sending routes without A/B experiments", len(body), peerPayloadMaxBytes)
		payload.Experiments = nil
		if body, err = json.Marshal(payload); err != nil {
			return nil, false
		}
	}
	return body, true
}

func (p *PeerSync) send(ctx context.Context, peer string, body []byte) {
	url := strings.TrimRight(peer, "/") + "/peer/routes"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.secret)
	resp, err := p.client.Do(req)
	if err != nil {
		// Peer unreachable — expected during restarts / network blips. Silent.
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		// Auth failure or malformed request: worth surfacing so misconfig is visible.
		log.Printf("proxy peer route push: %s → %s", url, resp.Status)
		_, _ = io.Copy(io.Discard, resp.Body)
	}
}
