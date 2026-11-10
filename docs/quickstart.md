# gkg quickstart

Document → knowledge graph → agent queries in five minutes. `gkg` ingests
text, files (Markdown/txt/PDF) and URLs into Memgraph with full provenance,
and answers agent queries over MCP and REST.

## 1. Configuration

```sh
git clone https://github.com/alessiosavi/go-knowledge-graph && cd go-knowledge-graph
cp .env.example .env
# edit .env:
#   MEMGRAPH_PASSWORD=<generate one, e.g. openssl rand -hex 24>
#   provider keys for agent/embeddings, if enabled
```

Every knob is documented in `.env.example`; environment variables are the
`GKG_<SECTION>_<KEY>` form of `config.yaml` and override it.

## 2. Start the stack

```sh
make up
curl -s localhost:8080/healthz        # {"status":"ok"}
curl -s localhost:8080/readyz         # {"status":"ready"}
```

Requires Docker Compose ≥ 2.24.4; servers need Docker Engine ≥ 28.
`make up` starts Memgraph, gkg, GLiNEREL and docling. Open
`http://127.0.0.1:8080/ui/` (or the configured `GKG_PORT`). All four host
ports bind to `127.0.0.1`; change `MEMGRAPH_PORT`, `GKG_PORT`, `SIDECAR_PORT`
or `DOCLING_PORT` in `.env` for port conflicts. `ENV_FILE=.env.other` selects
a different env file; `PROJECT=another-project` selects an isolated stack
and also needs an `ENV_FILE` with different `*_PORT` values to avoid port
conflicts with the existing stack.

Memgraph keeps its data in the `gkg_memgraph-data` volume. `make down`
preserves it; `make destroy CONFIRM=yes` permanently deletes it. `make smoke`
checks the running project's readiness, ingestion, search and deletion.

**Upgrade from an older `.env`:** existing `GKG_EXTRACTOR_MODE=openai` and
`GKG_CONVERTER_MODE=off` values override the new defaults, so gkg will not
use the sidecar/docling even though they start. Set them to `sidecar` and
`docling`, or delete the two lines to use the defaults.

**Host-run binaries:** the default sidecar and converter URLs use localhost
ports 8090 and 5001. Keep the stack up, configure URLs explicitly if its
host ports change, or set the extractor/converter modes explicitly for a
standalone setup. An unavailable model service fails ingestion without a
silent fallback. Leave the two URL overrides unset when sharing `.env`
between Compose and a host-run binary.

**Memory limits:** `deploy/docker-compose.yml` holds each service's
`mem_limit` and Memgraph's `--memory-limit` (MiB): 1.3 GiB / 768 MiB / 4.75 GiB /
6.5 GiB / 704 MiB for Memgraph/gkg/sidecar/docling/edge (14 GiB in total), sized from
measured peaks under concurrent ingestion, conversion (incl. a 101-page PDF)
and asks.
Raise a service's limit there if needed, retain host headroom, keep Memgraph's
internal budget below its container limit, then run `make up` to apply it.
`DOCLING_ENG_WORKERS` (default 2) and `DOCLING_NUM_THREADS` (default 4) tune
conversion concurrency.

### The edge (HTTPS, password, WAF)

`make up` also starts the **edge**: nginx with ModSecurity and the OWASP Core
Rule Set (paranoia level 1, blocking), the only service published beyond
loopback (`EDGE_BIND`, default `127.0.0.1`; the EC2 box sets `0.0.0.0`).

- **Password:** `make passwd LOGIN=<name>` reads a password from a prompt (or
  stdin) and stores its bcrypt hash in `deploy/edge/htpasswd` (untracked);
  `GENERATE=1` creates and prints a random one. `make up` refuses to start
  without it. Changing a password needs only `make passwd` + `make up`.
- **What is exposed:** the UI (`/ui/`, `/` redirects there), the REST routes
  the UI uses and `/mcp`. `/v1/cypher`, `/metrics`, `/readyz` and every other
  path or method are a 404 behind the password (use the `cypher` MCP tool or
  the loopback ports on the host). `/healthz` answers without a password.
