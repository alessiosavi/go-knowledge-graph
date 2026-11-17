#!/usr/bin/env bash
# Called under the host operations lock. Backups never enter the repo or image.
set -euo pipefail
umask 077
root=${GKG_ROOT:-/opt/gkg}
cd "$root"
compose=(docker compose --env-file "${GKG_ENV_FILE:-$root/.env}" -f deploy/docker-compose.yml)
backup=/var/backups/gkg
work=$(mktemp -d)
archive_tmp=
trap 'rm -rf "$work"; rm -f ${archive_tmp:+"$archive_tmp"}' EXIT
trap 'exit 1' HUP INT TERM
failed() { printf 'gkg-box: %s (private diagnostics withheld)\n' "$1" >&2; exit 1; }

# Both credentials expand in the Memgraph container, never on the host.
# Compare filenames before/after as well as ordering by mtime: a successful
# console exit without a new snapshot must not silently back up an old file.
# shellcheck disable=SC2016
"${compose[@]}" exec -T memgraph sh -c '
  set -eu
  dir=/var/lib/memgraph/snapshots
  before=$(find "$dir" -type f)
  printf "CREATE SNAPSHOT;\n" | mgconsole --username "$MEMGRAPH_USER" --password "$MEMGRAPH_PASSWORD" >/dev/null
  snapshot=$(ls -1t "$dir" | while IFS= read -r file; do
    [ -f "$dir/$file" ] || continue
    if ! printf "%s\n" "$before" | grep -Fxq "$dir/$file"; then
      printf "%s\n" "$file"
      break
    fi
  done)
  [ -n "$snapshot" ] || { echo "no new snapshot" >&2; exit 1; }
  printf "%s\n" "$snapshot"
  wc -c <"$dir/$snapshot"
' >"$work/snapshot" 2>"$work/error" || failed 'snapshot command failed'
snapshot=$(sed -n '1p' "$work/snapshot")
bytes=$(awk 'NR==2 {print $1}' "$work/snapshot")
[[ "$snapshot" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]*$ ]] || failed 'invalid snapshot filename'
[[ "$bytes" =~ ^[0-9]+$ ]] && [ "$bytes" -gt 0 ] || failed 'invalid snapshot size'
mkdir -p "$backup"
available=$(df -Pk "$backup" | awk 'NR==2 {print $4}')
[[ "$available" =~ ^[0-9]+$ ]] || failed 'cannot determine free backup disk'
[ "$((available*1024))" -ge "$((bytes*2))" ] || failed 'free disk is less than twice the snapshot size'
archive=$backup/$(date -u +%Y%m%dT%H%M%SZ).tar
[ ! -e "$archive" ] || failed 'snapshot archive already exists for this timestamp'
archive_tmp=$(mktemp "$backup/.snapshot.XXXXXX")
# shellcheck disable=SC2016
"${compose[@]}" exec -T memgraph sh -c 'exec tar -C /var/lib/memgraph/snapshots -cf - "$1"' sh "$snapshot" >"$archive_tmp" 2>"$work/error" || failed 'snapshot archive failed'
chmod 0600 "$archive_tmp"
mv -f "$archive_tmp" "$archive"
printf '%s\n' "$archive"
