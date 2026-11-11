#!/usr/bin/env bash
# Capture generated stdout directly; neither make nor bcrypt sees it in argv.
set -euo pipefail
# shellcheck source=deploy/aws/lib.sh
source "$(dirname "$0")/lib.sh"
load_box_env
out=${BOX_NETRC:-$AWS_DIR/box.netrc}
auth=${BOX_HTPASSWD:-$AWS_DIR/box.htpasswd}
[ ! -L "$out" ] && [ ! -L "$auth" ] || fail 'refusing a symlink destination'
tmp=$(mktemp "${out}.tmp.XXXXXX"); pw=$(mktemp "${out}.pw.XXXXXX")
trap 'rm -f "$tmp" "$pw"' EXIT
trap 'exit 1' HUP INT TERM
cd "$AWS_DIR/../.."
make --no-print-directory -s passwd LOGIN=deploy GENERATE=1 "EDGE_HTPASSWD=$auth" >"$pw" 2>/dev/null || fail 'deploy login generation failed'
# Generated base64 is a single netrc-safe token; never interpolate it in a tool.
awk 'NR!=1 || $0!~/^[A-Za-z0-9+\/=.-]+$/ {exit 1} END {if(NR!=1) exit 1}' "$pw" || fail 'invalid generated login password'
{ printf 'machine %s login deploy password ' "$HOSTNAME"; cat "$pw"; } >"$tmp"
chmod 600 "$tmp"; mv -f "$tmp" "$out"
