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
      "${compose[@]}" exec -T edge sh -c 'printf "memory.peak="; cat /sys/fs/cgroup/memory.peak; cat /sys/fs/cgroup/memory.events' >"$tmp/resources-exit.log" 2>&1 || :
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
# A stale, mismatched two-file generation must not prevent fresh TLS startup.
"${compose[@]}" --profile cert run --rm --no-deps --entrypoint sh certbot -c 'printf "obsolete certificate\n" > /etc/nginx/edge-certs/current.crt; printf "obsolete key\n" > /etc/nginx/edge-certs/current.key' >"$tmp/legacy-certs.log" 2>&1 || fail 'legacy certificate fixture'
make up "${make_args[@]}" >"$tmp/up.log" 2>&1 || fail 'isolated stack startup (private diagnostics withheld)'
"${compose[@]}" exec -T edge openssl x509 -in /etc/nginx/edge-certs/current.pem -outform PEM >"$tmp/ca.crt" 2>"$tmp/cp.log" || fail 'read placeholder certificate'
"${compose[@]}" exec -T edge sh -c 'test ! -e /etc/nginx/edge-certs/current.crt && test ! -e /etc/nginx/edge-certs/current.key' || fail 'legacy certificate cleanup'
ok 'legacy certificate/key ignored and removed at startup'
ready=0
for ((attempt=0; attempt<60; attempt++)); do
  if curl -q --silent --show-error --noproxy '*' --max-time 2 --cacert "$tmp/ca.crt" -f -o /dev/null "$base/healthz" 2>/dev/null; then ready=1; break; fi
  sleep 1
done
[ "$ready" = 1 ] || fail 'bounded TLS readiness'
ok 'TLS certificate trusted explicitly'

# Test ACME from the HTTP listener without credentials, with our own webroot.
# edge's mount is read-only; a one-shot with the same isolated volume writes it.
"${compose[@]}" --profile cert run --rm --no-deps --entrypoint sh certbot -c 'mkdir -p /var/www/acme/.well-known/acme-challenge; printf "edge-acme-check\n" > /var/www/acme/.well-known/acme-challenge/edge-check' >"$tmp/acme-write.log" 2>&1 || fail 'ACME fixture'
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
  "${compose[@]}" logs --no-color --no-log-prefix gkg >"$tmp/before-encoded-$mode.log" 2>&1
  "$tmp/check" encoded-paths "$tmp" "$base" "$mode"
  "${compose[@]}" logs --no-color --no-log-prefix gkg >"$tmp/after-encoded-$mode.log" 2>&1
  "$tmp/check" encoded-upstream "$tmp/before-encoded-$mode.log" "$tmp/after-encoded-$mode.log"
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
ok 'section renewal (offline fake lineage only)'
workers() {
  "${compose[@]}" exec -T edge sh -c '
    for path in /proc/[0-9]*/cmdline; do
      command=$(tr "\000" " " <"$path" 2>/dev/null) || continue
      case "$command" in "nginx: worker process"*) pid=${path#/proc/}; printf "%s\n" "${pid%/cmdline}" ;; esac
    done' | sort -n
}
served_fingerprint() {
  "${compose[@]}" exec -T edge sh -c '
    timeout 5 openssl s_client -connect 127.0.0.1:8443 -servername "$SERVER_NAME" </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256'
}
live_fingerprint() {
  "${compose[@]}" --profile cert run --rm --entrypoint sh certbot -c 'openssl x509 -in /etc/letsencrypt/live/localhost/fullchain.pem -noout -fingerprint -sha256' 2>"$tmp/live-fingerprint.log"
}
fake_lineage() {
  "${compose[@]}" --profile cert run --rm --entrypoint sh certbot -c '
    set -eu
    live=/etc/letsencrypt/live/localhost
    mkdir -p "$live"
    openssl req -x509 -nodes -newkey rsa:2048 -days "$1" -subj /CN=localhost -addext subjectAltName=DNS:localhost -keyout "$live/privkey.pem" -out "$live/fullchain.pem" >/dev/null 2>&1
    chmod 0600 "$live/privkey.pem"
  ' sh "$1" >"$tmp/fake-lineage.log" 2>&1 || fail 'fake certificate fixture'
}
refresh_ca() {
  "${compose[@]}" exec -T edge openssl x509 -in /etc/nginx/edge-certs/current.pem -outform PEM >"$tmp/next-ca.crt" 2>"$tmp/cp.log" || fail 'read renewed trust certificate'
  mv "$tmp/next-ca.crt" "$tmp/ca.crt"
}
assert_changed_workers() {
  after_workers="$(workers)"
  [ -n "$after_workers" ] && [ "$before_workers" != "$after_workers" ] || fail 'reload changed nginx worker PIDs'
  printf 'OK: nginx worker PIDs before=[%s] after=[%s]\n' "$(printf '%s' "$before_workers" | tr '\n' ',')" "$(printf '%s' "$after_workers" | tr '\n' ',')"
}
initial="$(served_fingerprint)"
before_workers="$(workers)"
[ -n "$before_workers" ] || fail 'nginx worker PID observation'
make renew "${make_args[@]}" >"$tmp/renew-empty.log" 2>&1 || fail 'renew with no live certificate'
grep -F 'unchanged' "$tmp/renew-empty.log" >/dev/null || fail 'no-live install result'
[ "$(served_fingerprint)" = "$initial" ] && [ "$(workers)" = "$before_workers" ] || fail 'no-live renewal changed edge'
ok 'no-live renewal unchanged: fingerprint and worker PIDs'
fake_lineage 365
expected="$(live_fingerprint)"
[ "$expected" != "$initial" ] || fail 'fake lineage fingerprint differs'
# Trust both local certificates during HUP so requests can authenticate either
# generation while proving uninterrupted service throughout renewal.
"${compose[@]}" --profile cert run --rm --entrypoint sh certbot -c 'cat /etc/letsencrypt/live/localhost/fullchain.pem' >"$tmp/next-ca.crt" 2>"$tmp/next-ca.log"
cat "$tmp/ca.crt" "$tmp/next-ca.crt" >"$tmp/both-ca.crt"
mv "$tmp/both-ca.crt" "$tmp/ca.crt"
# The wrapper replaces the bundle with the new trust anchor after verification.
{
  printf '#!/bin/bash\nset -euo pipefail\n'
  printf '%q ' make renew "${make_args[@]}"; printf '\n'
  printf '%q ' "${compose[@]}" exec -T edge openssl x509 -in /etc/nginx/edge-certs/current.pem -outform PEM; printf ' > %q\n' "$tmp/next-ca.crt"
  printf '%q ' mv "$tmp/next-ca.crt" "$tmp/ca.crt"; printf '\n'
} >"$tmp/renew-held.sh"
chmod 700 "$tmp/renew-held.sh"
"$tmp/check" reload "$tmp" "$base" bash "$tmp/renew-held.sh"
[ "$(served_fingerprint)" = "$expected" ] || fail 'served fingerprint equals live certificate'
assert_changed_workers
ok 'served fingerprint changed and equals live certificate'
# This image has no main-context shutdown-timeout include; once the held GET
# closes, the old worker must drain. The 120-second timeout remains unset.
for ((attempt=0; attempt<10; attempt++)); do
  remaining=0
  current_workers="$(workers)"
  for pid in $before_workers; do
    for current in $current_workers; do [ "$pid" != "$current" ] || remaining=1; done
  done
  [ "$remaining" = 1 ] || break
  sleep 1
