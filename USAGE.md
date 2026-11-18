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

`GKG_INGEST_STOPLIST_FILE` optionally points to a private entity-name stoplist
overlay: one literal entry per line; blank lines and lines beginning with `#`
are ignored. Entries match normalized names without case or diacritic differences.

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

Presets also configure entity-name hygiene with `allowlist:` (exact tokens allowed
at two characters or fewer), `stoplist:` (folded exact matches and regular
expressions), and `code_tokens:` (allow path and code tokens; enabled in `code`
and `documentation`):

```yaml
allowlist: [AI, EU, UK, US, UE, IT]
stoplist:
  exact: [Descrizione, Informazioni, Indice, Introduzione]
  patterns: ['^Sezione \d+$', '^Section \d+$']
code_tokens: true
```

Entity labels with `scope: document` are value labels: they skip the no-letter,
short-name, CLI-flag, path and code shape rules, while normalization and newline,
length, markdown-link, heading and stoplist checks still apply.

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
  By default, entity and traversal reads show typed facts that meet the assert
  floor or that the judge confirmed. Set `include_low_confidence` or check
  **Include low-confidence facts** in the viewer to show all typed facts.
  For judge-confirmed facts only, send
  `?confirmed_only=true` on the entity GET or `"confirmed_only": true` in the
  traverse POST body. This applies at every hop, includes judge-confirmed facts
  at any confidence, and takes precedence over both inclusion flags.
  Otherwise, `include_unverified` adds marked candidates when
  `facts.allow_unverified_reads` is enabled. When the server setting is disabled,
  `unverified_ignored:true` explains the ignored request. When any `hidden`
  count is nonzero, the response reports distinct facts per state from unjudged
  and unclear `FactCandidate` records and `RejectedFact` audits touching the
  viewed entity or traversal start. These counts are independent of read filters
  and exclude typed edges suppressed by `confirmed_only` or the default
  low-confidence filter.
  See [visibility options](docs/quickstart.md).
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

`gkg` is an MCP server (stdio and streamable HTTP) exposing eight tools:
`ingest`, `status`, `search`, `get_entity`, `get_chunks`, `traverse`, `list_documents`,
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

## 5. API reference and third-party access

**Interactive reference.** `GET /docs/` serves Swagger UI over
[docs/openapi.yaml](docs/openapi.yaml), bundled in the binary (no CDN, no
external requests). Through the edge, open `https://<host>/docs/` with the
usual login: "Try it out" reuses that browser login, so leave the Authorize
dialog empty. Against gkg directly with `GKG_API_KEY` set, use Authorize →
`bearerAuth`. Routes tagged `operator` (guarded Cypher, rebuild, metrics,
readiness) are loopback-only; the edge answers 404 for them.

