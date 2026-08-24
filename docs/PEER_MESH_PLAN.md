# Peer mesh — chosen design (orientation, not a spec)

Status: **Phase 2 (peer discovery + handshake) implemented.** Route sync
(peer-as-backend forwarding, `peersync.go`/`peermerge.go`) is not yet built —
see the note below. This is the actual direction picked for proxy-level
(request-path) federation, as opposed to `PEERS_PLAN.md`'s narrower
dashboard-only aggregation. Written so a future session doesn't re-derive the
design or accidentally build the older, narrower plan instead. Keep this
short — a few hundred words, not a phase-by-phase spec.

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
by inspection.

**Implemented so far (Phase 2):** `cmd/proxy/peers.go` holds the peer
list/config plumbing — a `PeerRegistry` that periodically POSTs an
authenticated, placeholder handshake to every configured peer's
`/peer/handshake` endpoint (also in `peers.go`) and records success/failure +
last-contact time per peer. This proves connectivity and auth work end to
end; no route/backend data crosses the wire yet. **`peersync.go` and
`peermerge.go` do not exist yet** — that's the next phase to pick up.

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

## Known limitation: self-referential health checks defeat failover

A backend's health-check endpoint is only a trustworthy signal if it's
actually independent of the proxy/edge layer that's consuming it. If a
backend's health endpoint reaches its own dependencies (e.g. a database) by a
path that also hairpins back out through the same proxy/edge infrastructure —
rather than staying fully internal to the Docker network — then that
backend's health signal is partly self-referential: a degradation in the
proxy or edge layer itself can make every backend behind it report unhealthy
at the same time, even though each backend's own application and database are
both fine.

Critically, peer-mesh failover does **not** rescue this case. A peer's own
probe of its own backend for the same route hairpins through the same shared
edge infrastructure, so the correlated failure isn't something failover can
route around by construction — every peer sees the same false-unhealthy
signal for the same structural reason. (For example, hypothetically: if a
backend's health endpoint round-trips through a public URL rather than an
internal Docker network route, an edge outage would look identical to that
backend actually being down, to every prober in the mesh at once.)

The fix is architectural (keep health-check dependency paths internal to the
Docker network, not routed back through the proxy/edge), not something this
plan's failover mechanism can paper over.

## Relationship to `PEERS_PLAN.md`

That document's dashboard-aggregation ideas (Cluster tab, `peers.json`,
fan-out `/api/cluster/*` reads) are unaffected by this plan and still make
sense as a UI layer *on top of* whatever proxy-level mesh state exists — they
were never proxy-level to begin with. Don't discard it; just don't implement
its Phase 1 as the proxy-level mesh, which is what this document is for.
