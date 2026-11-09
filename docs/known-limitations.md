# Known limitations

## PDF extraction (best-effort, pure Go)

PDF text extraction uses `github.com/dslipak/pdf` v0.0.2 (pinned) and is
**best-effort by design**:

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
- With `GKG_CONVERTER_MODE=docling` (see
  [quickstart](quickstart.md#7-optional-high-quality-document-ingestion-docling))
  the `docling` profile converts PDFs (scans included), Office formats and
  images to markdown before the pipeline; a conversion failure fails the
  document with the converter's reason instead of falling back to the
  best-effort path.

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

## Other v0.1 notes

- No natural-language `ask` endpoint, no crawling/scheduling, no
  multi-tenancy (see the design's non-goals).
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