**One login per caller.** Give every caller (a colleague, a script, another
team's agent) its own edge login, so one can be revoked without touching the
others. Every login currently has the same rights: read, ask, ingest and
delete. There are no read-only logins yet.

```sh
make passwd LOGIN=commerce-agent GENERATE=1   # prints the new password once
make up                                        # copies the password file into the edge
```

On the EC2 box, add `EDGE_HTPASSWD=deploy/aws/box.htpasswd` and follow
[docs/deploy-aws.md](docs/deploy-aws.md) (`make box-secrets`, then deploy).
To revoke a login, delete its line from the password file and repeat the
same steps.

**REST from a server.** Send the login as HTTP Basic; the edge swaps it for
gkg's internal key, which callers never see:

```sh
curl -s -u "commerce-agent:$GKG_PASSWORD" https://<host>/v1/agent/ask \
     -H 'Content-Type: application/json' \
     -d '{"question":"What does the graph know about Ada Lovelace?"}'
curl -s -u "commerce-agent:$GKG_PASSWORD" https://<host>/v1/search \
     -H 'Content-Type: application/json' -d '{"query":"Ada Lovelace","limit":5}'
```

Server-side callers (scripts, cURL, an agent's backend) send no browser
`Origin`, so the edge's cross-site guard does not affect them. A web page
served from another domain cannot call gkg from the browser: the edge sends
no CORS headers and rejects cross-site `POST` and `DELETE`.

**MCP from an agent.** The same login works on `https://<host>/mcp`
(streamable HTTP). [docs/agent-setup.md](docs/agent-setup.md) has client
configurations and a Python example for an agent that calls gkg as a tool.

## 6. Evaluate extraction quality

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
scores do not measure extraction quality.

The documentation, code, mail, meeting and demo thresholds are tuned on their
golden sets against the GLiNER sidecar: the highest values on the best F1
plateau of a sweep (`general` and `news` keep the starting values; the
`general` sample set does not discriminate between thresholds). To reproduce or
re-tune a preset after editing it or its golden set:

```sh
gkg eval --provider sidecar --labels documentation \
  --golden eval/golden/documentation.jsonl \
  --sweep 'entity=0.2:0.7:0.05 relation=0.1:0.5:0.05' --out documentation-sweep
```

### Judge backfill and audit

Judge pending fact candidates with the configured judge provider:

```sh
gkg judge backfill --limit 100 --budget-calls 120
gkg judge backfill --domain quillon --budget-calls 120
```

Candidates are processed by id; only `judge=''` counts as unjudged, including
candidates left on a `done` document after interrupted verification. An unclear
verdict stays a candidate. `--limit 0` (the default) means no candidate limit.
`--budget-calls` is required and must be greater than zero; missing, zero or
negative budgets exit 2 with usage. The budget counts provider attempts,
including retries; cache hits use no calls.
Verdicts are cached before application, so rerunning recovers cached work.
Per-candidate errors are logged and counted as `failed`; later candidates are
still judged, and a rerun may retry failed candidates. An unchanged candidate
whose endpoint disappeared is deleted and counted as `orphaned`, separately
from `vanished` candidates whose identity or input changed during judging.
A lease lasts 120 seconds and renews every 30 seconds. `--force` takes over a
live lease; it does not select already judged candidates. Cache misses call the
configured provider and can incur charges.

Sample confirmed typed facts by domain for manual labelling, then score the JSONL offline:

```sh
gkg judge audit --sample 200 --seed 1 --out audit.jsonl
# Set each row's label to ok, wrong, noise or skip.
gkg judge audit --score audit.jsonl
```

Each row includes its identity, domain, head, relation, tail and evidence sentence.
The population contains only typed edges with `judge='confirmed'`; deployments
without confirmation yield an empty or partial population. The snapshot reads
edge identities first, then loads evidence only for sampled rows.
The sample uses proportional domain allocation with largest remainders and is
repeatable for the same snapshot and seed. Scoring reports `ok/(ok+wrong+noise)`
and a Wilson 95% interval; skipped and unlabelled rows do not enter the denominator.
If no rows are scored, the command prints `no scored rows` and exits 2.
Audit files contain graph-derived text; store them with the source data.

### Graph quality report

```sh
gkg quality --json
gkg quality --domain acme --seed domains.json
gkg quality --json --seed - < domains.json
gkg quality --hygiene-sample 50 --sample-seed 1 --out hygiene-sample.json
```

`quality` reads done documents and their derived data in batches of 5,000,
without Bootstrap, graph writes, or provider calls. JSON uses `metrics: 2`;
undefined metrics are `null` (`n/a` in text). Candidate counts are `null`
before extraction schema v4. `--seed` uses the longest matching prefix of the
full document source, overriding its stored domain when matched. A domain
filter applies to documents before metrics are computed.

All populations below are selected done documents and their derived data.
Typed facts are stored entity-to-entity edges with valid document and chunk
provenance; a triple is a distinct directed (head, relation, tail).
Duplicate groups normalize names with NFKD, remove company suffixes using the
hygiene rule, then lowercase and keep letters and digits.
Groups form within domains and, for document-scoped keys, within documents.

| Report key | Definition and denominator |
| --- | --- |
| `metrics` | Metric definition version, currently 2 (2 shares the hygiene company-suffix rule; version 1 reports are not comparable on duplicate groups); no denominator. |
| `documents` | Selected done documents; count. |
| `chunks` | Distinct chunks of selected documents; count. |
| `entities` | Entities mentioned in selected chunks or incident to selected typed facts; count. |
| `mentions` | Distinct entity–chunk pairs in selected chunks; count. |
| `isolated_share` | Entities with no incident typed fact / `entities`. |
| `singleton_share` | Entities mentioned in exactly one distinct chunk / `entities`. |
| `single_document_share` | Entities supported by mentions or typed facts in exactly one selected document / `entities`. |
| `mention_gini` | Gini of distinct chunk counts per entity, including zeros: sum of absolute pairwise differences / (2 × `entities` × `mentions`). |
| `top1pct_mention_share` | Mentions of the top ceil(1% × `entities`) entities / `mentions`. |
| `cross_domain_entities` | Count of entities supported in multiple resolved domains; divide by `entities` for a share. Null under `--domain` or when no selected document has a resolved domain; unresolved documents are excluded from this metric. |
| `domain_coverage` | Selected documents with a resolved stored or seed domain / `documents`; null when there are no documents. |
| `fact_components` | Undirected connected components among endpoints of typed facts, including self-loops; count. |
| `giant_share_of_fact_nodes` | Entities in the largest fact component / distinct typed-fact endpoints. |
| `giant_share_of_entities` | Entities in the largest fact component / `entities`. |
| `facts` | Object containing the following stored fact counts; no denominator. |
| `facts.typed` | Stored typed fact edges, including repeated triples; count. |
| `facts.candidates_unjudged` | FactCandidate nodes with no judge verdict in selected documents; count, null before v4. |
| `facts.candidates_unclear` | FactCandidate nodes with judge verdict `unclear` in selected documents; count, null before v4. |
| `facts.rejected` | RejectedFact nodes in selected documents; count. |
| `low_confidence_share` | Typed edges whose stored `low_confidence` flag is true / `facts.typed`, independent of numeric confidence. |
| `confidence_lt_0_4_share` | Typed edges whose numeric confidence is strictly below 0.4 (missing confidence treated as 0) / `facts.typed`. |
| `support` | Object describing support per distinct triple; no denominator. |
| `support.triples` | Distinct directed (head, relation, tail) triples; count. |
| `support.one_chunk_share` | Triples supported by exactly one distinct chunk / `support.triples`. |
| `support.multi_document_share` | Triples supported by at least two distinct documents / `support.triples`. |
| `relation_types` | Distinct relation types among typed facts; count. |
| `relation_entropy_bits` | Shannon entropy −Σ p log₂ p in bits, with p = stored edges of a type / `facts.typed`. |
| `max_entropy_bits` | log₂(`relation_types`), the maximum relation entropy; null with no types. |
| `multi_type_pairs` | Unordered entity pairs {head, tail} with more than one relation type over distinct triples; count, including self-pairs. |
| `inverse_pairs` | Distinct triples (h,r,t) with a reverse (t,r,h), counted once with h < t; count. |
| `self_loops` | Distinct triples (h,r,h); count, regardless of repeated supporting edges. |
| `duplicate_groups` | Folded-name groups containing at least two distinct entities within their domain/document scope; count. |
| `duplicate_entities` | Distinct entities belonging to any duplicate group, counted once across groups; count. |
| `cross_type_groups` | Duplicate groups containing more than one entity type; count. |
| `noise_rule_share` | Entities whose name the hygiene policy drops / `entities`. |
| `co_mention` | Object describing unordered entity pairs sharing a chunk; no denominator. |
| `co_mention.nodes` | Entities incident to at least one co-mention pair; count. |
| `co_mention.edges` | Distinct unordered co-mention pairs, weighted by distinct shared chunks; count. |
| `co_mention.weight1_share` | Co-mention pairs sharing exactly one distinct chunk / `co_mention.edges`. |
| `co_mention.giant_share` | Nodes in the largest undirected co-mention component / `co_mention.nodes`. |
| `hygiene_policy` | Text identifying the hygiene policy used for noise and samples; no denominator. |

Co-mention pairs are uncapped; memory grows with Σ(entities per chunk)².

Noise and hygiene samples use the **built-in hygiene preset** plus the configured
`ingest.stoplist_file` overlay, passing each entity's `type` as its label. The
built-in preset has no value labels, so type exemptions do not apply to
`noise_rule_share`. This policy is recorded in the report. Sampling is
deterministic for the seed and writes up to N dropped and N renamed names, with original name, normalized name,
and rule (`normalize` for renamed names). The sample file records available
counts and uses mode 0600. Names go only to `--out`; stdout contains aggregates.

## 7. Operations

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

### Domain-scoped reads

`GET /v1/domains` returns `[{id, name, documents}]` for the UI pickers;
`documents` counts done documents, and empty domains remain listed. Search and
Ask offer independent domain pickers, defaulting to all domains. Search hits,
document list/detail responses, entity views, related entity references in facts,
co-mentions, and traversal nodes carry `domain`. The API accepts `domain` on
`POST /v1/search`, `POST /v1/agent/ask` and `/v1/agent/ask/stream`, and as a
query parameter on `GET /v1/documents`. Slugs use `[a-z0-9-]{1,40}`; empty
means all domains. MCP `search`, `get_entity`, `get_chunks`, `traverse`,
`list_documents` and `status` also accept `domain` (there is no MCP ask tool).

An ask's domain applies to every answer tool; model arguments cannot widen it.
An entity or traversal start outside scope is not found; `get_chunks` reports
out-of-scope ids in `missing`. Cypher is not domain-enforced; the prompt states
the scope. At schema v4, name resolution uses `EntityKey` folding (including
possessives and edge punctuation), covering document-scoped entities; older
schemas keep case-folded display-name matching. Unscoped ambiguous names
return `candidates`, one per domain. Text filters run before ranking and the
text engine's hit cap. Scoped vector search over-fetches 5× then post-filters,
so it can return fewer than the requested number of hits.

`gkg eval-retrieval --questions questions.json --domain zorbo` applies that
scope to all questions, including gold resolution and null checks. Without
`--domain`, each question's optional `domain` is used.

## Domain administration

Schema v4 domain mutations run inside `gkg serve` through loopback-only admin
routes. They require the configured `GKG_API_KEY` bearer when set and are blocked
at the edge. Read commands connect directly to the configured graph:

```sh
gkg domain list
gkg domain show acme
gkg domain evaluate
# Run mutations inside the running gkg container, on the box using the same
# Compose project as the deployed stack:
docker compose exec gkg gkg domain rename acme "Acme-Max"
docker compose exec gkg gkg domain move document-id zorbo --op-id acme-move
docker compose exec gkg gkg domain merge acme zorbo --op-id acme-merge
docker compose exec gkg gkg domain undo --op-id acme-move
```

Run Compose from the deployment directory, or pass its existing project name
with `docker compose -p <project> exec …`. The CLI defaults to
`http://127.0.0.1:<port in server.addr>` inside the container (`:8080` in Compose).
`--server` overrides that URL. Host requests through the published loopback port
arrive from the Docker gateway and are refused with HTTP 403 `loopback_only`.
An unreachable serve returns exit 1.

Moves may create a valid new domain slug. Merges require existing domains and
remove the emptied source. Before any change, the operation checks every pending
member: `done` documents have extraction reset and queued again, while judge
verdict caches survive; `superseded` versions get only their domain properties
updated and stay terminal. Failed or in-flight documents cause HTTP 409
`not_done` listing their IDs, with nothing changed. Rename changes the display
name and ledger without resetting extraction or requeueing documents.

An operation interrupted after a committed move can partially progress: retry
the same request and `--op-id` to continue without resetting documents already
recorded in the ledger. A merge reports `incomplete` with `remaining` document
IDs if concurrent ingestion keeps its source nonempty; retry the merge with the
same `--op-id` to include those members. `completed` means every live reset was
queued and the merge source was removed. Undo requires live members to be `done`
again; superseded members are skipped and listed in `skipped`. It restores live
members' prior partitions through re-extraction under a new operation ID.

`list` includes completed document counts and the nearest other domain, averaging
the existing top-three similarity score over the source domain's profiles.
`show` includes its decision ledger. `evaluate` reports document-weighted
leave-one-out top-choice accuracy, auto-attach share and unseen-domain false-attach
share using stored profiles and configured domain thresholds; it calls no provider.

### Schema migration

Use `gkg preflight` to check a deployment without writing to the graph or spool:
exit 0 means current or empty, 3 means a missing or invalid seed or an
unreadable `GKG_INGEST_STOPLIST_FILE`, and 10 means a v4 migration is ready. Existing documents require a schema-1 source-prefix seed
at `GKG_DOMAIN_SEED_FILE`; `detect-all` is available for development.
`gkg migrate --schema-only` assigns domains, clears derived data and requeues
documents without extracting them. `gkg serve` completes the re-extraction;
plain `gkg migrate` completes it synchronously. Unknown migrate flags fail with
exit 2. After a rebuild, run `gkg judge backfill --budget-calls N` with an
owner-approved budget to recover unjudged candidates.
