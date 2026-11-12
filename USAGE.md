# gkg usage guide

Practical day-to-day usage of **go-knowledge-graph**: ingest knowledge,
explore the graph, and query it from agents. Installation and configuration
live in the [README](README.md) and [quickstart](docs/quickstart.md);
product limits are recorded in [docs/known-limitations.md](docs/known-limitations.md).

## 1. Ingest knowledge

Ingest text, files (Markdown/txt/PDF/DOCX/… — rich formats convert through
docling by default) or URLs. Every submission becomes a
document with full provenance; unchanged content is a no-op, changed
content supersedes the previous version.

**CLI** (runs the pipeline synchronously):

```sh
gkg ingest notes.md                      # a file
gkg ingest https://example.com/article   # a URL
echo "Ada Lovelace wrote the first algorithm." | gkg ingest -
gkg ingest -source notes/ada renamed.md  # keep an earlier source's identity
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

Before returning `202`, gkg saves the converted text and submission metadata
to a private spool. Queued files, text and URLs survive a gkg restart without
re-uploading or converting again. Once chunks are persisted, recovery uses
them and removes the spool entry. Failed documents still need a re-push.

Compose stores the spool in the `gkg-data` named volume; keep that volume
alongside Memgraph's data. For standalone `gkg serve`, the default is
`$HOME/.local/share/gkg/ingest-spool` (created automatically); set
`GKG_INGEST_SPOOL_DIR` to another writable persistent directory if needed.
Use one gkg process per spool directory. `GKG_INGEST_SPOOL_MAX_COUNT` (1024)
and `GKG_INGEST_SPOOL_MAX_BYTES` (1073741824, 1 GiB of serialized payloads)
bound queued storage. When full, REST returns `503` (`error: ingest_busy`)
with `Retry-After: 30`; retry after capacity is freed. A force or failed/superseded
re-submit while a worker holds that document's processing lock returns `503`
(`error: document_busy`) with the same retry header. An unchanged live version
without force returns a no-op immediately, even while processing or when the
spool is full or unavailable. A spool write failure returns `500`
(`error: ingest_unavailable`).

Sidecar inference runs one request at a time per process, using the available
CPU threads. `GKG_EXTRACTOR_SIDECAR_TIMEOUT_S` defaults to 300 seconds per
batch of up to eight chunks, including time queued behind other workers
(four by default). Tune it for slower machines or larger worker pools;
serialization avoids CPU oversubscription but cannot guarantee a deadline.

**From the UI**: the ingest box also takes one file — pick Kind → File,
choose it, and the Source field prefills with the filename (it stays
editable). Keep the same source string when re-uploading a changed or
renamed file and it supersedes its previous version (`gkg ingest -source
notes/ada renamed.md` is the CLI equivalent). The optional Labels field
suggests the shipped presets listed below and accepts custom preset names.
Leave it empty for `general`. Rich formats (pdf, docx, …)
convert through docling by default. Extraction uses the GLiNEREL sidecar
by default; both services start with `make up`.

Presets in `labels/` choose the vocabulary for future extractions:

- `general`: broad default for mixed content.
- `documentation`: integration guides, runbooks, READMEs and design documents.
- `code`: source code, pull requests and diffs.
- `mail`: administrative and marketing mail, including invoices and orders.
- `meeting`: meeting notes and transcripts, with decisions, actions and risks.
- `demo`: demo scripts, recordings and product feedback.
- `news`: people, organizations, places and events in news articles.

Choose one in the UI's Labels field, or send `labels` in a REST submission:

```sh
curl -s -F 'file=@runbook.md' -F 'labels=documentation' localhost:8080/v1/ingest
curl -s -H 'Content-Type: application/json' \
     -d '{"text":"...","labels":"meeting"}' localhost:8080/v1/ingest
```

Custom presets are YAML files under `labels.presets_dir`
(`GKG_LABELS_PRESETS_DIR`). The built-in defaults match `general`.
Mail amounts and dates, and meeting dates, use document-local identity so
unrelated documents do not merge their value entities.

**From an agent**: the MCP `ingest` tool takes `url` or `text`, plus an
optional `source` label and `force` to re-run an unchanged document.

## 2. Explore the graph

The graph is plain Memgraph — anything that speaks Bolt works. Three easy
options:

- **Built-in Web UI**: open `http://localhost:8080/ui/` (the binary serves
  it — no extra service). Search entities and chunks, open entity cards
  with their evidenced facts, browse documents, submit text or URLs, and
  see a one-hop neighborhood graph of the selected entity. Clicking a
  document row, a chunk hit, or an evidence link opens the **document
  reader**: the document's stored text rendered as markdown (tables, code,
  links) by `GET /v1/documents/{id}/content`. A row's ✕ button deletes the
  document with its chunks, facts, mentions and older versions of the same
  source (`DELETE /v1/documents/{id}?chain=1`) — permanently, after a
  confirm. The UI is otherwise read-only, and asks for the API key once
  when `GKG_API_KEY` is set.
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
  `GKG_API_KEY` to require `Authorization: Bearer <key>`. Through the edge,
  use `https://<host>/mcp` with `Authorization: Basic <base64 of login:password>`.

Example prompts:

- "Ingest <url> and tell me when it is done."
- "Search for 'Widget Pro' and summarise the top chunks with their sources."
- "Show everything the graph knows about Ada Lovelace, with citations."
- "Traverse from `ada lovelace` two hops and list the evidence."

