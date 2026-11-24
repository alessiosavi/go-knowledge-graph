# Known limitations

## Document dates and windows

- Undated documents use their ingest time (`document_date_source=ingest`).
- Chunk deduplication keeps facts restated verbatim in a later document
  attributed to the first document and its date. A window that excludes the
  first document hides those facts. The restating chunk is still searchable.
- Entity hits, co-mentions and hidden counts ignore the window.
- Chunk search is exact only within Tantivy's first approximately 1,000 matches.
- Eligibility is evaluated per read. A document redated during a request can
  show a date outside the window, or be eligible in one traverse pass and
  ineligible in the next. A top-up can skip or repeat an edge.
- Historical `as_of` reads across versions are unavailable.
- Ranking does not use recency.
- Eval artifacts omit dates.
- A dated resubmit of a document that is still processing returns busy.

## PDF extraction (best-effort, pure Go)

When conversion is off (including light mode), PDF text extraction uses
`github.com/dslipak/pdf` v0.0.2 (pinned) and is **best-effort by design**:

- **No OCR** — scanned or image-only PDFs fail with a clear
  "no extractable text" error; they are never silently ingested empty.
- **No tables, columns or figures** — text is extracted in content-stream
  order.
- **Header constraint** — the library accepts only `%PDF-1.0` … `%PDF-1.7`
  at byte 0; a `%PDF-2.0`/`1.8` header is rejected with a clear open error
  (the dispatcher's `%PDF` magic is wider than what can succeed).
- **Trailer pre-flight + isolated parser** — dslipak/pdf v0.0.2 has an
  uncatchable EOF-spin (a lexer loop that never sets its EOF flag) triggered
  by crafted `startxref` offsets, in-range as well as out-of-range.
  `PDFToText` therefore (a) fast-fails unusable trailers
  (missing/malformed/out-of-range `startxref`) and (b) **parses in a
  killable subprocess** (`gkg pdf-extract`, 30 s timeout): a hostile
  document costs one killed child process, never a wedged worker. Panics
  from other malformed inputs are recovered into errors (pinned by walking
  truncated prefixes); both containment layers are pinned by watchdog
  tests.
- The optional sidecar's higher-quality PDF text path is an explicit v0.1
  deferral; the sidecar in v0.1 extracts entities only.
- With the default `GKG_CONVERTER_MODE=docling` (see
  [quickstart](quickstart.md#7-high-quality-document-ingestion-docling))
  docling converts PDFs (scans included) and Office formats
  to markdown before the pipeline; a conversion failure fails the
  document with the converter's reason instead of falling back to the
  best-effort path.
- **Image limits:** conversion is enabled by default, with Tesseract OCR.
  The docling image ships English OCR data only; Italian accents can be
  misread. Images above `GKG_CONVERTER_MAX_IMAGE_MP` (12.5 MP by default) are
  refused before conversion; downscale them or raise the cap only on a bigger
  machine after measuring concurrent conversion memory. PNG/JPEG/GIF/BMP/WEBP
  and the first TIFF image directory are checked; unknown or corrupt image
  headers are refused. HEIC/AVIF are unsupported: export as PNG or JPEG.
  Transparent PNGs are flattened onto white. Figures become image placeholders
  instead of embedded base64. Set `GKG_CONVERTER_IMAGES=false` to refuse images.

### Fixture extraction results (recorded)

| Fixture                                                              | Result                                                                      |
| -------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| `internal/ingest/testdata/sample.pdf` (cupsfilter-generated, 1 page) | Full phrase extracted: "GKG sample PDF fixture text for extraction checks." |
| `internal/ingest/testdata/sample.md`                                 | Passed through unchanged; `#`/`##` headings become chunker boundaries       |
| HTML bodies                                                          | Article text via readability, tag-strip fallback; scripts/styles stripped   |

## Restart behaviour of in-flight documents

- Accepted **file, text and URL** submissions persist their converted text
  before `202`; queued uploads survive restarts without re-conversion.
  Recovery checks the payload against the graph's content hash. Corrupt
  payloads fail rather than ingesting under a stale identity.
- The private spool is removed once chunks are durable, or the document
  fails or is superseded. Startup sweeps orphan payloads and abandoned
  temporary files before requeueing in-flight documents in the background.
  Retain Compose's `gkg-data` volume as well as Memgraph's volume across
  redeploys.
- **Extraction-schema upgrades:** v2 introduced name-only entity identity;
  v3 links every extracted mention to its chunk and sets entity types by a
  majority vote of typed mentions (ties alphabetical). `gkg serve`, `gkg mcp`
  and `gkg migrate` upgrade outdated graphs by clearing derived entities,
  mentions, facts and judge audits, then re-extracting
  previously extracted live documents from stored chunk text. Documents,
  chunks and fingerprints are retained; no source re-fetch is needed. The
  graph is partial until the recovery queue drains (`gkg migrate` waits for
  re-extraction to finish; it exits nonzero if any document fails and leaves
  those documents queued for `gkg serve`), and judge audits are regenerated.
  Previously failed and superseded documents are not requeued. `gkg ingest`
  refuses an outdated graph; run `gkg migrate` or start `gkg serve` before
  ingesting.
  Schema v4 migrations require `GKG_DOMAIN_SEED_FILE` covering every live source
  (or `detect-all` for development). `gkg preflight` is read-only, and
  `gkg migrate --schema-only` leaves re-extraction to the next `serve` startup.
  After the rebuild, run `gkg judge backfill --budget-calls N` with the owner's
  approved call budget: a crash during verification can leave unjudged candidates
  in a completed document; those candidates remain invisible until backfilled.
- Legacy or missing-spool **URLs** re-fetch and re-convert, then hash-check;
  changed content fails with a resubmit hint. Missing-spool **files/text**
  cannot reload; re-push them. **Failed** documents are not automatically
  retried: re-push the content, including after an extraction timeout.
- The spool supports one gkg process per directory and defaults to 1024
  entries / 1 GiB of serialized payloads. Full spools return `503` and
  `Retry-After: 30`; write failures return `500` before acceptance. See
  [USAGE](../USAGE.md) for standalone paths and configuration.
- Sidecar inference is serialized per process. The 300-second default
  `GKG_EXTRACTOR_SIDECAR_TIMEOUT_S` includes queueing behind other workers,
  so slow batches or a larger pool can still require a higher timeout.

## Deleting documents

Deletion is permanent: `DELETE /v1/documents/{id}` removes the document
node with its chunks, fact edges and mentions, the judge verdicts cached
for its facts, its rejected-fact audits, and prunes the entities left without relationships. There
is no undo and no tombstone. A verdict belongs to the document that first
asked for it; an identical request from another document reuses it, and
pays the judge again only after that first document is deleted.

`?chain=1` removes every version of the same source identity — superseded
history included — in one all-or-nothing transaction; the API default
without the flag is strictly per-version (the UI's ✕ always sends the
chain flag, matching "remove this document from the graph").

Deleting a document whose pipeline job is still running is benign: the
worker's later writes fail with `not found`, are logged, and the job is
dropped — the document does not come back. A concurrent resubmit of
identical content can recreate the same content-addressed id after the
delete.

A per-version delete of an older version while a newer version is finishing
is benign: the newer version completes normally.

## Other v0.1 notes

- With the stack down, a host-run binary's ingests fail at extraction or
  conversion because the defaults need sidecar/docling. There is no silent
  fallback; set the modes explicitly for a setup without those services.

- No crawling/scheduling, no multi-tenancy (see the design's non-goals).
- The gliner2.5 (Apache-2.0) local extraction swap is a v0.1 item: the
  pinned `glinerel-fast` 0.1.1 runtime only loads GLiNEREL checkpoints (its
  config contract rejects the gliner2.5 architecture). For a fully free
  extraction path in v0, point the `openai` provider at any
  OpenAI-compatible endpoint.
- URL fetches are capped at `Ingest.MaxURLMB` (default 10 MiB), uploads at
  `Ingest.MaxUploadMB` (default 20 MiB), and JSON bodies on the read
  endpoints at 1 MiB.

## Product page association

Exact URL matching lowercases the scheme and host, removes the fragment and
strips trailing path slashes. HTTP and HTTPS, `www` and apex hosts, Unicode
IDNs and punycode, percent-encoding case in paths and queries, dot segments
and explicit default ports are compared as-is and identify different pages.
Redirect targets can still match through the document's `final_url`.

Page-key inference scans done documents in the product's bound domain and
matches eligible opaque sources by their last segment. A namespaced source
can match an unrelated product when it shares that product's unique page key.
When multiple products in the domain share a key, page-key inference drops
that key. The URL and page-key index legs each have a 200,000-row and 64 MiB
input budget. When page-key coverage is unavailable, product detail reports
`pages_partial`, retains direct pages and sets `pages_missing` to false.
Page-key matches can reflect a later snapshot than the record and direct
pages.

Chunk product enrichment reads documents and live refs in one batch, then
resolves inferred links against the catalogue snapshot. The two reads can
reflect different instants. If either inferred leg is capped or unavailable,
`get_chunks` reports `links_partial` and keeps available document links.
When a display row is missing, only that product's inferred links are dropped.
`products_total` includes up to 50 URL owners per document across the source
and redirect URLs, in store, product ID, and graph ID order. Extra URL owners
are excluded from the count unless supported by other evidence. When the
inferred links are otherwise available, truncation reports
`links_partial: owners_truncated`.
The code-matching leg reports its own status in `products_error`. The
reported total counts the available associations when either status is set.

## Read-only Cypher surface

- `POST /v1/cypher` (and the future MCP `cypher` tool) accepts one
  statement; write clauses and data loaders are rejected, and `CALL` is
  limited to the `vector_search.` / `text_search.` namespaces.
- `UNION` is rejected: a Cypher `UNION` applies its `LIMIT` per branch, so
  the post-union row cap (`Limits.CypherRows`) cannot be enforced on a
  single statement. Queries without a LIMIT gain `LIMIT $cypher_rows`; a
  LIMIT that is not a plain integer or `$cypher_rows` is rejected because
  an expression (`LIMIT 2 * 1000`) would exceed the cap.

## Ask the graph agent (optional)

- Off by default (`GKG_AGENT_ENABLED=false`); while off, `POST /v1/agent/ask`
  answers 501 and the web UI keeps its panel hidden.
- Answers are LLM-generated and can be wrong: the response's `evidence`
  array carries the raw tool results, which are the source of truth.
- Needs an OpenAI-compatible chat endpoint; function calling is preferred,
  and endpoints that reject the `tools` parameter fall back to a
  constrained JSON plan (one round of tool calls, then a final answer).
  When streaming is supported, these providers keep streaming the final
  answer and pay one extra rejected request per streamed ask.
- A provider that does not actually stream is detected automatically
  (`GKG_AGENT_STREAM`, default on). With default generation settings, the
  agent remembers the fallback to non-streamed answers for the process lifetime.
  A request with explicit generation settings negotiates its own fallback
  without changing the streaming capabilities recorded for future requests.
- Provider cost and latency apply to every ask. Each ask is bounded by
  `GKG_AGENT_MAX_STEPS` (default 6, hard cap 30 model turns that request
  tools; each turn executes up to 8 tool calls) and `GKG_AGENT_TIMEOUT_S`
  (default 60), including one final tools-less turn when the step or context
  budget is reached. A failed or empty budget final turn returns a bounded
  notice with the gathered evidence; cancellation/deadline errors still
  fail the ask.
- Tool-result transcript copies are capped at 32 KiB each. Clipped copies
  include truncation guidance. `GKG_AGENT_CONTEXT_BUDGET_KB` (default 256 KiB)
  stops further tools once cumulative copies reach the budget. The final
  result may cross the threshold. Skipped calls still get paired budget-error
  results. This budget counts tool-result bytes. Questions, tool arguments,
  and other messages also consume model context. Evidence may be large when
  it retains the full tool result. The product tools fit their results to
  the 32 KiB budget and report omissions using
  `features_omitted`, `mentions_omitted_for_budget`, `truncated`/`omitted`, or
  a shorter `find_products` page with a cursor. The agent's `search` tool
  also fits `auto` and `product` results before recording evidence and
  reports the number of hits dropped for the budget in `hits_omitted`.
- Answer HTML uses the reader’s sanitized Markdown pipeline, with all images
  omitted. Raw HTML is omitted and unsafe link destinations become `#`.
  Progressive deltas remain plain text until the done event replaces them.
- The edge defaults its ask timeout to `GKG_AGENT_TIMEOUT_S + 30`; explicit
  `EDGE_ASK_TIMEOUT_S` values below agent timeout + 15 prevent startup.
  Raising the agent timeout increases the time and provider cost an ask may
  consume; both JSON and streamed edge routes use the aligned timeout.
- Read-only by design: the tool set mirrors the MCP read set — no ingest in
  v1. Tool outputs are treated as untrusted data in the prompt, but
  ingested documents can still attempt prompt injection; the read-only
  boundary and the step/timeout caps are the hard limits.
- No chat memory: each ask is independent.

## Entity embeddings (optional)

- With `Embedder.Enabled` the pipeline embeds each ingested document's
  distinct entity names after merge (batches of ≤64) and ensures the
  `entity_vec` vector index (cosine, capacity 10k); the provider is the
  OpenAI-compatible default or the asymmetric `furls` service (see
  [quickstart](quickstart.md#6-optional-embeddings-and-the-judge)). With
  `Embedder.Dim > 0`
  the dimension is requested from the provider (`dimensions` parameter) and
  validated on every response; `Dim = 0` accepts the provider's native
  dimension. Embedder failures are logged and never fail a document:
  entities simply stay unembedded.
- The index dimension is fixed at creation: pointing embeddings at a
  different model dimension later fails with a clear dimension-mismatch
  error instead of silently skipping vectors.
- Embedding runs after merge like the judge: recovery of a `merged` /
  `verifying` document skips it, and an unchanged re-submit is a no-op —
  use `force` to re-embed such documents.
- Entity deletes clear the `embedding` property first so the index drops
  the entry. Out-of-band deletes can leave stale index entries that make
  vector searches error until Memgraph reclaims them (observed within about
  a minute); searches degrade to text-only meanwhile (never dropping text
  recall) — rebuild the index after such maintenance.
- **Embedding-space switches are warn-only**: vectors from different
  providers/models live in different spaces, and two 768d spaces are
  indistinguishable to the store. At startup gkg records the active space
  (`provider|model|dim`) in a `:GkgMeta` marker node and logs a prominent
  warning when it differs from the stored one; the migration is to clear
  `Entity.embedding`, `DROP VECTOR INDEX entity_vec` and re-ingest (see
  [quickstart](quickstart.md#6-optional-embeddings-and-the-judge)).
- With the `furls` provider, every distinct entity name embedded at ingest
  becomes a persisted document-side row in the embeddings service (its
  content-hash cache, no TTL) — that is the service's contract and what
  makes unchanged re-ingests cheap.
- Vector search is approximate: entities whose distances are exactly tied
  (for example, distinct entities that share an identical embedding) may be
  selected arbitrarily among the tied set.

## Text search and aliases

- Entity text search is **name-only by design**: `entity_text_v2` indexes
  `name` and `domain_tok`, and excludes `aliases`. Alias-only lookups miss
  (pinned by `TestSearchAliasesAreNotIndexed`). Memgraph text indexes
  (Tantivy) do not index list properties.
- Memgraph 3.14 leaves a multi-property text-index property unindexed when
  it is declared after a property with a higher internal id (ids follow the
  first use of a property name). Bootstrap therefore references `text` and
  `name` before `domain_tok`, so the v2 indexes cover all their properties on
  a fresh database as well as on an existing one.
- `kind: auto` merges entity and chunk hits by fused score; with a small
  `limit`, entities (up to two fusion legs) can crowd chunks out — ask for
  the kind explicitly when that matters.
- Traversal surfaces one edge per (head, rel, tail, chunk); the same triple
  appearing in several chunks is intentionally several evidence rows.

## Edge (nginx + ModSecurity)

- **One password = full access**: every login can ingest, delete and ask; there
  are no per-user permissions.
- **JSON bodies are capped at 4 MiB at the edge** (pasted text, MCP calls);
  files keep 20 MiB. In `DetectionOnly` mode the cap is not enforced
  (ModSecurity processes oversized bodies partially) — keep that mode short.
- **Not exposed through the edge:** `/v1/admin/rebuild`, `/v1/admin/domains/*`, `/v1/cypher`, `/metrics`, `/readyz`; use
  the `cypher` MCP tool, the host loopback port for rebuild, and the
  in-container CLI for domain mutations.
- **Free-text fields are not WAF-inspected for attack payloads**: document
  text, search queries and agent questions are data to gkg (parameterized
  Cypher, no shell, sanitized rendering), so the SQLi/RCE/XSS/LFI/RFI/injection
  and protocol rule tags skip exactly those fields; every other field, header
  and the URI stay inspected.
- **Java-exploit rules skip the raw body of `POST /v1/ingest` and `POST /mcp`**
  (gkg runs no JVM; design documents contain Java code); they still inspect
  query parameters and headers. The restricted-upload filename rule (932180)
  is off for ingest uploads only, because it substring-matches names such as
  `dockerfile-notes.md` and gkg never writes uploads by name.
- **Matched request data reaches the WAF audit log** for blocked requests (an
  attack excerpt, never the password or the internal key); the log stays on
  the box with Docker's rotation (3 × 10 MB per service).
- **Reload and long streams:** after a certificate renewal an old nginx worker
  stays alive while an MCP session it serves remains open (the pinned image
  has no place for `worker_shutdown_timeout`).

### Domain filtering

`GET /v1/domains` and the UI picker are empty until documents have assigned domains (task 37.14b / the v4 migration).

`GET /v1/domains` lists domains with done-document counts, including empty
domains. Text search scopes inside the v2 index using an opaque token and
then checks the exact slug before ranking/limiting. Scoped entity vector
search over-fetches 5× and post-filters; a small domain can return fewer
results than the requested limit. Ask scope is enforced on answer tools;
guarded Cypher is not domain-enforced and relies on the stated prompt scope.
Name lookup uses indexed `Entity.name_key` and `EntityKey` folding at v4
(possessives and edge punctuation included); legacy graphs retain the
case-folded name scan. Ambiguous unscoped names return one candidate per domain.
Name lookup also matches folded variants on an exact miss, and entity selectors
follow merge redirects one hop.

## Admin reachability and historical audits

`POST /v1/admin/rebuild` is protected by the configured internal bearer, the
`127.0.0.1` port publish and the closed edge route map. It has no socket peer
check, so the host curl in the quickstart works. Domain admin routes additionally
require a loopback peer: host requests through the published port arrive from the
Docker gateway and receive 403 `loopback_only`. Run `docker compose exec gkg gkg
domain move|merge|rename|undo …` on the box from the deployment directory (or pass
its existing Compose project name) so the CLI calls serve on `127.0.0.1` inside
the container.

Audit rows written before relation-ID sanitation retain their unsanitized
relations and IDs; judging the same fact again can create a second audit row.
The v4 rebuild resets these audits.
