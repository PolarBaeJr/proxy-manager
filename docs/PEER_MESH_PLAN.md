# Peer mesh — chosen design (orientation, not a spec)

Status: **not implemented.** This is the actual direction picked for
proxy-level (request-path) federation, as opposed to `PEERS_PLAN.md`'s
narrower dashboard-only aggregation. Written so a future session doesn't
re-derive the design or accidentally build the older, narrower plan instead.
Keep this short — a few hundred words, not a phase-by-phase spec.

## The core idea: peer-as-backend

Unlike `PEERS_PLAN.md` (which only ever fans out read-only API calls between
dashboards), this design lets `cmd/proxy` on one host forward a live request
to a peer proxy when its own backends for a route are unhealthy or absent.
Mechanically: a peer is represented as a **synthetic backend** in a
`RouteGroup.Backends` list — same `*Backend` struct, same health-check and
retry path already in `router.go`, just pointing at `http://peer-tailnet-ip:8092`
instead of a local container IP. This means the request path gets peer
failover for free through existing `pickHealthy`/`tryProxy` — no separate
forwarding code path to maintain.

## Sync: periodic HTTP push, modeled on `cmd/edge/peers.go`

Route/backend state syncs the same way edge already gossips rate-limit
deltas: a `PeerSync` per proxy instance POSTs its known routes to every
configured peer's internal metrics port (`:8094`) on a short interval, and a
`gossipHandler`-shaped endpoint on the receiving side merges them in. No
consensus, no leader — eventual consistency, same tolerance model as edge's
existing gossip. `cmd/proxy/peersync.go` (push loop) and `cmd/proxy/peermerge.go`
(apply-received-routes-as-synthetic-backends) are the natural split, named
so a reviewer maps them onto `cmd/edge/peers.go`'s `PeerSync`/`gossipHandler`
by inspection. `cmd/proxy/peers.go` would hold the peer list/config plumbing.
**None of these three files exist yet** — this document is the only trace of
the design until someone picks up the phase.

## Trust: one shared bearer secret for v1

Like `EDGE_PEER_SECRET`, a single `PMGR_PEER_SECRET` shared by every proxy in
the mesh authenticates both the gossip push and any forwarded request's
peer-identifying header. No per-peer tokens for v1 (that's `PEERS_PLAN.md`'s
dashboard-token model, which is a separate, higher-trust surface). Revisit
per-peer scoping only if the flat-trust model proves too coarse in practice.

## Prerequisite: Redis-backed shared rate limiting

Peer-merging a `proxy.ratelimit` route (i.e. treating two hosts' backends for
the same route as one pool) is only safe once the per-IP rate limit itself is
shared — otherwise a client gets `N × instances` effective throughput just by
being load-balanced across peers. The Redis-backed `hybridLimiter` (see
`cmd/proxy/redisrl.go`) is why this landed *before* peer-as-backend forwarding
rather than alongside it: it removes that ordering hazard for whoever builds
this next.

## Relationship to `PEERS_PLAN.md`

That document's dashboard-aggregation ideas (Cluster tab, `peers.json`,
fan-out `/api/cluster/*` reads) are unaffected by this plan and still make
sense as a UI layer *on top of* whatever proxy-level mesh state exists — they
were never proxy-level to begin with. Don't discard it; just don't implement
its Phase 1 as the proxy-level mesh, which is what this document is for.
