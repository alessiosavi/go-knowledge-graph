# gkg usage guide

Practical day-to-day usage of **go-knowledge-graph**: ingest knowledge,
explore the graph, and query it from agents. Installation and configuration
live in the [README](README.md) and [quickstart](docs/quickstart.md);
product limits are recorded in [docs/known-limitations.md](docs/known-limitations.md).

## 1. Ingest knowledge

Ingest text, files (Markdown/txt/PDF/DOCX/… — rich formats convert through
the optional `docling` compose profile) or URLs. Every submission becomes a
document with full provenance; unchanged content is a no-op, changed
content supersedes the previous version.

**CLI** (runs the pipeline synchronously):

```sh
gkg ingest notes.md                      # a file
gkg ingest https://example.com/article   # a URL
echo "Ada Lovelace wrote the first algorithm." | gkg ingest -
```

**REST**:

```sh
curl -s -F 'file=@notes.pdf' localhost:8080/v1/ingest
curl -s -H 'Content-Type: application/json' \
     -d '{"url":"https://example.com/article"}' localhost:8080/v1/ingest
curl -s -H 'Content-Type: application/json' \
     -d '{"text":"...","source":"notes/ada"}' localhost:8080/v1/ingest
```

Submissions are queued: poll the returned id until it reaches `done`:

```sh
curl -s localhost:8080/v1/documents/<id>/status
```

**From an agent**: the MCP `ingest` tool takes `url` or `text`, plus an
optional `source` label and `force` to re-run an unchanged document.

## 2. Explore the graph

The graph is plain Memgraph — anything that speaks Bolt works. Three easy
options:

- **Built-in Web UI**: open `http://localhost:8080/ui/` (the binary serves
  it — no extra service). Search entities and chunks, open entity cards
  with their evidenced facts, browse documents, submit text or URLs, and
  see a one-hop neighborhood graph of the selected entity. It is read-only
  except the explicit ingest box, and asks for the API key once when
  `GKG_API_KEY` is set.
- **gkg REST**: `POST /v1/search` (entity + chunk hits), `GET /v1/entities/{id-or-key}`
  (entity card with evidenced facts), `POST /v1/traverse` (fact-edge walk),
  `POST /v1/cypher` (guarded read-only Cypher).
- **Memgraph Lab**: run `memgraph/lab`, point it at the Bolt endpoint
  (`bolt://localhost:7687`; credentials from your `.env`) and use the Query
  editor with the Graph view. Handy starting queries:

```cypher
// documents → chunks → entities
MATCH p=(d:Document)-[:HAS_CHUNK]->(:Chunk)-[:MENTIONS]->(:Entity) RETURN p LIMIT 150
// extracted facts between entities
MATCH p=(h:Entity)-[r]->(t:Entity) WHERE NOT h.id = t.id RETURN p LIMIT 200
// inventory: what has been ingested
MATCH (d:Document) RETURN d.source, d.status, d.title ORDER BY d.created_at DESC
```

## 3. Query from an agent (MCP)

`gkg` is an MCP server (stdio and streamable HTTP) exposing seven tools:
`ingest`, `status`, `search`, `get_entity`, `traverse`, `list_documents`,
`cypher`. Configuration examples for OpenCode and Claude Desktop are in
[docs/agent-setup.md](docs/agent-setup.md); the short version:

- **stdio**: run `gkg mcp` with `GKG_MEMGRAPH_URI` (+ credentials) in the
  environment — the agent host starts the process.
- **streamable HTTP**: `POST /mcp` on the `serve` surface; set
  `GKG_API_KEY` to require `Authorization: Bearer <key>`.

Example prompts:

- "Ingest <url> and tell me when it is done."
- "Search for 'Widget Pro' and summarise the top chunks with their sources."
- "Show everything the graph knows about Ada Lovelace, with citations."
- "Traverse from `person:ada lovelace` two hops and list the evidence."

## 4. Ask the graph (in-UI agent)

With `GKG_AGENT_ENABLED=true` the web UI grows an **Ask the graph** panel
and `POST /v1/agent/ask` answers natural-language questions server-side: an
OpenAI-compatible model plans and runs the same read-only tools as MCP —
search, entity cards, traversal, documents, guarded Cypher — and the
response carries the raw tool results as `evidence`, so every answer is
checkable. The browser never sees the provider key.

```sh
# in .env: GKG_AGENT_ENABLED=true plus GKG_AGENT_BASE_URL/API_KEY/MODEL
curl -s localhost:8080/v1/agent/ask -H 'Content-Type: application/json' \
     -d '{"question":"What does the graph know about Ada Lovelace?"}'
# → {"answer":"...","evidence":[{"tool":"search","arguments":{...},"result":{...}}],"steps":2}
```

Each ask is independent (no chat memory) and bounded:
`GKG_AGENT_MAX_STEPS` tool calls (default 6, hard cap 12) and
`GKG_AGENT_TIMEOUT_S` (default 60). While the agent is disabled the endpoint
answers `501` and the panel stays hidden. Answers are LLM-generated — the
`evidence` array is the source of truth
([known limits](docs/known-limitations.md)).

## 5. Evaluate extraction quality

```sh
gkg eval --golden eval/golden/sample.jsonl --provider mock      # offline smoke
gkg eval --golden eval/golden/sample.jsonl --provider openai    # real provider
gkg eval --golden eval/golden/sample.jsonl --provider openai \
  --sweep 'entity=0.2:0.8:0.1 relation=0.1:0.6:0.1' --out report
```

The harness prints lenient entity/relation F1, judge-scored precision and
reference-fact coverage; `--out` writes a Markdown + JSON report.

## 6. Operations

- Health: `GET /healthz`, `GET /readyz`; metrics: `GET /metrics`
  (auth-exempt).
- Optional knobs (defaults documented in `.env.example`): bearer auth,
  embeddings (OpenAI-compatible or the asymmetric `furls` service), the
  `docling` converter profile, the local offline extraction sidecar, the
  online fact judge, and the read-only ask agent (`GKG_AGENT_*`).
- Troubleshooting and known limits:
  [docs/known-limitations.md](docs/known-limitations.md) and the
  quickstart's troubleshooting section.
