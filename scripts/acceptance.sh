#!/usr/bin/env bash
# gkg acceptance run — design §6 checks, in two modes:
#
#   local   (ACCEPTANCE_MODE=local or GKG_MEMGRAPH_URI set): builds the
#           binary and runs against an existing Memgraph (mock extractor
#           unless overridden). This is the CI mode.
#   compose (default otherwise): drives an isolated light stack with .env;
#           ACCEPTANCE_FULL=1 includes sidecar and docling. Dedicated ports
#           default to 17687/18080/18090/15001 (ACCEPTANCE_*_PORT overrides).
#
# Checks:
#   1. ingest a Markdown file and a PDF, both queryable afterwards
#   2. MCP smoke over stdio: tools/list = 8 tools, search answers
#   3. `gkg eval` on the golden sample with the mock provider
#   4. unchanged re-ingest is a no-op; changed content supersedes cleanly
#   4b. REST ingest by domain ID and name, with status and conflict checks
#   5. products write path over REST, including document refs and cleanup
#      (skipped when a non-acceptance product release is active: use a scratch graph)
#   6. local model offline (EN+IT) — manual SKIP
#   7. judge verification on real facts — manual SKIP (needs a key)
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE=(docker compose -p gkg-acceptance --env-file .env -f deploy/docker-compose.yml)
if [[ "${ACCEPTANCE_FULL:-0}" != "1" ]]; then COMPOSE+=(-f deploy/compose.light.yml); fi
TMP="$(mktemp -d)"
SERVER_PID=""
TOKEN="acceptance-$(date +%s)-$RANDOM"
BASE_URL=""

step() { printf '\n== %s\n' "$*"; }
ok() { printf 'OK: %s\n' "$*"; }
skip() { printf 'SKIP (manual): %s\n' "$*"; }
fail() {
	printf 'FAIL: %s\n' "$*"
	exit 1
}

cleanup() {
	if [[ -n "$SERVER_PID" ]]; then kill "$SERVER_PID" 2>/dev/null || true; fi
	# The acceptance stack runs under its own project (gkg-acceptance): a
	# developer's long-lived gkg stack is never touched. Set
	# ACCEPTANCE_KEEP=1 to keep the acceptance volume around.
	if [[ "${ACCEPTANCE_KEEP:-0}" != "1" && "${MODE:-}" == "compose" ]]; then
		"${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
	fi
	rm -rf "$TMP"
}
trap cleanup EXIT

# Compose mode only: read a KEY=value from its documented env file.
# Local/CI mode uses only the shell environment and never reads .env.
env_value() { { sed -n "s/^$1=//p" .env 2>/dev/null || true; } | head -1; }

# Optional bearer auth travels through a private header file, never argv.
req() {
	if [[ -s "$TMP/auth" ]]; then
		curl -q "$@" -H @"$TMP/auth"
	else
		curl -q "$@"
	fi
}

expect_http() { # status, method, path, then curl options; sets HTTP_BODY
	local want="$1" method="$2" path="$3" code
	shift 3
	: >"$TMP/response.json"
	code="$(req -sS -o "$TMP/response.json" -w '%{http_code}' -X "$method" "$BASE_URL$path" "$@")" ||
		fail "$method $path: curl failed: $(<"$TMP/response.json")"
	HTTP_BODY="$(<"$TMP/response.json")"
	[[ "$code" == "$want" ]] || fail "$method $path: HTTP $code, want $want: $HTTP_BODY"
}

wait_http() { # url, attempts (health probes are auth-exempt)
	local url="$1" tries="${2:-60}"
	for _ in $(seq 1 "$tries"); do
		if curl -q -fsS -m 2 -o /dev/null "$url"; then return 0; fi
		sleep 1
	done
	return 1
}

