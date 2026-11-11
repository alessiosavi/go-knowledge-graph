#!/usr/bin/env bash
set -euo pipefail
umask 077
root=${GKG_ROOT:-/opt/gkg}
[ "$#" -ge 1 ] && [ "$#" -le 2 ] && [[ "$1" =~ ^[A-Za-z0-9]+$ ]] && [ "${#1}" -le 128 ] || { echo 'gkg-box: invalid ACME nonce' >&2; exit 1; }
mode=${2:-write}
case "$mode" in write|--remove) ;; *) exit 1 ;; esac
cd "$root"
# certbot owns the writable mount; the running edge's webroot is read-only.
docker compose --env-file "${GKG_ENV_FILE:-$root/.env}" -f deploy/docker-compose.yml --profile cert run --rm --no-deps --entrypoint sh certbot -c '
  set -eu
  dir=/var/www/acme/.well-known/acme-challenge
  if [ "$2" = --remove ]; then rm -f "$dir/$1"; else mkdir -p "$dir"; printf "%s" "$1" >"$dir/$1"; fi
' sh "$1" "$mode" >/dev/null 2>/dev/null
