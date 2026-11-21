# gkg usage guide

Practical day-to-day usage of **go-knowledge-graph**: ingest knowledge,
explore the graph, and query it from agents. Installation and configuration
live in the [README](README.md) and [quickstart](docs/quickstart.md);
product limits are recorded in [docs/known-limitations.md](docs/known-limitations.md).

## 1. Ingest knowledge

Ingest text, files (Markdown/txt/PDF/DOCX/… — rich formats convert through
docling by default) or URLs. Every submission becomes a
document with full provenance. Within the same source identity, unchanged
content is a no-op and changed content supersedes the previous version.

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

gkg cleans decoded text from Docling, native PDF extraction, Markdown,
plain text, and HTML before hashing or chunking it. It replaces invalid UTF-8,
strips control characters except tabs and line breaks, and removes bidi
embedding, override, and isolate controls (U+202A–U+202E and U+2066–U+2069).
A run of these bidi controls becomes one ASCII space only when both adjacent
characters are non-whitespace, the right character is not a combining mark,
and exactly one side is a right-to-left letter. If the left character is a
combining mark, gkg uses the preceding base character for the letter check.
gkg preserves left-to-right and right-to-left marks (U+200E and U+200F).
gkg repairs a repeated standalone right-to-left glyph when it is the only
distinct lone RTL letter outside fenced code, occurs on at least three lines
there, and the document has no multi-letter RTL token after bidi
normalization. It removes every occurrence of the glyph from a line whose body
starts with it when the line has a Markdown list marker or contained a bidi
control before normalization. It also repairs reversed Markdown list items
where the token before the last glyph starts with an uppercase letter or digit.
gkg collapses long dot leaders after text on the same line. Glyph repair and
leader collapse skip fenced Markdown code blocks. Indented code blocks are
eligible for cleaning.

The Memgraph `:Document` node records `text_clean_version` (currently `1`) and
the counts `text_invalid_utf8`, `text_controls`, `text_bidi_controls`, `text_glyph_lines`,
`text_leader_runs`, `text_fffd_removed`, and `text_fffd_remaining`. An
invalid byte run counts as one replacement. The `gkg_text_clean_total{step}`
metric adds these counts on each admitted submission, including force
resubmissions. Duplicate no-op submissions and reloads skip the increment.

Existing documents keep their old chunks until you resubmit the original
input under the same source identity. If the cleaned hash changes, gkg
creates a new version and supersedes the old one when processing succeeds.
Text submitted without a source gets its source identity from the content
hash. To supersede an existing text version, pass the same explicit source
on resubmission. Use a forced REST or MCP resubmission if the cleaned hash
is unchanged and you want to reprocess the document. The admin rebuild uses
stored chunks, so it cannot apply text cleaning to old content.

A queued submission from before this change can still use its original
spooled text if it matches the stored hash. Without a spool entry, a queued
URL is fetched and cleaned again. If the new hash differs from the stored
hash, processing fails with `source content changed`, and you must resubmit.
Queued file and inline text submissions without a spool entry also need
to be resubmitted.

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
section_label_keep_types: [location]
stoplist:
  exact: [Descrizione, Informazioni, Indice, Introduzione]
  patterns: ['^Sezione \d+$', '^Section \d+$']
code_tokens: true
```

`section_label_keep_types` lists entity label names whose mentions are exempt
from `heading_lead` during ingest and `gkg repair-labels`. Matching is
case-insensitive. Omit the key (or leave it empty) to use `[location]`; write `[]`
to apply `heading_lead` to all types. The `lead_in` rule still applies to every type.
`gkg preset check` and `gkg eval -replay` reject a name that is not an entity label
of the preset; ingest and `gkg repair-labels` ignore such a name.

Entity labels with `scope: document` are value labels: they skip the no-letter,
short-name, CLI-flag, path and code shape rules, while normalization and newline,
length, markdown-link, heading and stoplist checks still apply.

A custom preset can include an optional top-level `validity:` block with
`rules: catalog-v3` and a lexicon for catalog validity planning. Supply a
nonempty `product_types` map (for example, `{zorbs: zorb}`) and nonempty
`stop_tokens`, `quality_nouns`, `class_nouns`, `evaluative_stems`,
`evaluative_words`, `sensory_stems`, `sensory_words`, and `function_words`
lists. Use lowercase ASCII letters for stems and lowercase ASCII letters or
digits for other tokens. `gkg preset check` validates the block. Omit it to
leave validity rules off. An invalid `validity:` block fails every preset
loader, including those used by ingest, eval, and rejudge planning.

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

`gkg` is an MCP server (stdio and streamable HTTP) exposing
`ingest`, `status`, `search`, `get_entity`, `get_chunks`, `traverse`, `list_documents`,
`cypher`, `get_product`, `find_products`, and `list_features`. It also exposes
`ask` when `GKG_AGENT_ENABLED=true`. See
[docs/agent-setup.md](docs/agent-setup.md) for OpenCode and Claude Desktop
configuration examples.

- **stdio**: run `gkg mcp` with `GKG_MEMGRAPH_URI` (+ credentials) in the
  environment — the agent host starts the process.
- **streamable HTTP**: `POST /mcp` on the `serve` surface; set
  `GKG_API_KEY` to require `Authorization: Bearer <key>`. Through the edge,
  use `https://<host>/mcp` with `Authorization: Basic <base64 of login:password>`.

Prefer `ask` for natural-language questions. Pass `question` and optionally
`domain` to get a Markdown `answer` and `steps` in one call. Each call uses the
server's LLM budget and the agent timeout. Request `include_evidence: true`
to add evidence within a 32 KiB budget for one JSON copy of the result.
`truncated: true` means evidence was shortened or omitted. An answer that
exhausts the budget is returned in full with no evidence.
Use `search`, `get_entity`, `get_chunks`, and `traverse` for targeted lookups.

Example prompts:

- "Ingest <url> and tell me when it is done."
- "Search for 'Widget Pro' and summarise the top chunks with their sources."
- "Show everything the graph knows about Ada Lovelace, with citations."
- "Traverse from `ada lovelace` two hops and list the evidence."

## 4. Ask the graph (in-UI agent)

With `GKG_AGENT_ENABLED=true` the web UI grows an **Ask the graph** panel
and `POST /v1/agent/ask` answers natural-language questions server-side: an
OpenAI-compatible model plans and runs the graph read tools —
search, entity cards, chunks, traversal, documents, status, guarded Cypher,
`get_product`, `find_products`, and `list_features`. The response carries the
tool results as `evidence`, so every answer is checkable. Answers use
GitHub-flavoured Markdown. The `answer` field gives the source text and
`answer_html` gives server-sanitized HTML, shared with the document reader.
The renderer omits raw HTML and images. Links follow the
reader’s policy. The UI falls back to plain text if `answer_html` is absent.
The browser never sees the provider key.

Product searches default to 10 hits and cap the requested limit at 20. The
byte budget can shorten a page or omit hit evidence. Pass the returned cursor
with the same filters and effective limit to continue. Product detail preserves
the associated pages and reports dropped feature observations in
`features_omitted`. The feature list shows IDs, names, release status and
numeric-search support without source-key rows. Use its case-insensitive
`query` over feature IDs and names to narrow a result marked `truncated`.
The `omitted` count reports how many matching features were dropped. A
product tool returns an error if its required metadata exceeds the budget.

Open **Generation settings** before asking a question. Temperature ranges
from 0 to 2. An ask without a temperature uses the deployment setting, which
defaults to 0. Top-p ranges from 0 to 1 and uses the provider
default when blank. Sampling top-k accepts a positive integer when the
operator enables `GKG_AGENT_TOP_K_ENABLED=true` after confirming provider
support. It controls token sampling. The graph search result limit is separate.

`GKG_AGENT_REASONING_EFFORTS` sets the allowed reasoning choices for the
deployment. Without that setting, the `DeepSeek-V4.1-Flash-1017` deployment
name offers **Provider default**, **None**, **High** and **Max**. The default leaves the
choice to the model. Each provider determines which sampling parameters take
effect in a given reasoning mode. The form keeps your choices until reload.

