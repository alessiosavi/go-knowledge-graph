#!/usr/bin/env bash
# Preserve Compose syntax byte-for-byte; never evaluate provider values.
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
source "$(dirname "$0")/lib.sh"
load_box_env
src=${SOURCE_ENV:-$AWS_DIR/../../.env}
out=${BOX_SECRETS_FILE:-$AWS_DIR/box.secrets.env}
[ -f "$src" ] || fail 'source environment file missing'
[ ! -L "$out" ] || fail 'refusing a symlink destination'
tmp=$(mktemp "${out}.tmp.XXXXXX"); trap 'rm -f "$tmp"' EXIT
trap 'exit 1' HUP INT TERM
# Single-line dotenv assignments are the supported transfer format. Refusing
# multiline values avoids truncating a quoted key into another assignment.
awk '
  /^[[:space:]]*(#|$)/ {next}
  !/^[A-Za-z_][A-Za-z0-9_]*=/ {exit 1}
  { k=substr($0,1,index($0,"=")-1); if(seen[k]++) exit 1 }
' "$src" || fail 'source must contain unique single-line dotenv assignments'
# Converter URL, mode and key are wired by Compose for the box; a host-run
# URL (localhost) must never travel, so only operational converter limits do.
awk -F= '
  /^(GKG_(AGENT|EMBEDDER|EXTRACTOR|JUDGE|FURLS)_[A-Za-z0-9_]+|GKG_CONVERTER_(TIMEOUT_S|OCR_ENGINE|OCR_LANG|IMAGES|MAX_IMAGE_MP|MAX_RESPONSE_MB)|DOCLING_MAX_SYNC_WAIT|EMBED_(QUERY_FURL|PRODUCT_FURL|API_KEY|BATCH_SIZE))=/ && $1!="GKG_AGENT_ENABLED" && $1!="GKG_EMBEDDER_ENABLED" {print}
' "$src" >"$tmp"
for key in MEMGRAPH_PASSWORD GKG_API_KEY; do
  line=
  if [ -f "$out" ]; then
    count=$(awk -F= -v key="$key" '$1==key {n++} END {print n+0}' "$out")
    [ "$count" -le 1 ] || fail 'duplicate stable secret key'
    line=$(awk -F= -v key="$key" '$1==key {print}' "$out")
  fi
  if [ -z "$line" ] || [ "$line" = "$key=" ]; then
    value=$(openssl rand -hex 32)
    printf '%s=%s\n' "$key" "$value" >>"$tmp"
    unset value
  else printf '%s\n' "$line" >>"$tmp"; fi
  unset line
 done
printf 'EDGE_BIND=0.0.0.0\nEDGE_SERVER_NAME=%s\nGKG_AGENT_ENABLED=true\nGKG_EMBEDDER_ENABLED=true\n' "$HOSTNAME" >>"$tmp"
hsts=0
if [ -f "$out" ]; then
  hsts=$(awk -F= '$1=="EDGE_HSTS_MAX_AGE" {print $2}' "$out")
  hsts=${hsts:-0}
fi
case "$hsts" in *[!0-9]*) fail 'invalid HSTS setting' ;; esac
printf 'EDGE_HSTS_MAX_AGE=%s\n' "$hsts" >>"$tmp"
chmod 600 "$tmp"; mv -f "$tmp" "$out"
awk -F= '{print $1}' "$out"
