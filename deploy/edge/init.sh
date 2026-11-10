#!/bin/sh
# Root one-shot; nginx runs as uid/gid 101 in the pinned image.
set -eu
umask 077
certs=/etc/nginx/edge-certs
auth=/etc/nginx/edge-auth
mkdir -p "$certs" "$auth" /var/www/acme/.well-known/acme-challenge /var/www/acme/.edge-empty
chmod 0755 "$certs" "$auth" /var/www/acme /var/www/acme/.well-known /var/www/acme/.well-known/acme-challenge /var/www/acme/.edge-empty
work=$(mktemp -d "$certs/.init.XXXXXX")
auth_tmp=
trap 'rm -rf "$work"; if [ -n "$auth_tmp" ]; then rm -f "$auth_tmp"; fi' EXIT
trap 'exit 1' HUP INT TERM
if [ ! -s "$certs/current.crt" ] || [ ! -s "$certs/current.key" ]; then
 case "${SERVER_NAME:-}" in ''|*[!A-Za-z0-9.-]*) printf '%s\n' 'edge-init: invalid SERVER_NAME' >&2; exit 1 ;; esac
 openssl req -x509 -nodes -newkey rsa:2048 -days 365 \
  -subj "/CN=${SERVER_NAME}" -addext "subjectAltName=DNS:${SERVER_NAME}" \
  -keyout "$work/current.key" -out "$work/current.crt" >/dev/null 2>&1
 chown 0:101 "$work/current.key" "$work/current.crt"
 chmod 0640 "$work/current.key"
 chmod 0644 "$work/current.crt"
 mv "$work/current.key" "$certs/current.key"
 mv "$work/current.crt" "$certs/current.crt"
fi
auth_tmp=$(mktemp "$auth/.htpasswd.XXXXXX")
cat /run/edge/htpasswd > "$auth_tmp"
[ -s "$auth_tmp" ] || { printf '%s\n' 'edge-init: empty htpasswd' >&2; exit 1; }
chown 0:101 "$auth_tmp"
chmod 0640 "$auth_tmp"
mv "$auth_tmp" "$auth/htpasswd"
auth_tmp=
