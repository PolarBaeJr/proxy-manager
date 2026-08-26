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
	Backends    int    `json:"backends"`
	RateLimit   bool   `json:"ratelimit,omitempty"`
	RateRPM     int    `json:"ratelimit_rpm,omitempty"`
	// Spread carries RouteGroup.SpreadLocal — this host's OWN proxy.spread
	// labels — across the wire so the receiving side load-balances into this
	// peer instead of holding it in reserve as a failover tier. Deliberately
	// not the adopted RouteGroup.Spread: re-advertising what we learned would
	// latch the flag on forever, each host adopting its own claim back through
	// the other, with no way to clear it short of restarting both proxies
	// inside one TTL window. Removing the label from the replicas that carry
	// it is the off-switch, and it only works if adoption stays one-way.
	Spread bool `json:"spread,omitempty"`
}

// peerRoutePayload is the body POSTed to a peer's /peer/routes endpoint.
type peerRoutePayload struct {
	Peer      string          `json:"peer"`
	Advertise string          `json:"advertise"`
	Routes    []peerRouteInfo `json:"routes"`
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
	var routes []peerRouteInfo
	for _, g := range p.router.Snapshot() {
		localCount := 0
		for _, b := range g.Backends {
			if !b.Learned {
				localCount++
			}
		}
		if localCount == 0 {
			continue
		}
		routes = append(routes, peerRouteInfo{
			Host: g.Host, PathPrefix: g.PathPrefix, StripPrefix: g.StripPrefix,
			Name: g.Name, Backends: localCount,
			RateLimit: g.RateLimit, RateRPM: g.RateRPM, Spread: g.SpreadLocal,
		})
	}
	if len(routes) == 0 {
		return
	}
	body, err := json.Marshal(peerRoutePayload{Peer: p.identity, Advertise: p.advertise, Routes: routes})
	if err != nil {
		return
	}
	for _, peer := range p.peers {
		go p.send(ctx, peer, body)
	}
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
