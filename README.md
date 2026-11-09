# go-knowledge-graph (`gkg`)

**Document → knowledge graph → agent queries.** Ingest text, files
(Markdown/txt/PDF) or URLs, extract entities and relations into
[Memgraph](https://memgraph.com) with full provenance, and query the graph
from agents over MCP + REST — optionally verifying facts with an LLM judge
and measuring extraction quality with the built-in eval harness.

- **Pipeline**: decode → sentence-aware chunks → near-duplicate detection
  (MinHash/LSH) → extraction (OpenAI-compatible LLM or a local GLiNEREL
  sidecar) → confidence floors → provenance merge. Unchanged re-ingest is a
  no-op; changed content supersedes the old version with an audit trail.
- **Agent surface**: MCP over stdio and streamable HTTP, mirrored by REST:
  ingest, status, search, entity views with evidence, traversal, document
  listing, and guarded read-only Cypher (write clauses rejected, row cap +
  timeout enforced).
- **Quality**: optional online judge (`off | low-confidence | sample | all`)
  removes refuted facts with a `:RejectedFact` audit; `gkg eval` prints
  lenient F1, judge-scored precision and reference coverage, while the
  `--out` report carries the full strict/lenient P/R/F1 table on a golden
  set.
- **Optional embeddings**: entity vectors fuse into search when an
  OpenAI-compatible `/embeddings` endpoint is configured; without one,
  search is text-only.

Everything runs from one static Go binary (`gkg`) plus a Memgraph instance;
the sidecar profile adds a local, fully-offline extraction option.

## Quickstart

```sh
cp .env.example .env    # set MEMGRAPH_PASSWORD and your extractor key
docker compose --env-file .env -f deploy/docker-compose.yml up -d --wait
curl -s -F 'file=@notes.md' localhost:8080/v1/ingest
```

Then connect an agent ([docs/agent-setup.md](docs/agent-setup.md)),
evaluate ([docs/quickstart.md#7-evaluation](docs/quickstart.md)), or run
the acceptance script (`scripts/acceptance.sh`). The full five-minute tour,
configuration reference and troubleshooting live in
[docs/quickstart.md](docs/quickstart.md).

## Architecture

| Component          | What it does                                                                       |
| ------------------ | ---------------------------------------------------------------------------------- |
| `cmd/gkg`          | CLI: `serve` (REST + /mcp + workers), `ingest`, `mcp` (stdio), `eval`, `migrate`   |
| `internal/api`     | REST `/v1/*`, health/metrics, bearer auth, body caps, read-only Cypher guard       |
| `internal/mcp`     | the seven agent tools over stdio and streamable HTTP                               |
| `internal/ingest`  | pipeline, worker pool, recovery, dedup, URL/PDF/HTML inputs                        |
| `internal/graph`   | Memgraph store: documents/chunks/entities/facts, supersede, GC, text/vector search |
| `internal/search`  | entity/chunk search with rank fusion of text and optional embedding legs           |
| `internal/extract` | extractor contract + OpenAI-compatible and GLiNEREL-sidecar providers              |
| `internal/judge`   | online fact verification (OpenAI-compatible)                                       |
| `internal/eval`    | golden-set harness, strict/lenient metrics, threshold sweep                        |
| `internal/obs`     | Prometheus metrics, JSON logs, request middleware                                  |
| `sidecar/`         | FastAPI GLiNEREL service (weights baked into the image, runtime offline)           |

Read paths are documented in [docs/openapi.yaml](docs/openapi.yaml) and the
product's limits in [docs/known-limitations.md](docs/known-limitations.md).

## Configuration

`config.yaml` (values, no secrets) + `GKG_*` environment overrides
(secrets), all documented with defaults in [.env.example](.env.example).
Highlights: `GKG_EXTRACTOR_MODE=openai|sidecar|mock`,
`GKG_JUDGE_MODE=off|low-confidence|sample|all`,
`GKG_EMBEDDER_ENABLED`, `GKG_API_KEY` (optional bearer auth for HTTP),
`GKG_LIMITS_CYPHER_ROWS`.

## Development

```sh
go build ./... && go vet ./... && go test ./...
GKG_TEST_MEMGRAPH_URI=bolt://localhost:7688 go test -p 1 ./... -count=1
bash scripts/acceptance.sh
```

See [CONTRIBUTING.md](CONTRIBUTING.md) (DCO sign-off, checks, PR flow).

## License

| Component                                       | License        | Note                                                                                                                                                        |
| ----------------------------------------------- | -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| gkg code                                        | Apache-2.0     | [LICENSE](LICENSE)                                                                                                                                          |
| Memgraph                                        | BSL 1.1        | not bundled; you host it (free for most use, not for Memgraph-as-a-service)                                                                                 |
| GoGPUtils                                       | MIT            | chunk dedup (MinHash/LSH) and token normalization                                                                                                           |
| neo4j Go driver                                 | Apache-2.0     | Bolt connectivity                                                                                                                                           |
| go-shiori/go-readability                        | MIT            | HTML → text                                                                                                                                                 |
| dslipak/pdf                                     | BSD-3-Clause   | best-effort pure-Go PDF text (Go Authors license)                                                                                                           |
| GLiNEREL (`glinerel-fast`, default local model) | LFM            | **free for commercial use while annual revenue is under $10M USD**; above that a commercial license from the vendor is required                             |
| gliner2.5 (alternative model family)            | Apache-2.0     | fully free alternative; **not loadable by the pinned glinerel-fast v0.1 runtime — the local swap is a v0.1 item** (use an OpenAI-compatible endpoint today) |
| prometheus/client_golang                        | Apache-2.0     | metrics                                                                                                                                                     |
| modelcontextprotocol/go-sdk                     | MIT→Apache-2.0 | MCP server/client (license transition)                                                                                                                      |

Model weights are **not** redistributed in this repository; the sidecar
image downloads them at build time and runs offline afterwards. Choose the
model path that fits your terms — the quickstart documents the swap.

## Status

v0.1.0 — built task by task; the acceptance criteria for the release live
in the design document and are exercised by `scripts/acceptance.sh`.
