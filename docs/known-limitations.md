# Known limitations

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

- **URL** submissions are re-fetched on restart and verified against the
  stored content hash — a source that changed fails with a resubmit hint
  instead of ingesting under a stale identity.
- **File and text** submissions **cannot** be reloaded after a restart
  (bytes and raw text are not stored); they fail cleanly with a
  "resubmit the content" error and are retried by resubmitting.
- **docling mode**: URL documents re-fetch and re-convert on restart and
  are hash-checked like any source — a conversion that is not byte-stable
  across runs fails with the existing resubmit hint. File/text reload rules
  are unchanged (bytes are not stored).

## Deleting documents

Deletion is permanent: `DELETE /v1/documents/{id}` removes the document
node with its chunks, fact edges and mentions, and prunes the entities
left without relationships. There is no undo and no tombstone.

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
  (`GKG_AGENT_STREAM`, default on): the agent falls back to non-streamed
  answers for the process lifetime — answers are unchanged, only the
  progressive rendering stops.
- Provider cost and latency apply to every ask. Each ask is bounded by
  `GKG_AGENT_MAX_STEPS` (default 6, hard cap 30 model turns that request
  tools; each turn executes up to 8 tool calls) and `GKG_AGENT_TIMEOUT_S`
  (default 60), including one final tools-less turn when the step or context
  budget is reached. A failed or empty budget final turn returns a bounded
  notice with the gathered evidence; cancellation/deadline errors still
  fail the ask.
- Tool-result transcript copies are capped at 32 KiB each, with truncation
  guidance. `GKG_AGENT_CONTEXT_BUDGET_KB` (default 256 KiB) stops further
  tools once cumulative copies reach the budget. The final result may cross
  the threshold; skipped calls still get paired budget-error results. This
  is a tool-result byte budget, not a token or total-context limit: questions,
  tool arguments, and other messages also consume model context. Evidence
  retains complete results and may still be large.
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

- Memgraph text indexes (Tantivy) **do not index list properties**, so the
  `aliases` half of `entity_text` is inert upstream: entity search is
  authoritative on `name` only and alias-only lookups silently miss
  (pinned by `TestSearchAliasesAreNotIndexed`). A flattened scalar alias
  property is the planned follow-up.
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
- **Not exposed through the edge:** `/v1/cypher`, `/metrics`, `/readyz`; use
  the `cypher` MCP tool or the loopback ports on the host.
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
