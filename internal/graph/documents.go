package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// DocumentID returns the node id of one content version of a source:
// hex(sha256("kind|source|hash"))[:32]. The content hash is part of the id
// so that changed content yields a new document node that can supersede the
// previous one while the old node stays addressable.
func DocumentID(kind, source, hash string) string {
	sum := sha256.Sum256([]byte(kind + "|" + source + "|" + hash))
	return hex.EncodeToString(sum[:])[:32]
}

// UpsertDocument creates the document node for a submitted source version.
//
// Semantics:
//   - same source + same live content → Noop (zero extractor calls). A
//     document in status failed does not answer Noop: resubmitting its
//     content retries it (the node is reused, status resets to received).
//   - same source + changed content → new document id, PreviousID names the
//     newest live version other than the new one; supersede itself runs
//     later, after the new version has merged.
//   - upserts are serialized per Store so concurrent submits cannot race
//     the identity pre-read.
func (s *Store) UpsertDocument(ctx context.Context, in DocumentInput) (UpsertResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	live, err := s.liveDocuments(ctx, in.Kind, in.Source)
	if err != nil {
		return UpsertResult{}, err
	}
	id := DocumentID(in.Kind, in.Source, in.Hash)

	for _, d := range live {
		if d.Hash == in.Hash && d.Status != StatusFailed {
			if in.Force {
				// Preserve version ordering, but durably retain the new preset
				// and metadata before the pipeline can discard its payload.
				err := s.runWrite(ctx, `
MATCH (d:Document {id: $id})
SET d.status = $status, d.error = '', d.labels = $labels, d.title = $title,
    d.final_url = $final, d.updated_at = $now`, map[string]any{
					"id": d.ID, "status": string(StatusReceived), "labels": in.Labels,
					"title": in.Title, "final": in.FinalURL, "now": time.Now().UTC().Format(timeLayout),
				})
				return UpsertResult{ID: d.ID}, err
			}
			return UpsertResult{ID: d.ID, Noop: true}, nil
		}
	}

	// Previous is the newest live version by created_at — never updated_at:
	// a status bump on the old version during the supersede window must not
	// flip which version is current. Ordering by created_at, a failed
	// same-id node (retry) is skipped because PreviousID would be self.
	prevID := ""
	for _, d := range live {
		if d.ID != id {
			prevID = d.ID
			break
		}
	}

	err = s.runWrite(ctx, `
MERGE (d:Document {id: $id})
ON CREATE SET d.created_at = $now
SET d.source_kind = $kind, d.source = $source, d.final_url = $final, d.title = $title,
    d.hash = $hash, d.labels = $labels, d.status = $status, d.error = '', d.updated_at = $now, d.version_at = $now,
    d.supersedes = coalesce(d.supersedes, '')`,
		map[string]any{
			"id": id, "kind": in.Kind, "source": in.Source, "final": in.FinalURL,
			"title": in.Title, "hash": in.Hash, "labels": in.Labels,
			"status": string(StatusReceived),
			"now":    time.Now().UTC().Format(timeLayout),
		})
	if err != nil {
		return UpsertResult{}, fmt.Errorf("graph: upsert document: %w", err)
	}
	return UpsertResult{ID: id, PreviousID: prevID}, nil
}

// liveDoc is one non-superseded document of an identity.
type liveDoc struct {
	ID     string
	Hash   string
	Status Status
}

// liveDocuments returns the identity's live documents, newest version
// first (created_at DESC, id DESC as a deterministic tie-breaker).
func (s *Store) liveDocuments(ctx context.Context, kind, source string) ([]liveDoc, error) {
	recs, err := s.runRead(ctx, `
MATCH (d:Document {source_kind: $kind, source: $source})
WHERE d.status <> $superseded
RETURN d.id AS id, d.hash AS hash, d.status AS status
ORDER BY d.created_at DESC, d.id DESC`,
		map[string]any{"kind": kind, "source": source, "superseded": string(StatusSuperseded)})
	if err != nil {
		return nil, fmt.Errorf("graph: lookup document: %w", err)
	}
	live := make([]liveDoc, 0, len(recs))
	for _, r := range recs {
		live = append(live, liveDoc{
			ID:     getString(r, "id"),
			Hash:   getString(r, "hash"),
			Status: Status(getString(r, "status")),
		})
	}
	return live, nil
}

