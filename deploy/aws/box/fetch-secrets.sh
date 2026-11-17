#!/usr/bin/env bash
# SSM output travels only to private files; dotenv is never sourced.
set -euo pipefail
umask 077
prefix=${GKG_SSM_PREFIX:-}
[[ "$prefix" =~ ^/[a-z][a-z0-9-]{1,30}$ ]] || { echo 'gkg-box: GKG_SSM_PREFIX is missing or invalid' >&2; exit 1; }
root=${GKG_ROOT:-/opt/gkg}
out=${GKG_ENV_FILE:-$root/.env}
auth=$root/deploy/edge/htpasswd
data=$root/deploy/box-data
mkdir -p "$(dirname "$auth")"
[ ! -L "$out" ] && [ ! -L "$auth" ] && [ ! -L "$data" ] || { echo 'gkg-box: refusing symlink secret destination' >&2; exit 1; }
mkdir -p "$data"
chown 0:0 "$data"
chmod 0755 "$data"
env_tmp=$(mktemp "${out}.tmp.XXXXXX")
auth_tmp=$(mktemp "${auth}.tmp.XXXXXX")
optional_tmp='' error_tmp=''
trap 'rm -f "$env_tmp" "$auth_tmp" ${optional_tmp:+"$optional_tmp"} ${error_tmp:+"$error_tmp"}' EXIT
trap 'exit 1' HUP INT TERM
aws ssm get-parameter --name "$prefix/env" --with-decryption --query Parameter.Value --output text >"$env_tmp" 2>/dev/null || { echo 'gkg-box: environment fetch failed' >&2; exit 1; }
aws ssm get-parameter --name "$prefix/htpasswd" --with-decryption --query Parameter.Value --output text >"$auth_tmp" 2>/dev/null || { echo 'gkg-box: password fetch failed' >&2; exit 1; }
[ -s "$env_tmp" ] && [ -s "$auth_tmp" ] || { echo 'gkg-box: empty secret parameter' >&2; exit 1; }
chmod 600 "$env_tmp" "$auth_tmp"
mv -f "$env_tmp" "$out"
mv -f "$auth_tmp" "$auth"
for kind in domain-seed stoplist; do
  dest=$data/domain-seed.json
  [ "$kind" != stoplist ] || dest=$data/stoplist.txt
  [ ! -L "$dest" ] || { echo 'gkg-box: refusing symlink secret destination' >&2; exit 1; }
  optional_tmp=$(mktemp "$data/.fetch.XXXXXX")
  error_tmp=$(mktemp "$data/.error.XXXXXX")
  if aws ssm get-parameter --name "$prefix/$kind" --with-decryption --query Parameter.Value --output text >"$optional_tmp" 2>"$error_tmp"; then
    chown 10001:10001 "$optional_tmp"
    chmod 0400 "$optional_tmp"
    mv -f "$optional_tmp" "$dest"
  elif grep -q '(ParameterNotFound)' "$error_tmp"; then
    rm -f "$dest"
  elif grep -Eq '\((AccessDenied|AccessDeniedException)\)' "$error_tmp"; then
    rm -f "$dest"
    printf 'gkg-box: optional parameter %s/%s is not readable; run make box-policy\n' "$prefix" "$kind" >&2
  else
    printf 'gkg-box: optional %s fetch failed (details withheld)\n' "$kind" >&2
    exit 1
  fi
  rm -f "$optional_tmp" "$error_tmp"
done