JSON and streaming clients can send the same optional settings, for example
`{"question":"Who owns Zorbo?","generation":{"temperature":0.3,"top_p":0.95,"reasoning_effort":"high"}}`.
They apply to every provider turn for that question. After a completed answer,
**Download Markdown** saves its original Markdown locally as `gkg-answer.md`.

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
sends a `: ping` heartbeat every ~15s. With GCP auth, the stream obtains its first
token before sending SSE headers. Token acquisition failure at that point returns
JSON with HTTP 502. Once streaming starts, authentication failures arrive as
`error` events. JSON asks return HTTP 502 for authentication failures. Both
surfaces report `auth_permanent` or `auth_transient` in the `error` field.

```sh
curl -N -s localhost:8080/v1/agent/ask/stream -H 'Content-Type: application/json' \
     -d '{"question":"What does the graph know about Ada Lovelace?"}'
```

`GET /v1/agent/status` reports the capability as
`{"enabled":true,"stream":true,"generation":{"top_k":false,"reasoning_efforts":["none","high","max"]}}`
for that deployment. An empty `reasoning_efforts` list disables that control. With `GKG_AGENT_STREAM=false` the stream
endpoint still answers — SSE framing, tool and done events only, no deltas —
and the panel uses the JSON endpoint; a provider that does not actually
stream is detected automatically (with default generation settings the
agent falls back to non-streamed answers for the process lifetime; an ask
with explicit generation settings negotiates its own fallback and does not
change what later asks remember).

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

Set `EDGE_MAINTENANCE_MODE=1` and recreate the edge service to pause writes
during maintenance. Both edge listeners return HTTP `503` with
`{"error":"maintenance"}` for `POST`, `PUT`, `PATCH`, and `DELETE` other than
`/healthz`. This also pauses POST-based search and asks from the UI while
pages and GET requests remain available. Set the flag to `0` (the default)
and recreate the edge to restore write access.

While the agent is disabled the endpoint
answers `501` and the panel stays hidden. Answers are LLM-generated — the
`evidence` array is the source of truth
([known limits](docs/known-limitations.md)).

### Judge and agent provider configuration

`judge.auth` and `agent.auth` select authentication independently. The default
`static` sends the role's configured API key as a bearer credential when a key
is present. On Compute Engine, `gcp` obtains OAuth access tokens for the VM's
default service account from the metadata server. Give the service account
Vertex access in the configured project. The GCP credential type is
`metadata` in this release.

Both GCP roles use the approved `gcp.route` tuple. For each role using it, clear
its API key and set its base URL to
`https://<host>/v1/projects/<project>/locations/<location>/endpoints/openapi`.
A trailing slash is accepted. Configuration is checked after YAML and
environment overrides, and the GCP clients reject redirects. See the
[Vertex configuration example](docs/quickstart.md#vertex-ai-on-compute-engine).

| YAML settings | Environment overrides | Behavior |
| --- | --- | --- |
| `judge.auth`, `agent.auth` | `GKG_JUDGE_AUTH`, `GKG_AGENT_AUTH` | `static` or `gcp` |
| `gcp.credentials` | `GKG_GCP_CREDENTIALS` | `metadata` |
| `gcp.route.host`, `project`, `location` | `GKG_GCP_ROUTE_HOST`, `GKG_GCP_ROUTE_PROJECT`, `GKG_GCP_ROUTE_LOCATION` | Approved HTTPS route tuple |
| `judge.temperature`, `agent.temperature` | `GKG_JUDGE_TEMPERATURE`, `GKG_AGENT_TEMPERATURE` | Default 0. An explicit ask temperature takes precedence over the agent deployment setting. |
| `judge.max_tokens`, `agent.max_tokens` | `GKG_JUDGE_MAX_TOKENS`, `GKG_AGENT_MAX_TOKENS` | Omitted by default. The agent limit applies to each provider turn. |
| `judge.reasoning_effort` | `GKG_JUDGE_REASONING_EFFORT` | Omitted by default. |
| `agent.reasoning_efforts` | `GKG_AGENT_REASONING_EFFORTS` | YAML list or comma-separated env value. An explicitly empty list disables the capability. |

The judge uses `json_object` as its response format. Judge v1 requests use a
separate cache identity for GCP auth or non-default generation settings. The
identity includes the base URL without its query and the generation settings.
Restoring static auth and default settings restores the original v1 cache keys.
V3 rejudge and validity planning require static judge auth and retain their
frozen generation settings.

Online ingest leaves a fact unjudged when verification fails. With GCP judge
auth, `judge_failures_total` labels Vertex HTTP 401/403 and missing service
accounts as `auth_permanent`, metadata timeouts, server failures and malformed
tokens as `auth_transient`, and other judge failures as `provider`.

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

If a sidecar relation endpoint matches several returned entities and the model
cannot identify one, the sidecar reports an `ambiguous_relation` diagnostic
with candidate entity indexes for both endpoints. Ingest ignores diagnostics
and builds facts from resolved relations.

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

### Sample external text documents

```sh
gkg eval-sample -from documents.jsonl -domain zorvax -out sample.jsonl
gkg eval -replay sample.jsonl -predictions predictions.jsonl -labels general
```

Supply at least one JSON object, one per line, with nonempty `id`, `text`,
`product_id` and `kind`. You can also supply `source` and `title`. Document IDs
must be unique and have no leading or trailing whitespace. Text must contain
non-whitespace content. The command applies ingest's inline text loading, title
and source resolution, and configured chunk size and overlap. It carries
`product_id` and `kind` as document metadata.
Unknown fields and blank lines are errors. Invalid rows report their line
number and exit 2 without creating the sample.

The sample header records `origin: "from-file"`, `duplicate_links: "none"`,
the input file's SHA-256 and the domain. Replay always extracts file samples
locally, chunk by chunk. The command creates a new file with mode 0600 and
refuses to overwrite an existing file. File sampling needs no Memgraph connection.
`-docs` and `-seed` are for graph sampling. Replay still requires the
configured Memgraph schema and sidecar.

### Sample stored chunks and replay extraction

```sh
gkg eval-sample -domain zorvax -out sample.jsonl -docs 30 -seed 42
gkg eval -replay sample.jsonl -predictions predictions.jsonl -labels general
```

`eval-sample` opens the configured Memgraph for reads and exports done
documents in the requested domain with their titles, sources and verbatim
stored chunks. The document's `labels` field records the preset name used
at ingest. An empty string means built-in labels. Omit `-docs` or use `-docs 0`
to export the whole domain.
Sampling uses the document IDs and seed, which defaults to 0. A chunk's
`duplicate_of` and `dup_doc_id` identify the effective root and its owning
document, even when the root is outside the sample. Both fields are empty
when ingest would extract the chunk itself. A root in another document is
eligible for borrowing only if that document is `done` and in the same
domain. The export flags cyclic chains with `duplicate_root` set to `cycle`
and treats those chunks as roots for extraction. Standard output contains
document and chunk counts.

Replay verifies the sample's header, counts and digest and reads the
configured Memgraph's extraction schema marker without changing the graph.
Replay requires the binary's current extraction schema and exits 1 for an
older or newer schema before contacting the sidecar at
`extractor.sidecar.url`. Replay uses the selected preset or the built-in labels
and policy when `-labels` is omitted.
`-golden`, `-sweep`, `-out` and an explicit provider other than `sidecar`
are usage errors (exit 2).

Predictions include the input and preset hashes, effective hygiene and
confidence floors, ingest's `write_mode` (`hygiene`, `domains` and
`facts_as_candidates`), the sidecar's health-reported model when available,
the extractor version string and the gkg version. `sidecar_image` records
the configured `extractor.sidecar.image` (`GKG_EXTRACTOR_SIDECAR_IMAGE`).
`sidecar_image_digest` records the SHA-256 digest from a pinned image
reference or a bare `sha256:` digest. When the digest is unavailable,
replay records `unknown` and warns on standard error.
The header's `label_mismatches` lists documents whose stored `labels` differ
from the selected preset. Each chunk has one result row with the document's stored
`labels` and a `label_mismatch` flag. Older samples without stored labels
show `null` for `labels`.

