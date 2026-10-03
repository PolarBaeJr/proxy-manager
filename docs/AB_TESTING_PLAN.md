# A/B testing — implementation plan

Status: approved by the owner 2026-10-02. Build in this order:

- PR0: proxy race fix (separate branch).
- PR1: proxy core.
- PR2: proxy mesh.
- PR3: dashboard backend.
- PR4: MCP, UI and README.

This file is the single source of truth. Coder prompts reference it by section.

## 0. Owner decisions (fixed)

1. **Variants.**
   - B is its own canary replicas with a different **image** (image-only in v1; any `env` in the start request → 400).
   - A = the current live replicas.
   - `NEXT_PUBLIC_*` values are baked in at build time, so client-visible differences need a different image.
   - If B is only a feature flag on the same image, the app should gate it with `ab_group` itself; proxy A/B is for different builds.
2. **Measurement.** The proxy records per-variant stats: requests, error rate (5xx + transport failures; 4xx excluded), p50/p95. It forwards `X-Variant: A|B` to the backend so the app can log conversions.
3. **Ending.** The owner picks the winner (promote B / discard B). Auto-abort also runs and stops sending new sessions to B when B is clearly worse.
4. **Assignment unit = account, with groups.**
   - The app sets `ab_group=<name>` and `ab_uid=<opaque hash>` cookies.
   - The proxy maps groups to variants and splits ungrouped accounts by hash.
   - The proxy NEVER parses the Supabase auth cookie.
5. **Never swap a visitor mid-session.**
   - A session pin decides the variant for the whole session.
   - Abort, promote, discard, split and group changes affect NEW sessions only.
   - The old variant's replicas stay up until its pinned sessions drain.
   - The only exception is a real outage (B unhealthy or unreachable): that is failover, and the pin is kept.
6. **Excluded paths** always go to A, get no pin, and are not counted. Badminton default: `/api/cron,/api/webhooks,/api/passkey,/api/discord,/api/calendar,/api/health`.
7. **Experiments are per SERVICE (`proxy.service`), not per host.** Prod and staging share hostnames.
8. **Central env during a test: ACCEPT and defer.**
   - Edits land in the central store (version bump) but propagation is deferred while the test runs.
   - At promote finalize (or discard completion) the deferred propagation runs.
   - Promote must not fail on the env-version mismatch: for an A/B canary, skip the `labelEnvVersion` check in `promoteCanary`, then request an env sync so the promoted replicas move to the current version through the normal health-gated propagation.
   - The UI/API shows "env vN pending — applies after the test".
9. **Max drain = 24h** (`max_session` default 24h): the longest a pinned session keeps its variant before cutover. `force` skips the drain.
10. **First target:** badminton-player on staging, with B on the Mac (the public entrance), then prod player once PR2 is live on both proxies.

Defaults taken without asking (list them in the PR bodies):

- An inconclusive window (fewer than `window_min_samples` per variant) leaves the abort streak unchanged.
- After an abort drains, B is removed only by the operator.
- The `ab_anon` per-browser cookie is on for signed-out visitors.
- The QA override `?pm_variant=` is opt-in by label and off by default; prefer a `qa:B` group.
- A→B failover never happens while running.
- B must run on the entrance host until PR2 is deployed on both proxies.
- No extra persistence of the abort latch in the proxy's metricsState; the phase label is the durable record.

## 1. Design

### 1.1 Concept

- An A/B test is a canary (`proxy.canary=true`) plus `proxy.ab.*` labels on the B replicas. The existing canary guards apply automatically.
- A new dashboard `abManager` (shaped like `rolloutManager`) owns the lifecycle and a lock per service.
- Reused: `createCanaryReplicas` (new `extraLabels` param), `waitForCanaryHealthy`, `promoteCanary`, `discardCanary`.
- A canary without `proxy.ab.*` labels behaves exactly as today.

### 1.2 How the proxy knows A from B

