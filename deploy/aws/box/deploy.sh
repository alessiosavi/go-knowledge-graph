#!/usr/bin/env bash
# Called under the host lock by remote.sh. Build before touching containers.
set -euo pipefail
umask 077
root=${GKG_ROOT:-/opt/gkg}
cd "$root"
[ "$#" -ge 1 ] && [ "$#" -le 2 ] && [[ "$1" =~ ^[A-Za-z0-9_./-]+$ ]] && [[ "$1" != -* ]] || { echo 'gkg-box: invalid REF' >&2; exit 1; }
[ "$#" = 1 ] || [ "$2" = no-rollback ] || { echo 'gkg-box: second argument must be no-rollback' >&2; exit 1; }
no_rollback=${2:-}
migrated=0
prev=$(cat .deployed-sha)
[[ "$prev" =~ ^[A-Za-z0-9]+$ ]] || { echo 'gkg-box: missing valid deployed SHA' >&2; exit 1; }
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
failed() { echo "gkg-box: $1 (private diagnostics withheld)" >&2; exit 1; }
compose=(docker compose --env-file "${GKG_ENV_FILE:-$root/.env}" -f deploy/docker-compose.yml)
recovery_required() {
  printf 'gkg-box: %s; manual recovery required (%s)\n' "$1" "$2" >&2
  printf '%s\n' \
    'Stop gkg and Memgraph before restoring data.' \
    "Restore the pre-deploy snapshot from /var/backups/gkg with previous revision $prev (see docs/deploy-aws.md)." \
    'Only after restoring the data and previous revision, run make build and make up, then verify readiness.' >&2
  exit 1
}
startup_failed() {
  [ "$migrated" != 1 ] || recovery_required "$1" 'automatic rollback withheld after schema migration'
  [ "$no_rollback" != no-rollback ] || recovery_required "$1" 'automatic rollback disabled'
  rollback_stack "$1"
}
# Only Git may create public repository files; diagnostics and secrets stay private.
repo_git() ( umask 022; git "$@"; )
restore_revision() { repo_git checkout --detach "$prev" >"$work/rollback" 2>&1 || failed 'revision rollback failed'; }
# Rebuild after restoring the revision so cached image tags match it too.
restore_image_tags() { make build >"$work/rebuild" 2>&1 || failed 'previous image rebuild failed; recovery required'; }
# Once startup begins, recover the selected files and the running stack.
# Continue to make up after a failed rebuild so recovery is best effort.
rollback_stack() {
  local reason=$1 recovered=1
  if repo_git checkout --detach "$prev" >"$work/rollback-checkout" 2>&1; then
    make build >"$work/rollback-build" 2>&1 || recovered=0
    make up >"$work/rollback-up" 2>&1 || recovered=0
  else
    recovered=0
  fi
  if [ "$recovered" = 1 ]; then
    failed "$reason; previous revision restored"
  fi
  failed "$reason; recovery required"
}
repo_git fetch --tags origin >"$work/git" 2>&1 || { restore_revision; failed 'fetch failed; previous revision restored'; }
sha=$(git rev-parse --verify "$1^{commit}" 2>"$work/git") || { restore_revision; failed 'REF is not a commit; previous revision restored'; }
if ! repo_git checkout --detach "$sha" >"$work/git" 2>&1; then
  repo_git checkout --detach "$prev" >"$work/rollback" 2>&1 || failed 'checkout and rollback failed'
  failed 'checkout failed; previous revision restored'
fi
# Invoke from the selected checkout so secret rotation applies to every deploy.
if ! bash "$root/deploy/aws/box/fetch-secrets.sh"; then
  repo_git checkout --detach "$prev" >"$work/rollback" 2>&1 || failed 'secret fetch and rollback failed'
  failed 'secret fetch failed; previous revision restored'
fi
# make build uses the same compose file and substitutions as make up.
if ! make build >"$work/build" 2>&1; then
  repo_git checkout --detach "$prev" >"$work/rollback" 2>&1 || failed 'build and checkout rollback failed'
  restore_image_tags
  failed 'build failed; previous revision and image tags restored'
fi
# Preflight is read-only and runs on the newly built image. Older binaries
# return 2 for the unknown command; never run migrate on those images.
preflight=0
"${compose[@]}" run --rm -T --no-deps gkg preflight >"$work/preflight" 2>&1 || preflight=$?
case "$preflight" in
  0|2) ;;
  3) restore_revision; restore_image_tags; failed 'seed or stoplist overlay invalid; previous revision and image tags restored' ;;
  10)
    "${compose[@]}" stop gkg >"$work/stop" 2>&1 || {
      restore_revision
      restore_image_tags
      # A failed stop may already have stopped gkg; restart the restored image.
      make up >"$work/rollback-up" 2>&1 || true
      failed 'stop gkg failed; previous revision and image tags restored'
    }
    "${compose[@]}" run --rm -T --no-deps gkg migrate --schema-only >"$work/migrate" 2>&1 || recovery_required 'schema migration failed' 'automatic rollback withheld after schema migration'
    migrated=1
    ;;
  *) restore_revision; restore_image_tags; failed "preflight failed (exit $preflight); previous revision and image tags restored" ;;
esac
make up >"$work/up" 2>&1 || startup_failed 'stack startup failed'
# Read the effective value from the container, rather than reimplement dotenv.
# Secret bytes are redirected to a header file, never passed as arguments.
printf 'Authorization: Bearer ' >"$work/header"
"${compose[@]}" exec -T gkg printenv GKG_API_KEY >>"$work/header" 2>/dev/null || startup_failed 'read effective bearer failed'
[ "$(wc -c <"$work/header")" -gt 23 ] || startup_failed 'effective bearer missing'
ready=0
for ((i=0;i<30;i++)); do
  code=$(curl -q --silent --noproxy '*' --connect-timeout 2 --max-time 2 -H "@$work/header" --output /dev/null --write-out '%{http_code}' http://127.0.0.1:8080/readyz 2>/dev/null) || code=000
  if [ "$code" = 200 ]; then ready=1; break; fi
  sleep 2
done
[ "$ready" = 1 ] || startup_failed 'loopback readiness failed'
printf '%s\n' "$sha" >"$work/sha"
mv -f "$work/sha" "$root/.deployed-sha"
printf '%s\n' "$sha"
