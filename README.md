# proxy-manager

> A self-hosted reverse proxy, load balancer, and management dashboard for a single host. Two small Go binaries. Zero external dependencies aside from one QR code library.

Built for a Raspberry Pi running 10+ services. Replaces the usual nginx + Traefik + Portainer + Homepage + a pile of bash scripts with ~3000 lines of Go you can read in an afternoon.

---

## What you get

- **Label-driven routing.** Drop `proxy.enable=true` + `proxy.host=foo.example` labels on any container — it's routed. No config files to edit.
- **Weighted round-robin** across replicas with **automatic failover** and **per-backend health checks**.
- **Web dashboard** with: live route table, scale services up/down, create new services from a form, edit Cloudflare DNS, manage users, host CPU/RAM/disk stats.
- **Real deployment workflows**: blue/green Canary → Promote/Discard, atomic Replace, one-click Rollback, registry update detection.
- **Multi-user auth** with PBKDF2 passwords and TOTP 2FA (QR code setup). Every write requires 2FA.
- **Audit log** of every action, rate-limited login, read-only Docker socket for the request path.

---

## Architecture

```
                 ┌─────────────────────────────────┐
   internet ───→ │ nginx (TLS termination on :443) │
                 └─────────────────┬───────────────┘
                                   │ HTTP
                                   ▼
   ┌──────────────────────────────────────────────────────────┐
   │  proxy  :8092          (read-only docker socket)         │
   │  - label discovery + static routes                       │
   │  - weighted round-robin + retry on failure               │
   │  - background health checks (HTTP or TCP)                │
   └──────────────────────────────────────────────────────────┘
                                   ▲
                                   │ container labels
                                   ▼
   ┌──────────────────────────────────────────────────────────┐
   │  dashboard  :8093      (read-write docker socket)        │
   │  - auth + 2FA, audit log, rate-limited login             │
   │  - services: create/scale/replace/stage/promote/rollback │
   │  - DNS via Cloudflare API                                │
   │  - users, host stats                                     │
   └──────────────────────────────────────────────────────────┘
```

The two binaries don't talk to each other — they both watch the Docker socket. If the dashboard crashes, traffic keeps flowing. If the proxy crashes, the dashboard still works.

---

## Quick start

```bash
git clone https://github.com/PolarBaeJr/proxy-manager.git
cd proxy-manager
cp .env.example .env                # optional: Cloudflare token for DNS tab
docker compose up -d --build
```

Open `http://<host>:8093` — first-run setup creates the initial user and shows a TOTP QR code.

If the host is remote, tunnel the dashboard port:
```bash
ssh -L 8093:localhost:8093 your-host
open http://localhost:8093
```

---

## Labels reference

Add these to any container you want routed:

| Label | Required | Notes |
|---|---|---|
| `proxy.enable=true` | ✓ | opt in |
| `proxy.host=foo.example` | ✓ | match against request Host header |
| `proxy.port=8080` | ✓ | container's **internal** port |
| `proxy.path=/admin` |   | path prefix for fan-out |
| `proxy.strip=true` |   | strip prefix before forwarding |
| `proxy.weight=2` |   | weighted RR (default 1) |
| `proxy.health=/healthz` |   | HTTP probe (default = TCP connect) |
| `proxy.service=myapp` |   | group key — enables scale/replace/canary in the dashboard |
| `proxy.unscalable=true` |   | mark singleton (DB, gateway, Discord bot) |
| `proxy.name=Friendly` |   | dashboard label |

Containers must share the **`edge`** Docker network with the proxy. See `services/example.yml`.

---

## Repo layout

```
proxy/        request-path binary  (~700 LOC)
dashboard/    management UI binary (~2500 LOC, single-file embedded HTML)
services/     example service definitions
scripts/      CLI alternative for some dashboard actions
```

Every file is small enough to read top-to-bottom. No code generation, no third-party UI frameworks, no service mesh.

---

## Security

- 2FA required for **every** write (scale, create, delete, DNS edit, user mgmt)
- TOTP secrets stay on disk in `dashboard/data/auth.json` (mode `0600`, gitignored)
- Login + setup are rate-limited per IP
- All writes append to `dashboard/data/audit.log` as JSONL
- The proxy's Docker socket mount is **read-only** — an RCE there can't create or destroy containers
- Cookies are HMAC-signed, HttpOnly, SameSite=Lax

There's no TLS on the dashboard itself — front it with nginx + Let's Encrypt, or only access via SSH tunnel, before exposing it publicly.

---

## License

[MIT](LICENSE) © Matthew Cheng
