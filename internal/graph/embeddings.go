package graph

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// entityVectorIndexName is the vector index over :Entity(embedding).
const entityVectorIndexName = "entity_vec"

// vectorIndexCapacity caps the indexed entity count (design default).
const vectorIndexCapacity = 10_000

// EnsureVectorIndex creates the entity vector index idempotently. An
// existing index with a different dimension is an actionable error: the
// stored vectors would not be indexable. DDL runs in autocommit like the
// other schema manipulation.
func (s *Store) EnsureVectorIndex(ctx context.Context, dim int) error {
	if dim <= 0 {
		return fmt.Errorf("graph: vector index: dimension %d must be positive", dim)
	}
	recs, err := s.runRead(ctx, "SHOW VECTOR INDEX INFO", nil)
	if err != nil {
		return fmt.Errorf("graph: inspect vector indexes: %w", err)
	}
	for _, r := range recs {
		if getString(r, "index_name") != entityVectorIndexName {
			continue
		}
		if existing := getInt(r, "dimension"); existing != dim {
			return fmt.Errorf("graph: vector index %s exists with dimension %d, want %d", entityVectorIndexName, existing, dim)
		}
		return nil
	}
	ddl := fmt.Sprintf(`CREATE VECTOR INDEX %s ON :Entity(embedding) WITH CONFIG {"dimension": %d, "capacity": %d, "metric": "cos"}`,
		entityVectorIndexName, dim, vectorIndexCapacity)
	if err := s.runAuto(ctx, ddl, nil); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return fmt.Errorf("graph: create vector index: %w", err)
	}
	return nil
}

// SetEntityEmbeddings stores a batch of entity embeddings inside one
// transaction and reports how many ids did not match a live entity (a
// concurrent delete is not an error). ids and vecs must align; every
// vector must be non-empty.
func (s *Store) SetEntityEmbeddings(ctx context.Context, ids []string, vecs [][]float64) (int, error) {
	if len(ids) != len(vecs) {
		return 0, fmt.Errorf("graph: set entity embeddings: %d ids for %d vectors", len(ids), len(vecs))
	}
	if len(ids) == 0 {
		return 0, nil
	}
	rows := make([]map[string]any, 0, len(ids))
	for i, id := range ids {
		if len(vecs[i]) == 0 {
			return 0, fmt.Errorf("graph: set entity embeddings: empty vector for %s", id)
		}
		rows = append(rows, map[string]any{"id": id, "vec": vecs[i]})
	}
	matched := 0
	err := s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
		res, err := tx.Run(ctx, `
UNWIND $rows AS row
MATCH (e:Entity {id: row.id})
SET e.embedding = row.vec
RETURN count(e) AS matched`, map[string]any{"rows": rows})
		if err != nil {
			return fmt.Errorf("graph: set entity embeddings: %w", err)
		}
		recs, err := res.Collect(ctx)
		if err != nil {
			return fmt.Errorf("graph: set entity embeddings: %w", err)
		}
		if len(recs) > 0 {
			matched = getInt(recs[0], "matched")
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(ids) - matched, nil
}

// SetEntityEmbedding stores the entity's embedding vector in one write;
// ErrNotFound when the entity does not exist. The vector index must already
// cover the vector's dimension (EnsureVectorIndex).
func (s *Store) SetEntityEmbedding(ctx context.Context, id string, vec []float64) error {
	if len(vec) == 0 {
		return errors.New("graph: empty embedding vector")
	}
	return s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
		res, err := tx.Run(ctx, "MATCH (e:Entity {id: $id}) SET e.embedding = $vec RETURN 1",
			map[string]any{"id": id, "vec": vec})
		if err != nil {
			return fmt.Errorf("graph: set entity embedding: %w", err)
		}
		recs, err := res.Collect(ctx)
		if err != nil {
			return fmt.Errorf("graph: set entity embedding: %w", err)
		}
		if len(recs) == 0 {
			return fmt.Errorf("graph: entity %s: %w", id, ErrNotFound)
		}
		return nil
	})
}
