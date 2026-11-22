package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
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
//     supplied document date can still update the node in place. A
//     document in status failed does not answer Noop: resubmitting its
//     content retries it (the node is reused, status resets to received).
//   - same source + changed content → new document id, PreviousID names the
//     newest live version other than the new one; supersede itself runs
//     later, after the new version has merged.
//   - upserts are serialized per Store so concurrent submits cannot race
//     the identity pre-read.
func (s *Store) UpsertDocument(ctx context.Context, in DocumentInput) (UpsertResult, error) {
	if in.Domain != "" && !ValidDomain(in.Domain) {
		return UpsertResult{}, fmt.Errorf("graph: invalid domain slug %q", in.Domain)
	}
	if in.ProductRefs != nil && !in.productRefsValidated {
		refs, err := ValidateProductRefs(in.ProductRefs)
		if err != nil {
			return UpsertResult{}, err
		}
		in.ProductRefs = refs
	}
	id := DocumentID(in.Kind, in.Source, in.Hash)
	var result UpsertResult
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		var err error
		result, err = txUpsertDocument(ctx, tx, id, in)
		return err
	})
	if err != nil {
		return UpsertResult{}, fmt.Errorf("graph: upsert document: %w", err)
	}
	return result, nil
}

func txUpsertDocument(ctx context.Context, tx neo4j.ManagedTransaction, id string, in DocumentInput) (UpsertResult, error) {
	current, previous, newestDone, err := txDocumentVersions(ctx, tx, id, in.Kind, in.Source)
	if err != nil {
		return UpsertResult{}, err
	}
	reuse := current != nil && current.status != StatusSuperseded
	liveMatch := reuse && current.status != StatusFailed
	if liveMatch && !in.Force {
		if err := CheckProductRefsNoop(in.ProductRefs, current.refs); err != nil {
			return UpsertResult{}, err
		}
		if in.DocumentDate != "" && in.DocumentDate != current.documentDate {
			if err := txRun(ctx, tx, `
MATCH (d:Document {id:$id}) SET d.document_date = $date`,
				map[string]any{"id": id, "date": in.DocumentDate}); err != nil {
				return UpsertResult{}, err
			}
		}
		return UpsertResult{ID: id, Noop: true}, nil
	}

	stored := newestDone.refs
	if current != nil {
		stored = current.refs
	}
	refs := in.ProductRefs
	if refs == nil {
		refs = newestDone.refs
		if reuse {
			refs = current.refs
		}
	}
	domain := in.Domain
	if liveMatch || in.productRefsValidated {
		// Force preserves the domain unless a changed set supplies a pin.
		domain = ""
	}
	if len(in.ProductRefs) > 0 && !slices.Equal(in.ProductRefs, stored) {
		pin := in.Domain
		if !in.productRefsValidated {
			pin, err = productRefDomain(in.ProductRefs, func(query string, params map[string]any) ([]*neo4j.Record, error) {
				return txRows(ctx, tx, query, params)
			})
			if err != nil {
				return UpsertResult{}, err
			}
		}
		if pin != "" {
			if reuse {
				if err := checkProductRefPin(pin, current.domain, current.status); err != nil {
					return UpsertResult{}, err
				}
			}
			if err := txCheckProductRefDomain(ctx, tx, pin); err != nil {
				return UpsertResult{}, err
			}
			domain = pin
		}
	}

	params := map[string]any{
		"id": id, "kind": in.Kind, "source": in.Source, "final": in.FinalURL,
		"title": in.Title, "title_source": in.TitleSource, "hash": in.Hash, "labels": in.Labels,
		"status": string(StatusReceived), "preserve_version": liveMatch,
		"domain": domain, "domain_tok": domainTokenOrNil(domain),
		"now": time.Now().UTC().Format(timeLayout), "text_clean": textCleanProperties(in),
		"spool_digest":  in.SpoolPayloadDigest,
		"document_date": in.DocumentDate,
	}
	if err := txRun(ctx, tx, `
MERGE (d:Document {id: $id})
ON CREATE SET d.created_at = $now
SET d.source_kind = $kind, d.source = $source, d.final_url = $final, d.title = $title,
    d.title_source = $title_source, d.hash = $hash, d.labels = $labels,
    d.status = $status, d.error = '', d.updated_at = $now,
    d.version_at = CASE WHEN $preserve_version THEN d.version_at ELSE $now END,
    d.document_date = CASE WHEN $document_date <> '' THEN $document_date ELSE d.document_date END,
    d.supersedes = coalesce(d.supersedes, ''),
    d.domain = CASE WHEN $domain <> '' THEN $domain ELSE d.domain END,
    d.domain_tok = CASE WHEN $domain <> '' THEN $domain_tok ELSE d.domain_tok END,
    d.spool_payload_digest = $spool_digest
SET d += $text_clean`, params); err != nil {
		return UpsertResult{}, err
	}
	if err := txReplaceProductRefs(ctx, tx, id, refs); err != nil {
		return UpsertResult{}, err
	}
	if liveMatch {
		previous = ""
	}
	return UpsertResult{ID: id, PreviousID: previous}, nil
}

