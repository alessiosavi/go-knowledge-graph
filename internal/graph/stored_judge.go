package graph

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// StoredFactRef identifies the typed edge and write generation that supplied
// the judge request. Rel is normalized in the same way as extraction writes.
type StoredFactRef struct {
	HeadKey, Rel, TailKey, ChunkID string
	JudgeGen, InputHash            string
}

const matchStoredFact = `
MATCH (a:Entity {key:$head})-[r]->(b:Entity {key:$tail})
WHERE type(r)=$rel AND r.chunk_id=$chunk AND r.judge_gen=$gen AND r.input_hash=$hash AND r.judge=''`

// ApplyStoredFactVerdict applies a verdict only to the matching unjudged
// generation. When requireDone is true the edge's document must be done.
// A vanished or changed edge returns false, and ambiguous matches fail.
func (s *Store) ApplyStoredFactVerdict(ctx context.Context, ref StoredFactRef, status, reason string, requireDone bool) (bool, error) {
	var applied bool
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		var err error
		applied, err = txApplyStoredFactVerdict(ctx, tx, ref, status, reason, requireDone)
		return err
	})
	return applied && err == nil, err
}

func txApplyStoredFactVerdict(ctx context.Context, tx neo4j.ManagedTransaction, ref StoredFactRef, status, reason string, requireDone bool) (bool, error) {
	var write string
	switch status {
	case "confirmed", "unclear":
		write = ` SET r.judge = $status REMOVE r.judge_input`
	case "refuted":
		write = ` DELETE r`
	default:
		return false, fmt.Errorf("graph: invalid stored fact verdict %q", status)
	}
	match := matchStoredFact
	if requireDone {
		match += `
MATCH (d:Document {id: r.doc_id}) WHERE d.status='done'`
	}
	params := map[string]any{
		"head": ref.HeadKey, "rel": sanitizeRel(ref.Rel), "tail": ref.TailKey, "chunk": ref.ChunkID,
		"gen": ref.JudgeGen, "hash": ref.InputHash, "status": status,
	}
	rows, err := txRows(ctx, tx, match+`
RETURN a.key AS head, type(r) AS rel, b.key AS tail, r.chunk_id AS chunk,
       r.doc_id AS doc, r.confidence AS score, r.extractor_version AS version LIMIT 2`, params)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	if len(rows) > 1 {
		return false, fmt.Errorf("graph: stored fact verdict matched multiple edges in chunk %q", ref.ChunkID)
	}
	written, err := txRows(ctx, tx, match+write+` RETURN count(*) AS n`, params)
	if err != nil {
		return false, err
	}
	switch getInt(written[0], "n") {
	case 0:
		return false, nil
	case 1:
	default:
		return false, fmt.Errorf("graph: stored fact verdict modified multiple edges in chunk %q", ref.ChunkID)
	}
	if status == "refuted" {
		r := rows[0]
		if err := txWriteFactAudit(ctx, tx, FactAudit{
			HeadKey: getString(r, "head"), Rel: getString(r, "rel"), TailKey: getString(r, "tail"),
			ChunkID: getString(r, "chunk"), DocID: getString(r, "doc"), Score: getFloat(r, "score"),
			ExtractorVersion: getString(r, "version"), JudgeVerdict: status, Reason: reason,
		}); err != nil {
			return false, err
		}
	}
	return true, nil
}