// requireDocument returns ErrNotFound when the document does not exist.
func (s *Store) requireDocument(ctx context.Context, docID string) error {
	recs, err := s.runRead(ctx, "MATCH (d:Document {id: $id}) RETURN 1", map[string]any{"id": docID})
	if err != nil {
		return fmt.Errorf("graph: lookup document %s: %w", docID, err)
	}
	if len(recs) == 0 {
		return fmt.Errorf("graph: document %s: %w", docID, ErrNotFound)
	}
	return nil
}

// PutChunks persists the document's chunks atomically, deduplicating by
// derived chunk id (docID+"-c"+seq), and links them with HAS_CHUNK. The
// liveness check and the write share one transaction under Store.mu, so a
// stale worker racing Supersede cannot re-attach chunks to a superseded
// document: it either commits before the supersede (its chunks are then
// deleted) or observes the supersede and receives ErrSuperseded. A missing
// document yields ErrNotFound.
func (s *Store) PutChunks(ctx context.Context, docID string, chunks []Chunk) error {
	rows := make([]map[string]any, 0, len(chunks))
	for _, c := range chunks {
		rows = append(rows, map[string]any{
			"id":        docID + "-c" + strconv.Itoa(c.Seq),
			"seq":       c.Seq,
			"text":      c.Text,
			"start":     c.Start,
			"end":       c.End,
			"hash":      c.Hash,
			"exact_key": c.ExactKey,
			"sig":       c.MinhashSig,
			"bands":     c.BandKeys,
			"version":   c.SignatureVersion,
			"dup":       c.DuplicateOf,
		})
	}
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		st, err := txDocumentStatus(ctx, tx, docID)
		if err != nil {
			return err
		}
		if st == StatusSuperseded {
			return fmt.Errorf("graph: document %s: %w", docID, ErrSuperseded)
		}
		if len(rows) == 0 {
			return nil
		}
		if err := txRun(ctx, tx, `
MATCH (d:Document {id: $docID})
UNWIND $chunks AS c
MERGE (ch:Chunk {id: c.id})
SET ch.doc_id = $docID, ch.seq = c.seq, ch.text = c.text, ch.start = c.start, ch.end = c.end,
    ch.hash = c.hash, ch.exact_key = c.exact_key, ch.minhash_sig = c.sig, ch.band_keys = c.bands,
    ch.signature_version = c.version, ch.duplicate_of = c.dup
MERGE (d)-[h:HAS_CHUNK]->(ch)
SET h.seq = c.seq`,
			map[string]any{"docID": docID, "chunks": rows}); err != nil {
			return fmt.Errorf("graph: put chunks: %w", err)
		}
		return nil
	})
}

// DeleteChunksFrom prunes the document's chunks with seq >= fromSeq; the
// pipeline calls it before a full re-chunk so a shrink cannot leave the
// earlier attempt's higher-seq chunks attached.
func (s *Store) DeleteChunksFrom(ctx context.Context, docID string, fromSeq int) error {
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		if err := txRun(ctx, tx, `
MATCH (:Document {id: $docID})-[:HAS_CHUNK]->(stale:Chunk)
WHERE stale.seq >= $from
DETACH DELETE stale`,
			map[string]any{"docID": docID, "from": fromSeq}); err != nil {
			return fmt.Errorf("graph: prune stale chunks: %w", err)
		}
		return nil
	})
}

// SetStatus updates a document's status, error text and updated_at. A
// superseded document is terminal: a late status write from a stale worker
// is dropped instead of reviving it.
func (s *Store) SetStatus(ctx context.Context, docID string, st Status, errMsg string) error {
	if err := s.requireDocument(ctx, docID); err != nil {
		return err
	}
	err := s.runWrite(ctx, `
MATCH (d:Document {id: $id})
WHERE d.status <> $superseded
SET d.status = $status, d.error = $error, d.updated_at = $now`,
		map[string]any{
			"id": docID, "status": string(st), "error": errMsg,
			"superseded": string(StatusSuperseded),
			"now":        time.Now().UTC().Format(timeLayout),
		})
	if err != nil {
		return fmt.Errorf("graph: set status: %w", err)
	}
	return nil
}

