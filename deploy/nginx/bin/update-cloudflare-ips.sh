#!/usr/bin/env bash
# macOS port of the Pi's /usr/local/sbin/update-cloudflare-ips.sh.
#
# Four things differ from the Pi original and each one is load-bearing:
#   1. CONF/LOG live under /Users/matthew/deploy, not /etc/nginx and /var/log.
#   2. BSD date has no `-Is`. GNU `date -Is` prints an ISO timestamp; the BSD
#      date on macOS treats -I as an unknown flag and EXITS NONZERO, which under
#      `set -euo pipefail` would kill the script. Spelled out as a format string.
#   3. nginx here is a prefix install driven by a LaunchDaemon, so every nginx
#      invocation needs -p/-c or it reads /opt/homebrew/etc/nginx instead and
#      "nginx -t" passes against a config that is not the one being served.
#   4. There is no systemctl. Reload is `nginx -s reload` against the same
#      prefix; the master is root, so this must run as a LaunchDaemon (root).
set -euo pipefail

PREFIX=/Users/matthew/deploy/nginx
NGINX=/opt/homebrew/bin/nginx
CONF="$PREFIX/conf.d/cloudflare-realip.conf"
LOG="$PREFIX/logs/cloudflare-realip-update.log"
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

stamp() { date +%Y-%m-%dT%H:%M:%S%z; }
log() { echo "$(stamp) $*" >> "$LOG"; }

nginx_t() { "$NGINX" -p "$PREFIX" -c "$PREFIX/nginx.conf" -t; }

v4=$(curl -fsS https://www.cloudflare.com/ips-v4) || { log "FETCH FAILED (v4), leaving config untouched"; exit 1; }
v6=$(curl -fsS https://www.cloudflare.com/ips-v6) || { log "FETCH FAILED (v6), leaving config untouched"; exit 1; }

if [ -z "$v4" ] || [ -z "$v6" ]; then
    log "FETCH RETURNED EMPTY, leaving config untouched"
    exit 1
fi

{
    echo "# Resolves the true visitor IP behind Cloudflare for every nginx-fronted"
    echo "# host on this box. Without this, \$remote_addr (and therefore"
    echo "# X-Forwarded-For/X-Real-IP passed to backends, and proxy-manager's"
    echo "# proxy.ratelimit bucketing) is the Cloudflare edge IP, not the visitor -"
    echo "# every visitor collapses into one shared bucket."
    echo "#"
    echo "# Safe by construction: real_ip only rewrites \$remote_addr when the RAW TCP"
    echo "# peer is one of these Cloudflare ranges, so a direct-to-origin connection"
    echo "# (the intentional Tailscale/LAN maintenance-bypass path - see"
    echo "# conf.d/maintenance-bypass.conf) can't spoof its own IP via a forged"
    echo "# header. \$realip_remote_addr always preserves the true original TCP peer"
    echo "# regardless of whether a rewrite happened, which is what"
    echo "# maintenance-bypass.conf's geo block keys on instead of \$remote_addr."
    echo "#"
    echo "# Auto-updated by /Users/matthew/deploy/bin/update-cloudflare-ips.sh"
    echo "# (LaunchDaemon org.cloudflare-realip, Mondays 04:17) from"
    echo "# https://www.cloudflare.com/ips-v4 and /ips-v6. Do not hand-edit the"
    echo "# set_real_ip_from lines below - they get overwritten on the next run."
    echo "# Last updated: $(stamp)"
    for ip in $v4; do echo "set_real_ip_from $ip;"; done
    for ip in $v6; do echo "set_real_ip_from $ip;"; done
    echo "# X-Forwarded-For, not CF-Connecting-IP: live traffic (2026-08-19) showed"
    echo "# some request types (server-to-server RPC calls) arrive through Cloudflare"
    echo "# without a CF-Connecting-IP header at all, leaving \$remote_addr"
    echo "# unrewritten (correctly - real_ip has nothing to rewrite with). XFF is the"
    echo "# more universally-set header, and it's also what proxy-manager's own Go"
    echo "# code (cmd/proxy/auth.go realClientIP) already reads downstream, so this"
    echo "# keeps both layers consistent."
    echo "real_ip_header X-Forwarded-For;"
    echo "real_ip_recursive on;"
} > "$TMP"

if [ -f "$CONF" ] && diff -q <(grep '^set_real_ip_from' "$CONF") <(grep '^set_real_ip_from' "$TMP") > /dev/null 2>&1; then
    log "no change (IP ranges identical to live config)"
    exit 0
fi

BAK="${CONF}.bak-$(date +%Y%m%d)"
cp "$CONF" "$BAK" 2>/dev/null || true
cp "$TMP" "$CONF"

if ! nginx_t 2>>"$LOG"; then
    log "nginx -t FAILED on new config, rolling back"
    cp "$BAK" "$CONF"
    exit 1
fi

"$NGINX" -p "$PREFIX" -c "$PREFIX/nginx.conf" -s reload
log "APPLIED: IP ranges changed, config test passed, nginx reloaded"