wait_status() { # doc id, want
	local id="$1" want="$2" status=""
	for _ in $(seq 1 120); do
		expect_http 200 GET "/v1/documents/$id/status"
		status="$(sed -n 's/.*"status":"\([^"]*\)".*/\1/p' <<< "$HTTP_BODY")"
		case "$status" in
		"$want") return 0 ;;
		failed) fail "document $id failed: $HTTP_BODY" ;;
		esac
		sleep 0.5
	done
	fail "document $id did not reach $want: $HTTP_BODY"
}

ingest_file() { # path
	req -fsS -X POST "$BASE_URL/v1/ingest" -F "file=@$1" | sed -n 's/.*"document_id":"\([^"]*\)".*/\1/p'
}

# ---------------------------------------------------------------------------
step "mode and prerequisites"
if [[ "${ACCEPTANCE_MODE:-auto}" == "compose" || ("${ACCEPTANCE_MODE:-auto}" == "auto" && -z "${GKG_MEMGRAPH_URI:-}") ]]; then
	MODE=compose
else
	MODE=local
fi
echo "mode: $MODE"

if [[ "$MODE" == "compose" ]]; then
	# Isolated acceptance ports must beat any values in .env or the shell.
	export MEMGRAPH_PORT="${ACCEPTANCE_MEMGRAPH_PORT:-17687}"
	export GKG_PORT="${ACCEPTANCE_GKG_PORT:-18080}"
	export SIDECAR_PORT="${ACCEPTANCE_SIDECAR_PORT:-18090}"
	export DOCLING_PORT="${ACCEPTANCE_DOCLING_PORT:-15001}"
	command -v docker >/dev/null || fail "docker is required for the compose mode"
	[[ -f .env ]] || fail "compose mode needs .env (copy .env.example and set the keys)"
	GKG_API_KEY="${GKG_API_KEY:-$(env_value GKG_API_KEY)}"
	export GKG_API_KEY="${GKG_API_KEY:-$(openssl rand -hex 32)}"
	"${COMPOSE[@]}" config -q || fail "docker compose config"
	ok "compose config"
	AUTH_KEY="$GKG_API_KEY"
else
	AUTH_KEY="${GKG_API_KEY:-}"
fi
if [[ -n "$AUTH_KEY" ]]; then
	(umask 077; printf 'Authorization: Bearer %s\n' "$AUTH_KEY" >"$TMP/auth")
	chmod 600 "$TMP/auth"
fi
unset AUTH_KEY

# ---------------------------------------------------------------------------
step "build"
go build -o "$TMP/gkg" ./cmd/gkg
go build -o "$TMP/mcpsmoke" ./scripts/mcpsmoke
ok "gkg and mcpsmoke built"

# ---------------------------------------------------------------------------
step "start the service"
if [[ "$MODE" == "compose" ]]; then
	"${COMPOSE[@]}" up -d --build --wait gkg
	BASE_URL="http://127.0.0.1:${GKG_PORT}"
	GKG_CMD=("${COMPOSE[@]}" exec -T gkg gkg)
	PDF_INPUT_URL="${ACCEPTANCE_PDF_URL:-https://www.w3.org/WAI/ER/tests/xhtml/testfiles/resources/pdf/dummy.pdf}"
	# Host-side commands (eval, MCP over the container) use the same creds.
	export GKG_MEMGRAPH_URI="bolt://localhost:${MEMGRAPH_PORT}"
	GKG_MEMGRAPH_USER="$(env_value MEMGRAPH_USER)"
	GKG_MEMGRAPH_PASSWORD="$(env_value MEMGRAPH_PASSWORD)"
	export GKG_MEMGRAPH_USER GKG_MEMGRAPH_PASSWORD
