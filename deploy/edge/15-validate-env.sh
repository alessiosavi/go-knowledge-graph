#!/bin/sh
# Sourced as 15-validate-env.envsh before envsubst: validate directive values
# and export the derived ask timeout into the template renderer’s environment.
set -eu
LC_ALL=C
export LC_ALL
fail() { printf 'edge: invalid %s\n' "$1" >&2; exit 1; }
key=${GKG_API_KEY:-}
case "$key" in ''|*[!A-Za-z0-9._~+/=-]*) fail GKG_API_KEY ;; esac
[ "${#key}" -ge 16 ] && [ "${#key}" -le 256 ] || fail GKG_API_KEY
name=${SERVER_NAME:-}
case "$name" in ''|*[!A-Za-z0-9.-]*) fail SERVER_NAME ;; esac
[ "${#name}" -le 253 ] || fail SERVER_NAME
printf '%s\n' "$name" | awk -F. '{
 for (i=1;i<=NF;i++) {
  if (i==NF && $i=="" && NF>1) continue
  if (length($i)<1 || length($i)>63 || $i ~ /^-/ || $i ~ /-$/) exit 1
 }
}' || fail SERVER_NAME
# These values also enter nginx directives; accept decimal numbers only.
number() {
 case "$2" in ''|*[!0-9]*) fail "$1" ;; esac
 [ "${#2}" -le 9 ] || fail "$1"
}
number EDGE_HSTS_MAX_AGE "${EDGE_HSTS_MAX_AGE:-0}"
number EDGE_INGEST_TIMEOUT_S "${EDGE_INGEST_TIMEOUT_S:-390}"
# The image sources executable *.envsh files, but executes *.sh files.
# Compose mounts this script with the .envsh suffix so exports reach 20-envsubst.
# Upstream: https://github.com/nginx/docker-nginx-unprivileged/blob/main/entrypoint/docker-entrypoint.sh
agent_timeout=${GKG_AGENT_TIMEOUT_S:-60}
number GKG_AGENT_TIMEOUT_S "$agent_timeout"
# Normalize decimal inputs before shell arithmetic (leading zeros mean octal).
agent_timeout=$(printf '%s\n' "$agent_timeout" | awk '{printf "%d", $0}')
[ "$agent_timeout" -gt 0 ] || agent_timeout=60
if [ -z "${EDGE_ASK_TIMEOUT_S:-}" ]; then
 EDGE_ASK_TIMEOUT_S=$((agent_timeout + 30))
fi
number EDGE_ASK_TIMEOUT_S "$EDGE_ASK_TIMEOUT_S"
EDGE_ASK_TIMEOUT_S=$(printf '%s\n' "$EDGE_ASK_TIMEOUT_S" | awk '{printf "%d", $0}')
[ "$EDGE_ASK_TIMEOUT_S" -ge "$((agent_timeout + 15))" ] || fail 'EDGE_ASK_TIMEOUT_S (must be at least GKG_AGENT_TIMEOUT_S + 15 seconds)'
export EDGE_ASK_TIMEOUT_S
number EDGE_HTTPS_PORT "${EDGE_HTTPS_PORT:-443}"
