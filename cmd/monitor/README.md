# monitor

Fourth binary in the stack. Scrapes `/metrics` from each of the other binaries on a tick, keeps a rolling in-memory time series, and exposes aggregated stats over a JSON API the dashboard consumes.

## What it tracks per target

From the JSON each target emits at `/metrics`:

- `total` — lifetime request count
- `in_flight` — currently-active requests
- `bytes_out` — response bytes served
- `by_host`, `by_status`, `by_method` — request breakdown
- `by_host_status` — error rate per host
- `latency_ms.{p50,p90,p95,p99,max}` — response time percentiles
- `rate_limited` (edge only) — 429s issued
- `sample_size` — how many samples backed the percentiles

Monitor also computes its own per-target fields:

- `delta` — requests since the last scrape (sustained RPS)
- `health` — `up` / `flaky` (some recent failures) / `down` (3+ consecutive failures)
- `last_ok` — timestamp of the most recent successful scrape

## API

| Endpoint | Returns |
|---|---|
| `GET /api/snapshot` | latest scraped sample + state for every target |
| `GET /api/series?target=proxy&field=delta` | last hour of points for one field (sparkline data) |
| `GET /api/overview` | aggregated dashboard summary |
| `GET /healthz` | `ok` (monitor self) |

## Flags

| Flag | Default | Notes |
|---|---|---|
| `-addr` | `:8095` | listen address |
| `-targets` | `edge=http://edge:8094/metrics,proxy=http://proxy:8094/metrics` | comma-separated `name=url` |
| `-interval` | `5s` | scrape cadence |
| `-window` | `1h` | how much history to retain in memory |
| `-tls-probe-targets` |   | comma-separated `sni[@host:port]`; empty disables cert probing |
| `-tls-probe-interval` | `15m` | how often to reprobe cert expiry |
| `-tls-probe-dial` | `host.docker.internal:443` | default dial target when no `@host:port` is given |

## Why a separate binary?

Putting the scraper inside the dashboard would couple two unrelated concerns: the dashboard restarts every time we ship a UI change; we don't want monitoring history wiped on every push. Putting it in the proxy would tie its lifetime to request-path code, same problem.

Monitor is also the natural place to add downstream observability (alerts, log shipping, external Prometheus exposition) without bloating any of the other three.