done
[ "$remaining" = 0 ] || fail 'old nginx workers drained after held MCP GET'
ok 'old workers drained after held MCP GET (shutdown timeout unset)'
permissions="$("${compose[@]}" exec -T edge stat -c '%a:%u:%g' /etc/nginx/edge-certs/current.pem)"
[ "$permissions" = '640:0:101' ] || fail 'installed certificate bundle mode and owner/group'
ok 'certificate bundle mode 0640, owner root, group 101'
before_workers="$(workers)"
make renew "${make_args[@]}" >"$tmp/renew-noop.log" 2>&1 || fail 'idempotent renewal'
grep -F 'unchanged' "$tmp/renew-noop.log" >/dev/null || fail 'idempotent install result'
[ "$(served_fingerprint)" = "$expected" ] && [ "$(workers)" = "$before_workers" ] || fail 'idempotent renewal changed edge'
ok 'repeat renewal unchanged: fingerprint and worker PIDs'
"${compose[@]}" --profile cert run --rm --entrypoint sh certbot -c 'chmod 000 /etc/letsencrypt/live/localhost/privkey.pem' >"$tmp/key-mode.log" 2>&1
if make renew "${make_args[@]}" >"$tmp/renew-failed.log" 2>&1; then fail 'unreadable live key accepted'; fi
[ "$(served_fingerprint)" = "$expected" ] && [ "$(workers)" = "$before_workers" ] || fail 'failed installation changed edge'
ok 'unreadable key: nonzero exit, served fingerprint and worker PIDs unchanged'
# Recover a failed reload from a previous invocation: disk is new, TLS is old.
fake_lineage 365
"${compose[@]}" --profile cert run --rm --entrypoint /edge-install-cert.sh certbot localhost >"$tmp/install-only.log" 2>&1 || fail 'install-only recovery fixture'
[ "$(served_fingerprint)" = "$expected" ] || fail 'install-only unexpectedly reloaded'
expected="$(live_fingerprint)"
make renew "${make_args[@]}" >"$tmp/renew-repair.log" 2>&1 || fail 'recover installed but unserved certificate'
grep -F 'unchanged' "$tmp/renew-repair.log" >/dev/null || fail 'recovery should find installed files'
[ "$(served_fingerprint)" = "$expected" ] || fail 'recovery served fingerprint'
refresh_ca
assert_changed_workers
ok 'recovered installed-but-unserved certificate by fingerprint'
fake_lineage 7
expected="$(live_fingerprint)"
if make renew "${make_args[@]}" >"$tmp/renew-expiry.log" 2>&1; then fail '7-day expiry accepted'; fi
[ "$(served_fingerprint)" = "$expected" ] || fail 'short-lived certificate was not served'
grep -E '(6|7) days left' "$tmp/renew-expiry.log" >/dev/null || fail 'expiry failure names remaining days'
ok '7-day certificate installed and served; renew exits nonzero below 14 days'
refresh_ca
"$tmp/check" stream "$tmp" "$base"
"${compose[@]}" logs --no-color --no-log-prefix >"$tmp/all-renewal.log" 2>&1
"$tmp/check" hygiene "$tmp" "$tmp/all-renewal.log" "$tmp/after-legit"
"${compose[@]}" exec -T edge sh -c 'printf "memory.peak="; cat /sys/fs/cgroup/memory.peak; cat /sys/fs/cgroup/memory.events' >"$tmp/resources-renewal.log"
cat "$tmp/resources-renewal.log"
if grep -E '^(oom|oom_kill|oom_group_kill) [1-9]' "$tmp/resources-renewal.log" >/dev/null; then fail 'renewal OOM'; fi
printf '%s\n' 'EDGE CHECK PASS (engine On)'
