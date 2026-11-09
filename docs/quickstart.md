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
#   GKG_EXTRACTOR_OPENAI_API_KEY=<your key>       (LLM extraction mode)
```

Every knob is documented in `.env.example`; environment variables are the
`GKG_<SECTION>_<KEY>` form of `config.yaml` and override it.

## 2. Start the stack (LLM mode)

```sh
docker compose --env-file .env -f deploy/docker-compose.yml up -d --wait
curl -s localhost:8080/healthz        # {"status":"ok"}
curl -s localhost:8080/readyz         # {"status":"ready"}
```

Memgraph keeps its data in the `gkg_memgraph-data` volume; `down -v`
removes it. Host port conflicts: set `MEMGRAPH_PORT` / `GKG_PORT` in `.env`.

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

## 5. Optional: local, fully-offline extraction

The `local` profile runs the GLiNEREL sidecar with weights baked into the
image (runtime `HF_HUB_OFFLINE=1`, no network):

```sh
# in .env:
#   GKG_EXTRACTOR_MODE=sidecar
docker compose --env-file .env --profile local -f deploy/docker-compose.yml up -d --wait
```

The checkpoint is selected at **image build time** through `GLINEREL_MODEL`
in `.env` (`314m` default, `72m` smaller/faster; both resolve to the
`VAGOsolutions/SauerkrautLM-GLiNEREL-*` repositories). Changing it needs a
rebuild, because the weights are baked and the runtime is offline:

```sh
# GLINEREL_MODEL=72m in .env, then:
docker compose --env-file .env --profile local -f deploy/docker-compose.yml build sidecar
docker compose --env-file .env --profile local -f deploy/docker-compose.yml up -d --wait
```

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

```sh
# entity-embedding vector search leg (OpenAI-compatible /embeddings)
GKG_EMBEDDER_ENABLED=true
GKG_EMBEDDER_API_KEY=<key>

# online fact verification (real key; off by default)
GKG_JUDGE_MODE=low-confidence      # off | low-confidence | sample | all
GKG_JUDGE_API_KEY=<key>
```

With embeddings on, entity search fuses the text index with vector hits;
without them search is text-only. The judge verifies facts after extraction
against their evidence sentence and removes refuted ones with an audit
trail.

## 7. Evaluation

```sh
docker compose --env-file .env -f deploy/docker-compose.yml run --rm \
  -v "$PWD/eval:/eval" gkg eval --golden /eval/golden/sample.jsonl --provider mock
```

(The container's entrypoint is already `gkg`: `run` appends the command to
it, while `exec` takes a full command — hence `exec gkg gkg ingest …` vs
`run … gkg eval …`.)

Real extraction numbers need a configured provider/key (or the sidecar);
`--sweep 'entity=0.2:0.8:0.1 relation=0.1:0.6:0.1'` (one quoted argument)
searches confidence floors, and `--out report` writes `report.md` +
`report.json` — mount a writable directory for that, e.g.
`-v "$PWD:/work" -w /work`.

## 8. Health, metrics, acceptance

- `GET /healthz` (liveness), `GET /readyz` (Memgraph connectivity),
  `GET /metrics` (Prometheus) — all exempt from the optional API key
  (`GKG_API_KEY`).
- End-to-end acceptance: `scripts/acceptance.sh` (compose mode by default;
  `ACCEPTANCE_MODE=local GKG_MEMGRAPH_URI=…` for an existing Memgraph, which
  is the CI mode).

## Troubleshooting

- **Auth failures / `Unauthenticated`**: the `MEMGRAPH_USER` /
  `MEMGRAPH_PASSWORD` in `.env` must match between the memgraph and gkg
  services (compose wires both).
- **Port 7687 already used** (e.g. another Neo4j/Memgraph): set
  `MEMGRAPH_PORT=17687` in `.env`; the port is only for host access.
- **PDFs without text**: scanned PDFs fail with "no extractable text"; see
  [known-limitations.md](known-limitations.md).

## License notes

Code is Apache-2.0. The default sidecar model (GLiNEREL) and Memgraph carry
their own licenses — see the table in the [README](../README.md#license).