Replay checks sidecar health once for a nonempty sample and batches each
document's nonduplicate chunks (every chunk with `-no-borrow`) in sequence
order as ingest does. In replay,
each batch must return exactly one result for every requested chunk ID.
Missing, duplicate or unrequested IDs fail the batch, which is retried one
chunk at a time. Predictions for chunks with zero extracted relations are
`empty`. Each relation occurrence
has the original endpoint text and chunk-relative byte offsets, cleaned names
and entity keys, endpoint types, relation, model label text, confidence,
evidence, document title and disposition. `stored_winner` marks the occurrence
selected by ingest's per-chunk storage rules before judging. With
`facts.require_confirmation=true`, the winner would be stored as a candidate.
With normal borrowing, duplicates borrow occurrences from their root in the
sample, with `source_chunk_id` identifying the chunk the evidence and offsets
come from. Their winner flags are false. A duplicate whose root is outside the
sample is `empty` after a successful health check and has `duplicate_root` set
to `outside_sample`; exclude those rows from the fidelity comparison. With
`-no-borrow`, every duplicate (including one whose root is outside the sample)
is extracted locally: its rows carry their own occurrences and winner flags,
no `source_chunk_id`, and `duplicate_root` stays informational. A cyclic
chain is flagged as `cycle`: `eval-sample` exports it without
`duplicate_of`, and replay extracts it as ingest does, so its occurrences
stay comparable. With normal borrowing, the `empty` count includes `outside_sample` rows; with
`-no-borrow` those rows are extracted locally and counted by their own status. The replay
summary on standard output and the JSONL trailer count the flagged rows in
`duplicate_roots_outside_sample` and `duplicate_root_cycles`.
A kept relation below the assert floor is marked `low_confidence`.
If any chunk still fails after retry, or the sidecar fails its health check,
replay preserves the completed predictions with per-chunk error rows
and exits 1.

Both JSONL commands create new files with mode 0600. Each trailer's SHA-256
covers all preceding lines, including the header and line endings. The replay
header's input SHA-256 covers the entire sample file, including its trailer.
Named presets are hashed as file bytes. Built-in labels are hashed as
serialized defaults and the effective hygiene policy is recorded separately.

#### Validated mentions for scoring

```sh
gkg eval -replay sample.jsonl -predictions predictions.jsonl -labels general \
  -mentions mentions.jsonl -no-borrow
```

`-mentions` and `-no-borrow` require `-replay`. Use a distinct path for
each file. Replay reserves both outputs with mode 0600 before contacting
the sidecar. An existing output is a usage error (exit 2). A fatal replay
error removes both outputs. Per-chunk errors leave both files available
with error rows and exit 1.

`-no-borrow` sends every chunk, including duplicates, to the sidecar and
computes each chunk's winners locally. Prediction occurrences then omit
`source_chunk_id`. The `duplicate_root` field still records root issues.
With normal borrowing, the mentions file has a chunk row for every sample
chunk. A chunk that borrows from a sampled root has status `borrowed` and
records the root ID in `source_chunk_id`. It has no `entity`, `relation`,
or `ambiguous_relation` rows. If its root failed, the chunk has status
`error`. Only `borrowed` chunk rows include `source_chunk_id`.

The file uses the following JSONL rows.

- `header` records `format: "gkg-mentions/v1"`, `no_borrow`, and
  `offsets: "utf8_bytes_chunk"`. The shared 32-character hexadecimal
  `attempt_id` links this file to its predictions. The header also records
  `input_sha256`, `preset`, `preset_sha256`, `hygiene`, `write_mode`,
  `store_floor`, `assert_floor`, `sidecar_model` when available,
  `sidecar_image`, `sidecar_image_digest`, `extractor_version`, and `gkg_version`.
- `chunk` rows follow sample file order and include `chunk_id`, `doc_id`,
  any raw JSON `product_id` and `kind`, `status` (`ok`, `empty`, `borrowed`,
  or `error`), `error`, and counts for `entities`, `relations`, and
  `ambiguous_relations`. A chunk without borrowed evidence or an error
  is `empty` when all three counts are zero.
- `entity` rows follow each chunk row in validated index order. They carry
  `chunk_id`, `index`, `label`, `text`, `start`, `end`, `score` and
  `hygiene`. A `kept` outcome includes `name`, `aliases`, and `key`.
  A `dropped` outcome includes its `rule`.
- `relation` rows follow entities in candidate index order. They include
  `chunk_id`, `index`, `label`, `score`, `head` and `tail` (each with
  `index`, `start`, `end`, and `label`), `disposition` (`kept`,
  `floor:store_floor`, or `hygiene:<rule>`), `low_confidence`, and
  `stored_winner`.
- `ambiguous_relation` rows follow relations and have `type` set to
  `"ambiguous_relation"`. They carry `chunk_id`, `index`, `label`, `score`,
  `head_candidates`, and `tail_candidates`. The `index` is the diagnostic's
  zero-based position in the full diagnostics list. The candidate lists
  use the same validated endpoint fields as relation rows.
- `trailer` carries `sha256` and `counts` with `chunks_requested`,
  `chunks_ok`, `chunks_empty`, `chunks_borrowed`, `chunks_error`, `entities`,
  `relations`, `ambiguous_relations`, and `complete`. The file is complete
  when `chunks_error` is zero and `chunks_ok + chunks_empty + chunks_borrowed`
  equals `chunks_requested`. For scoring, use complete mentions files and
  check the attempt ID against each corresponding predictions file.

The header's `labels_sha256` is the lowercase hexadecimal SHA-256 of the
exact bytes of the `labels` JSON value as emitted in the mentions header line.
Verify it by hashing that substring directly and skipping re-serialization.

For reference, Go's `encoding/json.Marshal` encodes
the object with `entities` then `relations`, arrays in preset order
(empty arrays are `[]`), and keys in the order `name`, `text`,
`threshold`, then `scope` for entities. The `text` field is the model
label text, falling back to `name`. The `scope` field is present even when
empty. This encoding uses Go's default number and string encoding,
including escaping `&`, `<`, `>`, U+2028, and U+2029.

### Validate a preset

```sh
gkg preset check labels/general.yaml
gkg preset check labels/general.yaml -stoplist /path/to/stop-words.txt
```

The check and replay use the same strict schema for extraction labels and name
hygiene. Pass `-stoplist FILE` to apply the same stoplist overlay as ingest's
`ingest.stoplist_file`. The check and replay reject unknown keys, ambiguous
sidecar aliases, relation names that sanitize to the same type, and invalid
hygiene values. Thresholds must be finite and in (0, 1], with up to 30 entity
and 15 relation labels and at most 60 characters in each label sent to the
model. Ingest continues to accept unknown keys and logs a warning for each.

### Judge backfill and audit

`judge.store_inputs` defaults to `false`. Enable it on deployments with
`GKG_JUDGE_STORE_INPUTS=true` so an older binary can ignore the setting after
a rollback. It works with the four judge modes below and is incompatible with
`facts.require_confirmation`.

| `judge.mode` | Online verification | `store_inputs: false` | `store_inputs: true` |
| --- | --- | --- | --- |
| `off` | None | No typed-edge backfill | Stores requests for all new typed edges |
| `low-confidence` | Facts below `judge.verify_floor` | No typed-edge backfill | Keeps requests for skipped edges and failed judgments |
| `sample` | Deterministic sample at `judge.sample_pct` | No typed-edge backfill | Keeps requests for skipped edges and failed judgments |
| `all` | All facts | No typed-edge backfill | Keeps requests for failed judgments |

Only typed edges written with input storage enabled are backfillable. An
explicit admin rebuild re-extracts chunks and replaces their facts and rejection
audits. The backfill selects edges with an empty verdict and a stored request
on a `done` document. Confirmation and unclear verdicts remove the stored
request from the edge. Refutation removes the edge and writes a rejection audit.

Online judging and both backfill modes record `judge_model` on confirmed and
unclear typed edges, unclear candidates, and rejection audits from refuted
verdicts. Promoted candidates carry the model on the resulting edge. Cache hits
use the cached verdict's model. Historical verdicts with unknown provenance
lack `judge_model`. When the sealed rejudge run recorded a model,
`repair-validity -apply` writes it on stamped, demoted, and rewritten edges and
on rejection audits; otherwise an existing `judge_model` is left as it was.

Run the typed-edge backfill inside the running service container so it uses the
same judge model and cache as ingest.

