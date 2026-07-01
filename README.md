# proxy-manager

> Self-hosted reverse proxy, load balancer, and management dashboard for a single host. Four small Go binaries, no external runtime dependencies.

Built for a Raspberry Pi running 10+ production services. Replaces the usual nginx + Traefik + Portainer + Homepage + assorted bash with ~4000 lines of Go you can read in an afternoon. Container labels are the source of truth — drop two labels on a service and it's routed.

---

## What you get

- **Label-driven routing.** `proxy.enable=true` + `proxy.host=foo.example` on any container — done. No config files to edit.
- **Weighted round-robin** across replicas with **automatic failover** and **per-backend health checks**.
- **Dashboard** with: live route table; scale services up/down; blue/green canary (stage → promote/discard); atomic replace + one-click rollback; Cloudflare DNS edits; per-target traffic stats and TLS cert expiry tracking.
- **Container logs viewer** (`docker logs` per container, with filter) and **proxy access log** (last 2000 requests with method / host / path / status / bytes / latency / client IP / backend).
- **Multi-user auth** with PBKDF2 passwords and TOTP 2FA via QR code. Every write requires elevation. Per-user API tokens.
- **Audit log** of every action, rate-limited login, **read-only** Docker socket on the request path.

---

## Architecture

```
                ┌──────────────────────────────────────────────┐
  internet ───→ │  edge  :443                       (OPT-IN)   │
                │  TLS via Let's Encrypt autocert              │
                │  per-IP rate limit + body size cap           │
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
```

Binaries don't depend on each other:
- **proxy** watches Docker for labels and serves user traffic.
- **dashboard** watches Docker independently and manages services.
- **monitor** scrapes the others over the internal network — if it dies, requests still flow.
- **edge** is optional. If you already terminate TLS (nginx, Cloudflare Tunnel, Caddy), leave it off. Enable with `docker compose --profile edge up -d`.

Any one binary can crash without taking the others with it.

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

Containers must share the **`edge`** Docker network with the proxy. See `examples/service.yml`.

---

## Repository layout

```
cmd/
  edge/         outermost TLS / WAF binary       (~600 LOC, opt-in)
  proxy/        request-path binary              (~1000 LOC, exposes /metrics + /access)
  dashboard/    management UI binary             (~3000 LOC, single-file embedded HTML)
  monitor/      scrapes proxy + edge metrics     (~500 LOC, time series + cert probe)
internal/
  httpx/        shared HTTP helpers (WriteJSON / WriteErr)
examples/
  service.yml   minimal labelled compose file
scripts/
  cname         Cloudflare DNS CLI (alternative to the DNS tab)
docs/
  DESIGN_BRIEF.md   archived design handoff (Vercel/Geist dashboard rebuild)
  PEERS_PLAN.md     draft design for federated multi-host support
go.mod          single module: github.com/PolarBaeJr/proxy-manager
docker-compose.yml
.env.example
```

Standard Go layout: one `go.mod` at the root, one binary per `cmd/<name>/`, shared code under `internal/`. Each binary's Docker image builds from the repo root (`context: .`) and only `COPY`s its own `cmd/<name>/` plus `internal/`, so the build context stays tight even though all four binaries share one module. The dashboard's HTML lives inside a Go raw string in `cmd/dashboard/ui.go` — no bundler, no node_modules.

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

Tokens are shown once at creation and stored only as SHA-256 hashes. Revoke from the same UI.

---

## Security

- 2FA required for **every** write (scale, create, delete, DNS edit, user mgmt).
- TOTP secrets and password hashes live in `cmd/dashboard/data/auth.json` (mode `0600`, gitignored).
- Login and setup endpoints are rate-limited per IP.
- All writes append to `cmd/dashboard/data/audit.log` as JSONL.
- The proxy's Docker socket mount is **read-only** — RCE on the request path can't create or destroy containers.
- Cookies are HMAC-signed, HttpOnly, SameSite=Lax. API tokens use constant-time compare against SHA-256 hashes.

There is no TLS on the dashboard itself — front it with nginx + Let's Encrypt, use the opt-in `edge` binary, or only access via SSH tunnel before exposing it publicly.

---

## License

[MIT](LICENSE) © Matthew Cheng