func textCleanProperties(in DocumentInput) map[string]any {
	if in.TextCleanVersion == 0 {
		return map[string]any{}
	}
	return map[string]any{
		"text_clean_version":  in.TextCleanVersion,
		"text_invalid_utf8":   in.TextClean.InvalidUTF8,
		"text_controls":       in.TextClean.Controls,
		"text_bidi_controls":  in.TextClean.BidiControls,
		"text_glyph_lines":    in.TextClean.GlyphLines,
		"text_leader_runs":    in.TextClean.LeaderRuns,
		"text_fffd_removed":   in.TextClean.FFFDRemoved,
		"text_fffd_remaining": in.TextClean.FFFDRemaining,
	}
}

type documentVersion struct {
	status       Status
	domain       string
	refs         []string
	documentDate string
}

// txDocumentVersions reads the exact version (including superseded history),
// the previous live ID in creation order, and the latest live done version
// in submission order. A reused ID can have a new version_at.
func txDocumentVersions(ctx context.Context, tx neo4j.ManagedTransaction, id, kind, source string) (current *documentVersion, previous string, done documentVersion, err error) {
	rows, err := txRows(ctx, tx, `
MATCH (d:Document {source_kind:$kind, source:$source})
WHERE d.status <> 'superseded' OR d.id = $id
RETURN d.id AS id, d.status AS status, d.domain AS domain, d.product_refs_raw AS refs,
       d.document_date AS document_date, coalesce(d.version_at, d.created_at) AS version_at
ORDER BY d.created_at DESC, d.id DESC`, map[string]any{"id": id, "kind": kind, "source": source})
	if err != nil {
		return nil, "", done, err
	}
	var newestAt, newestID string
	for _, row := range rows {
		rowID := getString(row, "id")
		v := documentVersion{
			status: Status(getString(row, "status")), domain: getString(row, "domain"),
			refs:         getStringSlice(row, "refs"),
			documentDate: getString(row, "document_date"),
		}
		if rowID == id {
			current = &v
		} else if previous == "" {
			previous = rowID
		}
		if v.status == StatusDone {
			at := getString(row, "version_at")
			if at > newestAt || (at == newestAt && rowID > newestID) {
				done, newestAt, newestID = v, at, rowID
			}
		}
	}
	return current, previous, done, nil
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

// ChunkID returns the document-scoped sequence number's globally unique id.
func ChunkID(docID string, seq int) string {
	return docID + "-c" + strconv.Itoa(seq)
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
			"id":        ChunkID(docID, c.Seq),
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
    ch.signature_version = c.version, ch.duplicate_of = c.dup,
    ch.domain = d.domain, ch.domain_tok = d.domain_tok
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
WITH stale.id AS cid
MATCH (f:FactCandidate {chunk_id: cid}) DELETE f`,
			map[string]any{"docID": docID, "from": fromSeq}); err != nil {
			return fmt.Errorf("graph: prune stale fact candidates: %w", err)
		}
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

// DoneDocumentIDs lists the completed documents eligible for rebuilding.
func (s *Store) DoneDocumentIDs(ctx context.Context) ([]string, error) {
	recs, err := s.runRead(ctx, `
MATCH (d:Document {status: $done}) RETURN d.id AS id ORDER BY d.id`,
		map[string]any{"done": string(StatusDone)})
	if err != nil {
		return nil, fmt.Errorf("graph: done document IDs: %w", err)
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, getString(r, "id"))
	}
	return ids, nil
}

// ResetExtraction removes a done document's derived extraction in one locked
// transaction, preserving its content and chunks for re-extraction. The caller
// must also hold the pipeline's document lock to exclude an active worker.
// Empty labels preserve the document's current preset.
func (s *Store) ResetExtraction(ctx context.Context, docID, labels string) error {
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		return txResetExtraction(ctx, tx, docID, labels)
	})
}

// txResetExtraction shares the complete reset with an atomic admin move.
func txResetExtraction(ctx context.Context, tx neo4j.ManagedTransaction, docID, labels string) error {
	status, err := txDocumentStatus(ctx, tx, docID)
	if err != nil {
		return err
	}
	if status != StatusDone {
		return fmt.Errorf("graph: reset document %s (%s): %w", docID, status, ErrNotDone)
	}
	touched, err := txMentionedKeys(ctx, tx, docID)
	if err != nil {
		return err
	}
	// Include legacy fact endpoints that may lack MENTIONS edges.
	params := map[string]any{"doc": docID}
	res, err := tx.Run(ctx, `
MATCH (a:Entity)-[r]->(b:Entity) WHERE r.doc_id = $doc
RETURN a.key AS head, b.key AS tail`, params)
	if err != nil {
		return fmt.Errorf("graph: reset fact keys: %w", err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		return err
	}
	for _, r := range recs {
		touched = append(touched, getString(r, "head"), getString(r, "tail"))
	}
	slices.Sort(touched)
	touched = slices.Compact(touched)
	for _, query := range []string{
		`MATCH (:Document {id: $doc})-[:HAS_CHUNK]->(:Chunk)-[m:MENTIONS]->(:Entity) DELETE m`,
		`MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = $doc DELETE r`,
		`MATCH (r:RejectedFact {doc_id: $doc}) DETACH DELETE r`,
		`MATCH (f:FactCandidate {doc_id: $doc}) DELETE f`,
	} {
		if err := txRun(ctx, tx, query, params); err != nil {
			return fmt.Errorf("graph: reset extraction: %w", err)
		}
	}
	if err := txRefreshMentionCounts(ctx, tx, touched); err != nil {
		return err
	}
	if err := txGC(ctx, tx, touched); err != nil {
		return err
	}
	return txRun(ctx, tx, `
MATCH (d:Document {id: $doc})
SET d.labels = CASE WHEN $labels = '' THEN coalesce(d.labels, '') ELSE $labels END,
    d.status = $chunked, d.updated_at = $now`, map[string]any{
		"doc": docID, "labels": labels, "chunked": string(StatusChunked),
		"now": time.Now().UTC().Format(timeLayout),
	})
}

// SetStatus updates a document's status, error text and updated_at. A
// superseded document is terminal: a late status write from a stale worker
// is dropped instead of reviving it.
func (s *Store) SetStatus(ctx context.Context, docID string, st Status, errMsg string) error {
	if st == StatusDone {
		_, err := s.finishDocument(ctx, docID, false, errMsg)
		return err
	}
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
// without mentions or facts have no edges and are therefore invisible to the
// touched-set GC — the periodic global GC sweeps them.
func (s *Store) Supersede(ctx context.Context, oldID, newID string) error {
	if oldID == newID {
		return fmt.Errorf("graph: supersede %s: old and new are the same document", oldID)
	}
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		if _, err := txDocumentStatus(ctx, tx, newID); err != nil {
			return err
		}
		return txSupersede(ctx, tx, oldID, newID)
	})
}

func txSupersede(ctx context.Context, tx neo4j.ManagedTransaction, oldID, newID string) error {
	st, err := txDocumentStatus(ctx, tx, oldID)
	if err != nil {
		return err
	}
	if st == StatusSuperseded {
		return fmt.Errorf("graph: supersede %s: %w", oldID, ErrSuperseded)
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
MATCH (replacement:Document {id: $new})
SET replacement.supersedes = $old
WITH replacement WHERE replacement.status = 'done'
MATCH (:Document {id: $old})-[r:REFS]->(:ProductRef)
DELETE r`,
		map[string]any{"new": newID, "old": oldID}); err != nil {
		return fmt.Errorf("graph: record supersedes: %w", err)
	}
	return txGC(ctx, tx, touched)
}

