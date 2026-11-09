package graph

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// terminalStatuses are the document states recovery never requeues.
var terminalStatuses = []string{
	string(StatusDone), string(StatusFailed), string(StatusSuperseded),
}

// InFlight lists documents in non-terminal states, oldest first — the
// ingest pipeline requeues them on restart.
func (s *Store) InFlight(ctx context.Context) ([]Document, error) {
	recs, err := s.runRead(ctx, `
MATCH (d:Document)
WHERE NOT d.status IN $terminal
RETURN d.id AS id, d.source_kind AS source_kind, d.source AS source, d.final_url AS final_url,
       d.title AS title, d.hash AS hash, d.status AS status, d.error AS error,
       coalesce(d.labels, '') AS labels,
       d.created_at AS created_at, d.updated_at AS updated_at, d.supersedes AS supersedes
ORDER BY d.updated_at ASC`,
		map[string]any{"terminal": terminalStatuses})
	if err != nil {
		return nil, fmt.Errorf("graph: in-flight documents: %w", err)
	}
	docs := make([]Document, 0, len(recs))
	for _, r := range recs {
		docs = append(docs, *recordToDocument(r))
	}
	return docs, nil
}

// GC prunes entities with no relationships left (no mentions, no facts).
// It is serialized with the other mutation paths (Store.mu). Embeddings are
// cleared before the delete so the vector index drops their entries (an
// entry pointing at a deleted node poisons later vector searches).
func (s *Store) GC(ctx context.Context) error {
	return s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		return txRun(ctx, tx, `
MATCH (e:Entity)
OPTIONAL MATCH (e)-[r]-()
WITH e, count(r) AS rels
WHERE rels = 0
SET e.embedding = null
DETACH DELETE e`, nil)
	})
}

// txGC prunes the given entity keys when they have no relationships left.
func txGC(ctx context.Context, tx neo4j.ManagedTransaction, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := txRun(ctx, tx, `
UNWIND $keys AS k
MATCH (e:Entity {key: k})
OPTIONAL MATCH (e)-[r]-()
WITH e, count(r) AS rels
WHERE rels = 0
SET e.embedding = null
DETACH DELETE e`,
		map[string]any{"keys": keys}); err != nil {
		return fmt.Errorf("graph: gc entities: %w", err)
	}
	return nil
}

// txRefreshMentionCounts recomputes mention_count for the given entity keys
// from the actual MENTIONS edges.
func txRefreshMentionCounts(ctx context.Context, tx neo4j.ManagedTransaction, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := txRun(ctx, tx, `
UNWIND $keys AS k
MATCH (e:Entity {key: k})
OPTIONAL MATCH (:Chunk)-[m:MENTIONS]->(e)
WITH e, count(m) AS cnt
SET e.mention_count = cnt`,
		map[string]any{"keys": keys}); err != nil {
		return fmt.Errorf("graph: refresh mention counts: %w", err)
	}
	return nil
}