else
	[[ -n "${GKG_MEMGRAPH_URI:-}" ]] || fail "local mode needs GKG_MEMGRAPH_URI"
	export GKG_EXTRACTOR_MODE="${GKG_EXTRACTOR_MODE:-mock}"
	# CI and local mode do not start docling.
	export GKG_CONVERTER_MODE="${GKG_CONVERTER_MODE:-off}"
	ACCEPTANCE_PORT="${ACCEPTANCE_PORT:-18099}"
	export GKG_SERVER_ADDR="127.0.0.1:${ACCEPTANCE_PORT}"
	BASE_URL="http://127.0.0.1:${ACCEPTANCE_PORT}"
	"$TMP/gkg" serve >"$TMP/serve.log" 2>&1 &
	SERVER_PID=$!
	GKG_CMD=("$TMP/gkg")
	PDF_INPUT_URL=""
fi
wait_http "$BASE_URL/healthz" || fail "service did not become healthy (see $TMP/serve.log)"
wait_http "$BASE_URL/readyz" || fail "service did not become ready"
ok "healthz + readyz"

# ---------------------------------------------------------------------------
step "embedded web UI (optional)"
if [[ "${ACCEPTANCE_UI:-0}" == "1" ]]; then
	UI_BODY="$(req -fsS "$BASE_URL/ui/")"
	[[ "$UI_BODY" == *'id="graph"'* ]] || fail "GET /ui/ did not serve the embedded UI"
	ok "embedded UI served at /ui/"
else
	skip "embedded UI at /ui/ (set ACCEPTANCE_UI=1 to check)"
fi

# ---------------------------------------------------------------------------
step "1. ingest a Markdown file and a PDF, both queryable"
cat >"$TMP/acceptance.md" <<EOF
# Acceptance note

The gkg acceptance marker is $TOKEN and it proves end-to-end ingestion.
EOF
MD_DOC="$(ingest_file "$TMP/acceptance.md")"
[[ -n "$MD_DOC" ]] || fail "markdown ingest did not return a document id"
wait_status "$MD_DOC" "done"
ok "markdown ingested ($MD_DOC)"

if [[ -n "$PDF_INPUT_URL" ]]; then
	PDF_DOC="$(req -fsS -X POST "$BASE_URL/v1/ingest" -H 'Content-Type: application/json' \
		--data-binary "{\"url\":\"$PDF_INPUT_URL\"}" | sed -n 's/.*"document_id":"\([^"]*\)".*/\1/p')"
else
	PDF_DOC="$(ingest_file internal/ingest/testdata/sample.pdf)"
fi
[[ -n "$PDF_DOC" ]] || fail "pdf ingest did not return a document id"
wait_status "$PDF_DOC" "done"
ok "pdf ingested ($PDF_DOC)"

SEARCH_BODY="$(req -fsS -X POST "$BASE_URL/v1/search" -H 'Content-Type: application/json' \
	--data-binary "{\"query\":\"$TOKEN\",\"kind\":\"auto\",\"limit\":10}")"
HITS="$(printf '%s' "$SEARCH_BODY" | grep -o '"kind"' | wc -l | tr -d ' ' || true)"
[[ "$HITS" -ge 1 ]] || fail "search for the acceptance marker returned no hits: $SEARCH_BODY"
ok "search finds the ingested content ($HITS hits)"
req -fsS "$BASE_URL/v1/domains" >"$TMP/domains.json" || fail "domains list"
ok "domains list responds"

# ---------------------------------------------------------------------------
step "2. MCP smoke over stdio"
"$TMP/mcpsmoke" -query "$TOKEN" -- "${GKG_CMD[@]}" mcp
ok "mcp tools/list + search"

# ---------------------------------------------------------------------------
step "3. evals on the golden sample (mock provider)"
"$TMP/gkg" eval --provider mock --golden eval/golden/sample.jsonl >"$TMP/eval.out" 2>&1 ||
	fail "gkg eval (mock): $(tail -5 "$TMP/eval.out")"
ok "eval run: $(tail -1 "$TMP/eval.out")"

# ---------------------------------------------------------------------------
step "4. re-ingest no-op and supersede"
NOOP="$(req -fsS -X POST "$BASE_URL/v1/ingest" -F "file=@$TMP/acceptance.md" |
	grep -o '"noop":true' || true)"
