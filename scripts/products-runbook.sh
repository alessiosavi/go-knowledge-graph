#!/usr/bin/env bash
# Run the P8 catalogue proof against an isolated, disposable database.
set -euo pipefail

name=gkgrf-memgraph
port=${PORT:-17756}
if [[ ! "$port" =~ ^[1-9][0-9]{0,4}$ ]] || (( port > 65535 || port == 7687 )); then
    printf 'PORT must be a non-default Bolt port (1–65535, excluding 7687)\n' >&2
    exit 2
fi
: "${GKG_PRODUCTS_RUNBOOK_DIR:?Set GKG_PRODUCTS_RUNBOOK_DIR to the frozen Part A mirror}"
GKG_PRODUCTS_RUNBOOK_DIR=$(cd "$GKG_PRODUCTS_RUNBOOK_DIR" && pwd -P)
export GKG_PRODUCTS_RUNBOOK_DIR

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
image=$(awk '$1 == "image:" && $2 ~ /^memgraph\/memgraph:3\.14\.0@sha256:/ { print $2 }' "$repo/deploy/docker-compose.yml")
if [[ ! "$image" =~ ^memgraph/memgraph:3\.14\.0@sha256:[a-f0-9]{64}$ ]]; then
    printf 'Expected one digest-pinned Memgraph 3.14.0 image in deploy/docker-compose.yml\n' >&2
    exit 1
fi
existing=$(docker ps -aq --filter "name=^/${name}$")
if [[ -n "$existing" ]]; then
    printf 'Container %s already exists\n' "$name" >&2
    exit 1
fi

cid=
temporary_out=
# ShellCheck does not recognize the EXIT trap as a call.
# shellcheck disable=SC2329
cleanup() {
    local status=$1
    if [[ -n "$cid" ]]; then
        docker stop "$cid" >/dev/null || true
        docker rm -fv "$cid" >/dev/null || status=1
    fi
    if [[ -n "$temporary_out" ]]; then
        rm -rf -- "$temporary_out"
    fi
    exit "$status"
}
trap 'cleanup "$?"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [[ -z ${GKG_PRODUCTS_RUNBOOK_OUT:-} ]]; then
    temporary_out=$(mktemp -d "${TMPDIR:-/tmp}/products-runbook.XXXXXXXX")
    GKG_PRODUCTS_RUNBOOK_OUT=$temporary_out
fi
mkdir -p "$GKG_PRODUCTS_RUNBOOK_OUT"
GKG_PRODUCTS_RUNBOOK_OUT=$(cd "$GKG_PRODUCTS_RUNBOOK_OUT" && pwd -P)
export GKG_PRODUCTS_RUNBOOK_OUT

cid=$(docker create --pull=never --name "$name" --memory 1344m \
    --publish "127.0.0.1:$port:7687" "$image" --memory-limit=832 --telemetry-enabled=false)
docker start "$cid" >/dev/null
ready=false
for (( attempt=0; attempt<60; attempt++ )); do
    if docker exec -i "$cid" mgconsole <<<'RETURN 1;' >/dev/null 2>&1; then
        ready=true
        break
    fi
    sleep 1
done
if [[ "$ready" != true ]]; then
    printf 'Scratch Memgraph did not accept Bolt connections\n' >&2
    docker logs --tail 40 "$cid" >&2
    exit 1
fi

cd "$repo"
export GKG_TEST_MEMGRAPH_URI="bolt://127.0.0.1:$port"
rm -f "$GKG_PRODUCTS_RUNBOOK_OUT/measurements.json"
status=0
go test -run '^TestProductsRunbook$' -v -count=1 -timeout 90m ./cmd/gkg || status=$?
if [[ -f "$GKG_PRODUCTS_RUNBOOK_OUT/measurements.json" ]]; then
    cat "$GKG_PRODUCTS_RUNBOOK_OUT/measurements.json"
elif (( status == 0 )); then
    printf 'Runbook did not produce measurements.json\n' >&2
    status=1
fi
exit "$status"