```sh
docker compose --env-file .env -f deploy/docker-compose.yml exec -T gkg gkg judge backfill --edges --budget-calls 120
```

`--limit` caps the number of edges selected. `--domain` filters their documents
and requires extraction schema v4. The judge URL must be a valid HTTP(S) URL.
An empty API key is allowed for endpoints without authentication. Each provider
attempt, including retries, counts toward the budget. Cache hits use no
attempts. A rerun can apply a previously obtained verdict without another call
when its cache write succeeded and the same request and model are still in the
cache. Provider-wide failures stop the run with `stopped_provider=true` and
leave the edge pending. The command exits `2` on `auth_permanent` and `3` on
other provider stops, including `auth_transient`. Provider stops include HTTP
401, 403, 404, 405, 415, 429, and 5xx, and network errors and timeouts. Row-specific failures such as HTTP 400,
an undecodable response, or an unknown verdict leave the edge pending for a
later run and allow this run to continue.

Edge backfill and rejudge exclude each other. `--force` can take over a
`judge-backfill` lease. A live `repair-validity` or `judge-rejudge` lease still
blocks edge backfill. These exclusive lease acquisitions and renewals, also in
`gkg judge rejudge` and `repair-validity -apply` runs that never use edge
backfill, increment a counter on one `(:GkgMeta {id:'judge_lease_fence'})` node
to serialize them. Its `seq` is not a version and may be ignored. Treat edge
backfill as a graph writer during a quiescent validity repair. Stop the timer,
let the active backfill finish, and verify no backfill process or live lease
remains before `repair-validity -classify`.
Keep it disabled through the post-apply decision. Pending opted-in edges remain
in the rejudge and validity catalogs (`judge IN ['confirmed','']`). Rejudge
before-images include `judge_input` in the saved edge properties. After the
window, validity-deferred edges with stored requests are eligible for backfill.

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
Rerunning recovers verdicts whose cache write succeeded before application,
for the same request and model.
Provider-wide failures leave the affected candidate pending. Candidate backfill
exits `2` on `auth_permanent`, `3` on other provider stops including
`auth_transient`, and `1` on other run errors. Per-candidate errors are logged
and counted as `failed`, and judging continues. An unchanged candidate
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

### Evaluate judge v2

Run each labelled set separately, using one output directory per set. For the
corrected gkg evaluation file, relabel the six markup-only `noise` rows as
`ok` and record that correction with the audited labels. This gives 175 ok and
25 wrong or noise rows for gkg. Use the bc holdout separately (149 ok and 51
wrong or noise). Run the evaluation before applying `repair-labels`, while the
labelled edges still exist.

```sh
mkdir -p eval-gkg
gkg judge rejudge -plan -ids labelled.jsonl -out eval-gkg/manifest.jsonl
gkg judge rejudge -judge -manifest eval-gkg/manifest.jsonl \
  -budget-attempts 240 -out eval-gkg/verdicts.jsonl
```

Planning makes zero provider calls. It selects typed edges with a confirmed or
empty judge status. `-domain` scopes the selection. Use `-ids` to restrict it to
the audit file, or `-limit N` to cap an unlabelled plan. Planning fails when any
requested id is missing or unverifiable. Reconciliation accepts either the
entity name or the chunk surface for each endpoint and checks the relation
and head sentence. Planning lists and excludes mismatches, and fails if zero
rows remain. Blank and `skip` labels are excluded from scoring.
Inspect the cache-adjusted call estimate and per-relation table-header counts
before approving spend. The plan refuses to overwrite a manifest.

Use the same non-empty manifest and verdict file to resume. The verdict log is
read before the cache. Each verdict must carry the matching frozen request key.
The sibling `attempts.json` ledger stores the cumulative cap and debits
attempts before calls, including retries.
The summary shows the stored cap and this invocation's effective budget.
Set `-raise-cap` explicitly when authorizing a larger cap. Keep all three private
files together for the run. The judge stops on cache errors, preserving a paid
verdict in the verdict log if the cache write fails.

When every manifest row has a verdict and the judge finishes without error,
it atomically records `completed: true` and `verdicts_sha256` in `attempts.json`.
Budget stops and runs that return a cache error leave the ledger unsealed.
A sealed resume verifies the SHA-256 of the exact verdict file bytes and reports
the saved results with `sealed=true` in the summary, without calling the provider
or changing the run files. Other invocations report `sealed=false`. A digest
mismatch fails the resume before any spend.

Each preset has its own profile fingerprint, based on its full relation
definitions, the model, the prompt and the request schema. The same frozen
request keeps its fingerprint and cache key when other presets are added to a
manifest. Regenerate mixed-preset manifests created with a shared fingerprint.
Each manifest row also records the prompt profile version (currently 3). The
judge rejects rows planned with another version, so plan again after an
upgrade that changes the prompt.
The judge prints one `preset=… fingerprint=…` line per preset, with `preset=""`
for the built-in definitions. For labelled runs these lines precede the gate.
At least 95% of the scored audit rows must reconcile, every planned row must
have a verdict, at least 98% of ok rows must be confirmed, and at least 95%
of confirmed rows must be ok. If
reconciliation is below 95%, the judge reports the failed gate before spending
any attempts. The table includes the Wilson 95% interval, distinct facts and
domain splits. It shows reconciled and judged totals separately and lists lost
ok and kept bad ids with reasons. Apply is not part of this command yet.

| Exit code | Meaning |
| --- | --- |
| `0` | Success. |
| `1` | Operational error such as an invalid manifest, graph failure, or a plan with zero rows when `-ids` is absent. |
| `2` | Invalid arguments or audit input, including an invalid label, an empty or duplicate ID, a requested ID with no eligible or verifiable edge, or a `-ids` selection that leaves zero rows to plan. GCP judge auth for v3 rejudge and permanent authentication stops also map to this code. |
| `3` | The labelled gate failed or the run stopped on another provider-wide failure. |
| `4` | The run ended before all planned rows had verdicts and had no scored gate. This includes runs whose labels are all `skip` or blank. |

### Catalog validity repair planning

Use a preset with a `validity:` block. The default preset is `catalog`.
Classification and planning read Memgraph. Finalization works offline. The
two `judge rejudge -judge` invocations are separate budgeted runs.

```sh
mkdir -p bulk-run rewrite-run
gkg repair-validity -classify -out inventory
gkg judge rejudge -plan -ids inventory/planned-ids.txt -out bulk-run/manifest.jsonl
gkg judge rejudge -judge -manifest bulk-run/manifest.jsonl \
  -budget-attempts "$BULK_BUDGET" -out bulk-run/verdicts.jsonl
gkg repair-validity -plan -manifest bulk-run/manifest.jsonl \
  -verdicts bulk-run/verdicts.jsonl -out draft
gkg judge rejudge -judge -manifest draft/rewrites.jsonl \
  -budget-attempts "$REWRITE_BUDGET" -out rewrite-run/verdicts.jsonl
gkg repair-validity -finalize -plan draft -rewrite-verdicts rewrite-run/verdicts.jsonl
```

Pass the same `-preset NAME` to classify and plan when selecting another
preset. `planned-ids.txt` contains ID-only JSONL for the rejudge selection.
Ambiguous logical keys and unverifiable edges are recorded in the inventory.
A missing document is attributed to the chunk's recorded owner when available.
Orphans with no attributable owner are reported for any selected preset.
Planning rescans the inventory and checks the planned IDs, prior judges and
frozen requests against the sealed bulk run. Each verdict file needs its
completed sibling `attempts.json`.

For a draft with zero candidates, skip the rewrite run and use
`gkg repair-validity -finalize -plan draft -no-rewrites`. Read the projected
counts in `plan.json` and the final counts and digest in `final-plan.json`.
`affected.jsonl` lists sources with missing or ambiguous targets and sources
involved in collisions. Each row includes the source `doc_id` and `chunk_id`
and each destination's projected state.
`affected-summary.json` counts sources without a confirmed destination.
Add `-defer-unconfirmed` to either finalize command to mark affected sources
without a projected confirmed destination as `deferred`. The final plan records
the deferral mode, and the affected reports show which sources were deferred
and how many in each group.
With `-defer-unconfirmed`, the `destination_states` and
`has_confirmed_destination` fields in `affected.jsonl` show the projection
before deferral, including how a deferred occupant would appear with the flag omitted.
Artifacts have mode `0600`. Use separate inventory, run and draft directories.
An existing output file or an input mismatch exits `2` before publishing any
outputs. Success exits `0` and operational errors exit `1`.

