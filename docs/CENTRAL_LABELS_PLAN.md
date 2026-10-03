# Central labels (Redis-backed live `proxy.*` labels)

Source of truth for the central-labels work. Today every `proxy.*` setting
lives on the container, so changing a weight, a cache TTL or a rate limit
means recreating replicas. This plan moves the *live-tunable* labels of an
adopted service into Redis, where both proxies (and later the dashboard)
read them and apply them without a restart.

Status: Phase 1a (proxy side) implemented on `feat-central-labels`.
Phase 1b, 2, 3 and the ACL PR are not started.

## Facts (verified against the code at plan time)

- The proxy only reads containers labelled `proxy.enable=true`
  (`listEnabledContainers`, `cmd/proxy/docker.go`). The overlay therefore
  cannot *enable* a container; it can only change the managed keys of
  containers the proxy already sees.
- `assembleGroups` (`cmd/proxy/router.go`) has ~20 test callers, so its
  signature stays. A static `routes.json` entry owns its host+path and
  ignores container labels, except that the `backendsByService` feed uses the
  container's `proxy.weight`. Weights are per container.
- Peer route push sends the assembled `RouteGroup` (`peersync.go`), so
  overlaid values propagate across the mesh. Each proxy overlays only its own
  local containers.
- The dashboard reads raw labels in `listServices`, `drainStopRemove`,
  `memberDrainSeconds` (`lifecycle.go`), `guardUnscalable`, `spread`
  (`spread.go`), `createContainer` (`proxy.drain` → `StopTimeout`) and adopt
  strictness (`centralenv_adopt.go`, which must keep reading raw labels).
- The setters `setAutoUpdateLabel` / `setUnscalableLabel` / `setWeightLabel`
  (`cmd/dashboard/docker.go`) recreate replicas. They are called from
  `api.go` and `peers.go`.
- Label maps can alias the list cache (`centralenv.go`). The overlay must
  always copy them.
- Redis is `redis:7-alpine`, so ACL selectors are available. Both binaries
  connect as the default user today. The existing keys are
  `pmgr:auth:users`, `pmgr:dashboard:versions` and `pmgr:rl:*`.
- There is no miniredis. Existing tests use an interface seam
  (`redisPrimary`, `redisrl.go`).
- Central env stores its origin file at `/data/central-env/<svc>.json`
  (`{Version, ResolvedHash, Prev, LastRequestIDs, State, Adopting}`) and its
  non-origin cache at `/data/central-env-cache/<svc>.json`
  (`{FailedVersion, Healthcheck}`). Replicas carry `pmgr.env.origin` and
  `pmgr.env.version`, and `ref:NAME` values resolve from the origin's secrets
  directory.
- MCP: `set_service_env` is registered only after both the `!allowWrites`
  return and the `!allowPeerWrites` return. `hostArg` gates the host.
  `/api/services/` is wrapped in `requireElevated`. `forwardServiceMutation`
  is a fixed switch, so label dispatch must sit next to the central-env hook.
- A/B: the split comes from `proxy.ab.split`. Weight is only summed into
  `BWeight`. The overlay applies to all members, B included, and never
  touches `proxy.ab.*`.
- The dashboard groups services by `proxy.service`, the same key the proxy
  overlay joins on. Adoption (1b) must still require `proxy.service` on
  every member, so nothing is adopted that the proxy cannot overlay.

## Redis key schema

All key names are constants in `internal/labels`.

| Key | Type | Contents |
|---|---|---|
| `pmgr:labels:<svc>` | HASH | Managed keys only. Authoritative once adopted: a missing field means unset. |
| `pmgr:labels:<svc>:meta` | HASH | `version` (per-service CAS), `adopted_at`, `adopted_by`, `updated_at`, `updated_by`, `imported` (JSON host→{k:v} at adopt), `prev` (JSON previous map), `req_ids` (JSON ring of the last 32 `{request_id, version}`) |
| `pmgr:labels:index` | SET | Adopted services |
| `pmgr:labels:version` | STRING | Global counter, INCR on every write. Missing means Redis was wiped. |
| channel `pmgr:labels:changed` | pub/sub | Message `"<svc> <global_version>"` |

All writes go through one Lua script. It checks `meta.version == if_version`,
replaying a known `request_id`. It then writes `prev`, does `DEL` of the hash
followed by `HSET` of the full map, `HINCRBY meta version`, `SADD index`,
`INCR global` and `PUBLISH`. If the global counter is missing at write time,
it is initialised to 1.

