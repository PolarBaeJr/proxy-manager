# Host nginx config

nginx runs on the host, in front of the stack — it is not a compose service, so
nothing here is deployed by `docker compose up`. These files are tracked so the
host config has a reviewable source of truth; installing them is manual.

```
snippets/maintenance.conf   ->  /etc/nginx/snippets/maintenance.conf
```

## Installing

```sh
sudo cp /etc/nginx/snippets/maintenance.conf \
        /etc/nginx/snippets/maintenance.conf.bak-$(date +%Y%m%d-%H%M%S)
sudo cp deploy/nginx/snippets/maintenance.conf /etc/nginx/snippets/maintenance.conf
sudo mkdir -p /var/www/maintenance/hosts
sudo nginx -t && sudo nginx -s reload
```

Rollback is `cp` the `.bak-*` file back and reload.

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
