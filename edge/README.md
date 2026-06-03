# edge

The outermost network layer. Terminates TLS and applies the nginx-shaped concerns the proxy intentionally skips:

- **TLS termination** — Let's Encrypt autocert (recommended) or static cert paths
- **HTTP → HTTPS redirect** with ACME challenge handling on :80
- **Per-IP rate limiting** (token bucket; default 100 req/s sustained, burst 200)
- **Request body size cap** (default 10 MB)
- **gzip compression** of compressible response types
- **X-Forwarded-* normalization** so upstream sees real client IP
- **JSONL access log** to stdout

Forwards every request to the internal proxy (default `http://proxy:8092`).

## Usage

```bash
# Let's Encrypt autocert — one or more domains, no cron jobs needed
./edge -tls-domains foo.example.com,bar.example.com

# Static cert paths (you manage renewal — certbot, etc.)
./edge -tls-cert /etc/letsencrypt/live/foo/fullchain.pem \
       -tls-key  /etc/letsencrypt/live/foo/privkey.pem

# Local testing — plain HTTP, no TLS
./edge -insecure -http-addr :8080 -backend http://localhost:8092
```

## Flags

| Flag | Default | Notes |
|---|---|---|
| `-https-addr` | `:443` | TLS listen address |
| `-http-addr` | `:80` | ACME challenge + HTTPS redirect |
| `-backend` | `http://proxy:8092` | upstream URL |
| `-tls-domains` |   | comma-separated; enables autocert |
| `-tls-cert` / `-tls-key` |   | static cert mode (alternative to autocert) |
| `-cert-dir` | `/data/certs` | autocert cache (mount a volume) |
| `-rate` | `100` | sustained req/s per IP (0 disables) |
| `-burst` | `200` | burst capacity per IP |
| `-max-body` | `10485760` | request body size cap |
| `-insecure` | `false` | plain HTTP for local testing only |

## Why a third binary?

The proxy is intentionally minimal: routing, load balancing, health checks. nothing else. Adding TLS / WAF / compression there would couple two unrelated lifecycles (cert renewals shouldn't restart the routing layer; routing config changes shouldn't break TLS).

Three binaries:
- **edge** does TLS and bouncer-shaped things, at the network boundary.
- **proxy** does routing + load balancing, on the internal network.
- **dashboard** does management, behind auth.

Each binary fails independently and is restartable without touching the others.
