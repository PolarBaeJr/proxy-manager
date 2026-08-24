# Host nginx config

nginx runs on the host, in front of the stack — it is not a compose service, so
nothing here is deployed by `docker compose up`. These files are tracked so the
host config has a reviewable source of truth; installing them is manual.

```
snippets/maintenance.conf     ->  /etc/nginx/snippets/maintenance.conf
conf.d/cloudflare-realip.conf ->  /etc/nginx/conf.d/cloudflare-realip.conf
conf.d/maintenance-bypass.conf -> /etc/nginx/conf.d/maintenance-bypass.conf
stream.d/redis-peer.conf      ->  Mac mini peer node ONLY, see below — NOT
                                   the same nginx instance/install path as
                                   the three files above.
bin/update-cloudflare-ips.sh  ->  Mac mini peer node ONLY, see below — the
                                   job that keeps cloudflare-realip.conf fresh.
org.cloudflare-realip.plist   ->  /Library/LaunchDaemons/org.cloudflare-realip.plist
```

See also `deploy/nginx-stream-pi/` — the Pi's own separate stream-only nginx
instance (dashboard-peer transport), the Linux/systemd counterpart to the Mac
mini's `nginx-stream/` below.

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

## `nginx-stream/` — Mac mini peer node only, SEPARATE nginx instance

Started as Redis-only (`stream.d/redis-peer.conf` below is that original,
now-superseded single-block form); the live instance has since grown a
second `server{}` block for the dashboard's main port (`:8093`), and will
grow a third for the dashboard peer-handshake port. All three share this one
instance rather than each getting their own, for the same reason below.

Do not install any of this into the main nginx (the one serving
`conf.d/`/`snippets/` above). `stream{}` is a top-level block and cannot be
reached from `conf.d/`'s `http{}` scope — and more importantly,
`listen <tailscale-ip>` is FATAL to nginx at boot if that Tailscale interface
doesn't exist yet (`nginx -t` fails outright, process doesn't start).
Tailscale comes up as an independent boot service with no ordering guarantee
ahead of nginx, so putting any of this in the main instance risks taking down
`:80`/`:443` for every site on the box over a side-channel proxy on an
unlucky boot.

Instead it runs as a second, minimal nginx (`events{}` + a `stream{}` block
including this file, nothing else — no `http{}`), under its own
LaunchDaemon with `KeepAlive` so a boot-time bind failure just retries until
Tailscale is up:

```
deploy/nginx-stream/nginx.conf            ->  /Users/matthew/deploy/nginx-stream/nginx.conf
deploy/nginx-stream/org.nginx-stream.plist -> /Library/LaunchDaemons/org.nginx-stream.plist
```

Both tracked files are a **live copy of the Mac mini's actual running
config**, not a portable template — nginx has no env-var interpolation, so
the Tailscale IP (`100.83.62.68`) and `/Users/matthew` path are literal to
this one machine. Copying either file onto a second peer node requires
editing both by hand first.

```sh
sudo mkdir -p /Users/matthew/deploy/nginx-stream/logs
sudo cp deploy/nginx-stream/nginx.conf /Users/matthew/deploy/nginx-stream/nginx.conf
sudo cp deploy/nginx-stream/org.nginx-stream.plist /Library/LaunchDaemons/org.nginx-stream.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/org.nginx-stream.plist
```

Verified running concurrently with the main nginx with no port conflict
(different address, different port, own pid file) — this does not reopen the
one-nginx-per-box rule from above, which is specifically about `:443`
contention. Confirmed end-to-end (2026-08-24): the Pi's proxy reaches Redis
through this path with `PING` → `PONG`, auth included, and both directions of
Pi<->Mac dashboard reachability work through the `:8093` block.

## `deploy/nginx-stream-pi/` — Pi side of the dashboard peer transport, SEPARATE nginx instance

The Linux/systemd counterpart to `nginx-stream/` above, same reasoning:
`listen <tailscale-ip>` is fatal at boot if Tailscale isn't up yet, and
`tailscaled.service` has no ordering guarantee ahead of `nginx.service`, so
this runs as its own instance rather than a `stream{}` block in the main
config. `Restart=on-failure` (systemd) does the same job as the Mac's
LaunchDaemon `KeepAlive`.

One real difference from the Mac: this Debian nginx build has the stream
module as a **dynamic** module (`--with-stream=dynamic`, confirmed via
`nginx -V`), not compiled in — `deploy/nginx-stream-pi/nginx.conf` loads it
explicitly (`load_module .../ngx_stream_module.so;`). The package
(`libnginx-mod-stream`) must be installed first:

