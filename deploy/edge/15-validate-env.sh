#!/bin/sh
# Runs before envsubst: reject values that could become nginx syntax.
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
number EDGE_ASK_TIMEOUT_S "${EDGE_ASK_TIMEOUT_S:-90}"
number EDGE_HTTPS_PORT "${EDGE_HTTPS_PORT:-443}"
