# proxy-manager

Self-hosted reverse proxy + load balancer + management dashboard for a single host (Raspberry Pi, VPS, anything that runs Docker).

Two small Go binaries, zero external dependencies aside from one QR library:

| Binary | Port | Role | Docker socket |
|---|---|---|---|
| **proxy** | 8092 | Reverse proxy + weighted round-robin LB + health checks | read-only |
| **dashboard** | 8093 | Auth + 2FA UI for services / DNS / users | read-write |

## Routing model

Any container with `proxy.enable=true` and a `proxy.host` label is routable. The proxy reads container labels from the Docker socket, builds an in-memory route table, and round-robins traffic across all healthy backends sharing the same `proxy.host` + `proxy.path`.

For backends not in containers (host-bound services, third-party stuff), drop entries into `proxy/routes.json`.

## Quick start

```bash
# Build + run both binaries:
cp .env.example .env                # optional: fill in CLOUDFLARE_* to enable the DNS tab
docker compose up -d --build

# Visit the dashboard (use SSH tunnel if remote):
#   ssh -L 8093:localhost:8093 your-host
open http://localhost:8093          # first-run setup screen
```

First-run setup creates the initial user with TOTP 2FA (QR code shown).

## Labels reference

| Label | Required | Notes |
|---|---|---|
| `proxy.enable=true` | yes | Opt the container into routing |
| `proxy.host=foo.example` | yes | Match against request Host header |
| `proxy.port=8080` | yes | Container's *internal* port |
| `proxy.path=/admin` | no | Path prefix for fan-out (longer prefixes win) |
| `proxy.strip=true` | no | Strip the prefix before forwarding |
| `proxy.name=Friendly` | no | Dashboard display name |
| `proxy.weight=2` | no | Weighted RR (default 1) |
| `proxy.health=/healthz` | no | HTTP health probe (default = TCP connect) |
| `proxy.service=myapp` | no | Group key for scale/replace/canary in the dashboard |
| `proxy.unscalable=true` | no | Mark singleton (DB, gateway, bot) — disables +/- |

Containers must share the **`edge`** network with the proxy so it can reach them by IP.

## Dashboard capabilities

- **Routes** — every active route + backend health, auto-refresh
- **Services** — scale + / − , create from form, Stage (canary), Promote, Discard, Replace, Rollback, Delete
- **DNS** — list / create / edit / delete CNAME, A, TXT records on a Cloudflare zone (uses your scoped API token)
- **Users** — multi-user, per-user TOTP, add/delete users
- **Header** — live host CPU / RAM / disk stats
- **Auth** — PBKDF2 password + RFC 6238 TOTP, HMAC-signed session cookies, rate limiting, audit log

## Repo layout

```
proxy/                  # request-path binary (~700 LOC)
  main.go               # wiring
  router.go             # routing, weighted RR, retry
  docker.go             # docker socket client (read-only)
  health.go             # background health checker
  routes.json           # static routes for non-Docker backends

dashboard/              # management UI binary (~2500 LOC)
  main.go               # wiring
  api.go                # HTTP handlers
  auth.go               # users, sessions, TOTP, PBKDF2
  ratelimit.go          # per-IP token bucket
  audit.go              # JSONL audit log
  cloudflare.go         # CF DNS CRUD
  docker.go             # docker socket client (read-write)
  images.go             # background registry digest checker
  stats.go              # host CPU/MEM/DISK
  qr.go                 # TOTP QR rendering
  ui.go                 # single-file HTML+CSS+JS dashboard
  data/                 # auth.json + audit.log (gitignored)

services/               # example service definitions (drop your own here)
docker-compose.yml      # the whole stack
.env.example            # CF token placeholders
```

## Security posture

- Auth required for every read; **2FA required for every write**
- Rate-limited login + setup endpoints (5/min/IP, then 429)
- Audit log of every write to `dashboard/data/audit.log`
- proxy's docker socket mount is **read-only** — RCE there can't create/destroy containers
- Cookies are HttpOnly + SameSite=Lax, HMAC-signed
- TOTP secrets stored as plain JSON (file perms `0600`); back up `dashboard/data/`

## License

(your call)