- `proxy.ab.variant=B` plus a valid `proxy.ab.id` makes a backend B. No label means A.
- `variant=A` is a no-op. Any other value is logged once and the backend is left out of routing (fail closed).
- Config is read from the B container with the smallest name; the dashboard writes identical labels on every B.
- If one service has two ids, the smallest wins and the rest are left out.
- A test attaches to a RouteGroup only if a B-capable backend exists (local or learned from a peer).
- Static routes.json `service:` routes are not part of tests in v1.

### 1.3 Assignment (in `Router.ServeHTTP`, after auth, BEFORE the cache decision and the DropHeaders loop)

Modes (`proxy.ab.assign`):

- `cookie` (default): browsers. Uses the session pin.
- `header:<Token>`: API clients. Hash of the header value; no cookie, no pin.
- `random`: chosen per request.

Precedence, first match wins:

1. Excluded path (prefix match on the original client path) → A. No pin, not counted.
2. Authenticated peer hop → incoming `X-Variant` if it is exactly `A` or `B`. Never issues or refreshes a pin.
   - CHECK how the hop marker is authenticated today. If a client can forge it, strip it at the entrance or require the peer secret.
   - Unauthenticated hops must not take this path and must not be recorded.
3. Valid pin whose id matches the current test → pinned variant. `ab_group` and `ab_uid` are ignored.
4. QA override `?pm_variant=A|B`:
   - only if `proxy.ab.override=true`;
   - strip only that query key;
   - create a pin with `src=q`, which is routed but never counted.
5. Header mode → `h % 10000 < split*100`.
6. Random mode → per request.
7. `ab_group` valid and in the groups map → mapped variant. A valid group that isn't in the map counts as ungrouped.
8. `ab_uid` valid → split hash.
9. Otherwise (signed out), split-hash `ab_anon`:
   - The proxy sets `ab_anon` once: 16 random hex, Max-Age 30d, HttpOnly, Secure, SameSite=Lax, Path=/.
   - With `proxy.ab.anon=false`, draw randomly per session instead.
10. Steps 7–9 then create the pin (cookie mode only).

Hash:

- `h` = first 8 bytes, big-endian, of `sha256(id + "\x00" + value)`.
- The test id is part of the hash on purpose: each test gets its own cohort, and a reset reshuffles it.
- Both proxies agree.

Phase overrides (§1.10): in `aborted`/`discarding` new sessions go to A; in `promoting` new sessions go to B. Valid pins always keep their variant.

App cookies (read only, never set or cleared by the proxy):

- Names are labels: `uid_cookie` defaults to `ab_uid`, `group_cookie` to `ab_group`.
- Values:
  - ab_uid: `^[A-Za-z0-9_-]{1,64}$`;
  - ab_group: `^[a-z0-9_-]{1,32}$`;
  - anything else counts as absent.
- They are read only when a pin is created, so membership changes apply from the next session.

Session pin (**unsigned, strictly parsed**; there is no per-test key, see §1.10 for why):

- Name: `ab_v_` + the first 8 hex chars of sha256(service).
- Value: `<id>.<A|B>.<src g|u|n|q>.<nonce8hex>.<issued>.<lastSeen>`, at most 96 bytes, strict regex.
- Attributes: HttpOnly, Secure, SameSite=Lax, Path=/. No Max-Age or Expires (session cookie).
- Valid when all of these hold:
  - the id matches the current test;
  - now − lastSeen < `session_idle`;
  - now − issued < `max_session`;
  - lastSeen ≤ now + 2m.
- Refreshed with a Set-Cookie at most every `pin_refresh`; never on hopped, static or excluded requests.
- A forged pin only lets a visitor choose their own variant or extend their own session, and the server-side drain deadline caps that (§1.10).
- Only the proxy's own cookie names are added. Other Set-Cookie headers are never touched (same pattern as `setStickyCookie`).
- Optional JS mirror (`proxy.ab.cookie_js=true`): `ab_vjs_<hash>` holding `A` or `B`, session-scoped, not HttpOnly.