// Supersede replaces a live document with its new version inside one
// transaction: the old version's chunks, mentions and fact edges are
// deleted, the old node is marked superseded, the new node records which
// version it replaced, and entities left without any relationship are
// pruned. It refuses to run when the old version is missing or already
// superseded, or when the new version is missing. Note: entities merged
// without any fact have no edges and are therefore invisible to the
// touched-set GC — the periodic global GC sweeps them.
func (s *Store) Supersede(ctx context.Context, oldID, newID string) error {
	if oldID == newID {
		return fmt.Errorf("graph: supersede %s: old and new are the same document", oldID)
	}
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		st, err := txDocumentStatus(ctx, tx, oldID)
		if err != nil {
			return err
		}
		if st == StatusSuperseded {
			return fmt.Errorf("graph: supersede %s: %w", oldID, ErrSuperseded)
		}
		if _, err := txDocumentStatus(ctx, tx, newID); err != nil {
			return err
		}
		touched, err := txMentionedKeys(ctx, tx, oldID)
		if err != nil {
			return err
		}
		if err := txDeleteSubtree(ctx, tx, oldID); err != nil {
			return err
		}
		if err := txRefreshMentionCounts(ctx, tx, touched); err != nil {
			return err
		}
		now := time.Now().UTC().Format(timeLayout)
		if err := txRun(ctx, tx, `
MATCH (d:Document {id: $old})
SET d.status = $superseded, d.error = '', d.updated_at = $now`,
			map[string]any{"old": oldID, "superseded": string(StatusSuperseded), "now": now}); err != nil {
			return fmt.Errorf("graph: mark superseded: %w", err)
		}
		if err := txRun(ctx, tx, `
MATCH (d:Document {id: $new})
SET d.supersedes = $old`,
			map[string]any{"new": newID, "old": oldID}); err != nil {
			return fmt.Errorf("graph: record supersedes: %w", err)
		}
		return txGC(ctx, tx, touched)
	})
}

// DeleteDocument removes a document with its chunks, mentions and fact
// edges, prunes the entities left without relationships, and clears
// supersedes pointers that reference the removed node, so a surviving
// newer version cannot dangle.
func (s *Store) DeleteDocument(ctx context.Context, id string) error {
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		return txDeleteDocument(ctx, tx, id)
	})
}

