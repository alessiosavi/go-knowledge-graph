#!/usr/bin/env bash
# Check a running gkg API and remove the temporary document on every exit.
# curl >= 7.55 supports private header files; no jq or Go is needed.
set -euo pipefail

fail() { printf 'SMOKE FAIL: %s\n' "$*"; exit 1; }
ok() { printf 'OK: %s\n' "$*"; }

[[ -n "${SMOKE_URL:-}" ]] || fail 'SMOKE_URL is required'
SMOKE_URL="${SMOKE_URL%/}"
SMOKE_TIMEOUT_S="${SMOKE_TIMEOUT_S:-180}"
SMOKE_POLL_S="${SMOKE_POLL_S:-2}"
SMOKE_REQ_TIMEOUT_S="${SMOKE_REQ_TIMEOUT_S:-30}"
case "$SMOKE_TIMEOUT_S" in ''|*[!0-9]*) fail 'SMOKE_TIMEOUT_S must be an integer' ;; esac
[[ "$SMOKE_TIMEOUT_S" -gt 0 ]] || fail 'SMOKE_TIMEOUT_S must be positive'

umask 077
TMP="$(mktemp -d)" || fail 'cannot create temporary directory'
DOCUMENT_ID=""
DELETED=0
HTTP_CODE=""

request() { # method, path, optional JSON body; response goes to TMP/body
	local method="$1" path="$2"
	local args=(-sS --connect-timeout "$SMOKE_REQ_TIMEOUT_S" --max-time "$SMOKE_REQ_TIMEOUT_S"
		-X "$method" -o "$TMP/body" -w '%{http_code}')
	if [[ $# -gt 2 ]]; then args+=(-H 'Content-Type: application/json' --data-binary "$3"); fi
	if [[ -f "$TMP/auth" ]]; then args+=(-H "@$TMP/auth"); fi
	HTTP_CODE="$(curl "${args[@]}" "$SMOKE_URL$path" 2>/dev/null)"
}

cleanup() {
	local code=$?
	trap - EXIT
	if [[ -n "$DOCUMENT_ID" && "$DELETED" == 0 ]]; then
		if ! request DELETE "/v1/documents/$DOCUMENT_ID" || [[ "$HTTP_CODE" != 2?? ]]; then
			printf 'SMOKE FAIL: cleanup delete failed for document %s\n' "$DOCUMENT_ID"
			code=1
		fi
	fi
	rm -rf "$TMP"
	exit "$code"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

if [[ -n "${SMOKE_KEY_FILE:-}" && -s "$SMOKE_KEY_FILE" ]]; then
	key="$(cat "$SMOKE_KEY_FILE")"
	if [[ -n "$key" ]]; then
		printf 'Authorization: Bearer %s\n' "$key" >"$TMP/auth"
		chmod 600 "$TMP/auth"
	fi
	unset key
fi

# The API returns small, flat fields; tolerate whitespace around the colon.
json_string() { sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$TMP/body"; }

deadline=$((SECONDS + SMOKE_TIMEOUT_S))
until request GET /readyz && [[ "$HTTP_CODE" == 200 ]]; do
	[[ "$SECONDS" -lt "$deadline" ]] || fail 'service did not become ready'
	sleep "$SMOKE_POLL_S"
done
ok 'service ready'

token="gkgsmoke$(date +%s)$RANDOM"
payload="{\"text\":\"gkg smoke check. The marker $token identifies this temporary document; the check deletes it.\",\"source\":\"gkg-smoke-$token\"}"
request POST /v1/ingest "$payload" || fail 'ingest request failed'
[[ "$HTTP_CODE" == 2?? ]] || fail 'ingest was rejected'
DOCUMENT_ID="$(json_string document_id)"
[[ -n "$DOCUMENT_ID" ]] || fail 'ingest returned no document id'
case "$DOCUMENT_ID" in *[!a-zA-Z0-9_-]*) DOCUMENT_ID=""; fail 'ingest returned an invalid document id' ;; esac
ok "ingested document $DOCUMENT_ID"

deadline=$((SECONDS + SMOKE_TIMEOUT_S))
while :; do
	request GET "/v1/documents/$DOCUMENT_ID/status" || fail "status request failed for document $DOCUMENT_ID"
	[[ "$HTTP_CODE" == 200 ]] || fail "status unavailable for document $DOCUMENT_ID"
	status="$(json_string status)"
	case "$status" in
		done) break ;;
		failed) fail "document $DOCUMENT_ID failed" ;;
	esac
	[[ "$SECONDS" -lt "$deadline" ]] || fail "document $DOCUMENT_ID timed out"
	sleep "$SMOKE_POLL_S"
done
ok "document $DOCUMENT_ID done"

request POST /v1/search "{\"query\":\"$token\",\"kind\":\"chunk\",\"limit\":5}" || fail "search request failed for document $DOCUMENT_ID"
[[ "$HTTP_CODE" == 200 ]] || fail "search failed for document $DOCUMENT_ID"
grep -Eq "\"document_id\"[[:space:]]*:[[:space:]]*\"$DOCUMENT_ID\"" "$TMP/body" || fail "search missed document $DOCUMENT_ID"
ok "search found document $DOCUMENT_ID"

request DELETE "/v1/documents/$DOCUMENT_ID" || fail "delete request failed for document $DOCUMENT_ID"
[[ "$HTTP_CODE" == 2?? ]] || fail "delete failed for document $DOCUMENT_ID"
DELETED=1
request GET "/v1/documents/$DOCUMENT_ID/status" || fail "delete verification failed for document $DOCUMENT_ID"
[[ "$HTTP_CODE" == 404 ]] || fail "delete did not remove document $DOCUMENT_ID"
ok "deleted document $DOCUMENT_ID"
printf 'SMOKE PASS\n'
