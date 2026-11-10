#!/usr/bin/env bash
# Offline acceptance test. All state and credentials belong to this project.
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
started="$(date +%s)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/gkg-edge-check.XXXXXX")"
chmod 700 "$tmp"
project="gkg-edge-check-$started"
make_args=("ENV_FILE=$tmp/edge.env" "EDGE_HTPASSWD=$tmp/htpasswd" "EDGE_WAF_ENV=$tmp/waf.env" "PROJECT=$project" LIGHT=1 COMPOSE_EXTRA=deploy/compose.edgecheck.yml)
compose=(docker compose -p "$project" --env-file "$tmp/edge.env" -f deploy/docker-compose.yml -f deploy/compose.light.yml -f deploy/compose.edgecheck.yml)
export GKG_ENV_FILE="$tmp/edge.env" EDGE_HTPASSWD="$tmp/htpasswd" EDGE_WAF_ENV="$tmp/waf.env"
active=0
cleanup() {
  result=$?
  trap - EXIT
  # Diagnostics are optional. A failed write must never abort the EXIT trap
  # before the project and its private credential files have been removed.
  set +e
  if [ -n "${EDGE_CHECK_EVIDENCE_DIR:-}" ]; then
    if [ "$active" = 1 ]; then
      "${compose[@]}" logs --no-color --no-log-prefix edge >"$tmp/final-edge.log" 2>&1 || :
      "${compose[@]}" logs --no-color --no-log-prefix gkg >"$tmp/final-gkg.log" 2>&1 || :
      ids="$("${compose[@]}" ps -aq)"
      if [ -n "$ids" ]; then
        docker inspect --format '{{.Name}} restarts={{.RestartCount}} oom={{.State.OOMKilled}} exit={{.State.ExitCode}}' $ids >"$tmp/container-state.log" 2>&1 || :
      fi
    fi
    mkdir -p "$EDGE_CHECK_EVIDENCE_DIR"
    chmod 700 "$EDGE_CHECK_EVIDENCE_DIR"
    # Only this project's diagnostics, never credential/config files.
    for artifact in "$tmp"/*.log "$tmp"/after-* "$tmp"/before-*; do
      [ ! -f "$artifact" ] || cp "$artifact" "$EDGE_CHECK_EVIDENCE_DIR/"
    done
  fi
  if [ "$active" = 1 ]; then
    if ! make destroy CONFIRM=yes "${make_args[@]}" >"$tmp/cleanup.log" 2>&1; then
      printf '%s\n' 'FAIL: isolated project cleanup'
      result=1
    fi
    if [ -d "${EDGE_CHECK_EVIDENCE_DIR:-}" ]; then
      cp "$tmp/cleanup.log" "$EDGE_CHECK_EVIDENCE_DIR/" 2>/dev/null
    fi
  fi
  if ! rm -rf "$tmp"; then printf '%s\n' 'FAIL: private temp cleanup'; result=1; fi
  printf 'OK: edge-check duration = %ss\n' "$(( $(date +%s) - started ))"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
fail() { printf 'FAIL: %s\n' "$*"; exit 1; }
ok() { printf 'OK: %s\n' "$*"; }

# Use a private buildx state directory as well: no shared metadata writes.
export BUILDX_CONFIG="$tmp/buildx"
mkdir -p "$BUILDX_CONFIG"
printf '%s' edgecheck >"$tmp/login"
printf 'edge$:\047"%s' "$(openssl rand -hex 12)" >"$tmp/password"
openssl rand -hex 32 | tr -d '\n' >"$tmp/api-key"
openssl rand -hex 24 | tr -d '\n' >"$tmp/memgraph-password"
printf 'edgecorpus%s' "$(openssl rand -hex 12)" >"$tmp/marker"
openssl rand -hex 8 | tr -d '\n' >"$tmp/attack-marker"
cat >"$tmp/edge.env" <<EOF
EDGE_BIND=127.0.0.1
EDGE_SERVER_NAME=localhost
EDGE_HTTP_PORT=${EDGE_HTTP_PORT:-28080}
EDGE_HTTPS_PORT=${EDGE_HTTPS_PORT:-28443}
GKG_PORT=${GKG_PORT:-28085}
MEMGRAPH_PORT=${MEMGRAPH_PORT:-27687}
MEMGRAPH_USER=gkg
MEMGRAPH_PASSWORD=$(cat "$tmp/memgraph-password")
GKG_API_KEY=$(cat "$tmp/api-key")
GKG_AGENT_ENABLED=true
GKG_AGENT_BASE_URL=http://llmstub:8099/v1
GKG_AGENT_API_KEY=offline
GKG_AGENT_MODEL=edge-check
GKG_AGENT_STREAM=true
GKG_EMBEDDER_ENABLED=false
GKG_LIGHT_EXTRACTOR_MODE=mock
GKG_CONVERTER_MODE=off
EOF
# These generated values must also beat inherited shell settings in Compose.
while IFS= read -r setting; do export "$setting"; done <"$tmp/edge.env"
base="https://localhost:$EDGE_HTTPS_PORT"
go build -o "$tmp/check" ./scripts/edgecheck
"$tmp/check" prepare "$tmp"
{ cat "$tmp/password"; printf '\n'; } | make passwd LOGIN=edgecheck "${make_args[@]}" >"$tmp/passwd.log" 2>&1 || fail 'password helper'
ok 'private password with dollar, colon and quotes'
active=1
make up "${make_args[@]}" >"$tmp/up.log" 2>&1 || fail 'isolated stack startup (private diagnostics withheld)'
"${compose[@]}" cp edge:/etc/nginx/edge-certs/current.crt "$tmp/ca.crt" >"$tmp/cp.log" 2>&1 || fail 'copy placeholder certificate'
ready=0
for ((attempt=0; attempt<60; attempt++)); do
  if curl -q --silent --show-error --noproxy '*' --max-time 2 --cacert "$tmp/ca.crt" -f -o /dev/null "$base/healthz" 2>/dev/null; then ready=1; break; fi
  sleep 1
done
[ "$ready" = 1 ] || fail 'bounded TLS readiness'
ok 'TLS certificate trusted explicitly'

# Test ACME from the HTTP listener without credentials, with our own webroot.
# edge's mount is read-only; a one-shot with the same isolated volume writes it.
"${compose[@]}" run --rm --no-deps --entrypoint sh edge-init -c 'printf "edge-acme-check\n" > /var/www/acme/.well-known/acme-challenge/edge-check' >"$tmp/acme-write.log" 2>&1 || fail 'ACME fixture'
code="$(curl -q --silent --show-error --noproxy '*' --max-time 10 -o "$tmp/acme" -w '%{http_code}' "http://localhost:$EDGE_HTTP_PORT/.well-known/acme-challenge/edge-check")"
[ "$code" = 200 ] && [ "$(cat "$tmp/acme")" = edge-acme-check ] || fail 'ACME without auth'
ok 'ACME HTTP without auth'
code="$(curl -q --silent --show-error --noproxy '*' --max-time 10 -H 'Host: untrusted.invalid' -D "$tmp/redirect" -o /dev/null -w '%{http_code}' "http://localhost:$EDGE_HTTP_PORT/ui/")"
[ "$code" = 301 ] && grep -Fqi "Location: $base/ui/" "$tmp/redirect" || fail 'redirect uses configured host'
ok 'HTTP redirect ignores inbound Host'

capture() { "${compose[@]}" logs --no-color --no-log-prefix edge >"$1" 2>&1; }
for mode in DetectionOnly On; do
  make waf-mode "MODE=$mode" "${make_args[@]}" >"$tmp/waf-mode.log" 2>&1 || fail 'WAF mode recreation'
  ready=0
  for ((attempt=0; attempt<60; attempt++)); do
    if curl -q --silent --noproxy '*' --max-time 2 --cacert "$tmp/ca.crt" -f -o /dev/null "$base/healthz"; then ready=1; break; fi
    sleep 1
  done
  [ "$ready" = 1 ] || fail 'WAF mode readiness'
  # Rejected methods, traversal and cross-site requests are negative probes,
  # separate from the legitimate corpus whose CRS count must be zero.
  ok "section auth ($mode)"
  "$tmp/check" auth "$tmp" "$base" "$mode"
  # Positive control precedes every zero-match assertion. Logs are compared by
  # unique transaction ID, never grep-counted or sliced by uncertain timestamps.
  ok "section waf ($mode)"
  capture "$tmp/before-positive"
  "$tmp/check" waf "$tmp" "$base" "$mode"
  sleep 1
  capture "$tmp/after-positive"
  "$tmp/check" audit "$tmp/before-positive" "$tmp/after-positive" positive
  # DetectionOnly processes partial bodies instead of rejecting them, even
  # with Reject configured. Prove the production On-mode rejection locally,
  # independently of the response status and legitimate CRS window.
  if [ "$mode" = On ]; then
    "${compose[@]}" logs --no-color --no-log-prefix gkg >"$tmp/before-nofiles"
    "$tmp/check" reject-json "$tmp" "$base"
    "${compose[@]}" logs --no-color --no-log-prefix gkg >"$tmp/after-nofiles"
    "$tmp/check" nofiles-upstream "$tmp/before-nofiles" "$tmp/after-nofiles"
  fi
  capture "$tmp/before-legit"
  for section in routing limits stream mcp; do
    ok "section $section ($mode)"
    "$tmp/check" "$section" "$tmp" "$base" long
  done
  sleep 1
  capture "$tmp/after-legit"
  "$tmp/check" audit "$tmp/before-legit" "$tmp/after-legit" zero
  # Unlisted requests must not appear in gkg's request log (nginx status alone
  # cannot demonstrate that no upstream request was made).
  "${compose[@]}" logs --no-color --no-log-prefix gkg >"$tmp/gkg.log" 2>&1
  grep -F '"path":"/v1/documents"' "$tmp/gkg.log" >/dev/null || fail 'upstream request log positive control'
  if grep -E '"path":"(/metrics|/readyz|/v1/cypher|/v1/unknown|/ui/\.\./metrics)"|"method":"(PUT|OPTIONS)"' "$tmp/gkg.log" >/dev/null; then fail 'allowlist reached upstream'; fi
  ok 'allowlist absent from upstream logs'
  ok 'section hygiene'
  "${compose[@]}" logs --no-color --no-log-prefix >"$tmp/all.log" 2>&1
  "$tmp/check" hygiene "$tmp" "$tmp/all.log" "$tmp/after-legit"
  "${compose[@]}" exec -T edge sh -c 'printf "memory.peak="; cat /sys/fs/cgroup/memory.peak; cat /sys/fs/cgroup/memory.events' >"$tmp/resources-$mode.log"
  ids="$("${compose[@]}" ps -aq)"
  docker inspect --format '{{.Name}} restarts={{.RestartCount}} oom={{.State.OOMKilled}} exit={{.State.ExitCode}} memory_limit={{.HostConfig.Memory}}' $ids >>"$tmp/resources-$mode.log"
  cat "$tmp/resources-$mode.log"
  if grep -E '^(oom|oom_kill|oom_group_kill) [1-9]|restarts=[1-9]|oom=true' "$tmp/resources-$mode.log" >/dev/null; then fail 'container OOM or restart'; fi
done
# TODO(Task 3): renewal section, reload during a held ask stream/new request,
# and old-worker shutdown observation for a held MCP GET, per Rev 2 Step 3.
printf '%s\n' 'EDGE CHECK PASS (engine On)'
