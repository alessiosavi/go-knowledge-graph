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

## Other v0.1 notes

- No natural-language `ask` endpoint, no crawling/scheduling, no
  multi-tenancy (see the design's non-goals).
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
