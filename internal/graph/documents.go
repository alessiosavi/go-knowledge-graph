package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
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
    d.hash = $hash, d.status = $status, d.error = '', d.updated_at = $now,
    d.supersedes = coalesce(d.supersedes, '')`,
		map[string]any{
			"id": id, "kind": in.Kind, "source": in.Source, "final": in.FinalURL,
			"title": in.Title, "hash": in.Hash, "status": string(StatusReceived),
			"now": time.Now().UTC().Format(timeLayout),
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
// derived chunk id (docID+"-c"+seq), and links them with HAS_CHUNK. It
// returns ErrNotFound when the document does not exist.
func (s *Store) PutChunks(ctx context.Context, docID string, chunks []Chunk) error {
	if err := s.requireDocument(ctx, docID); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
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
	err := s.runWrite(ctx, `
MATCH (d:Document {id: $docID})
UNWIND $chunks AS c
MERGE (ch:Chunk {id: c.id})
SET ch.doc_id = $docID, ch.seq = c.seq, ch.text = c.text, ch.start = c.start, ch.end = c.end,
    ch.hash = c.hash, ch.exact_key = c.exact_key, ch.minhash_sig = c.sig, ch.band_keys = c.bands,
    ch.signature_version = c.version, ch.duplicate_of = c.dup
MERGE (d)-[h:HAS_CHUNK]->(ch)
SET h.seq = c.seq`,
		map[string]any{"docID": docID, "chunks": rows})
	if err != nil {
		return fmt.Errorf("graph: put chunks: %w", err)
	}
	return nil
}

// SetStatus updates a document's status, error text and updated_at.
func (s *Store) SetStatus(ctx context.Context, docID string, st Status, errMsg string) error {
	if err := s.requireDocument(ctx, docID); err != nil {
		return err
	}
	err := s.runWrite(ctx, `
MATCH (d:Document {id: $id})
SET d.status = $status, d.error = $error, d.updated_at = $now`,
		map[string]any{
			"id": docID, "status": string(st), "error": errMsg,
			"now": time.Now().UTC().Format(timeLayout),
		})
	if err != nil {
		return fmt.Errorf("graph: set status: %w", err)
	}
	return nil
}

// Document returns one document node by id.
func (s *Store) Document(ctx context.Context, id string) (*Document, error) {
	recs, err := s.runRead(ctx, `
MATCH (d:Document {id: $id})
RETURN d.id AS id, d.source_kind AS source_kind, d.source AS source, d.final_url AS final_url,
       d.title AS title, d.hash AS hash, d.status AS status, d.error AS error,
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

// Chunks returns the document's chunks ordered by sequence. It returns
// ErrNotFound when the document does not exist; a document without chunks
// yields an empty slice.
func (s *Store) Chunks(ctx context.Context, docID string) ([]Chunk, error) {
	if err := s.requireDocument(ctx, docID); err != nil {
		return nil, err
	}
	recs, err := s.runRead(ctx, `
MATCH (d:Document {id: $docID})-[:HAS_CHUNK]->(ch:Chunk)
RETURN ch.id AS id, ch.doc_id AS doc_id, ch.seq AS seq, ch.text AS text, ch.start AS start,
       ch.end AS end, ch.hash AS hash, ch.exact_key AS exact_key, ch.minhash_sig AS sig,
       ch.band_keys AS bands, ch.signature_version AS version, ch.duplicate_of AS duplicate_of
ORDER BY ch.seq`,
		map[string]any{"docID": docID})
	if err != nil {
		return nil, fmt.Errorf("graph: chunks %s: %w", docID, err)
	}
	chunks := make([]Chunk, 0, len(recs))
	for _, r := range recs {
		chunks = append(chunks, Chunk{
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
		})
	}
	return chunks, nil
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