### 1.4 Sticky sessions inside a variant

- A `proxy.sticky` pin is honoured only if the pinned backend's variant matches the session's variant.
- Otherwise the normal pick runs, restricted to that variant's pool.

### 1.5 X-Variant and Vary

- Groups with a test (any phase), on non-hopped requests:
  - delete the client's `X-Variant`;
  - before each attempt, set `X-Variant` from the constants `"A"`/`"B"` = the variant actually served. After failover that is the failover target.
- Groups without a test: leave the client header UNTOUCHED (no regression).
- Vary on non-static, non-excluded responses of an active test: `Vary: Cookie` in cookie mode, `Vary: <Name>` in header mode. Never on static paths.

### 1.6 Micro-cache

- For an active test, set `cacheable=false` explicitly on every non-static path, including first visits with NO cookies.
- Static paths stay cacheable.
- A cache HIT returns before `proxyToGroup`, so assignment must come first.

### 1.7 Failover and static retry

Failover:

- Running: B-pinned with no healthy B → A.
- Promoting: A-pinned with no healthy A → B.
- Running: A-pinned never goes to B.
- Counted in `failover_<pinned>`, never in A's stats.
- The pin is kept, so sessions return when B recovers.
- B failover counts as a B error when judging abort.

Static 404 retry (GET/HEAD under `proxy.ab.static`, default `/_next/static/`; `none` disables it):

- The first attempt goes through `abStaticWriter`, which has its own header map. A 404 is swallowed; any other status is copied through. It passes `SetBackend`/`Flush`/`Hijack` through.
- On a swallowed 404, retry once on the other variant, unwrapped.
- Never counted; `static_fallbacks` is kept for display.

### 1.8 Recording and judging

Recording:

- Each request is recorded once, by the proxy whose LOCAL backend served it (or last tried it).
- A forwarding proxy doesn't record the request; it counts `hop_errors` for display only.
- Not recorded for judging: excluded paths, `src=q` pins, static retries, unauthenticated hops, and anything after the freeze.
- Per variant, per window: requests, err5xx, transport, failover, and a 64-bucket log latency histogram (ratio 1.2, 1ms–~120s; bounds exposed as `hist_bounds_ms`).
- Error rate = (5xx + transport + failover) / (requests + transport + failover).

Windows:

- Tumbling windows anchored at `epoch + warmup`, so both proxies judge the same windows.
- Ring of 3 windows plus cumulative counters.
- Warm-up traffic is counted separately.

Pinned activity (non-hopped requests with a valid pin), per variant:

- `last_pinned_at`;
- an approximate count of active pins: nonce → lastSeen, pruned after `session_idle`, capped at 50k.

On abort:

- Judging stats freeze.
- `draining` counters keep counting pinned traffic for display only.

`judge(cfg, own, peers)` is a pure function, run every 10s over merged own + fresh peer data:

1. Preconditions:
   - cumulative ≥ `min_samples` (500) for both variants;
   - now ≥ `started + min_runtime` (15m);
   - not already aborted.
2. A window is judged only if each variant has ≥ `window_min_samples` (50) in it; otherwise it is neutral.
3. A judged window is bad if either rule fires:
   - errors: `errB > errA + err_delta/100` AND `errB ≥ err_ratio·errA`;
   - latency: `p95B > p95_ratio·p95A + p95_slack`.
4. Abort when the last `windows` (2) judged windows are all bad.

### 1.9 Peer mesh (PR2)

`peerRouteInfo` adds, all omitempty:

- `ABID`, `BBackends`, `BWeight`;
- `Backends`/`Weight` stay totals for old receivers.

`peerRoutePayload.Experiments []peerABInfo`, re-validated on receipt:

- Service, ID, normalized Config, Phase, PhaseAt, AbortReason, Pinned;
- Cumulative and Windows (≤3, Hist length exactly 64);
- at most 64 experiments.
- NO key, because there is none.

