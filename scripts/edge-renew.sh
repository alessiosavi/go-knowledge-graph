#!/usr/bin/env bash
# Arguments are the selected compose command, including its project/files.
set -euo pipefail
issue=0
staging=()
if [ "${1:-}" = --issue ]; then
  issue=1
  shift
  if [ "${1:-}" = --test-cert ]; then staging=(--test-cert); shift; fi
fi
[ "$#" -gt 0 ] || { printf '%s\n' 'edge: compose command required' >&2; exit 1; }
name="$("$@" exec -T edge printenv SERVER_NAME)"
if ! [[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9.-]*$ ]] || [ "${#name}" -gt 253 ]; then
  printf '%s\n' 'edge: invalid SERVER_NAME' >&2; exit 1
fi
if [ "$issue" = 1 ]; then
  [ "$name" != localhost ] || { printf '%s\n' 'edge: SERVER_NAME must be a public DNS name for issuance' >&2; exit 1; }
  "$@" --profile cert run --rm certbot certonly --webroot -w /var/www/acme -d "$name" --non-interactive --agree-tos --register-unsafely-without-email ${staging[@]+"${staging[@]}"}
fi
status="$("$@" --profile cert run --rm --entrypoint /edge-install-cert.sh certbot "$name")"
case "$status" in installed|unchanged) printf 'edge: certificate %s\n' "$status" ;; *) printf '%s\n' 'edge: invalid installer result' >&2; exit 1 ;; esac
expected="$("$@" exec -T edge openssl x509 -in /etc/nginx/edge-certs/current.crt -noout -fingerprint -sha256)"
[ -n "$expected" ] || { printf '%s\n' 'edge: installed fingerprint missing' >&2; exit 1; }
served() {
  "$@" exec -T edge sh -c '
    set -eu
    pem=$(timeout 5 openssl s_client -connect 127.0.0.1:8443 -servername "$1" </dev/null 2>/dev/null | openssl x509 -outform PEM) || exit 1
    printf "%s\n" "$pem" | openssl x509 -noout -fingerprint -sha256
    expires=$(printf "%s\n" "$pem" | openssl x509 -noout -enddate)
    epoch=$(date -u -d "${expires#notAfter=}" +%s)
    printf "%s\n" "$(( (epoch - $(date +%s)) / 86400 ))"
  ' sh "$name"
}
observed="$(served "$@")" || observed=
if [ "$status" = installed ] || [ "${observed%%$'\n'*}" != "$expected" ]; then
  if ! "$@" exec -T edge nginx -s reload; then
    printf '%s\n' 'edge: reload failed; fallback restart edge' >&2
    "$@" restart edge
  fi
  matched=0
  attempts="${EDGE_RENEW_ATTEMPTS:-30}"
  case "$attempts" in ''|*[!0-9]*) exit 1 ;; esac
  [ "$attempts" -gt 0 ] && [ "$attempts" -le 60 ] || exit 1
  for ((i=0; i<attempts; i++)); do
    observed="$(served "$@")" || observed=
    if [ "${observed%%$'\n'*}" = "$expected" ]; then matched=1; break; fi
    sleep 1
  done
  [ "$matched" = 1 ] || { printf '%s\n' 'edge: served fingerprint verification failed' >&2; exit 1; }
fi
days="${observed#*$'\n'}"
if ! [[ "$days" =~ ^-?[0-9]+$ ]]; then printf '%s\n' 'edge: served expiry verification failed' >&2; exit 1; fi
printf 'edge: served certificate has %s days left\n' "$days"
[ "$days" -ge 14 ] || { printf '%s\n' 'edge: certificate expires below the 14-day minimum' >&2; exit 1; }