// FinishDocument publishes a processed version and retires its older
// versions atomically, so the prior done versions and their refs stay
// visible until the replacement commits. The result lists the retired IDs.
func (s *Store) FinishDocument(ctx context.Context, id string) ([]string, error) {
	return s.finishDocument(ctx, id, true, "")
}

func (s *Store) finishDocument(ctx context.Context, id string, supersede bool, errMsg string) ([]string, error) {
	var retired []string
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		var err error
		retired, err = txFinishDocument(ctx, tx, id, supersede, errMsg)
		return err
	})
	return retired, err
}

func txFinishDocument(ctx context.Context, tx neo4j.ManagedTransaction, id string, supersede bool, errMsg string) ([]string, error) {
	st, err := txDocumentStatus(ctx, tx, id)
	if err != nil || st == StatusSuperseded {
		return nil, err
	}
	var retired []string
	if supersede {
		if st != StatusMerged && st != StatusVerifying && st != StatusDone {
			return nil, fmt.Errorf("graph: cannot finish document %s in status %q", id, st)
		}
		// Select and retire old IDs in one snapshot. A superseded ID
		// resubmitted in the meantime has a newer version_at and is spared.
		rows, err := txRows(ctx, tx, olderLiveVersionsQuery, map[string]any{"id": id})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			old := getString(row, "id")
			if err := txSupersede(ctx, tx, old, id); err != nil {
				return nil, err
			}
			retired = append(retired, old)
		}
	}
	if err := txRun(ctx, tx, `
MATCH (d:Document {id:$id})
SET d.status = 'done', d.error = $error, d.updated_at = $now`,
		map[string]any{"id": id, "error": errMsg, "now": time.Now().UTC().Format(timeLayout)}); err != nil {
		return nil, err
	}
	// Include already-superseded versions in case an earlier worker or
	// standalone Supersede retired them before this replacement finished.
	err = txRun(ctx, tx, `
MATCH (d:Document {id:$id})
MATCH (o:Document {source_kind:d.source_kind, source:d.source, status:'superseded'})-[r:REFS]->(:ProductRef)
WHERE coalesce(o.version_at, o.created_at) < coalesce(d.version_at, d.created_at)
DELETE r`, map[string]any{"id": id})
	if err != nil {
		return nil, err
	}
	return retired, nil
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

