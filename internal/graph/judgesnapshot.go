package graph

import (
	"context"
	"fmt"
	"slices"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// FactCandidate carries the persisted judge input and its guarded identity.
type FactCandidate struct {
	ID, InputHash, JudgeInput, DocID, ChunkID, Domain string
	HeadKey, Relation, TailKey, ExtractorVersion      string
	Confidence                                        float64
}

// UnjudgedCandidates returns candidates with an empty judge status in ascending id order.
// Document status is deliberately irrelevant: verification can be interrupted
// after merge, leaving candidates on a document that recovery marks done.
// An empty domain or zero limit means unrestricted.
func (s *Store) UnjudgedCandidates(ctx context.Context, domain string, limit int) ([]FactCandidate, error) {
	if limit < 0 {
		return nil, fmt.Errorf("graph: candidate limit must be non-negative")
	}
	q := `MATCH (c:FactCandidate)
WHERE c.judge = '' AND ($domain = '' OR coalesce(c.domain, '') = $domain)
RETURN c.id AS id, c.input_hash AS input_hash, c.judge_input AS judge_input,
 c.doc_id AS doc_id, c.chunk_id AS chunk_id, coalesce(c.domain, '') AS domain,
 c.head_key AS head_key, c.rel AS rel, c.tail_key AS tail_key,
 c.confidence AS confidence, c.extractor_version AS extractor_version
ORDER BY c.id`
	if limit > 0 {
		q += " LIMIT $limit"
	}
	rows, err := s.runRead(ctx, q, map[string]any{"domain": domain, "limit": limit})
	if err != nil {
		return nil, fmt.Errorf("graph: unjudged candidates: %w", err)
	}
	out := make([]FactCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, FactCandidate{ID: getString(r, "id"), InputHash: getString(r, "input_hash"), JudgeInput: getString(r, "judge_input"), DocID: getString(r, "doc_id"), ChunkID: getString(r, "chunk_id"), Domain: getString(r, "domain"), HeadKey: getString(r, "head_key"), Relation: getString(r, "rel"), TailKey: getString(r, "tail_key"), Confidence: getFloat(r, "confidence"), ExtractorVersion: getString(r, "extractor_version")})
	}
	return out, nil
}

// DeleteOrphanedCandidate removes only an unchanged candidate with a missing
// endpoint. It distinguishes failed promotion from a vanished or changed input.
func (s *Store) DeleteOrphanedCandidate(ctx context.Context, id, inputHash string) (bool, error) {
	deleted := false
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		deleted = false
		res, err := tx.Run(ctx, `MATCH (c:FactCandidate {id: $id}) WHERE c.input_hash = $hash
OPTIONAL MATCH (h:Entity {key: c.head_key})
OPTIONAL MATCH (t:Entity {key: c.tail_key})
WITH c, h, t WHERE h IS NULL OR t IS NULL
DELETE c RETURN count(*) AS deleted`, map[string]any{"id": id, "hash": inputHash})
		if err != nil {
			return fmt.Errorf("graph: delete orphaned candidate: %w", err)
		}
		rows, err := res.Collect(ctx)
		if err != nil {
			return fmt.Errorf("graph: delete orphaned candidate: %w", err)
		}
		deleted = len(rows) > 0 && getInt(rows[0], "deleted") > 0
		return nil
	})
	return deleted && err == nil, err
}

// TypedFact holds an edge identity, with evidence populated only after sampling.
type TypedFact struct {
	ID, Domain, HeadKey, Relation, TailKey, ChunkID string
	Head, Tail, Text                                string
	HeadStart, HeadEnd                              int
}

// TypedFactsSnapshot reads confirmed edge identities in one ordered query.
// Names, chunk text and spans are loaded only for the selected sample.
func (s *Store) TypedFactsSnapshot(ctx context.Context) ([]TypedFact, error) {
	rows, err := s.runRead(ctx, `MATCH (h:Entity)-[r]->(t:Entity)
WHERE coalesce(r.judge, '') = 'confirmed'
RETURN h.key AS head_key, type(r) AS rel, t.key AS tail_key, coalesce(r.chunk_id, '') AS chunk_id,
 coalesce(h.domain, '') AS domain
ORDER BY head_key, rel, tail_key, chunk_id`, nil)
	if err != nil {
		return nil, fmt.Errorf("graph: typed facts snapshot: %w", err)
	}
	out := make([]TypedFact, 0, len(rows))
	for _, r := range rows {
		f := TypedFact{HeadKey: getString(r, "head_key"), Relation: getString(r, "rel"), TailKey: getString(r, "tail_key"), ChunkID: getString(r, "chunk_id"), Domain: getString(r, "domain")}
		f.ID = FactAuditID(f.HeadKey, f.Relation, f.TailKey, f.ChunkID)
		out = append(out, f)
	}
	return out, nil
}