- **Upstream:** the edge replaces the client's `Authorization` with
  `GKG_API_KEY` (now required), so users never see gkg's key.
- **Limits:** files up to 20 MiB (form overhead included); JSON bodies
  (pasted text, MCP calls) up to 4 MiB at the edge — upload larger documents
  as files. Cross-site form posts are refused (CSRF guard). Per-IP rate limit
  20 requests/s with bursts of 80.
- **WAF:** `make waf-mode MODE=DetectionOnly` logs without blocking (use it
  first on a new deployment with real documents), `MODE=On` blocks (the
  default). Exclusions live in
  `deploy/edge/modsec/REQUEST-900-EXCLUSION-RULES-BEFORE-CRS.conf`, each scoped
  to one route and named fields.
- **Certificates:** locally a self-signed placeholder (browsers warn). On a
  public box with DNS pointing at it and port 80 open, `make cert`
  (`STAGING=1` for Let's Encrypt staging) issues for `EDGE_SERVER_NAME`;
  `make renew` (run twice a day by the box's timer) renews, installs only a
  changed certificate, reloads nginx, verifies the served certificate and
  exits non-zero below 14 days to expiry.
- **Acceptance:** `make edge-check` runs routing, auth, CSRF, limits,
  streaming, MCP, the attack battery (DetectionOnly then On), log hygiene and
  renewal against an isolated stack (also the CI `edge` job).

**Upgrade:** an existing `.env` needs `GKG_API_KEY` and a `make passwd` before
`make up`; set `EDGE_HTTP_PORT`/`EDGE_HTTPS_PORT` if 80/443 are taken.

## 3. Ingest and query

```sh
# a local markdown file
curl -s -F 'file=@notes.md' localhost:8080/v1/ingest
# → {"document_id":"9f2c…","noop":false}

# a URL
curl -s -H 'Content-Type: application/json' \
     -d '{"url":"https://en.wikipedia.org/wiki/Knowledge_graph"}' \
     localhost:8080/v1/ingest

# poll one document
curl -s localhost:8080/v1/documents/<id>/status

# search entities and chunks
curl -s -H 'Content-Type: application/json' \
     -d '{"query":"knowledge graph","kind":"auto"}' localhost:8080/v1/search

# one entity with evidenced facts
curl -s 'localhost:8080/v1/entities/<id-or-key>?include_low_confidence=true'

# guarded read-only Cypher
curl -s -H 'Content-Type: application/json' \
     -d '{"query":"MATCH (d:Document) RETURN d.id, d.status"}' localhost:8080/v1/cypher
```

The same data is browsable in the built-in viewer: open
`http://localhost:8080/ui/` for search, entity cards with evidence,
documents, ingest and a one-hop neighborhood graph — no extra service.

The same submissions work from the CLI without HTTP:

```sh
docker compose --env-file .env -f deploy/docker-compose.yml exec gkg \
  gkg ingest /dev/stdin <<'EOF'
Ada Lovelace wrote the first algorithm.
EOF
```

Label presets (`labels/general.yaml`, `labels/news.yaml`) are mounted at
`/labels` in compose; select one per submission:

```sh
curl -s -F 'file=@article.md' -F 'labels=news' localhost:8080/v1/ingest
```

Re-ingesting unchanged content is a no-op (`"noop":true`); changed content
creates a new version and supersedes the old one.

## 4. Connect an agent (MCP)

The service is an MCP server over stdio (`gkg mcp`) and streamable HTTP
(`http://localhost:8080/mcp`). Configuration examples for OpenCode and
Claude Desktop are in [agent-setup.md](agent-setup.md); the tool set is
`ingest`, `status`, `search`, `get_entity`, `traverse`, `list_documents`,
`cypher`.

## 5. Local, fully-offline extraction

The GLiNEREL sidecar starts with `make up`; `sidecar` is the default
extractor mode. Weights are baked into the image (runtime
`HF_HUB_OFFLINE=1`, no network).

Select the checkpoint at **image build time** with `GLINEREL_MODEL` in
`.env` (`314m` default, `72m` smaller/faster; both resolve to the
`VAGOsolutions/SauerkrautLM-GLiNEREL-*` repositories). After changing it:

```sh
make build S=sidecar && make up
```

To run without either model service on CI or a laptop:

```sh
make up LIGHT=1
```

Light mode uses mock extraction and disables conversion; neither model
service is built or pulled. Set `GKG_LIGHT_EXTRACTOR_MODE=openai` and the
OpenAI-compatible extractor settings to use an LLM instead. Include
`LIGHT=1` with subsequent stack commands for that setup.

Any other Hugging Face repository id is passed through verbatim; the
checkpoint must be loadable by the bundled `glinerel-fast` runtime (the
GLiNEREL config contract). The gliner2.5 family (Apache-2.0) is the planned
fully-free local alternative but **cannot be loaded by the pinned runtime
yet** — that swap is a v0.1 item. For a free extraction path in v0, run any
model behind an OpenAI-compatible endpoint and use
`GKG_EXTRACTOR_MODE=openai` against it.

Non-Latin scripts and mixed-language documents (e.g. Italian) go through
the same pipeline; both enrichment quantities are covered by the manual
acceptance check (`scripts/acceptance.sh` prints it as a SKIP).

## 6. Optional: embeddings and the judge

Two embedding providers feed the entity-vector search leg:

```sh
# OpenAI-compatible /embeddings (default provider)
GKG_EMBEDDER_ENABLED=true
GKG_EMBEDDER_API_KEY=<key>

# asymmetric embeddings service (document and query Function URLs, 768d)
GKG_EMBEDDER_ENABLED=true
GKG_EMBEDDER_PROVIDER=furls
# the endpoints and the shared secret live in .env (never committed):
#   EMBED_QUERY_FURL=<embed_query Function URL>
#   EMBED_PRODUCT_FURL=<embed_product Function URL>
#   EMBED_API_KEY=<shared secret>
#   EMBED_BATCH_SIZE=16   (optional: entries per request; chunks run in
#                          parallel and merge in order; service max 128)
```

The `furls` provider embeds documents at ingest (`embed_product`) and queries
at search (`embed_query`) — the service applies its own asymmetric prompt
packing, and the shared secret travels in the `x-api-key` header. Startup
fails fast when an endpoint or the secret is missing, or when
`GKG_EMBEDDER_MODEL` is set to anything but the service's allowlisted model
(`gemini-embedding-2`; leave it empty to default).

**Switching embedding spaces** (e.g. OpenAI → furls) needs a manual reset:
the store can only hold one space per index, and both spaces here are 768d,
so the switch is invisible to Memgraph. Clear the old vectors and rebuild:

```cypher
MATCH (e:Entity) WHERE e.embedding IS NOT NULL REMOVE e.embedding;
DROP VECTOR INDEX entity_vec;
```

then re-ingest; unchanged documents need an explicit forced re-submission
(`"force":true` on the REST submission or the MCP `ingest` tool). At startup
gkg records the active space and logs a prominent warning when it differs
from the stored one — if you see that warning, run the migration above
first.

```sh
# online fact verification (real key; off by default)
GKG_JUDGE_MODE=low-confidence      # off | low-confidence | sample | all
GKG_JUDGE_API_KEY=<key>
```

With embeddings on, entity search fuses the text index with vector hits;
without them search is text-only. The judge verifies facts after extraction
against their evidence sentence and removes refuted ones with an audit
trail.

## 7. High-quality document ingestion (docling)

[docling-serve](https://github.com/docling-project/docling-serve) starts
with `make up` and `docling` is the default converter mode. Its CPU image
includes model weights and converts scanned PDFs and DOCX/PPTX/XLSX
to clean markdown for the ingestion pipeline. Use `LIGHT=1` to run without
it.

Images are converted by default (`GKG_CONVERTER_IMAGES=true`). Every request
uses Tesseract OCR (`GKG_CONVERTER_OCR_ENGINE=tesseract`); the docling image
ships English OCR data only, so Italian accents can be misread. The engine is
configurable for a docling image that ships another supported engine.

Images larger than 12.5 megapixels are refused from their headers before they
reach docling. Downscale them, or raise `GKG_CONVERTER_MAX_IMAGE_MP` only on a
larger-memory machine after measuring peak usage with concurrent conversions.
PNG, JPEG, GIF, BMP, WEBP and TIFF dimensions are checked; unsupported or
corrupt image headers are refused. HEIC/AVIF must be exported as PNG or JPEG.
Transparent PNGs are flattened onto white, one at a time, before conversion.
Figures in all converted documents use image placeholders instead of embedded
base64 payloads. Set `GKG_CONVERTER_IMAGES=false` to refuse all image uploads
while continuing to convert PDF and Office documents.

With `GKG_CONVERTER_MODE=docling`, PDF and Office inputs are
converted by the sidecar instead of the best-effort pure-Go PDF path. The
sidecar publishes loopback `127.0.0.1:5001`, so host-run builds use
the default `GKG_CONVERTER_URL` (`http://localhost:5001`) (compose defaults to
`http://docling:5001`). Set `DOCLING_API_KEY` in `.env` to require the
`X-Api-Key` header on the sidecar. A conversion failure fails the document
with the converter's reason — there is no silent fallback.
`GKG_CONVERTER_TIMEOUT_S` (default 300) bounds a conversion; the server's
sync wait is pinned just below it, so a slow document fails within the
client budget (the conversion task keeps running server-side). With the
pinned `v1.1.0`, exceeding the sync wait surfaces a generic HTTP 500 rather
than the documented explanatory 504; gkg reports either as a load-stage
failure.

## 8. Evaluation

```sh
docker compose --env-file .env -f deploy/docker-compose.yml run --rm --no-deps \
  -v "$PWD/eval:/eval" gkg eval --golden /eval/golden/sample.jsonl --provider mock
```

(The container's entrypoint is already `gkg`: `run` appends the command to
it, while `exec` takes a full command — hence `exec gkg gkg ingest …` vs
`run … gkg eval …`.)
`--no-deps` keeps model services stopped: eval never opens the graph.

Real extraction numbers need a configured provider/key (or the sidecar);
`--sweep 'entity=0.2:0.8:0.1 relation=0.1:0.6:0.1'` (one quoted argument)
searches confidence floors, and `--out report` writes `report.md` +
`report.json` — mount a writable directory for that, e.g.
`-v "$PWD:/work" -w /work`.

## 9. Health, metrics, acceptance

- `GET /healthz` (liveness), `GET /readyz` (Memgraph connectivity),
  `GET /metrics` (Prometheus) — all exempt from the optional API key
  (`GKG_API_KEY`).
- End-to-end acceptance: `scripts/acceptance.sh` (compose mode by default;
  `ACCEPTANCE_MODE=local GKG_MEMGRAPH_URI=…` for an existing Memgraph, which
  is the CI mode). Compose acceptance uses an isolated light stack and
  ports 17687/18080/18090/15001 by default; `ACCEPTANCE_FULL=1` includes
  model services, and `ACCEPTANCE_MEMGRAPH_PORT`, `ACCEPTANCE_GKG_PORT`,
  `ACCEPTANCE_SIDECAR_PORT`, `ACCEPTANCE_DOCLING_PORT` override those ports.

## Troubleshooting

- **Auth failures / `Unauthenticated`**: the `MEMGRAPH_USER` /
  `MEMGRAPH_PASSWORD` in `.env` must match between the memgraph and gkg
  services (compose wires both).
- **Port 7687 already used** (e.g. another Neo4j/Memgraph): set
  `MEMGRAPH_PORT=17687` in `.env`; the port is only for host access.
- **PDFs without text in light mode**: the pure-Go path has no OCR; see
  [known-limitations.md](known-limitations.md).

## License notes

Code is Apache-2.0. The default sidecar model (GLiNEREL) and Memgraph carry
their own licenses — see the table in the [README](../README.md#license).
