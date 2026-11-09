#!/usr/bin/env bash
# gkg acceptance run — design §6 checks, in two modes:
#
#   local   (ACCEPTANCE_MODE=local or GKG_MEMGRAPH_URI set): builds the
#           binary and runs against an existing Memgraph (mock extractor
#           unless overridden). This is the CI mode.
#   compose (default otherwise): drives the full docker compose stack from
#           deploy/docker-compose.yml with the .env configuration.
#
# Checks:
#   1. ingest a Markdown file and a PDF, both queryable afterwards
#   2. MCP smoke over stdio: tools/list = 7 tools, search answers
#   3. `gkg eval` on the golden sample with the mock provider
#   4. unchanged re-ingest is a no-op; changed content supersedes cleanly
#   5. local model profile offline (EN+IT) — manual SKIP
#   6. judge verification on real facts — manual SKIP (needs a key)
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE=(docker compose -p gkg-acceptance --env-file .env -f deploy/docker-compose.yml)
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

# env_value reads a KEY=value from .env (the documented override location);
# a missing .env is fine (local/CI mode must not need one).
env_value() { { sed -n "s/^$1=//p" .env 2>/dev/null || true; } | head -1; }

# Optional bearer auth: the optional GKG_API_KEY (shell env first, then
# .env) would 401 every non-exempt REST call, so every request goes through
# req() which adds the header when the key is set.
AUTH_KEY="${GKG_API_KEY:-$(env_value GKG_API_KEY)}"
req() {
	if [[ -n "$AUTH_KEY" ]]; then
		curl "$@" -H "Authorization: Bearer $AUTH_KEY"
	else
		curl "$@"
	fi
}

wait_http() { # url, attempts (health probes are auth-exempt)
	local url="$1" tries="${2:-60}"
	for _ in $(seq 1 "$tries"); do
		if curl -fsS -m 2 -o /dev/null "$url"; then return 0; fi
		sleep 1
	done
	return 1
}

wait_status() { # doc id, want
	local id="$1" want="$2" status=""
	for _ in $(seq 1 120); do
		status="$(req -fsS "$BASE_URL/v1/documents/$id/status" | sed -n 's/.*"status":"\([^"]*\)".*/\1/p')"
		case "$status" in
		"$want") return 0 ;;
		failed) fail "document $id failed: $(req -s "$BASE_URL/v1/documents/$id/status")" ;;
		esac
		sleep 0.5
	done
	fail "document $id did not reach $want (last: $status)"
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
	command -v docker >/dev/null || fail "docker is required for the compose mode"
	[[ -f .env ]] || fail "compose mode needs .env (copy .env.example and set the keys)"
	"${COMPOSE[@]}" config -q || fail "docker compose config"
	ok "compose config"
fi

# ---------------------------------------------------------------------------
step "build"
go build -o "$TMP/gkg" ./cmd/gkg
go build -o "$TMP/mcpsmoke" ./scripts/mcpsmoke
ok "gkg and mcpsmoke built"

# ---------------------------------------------------------------------------
step "start the service"
if [[ "$MODE" == "compose" ]]; then
	"${COMPOSE[@]}" up -d --wait
	# Host ports: shell env wins, then .env (the documented override), then defaults.
	GKG_PORT="${GKG_PORT:-$(env_value GKG_PORT)}"
	GKG_PORT="${GKG_PORT:-8080}"
	MEMGRAPH_PORT="${MEMGRAPH_PORT:-$(env_value MEMGRAPH_PORT)}"
	MEMGRAPH_PORT="${MEMGRAPH_PORT:-7687}"
	BASE_URL="http://127.0.0.1:${GKG_PORT}"
	MCP_CMD=(docker compose -p gkg-acceptance --env-file .env -f deploy/docker-compose.yml exec -T gkg gkg mcp)
	PDF_INPUT_URL="${ACCEPTANCE_PDF_URL:-https://www.w3.org/WAI/ER/tests/xhtml/testfiles/resources/pdf/dummy.pdf}"
	# Host-side commands (eval, MCP over the container) use the same creds.
	export GKG_MEMGRAPH_URI="bolt://localhost:${MEMGRAPH_PORT}"
	export GKG_MEMGRAPH_USER="$(env_value MEMGRAPH_USER)"
	export GKG_MEMGRAPH_PASSWORD="$(env_value MEMGRAPH_PASSWORD)"
else
	[[ -n "${GKG_MEMGRAPH_URI:-}" ]] || fail "local mode needs GKG_MEMGRAPH_URI"
	export GKG_EXTRACTOR_MODE="${GKG_EXTRACTOR_MODE:-mock}"
	ACCEPTANCE_PORT="${ACCEPTANCE_PORT:-18099}"
	export GKG_SERVER_ADDR="127.0.0.1:${ACCEPTANCE_PORT}"
	BASE_URL="http://127.0.0.1:${ACCEPTANCE_PORT}"
	"$TMP/gkg" serve >"$TMP/serve.log" 2>&1 &
	SERVER_PID=$!
	MCP_CMD=("$TMP/gkg" mcp)
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
wait_status "$MD_DOC" done
ok "markdown ingested ($MD_DOC)"

if [[ -n "$PDF_INPUT_URL" ]]; then
	PDF_DOC="$(req -fsS -X POST "$BASE_URL/v1/ingest" -H 'Content-Type: application/json' \
		--data-binary "{\"url\":\"$PDF_INPUT_URL\"}" | sed -n 's/.*"document_id":"\([^"]*\)".*/\1/p')"
else
	PDF_DOC="$(ingest_file internal/ingest/testdata/sample.pdf)"
fi
[[ -n "$PDF_DOC" ]] || fail "pdf ingest did not return a document id"
wait_status "$PDF_DOC" done
ok "pdf ingested ($PDF_DOC)"

SEARCH_BODY="$(req -fsS -X POST "$BASE_URL/v1/search" -H 'Content-Type: application/json' \
	--data-binary "{\"query\":\"$TOKEN\",\"kind\":\"auto\",\"limit\":10}")"
HITS="$(printf '%s' "$SEARCH_BODY" | grep -o '"kind"' | wc -l | tr -d ' ' || true)"
[[ "$HITS" -ge 1 ]] || fail "search for the acceptance marker returned no hits: $SEARCH_BODY"
ok "search finds the ingested content ($HITS hits)"

# ---------------------------------------------------------------------------
step "2. MCP smoke over stdio"
"$TMP/mcpsmoke" -query "$TOKEN" -- "${MCP_CMD[@]}"
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
wait_status "$NEW_DOC" done
OLD_STATUS="$(req -fsS "$BASE_URL/v1/documents/$MD_DOC/status" | sed -n 's/.*"status":"\([^"]*\)".*/\1/p')"
[[ "$OLD_STATUS" == "superseded" ]] || fail "old version status = $OLD_STATUS, want superseded"
ok "changed content superseded the old version"

# ---------------------------------------------------------------------------
step "manual checks (out of this script's scope)"
skip "local model profile fully offline (EN + IT): see docs/quickstart.md (profile local)"
skip "judge verification removes seeded wrong facts (needs a real key): run gkg eval with the judge enabled"

printf '\nACCEPTANCE PASS (%s mode)\n' "$MODE"
