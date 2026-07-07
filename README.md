# proxy-manager

> Self-hosted reverse proxy, load balancer, and management dashboard for a single host. Five small Go binaries with a tight dependency footprint (`webauthn` for passkeys, `x/crypto/acme` for TLS autocert, `rsc.io/qr` for TOTP QR codes — that's it).

Built for a Raspberry Pi running 10+ production services. Replaces the usual nginx + Traefik + Portainer + Homepage + assorted bash with ~10k lines of Go you can read in a weekend. Container labels are the source of truth — drop two labels on a service and it's routed.

---

## What you get

- **Label-driven routing.** `proxy.enable=true` + `proxy.host=foo.example` on any container — done. No config files to edit.
- **Weighted round-robin** across replicas with **automatic failover** and **per-backend health checks**.
- **Dashboard** with: live route table; scale services up/down; blue/green canary (stage → promote/discard); atomic replace + one-click rollback; Cloudflare DNS edits; per-target traffic stats and TLS cert expiry tracking.
- **Container logs viewer** (`docker logs` per container, with filter) and **proxy access log** (last 2000 requests with method / host / path / status / bytes / latency / client IP / backend).
- **Multi-user auth** with PBKDF2 passwords, TOTP 2FA (QR enrolment), and WebAuthn passkeys. Every write requires elevation. Per-user API tokens.
- **Opt-in SSO forward auth.** Drop `proxy.auth=true` on any routed service to gate it behind a single sign-on portal — one login covers every protected `*.<domain>` host. Bearer tokens for scripts, an OAuth 2.0 authorization server for claude.ai MCP connectors, optional LAN bypass. Default-off: a service with no `proxy.auth` label stays fully public.
- **Audit log** of every action, rate-limited login, **read-only** Docker socket on the request path.

---

## Architecture

```
                ┌──────────────────────────────────────────────┐
  internet ───→ │  edge  :443                       (OPT-IN)   │
                │  TLS via Let's Encrypt autocert + HSTS       │
                │  per-IP rate limit (optional peer gossip     │
                │    for cluster-wide caps) + body size cap    │
                │  gzip + X-Forwarded-* + access log           │
                └──────────────────────┬───────────────────────┘
                                       │ HTTP (internal)
                                       ▼
                ┌──────────────────────────────────────────────┐
                │  proxy  :8092             (READ-only socket) │
                │  label discovery + static routes             │
                │  weighted round-robin + retry                │
                │  background health checks                    │
                │  in-memory access log on :8094/access        │
                └──────────────────────────────────────────────┘
                                       ▲
                                       │ container labels
                                       ▼
                ┌──────────────────────────────────────────────┐
                │  dashboard  :8093        (READ-WRITE socket) │
                │  auth + 2FA + audit + rate-limited login     │
                │  service mgmt (scale/replace/stage/promote)  │
                │  DNS via Cloudflare API                      │
                │  container logs + proxy access log viewers   │
                │  Stats tab (forwards monitor + cert probe)   │
                └──────────────────────────────────────────────┘
                                       │
                                       │ /api/monitor/*
                                       ▼
                ┌──────────────────────────────────────────────┐
                │  monitor  :8095               (no host port) │
                │  scrapes proxy + edge + dashboard /metrics   │
                │  TLS handshake probing for cert expiry       │
                │  1h rolling time series in memory            │
                │  health classification: up / flaky / down    │
                └──────────────────────────────────────────────┘

                ┌──────────────────────────────────────────────┐
   proxy ─────→ │  auth  :8096                  (no host port) │
   (redirect    │  SSO login portal at auth.<domain>           │
    unauth'd    │  + OAuth 2.0 authorization server for MCP    │
    browsers)   │  verifies creds against dashboard,           │
                │  reads routed hosts from proxy /routes       │
                │  issues the HMAC pmgr_sso cookie the proxy   │
                │  verifies in-process (no shared state)       │
                └──────────────────────────────────────────────┘
```

Binaries stay loosely coupled:
- **proxy** watches Docker for labels and serves user traffic. It verifies the SSO cookie in-process with the shared secret — no call to the auth binary on the request path.
- **dashboard** watches Docker independently and manages services.
- **monitor** scrapes the others over the internal network — if it dies, requests still flow.
- **edge** is optional. If you already terminate TLS (nginx, Cloudflare Tunnel, Caddy), leave it off. Enable with `docker compose --profile edge up -d`.
- **auth** is only reached when a user actually signs in: it verifies credentials against the dashboard and reads routed hosts from the proxy, so the login flow depends on those two — but already-issued cookies keep working while it's down.

The request path survives any single binary crashing.

---

## Quick start

```bash
git clone https://github.com/PolarBaeJr/proxy-manager.git
cd proxy-manager
cp .env.example .env                # optional: Cloudflare token for DNS tab
docker compose up -d --build        # add --profile edge to enable TLS termination
```

Open `http://<host>:8093` — first-run setup creates the initial user and shows a TOTP QR code.

If the host is remote:
```bash
ssh -L 8093:localhost:8093 your-host
open http://localhost:8093
```

---

## Labels reference

Drop these on any container you want routed:

| Label | Required | Notes |
|---|---|---|
| `proxy.enable=true` | ✓ | opt in |
| `proxy.host=foo.example` | ✓ | match against request `Host` |
| `proxy.port=8080` | ✓ | container's **internal** port |
| `proxy.path=/admin` |   | path prefix for fan-out |
| `proxy.strip=true` |   | strip prefix before forwarding |
| `proxy.weight=2` |   | weighted RR (default 1) |
| `proxy.health=/healthz` |   | HTTP probe (default: TCP connect) |
| `proxy.service=myapp` |   | group key — unlocks scale/replace/canary in dashboard |
| `proxy.unscalable=true` |   | singleton (DB, bot, gateway) — disables scale buttons |
| `proxy.name=Friendly` |   | dashboard label |
| `proxy.auth=true` |   | require SSO login (default: public, no auth) — see [Access control](#access-control-opt-in-sso) |
| `proxy.auth.users=alice,bob` |   | optional allowlist; empty = any authenticated user |
| `proxy.auth.mode=oauth` |   | bearer-only OAuth mode for MCP servers (default is cookie SSO) |

Containers must share the **`edge`** Docker network with the proxy. See `examples/docker-compose-sample.yml`.

---

## Access control (opt-in SSO)

Every routed host is **public by default**. To put a service behind single sign-on, add one label:

```yaml
labels:
  proxy.enable: "true"
  proxy.host:   "grafana.example.com"
  proxy.port:   "3000"
  proxy.auth:   "true"                # require a logged-in user
  # proxy.auth.users: "alice,bob"    # optional allowlist; empty = any authenticated user
  # proxy.auth.mode:  "oauth"        # bearer-only OAuth mode (for MCP servers)
```

A service with no `proxy.auth` label is untouched — no cookie check, no redirect, exactly as before.

**How login works.** The first time a browser hits a protected host it's redirected to the SSO portal at `auth.<domain>` (the `auth` binary). The portal validates username + password + TOTP against the dashboard's user store and, optionally, **passkeys** (WebAuthn, enrolled per parent domain on the portal — a separate credential set from the dashboard's own passkeys). On success it sets an HMAC-signed `pmgr_sso` cookie scoped to the parent domain with a ~30-day lifetime. One login covers **every** protected `*.<domain>` host; the proxy verifies the cookie in-process with the shared secret, so no round-trip to the auth binary on the request path.

**Scripts and non-browser clients.** A dashboard-issued `pmt_` API token works on protected hosts too — pass it as `Authorization: Bearer pmt_XXXX`. No cookie, no browser required.

**LAN bypass.** `AUTH_TRUSTED_CIDRS` lets configured client IPs skip auth entirely (cookie-mode hosts only; OAuth-mode hosts are always bearer-only). Client-IP resolution is spoof-resistant — `X-Forwarded-For` is trusted only from configured upstream CIDRs.

**OAuth for MCP.** Set `proxy.auth.mode=oauth` on an MCP server host and the `auth` binary acts as an OAuth 2.0 authorization server for claude.ai connectors and Claude Code: dynamic client registration, PKCE S256, authorization-code + refresh, and a consent page. The host is bearer-token-only — no cookie, no LAN bypass — and returns a `401` challenge (not a login redirect) carrying RFC 9728 resource metadata so MCP clients can discover the authorization server. Access tokens are stateless HMAC blobs the proxy verifies in-process.

**Configuration.** Auth is off until you turn it on. Set these in `.env`:

| Env var | Default | Meaning |
|---|---|---|
| `PMGR_AUTH_SECRET` | *(empty)* | Shared HMAC secret, required by `proxy` **and** `auth` once auth is enabled. Generate with `openssl rand -hex 32`. |
| `AUTH_DOMAINS` | *(empty)* | Parent domains that have an `auth.<domain>` login host (e.g. `example.com`). Empty = auth gate disabled. |
| `AUTH_TRUSTED_CIDRS` | *(empty)* | CIDRs that bypass auth on cookie-mode hosts (e.g. your LAN range). |
| `AUTH_PASSKEY_DOMAINS` | *(empty)* | Parent domains to enable portal passkeys for. Empty = passkeys off. |

**Fail-closed.** A missing secret or an unknown domain returns `503`, never open access.

---

## Repository layout

```
cmd/
  edge/         outermost TLS / WAF binary       (~800 LOC, opt-in)
  proxy/        request-path binary              (~1000 LOC, exposes /metrics + /access)
  dashboard/    management UI binary             (~7900 LOC, single-file embedded HTML)
  monitor/      scrapes proxy + edge metrics     (~800 LOC, time series + cert probe)
  auth/         SSO portal + OAuth AS binary     (~1900 LOC, no host port, reached via proxy)
internal/
  httpx/        shared HTTP helpers (WriteJSON / WriteErr)
examples/
  docker-compose-sample.yml   minimal labelled compose file
scripts/
  cname         Cloudflare DNS CLI (alternative to the DNS tab)
docs/
  DESIGN_BRIEF.md   archived design handoff (Vercel/Geist dashboard rebuild)
  PEERS_PLAN.md     draft design for federated multi-host support
go.mod          single module: github.com/PolarBaeJr/proxy-manager
docker-compose.yml
.env.example
```

Standard Go layout: one `go.mod` at the root, one binary per `cmd/<name>/`, shared code under `internal/`. Each binary's Docker image builds from the repo root (`context: .`) and only `COPY`s its own `cmd/<name>/` plus `internal/`, so the build context stays tight even though all five binaries share one module. The dashboard's HTML lives inside a Go raw string in `cmd/dashboard/ui.go` — no bundler, no node_modules.

---

## Observability

Each binary exposes an internal `/metrics` endpoint (port `:8094` on edge / proxy / dashboard; `:8095` on monitor) returning JSON:

- per-host, per-status, per-method request counts
- per-host × per-status counts (error rate per route)
- latency percentiles (p50/p90/p95/p99)
- in-flight requests, bytes out
- rate-limit hits (edge only)

The proxy additionally exposes **`/access`** on :8094 — the last 2000 requests as JSONL-ish entries (method, host, path, status, bytes, ms, client IP, backend, UA). The dashboard's **Access** tab forwards through its auth layer.

The `monitor` binary scrapes the others on a 5s tick, keeps a 1h rolling time series in memory, TLS-probes external hosts you specify, and exposes:

- `/api/snapshot` — every target's latest sample + health
- `/api/series` — recent points of any scalar field (sparkline-ready)
- `/api/overview` — dashboard summary
- `/api/certs` — probed certificate expiry / issuer / status

The dashboard's **Stats** tab proxies these through its auth boundary — monitor is never exposed publicly.

---

## Programmatic access

Two ways to call the dashboard from external tools:

**1. Public health endpoint (no auth).** Returns only per-binary up/degraded/down — no host names, no traffic counts. Safe for status pages and uptime monitors.
```bash
curl https://dashboard.example.com/api/health
# {"status":"up","targets":[{"name":"proxy","health":"up"}, ...],"checked_at":"..."}
```

**2. API tokens (full access).** Generate per-user from the dashboard Users tab. Pass as a bearer token:
```bash
curl -H 'Authorization: Bearer pmt_XXXX' https://dashboard.example.com/api/monitor/overview
curl -H 'Authorization: Bearer pmt_XXXX' https://dashboard.example.com/api/services
curl -H 'Authorization: Bearer pmt_XXXX' -X POST https://dashboard.example.com/api/services/myapp/scale -d '{"replicas":3}'
curl -H 'Authorization: Bearer pmt_XXXX' "https://dashboard.example.com/api/access?limit=500"
curl -H 'Authorization: Bearer pmt_XXXX' https://dashboard.example.com/api/logs/myapp?tail=200
```

Tokens are shown once at creation and stored only as SHA-256 hashes. Revoke from the same UI. The same `pmt_` token also authenticates against any `proxy.auth`-protected host (see [Access control](#access-control-opt-in-sso)) — handy for scripts that hit a gated service.

---

## Security

- 2FA required for **every** write (scale, create, delete, DNS edit, user mgmt).
- TOTP secrets and password hashes live in `cmd/dashboard/data/auth.json` (mode `0600`, gitignored).
- Login and setup endpoints are rate-limited per IP.
- All writes append to `cmd/dashboard/data/audit.log` as JSONL.
- The proxy's Docker socket mount is **read-only** — RCE on the request path can't create or destroy containers.
- Cookies are HMAC-signed, HttpOnly, SameSite=Lax. API tokens and peer-gossip bearers use `crypto/subtle.ConstantTimeCompare` against SHA-256 hashes.
- `proxy.*` container labels are allowlist-validated at ingestion (backend regex + frontend `pattern=`) so a rogue image can't inject HTML/JS into the dashboard by putting a payload in a `LABEL` line.
- The repo ships unit tests (`go test ./...`, including the auth gate, client-IP resolution, and OAuth token handling). Every PR runs a required `test` GitHub Actions check plus a Claude security-review action; `main` is branch-protected on both.

The proxy and dashboard bind to `127.0.0.1` on the host (loopback-only) — they're reached over the internal Docker network or through host-side nginx / the opt-in `edge` binary. Neither is exposed to the LAN directly, which is what would otherwise leak the dashboard's plaintext login page.

---

## License

[MIT](LICENSE) © Matthew Cheng