Receiver:

- `merge` reports a change on any AB, phase or B-count change.
- `overlay` builds separate A and B synthetic backends (stickyID `url|B`, Container `peer:<id>:B`).
- `Router.Set`'s health carry-over key includes the variant.
- `Router.applyPeerAB` writes runtime state directly, so an abort latch propagates within about 5s.
- A proxy with no local test adopts the peer's config; local config wins on conflict.
- The abort latch is a union by id.

Routing and compatibility:

- Variant-filtered tiers: local B → learned B → hop with `X-Variant`.
- An older sender produces one A backend. An older receiver forwards everything.
- Both proxies must be upgraded before B runs off the entrance host.

Known gaps (PR2):

- A static-404 retry that lands on a peer is counted by that peer.
- On a test-id conflict (local labels win), authenticated A hops to the peer are counted under the peer's test.
- A receiver still in `running` honours a promoting forwarder's failover hop (the forwarder is trusted).
- If the worst-case payload exceeds 1 MiB, the sender drops all experiments from the push (logged); routes still sync.
- Old receivers never record hopped requests — upgrade both proxies before B runs off the entrance host.

### 1.10 Phases, abort and drain

Phase labels:

- `proxy.ab.phase`: running | aborted | promoting | discarding;
- `proxy.ab.phase_at`: unix seconds;
- `proxy.ab.abort_reason`: errors | latency | manual.

| Phase | New sessions | A pins | B pins | header/random | Judging |
|---|---|---|---|---|---|
| running | §1.3 | A | B | split | on |
| aborted / discarding | A | A | B | A | frozen |
| promoting | B | A | B | B | off |

Auto-abort:

1. The proxy latches the abort in memory.
2. The peer receives it within about 5s.
3. Within about 15s, the dashboard `abManager.Run` relabels B with phase=aborted (surge recreate) so it is durable. B stays up.

Drained, for variant v:

- either no pinned traffic for v across the mesh for `session_idle`: `max(last_pinned_at) + session_idle < now`;
- or `now ≥ phase_at + max_session` (the server-side deadline, capped at 24h by default).
- If a proxy is unreachable, only the deadline applies.
- `force` skips the wait.
- As built (PR3): the idle path is trusted only when every proxy (local + each peer via `/peer/ab`) is reachable, lists this test id, and reports `tracking_since` (when it began tracking pins for the id). The idle clock runs from `max(last_pinned_at, latest tracking_since)`, so a restarted proxy's empty pin table can't end a drain early. `last_pinned_at` is the max ever observed, persisted in the record (`pinned_max_a/b`). Anything less → deadline only.
- Finalize promote first rolls every peer running the service onto B's image (see §3, promote-replace), one peer at a time, health-gated; a peer failure leaves the promote pending for `Run` to retry (peers already done are skipped).

### 1.11 Split and groups changes

- These relabel B with the same id: a surge recreate with a health gate, then `proxyRefresh`.
- They bump `epoch`, so warm-up restarts.
- Existing pins stay; new sessions see the change.
- Reset issues a new id and invalidates every pin, so it is allowed only once B is drained (or with force).
- As built (PR3): B replicas get fixed-width names (`goproxy-<svc>-canary-NNNNNN`) and a reset's new id sorts after the old one. The proxy reads config from the B replica with the smallest (id, name), so during a surge the pick stays on an old replica and switches once, to a new replica that already passed its gate.

### 1.12 Dashboard guards while a test is active

- `abActive` is true if local labels OR the local proxy's `/ab` list the test.
- The guards go in BOTH `api.go` and `peers.go` `peerServicesMutateHandler`.

