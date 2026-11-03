#!/usr/bin/env bash
# Runs as root on the server: installs one release of smsd and rolls back
# if it does not come up.
set -euo pipefail
release=$1
domain=$2
app=/opt/smsd
release_dir="$app/releases/$release"
service_file=/etc/systemd/system/smsd.service
env_file=/etc/smsd/smsd.env
nginx_site=/etc/nginx/sites-available/$domain
nginx_enabled=/etc/nginx/sites-enabled/$domain
database=/var/lib/smsd/sms.db
# nginx, certbot and Python are already on this host; nothing is installed here.
id smsd >/dev/null 2>&1 || useradd --system --home-dir /var/lib/smsd --no-create-home --shell /usr/sbin/nologin smsd
mkdir -p "$release_dir"
tar -xzf "/tmp/smsd-$release.tar.gz" -C "$release_dir"
rm -f "/tmp/smsd-$release.tar.gz"
chmod 755 "$release_dir/smsd"
# First deploy only: a login with a random password, and the cloud relay.
# The gateway credentials come from the SMSGate app and are filled in by hand.
if [ ! -f "$env_file" ]; then
    install -d -m 700 /etc/smsd
    install -m 600 /dev/null "$env_file"
    cat > "$env_file" <<ENV
SMS_AUTH_USER=sms
SMS_AUTH_PASSWORD=$(head -c 15 /dev/urandom | base64 | tr -d '/+=')
SMS_GATEWAY_URL=https://api.sms-gate.app/3rdparty/v1
SMS_GATEWAY_USERNAME=
SMS_GATEWAY_PASSWORD=
SMS_SIM=1
SMS_HOURS_START=00:00
SMS_HOURS_END=24:00
SMS_MESSAGE_MAX_SEGMENTS=3
ENV
fi
# Releases before 2026-10-22 used a bearer token; swap it for a login.
if ! grep -q '^SMS_AUTH_PASSWORD=' "$env_file"; then
    sed -i '/^SMS_API_TOKEN=/d' "$env_file"
    printf 'SMS_AUTH_USER=sms\nSMS_AUTH_PASSWORD=%s\n' "$(head -c 15 /dev/urandom | base64 | tr -d '/+=')" >> "$env_file"
fi
if [ -f "$database" ]; then
    python3 - "$database" "$release" <<'PY'
import sqlite3
import sys

path, release = sys.argv[1:]
with sqlite3.connect(path) as source, sqlite3.connect(f'{path}.{release}.backup') as target:
    source.backup(target)
PY
    chmod 600 "$database.$release.backup"
fi
# One-time certificate issue; certbot.timer renews it afterwards.
if [ ! -f "/etc/letsencrypt/live/$domain/fullchain.pem" ]; then
    certbot certonly --nginx --non-interactive --agree-tos --keep-until-expiring -d "$domain"
fi
previous=$(readlink "$app/current" || true)
[ -f "$service_file" ] && cp -a "$service_file" "$release_dir/previous.service"
rollback_app() {
    if [ -n "$previous" ]; then
        ln -s "$previous" "$app/rollback"
        mv -Tf "$app/rollback" "$app/current"
        cp "$release_dir/previous.service" "$service_file"
        systemctl daemon-reload
        systemctl restart smsd
    else
        systemctl stop smsd || true
    fi
}
ln -s "$release_dir" "$app/current-next"
mv -Tf "$app/current-next" "$app/current"
cp "$app/current/smsd.service" "$service_file"
systemctl daemon-reload
systemctl enable smsd
systemctl restart smsd
ready=false
for attempt in {1..20}; do
    if curl -fsS http://127.0.0.1:8097/healthz >/dev/null 2>&1; then ready=true; break; fi
    sleep 1
done
if [ "$ready" != true ]; then
    journalctl -u smsd -n 20 --no-pager
    rollback_app
    exit 1
fi
nginx_backup="$release_dir/nginx-before"
mkdir -p "$nginx_backup"
[ -f "$nginx_site" ] && cp -a "$nginx_site" "$nginx_backup/$domain"
sed "s/__DOMAIN__/$domain/g" "$app/current/nginx-site.conf" > "$nginx_site"
ln -sfn "$nginx_site" "$nginx_enabled"
if ! nginx -t; then
    if [ -f "$nginx_backup/$domain" ]; then cp -a "$nginx_backup/$domain" "$nginx_site"; else rm -f "$nginx_site" "$nginx_enabled"; fi
    rollback_app
    exit 1
fi
systemctl reload nginx
# The API must ask for the login. nginx reloads gracefully, so old workers
# may answer the first requests.
for attempt in {1..10}; do
    status=$(curl -sS -o /dev/null -w '%{http_code}' --retry 5 --retry-all-errors --retry-delay 1 "https://$domain/api/status")
    [ "$status" = 401 ] && break
    sleep 1
done
if [ "$status" != 401 ]; then
    echo "Expected the login check (401) from https://$domain/api/status, got $status." >&2
    exit 1
fi
# Keep the five newest releases.
ls -1d "$app"/releases/* | sort | head -n -5 | xargs -r rm -rf
printf 'Deployed https://%s/ (%s)\n' "$domain" "$release"
