#!/usr/bin/env bash
set -euo pipefail
mode="$1"
file="$2"
case "$mode" in
  On|DetectionOnly) ;;
  *) printf '%s\n' 'edge: MODE must be On or DetectionOnly'; exit 1 ;;
esac
umask 077
mkdir -p "$(dirname "$file")"
pending="$(mktemp "${file}.XXXXXX")"
trap 'rm -f "$pending"' EXIT
trap 'exit 1' HUP INT TERM
printf 'MODSEC_RULE_ENGINE=%s\n' "$mode" > "$pending"
chmod 600 "$pending"
mv -f "$pending" "$file"