| Action | During a test |
|---|---|
| replace, rolling-replace, spread, stage, rollout | 409 (stage/rollout also guarded explicitly, not just via the canary check — B may run on a peer) |
| plain promote / `DELETE /canary` on an A/B canary | 409 "use the A/B endpoints" |
| weight | already refused (a canary exists) |
| scale A | allowed (templates from live replicas only, never a B/canary) |
| autoupdate | deferred |
| central env edit | ACCEPTED, stored; propagation ends as `deferred_ab_test` (checked under the service claim, origin and peer side) and is re-requested after finalize |
| central env adopt / release | 409 |
| onboarded services | A/B refused in v1 |

## 2. Labels (B replicas only, written by the dashboard)

| Label | Default | Allowed values |
|---|---|---|
| proxy.ab.variant | — | B (A = no-op; anything else excludes the backend) |
| proxy.ab.id | — | `^[a-z0-9]{4,16}$` (dashboard: 8 hex from crypto/rand) |
| proxy.ab.assign | cookie | cookie \| random \| header:<RFC7230 token ≤64B, not Cookie/Host> |
| proxy.ab.split | 10 | int 0..100 (ungrouped and anonymous only) |
| proxy.ab.groups | "" | ≤32 entries of `name:A\|B`, names `[a-z0-9_-]{1,32}` |
| proxy.ab.uid_cookie / group_cookie | ab_uid / ab_group | cookie-name token ≤64B |
| proxy.ab.anon | true | bool |
| proxy.ab.session_idle | 30m | 5m..24h |
| proxy.ab.pin_refresh | 5m | 1m..session_idle/2 |
| proxy.ab.max_session | 24h | 1h..7d |
| proxy.ab.started | — | unix seconds within ±1d of now |
| proxy.ab.epoch | = started | unix seconds (warm-up anchor) |
| proxy.ab.phase / phase_at / abort_reason | running / — / — | see §1.10 |
| proxy.ab.exclude | "" | ≤32 prefixes, each starting with `/`, ≤256B, no control chars |
| proxy.ab.static | /_next/static/ | same rules; `none` disables |
| proxy.ab.cookie_js | false | bool |
| proxy.ab.override | false | bool |
| proxy.ab.autoabort | true | bool |
| proxy.ab.min_samples | 500 | 1..1e7 |
| proxy.ab.min_runtime | 15m | 0..24h |
| proxy.ab.warmup | 3m | 0..1h |
| proxy.ab.window | 5m | 1m..1h |
| proxy.ab.windows | 2 | 1..12 |
| proxy.ab.window_min_samples | 50 | 1..1e6 |
| proxy.ab.err_delta | 2 | 0.1..100 (percentage points) |
| proxy.ab.err_ratio | 2 | 1..100 |
| proxy.ab.p95_ratio | 1.5 | 1..100 |
| proxy.ab.p95_slack | 200ms | 0..60s |

- Floats are parsed with `ParseFloat`; NaN and Inf are rejected.
- An invalid value is logged once and the default is used. A bad id or variant instead leaves the backend out.
- Peer-received config is re-validated with the same parser.

## 3. Wire formats

Proxy `GET /ab[?service=]`:

- Served on the metrics port, read-only.
- Fields: hist_bounds_ms, and experiments[]:
  - service, id, phase, phase_at, config, started, epoch;
  - abort{reason, detail, at, source};
  - b_backends_local, b_backends_peer;
  - cumulative{A,B}, windows[], frozen, warmup;
  - pinned{A,B: active_sessions, last_seen}, failover{A,B}, draining{A,B};
  - hop_errors, static_fallbacks;
  - peers[].

Dashboard REST under `/api/services/{name}/`:

- All routes are `requireElevated` and forwarded with `?host=` to `/peer/services/{name}/ab*`.
- `POST ab`: start. Body: image, split, replicas (1..10), assign, header, groups, uid_cookie, group_cookie, anon, session_idle, pin_refresh, max_session, exclude[], static[], cookie_js, override, thresholds{}. `env` → 400.
- `GET ab`: status.
  - state, phase, pending (promote | discard | null);
  - drain{variant, active_sessions_approx, last_pinned_at, drained, drains_by};
  - merged stats {A,B: requests, errors, error_rate, p50_ms, p95_ms}, windows, per_host;
  - abort, hint (z-test on error rate), env_pending_version, history.
