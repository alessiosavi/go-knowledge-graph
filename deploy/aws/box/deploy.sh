#!/usr/bin/env bash
# Called under the host lock by remote.sh. Build before touching containers.
set -euo pipefail
umask 077
root=${GKG_ROOT:-/opt/gkg}
cd "$root"
[ "$#" = 1 ] && [[ "$1" =~ ^[A-Za-z0-9_./-]+$ ]] && [[ "$1" != -* ]] || { echo 'gkg-box: invalid REF' >&2; exit 1; }
prev=$(cat .deployed-sha)
[[ "$prev" =~ ^[A-Za-z0-9]+$ ]] || { echo 'gkg-box: missing valid deployed SHA' >&2; exit 1; }
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
failed() { echo "gkg-box: $1 (private diagnostics withheld)" >&2; exit 1; }
restore_revision() { git checkout --detach "$prev" >"$work/rollback" 2>&1 || failed 'revision rollback failed'; }
# Once startup begins, recover the selected files and the running stack.
# Continue to make up after a failed rebuild so recovery is best effort.
rollback_stack() {
  local reason=$1 recovered=1
  if git checkout --detach "$prev" >"$work/rollback-checkout" 2>&1; then
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
git fetch --tags origin >"$work/git" 2>&1 || { restore_revision; failed 'fetch failed; previous revision restored'; }
sha=$(git rev-parse --verify "$1^{commit}" 2>"$work/git") || { restore_revision; failed 'REF is not a commit; previous revision restored'; }
if ! git checkout --detach "$sha" >"$work/git" 2>&1; then
  git checkout --detach "$prev" >"$work/rollback" 2>&1 || failed 'checkout and rollback failed'
  failed 'checkout failed; previous revision restored'
fi
# Invoke from the selected checkout so secret rotation applies to every deploy.
if ! bash "$root/deploy/aws/box/fetch-secrets.sh"; then
  git checkout --detach "$prev" >"$work/rollback" 2>&1 || failed 'secret fetch and rollback failed'
  failed 'secret fetch failed; previous revision restored'
fi
# make build uses the same compose file and substitutions as make up.
if ! make build >"$work/build" 2>&1; then
  git checkout --detach "$prev" >"$work/rollback" 2>&1 || failed 'build and checkout rollback failed'
  make build >"$work/rebuild" 2>&1 || failed 'previous image rebuild failed; recovery required'
  failed 'build failed; previous revision and image tags restored'
fi
make up >"$work/up" 2>&1 || rollback_stack 'stack startup failed'
# Read the effective value from the container, rather than reimplement dotenv.
# Secret bytes are redirected to a header file, never passed as arguments.
printf 'Authorization: Bearer ' >"$work/header"
docker compose --env-file "${GKG_ENV_FILE:-$root/.env}" -f deploy/docker-compose.yml exec -T gkg printenv GKG_API_KEY >>"$work/header" 2>/dev/null || rollback_stack 'read effective bearer failed'
[ "$(wc -c <"$work/header")" -gt 23 ] || rollback_stack 'effective bearer missing'
ready=0
for ((i=0;i<30;i++)); do
  code=$(curl -q --silent --noproxy '*' --connect-timeout 2 --max-time 2 -H "@$work/header" --output /dev/null --write-out '%{http_code}' http://127.0.0.1:8080/readyz 2>/dev/null) || code=000
  if [ "$code" = 200 ]; then ready=1; break; fi
  sleep 2
done
[ "$ready" = 1 ] || rollback_stack 'loopback readiness failed'
printf '%s\n' "$sha" >"$work/sha"
mv -f "$work/sha" "$root/.deployed-sha"
printf '%s\n' "$sha"