Release (Phase 2) does `DEL` of both hashes, `SREM`, `INCR` and `PUBLISH`.

Reserved service names: `index` and `version` would collide with the index
and version keys. Service names cannot contain `:`, so these are the only
collisions. `labels.ValidService` rejects them, and every reader and writer
must use it.

## Classification (`internal/labels` registry)

| Class | Keys | Validation |
|---|---|---|
| ProxyLive | `proxy.weight` | int 1–100 (mirrors the dashboard's `maxServiceWeight`) |
| | `proxy.health` | path starting with `/` |
| | `proxy.strip` | bool |
| | `proxy.name` | ≤64 characters, no control characters |
| | `proxy.ratelimit` | bool |
| | `proxy.ratelimit.rpm` | int > 0 |
| | `proxy.sticky` | bool |
| | `proxy.cache` | `ParseCacheTTL` |
| | `proxy.cache.paths` | comma list, each entry starts with `/` |
| ProxyLive, tighten-only | `proxy.drop_headers` | comma list of header tokens. Removing a currently dropped header is refused unless `allow_loosen`. |
| DashboardLive | `proxy.autoupdate` | bool |
| | `proxy.unscalable` | bool |
| | `proxy.drain` | int 0–300. The dashboard applies it to stops immediately. Docker's `StopTimeout` only picks it up on the next recreate. |
| | `proxy.group` | service-name shape |
| | `proxy.maintenance` | absolute path |
| | `proxy.overlap` | bool. With `proxy.unscalable=true`, dashboard restarts, replaces, updates and label edits do start-new → health gate → drain-stop old. Being implemented on `feat-overlap-restart`. |
| Security (container-only) | `proxy.auth`, `proxy.auth.users`, `proxy.auth.mode` | Rejected via Redis for now. Anyone holding Redis credentials could otherwise switch auth off. |
| Identity (recreate/compose) | `proxy.enable`, `proxy.service`, `proxy.host`, `proxy.port`, `proxy.path` | Rejected |
| Per-replica (dashboard-owned) | `proxy.canary`, `proxy.previous_image`, `proxy.spread`, `proxy.ab.*`, `pmgr.env.*` | Rejected, never imported |
| Unknown | anything else | Rejected with a 400 that lists the allowed keys |

`labels.Managed(key)` is true for ProxyLive and DashboardLive.

`labels.ValidateChange(current, set, unset, allowLoosen)` enforces the
following:

- It rejects non-registry, identity, security and per-replica keys.
- A key may not appear in both `set` and `unset`.
- It runs each key's validator.
- Values are ≤1 KiB, contain no control characters and are not empty (use
  `unset` instead).
- The resulting map has ≤64 keys.
- `proxy.drop_headers` is tighten-only. Header names are compared after
  canonicalisation.

### Precedence

1. A `routes.json` static entry for the host+path.
2. The adopted service's managed keys from Redis, authoritatively. An unset
   key is removed from the container's view even if compose sets it.
3. Container labels, for unadopted services and for all non-managed keys.

## Phase 1a: proxy overlay (this branch, no dashboard changes)

- **New `internal/labels/labels.go` (+ test).** Contains:
  - the registry `Spec{Key, Class, Validate, Consumer}`, the `Class` enum and
    the validators
  - `Managed`, `ValidateChange`, `ValidService` and `ParseCacheTTL`
  - the Redis key-name constants
  - validation constants duplicated from `cmd/dashboard` with a "mirrors"
    comment, because `internal/` must not import `cmd/*`

  `cmd/proxy/cache.go`'s `parseCacheTTL` delegates to `ParseCacheTTL`.
- **New `cmd/proxy/labeloverlay.go` (+ test).**
  - `labelOverlay{Version, Source, LoadedAt, Services}`.
  - `applyLabelOverlay` copies the container structs and label maps. It
    iterates the *registry's* managed keys, setting them from the overlay or
    deleting them. Non-managed keys are untouched, and `proxy.ab.*`,
    `proxy.service` and identity keys can never be touched.
  - `overlaySource` interface: `Load(ctx) (*labelOverlay, wiped bool, error)`,
    `Version(ctx)` (a cheap GET for the poll) and `Watch(ctx, onChange)`.
  - The Redis implementation does `GET version` (missing means wiped), then
    `SMEMBERS index`, then pipelined `HGETALL`. It re-validates every value
    and drops invalid ones with a deduped log. A dropped value means unset.
  - Disk cache at `/data/labels-overlay.json`, written atomically with
    tmp+rename (the `persist.go` pattern).
  - `overlayManager` holds the overlay in an `atomic.Pointer`. `Run`
    subscribes to `pmgr:labels:changed` and also polls the version every 5s.
    - Change is detected by version `!=`, not `>`, because a reseed after a
      wipe restarts the counter at 1. On change it reloads, swaps, saves to
      disk and calls `refresh()`.
    - On a Redis error it keeps the current overlay, sets `redis_ok=false`
      and backs off up to 30s. `redis_ok` and the backoff are driven by the
      poll only, because go-redis pub/sub reconnects silently.
    - If Redis is wiped while the overlay is non-empty, it keeps the overlay
      and logs loudly once. The logged flag resets once the version key
      reappears.
  - Startup loads the disk cache (source `disk`), then syncs from Redis with
    a 2s timeout, all before the first `refresh()`.
- **`router.go`.** `assembleGroupsWithOverlay(ctx, dc, configPath, ov)` holds
  the old body, and `assembleGroups` wraps it with `nil`. The overlay is
  applied right after `listEnabledContainers` and before `abScan`.
- **`main.go`.**
  - One `redisClient` is shared by the limiter and the overlay.
  - New flag `-labels-cache` (default `/data/labels-overlay.json`).
  - Optional `REDIS_USERNAME` env, passed as `redis.Options.Username`.
  - `refresh` uses `om.Current()`.
  - `GET /labels` on the metrics mux returns
    `{version, source: redis|disk|none, loaded_at, redis_ok, services}`. It
    never includes the Redis address, credentials or error strings.
  - With an empty `-redis-addr` the overlay is entirely off: the disk cache
    is not read, the source is `none` and behaviour is unchanged.

## Phase 1b: dashboard (after `feat-overlap-restart` merges)

1b edits the same setters in `docker.go` that `feat-overlap-restart`
touches, and it must route `overlapEnabled()` reads through
`effectiveLabels` too.

### New files

- **`cmd/dashboard/labelstore.go`.** Defines the `labelStore` interface
  (`Get` / `Write` / `Index` / `GlobalVersion`). The Redis implementation uses
  the Lua script above and keeps an in-memory snapshot fed by pub/sub plus a
  5s poll. `dc.labels` is nil-safe.
- **`labels_overlay.go`.** `effectiveLabels` (always returns a copy),
  `effectiveDrainSeconds`, `labelsAdopted` and `redirectLabelSetter`.
- **`labels_api.go`.**
  - `GET` and `POST /api/services/{svc}/labels[?host]`.
  - Peer endpoint `/peer/labels/{svc}`.
  - Import/adopt and drift reporting.

### Wiring

- `api.go`: one hunk after the central-env block.
- `main.go`: create the store only when `redisClient != nil &&
  LABELS_CENTRAL`. Add `/peer/labels/` to `peerHandlers`. Writes are gated by
  `LABELS_WRITES`.
- `docker.go` hunks:
  - `listServices` uses effective labels (copied).
  - `drainStopRemove` uses `effectiveDrainSeconds`.
  - `guardUnscalable` uses effective labels.
  - The setter preambles call `redirectLabelSetter`.
  - Not in `listAll`, `listAllDirect` or `cloneEnvAndSpec`.
- `lifecycle.go`: `memberDrainSeconds` uses `effectiveDrainSeconds`.
- `spread.go`: reads effective labels. For an adopted service it does not
  copy autoupdate/weight.
- MCP: `get_service_labels` is read-only, registered near `get_service_env`.
  `set_service_labels` is registered after `!allowPeerWrites`.
- Adoption requires `proxy.service` on every member.

### GET response

- `managed`, `version`, `labels`, `effective`
- `container_labels` per host, and `drift`
- `import_preview` plus `conflicts` when unadopted
- `readonly_keys`
- `applied`: each proxy's `/labels` version and source

### POST body

`{if_version, request_id, set, unset, allow_loosen, resolve_conflicts?}`

1. Self-guard first (`serviceContainsSelfByName`). Return 403 on self or on a
   Docker error.
2. Skip the rollout guard (leave a comment saying why), except refuse
   `proxy.weight` while a canary is staged.
3. Forward to the peer if `host` names one.
4. A nil store or Redis down returns 503. Never fall back to recreating.
5. Unadopted: import from the preview. Unresolved conflicts return 409.
6. Adopted: `if_version` CAS, merge, validate, write.
7. Audit `service.labels_set` with key names only.
8. Return `{version, changed_keys, imported, warnings}`, including a drain
   warning.

### Peer handler

- Bearer auth.
- POST requires `peerWritesEnabled` plus the handler's own self-guard.
- Actor `peer-mesh`, marked forwarded.

### `redirectLabelSetter`

- Only applies when the service is adopted.
- autoupdate/unscalable `false` becomes an unset, and weight `1` becomes an
  unset.
- Uses the current version as `if_version` and retries once.
- Audits `"(via <setter>)"`.

### MCP tools

- `get_service_labels{service, host?}`.
- `set_service_labels{service, host?, if_version, request_id?, set, unset,
  allow_loosen?}`, marked Mutating.
- The descriptions list:
  - the live keys
  - the rejected identity/auth keys
  - that changes apply in ~5s with no restart
  - that callers verify via `applied`

### Rollout

1. Proxies on both hosts first. Verify that `/labels` reports source
   `redis`.
2. Dashboard reads (`LABELS_CENTRAL`).
3. Dashboard writes (`LABELS_WRITES`).

### Wipe recovery

- The dashboard may see the version missing and an empty snapshot. It then
  fetches the local proxy's `/labels`. If the source is `disk` with services,
  it reseeds via Lua with `if_version=0` and audits
  `labels_reseed_from_proxy`.
- While `pmgr:labels:version` is missing, every 1b write must refuse with 503
  or reseed first. The Lua script re-initialises the counter on any write,
  and the proxy would then load an index holding only that one service,
  reverting every other adopted service to its compose labels.
- Autoupdate skips adopted-but-unknown services while the store has never
  loaded (`everLoaded`).

### Tests

- **1a:**
  - Registry accept/reject table, the drop_headers rule and
    `parseCacheTTL` parity.
  - Overlay set/unset, input not mutated, unadopted and ab/service keys
    untouched.
  - `assembleGroupsWithOverlay` via `fakeDocker`.
  - `overlayManager` with a fake source: disk startup, Redis error, wipe,
    one refresh per bump, atomic disk write.
  - A race test.
- **1b:**
  - `labels_api_test` with a fake store covering:
    - import
    - conflict 409 and `if_version` 409
    - self-guard 403, including on a Docker error
    - 503 when the store is unavailable
    - audit and host forwarding
    - peer `writesEnabled`
  - `listServices` and `drainStopRemove` use effective labels.
  - Redirect does no `createContainer`.
  - Adopt strictness stays on raw labels.
  - MCP registration gating.
- Run `-race` for all concurrent paths.

## Phase 2: UI

- `ui.go` gets a Labels panel with:
  - an effective-labels table that shows each value's source
  - drift badges
  - per-host "applied vN"
  - an edit form (set/unset/if_version)
  - import-preview conflict pickers
  - a "Release labels" button
- Release is `POST .../labels/release`. It does `DEL` of hash and meta,
  `SREM`, `INCR` and `PUBLISH`. It asks for confirmation, requires an
  elevated session and is audited.
- Point the autoupdate/singleton/weight controls at the labels endpoint when
  the service is managed.
- Tests follow `template_selection_test.go`.

## Phase 3: central env → Redis (after `feat-ab-dashboard` merges)

### Keys

| Key | Type | Contents |
|---|---|---|
| `pmgr:env:<svc>` | STRING | the exact `centralEnvRecord` JSON |
| `pmgr:env:index` | SET | |
| `pmgr:env:version` | STRING | |
| channel `pmgr:env:changed` | pub/sub | |
| `pmgr:env:<svc>:host:<identity>` | HASH | `{failed_version, healthcheck JSON}` |

### Backend

- A `centralEnvBackend` interface sits behind `centralEnvStore`, which keeps
  its public methods.
- `persist` becomes a CAS put, via Lua or WATCH/MULTI like authredis's
  `RunTx`.
- A bad record marks the service broken and fails closed. The value never
  appears in logs.

### Origin semantics (preserved)

- The origin runs propagation and resolves `ref:` values.
- set/sync/release still forward to the origin.
- On the origin, `Has(svc)` means `rec.Origin == identity`.

### Non-origin Resolve

- Reads Redis.
- With no `ref:` values it computes `effectiveEnvAt` locally.
- With refs it calls `fetchFromOrigin` and keeps the last-known value in
  memory only.
- `FailedVersion` and `Healthcheck` move to the per-host hash, and the cache
  files are retired.
- Accepted regression: if a non-origin restarts while the origin is down and
  the env uses refs, it fails closed. Document this.

### One-time import

The import runs at startup, before `newEnvSyncManager` and `reconcileLoop`.

1. `SET NX` each `/data/central-env/<svc>.json` byte-for-byte, and `SADD` the
   index.
   - If Redis already has the same `Version` + `ResolvedHash`, that's fine.
   - If it differs, log it, keep Redis and leave the file.
2. For cache files with `FailedVersion != 0`, `HSETNX` into the per-host
   hash.
3. Once everything has imported, rename the directories to
   `*.imported-<unix>`.
4. `CENTRAL_ENV_IMPORT_DIR` allows a manual re-import.
5. Verify that `get_service_env` for badminton admin/player is identical
   before and after.

### Flag and rollout

`CENTRAL_ENV_STORE=redis|file`, default `file`. The Mac (origin) flips
first, then the Pi.

### Tests

- backend fake
- golden round-trip
- a conflict keeps the Redis value
- the rename happens only after every import succeeds
- a corrupt record marks the service broken
- non-origin local compute vs fetch
- `FailedVersion` carries over
- the existing centralenv tests become table-driven over both backends
- `centralenv_paths_test` and `centralenv_leak_test` updated

## ACL (separate small PR)

- `--aclfile /data/users.acl`.
- The proxy user can read labels (`%R~pmgr:labels:*`), read and write
  `pmgr:rl:*`, use the `pmgr:labels:changed` channel and run a limited set of
  commands.
- The dashboard user gets `~pmgr:*`.
- Keep the `default` user during migration, then disable it.
- `REDIS_USERNAME` for both binaries.
- Never log the password, and `/labels` never includes the address or
  credentials.
- README: anyone holding Redis credentials can change live routing, which is
  why auth keys stay container-only.

## README / compose

- The labels reference gets a "Live via Redis?" column.
- A Central labels section covers precedence, adopt/import, drift, the
  fail-static cache, flags, MCP tools and the ACL.
- `docker-compose.yml` and `.env.example`:
  - dashboard: `LABELS_CENTRAL`, `LABELS_WRITES`, `REDIS_USERNAME` (1b)
  - proxy: `REDIS_USERNAME` (1a)
- `-labels-cache` lives inside the existing `./cmd/proxy/data:/data` mount.
- Phase 3 adds `CENTRAL_ENV_STORE`.
- Deploy: the Pi compose is hand-maintained and the Mac has local edits, so
  hand-patch the env on both hosts. Verify via `/labels` `source` and
  `get_service_labels.applied`.

## Risks

- **A/B conflicts.** 1b touches one hunk in `api.go`, four small hunks in
  `docker.go` and the dashboard `main.go`. Phase 3 waits for the A/B work.
- **`feat-overlap-restart` conflicts.** 1b waits for it to merge and routes
  `overlapEnabled()` through `effectiveLabels`.
- **Ignored compose edits.** After adoption, compose edits to managed keys
  are ignored. The drift report shows them, and this must be documented
  loudly.
- **Map aliasing.** Always copy. Clone/create and adopt strictness read raw
  labels.
- **Stale `StopTimeout`.** Docker keeps the old `proxy.drain` value until the
  next recreate.
- **Canary weight.** Weight changes are refused while a canary is staged.
- **Redis down.**
  - Proxy: fail-static from memory plus the disk cache.
  - Dashboard writes return 503.
  - A dashboard cold start with Redis down falls back to raw labels
    (dashboard only), so autoupdate is skipped via `everLoaded`.
- **A/B overlay safety.** The overlay never touches `proxy.ab.*` and runs
  before `abScan`.
- **Reserved names.** Services named `index` or `version` would collide with
  the global keys. `labels.ValidService` rejects them on every path.
- **Write after a wipe.** A single-service write before a reseed would
  drop every other adopted service from the overlay. 1b writes must 503 or
  reseed while the global version key is missing.
- **Eviction.** Partial eviction of one `pmgr:labels:<svc>` hash looks like
  "every managed key unset" and is not detected as a wipe. The Mac Redis runs
  `maxmemory-policy volatile-lru`, and these keys never get a TTL, so they are
  never evicted. Keep it that way: never set a TTL on `pmgr:labels:*`, and
  never move to an `allkeys-*` policy.
- **Invalid values in Redis.** At load time an invalid value is dropped and
  treated as unset. For `proxy.drop_headers` this loosens header stripping.
  Values only become invalid through manual Redis edits, because every write
  path validates.