- `POST ab/split`, `ab/groups`, `ab/abort`, `ab/promote {force, confirm_aborted}`, `ab/discard {force}`, `ab/reset {force}`.
- As built (PR3): every POST is async — it persists the intended record and answers `202` with `{id, phase, pending, op}`; container work runs in the background. Poll `GET ab`: `op` clears once applied, `last_error` says why not.
- New read-only `GET /peer/ab?service=`: bearer `DASHBOARD_PEER_SECRET`, relays the local proxy's `/ab`. `GET /peer/services/{name}/ab` also needs only the secret.
- New `POST /peer/services/{name}/ab/promote-replace` (secret + `-peer-writes`), body `{id, image}`: the owner's promote asks a peer to rolling-replace its A replicas onto B's image. Refused unless the peer's own proxy lists that test id (not discarding) and the peer doesn't hold B itself. Poll the peer's `GET .../rolling-replace`.
- Proxy `/ab` gained `tracking_since` per experiment (unix s) for the drain rule above.

## 4. PR scopes

- **PR0** (branch fix-proxy-errorhandler-race):
  - set `ErrorHandler` once, with per-request state carried in the request context;
  - no A/B code;
  - deploy and verify before PR1.
- **PR1 — proxy core, single proxy, inert without labels:**
  - labels and parser;
  - `Backend.Variant`;
  - variant-filtered picks;
  - §1.3–1.8 (cookies, pin, X-Variant, Vary, cache, failover, static retry, recording, windows, judge, latch);
  - phase handling from labels;
  - `GET /ab`;
  - evaluator goroutine;
  - hop-marker trust check;
  - no-regression tests for routes without a test.
- **PR2 — proxy mesh:** §1.9.
- **PR3 — dashboard backend:**
  - `abManager` + store (`-abtests` flag, default `/data/abtests.json`);
  - `createCanaryReplicas` `extraLabels`;
  - `promoteCanary` strips `proxy.ab.*` and skips the env-version check for A/B;
  - phases, drain, `Run`, guards (§1.12) in api.go + peers.go;
  - deferred central env (§0.8);
  - REST, `/peer/ab`, `Service.ABTest`, audit.
- **PR4:**
  - MCP tools: start_ab_test, get_ab_test (read-only), set_ab_split, set_ab_groups, resolve_ab_test (promote | discard | abort | reset, force, confirm_aborted);
  - UI card, start dialog, live A-vs-B table;
  - README labels and an A/B section.

## 5. Tests (all `go test -race`)

See each PR's scope. Required in every PR:

- tests that actually exercise the new code;
- no-regression tests;
- tests for the security-relevant parsing bounds;
- a security checklist in the PR body:
  - no mutating endpoint on the metrics port;
  - cookie attributes and strict parsing;
  - `X-Variant` written only from constants;
  - label bounds;
  - peer payload re-validated and size-limited;
  - QA override off by default;
  - Authorization, header-mode values and uid never logged;
  - write routes gated by elevated session, `-peer-writes`, and `MCP_ALLOW_WRITES` / `MCP_ALLOW_PEER_WRITES`.

## 6. Risks

- **Long drains:** keep capacity for both variants for up to 24h; `force` is the way out.
- **Recreates:** every phase, split or groups change recreates B (labels are immutable). Always `proxyRefresh` afterwards.
- **Mixed proxy versions:** keep B on the entrance host until PR2 is on both proxies.
- **Low traffic** may never reach the thresholds, so there is no auto-abort; the UI shows "insufficient samples".
- **Flooding B** can force an abort; that fails safe.
- **Clock skew** between hosts misaligns windows; pins tolerate 2m.
- **Cloudflare HTML caching** must stay off for A/B hosts; check zone rules before the prod run.