// TypedFactsEvidence fetches names, text and spans for selected identities in a
// single query, retaining their snapshot order and domain.
func (s *Store) TypedFactsEvidence(ctx context.Context, facts []TypedFact) ([]TypedFact, error) {
	if len(facts) == 0 {
		return facts, nil
	}
	selected := make([]map[string]any, 0, len(facts))
	for i, f := range facts {
		selected = append(selected, map[string]any{"index": i, "head_key": f.HeadKey, "rel": f.Relation, "tail_key": f.TailKey, "chunk_id": f.ChunkID})
	}
	rows, err := s.runRead(ctx, `UNWIND $selected AS f
OPTIONAL MATCH (h:Entity {key: f.head_key})-[r]->(t:Entity {key: f.tail_key})
WHERE type(r) = f.rel AND coalesce(r.chunk_id, '') = f.chunk_id
OPTIONAL MATCH (c:Chunk {id: f.chunk_id})
RETURN f.index AS idx, coalesce(h.name, f.head_key) AS head, coalesce(t.name, f.tail_key) AS tail,
 coalesce(c.text, '') AS text, coalesce(r.head_start, 0) AS head_start, coalesce(r.head_end, 0) AS head_end
ORDER BY idx`, map[string]any{"selected": selected})
	if err != nil {
		return nil, fmt.Errorf("graph: typed facts evidence: %w", err)
	}
	out := slices.Clone(facts)
	for _, r := range rows {
		f := &out[getInt(r, "idx")]
		f.Head, f.Tail, f.Text = getString(r, "head"), getString(r, "tail"), getString(r, "text")
		f.HeadStart, f.HeadEnd = getInt(r, "head_start"), getInt(r, "head_end")
	}
	return out, nil
}

// StoredJudgeEdge carries the exact v1 user message and the identity used for
// keyset pagination and guarded application.
type StoredJudgeEdge struct {
	StoredFactRef
	DocID, JudgeInput string
}

// PendingStoredJudgeEdges reads up to limit pending typed edges after the
// supplied cursor, ordered by immutable properties. A nil cursor starts a run.
func (s *Store) PendingStoredJudgeEdges(ctx context.Context, domain string, after *StoredFactRef, limit int) ([]StoredJudgeEdge, error) {
	const query = `
MATCH (h:Entity)-[r]->(t:Entity)
WHERE r.judge = '' AND r.judge_input <> ''
MATCH (d:Document {id: r.doc_id})
WHERE d.status = 'done' AND ($domain = '' OR d.domain = $domain)
WITH coalesce(r.input_hash, '') AS hash, coalesce(h.key, '') AS head,
     type(r) AS rel, coalesce(t.key, '') AS tail, coalesce(r.chunk_id, '') AS chunk,
     coalesce(r.judge_gen, '') AS gen, r.doc_id AS doc, r.judge_input AS input
WHERE NOT $has_cursor OR hash > $hash OR (hash = $hash AND (
  head > $head OR (head = $head AND (
  rel > $rel OR (rel = $rel AND (
  tail > $tail OR (tail = $tail AND (
  chunk > $chunk OR (chunk = $chunk AND gen > $gen)))))))))
RETURN hash, head, rel, tail, chunk, gen, doc, input
ORDER BY hash, head, rel, tail, chunk, gen LIMIT $limit`
	var cursor StoredFactRef
	if after != nil {
		cursor = *after
	}
	rows, err := s.runRead(ctx, query, map[string]any{
		"domain": domain, "limit": limit, "has_cursor": after != nil,
		"hash": cursor.InputHash, "head": cursor.HeadKey, "rel": cursor.Rel,
		"tail": cursor.TailKey, "chunk": cursor.ChunkID, "gen": cursor.JudgeGen,
	})
	if err != nil {
		return nil, fmt.Errorf("graph: pending stored judge edges: %w", err)
	}
	edges := make([]StoredJudgeEdge, 0, len(rows))
	for _, row := range rows {
		edges = append(edges, StoredJudgeEdge{
			StoredFactRef: StoredFactRef{
				InputHash: getString(row, "hash"), HeadKey: getString(row, "head"), Rel: getString(row, "rel"),
				TailKey: getString(row, "tail"), ChunkID: getString(row, "chunk"), JudgeGen: getString(row, "gen"),
			},
			DocID: getString(row, "doc"), JudgeInput: getString(row, "input"),
		})
	}
	return edges, nil
}