// DeleteChain removes every version of the document's identity — the live
// version, its superseded history and their subtrees — inside ONE
// transaction: all-or-nothing. The version set is snapshotted before the
// transaction opens, so a concurrent resubmit of identical content can
// recreate the same content-addressed id after the delete (documented
// race). Returns the number of removed versions; ErrNotFound when the id
// is unknown.
func (s *Store) DeleteChain(ctx context.Context, id string) (int, error) {
	doc, err := s.Document(ctx, id)
	if err != nil {
		return 0, err
	}
	ids, err := s.SameIdentityDocumentIDs(ctx, doc.SourceKind, doc.Source)
	if err != nil {
		return 0, err
	}
	ordered := make([]string, 0, len(ids))
	for did := range ids {
		ordered = append(ordered, did)
	}
	slices.Sort(ordered)
	err = s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		for _, did := range ordered {
			if err := txDeleteDocument(ctx, tx, did); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(ordered), nil
}

// txDeleteDocument deletes one document's mentions, fact edges, chunks and
// node inside an existing transaction, clears supersedes pointers that
// reference it, refreshes the mention counts of the touched entities and
// prunes the ones left without relationships.
func txDeleteDocument(ctx context.Context, tx neo4j.ManagedTransaction, id string) error {
	if _, err := txDocumentStatus(ctx, tx, id); err != nil {
		return err
	}
	touched, err := txMentionedKeys(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := txDeleteSubtree(ctx, tx, id); err != nil {
		return err
	}
	if err := txRun(ctx, tx, `MATCH (d:Document {id: $id}) DETACH DELETE d`,
		map[string]any{"id": id}); err != nil {
		return fmt.Errorf("graph: delete document: %w", err)
	}
	if err := txRun(ctx, tx, `MATCH (d:Document) WHERE d.supersedes = $id SET d.supersedes = ''`,
		map[string]any{"id": id}); err != nil {
		return fmt.Errorf("graph: clear supersedes pointers: %w", err)
	}
	if err := txRefreshMentionCounts(ctx, tx, touched); err != nil {
		return err
	}
	return txGC(ctx, tx, touched)
}

// txDocumentStatus returns a document's status inside a transaction,
// ErrNotFound when the document does not exist.
func txDocumentStatus(ctx context.Context, tx neo4j.ManagedTransaction, id string) (Status, error) {
	res, err := tx.Run(ctx, "MATCH (d:Document {id: $id}) RETURN d.status AS status", map[string]any{"id": id})
	if err != nil {
		return "", fmt.Errorf("graph: document %s: %w", id, err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		return "", fmt.Errorf("graph: document %s: %w", id, err)
	}
	if len(recs) == 0 {
		return "", fmt.Errorf("graph: document %s: %w", id, ErrNotFound)
	}
	return Status(getString(recs[0], "status")), nil
}

// txMentionedKeys returns the distinct entity keys the document's chunks
// mention.
func txMentionedKeys(ctx context.Context, tx neo4j.ManagedTransaction, docID string) ([]string, error) {
	res, err := tx.Run(ctx, `
MATCH (:Document {id: $doc})-[:HAS_CHUNK]->(:Chunk)-[:MENTIONS]->(e:Entity)
RETURN DISTINCT e.key AS key`, map[string]any{"doc": docID})
	if err != nil {
		return nil, fmt.Errorf("graph: mentioned keys: %w", err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("graph: mentioned keys: %w", err)
	}
	keys := make([]string, 0, len(recs))
	for _, r := range recs {
		keys = append(keys, getString(r, "key"))
	}
	return keys, nil
}

// txDeleteSubtree removes a document's mentions, fact edges and chunks (the
// document node itself is left to the caller).
func txDeleteSubtree(ctx context.Context, tx neo4j.ManagedTransaction, docID string) error {
	if err := txRun(ctx, tx, `
MATCH (:Document {id: $doc})-[:HAS_CHUNK]->(:Chunk)-[m:MENTIONS]->(:Entity) DELETE m`,
		map[string]any{"doc": docID}); err != nil {
		return fmt.Errorf("graph: delete mentions: %w", err)
	}
	if err := txRun(ctx, tx, `
MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = $doc DELETE r`,
		map[string]any{"doc": docID}); err != nil {
		return fmt.Errorf("graph: delete facts: %w", err)
	}
	if err := txRun(ctx, tx, `
MATCH (:Document {id: $doc})-[:HAS_CHUNK]->(c:Chunk) DETACH DELETE c`,
		map[string]any{"doc": docID}); err != nil {
		return fmt.Errorf("graph: delete chunks: %w", err)
	}
	return nil
}

// Document returns one document node by id.
func (s *Store) Document(ctx context.Context, id string) (*Document, error) {
	recs, err := s.runRead(ctx, `
MATCH (d:Document {id: $id})
RETURN d.id AS id, d.source_kind AS source_kind, d.source AS source, d.final_url AS final_url,
       d.title AS title, d.hash AS hash, d.status AS status, d.error AS error,
       coalesce(d.labels, '') AS labels,
       d.created_at AS created_at, d.updated_at AS updated_at, d.supersedes AS supersedes`,
		map[string]any{"id": id})
	if err != nil {
		return nil, fmt.Errorf("graph: document %s: %w", id, err)
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("graph: document %s: %w", id, ErrNotFound)
	}
	return recordToDocument(recs[0]), nil
}

// Documents returns documents, most recently updated first; limit <= 0
// returns all.
func (s *Store) Documents(ctx context.Context, limit int) ([]Document, error) {
	q := `
MATCH (d:Document)
RETURN d.id AS id, d.source_kind AS source_kind, d.source AS source, d.final_url AS final_url,
       d.title AS title, d.hash AS hash, d.status AS status, d.error AS error,
       coalesce(d.labels, '') AS labels,
       d.created_at AS created_at, d.updated_at AS updated_at, d.supersedes AS supersedes
ORDER BY d.updated_at DESC`
	params := map[string]any{}
	if limit > 0 {
		q += " LIMIT $limit"
		params["limit"] = limit
	}
	recs, err := s.runRead(ctx, q, params)
	if err != nil {
		return nil, fmt.Errorf("graph: documents: %w", err)
	}
	docs := make([]Document, 0, len(recs))
	for _, r := range recs {
		docs = append(docs, *recordToDocument(r))
	}
	return docs, nil
}

// DocumentsWithStatus returns only the documents in one status, most
// recently updated first; limit <= 0 returns all. The filter runs in the
// query, before the row limit, so a page cannot be emptied by newer
// non-matching documents.
func (s *Store) DocumentsWithStatus(ctx context.Context, status Status, limit int) ([]Document, error) {
	q := `
MATCH (d:Document {status: $status})
RETURN d.id AS id, d.source_kind AS source_kind, d.source AS source, d.final_url AS final_url,
       d.title AS title, d.hash AS hash, d.status AS status, d.error AS error,
       coalesce(d.labels, '') AS labels,
       d.created_at AS created_at, d.updated_at AS updated_at, d.supersedes AS supersedes
ORDER BY d.updated_at DESC`
	params := map[string]any{"status": string(status)}
	if limit > 0 {
		q += " LIMIT $limit"
		params["limit"] = limit
	}
	recs, err := s.runRead(ctx, q, params)
	if err != nil {
		return nil, fmt.Errorf("graph: documents by status: %w", err)
	}
	docs := make([]Document, 0, len(recs))
	for _, r := range recs {
		docs = append(docs, *recordToDocument(r))
	}
	return docs, nil
}

// Chunks returns the document's chunks ordered by sequence. It returns
// ErrNotFound when the document does not exist; a document without chunks
// yields an empty slice.
func (s *Store) Chunks(ctx context.Context, docID string) ([]Chunk, error) {
	if err := s.requireDocument(ctx, docID); err != nil {
		return nil, err
	}
	recs, err := s.runRead(ctx, chunkSelect+`
ORDER BY ch.seq`,
		map[string]any{"docID": docID})
	if err != nil {
		return nil, fmt.Errorf("graph: chunks %s: %w", docID, err)
	}
	chunks := make([]Chunk, 0, len(recs))
	for _, r := range recs {
		chunks = append(chunks, chunkFromRecord(r))
	}
	return chunks, nil
}

const chunkSelect = `
MATCH (d:Document {id: $docID})-[:HAS_CHUNK]->(ch:Chunk)
RETURN ch.id AS id, ch.doc_id AS doc_id, ch.seq AS seq, ch.text AS text, ch.start AS start,
       ch.end AS end, ch.hash AS hash, ch.exact_key AS exact_key, ch.minhash_sig AS sig,
       ch.band_keys AS bands, ch.signature_version AS version, ch.duplicate_of AS duplicate_of`

func chunkFromRecord(r *neo4j.Record) Chunk {
	return Chunk{
		ID:               getString(r, "id"),
		DocID:            getString(r, "doc_id"),
		Seq:              getInt(r, "seq"),
		Text:             getString(r, "text"),
		Start:            getInt(r, "start"),
		End:              getInt(r, "end"),
		Hash:             getString(r, "hash"),
		ExactKey:         getString(r, "exact_key"),
		MinhashSig:       getInt64Slice(r, "sig"),
		BandKeys:         getInt64Slice(r, "bands"),
		SignatureVersion: getInt(r, "version"),
		DuplicateOf:      getString(r, "duplicate_of"),
	}
}

// ChunkByID returns one chunk by id, ErrNotFound when it does not exist.
func (s *Store) ChunkByID(ctx context.Context, chunkID string) (Chunk, error) {
	recs, err := s.runRead(ctx, `
MATCH (ch:Chunk {id: $id})
RETURN ch.id AS id, ch.doc_id AS doc_id, ch.seq AS seq, ch.text AS text, ch.start AS start,
       ch.end AS end, ch.hash AS hash, ch.exact_key AS exact_key, ch.minhash_sig AS sig,
       ch.band_keys AS bands, ch.signature_version AS version, ch.duplicate_of AS duplicate_of`,
		map[string]any{"id": chunkID})
	if err != nil {
		return Chunk{}, fmt.Errorf("graph: chunk %s: %w", chunkID, err)
	}
	if len(recs) == 0 {
		return Chunk{}, fmt.Errorf("graph: chunk %s: %w", chunkID, ErrNotFound)
	}
	return chunkFromRecord(recs[0]), nil
}

// ChunkRef pairs a chunk id with its owning document.
type ChunkRef struct {
	ID    string
	DocID string
}

// ChunkRefsByExactKey returns chunks carrying the exact key, excluding the
// given document ids and chunk id; ordered deterministically so a large
// ineligible crowd cannot mask a valid cross-document duplicate.
func (s *Store) ChunkRefsByExactKey(ctx context.Context, exactKey string, excludeDocIDs []string, selfChunkID string) ([]ChunkRef, error) {
	recs, err := s.runRead(ctx, `
MATCH (ch:Chunk {exact_key: $key})
WHERE ch.id <> $self AND NOT ch.doc_id IN $exclude
RETURN ch.id AS id, ch.doc_id AS doc_id
ORDER BY ch.id ASC
LIMIT 25`,
		map[string]any{"key": exactKey, "self": selfChunkID, "exclude": excludeDocIDs})
	if err != nil {
		return nil, fmt.Errorf("graph: exact key lookup: %w", err)
	}
	refs := make([]ChunkRef, 0, len(recs))
	for _, r := range recs {
		refs = append(refs, ChunkRef{ID: getString(r, "id"), DocID: getString(r, "doc_id")})
	}
	return refs, nil
}

// SameIdentityDocumentIDs returns the ids of every document sharing the
// identity (source_kind, source). Chunks of those documents are not dedup
// targets for a new version of the same source: the old version's chunks
// are deleted by supersede, so skipping extraction against them would lose
// facts.
func (s *Store) SameIdentityDocumentIDs(ctx context.Context, kind, source string) (map[string]bool, error) {
	recs, err := s.runRead(ctx, `
MATCH (d:Document {source_kind: $kind, source: $source})
RETURN d.id AS id`,
		map[string]any{"kind": kind, "source": source})
	if err != nil {
		return nil, fmt.Errorf("graph: same identity documents: %w", err)
	}
	ids := make(map[string]bool, len(recs))
	for _, r := range recs {
		ids[getString(r, "id")] = true
	}
	return ids, nil
}

// OlderLiveVersions returns the live versions of the document's identity
// that were submitted before it (ordered by version_at ascending): exactly
// the versions this one supersedes once merged. Versions submitted after it
// — even if processed earlier — are never superseded.
func (s *Store) OlderLiveVersions(ctx context.Context, docID string) ([]string, error) {
	recs, err := s.runRead(ctx, `
MATCH (d:Document {id: $id})
MATCH (o:Document {source_kind: d.source_kind, source: d.source})
WHERE o.id <> d.id AND o.status <> $superseded
  AND coalesce(o.version_at, o.created_at) < coalesce(d.version_at, d.created_at)
RETURN o.id AS id
ORDER BY coalesce(o.version_at, o.created_at) ASC, o.id ASC`,
		map[string]any{"id": docID, "superseded": string(StatusSuperseded)})
	if err != nil {
		return nil, fmt.Errorf("graph: older live versions: %w", err)
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, getString(r, "id"))
	}
	return ids, nil
}

func recordToDocument(r *neo4j.Record) *Document {
	return &Document{
		ID:         getString(r, "id"),
		SourceKind: getString(r, "source_kind"),
		Source:     getString(r, "source"),
		FinalURL:   getString(r, "final_url"),
		Title:      getString(r, "title"),
		Hash:       getString(r, "hash"),
		Status:     Status(getString(r, "status")),
		Error:      getString(r, "error"),
		Labels:     getString(r, "labels"),
		CreatedAt:  parseTime(getString(r, "created_at")),
		UpdatedAt:  parseTime(getString(r, "updated_at")),
		Supersedes: getString(r, "supersedes"),
	}
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