```sh
sudo apt-get install -y libnginx-mod-stream
```

Installing it also auto-enables the module for the MAIN nginx too, via
`/etc/nginx/modules-enabled/50-mod-stream.conf` (a symlink the package
creates on its own) — harmless since the main config has no `stream{}`
block to activate, but worth knowing before assuming the module is scoped to
just this second instance. On this Pi, installing the module also pulled in
an `nginx`/`nginx-common` point-release upgrade as a dependency, whose
postinst triggered an automatic reload of the live production nginx —
verified safe afterward (config test passed, public sites returned 200,
`cloudflare-realip.conf`'s `real_ip` entries were intact), but not something
this command warns you about up front.

```
deploy/nginx-stream-pi/nginx.conf          ->  /etc/nginx-stream/nginx.conf
deploy/nginx-stream-pi/nginx-stream.service -> /etc/systemd/system/nginx-stream.service
```

```sh
sudo apt-get install -y libnginx-mod-stream
sudo mkdir -p /etc/nginx-stream /var/log/nginx-stream
sudo cp deploy/nginx-stream-pi/nginx.conf /etc/nginx-stream/nginx.conf
sudo cp deploy/nginx-stream-pi/nginx-stream.service /etc/systemd/system/nginx-stream.service
sudo nginx -t -c /etc/nginx-stream/nginx.conf
sudo systemctl daemon-reload
sudo systemctl enable --now nginx-stream.service
```

Verified running concurrently with the main nginx with no port conflict
(bound to `100.123.79.47:8093` only, confirmed via `ss -tlnp` — not
`0.0.0.0`) — same "different address, different port" reasoning as the Mac
side, not a reopening of the one-nginx-per-box rule. Confirmed end-to-end
(2026-08-24): the Mac mini reaches the Pi's dashboard through this path,
`HTTP 200` in ~26ms.

## `bin/update-cloudflare-ips.sh` — Mac mini peer node only, keeps `cloudflare-realip.conf` fresh

On the Pi, `conf.d/cloudflare-realip.conf` (see below for why it matters) is
regenerated weekly by a root crontab line:

```
17 4 * * 1 /usr/local/sbin/update-cloudflare-ips.sh
```

When the Pi's nginx tree was copied to the Mac mini, only the *output* of that
script came along — not the script or its schedule. Left unported, the ranges
in `cloudflare-realip.conf` go stale as Cloudflare adds prefixes: `real_ip`
silently stops rewriting `$remote_addr` for the missing ranges, and every
visitor from them collapses into one `proxy.ratelimit` bucket. Nothing errors —
the site keeps returning 200.

`bin/update-cloudflare-ips.sh` is a macOS port of the Pi script, and
`org.cloudflare-realip.plist` is its LaunchDaemon (Mondays 04:17, same slot the
Pi used). Both are a **live copy of the Mac mini's actual job**, not a
portable template — paths (`/Users/matthew/...`) are literal to this one
machine, same caveat as `nginx-stream/` below. Differences from the Pi
original, all commented in the script itself:

1. Config/log paths live under `/Users/matthew/deploy`, not `/etc/nginx` and
   `/var/log`.
2. BSD `date` has no `-Is` — GNU's ISO-timestamp shorthand exits nonzero on
   macOS, which would kill the script under `set -euo pipefail`. Replaced with
   an explicit `+%Y-%m-%dT%H:%M:%S%z` format string.
3. Every nginx invocation carries `-p`/`-c` — otherwise it tests/reloads
   `/opt/homebrew/etc/nginx` (Homebrew's default prefix) instead of the config
   actually being served, and `nginx -t` would pass against the wrong tree.
4. There is no `systemctl`; reload is `nginx -s reload` against the same
   prefix. The nginx master runs as root, so this must run as a root
   LaunchDaemon, not in a user's interactive shell.

```sh
sudo mkdir -p /Users/matthew/deploy/bin
sudo cp deploy/nginx/bin/update-cloudflare-ips.sh /Users/matthew/deploy/bin/update-cloudflare-ips.sh
sudo chmod +x /Users/matthew/deploy/bin/update-cloudflare-ips.sh
sudo cp deploy/nginx/org.cloudflare-realip.plist /Library/LaunchDaemons/org.cloudflare-realip.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/org.cloudflare-realip.plist
```

Verified end-to-end (2026-08-24): forcing a change path (deleting one range
from the live config) was detected, the full set was restored, `nginx -t`
passed, a dated backup was written, and nginx reloaded gracefully (master pid
unchanged, workers cycled). Also verified running under `launchctl kickstart`
directly, to confirm it survives launchd's minimal `PATH` rather than only
working in an interactive shell.

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
