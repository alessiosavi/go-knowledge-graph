# go-knowledge-graph (`gkg`)

**Document → knowledge graph → agent queries.** Ingest text, files
(Markdown/txt/PDF) or URLs, extract entities and relations into
[Memgraph](https://memgraph.com) with full provenance, and query the graph
from agents over MCP + REST — optionally verifying facts with an LLM judge
and measuring extraction quality with the built-in eval harness.

- **Pipeline**: decode → sentence-aware chunks → near-duplicate detection
  (MinHash/LSH) → extraction (local GLiNEREL sidecar by default, or an
  OpenAI-compatible LLM) → confidence floors → provenance merge. Unchanged re-ingest is a
  no-op; changed content supersedes the old version with an audit trail.
- **Agent surface**: MCP over stdio and streamable HTTP, mirrored by REST:
  ingest, status, search, entity views with evidence, traversal, document
  listing, and guarded read-only Cypher (write clauses rejected, row cap +
  timeout enforced).
- **Built-in web UI**: `GET /ui/` serves a self-contained viewer straight
  from the binary — search, entity cards with evidence, documents (with a
  markdown document reader), ingest (text, URL or file upload),
  per-document delete (✕ removes the whole version chain) and a one-hop
  neighborhood; no extra tools needed.
- **Ask the graph**: an optional server-side agent (`POST /v1/agent/ask`,
  disabled by default) answers natural-language questions by running the
  read-only graph tools and returning the raw evidence it used; answers
  stream progressively (`POST /v1/agent/ask/stream`) with the full evidence
  as the final payload.
- **Quality**: optional online judge (`off | low-confidence | sample | all`)
  removes refuted facts with a `:RejectedFact` audit; `gkg eval` prints
  lenient F1, judge-scored precision and reference coverage, while the
  `--out` report carries the full strict/lenient P/R/F1 table on a golden
  set.
- **Optional embeddings**: entity vectors fuse into search when an
  OpenAI-compatible `/embeddings` endpoint or the asymmetric `furls`
  service (separate document and query Function URLs, 768d) is configured;
  without one, search is text-only.

The default stack runs Memgraph, the static Go binary (`gkg`), a local
GLiNEREL sidecar with offline extraction, and docling-serve for high-quality
conversion of PDFs, Office documents and images to markdown. Images use
Tesseract OCR by default (English only in the docling image), with a 12.5 MP
limit checked before conversion. Transparent PNGs are flattened onto white;
HEIC/AVIF must be exported as PNG or JPEG. Figures become image placeholders
instead of embedded base64. See the [conversion settings](docs/quickstart.md#7-high-quality-document-ingestion-docling)
for limits and overrides.

## Quickstart

Prerequisites: Docker with Compose ≥ 2.24.4; servers need Engine ≥ 28.

```sh
cp .env.example .env    # set MEMGRAPH_PASSWORD and GKG_API_KEY; provider keys for agent/embeddings if enabled
make passwd LOGIN=alice  # the password people type at the edge (read from a prompt)
make up
```

Open `https://localhost/ui/` (the edge: nginx + ModSecurity, password,
self-signed certificate until `make cert`; `EDGE_HTTPS_PORT` if changed). The
direct loopback port `http://127.0.0.1:8080/ui/` keeps working on the host.

- `make up LIGHT=1` starts Memgraph + gkg with mock extraction and conversion off.
- `make down` stops the stack and preserves graph data.
- `make smoke` checks the running project and deletes its temporary document.
- `make destroy CONFIRM=yes` permanently deletes the graph volume.
- `make cert` / `make renew` issue and renew the Let's Encrypt certificate
  on a public box; `make waf-mode MODE=DetectionOnly|On` switches the WAF;
  `make edge-check` runs the edge acceptance suite on an isolated stack.

Then connect an agent ([docs/agent-setup.md](docs/agent-setup.md)),
evaluate ([docs/quickstart.md#8-evaluation](docs/quickstart.md)), or run
the acceptance script (`scripts/acceptance.sh`); day-to-day usage lives in
[USAGE.md](USAGE.md). The full five-minute tour, configuration reference
and troubleshooting live in [docs/quickstart.md](docs/quickstart.md).
For the SSM-managed EC2 deployment, see [AWS box operations](docs/deploy-aws.md).

## Architecture

| Component          | What it does                                                                       |
| ------------------ | ---------------------------------------------------------------------------------- |
| `cmd/gkg`          | CLI: `serve` (REST + /mcp + workers), `ingest`, `mcp` (stdio), `eval`, `migrate`   |
| `internal/api`     | REST `/v1/*`, health/metrics, bearer auth, body caps, read-only Cypher guard       |
| `internal/mcp`     | the seven agent tools over stdio and streamable HTTP                               |
| `internal/ingest`  | pipeline, worker pool, recovery, dedup, URL/PDF/HTML inputs                        |
| `internal/convert` | default docling-serve converter (PDF/Office/images → markdown)                     |
| `internal/graph`   | Memgraph store: documents/chunks/entities/facts, supersede, GC, text/vector search |
| `internal/search`  | entity/chunk search with rank fusion of text and optional embedding legs           |
| `internal/extract` | extractor contract + OpenAI-compatible and GLiNEREL-sidecar providers              |
| `internal/judge`   | online fact verification (OpenAI-compatible)                                       |
| `internal/eval`    | golden-set harness, strict/lenient metrics, threshold sweep                        |
| `internal/obs`     | Prometheus metrics, JSON logs, request middleware                                  |
| `sidecar/`         | FastAPI GLiNEREL service (weights baked into the image, runtime offline)           |

The REST API is described in [docs/openapi.yaml](docs/openapi.yaml), browsable
as Swagger UI at `/docs/` (behind the edge login), and the product's limits
are in [docs/known-limitations.md](docs/known-limitations.md). Third-party
access (per-caller logins, REST, MCP): [USAGE.md §5](USAGE.md).

## Configuration

`config.yaml` (values, no secrets) + `GKG_*` environment overrides
(secrets), all documented with defaults in [.env.example](.env.example).
Highlights: `GKG_EXTRACTOR_MODE=openai|sidecar|mock`,
`GKG_JUDGE_MODE=off|low-confidence|sample|all`,
`GKG_EMBEDDER_ENABLED`, `GKG_EMBEDDER_PROVIDER=openai|furls`,
`GKG_API_KEY` (gkg's bearer; required by the edge, which injects it), `GKG_LIMITS_CYPHER_ROWS`.

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
| yuin/goldmark                                   | MIT            | server-side markdown rendering for the document reader                                                                                                      |
| dslipak/pdf                                     | BSD-3-Clause   | best-effort pure-Go PDF text (Go Authors license)                                                                                                           |
| GLiNEREL (`glinerel-fast`, default local model) | LFM            | **free for commercial use while annual revenue is under $10M USD**; above that a commercial license from the vendor is required                             |
| gliner2.5 (alternative model family)            | Apache-2.0     | fully free alternative; **not loadable by the pinned glinerel-fast v0.1 runtime — the local swap is a v0.1 item** (use an OpenAI-compatible endpoint today) |
| prometheus/client_golang                        | Apache-2.0     | metrics                                                                                                                                                     |
| modelcontextprotocol/go-sdk                     | MIT→Apache-2.0 | MCP server/client (license transition)                                                                                                                      |
| swagger-ui-dist                                 | Apache-2.0     | API reference page at `/docs/`, vendored in `internal/apidocs` (license and pinned version alongside)                                                       |

Model weights are **not** redistributed in this repository; the sidecar
image downloads them at build time and runs offline afterwards. Choose the
model path that fits your terms — the quickstart documents the swap.

## Status

v0.1.0 — built task by task; the acceptance criteria for the release live
in the design document and are exercised by `scripts/acceptance.sh`.
