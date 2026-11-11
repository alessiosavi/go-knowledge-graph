#!/usr/bin/env bash
# SSM output travels only to private files; dotenv is never sourced.
set -euo pipefail
umask 077
prefix=${GKG_SSM_PREFIX:-}
[[ "$prefix" =~ ^/[a-z][a-z0-9-]{1,30}$ ]] || { echo 'gkg-box: GKG_SSM_PREFIX is missing or invalid' >&2; exit 1; }
root=${GKG_ROOT:-/opt/gkg}
out=${GKG_ENV_FILE:-$root/.env}
auth=$root/deploy/edge/htpasswd
mkdir -p "$(dirname "$auth")"
[ ! -L "$out" ] && [ ! -L "$auth" ] || { echo 'gkg-box: refusing symlink secret destination' >&2; exit 1; }
env_tmp=$(mktemp "${out}.tmp.XXXXXX")
auth_tmp=$(mktemp "${auth}.tmp.XXXXXX")
trap 'rm -f "$env_tmp" "$auth_tmp"' EXIT
trap 'exit 1' HUP INT TERM
aws ssm get-parameter --name "$prefix/env" --with-decryption --query Parameter.Value --output text >"$env_tmp" 2>/dev/null || { echo 'gkg-box: environment fetch failed' >&2; exit 1; }
aws ssm get-parameter --name "$prefix/htpasswd" --with-decryption --query Parameter.Value --output text >"$auth_tmp" 2>/dev/null || { echo 'gkg-box: password fetch failed' >&2; exit 1; }
[ -s "$env_tmp" ] && [ -s "$auth_tmp" ] || { echo 'gkg-box: empty secret parameter' >&2; exit 1; }
chmod 600 "$env_tmp" "$auth_tmp"
mv -f "$env_tmp" "$out"
mv -f "$auth_tmp" "$auth"