The inventory, draft and final digests are SHA-256 over the following ordered
tuples. Each component is prefixed with its byte length as an unsigned 64-bit
big-endian integer. Use lowercase hex text for digest components and exact file
bytes for JSONL components, including their final newlines.

- Inventory uses the preset name (the contents of `preset.txt` without the
  final newline), `inventory.jsonl` and `planned-ids.txt`.
- Draft uses the SHA-256 of `source-manifest.jsonl`, the SHA-256 of
  `source-verdicts.jsonl`, `inputs.jsonl`, `rewrites.jsonl`, the rules ID in
  `plan.json` and the SHA-256 of `lexicon.json`. The first input row carries the
  inventory digest.
- Final uses the draft digest, the SHA-256 of `rewrite-verdicts.jsonl` (or the
  literal text `no-rewrites`) and `final.jsonl`.

### Apply an approved catalog validity plan

After the gate and owner approval, stop serve and every other graph writer.
Take and verify a pre-apply snapshot with `make box-snapshot` using the procedure in
[the deployment guide](docs/deploy-aws.md). Keep writers stopped through the
projected-count check, spot check and post-apply audit.

```sh
gkg repair-validity -apply -plan draft -approved-digest "$APPROVED_DIGEST" \
  -quiescent -report validity-lineage.jsonl
```

Apply requires a finalized directory, the approved digest, the sealed preset
rules and lexicon, and a graph with no live judge lease or previous validity run.
Every Document must have a terminal status. A missing status blocks apply.
Apply checks the live inventory and collision inputs before taking the lease
and rebuilds the evidence and requests in each row's transaction. Preflight also
requires rebuilt request bytes for every planned source and judged rewrite
candidate to match the sealed bulk and rewrites manifests. Ambiguous and
unverifiable rows are counted and left untouched.
Apply leaves deferred edges and their properties untouched and records
`deferred` in the lineage report.

Choose a fresh `-report` path. Apply checks that the path is absent during
preflight and creates the `0600` JSONL file after acquiring the lease, before
applying any rows. The report records each source's disposition and reason,
followed by projected and actual outcomes and typed-edge visibility counts.
A stopped or completed apply consumes the report path. Use a new path when
rerunning after a restore.

Restore the pre-apply Memgraph data-directory snapshot to preserve internal
edge IDs, which the sealed occupant `edge_id` values depend on. A logical
dump and import changes those IDs and requires re-planning from `-classify`.

| Exit code | Meaning |
| --- | --- |
| `0` | Apply completed and projected totals equal actual totals. |
| `1` | Operational error, for example failure while writing the report. Check the graph and report before resuming writers. |
| `2` | Usage or precondition failure. The graph and report path are unchanged. |
| `4` | Apply stopped and wrote a stop report. Earlier rows remain committed. A new marker after preflight stops the first row with `validity_run appeared after preflight`. Keep writers stopped and restore the pre-apply snapshot before starting a new apply. |

### Graph quality report

```sh
gkg quality --json
gkg quality --domain acme --seed domains.json
gkg quality --json --seed - < domains.json
gkg quality --hygiene-sample 50 --sample-seed 1 --out hygiene-sample.json
gkg quality --domain acme --duplicates-out duplicates.jsonl
```

`quality` reads done documents and their derived data in batches of 5,000,
without Bootstrap, graph writes, or provider calls. JSON uses `metrics: 3`;
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

`--duplicates-out` creates a new private JSONL file (mode `0600`) and refuses
an existing path. It works independently of the hygiene sample. `--seed`
assigns effective document domains by source prefix, and `--domain` selects
done documents by their effective domain. Global entity keys supported by
those documents are grouped by the domain in the stored `@<domain>:` key
prefix and by name fold. Each group of at least two keys produces one row.
Keys without a valid domain prefix use an empty row domain.

Tier `A` requires a shared valid key domain, one shared non-empty entity type,
at least four letters in the fold, and matching symbol counts (`+ # @ / &`)
and digit runs after NFKD normalization. Names with a dotted version number
such as `1.10`, `v` or `V` immediately followed by a digit, or letters
followed by digits with optional spacing (such as `X100`, `X 100`, or
`Area 51`) fail the version or model guard. Tier `other` records the first
failing guard in `reason`. An absent or mismatched key domain is
`key_domain_mismatch`.

`suggested_canonical` is the key with the most stored mention edges, with ties
going to the smallest key. Each member includes its supporting document count
among selected documents, its stored mentions, confirmed and other typed
edges, candidates and rejected facts, and up to two evidence sentences from
selected documents, capped at 200 bytes each. Edge counts cover all stored
incident data for each member. A link between two members contributes to both
counts. The stdout summary gives tier counts and the maximum and nearest-rank
p95 rewire sizes. With `--json`, the single-line report carries the same tier
counts and sizes in `duplicate_census` (`groups` and `members`, each `{A,
other}`, plus `max_group_size` and `p95_group_size`). Both sizes summarize
per-group rewire volume, calculated as the sum of mentions, typed edges,
candidates and rejected facts.