// txDeleteDocument deletes one document's mentions, fact edges, chunks,
// cached judge verdicts, rejected-fact audits and node inside an existing
// transaction, clears supersedes pointers that reference it, refreshes the
// mention counts of the touched entities and prunes the ones left without
// relationships.
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
	// Cached judge verdicts and rejected-fact audits hold text derived from
	// the document's evidence (reasons), so they go with it.
	if err := txRun(ctx, tx, `MATCH (v:JudgeVerdict {doc_id: $id}) DELETE v`,
		map[string]any{"id": id}); err != nil {
		return fmt.Errorf("graph: delete judge verdicts: %w", err)
	}
	if err := txRun(ctx, tx, `MATCH (r:RejectedFact {doc_id: $id}) DETACH DELETE r`,
		map[string]any{"id": id}); err != nil {
		return fmt.Errorf("graph: delete rejected-fact audits: %w", err)
	}
	if err := txRun(ctx, tx, `MATCH (f:FactCandidate {doc_id: $id}) DELETE f`,
		map[string]any{"id": id}); err != nil {
		return fmt.Errorf("graph: delete fact candidates: %w", err)
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
	if err := txRun(ctx, tx, `MATCH (f:FactCandidate {doc_id: $doc}) DELETE f`,
		map[string]any{"doc": docID}); err != nil {
		return fmt.Errorf("graph: delete fact candidates: %w", err)
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
       coalesce(d.title, '') AS title, coalesce(d.title_source, '') AS title_source,
       d.hash AS hash, d.status AS status, d.error AS error,
       coalesce(d.labels, '') AS labels, d.domain AS domain,
       d.product_refs_raw AS product_refs_raw, d.spool_payload_digest AS spool_payload_digest,
       d.created_at AS created_at, d.updated_at AS updated_at, d.supersedes AS supersedes,
       d.document_date AS raw_document_date, `+documentDateColumns("d"),
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
	return s.DocumentsIn(ctx, "", "", limit)
}

func (s *Store) DocumentsWithStatus(ctx context.Context, status Status, limit int) ([]Document, error) {
	return s.DocumentsIn(ctx, status, "", limit)
}

// DocumentsIn applies both filters before the limit; empty values select all.
func (s *Store) DocumentsIn(ctx context.Context, status Status, slug string, limit int) ([]Document, error) {
	if slug != "" && !ValidDomain(slug) {
		return nil, argerr.Errorf("graph: invalid domain slug %q", slug)
	}
	q := "MATCH (d:Document)"
	var filters []string
	if status != "" {
		filters = append(filters, "d.status = $status")
	}
	if slug != "" {
		filters = append(filters, "d.domain = $domain")
	}
	if len(filters) > 0 {
		q += " WHERE " + strings.Join(filters, " AND ")
	}
	q += `
 RETURN d.id AS id, d.source_kind AS source_kind, d.source AS source, d.final_url AS final_url,
 coalesce(d.title, '') AS title, coalesce(d.title_source, '') AS title_source,
 d.hash AS hash, d.status AS status, d.error AS error,
 coalesce(d.labels, '') AS labels, d.domain AS domain,
 d.product_refs_raw AS product_refs_raw,
 d.created_at AS created_at, d.updated_at AS updated_at, d.supersedes AS supersedes,
 d.document_date AS raw_document_date, ` + documentDateColumns("d") + `
 ORDER BY d.updated_at DESC`
	params := map[string]any{"status": string(status), "domain": slug}
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
       ch.band_keys AS bands, ch.signature_version AS version, ch.duplicate_of AS duplicate_of, ch.domain AS domain`

func chunkFromRecord(r *neo4j.Record) Chunk {
	return Chunk{
		ID:               getString(r, "id"),
		Domain:           getString(r, "domain"),
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
       ch.band_keys AS bands, ch.signature_version AS version, ch.duplicate_of AS duplicate_of, ch.domain AS domain`,
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

const olderLiveVersionsQuery = `
MATCH (d:Document {id: $id})
MATCH (o:Document {source_kind: d.source_kind, source: d.source})
WHERE o.id <> d.id AND o.status <> 'superseded'
  AND coalesce(o.version_at, o.created_at) < coalesce(d.version_at, d.created_at)
RETURN o.id AS id
ORDER BY coalesce(o.version_at, o.created_at) ASC, o.id ASC`

// OlderLiveVersions returns the live versions of the document's identity
// that were submitted before it (ordered by version_at ascending): exactly
// the versions this one supersedes once merged. Versions submitted after it
// — even if processed earlier — are never superseded.
func (s *Store) OlderLiveVersions(ctx context.Context, docID string) ([]string, error) {
	recs, err := s.runRead(ctx, olderLiveVersionsQuery, map[string]any{"id": docID})
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
	doc := &Document{
		ID:                 getString(r, "id"),
		Domain:             getString(r, "domain"),
		SourceKind:         getString(r, "source_kind"),
		Source:             getString(r, "source"),
		FinalURL:           getString(r, "final_url"),
		Title:              getString(r, "title"),
		TitleSource:        getString(r, "title_source"),
		Hash:               getString(r, "hash"),
		Status:             Status(getString(r, "status")),
		Error:              getString(r, "error"),
		Labels:             getString(r, "labels"),
		CreatedAt:          parseTime(getString(r, "created_at")),
		UpdatedAt:          parseTime(getString(r, "updated_at")),
		Supersedes:         getString(r, "supersedes"),
		RawDocumentDate:    getString(r, "raw_document_date"),
		ProductRefs:        getStringSlice(r, "product_refs_raw"),
		SpoolPayloadDigest: getString(r, "spool_payload_digest"),
	}
	if date := parseTime(getString(r, "document_date")).UTC(); !date.IsZero() {
		doc.DocumentDate = &date
	}
	if origin := getString(r, "document_date_source"); origin != "" {
		doc.DocumentDateSource = &origin
	}
	return doc
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ChunkNeighbourhood returns the requested chunk followed separately by its
// same-document neighbours within n sequence positions, ordered by sequence.
// Only ID, DocID, Seq, Text and Domain are populated.
// A missing center returns ErrNotFound.
func (s *Store) ChunkNeighbourhood(ctx context.Context, id string, n int) (center Chunk, around []Chunk, err error) {
	recs, err := s.runRead(ctx, `
MATCH (c:Chunk {id: $id})
OPTIONAL MATCH (:Document {id: c.doc_id})-[:HAS_CHUNK]->(ch:Chunk)
WHERE ch.seq >= c.seq - $n AND ch.seq <= c.seq + $n AND ch.id <> c.id
RETURN c.id AS center_id, c.doc_id AS center_doc_id, c.seq AS center_seq, c.text AS center_text, c.domain AS center_domain,
       ch.id AS id, ch.doc_id AS doc_id, ch.seq AS seq, ch.text AS text, ch.domain AS domain
ORDER BY ch.seq`, map[string]any{"id": id, "n": n})
	if err != nil {
		return Chunk{}, nil, fmt.Errorf("graph: chunk neighbourhood %s: %w", id, err)
	}
	if len(recs) == 0 {
		return Chunk{}, nil, fmt.Errorf("graph: chunk %s: %w", id, ErrNotFound)
	}
	center = Chunk{
		ID: getString(recs[0], "center_id"), DocID: getString(recs[0], "center_doc_id"),
		Seq: getInt(recs[0], "center_seq"), Text: getString(recs[0], "center_text"), Domain: getString(recs[0], "center_domain"),
	}
	around = []Chunk{}
	for _, r := range recs {
		if getString(r, "id") != "" {
			around = append(around, Chunk{
				ID: getString(r, "id"), DocID: getString(r, "doc_id"),
				Seq: getInt(r, "seq"), Text: getString(r, "text"), Domain: getString(r, "domain"),
			})
		}
	}
	return center, around, nil
}

// GoldChunk identifies an exact quote match in a completed document.
type GoldChunk struct{ ChunkID, DocID, Source, Domain string }

// ChunksContaining returns every case-sensitive substring match in done
// documents. An empty domain searches all domains.
func (s *Store) ChunksContaining(ctx context.Context, quote, domain string) ([]GoldChunk, error) {
	recs, err := s.runRead(ctx, `
MATCH (d:Document)-[:HAS_CHUNK]->(ch:Chunk)
WHERE d.status = $done AND ch.text CONTAINS $quote
  AND ($domain = '' OR d.domain = $domain)
RETURN ch.id AS chunk_id, d.id AS doc_id, d.source AS source, d.domain AS domain
ORDER BY ch.id`, map[string]any{"done": string(StatusDone), "quote": quote, "domain": domain})
	if err != nil {
		return nil, fmt.Errorf("graph: chunks containing: %w", err)
	}
	out := make([]GoldChunk, 0, len(recs))
	for _, r := range recs {
		out = append(out, GoldChunk{ChunkID: getString(r, "chunk_id"), DocID: getString(r, "doc_id"), Source: getString(r, "source"), Domain: getString(r, "domain")})
	}
	return out, nil
}

// DoneChunkTexts visits completed-document texts in id order, retaining at
// most 1,000 rows per batch. Callback and database failures stop the scan.
func (s *Store) DoneChunkTexts(ctx context.Context, domain string, fn func(chunkID, text string) error) error {
	last := ""
	for {
		recs, err := s.runRead(ctx, `
MATCH (d:Document)-[:HAS_CHUNK]->(ch:Chunk)
WHERE d.status = $done AND ch.id > $last
  AND ($domain = '' OR d.domain = $domain)
RETURN ch.id AS id, ch.text AS text
ORDER BY ch.id LIMIT 1000`, map[string]any{"done": string(StatusDone), "domain": domain, "last": last})
		if err != nil {
			return fmt.Errorf("graph: done chunk texts: %w", err)
		}
		if len(recs) > 0 {
			last = getString(recs[len(recs)-1], "id")
			if last == "" {
				return fmt.Errorf("graph: done chunk texts: empty chunk id at end of page")
			}
		}
		for _, r := range recs {
			if err := fn(getString(r, "id"), getString(r, "text")); err != nil {
				return err
			}
		}
		if len(recs) < 1000 {
			return nil
		}
	}
}

// SetDocumentHygiene replaces the current extraction's durable counts. A retry
// replaces the map rather than adding to counts from an earlier attempt.
func (s *Store) SetDocumentHygiene(ctx context.Context, docID string, counts map[string]int) error {
	values := make(map[string]any, len(counts))
	for rule, n := range counts {
		values[rule] = n
	}
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		status, err := txDocumentStatus(ctx, tx, docID)
		if err != nil {
			return err
		}
		if status == StatusSuperseded {
			return fmt.Errorf("graph: document %s: %w", docID, ErrSuperseded)
		}
		if err := txRun(ctx, tx, `MATCH (d:Document {id: $id}) SET d.hygiene = $counts`, map[string]any{"id": docID, "counts": values}); err != nil {
			return fmt.Errorf("graph: set document hygiene: %w", err)
		}
		return nil
	})
}
