# Federated peers — design plan

Status: **draft, not implemented.** Captures the shape of the work for the
"active scheduler / placement" tier of distributed support the user picked.
Lives here so future sessions can pick it up without re-deriving the design.

The goal: run proxy-manager on multiple hosts (Pis, VPSes, friends' boxes), see
all of them in one dashboard, and let the dashboard place new services on the
right host automatically based on capacity and intent.

This is a **large, security-sensitive** scope. The plan is phased so that each
phase ships independently and is useful on its own; we never have to merge it
all at once.

---

## Constraints carried over from the existing project

- Zero external runtime deps (just QR, autocert, crypto). Federation must not
  introduce a control-plane process the user doesn't already run.
- All four binaries can crash without the others noticing. A peer going down
  must not take other peers' dashboards offline.
- Single-file embedded UI. Federation tabs add to `cmd/dashboard/ui.go`, not
  a separate SPA.
- 2FA-required for writes, read-only Docker socket on the request path.

If a design choice violates these, it's the wrong choice.

---

## Phase 1 — read-only aggregation (foundation)

Before any scheduling, prove the mesh by aggregating views. This is also
what the "read-only aggregation" tier would have shipped on its own.

### Topology

Star — there is no consensus. Every peer has the same `peers.json`. When the
local dashboard queries a peer, it does so directly over the Tailscale (or
WireGuard) network. No leader election, no Raft, no etcd.

```
                ┌──────────────────────────────────────────┐
   dashboard A ←┼→ dashboard B ←┼→ dashboard C
                │ Tailscale mesh                            │
                └──────────────────────────────────────────┘
```

Any dashboard can answer cluster queries by fan-out to its peers. Peer A asking
peer B for its services is the same code path as a local request — the dashboard
calls `https://peer-b.tailnet/api/services` with a peer-only API token.

### Network assumption

We assume **Tailscale** (or any private overlay) is already present on every
peer. We do NOT bundle the mesh. Rationale:
- Tailscale solves auth, NAT, key rotation, ACLs better than we can.
- If the user prefers WireGuard or even just `ssh -L` port-forward tunnels, the
  same `peers.json` works — only the URLs change.
- Federation should never expose a dashboard port publicly to other peers.

### Identity

Each peer holds a per-peer API token issued by the OTHER peer's dashboard. The
user (or `scripts/peer-link` once it exists) walks through:

1. On peer A's dashboard: Users → Tokens → "Create peer token". Copy.
2. On peer B's dashboard: Cluster → Peers → "Add peer", paste token + URL.
3. B can now call A's API with that bearer token.

Tokens are SHA-256-hashed at rest (already the case). Per-peer tokens carry an
extra `peer:` label so they're visible as such in the Users tab and easy to
revoke.

### peers.json schema

```json
{
  "self": {
    "name": "pi-home",
    "labels": ["arm64", "ssd", "home-network"]
  },
  "peers": [
    {
      "name": "vps-eu",
      "url": "https://vps-eu.tailnet/api",
      "token": "pmt_..."         // bearer for outbound calls
    },
    {
      "name": "pi-friend",
      "url": "http://100.64.5.7:8093/api",
      "token": "pmt_..."
    }
  ]
}
```

Stored at `cmd/dashboard/data/peers.json`, mode 0600, gitignored. Hot-reloaded
on file change like `routes.json`.

### API surface — phase 1

| Endpoint | Auth | Behaviour |
|---|---|---|
| `GET /api/cluster/peers` | session | list configured peers + last-seen + health |
| `GET /api/cluster/services` | session | fan-out `/api/services` across all peers, returns `{peer → [services]}` |
| `GET /api/cluster/routes` | session | same shape, for routes |
| `GET /api/cluster/health` | session | per-peer up/degraded/down from each peer's `/api/health` |
| `POST /api/cluster/peers` | elevated | add a peer (validates token by calling `/api/health`) |
| `DELETE /api/cluster/peers/{name}` | elevated | remove from `peers.json` |

Fan-out timeout: 3s per peer, in parallel. A slow or down peer never blocks
the whole call — it returns `{peer-name: {error: "..."}}` in its slot.

### Dashboard UI — phase 1

New top-level tab **Cluster** (between Stats and the right-edge logout):
- **Peers** sub-section: list, add, remove, last-seen latency, health pill.
- **All services** sub-section: every service across the mesh, columns
  `peer | service | replicas | image | host | health`. Click a row → opens the
  service detail on its home peer's dashboard (deep link).
- **All routes** sub-section: same for routes.

Cluster tab hidden when `peers.json` is missing or has zero peers — keeps the
single-host UX clean.

### Failure modes addressed in phase 1

- Peer unreachable: surface as `degraded` in the peers list; cluster queries
  return partial results without erroring.
- Token rotated on the remote peer: 401 from that peer; UI highlights the row
  with a "re-issue token" action.
- Clock skew between peers: doesn't matter yet (no leases, no causal ops).

**Phase 1 is shippable on its own** and is the prerequisite for everything below.

---

## Phase 2 — remote operations (write-through)

Same fan-out machinery, but writes. The user picks a peer in the UI before
acting. No scheduler yet.

| Endpoint | Auth | Behaviour |
|---|---|---|
| `POST /api/cluster/peers/{peer}/services` | elevated | proxy create-service to that peer |
| `POST /api/cluster/peers/{peer}/services/{name}/scale` | elevated | proxy scale |
| `POST /api/cluster/peers/{peer}/services/{name}/replace` | elevated | proxy replace |
| `POST /api/cluster/peers/{peer}/services/{name}/stage|promote` | elevated | proxy canary |
| `DELETE /api/cluster/peers/{peer}/services/{name}` | elevated | proxy delete |

Audit: every cross-peer write is logged on BOTH sides — the originator
records `user → peer.action`, the target records `peer-token → action` with
the originator's name attached via an `X-Peer-Originator` header.

UI changes: every action button gets a peer selector when more than one peer is
configured. Default is `self`.

---

## Phase 3 — active scheduler / placement (the asked-for tier)

This is where it gets interesting. The user creates a service WITHOUT picking
a peer; the scheduler chooses one.

### Service intent

`CreateServiceRequest` gains optional fields:

```json
{
  "name": "blog",
  "image": "ghcr.io/me/blog:latest",
  "host": "blog.polardev.org",
  "port": 3000,
  "replicas": 2,
  "placement": {
    "any_of": ["amd64"],          // require label
    "none_of": ["home-network"],  // exclude label
    "spread": true,               // replicas on different peers if possible
    "prefer": ["vps-eu"]          // tie-break preference
  }
}
```

If `placement` is omitted, scheduler treats it as "prefer self".

### Scheduler algorithm

Pure-function, deterministic given inputs. Lives in
`cmd/dashboard/scheduler.go`. Input: the service intent + a snapshot of each
peer's capacity (from monitor). Output: `{peer → replica count}`.

Pseudocode:

```
1. Filter peers by any_of/none_of labels → eligible set.
2. Score each eligible peer:
     score = capacity_factor − load_factor − affinity_penalty
   where:
     capacity_factor = 1 − (cpu_5m / cpu_total) − (mem_used / mem_total)
     load_factor     = peer's current request rate / peer's p95 capacity
     affinity_penalty = +1 for each `prefer` not matched, scaled
3. If spread=true, do round-robin over top-N scored peers until replicas placed.
   Else, place all on the top-scoring peer.
4. Emit placement.
```

The scheduler NEVER places anything itself — it returns the plan; the
dashboard's existing write path executes it via phase 2 endpoints.

### Capacity feed

The `monitor` binary already scrapes per-binary metrics. Extend its
`/api/overview` to include peer-local cpu/mem/disk + recent request rate.
The dashboard's existing host-stats code (`stats.go`) already collects these
locally — expose them on the dashboard's `/api/host-stats` so peers can scrape
each other.

### Env / secrets distribution

The hard part. Three options, ranked by risk:

| Option | Risk | Notes |
|---|---|---|
| **A: user pre-provisions** | low | env vars must already exist on the target peer (via `.env` or compose override). Scheduler refuses to place if a required env key is missing. Phase-3-default. |
| **B: dashboard syncs** | medium | originating dashboard ships env over the peer tunnel; target dashboard writes to its `.env` for that service. Auditable; tokens-only. |
| **C: shared secret store** | high | introduces a 5th component (Vault, age-encrypted file synced). Out of scope for v1. |

Default to **A** for v1 — the user keeps secrets on each peer manually, the
scheduler just refuses if it can't find them. Revisit B once we have phase 3
working with A.

### UI changes

- "New service" form gains an optional **Placement** section (collapsed by
  default) with the four fields above.
- The placement preview shows scoring breakdown before the user confirms.
- The Cluster → All services table gains a column showing which peer holds
  each replica.

### Replica migration

NOT in scope for phase 3 v1. If a peer goes down, replicas there are simply
unreachable; the proxy already retries other healthy backends. The user
re-runs "place" to choose a new home. (Future phase 4: automatic re-place
on peer-down — needs leader election to avoid double-place. Avoid for now.)

---

## Out of scope (write down so we don't drift)

- Shared volumes / stateful workload movement (CSI-like) — too much for a
  homelab tool.
- Multi-region traffic steering — that's Cloudflare's job.
- Cross-peer logs aggregation — Phase 1's `/api/cluster/services` is fan-out,
  not stream-merging. Logs/access tabs stay per-peer for now.
- Public discovery / gossip protocols. `peers.json` is the only source of
  truth.

---

## Security model — explicit

Even in phase 3, every cross-peer call carries:

1. **TLS** (or Tailscale's transport encryption + auth).
2. **Bearer token** issued by the receiving peer, not the calling peer.
3. **Per-peer audit** on BOTH sides.

Token never travels OVER the peer tunnel in either direction at runtime —
only at the original peer-link setup, where the user pastes it in by hand.

A compromised peer can:
- See the routes / services / metrics of any peer that gave it a token.
- Create / delete services on any peer it has a token for.

A compromised peer CANNOT:
- Read peers it wasn't linked to (tokens are scoped per-peer).
- Read another peer's auth.json or audit.log.
- Issue tokens on behalf of users on another peer.

The user can revoke a peer's token at any time — same UI as user tokens.

---

## Open questions (need answers before phase 1 code)

1. **Mesh transport.** Confirm Tailscale is the assumed underlay, or pick
   WireGuard / pure SSH tunnels. This decides whether we need to document
   firewall rules or assume Tailscale ACLs handle it.
2. **Peer discovery.** Static `peers.json` only? Or do we want a `peer
   announce` push (one-way: A tells B "here I am at this URL")? Static is
   simpler; push is nicer when peers move.
3. **Self-hosting peer-link.** Should the dashboard generate the peer
   token + show a copy-paste snippet, or should we ship a `scripts/peer-link`
   that does both sides over SSH?
4. **Naming.** "Peers" vs "Cluster" vs "Nodes" — pick once, use everywhere.
   Recommend "Cluster" (it's what users will search for).

---

## Rollout

Phase 1 alone is ~1 weekend of work. Each phase is a separate branch and a
separate merge so the user can stop at any phase and the rest stays
unshipped.

```
phase 1  →  cluster read-only         (small, isolated, useful by itself)
phase 2  →  cluster writes             (extends phase 1; user-driven placement)
phase 3  →  scheduler                 (extends phase 2; adds the plan endpoint)
phase 4  →  automatic re-place         (later, only if needed)
```

The recommendation when picking this up: start with phase 1, get a real
two-peer setup working end-to-end, then re-evaluate scope before phase 2.