[[ "$NOOP" == '"noop":true' ]] || fail "unchanged re-ingest was not a no-op"
ok "unchanged re-ingest is a no-op"

printf '\nAn added line with fresh marker %s-2.\n' "$TOKEN" >>"$TMP/acceptance.md"
NEW_DOC="$(ingest_file "$TMP/acceptance.md")"
[[ "$NEW_DOC" != "$MD_DOC" ]] || fail "changed content kept the old document id"
wait_status "$NEW_DOC" "done"
OLD_STATUS="$(req -fsS "$BASE_URL/v1/documents/$MD_DOC/status" | sed -n 's/.*"status":"\([^"]*\)".*/\1/p')"
[[ "$OLD_STATUS" == "superseded" ]] || fail "old version status = $OLD_STATUS, want superseded"
ok "changed content superseded the old version"

# ---------------------------------------------------------------------------
step "4b. choose an existing domain at ingest"
expect_http 200 GET "/v1/documents/$NEW_DOC"
ORIGINAL_DOMAIN="$(sed -n 's/.*"domain":"\([^"]*\)".*/\1/p' <<< "$HTTP_BODY")"
[[ -n "$ORIGINAL_DOMAIN" ]] || fail "Auto did not assign an original domain"
CHOSEN_ID="chosen-$TOKEN"
CHOSEN_NAME="Acceptance / $TOKEN"
expect_http 202 POST /v1/ingest -H 'Content-Type: application/json' \
	--data-binary "{\"source\":\"$TOKEN-seed\",\"text\":\"Zymurgy $TOKEN fixture for domain assignment.\"}"
SEED_DOC="$(sed -n 's/.*"document_id":"\([^"]*\)".*/\1/p' <<< "$HTTP_BODY")"
[[ -n "$SEED_DOC" ]] || fail "domain fixture ingest did not return a document id"
wait_status "$SEED_DOC" "done"
"${GKG_CMD[@]}" domain move "$SEED_DOC" "$CHOSEN_ID" || fail "create acceptance domain"
wait_status "$SEED_DOC" "done"
"${GKG_CMD[@]}" domain rename "$CHOSEN_ID" "$CHOSEN_NAME" || fail "name acceptance domain"

check_chosen_document() {
	local id
	id="$(sed -n 's/.*"document_id":"\([^"]*\)".*/\1/p' <<< "$HTTP_BODY")"
	[[ -n "$id" ]] || fail "chosen-domain ingest did not return a document id"
	wait_status "$id" "done"
	[[ "$HTTP_BODY" == *"\"domain\":\"$CHOSEN_ID\""* ]] || fail "status is outside the chosen domain: $HTTP_BODY"
	expect_http 200 GET "/v1/documents/$id"
	[[ "$HTTP_BODY" == *"\"domain\":\"$CHOSEN_ID\""* ]] || fail "document is outside the chosen domain: $HTTP_BODY"
}

for selector in id name; do
	DOMAIN_VALUE="$CHOSEN_ID"
	if [[ "$selector" == "name" ]]; then DOMAIN_VALUE="$CHOSEN_NAME"; fi
	expect_http 202 POST /v1/ingest -H 'Content-Type: application/json' \
		--data-binary "{\"source\":\"$TOKEN-json-$selector\",\"text\":\"Chosen domain from JSON.\",\"domain\":\"$DOMAIN_VALUE\"}"
	check_chosen_document
	expect_http 202 POST /v1/ingest -F "file=@$TMP/acceptance.md" \
		-F "source=$TOKEN-file-$selector" -F "domain=$DOMAIN_VALUE"
	check_chosen_document
	ok "JSON and multipart selected domain by $selector"
done

expect_http 400 POST /v1/ingest -H 'Content-Type: application/json' \
	--data-binary "{\"text\":\"Unknown domain.\",\"domain\":\"missing-$TOKEN\"}"
[[ "$HTTP_BODY" == *'"error":"unknown_domain"'* ]] || fail "expected unknown_domain: $HTTP_BODY"
expect_http 409 POST /v1/ingest -H 'Content-Type: application/json' \
	--data-binary "{\"source\":\"$TOKEN-json-id\",\"text\":\"Chosen domain from JSON.\",\"domain\":\"$ORIGINAL_DOMAIN\"}"
[[ "$HTTP_BODY" == *'"error":"domain_conflict"'* ]] || fail "expected domain_conflict: $HTTP_BODY"
ok "unknown domain and conflicting identical content rejected"

# ---------------------------------------------------------------------------
step "5. products write path over REST"
# Activating the acceptance release prunes every other store's feature evidence
# (USAGE "Product registries and releases"), so the step runs only on a graph
# with no active release or one a previous acceptance run created.
products_write_path() {
	PRODUCT_FIXTURES=scripts/testdata/acceptance-products
	# Unbinding preserves a store's sequence floor, so each run needs a fresh store.
	PRODUCT_STORE="$TOKEN.test"
	PRODUCT_A="$PRODUCT_STORE:a"
	PRODUCT_B="$PRODUCT_STORE:b"

	product_cli() {
		"$TMP/gkg" "$@" >"$TMP/product-cli.log" 2>&1 || fail "gkg $*: $(<"$TMP/product-cli.log")"
	}

	product_pin_sha() {
		local digest
		digest="$(openssl dgst -sha256 <"$1")" || return 1
		printf '%s\n' "${digest##* }"
	}

	put_product() { # HTTP status, seq, complete record
		expect_http "$1" PUT /v1/products -H 'Content-Type: application/json' \
			--data-binary "{\"seq\":$2,\"record\":$3}"
	}

	check_product_event() { # result status, product id, seen_seq
		[[ "$HTTP_BODY" == *"\"status\":\"$1\""* && "$HTTP_BODY" == *"\"product_id\":\"$2\""* &&
			"$HTTP_BODY" == *"\"seen_seq\":$3"[,\}]* ]] || fail "product $2 seq $3: expected $1: $HTTP_BODY"
		ok "product $2 seq $3 returned $1"
	}

	expect_http 200 GET "/v1/documents/$NEW_DOC"
	PRODUCT_DOMAIN="$(sed -n 's/.*"domain":"\([^"]*\)".*/\1/p' <<< "$HTTP_BODY")"
	[[ -n "$PRODUCT_DOMAIN" ]] || fail "acceptance note has no domain: $HTTP_BODY"

	sed "s/acceptance\.test/$PRODUCT_STORE/g" "$PRODUCT_FIXTURES/R.json" >"$TMP/R.json"
	sed "s/acceptance\.test/$PRODUCT_STORE/g" "$PRODUCT_FIXTURES/records.jsonl" >"$TMP/records.jsonl"
	product_cli features import "$TMP/R.json"
	REGISTRY_SHA="$(sed -n 's/^.* rev=[0-9a-f]* digest=\([0-9a-f]*\)$/\1/p' "$TMP/product-cli.log")"
	[[ ${#REGISTRY_SHA} == 64 ]] || fail "feature registry import did not return a digest: $(<"$TMP/product-cli.log")"

	printf '{"registry_sha256":"%s","keys":[{"store":"%s","feature":"acceptance_code","source_key":"code"}]}\n' \
		"$REGISTRY_SHA" "$PRODUCT_STORE" >"$TMP/candidates.json"
	CANDIDATES_SHA="$(product_pin_sha "$TMP/candidates.json")" || fail "sha256 of $TMP/candidates.json"
	printf '{"registry_sha256":"%s","release_candidates_sha256":"%s"}\n' \
		"$REGISTRY_SHA" "$CANDIDATES_SHA" >"$TMP/score.json"
	printf '{"registry_sha256":"%s","release_candidates_sha256":"%s","release_allowed":true,"owner_decision_required":false}\n' \
		"$REGISTRY_SHA" "$CANDIDATES_SHA" >"$TMP/report.json"
	KIT_SHA="$(product_pin_sha "$PRODUCT_FIXTURES/rf_kit.py")" || fail "sha256 of $PRODUCT_FIXTURES/rf_kit.py"
	E2_SHA="$(product_pin_sha "$PRODUCT_FIXTURES/e2.jsonl")" || fail "sha256 of $PRODUCT_FIXTURES/e2.jsonl"
	printf '{"sha256":{"R.json":"%s","rf_kit.py":"%s"},"e2_sha256":"%s"}\n' \
		"$REGISTRY_SHA" "$KIT_SHA" "$E2_SHA" >"$TMP/freeze.json"
	product_cli products emit -registry "$TMP/R.json" -release "$TMP/candidates.json" \
		-out "$TMP/record-obs.jsonl" "$TMP/records.jsonl"
	product_cli products release create -id "$TOKEN" -registry "${REGISTRY_SHA:0:16}" -candidates "$TMP/candidates.json" \
		-store-domain "$PRODUCT_STORE=$PRODUCT_DOMAIN" -approved-by acceptance \
		-pin "R.json=$TMP/R.json" -pin "rf_kit.py=$PRODUCT_FIXTURES/rf_kit.py" -pin "E2=$PRODUCT_FIXTURES/e2.jsonl" \
		-pin "score.json=$TMP/score.json" -pin "report.json=$TMP/report.json" -pin "freeze.json=$TMP/freeze.json" \
		-pin "record-obs.jsonl=$TMP/record-obs.jsonl"
	product_cli products release activate "$TOKEN" -materialize "$PRODUCT_STORE=1:$TMP/records.jsonl"
	ok "product release activated and snapshot imported for $PRODUCT_STORE"

	expect_http 200 GET /v1/products/stores
	STORE_ROW="$(grep -o '{[^{}]*}' <<< "$HTTP_BODY" | grep -F "\"store\":\"$PRODUCT_STORE\"" || true)"
	[[ "$STORE_ROW" == *'"seen_seq":1,'* && "$STORE_ROW" == *'"product_count":2,'* ]] ||
		fail "store $PRODUCT_STORE did not report seen_seq 1 and two products: $HTTP_BODY"
	ok "store $PRODUCT_STORE has seen_seq 1 and two products"

	ORIGINAL_A="$(sed -n '1p' "$TMP/records.jsonl")"
	UPDATED_A="${ORIGINAL_A/Widget A/Widget A updated}"
	put_product 200 2 "$UPDATED_A"
	check_product_event updated "$PRODUCT_A" 2
	PRODUCT_A_ID="$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<< "$HTTP_BODY")"
	[[ -n "$PRODUCT_A_ID" ]] || fail "update did not return a graph product id: $HTTP_BODY"
	put_product 200 2 "$UPDATED_A"
	check_product_event duplicate "$PRODUCT_A" 2
	put_product 409 1 "$ORIGINAL_A"
	[[ "$HTTP_BODY" == *'"error":"stale"'* && "$HTTP_BODY" == *'"seen_seq":2'[,\}]* ]] ||
		fail "PUT seq 1 did not return stale with seen_seq 2: $HTTP_BODY"
	ok "stale PUT returned seen_seq 2"

	expect_http 202 POST /v1/ingest -F "file=@$PRODUCT_FIXTURES/page.md" \
		-F "source=$TOKEN-products.md" -F "products=$PRODUCT_A"
	PRODUCT_DOC="$(sed -n 's/.*"document_id":"\([^"]*\)".*/\1/p' <<< "$HTTP_BODY")"
	[[ -n "$PRODUCT_DOC" ]] || fail "product page ingest did not return a document id: $HTTP_BODY"
	wait_status "$PRODUCT_DOC" "done"
	[[ "$HTTP_BODY" == *"\"products\":[\"$PRODUCT_A\"]"* ]] || fail "product page status did not contain its ref: $HTTP_BODY"
	ok "product page status shows the ref to $PRODUCT_A"
	expect_http 200 GET "/v1/documents/$PRODUCT_DOC"
	[[ "$HTTP_BODY" == *"\"domain\":\"$PRODUCT_DOMAIN\""* ]] ||
		fail "product page is outside bound domain $PRODUCT_DOMAIN: $HTTP_BODY"
	ok "product page is in bound domain $PRODUCT_DOMAIN"
	expect_http 200 GET "/v1/products/$PRODUCT_A_ID"
	[[ "$HTTP_BODY" == *'"title":"Widget A updated"'* && "$HTTP_BODY" == *"\"pages\":[{\"id\":\"$PRODUCT_DOC\""* ]] ||
		fail "product A did not show the updated record and referenced page: $HTTP_BODY"
	ok "GET product A lists the referenced page"

	UPDATED_AGAIN_A="${ORIGINAL_A/Widget A/Widget A revised}"
	put_product 200 3 "$UPDATED_AGAIN_A"
	check_product_event updated "$PRODUCT_A" 3
	expect_http 200 GET "/v1/products/$PRODUCT_A_ID"
	[[ "$HTTP_BODY" == *'"title":"Widget A revised"'* && "$HTTP_BODY" == *"\"pages\":[{\"id\":\"$PRODUCT_DOC\""* ]] ||
		fail "product A did not retain its referenced page after PUT seq 3: $HTTP_BODY"
	ok "the referenced page survived PUT seq 3"

	expect_http 200 GET "/v1/products?store=$PRODUCT_STORE&product_id=$PRODUCT_STORE%3Ab"
	PRODUCT_B_ID="$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<< "${HTTP_BODY%%\"features\":*}")"
	[[ -n "$PRODUCT_B_ID" ]] || fail "lookup did not return a graph id for $PRODUCT_B: $HTTP_BODY"
	expect_http 200 DELETE "/v1/products?product_id=$PRODUCT_STORE%3Ab&seq=4"
	check_product_event deleted "$PRODUCT_B" 4
	expect_http 200 DELETE "/v1/products?product_id=$PRODUCT_STORE%3Ab&seq=5"
	check_product_event absent "$PRODUCT_B" 5
	expect_http 404 GET "/v1/products/$PRODUCT_B_ID"
	ok "GET product B returned 404 after deletion"

	expect_http 200 DELETE "/v1/documents/$PRODUCT_DOC"
	[[ "$HTTP_BODY" == '{"deleted":1}' ]] || fail "failed to delete product page: $HTTP_BODY"
	ok "product page deleted"
	product_cli products delete-store "$PRODUCT_STORE"
	product_cli products unbind "$PRODUCT_STORE"
	expect_http 200 GET /v1/products/stores
	[[ "$HTTP_BODY" == *'"stores":['* && "$HTTP_BODY" != *"\"store\":\"$PRODUCT_STORE\""* ]] ||
		fail "store $PRODUCT_STORE still listed after wipe and unbind: $HTTP_BODY"
	ok "test store no longer listed"
}

expect_http 200 GET /v1/products/stores
ACTIVE_RELEASE="$(sed -n 's/.*"active_release_id":"\([^"]*\)".*/\1/p' <<< "$HTTP_BODY")"
if [[ -n "$ACTIVE_RELEASE" && "$ACTIVE_RELEASE" != acceptance-* ]]; then
	skip "products write path: active release $ACTIVE_RELEASE is not an acceptance release; activating one would prune its stores (use a scratch database)"
else
	products_write_path
fi

# ---------------------------------------------------------------------------
step "manual checks (out of this script's scope)"
skip "local model fully offline (EN + IT): see docs/quickstart.md"
skip "judge verification removes seeded wrong facts (needs a real key): run gkg eval with the judge enabled"

printf '\nACCEPTANCE PASS (%s mode)\n' "$MODE"