## 4. Ask the graph (in-UI agent)

With `GKG_AGENT_ENABLED=true` the web UI grows an **Ask the graph** panel
and `POST /v1/agent/ask` answers natural-language questions server-side: an
OpenAI-compatible model plans and runs the same read-only tools as MCP —
search, entity cards, traversal, documents, guarded Cypher — and the
response carries the raw tool results as `evidence`, so every answer is
checkable. Answers use GitHub-flavoured Markdown: `answer` is the source
text and `answer_html` is server-sanitized HTML, shared with the document
reader. Raw HTML and all answer images are omitted; links follow the
reader’s policy. The UI falls back to plain text if `answer_html` is absent.
The browser never sees the provider key.

```sh
# in .env: GKG_AGENT_ENABLED=true plus GKG_AGENT_BASE_URL/API_KEY/MODEL
curl -s localhost:8080/v1/agent/ask -H 'Content-Type: application/json' \
     -d '{"question":"What does the graph know about Ada Lovelace?"}'
# → {"answer":"...","answer_html":"<p>...</p>\n","evidence":[{"tool":"search","arguments":{...},"result":{...}}],"steps":2}
```

**Streaming** (default on) extends the same answer with live progress: the
panel consumes `POST /v1/agent/ask/stream`, an SSE surface emitting `tool`
events when a read-only call is about to run, `delta` fragments of the
answer as the model produces them, and one authoritative `done` event with
the same `{"answer","answer_html","evidence","steps"}` payload (`error` events are
classified). It disables proxy buffering (`X-Accel-Buffering: no`) and
sends a `: ping` heartbeat every ~15s.

```sh
curl -N -s localhost:8080/v1/agent/ask/stream -H 'Content-Type: application/json' \
     -d '{"question":"What does the graph know about Ada Lovelace?"}'
```

`GET /v1/agent/status` reports the capability as
`{"enabled":true,"stream":true}`. With `GKG_AGENT_STREAM=false` the stream
endpoint still answers — SSE framing, tool and done events only, no deltas —
and the panel uses the JSON endpoint; a provider that does not actually
stream is detected automatically (the agent falls back to non-streamed
answers for the process lifetime).

Each ask is independent (no chat memory) and bounded:
`GKG_AGENT_MAX_STEPS` model turns that request tools (default 6, hard cap
30; each turn may run up to 8 tool calls) and `GKG_AGENT_TIMEOUT_S`
(default 60). Model-facing tool results are capped at 32 KiB each; the
cumulative transcript budget is `GKG_AGENT_CONTEXT_BUDGET_KB` (default 256
KiB). Reaching either the step or context budget triggers one final turn
without tools, using the gathered results within the same timeout. If that
turn fails or is empty, the response carries a bounded notice and the full
evidence. `steps` counts tool turns only. These byte limits cover tool
results, not the question, tool arguments, or other transcript messages.

The edge derives `EDGE_ASK_TIMEOUT_S` from the agent timeout plus 30 seconds
when unset (90 by default). An explicit value below the agent timeout plus
15 seconds prevents edge startup. For a 300-second agent timeout, leaving
the edge timeout unset produces 330 seconds for both JSON and streamed asks.

While the agent is disabled the endpoint
answers `501` and the panel stays hidden. Answers are LLM-generated — the
`evidence` array is the source of truth
([known limits](docs/known-limitations.md)).

## 5. Evaluate extraction quality

```sh
gkg eval --golden eval/golden/sample.jsonl --provider mock      # offline smoke
gkg eval --golden eval/golden/sample.jsonl --provider mock --labels general
gkg eval --golden eval/golden/sample.jsonl --provider openai    # real provider
gkg eval --golden eval/golden/sample.jsonl --provider openai \
  --sweep 'entity=0.2:0.8:0.1 relation=0.1:0.6:0.1' --out report
```

The harness prints lenient entity/relation F1, judge-scored precision and
reference-fact coverage; `--out` writes a Markdown + JSON report.
`--labels <preset>` loads `<preset>.yaml` from `labels.presets_dir` (default:
`labels/`). Without `--labels`, evaluation uses the built-in labels. Unknown
or invalid presets fail before extraction. An optional label `text` supplies
natural-language wording to the sidecar; extracted types keep the label `name`.
Each new preset has a synthetic golden set at `eval/golden/<preset>.jsonl`;
`general` uses `eval/golden/sample.jsonl`. For example:

```sh
gkg eval --provider mock --labels mail --golden eval/golden/mail.jsonl --out mail-report
```

The JSON report records `preset` (empty for built-in defaults), plus each
label's `name`, `text` and `threshold`. Mock runs check the harness; their
scores do not measure extraction quality. The presets ship starting thresholds,
with tuning deferred to a separate evaluation.

## 6. Operations

- Health: `GET /healthz`, `GET /readyz`; metrics: `GET /metrics`
  (auth-exempt).
- `make up` starts the default sidecar extractor and docling converter;
  `make up LIGHT=1` uses mock extraction with conversion off. `make down`
  preserves graph data; `make smoke` checks the running stack.
- Optional knobs (defaults documented in `.env.example`): bearer auth,
  embeddings (OpenAI-compatible or the asymmetric `furls` service), the
  online fact judge, and the read-only ask agent (`GKG_AGENT_*`).
- Troubleshooting and known limits:
  [docs/known-limitations.md](docs/known-limitations.md) and the
  quickstart's troubleshooting section.
