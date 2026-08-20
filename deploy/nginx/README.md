# Host nginx config

nginx runs on the host, in front of the stack — it is not a compose service, so
nothing here is deployed by `docker compose up`. These files are tracked so the
host config has a reviewable source of truth; installing them is manual.

```
snippets/maintenance.conf     ->  /etc/nginx/snippets/maintenance.conf
conf.d/cloudflare-realip.conf ->  /etc/nginx/conf.d/cloudflare-realip.conf
conf.d/maintenance-bypass.conf -> /etc/nginx/conf.d/maintenance-bypass.conf
```

## Installing

```sh
sudo cp /etc/nginx/snippets/maintenance.conf \
        /etc/nginx/snippets/maintenance.conf.bak-$(date +%Y%m%d-%H%M%S)
sudo cp deploy/nginx/snippets/maintenance.conf /etc/nginx/snippets/maintenance.conf
sudo mkdir -p /var/www/maintenance/hosts

sudo cp /etc/nginx/conf.d/cloudflare-realip.conf \
        /etc/nginx/conf.d/cloudflare-realip.conf.bak-$(date +%Y%m%d-%H%M%S)
sudo cp deploy/nginx/conf.d/cloudflare-realip.conf /etc/nginx/conf.d/cloudflare-realip.conf

sudo cp /etc/nginx/conf.d/maintenance-bypass.conf \
        /etc/nginx/conf.d/maintenance-bypass.conf.bak-$(date +%Y%m%d-%H%M%S)
sudo cp deploy/nginx/conf.d/maintenance-bypass.conf /etc/nginx/conf.d/maintenance-bypass.conf

sudo nginx -t && sudo nginx -s reload
```

Rollback is `cp` the `.bak-*` file back and reload.

## Why `cloudflare-realip.conf` and `maintenance-bypass.conf` matter

Both are load-bearing and both fail silently if lost (e.g. a from-scratch nginx
rebuild that only pulls what's tracked here):

- **`cloudflare-realip.conf`** makes `$remote_addr` the real visitor IP instead
  of the Cloudflare edge IP for every nginx-fronted host. Without it,
  `proxy.ratelimit` buckets every Cloudflare visitor together as one client.
  The Cloudflare IP ranges inside are pinned as of 2026-08-19 — re-fetch from
  `https://www.cloudflare.com/ips-v4` and `/ips-v6` periodically, since a new
  range appearing means those visitors silently stop being trusted and fall
  back to edge-IP bucketing with no error.
- **`maintenance-bypass.conf`** is what `snippets/maintenance.conf` (above)
  reads `$maint_bypass` from — it's what lets Tailscale/LAN traffic through a
  host in maintenance mode while Cloudflare-fronted traffic still gets the
  maintenance page. It depends on `cloudflare-realip.conf` already being
  loaded (keys off `$realip_remote_addr`, which only exists once the realip
  module is configured). Without it, the maintenance bypass silently starts
  admitting or rejecting the wrong traffic instead of erroring.

See `project_ratelimit_xff_cloudflare_bug.md` and
`reference_nginx_wildcard_server_name_gotchas.md` in proxy-manager memory for
the incidents that led to each file.

## Per-app maintenance pages

`@maintenance` resolves the page with

```nginx
try_files /hosts/$host/index.html /index.html =503;
```

so `/var/www/maintenance/hosts/<host>/index.html` wins for that host and every
other host falls back to `/var/www/maintenance/index.html`. With no `hosts/`
directories present the behaviour is byte-identical to the single-page version.

Two ways a host gets its own page:

1. **From the app** (the normal path). The container carries

   ```yaml
   labels:
     proxy.maintenance: "/app/maintenance.html"   # path inside the image
   ```

   and the dashboard copies that file out to `hosts/<proxy.host>/index.html`,
   refreshing every couple of minutes. The copy is eager on purpose: maintenance
   is usually switched on *because* the app is down, so reading the page at flip
   time would fail exactly when it's needed. Docker's archive endpoint reads a
   stopped container too, so restarts and redeploys keep working — only removing
   the container entirely falls back to the default page.

   The page must be self-contained (inline CSS, no external assets): only that
   one file is copied, and nginx serves it as `index.html` with no other paths
   under `hosts/<host>/` resolving.

2. **By hand.** Drop `hosts/<host>/index.html` yourself. The dashboard's
   reconciliation only deletes directories carrying a `.managed` marker file,
   so a hand-written page is never removed.

The dashboard needs `/var/www/maintenance/hosts` bind-mounted (see
`MAINT_PAGE_HOST_DIR` in `docker-compose.yml`). Without the mount the feature
logs a warning at startup and stays off — nginx still serves the default page.

> The Pi's `docker-compose.yml` is intentionally not a copy of this repo's (no
> `auth` service, different defaults). Add the mount and `MAINT_PAGE_DIR` there
> by hand rather than overwriting the file.