| Report key | Definition and denominator |
| --- | --- |
| `metrics` | Metric definition version, currently 3 (3 judges hygiene noise with each document's own label preset, so `noise_rule_share` and hygiene samples are not comparable with version 2; 2 shares the hygiene company-suffix rule, so version 1 reports are not comparable on duplicate groups); no denominator. |
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
| `noise_rule_share` | Entities whose name every supporting document's hygiene preset drops / `entities`. |
| `co_mention` | Object describing unordered entity pairs sharing a chunk; no denominator. |
| `co_mention.nodes` | Entities incident to at least one co-mention pair; count. |
| `co_mention.edges` | Distinct unordered co-mention pairs, weighted by distinct shared chunks; count. |
| `co_mention.weight1_share` | Co-mention pairs sharing exactly one distinct chunk / `co_mention.edges`. |
| `co_mention.giant_share` | Nodes in the largest undirected co-mention component / `co_mention.nodes`. |
| `hygiene_policy` | Text identifying the hygiene policy used for noise and samples; no denominator. |
| `fold` | Advisory fold rebuild record, or `null` if there is no record. The entire object is graph-wide, even with `--domain`. |
| `fold.version` | Fold key algorithm version stored in the rebuild record. |
| `fold.exceptions_digest` | Digest identifying the stored exception list. |
| `fold.exceptions_count` | Number of stored exception keys. |
| `fold.scope_count` | Number of distinct key scopes with fold hints in the last completed rebuild, including the bare-key scope when present. |
| `fold.status` | `running`, `complete`, or `unknown` for older records without a status. |
| `fold.rebuilt_at` | UTC completion timestamp, or an empty string when no timestamp is recorded. |
| `fold.rows_written` | Number of entity rows changed by the completed rebuild. Zero when no count is recorded. |
| `fold.rows_skipped` | Number of entity rows skipped by the completed rebuild for a missing or empty key or ID. Zero when no count is recorded. |
| `fold.rows_stale` | Number of rows maintained by the rebuild whose stored hints differ from the expected hints. Present with `--fold-check` when the stored and current fold versions match. |
| `fold.matching` | `off (scopes missing; run gkg fold rebuild)` when the record has no lookup scopes. With `--fold-check`, a different stored fold version reports `off (version mismatch)`. Omitted otherwise. |

Co-mention pairs are uncapped; memory grows with Σ(entities per chunk)².

Noise and hygiene samples use each selected done document's label preset plus the
configured `ingest.stoplist_file` overlay, passing each entity's `type` as its
label. An empty preset name uses the built-in policy. If a selected document's
preset fails to load, `quality` exits with an error. A run that selects no
documents loads no presets. An entity counts as noise when every preset of its
supporting documents within the selection rejects it.
For a dropped name, the sample reports the rule from the first supporting preset
in sorted name order.

Sampling is deterministic for the seed and writes up to N dropped and N renamed
names with their original name, normalized name, and rule (`normalize` for
renamed names). The sample file records available counts and uses mode 0600.
Names go only to `--out`. Stdout contains aggregates.

## 7. Operations

- Health: `GET /healthz`, `GET /readyz`; metrics: `GET /metrics`
  (auth-exempt).
- `gkg_request_duration_seconds{surface,op,outcome}` measures mounted
  `/v1/*` handlers and registered MCP and agent tools. For `rest`, `op`
  is the mounted route pattern and timing starts when the request reaches
  the handler after authentication. The HTTP status determines `outcome`,
  with `ok` below 400, `client_error` for 4xx, and `error` for 5xx.
  Streaming handlers record on return. For `mcp` and `agent_tool`, `op` is
  the registered tool name and the same outcomes indicate success, invalid
  arguments, and tool failure.
- `gkg_store_write_lock_wait_seconds` measures the time to acquire the
  store's write lock before a serialized transaction.
- `limits.read_timeout_ms` (`GKG_LIMITS_READ_TIMEOUT_MS`) sets the time budget
  for each REST read and MCP or agent read tool, except `cypher` and `ask`.
  The default is 15000 ms and applies when the value is zero or negative. Cypher uses
  `limits.cypher_timeout_ms` (`GKG_LIMITS_CYPHER_TIMEOUT_MS`, default 10000 ms).
  MCP `ask` uses `GKG_AGENT_TIMEOUT_S`, with the normal tool budgets inside the loop.
  A search's embedding call gets at most half of the remaining read budget.
  If the embedder exceeds its share while the read budget is still active,
  search returns text-only results. Memgraph receives the remaining budget
  for each query. A timed-out REST query returns HTTP 504 with code
  `query_timeout`. MCP and agent tools return a tool error.
- Set `server.pprof_addr` or `GKG_SERVER_PPROF_ADDR` to a loopback
  host and port to expose `/debug/pprof/` there. An empty address disables
  profiling. Only the loopback listener serves profiles.
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

`GET /v1/domains` returns `[{id, name, documents, products, products_bound}]` for the UI pickers;
`documents` counts done documents, and empty domains remain listed. Search and
Ask offer independent domain pickers, defaulting to all domains. Search hits,
document list/detail responses, entity views, related entity references in facts,
co-mentions, and traversal nodes carry `domain`. The API accepts `domain` on
`POST /v1/search`, `POST /v1/agent/ask` and `/v1/agent/ask/stream`, and as a
query parameter on `GET /v1/documents`. Slugs use `[a-z0-9-]{1,40}`; empty
means all domains. MCP `ask`, `search`, `get_entity`, `get_chunks`, `traverse`,
`list_documents` and `status` also accept `domain`.

Answer tools enforce the ask domain regardless of model arguments. Entity
lookups and traversal starts outside that domain return a not-found error.
`get_chunks` reports out-of-scope ids in `missing`. `find_products` inherits
the ask domain and rejects a different domain argument. `get_product` returns
the unknown-product error for out-of-scope products. `list_features` keeps
features with a key in a store bound to the ask domain and computes release
and numeric-search status from those keys. The feature registry and store
bindings are read in separate snapshots. The prompt instructs the model to
apply the domain in Cypher queries. At schema v4, name resolution
uses `EntityKey` folding (including possessives and edge punctuation) for
document-scoped entities. Older schemas use case-folded display-name
matching. When the exact name lookup
returns no matches, MCP and agent `get_entity` can use verified advisory fold
hints. A fold hit carries `match: "fold"`. Unscoped ambiguous names return
`candidates`, one per domain. Text filters run before ranking and the
text engine's hit cap. Scoped vector search over-fetches 5× then post-filters,
so it can return fewer than the requested number of hits.

Entity and traversal selectors try a live ID, then a live key, then a recorded
merge redirect. REST, MCP and agent tool responses include `requested` with the
retired selector and `resolved_via: "merged"` when a redirect reaches a live
entity in scope. A redirect whose survivor is missing or outside the request
scope returns not found.

`gkg eval-retrieval --questions questions.json --domain zorbo` applies that
scope to all questions, including gold resolution and null checks. Without
`--domain`, each question's optional `domain` is used.

## Product feature observations

```sh
gkg features check R.json
gkg products emit -registry R.json records.jsonl > record-obs.jsonl
gkg products emit -registry R.json -release CANDIDATES.json -out released.jsonl records.jsonl
```

`features check` validates the JSON registry against `gkg-features/v1`.
On success, it prints the SHA-256 of the registry file bytes and the counts
of stores, features and mapped keys. It exits 0 for a valid registry, 1 for
a read or validation error, and 2 for incorrect usage.

`products emit` converts A1 product-envelope JSONL to canonical
`gkg-record-obs/v1` JSONL using the records file and the registry. Product
lines omit the Part A audit fields `partition` and `page_present`.

`-release` accepts a candidates file with `registry_sha256` and `keys`
containing `store`, `feature` and `source_key` triples. Conflicts are
calculated across all mapped aliases before the projection. Every product
still gets a product record, including products with zero emitted
observations. Use `-out PATH` for an atomically published file with mode
`0600`. Both commands run offline.

### Product registries and releases

Run the graph-backed commands against the configured Memgraph after
`gkg migrate -schema-only` or a successful `gkg serve` startup.

```sh
gkg features import R.json
gkg products release create -registry REV -candidates release-candidates.json \
  -store-domain example.test=example -approved-by owner \
  -pin R.json=R.json -pin score.json=score.json -pin report.json=report.json \
  -pin freeze.json=freeze.json -pin rf_kit.py=rf_kit.py \
  -pin E2=e2-raw.jsonl -pin record-obs.jsonl=record-obs.jsonl
gkg products release list
gkg products release activate ID
```

Import prints the registry revision, the first 16 hex characters of the
SHA-256 of the registry file. Re-importing those bytes prints `unchanged`.
A reused feature ID must retain its definition, kind, cardinality, canonical
unit, aliases and separators for shared mapped store keys across all
historical revisions. Display names can change.
New revisions serialize on a shared Memgraph node so concurrent imports
cannot approve incompatible meanings for the same feature ID.

Release create requires a `-pin` for each of `R.json`, `rf_kit.py`, `score.json`,
`report.json`, `freeze.json`, `E2` and `record-obs.jsonl`. Missing pins cause
exit 2 and the usage message names them. The candidates must name the imported
registry digest and contain only mapped triples. A successful create prints
the release ID and manifest digest.

Use a repeatable `-store-domain` to bind exactly the stores in the released
triples to existing domains. An extra binding causes exit 5 without writing
a release or any bindings. Each binding is immutable and every domain operation
touching the domain must be completed. Omit `-id` to derive a stable ID from
the inputs. Repeating the same create returns the original manifest.

`score.json` and `report.json` pins must reference the registry and candidates
digests. The report must have `release_allowed: true` and
`owner_decision_required: false`. The `freeze.json` pin must reference the
registry digest and matching `rf_kit.py` and `E2` pins. Additional pin names
record the SHA-256 of the named file.

`release list` prints JSON with each release's ID, state, registry revision,
approver, approval time and stores. Activating a created or retired release
retires the previous active release and immediately removes edges outside the
new triples or registry revision. It updates the affected stores' edge
counts in the same transaction.

### Product snapshots

```sh
gkg products import -store example.test -release ID -snapshot-seq 1 records.jsonl -report import.json
gkg products release activate NEXT -materialize example.test=2:records.jsonl
gkg products pages-report -store example.test
gkg products delete-store example.test
gkg products unbind example.test
```

`products import` takes a complete A1 JSONL snapshot for one bound store and
the active release. Memgraph's `--query-execution-timeout-sec` must be 0
(unlimited) or at least 300 seconds. The pinned Memgraph 3.14.0 image defaults
to 600 seconds. The CLI checks the input before writing and replaces all
products, feature edges and the snapshot watermark in one transaction. A
rejected row refuses the whole snapshot. Limits are
10,000 product rows, 60,000 released edges, 64 KiB per row, 512 attributes per
row and 64 MiB of replacement properties. Each released observation may have
up to 64 items, with up to 1 KiB in its raw value and in each item. Unmapped
and unreleased attributes count toward the row and attribute limits.
The `title`, `brand`, `category`, `subcategory`, `product_type` and `link`
fields allow up to 4 KiB each. The `product_id` allows up to 1 KiB.
Property bytes include names and stored values, including folded values.

Use a positive sequence strictly above the last admitted sequence and any
wipe sequence. A byte-identical file for the same release prints `unchanged`
when that materialization is still intact. Every successful import advances
`seen_seq`, stamps the current activation and clears the last push or
delete marker.
A drop of more than 10% in the product count requires `-allow-shrink`.
The private JSON report records the requested and previous counts, digest,
override, rejection details, unmapped keys, conflicts and parse status counts
by feature. Rejection and unmapped-key details are each capped at 1,000.
Exit codes are 0 for success, 1 for runtime failure, 2 for incorrect usage
and 5 for data rejection, including stale or incomplete snapshots.
Imports serialize on the active release pointer, including imports for different
stores. If a concurrent import loses with a write conflict (exit 1), rerun it.

`release activate ID -materialize STORE=SEQ:FILE` accepts repeatable
`-materialize` arguments. Activation commits first. Each file then imports
in order with its own transaction and prints its result. The first failure
stops processing and determines the command's exit code. Activation and
earlier successful imports stay committed. Use standalone `products import`
for a JSON report or to approve a shrink.

`products delete-store` is an owner-only operation. Run it with graph write
credentials only when wiping a store is intended. It removes all products
and edges and retains the binding, setting `wiped_seq` to `seen_seq` and the
product and edge counts to zero. It clears `snapshot_digest`, `release_id`
and `imported_at` while retaining `snapshot_seq` and `seen_seq`. The next
import must use a sequence higher than `wiped_seq`.
The wipe also clears the materialization stamp and last event marker.
`products unbind` removes a store's domain binding once it has zero products.
It advances the bound domain's guard counter and accepts any domain operation
state, including `incomplete`. A new `products release create` can bind that
store again. The store keeps its sequence floor across unbind and rebind.

### Push or remove one product

```sh
gkg products push updates.jsonl
gkg products remove -product example.test:42 -seq 4
```

A push file contains one `{"seq": N, "record": {...}}` JSON object per line.
Use the complete A1 record from the normalizer, including the store and its
full `product_id`. The ID must start with the store and a colon followed by
a non-empty suffix. The push envelope allows 128 KiB and the embedded
record uses the same row and projection limits as a snapshot import.
A push replaces the product properties and released observations for that ID.
A remove deletes that product and its edges. Remove any legacy `store:` IDs
by omitting them from a full import.

Use one increasing positive int64 sequence for all imports, pushes and
removals in each store. Each line commits in its own transaction in file order.
Processing stops at the first error. Earlier successes stay committed.
After any release activation, re-import **every store** at a higher sequence
before pushing or removing products. The activation sequence is global. A
wiped store also needs a new import before pushing or removing products.

| Exit | Status | Meaning |
| --- | --- | --- |
| 0 | `created`, `updated`, `deleted`, `absent` | The event applied and advanced the store's sequence, including removal of an already absent product. |
| 0 | `duplicate` | The sequence, operation and product ID match the latest event. The original write stays in place. |
| 5 | `stale`, `seq_conflict` | The sequence is at or below the wipe floor, below the watermark, or already belongs to a different event. The error includes `seen_seq`. |
| 5 | `release_changed`, `release_not_materialized`, `store_not_bound`, `store_cap` | Check the active release and binding, materialize the store, or reduce its size as indicated. A rejected event leaves its sequence available. |
| 5 | `retry` | A concurrent writer held a fence. Retry the same event after that write finishes. |
| 1 | `unknown_outcome` | The write timed out or its commit could not be confirmed. |
| 1 | Other errors | Input validation, file or graph errors. |
| 2 | Usage error | Check the required arguments. |

Each successful event prints a JSON result with `status`, `product_id`, `id`
and `seen_seq`. For `unknown_outcome`, resend the same sequence, operation
and product ID. If it already applied and is still the latest event, the
retry returns `duplicate`. If a newer event has advanced the store, reconcile
a `stale` result by reading the current product. The stores response shows
`seen_seq`, `materialized_activation_seq` and `pushed_at`. To correct an
applied push, send the corrected record or a full snapshot at a higher sequence.

### Product rollout runbook (P8)

Set `GKG_PRODUCTS_RUNBOOK_DIR` to the frozen Part A catalogue mirror and run
[`scripts/products-runbook.sh`](scripts/products-runbook.sh) from the checkout.
Docker must be running with the Memgraph image pinned in
`deploy/docker-compose.yml` already pulled because the script uses `--pull=never`.
The script starts a scratch database on loopback (`PORT` defaults to `17756`)
and removes it on exit.

The runbook checks the frozen oracle, snapshot replay and replacement,
release remapping and narrowing, and maintenance output on a mixed graph.
It prints the measurement JSON with one-store and three-store import timings
and memory measurements. Set `GKG_PRODUCTS_RUNBOOK_OUT` to a directory to
retain `measurements.json`.

### Read products and features

```sh
curl -s 'http://localhost:8080/v1/products?store=example.test&product_id=example.test%3A42'
curl -s 'http://localhost:8080/v1/products/stores'
curl -s 'http://localhost:8080/v1/features'
curl -s 'http://localhost:8080/v1/products/search' -H 'Content-Type: application/json' \
  -d '{"domain":"catalog","feature":"length","min":"10","max":"20"}'
```

Use `GET /v1/products/{id}` when you have a graph product ID from search.
Both lookups return product metadata and a `features` array with raw
observations and their feature revisions. Feature edges appear only when
their release ID matches the active release. After activation, a product
can have an empty feature array until its store is imported for that release.

Both lookups and MCP `get_product` return `pages` with up to three live,
`done` documents from the store's bound domain. Each page contains `id`,
`title`, the stored `source`, and `updated_at`. Pages are ordered by newest
`updated_at`, with ascending document ID breaking ties. Only the first
matching version per source kind and canonical source counts as a page.
An empty `pages` array has `pages_missing: true`. Pages can appear after
a product import as soon as their documents reach `done`.

The product link must match the document's `source` or `final_url` after
canonicalization. Any source kind can match. For absolute URLs with a host,
canonicalization lowercases the scheme and host, removes the fragment, and
strips trailing `/` characters from the path. It preserves the rest of the
URL, including user info, path case and query order. HTTP and HTTPS,
`www` and apex hosts, Unicode IDNs and punycode, percent-encoding case in
paths and queries, dot segments and explicit default ports are compared as-is.
These variants identify different pages. Redirect targets can still match
through `final_url`.

The page candidate scan costs O(done documents in the bound domain) per
product read. Setting a canonical URL property at ingest is the post-v1 path.

Use the read-only pages report to check coverage for imported product
snapshots before rollout. It counts products with zero, one, or multiple
matching pages per bound store, with up to 20 missing graph product IDs in
ascending order. The optional `-store` limits coverage to one store.

```sh
gkg products pages-report
gkg products pages-report -store example.test -json
```

Place rollout pages in their target domain before `gkg products release
create` binds it. If domain detection places a page elsewhere, move it into
the target domain and let it reach `done` before creating the release.

The store list reports `active_release_id` separately from each store's
materialized `release_id`. A wiped store has `wiped: true`, zero products
and edges, null `release_id` and `imported_at`, and retained sequence
watermarks. The feature list describes every feature in the active
registry. Each key reports its store, source key, `numeric_search` flag
and release status.

Search takes a feature ID and either `value` or at least one of `min`
and `max`. Value matching applies the kind's fold to the whole scalar or
unsplit value and matches any item in a split value. Conflicting edges can
match independently. For numeric search, use non-negative decimal strings
in the canonical unit, with up to six fractional digits. Bounds are
inclusive and match exact observations from numeric-enabled keys. If the
feature has released keys in scope and none support numeric search, the
search returns HTTP 400 with `error: not_numeric`. An unknown feature ID
returns empty hits in either search mode. A numeric search without `domain`
includes `numeric_excluded_stores` when any stores with released keys for
the feature lack a numeric-enabled released key. The list is sorted by store.

Search returns one hit per product with up to five matching evidence edges,
defaulting to 20 hits with a maximum of 50. The JSON page fits within 1 MiB.
`evidence_truncated: true` marks a hit whose evidence was cut. Pass the
returned `cursor` with the same domain, feature, value or bounds, and page
size to continue. A change to any of these parameters, a release activation,
or a watermark change in the searched scope invalidates the cursor. Start
again without it after HTTP 400 with `error: stale_cursor`.

MCP clients can call `get_product` with `id` or `store` and `product_id`,
`find_products` with the search fields above, and `list_features` with
empty arguments. Large `find_products` pages are available in
`structuredContent`.

#### Find products sharing a released feature value

Use the MCP or agent `cypher` tool with the seed's graph product ID as
`$id`. Set `$domain` to a domain slug or the empty string for all domains.
The query checks exact folded values and list items on both products.

```cypher
MATCH (m:GkgMeta {id:'product_release'})
MATCH (p:Product {id:$id})-[e1:HAS_FEATURE]->(f:FeatureRev)<-[e2:HAS_FEATURE]-(q:Product)
WHERE e1.release_id = m.value AND e2.release_id = m.value AND q <> p
AND (e1.value_fold IS NOT NULL AND (e2.value_fold = e1.value_fold OR e1.value_fold IN coalesce(e2.items_fold, []))
     OR any(v IN coalesce(e1.items_fold, []) WHERE v = e2.value_fold OR v IN coalesce(e2.items_fold, [])))
AND ($domain = '' OR q.domain = $domain)
RETURN q.id, q.title, collect(DISTINCT f.id) AS shared ORDER BY size(shared) DESC LIMIT 20
```

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
name and ledger without resetting extraction or requeueing documents. It also
works on domains with product store bindings.

A product store binding blocks moving documents into or out of the domain,
merging, deleting the empty domain, and undoing a move involving it.
The binding counts even when the store has zero products. The admin API returns
HTTP 409 `domain_has_products` and the CLI exits 1. If an in-progress move,
merge or undo detects a binding, it records `incomplete` and keeps any committed
progress. After removing the empty store binding with
`gkg products unbind STORE`, retry the original request with the same operation ID.
Supply `--op-id` when starting a move or merge so you have the ID if it fails.

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

### Rebuild the advisory entity fold index

Run `gkg fold rebuild` against a graph with the current extraction schema.
On the first run, pass `-exceptions FILE` or `-clear-exceptions`. Use
`-exceptions FILE` to replace the stored list and `-clear-exceptions` to empty
it. Later runs with neither flag reuse the stored list. The file takes one
exact entity key per line. Use qualified keys such as `@demo:zorbulo` on
graphs with domains. The command warns about exception keys absent from the graph.

With Compose, copy the file into the `gkg-data` volume and use its container path.

```sh
docker compose exec -T gkg mkdir -p /var/lib/gkg/fold
docker compose exec -T gkg sh -c 'umask 077; cat > /var/lib/gkg/fold/exceptions.txt' < exceptions.txt
docker compose exec gkg gkg fold rebuild -exceptions /var/lib/gkg/fold/exceptions.txt
```

The rebuild can run while the service is ingesting. It reports committed row
changes and the number of rows skipped for missing keys or IDs. Rerun after an
interruption or after running an older binary that can leave stale fold hints.
`gkg quality` shows the latest exception digest and rebuild status. The status
stays `running` after an interruption. A completed rebuild sets `complete` and
records its timestamp, counters and the key scopes used by fold name lookup.
Run a rebuild to populate scopes for older records. Records without a status
show `unknown`. Add `--fold-check` for a read-only count of missing or stale hints
among the rows the rebuild maintains. Rows skipped by the rebuild appear in
`rows_skipped`. The stale count requires matching fold versions. A version
mismatch reports that fold matching is off.

### Plan entity merges by fold

```sh
gkg merge-folds -plan -out merge-plan.jsonl
gkg merge-folds -plan -out demo-merge-plan.jsonl -domain demo -timeout 10m
```

Run this after a complete `gkg fold rebuild` with the current fold version.
The planner uses the rebuild's eligibility rule. An entity needs a non-empty
key and ID to be included in `entity_count`, the stale hint check or any group.
Other rows are counted in `skipped_rows`. The planner folds persisted names
using the stored exceptions. Document-scoped and vetoed entity keys are
excluded from groups. With `-domain SLUG`, the planner scans the key range
for the `@SLUG:` prefix. Only rows in that scope contribute to its counts.

The planner reads one graph snapshot in a single read transaction and holds
the whole plan in memory. It is intended for the current boxes with about
30,000 entities. Audit ID lookups can scan the graph when there is no
`RejectedFact(id)` index. `-timeout` limits the graph connection and planning
reads and defaults to `10m`.

The command creates a new private JSONL file with mode `0600` and refuses an
existing path. The header records the fold version, exception digest, entity
count, skipped rows and generation time. Each group identifies the canonical
key by stored mention count, with ties going to the smallest key. Members
include their distinct supporting documents, incoming and outgoing typed
edges by judge status, candidates, rejected facts and embedding presence.
Rewire counts each stored mention, typed edge, candidate or audit touching a
loser once per group. Candidate and audit ID checks report same-tuple
collapses and list different-tuple conflicts in `blocking`. The trailer's
`sha256` hashes the exact bytes of the header and group lines, including their
newlines.

Stdout reports group and member counts, type mismatches, blocking groups,
maximum and nearest-rank p95 rewire counts, stale hints and `skipped_rows`.
Exit `0` means the report was produced, including any blocking findings.
Exit `1` means an operational error. Exit `2` means invalid arguments or a
missing, outdated or incomplete fold record.

### Repair stored entity names

Run `gkg repair-names -dry-run` to preview the number of stored entity names
with removable edge markup. Run `gkg repair-names` to repair them in the
configured graph without re-extraction. The commands report `scanned=N` and
either `would_change=M` or `changed=M`. The repair updates the display name
and its lookup key only when the name still matches the one it read.
If repair fails partway through, the error includes the partial counts on
stderr. Rerunning the repair is safe and idempotent because it skips names
that are already clean.

### Repair stored document titles

Run `gkg repair-titles` to preview titles for documents with an empty or
missing title. The command checks the leading 8 KiB of stored chunks in
sequence for Markdown front matter and an early H1, then tries the stored
source's filename or URL slug. Run `gkg repair-titles -apply` to write the
derived titles and their origins in batches. The command leaves existing
titles alone and can be rerun after an interruption.

Use `-domain SLUG` to limit the scan to one domain. Add `-out FILE` to write
the proposed changes as JSONL records with `id`, `title`, and `title_source`.
The path must be new and the file is created with mode `0600`. With
`-apply -out FILE`, the command writes and closes the report, then scans for
empty titles again and applies the titles derived in that second pass.
Stdout prints the total scanned, counts by origin, and the unchanged count.
The repair updates title metadata without running extraction or judging.

### Repair stored section labels

Run `gkg repair-labels -dry-run` to count section-label mentions and their
dependent facts using stored chunk text and byte offsets. Run
`gkg repair-labels` to remove them. The repair uses each document's hygiene
preset allowlist and commits one chunk at a time. You can rerun it after an
interruption. It requires no extraction or judge calls.
Ingest and `repair-labels` check for code fences only within the current chunk,
so a short heading-shaped comment can be dropped if it starts a new chunk
inside a fenced block.

Run the sweep with ingestion idle, then run `gkg repair-labels -dry-run`
again for the same scope and expect `spans=0`. A server running an older
binary can recreate the labels if it re-extracts a repaired document.
The sweep skips documents with a status other than `done` and warns when it
skips a document because its hygiene preset could not be loaded. Check
`skipped_docs` alongside `spans` to see whether any documents with chunks
were skipped.

Use `-domain slug` to scan only documents in that domain. Add
`-report path` to write a private JSONL file with each matching span's entity,
rule and source line, capped at 200 bytes. The final record contains
`skipped_docs`, the number of distinct documents skipped. The path must be new. With
`-report`, apply writes the report before making graph changes.

The summary reports scanned chunks, matched spans by rule, confirmed and
other fact edges, candidates, rejected facts and entities removed. The dry-run
entity count and the report's `entity_removed` flag indicate that every
mention of the entity across the graph is selected for removal. Apply counts
entities actually collected after all their relationships are gone.
