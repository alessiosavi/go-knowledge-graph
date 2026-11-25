package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	ProductTextSpace = "furls|gemini-embedding-2|768"
	textVecIndexName = "text_vec"
	textVecDimension = 768
	textVecGCLimit   = 1000
	textVecGrace     = 7 * 24 * time.Hour
)

// MaxPendingTextVecs bounds the number of pending texts returned per scan.
const MaxPendingTextVecs = 4096

// EnsureTextVecIndex runs at serve startup for the supported product text
// space. Existing vector index settings are never changed.
func (s *Store) EnsureTextVecIndex(ctx context.Context) error {
	found, err := s.checkTextVecIndex(ctx)
	if err != nil {
		return err
	}
	if !found {
		err = s.runAuto(ctx, `CREATE VECTOR INDEX text_vec ON :TextVec(embedding)
WITH CONFIG {"dimension": 768, "capacity": 32768, "metric": "cos", "scalar_kind": "f32"}`, nil)
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("graph: create text_vec: %w", err)
		}
		found, err = s.checkTextVecIndex(ctx)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("graph: text_vec missing after create")
		}
	}
	// Failure ids are store-independent vec_ids in the one supported space.
	// Keep this DDL on the worker path too.
	return s.ensureIndex(ctx, "TextVecFailure", []string{"id"})
}

func (s *Store) checkTextVecIndex(ctx context.Context) (bool, error) {
	size, err := s.textVecIndexSize(ctx)
	return size >= 0, err
}

var ErrProductTextIndexUnavailable = errors.New("product text vector index unavailable")

// textVecIndexSize returns -1 when the index is absent.
func (s *Store) textVecIndexSize(ctx context.Context) (int, error) {
	rows, err := s.runRead(ctx, "SHOW VECTOR INDEX INFO", nil)
	if err != nil {
		return -1, fmt.Errorf("graph: inspect text_vec: %w", err)
	}
	for _, row := range rows {
		if getString(row, "index_name") != textVecIndexName {
			continue
		}
		label, property := getString(row, "label"), getString(row, "property")
		dimension := getInt(row, "dimension")
		metric, scalar := getString(row, "metric"), getString(row, "scalar_kind")
		if label != ":TextVec" || property != "embedding" || dimension != textVecDimension || metric != "cos" || scalar != "f32" {
			return -1, fmt.Errorf("%w: text_vec mismatch: got %s(%s), dimension=%d metric=%s scalar_kind=%s, want :TextVec(embedding), dimension=768 metric=cos scalar_kind=f32",
				ErrProductTextIndexUnavailable, label, property, dimension, metric, scalar)
		}
		return getInt(row, "size"), nil
	}
	return -1, nil
}

// CheckProductTextIndex checks readiness without creating or changing an index.
func (s *Store) CheckProductTextIndex(ctx context.Context) error {
	found, err := s.checkTextVecIndex(ctx)
	if err == nil && !found {
		return ErrProductTextIndexUnavailable
	}
	return err
}

// PendingTextVec is one unique text eligible for an embedding request.
type PendingTextVec struct {
	VecID    string
	Text     string
	Attempts int
}

// PendingTextVecs selects up to limit distinct texts across stores, capped at
// MaxPendingTextVecs. Retry state suppresses selection until it becomes due.
func (s *Store) PendingTextVecs(ctx context.Context, space string, now time.Time, limit int) ([]PendingTextVec, error) {
	rows, err := s.runRead(ctx, `
MATCH (p:ProductText)
WHERE p.vec_id IS NOT NULL
WITH DISTINCT p.vec_id AS vec_id, p.text AS text
OPTIONAL MATCH (v:TextVec {vec_id: vec_id})
WHERE v.space = $space
WITH vec_id, text, v WHERE v IS NULL
OPTIONAL MATCH (f:TextVecFailure {id: vec_id})
WITH vec_id, text, f
WHERE f.next_at IS NULL OR f.next_at <= $now
RETURN vec_id, text, coalesce(f.attempts, 0) AS attempts
LIMIT $limit`, map[string]any{"space": space, "now": now.UnixMilli(), "limit": min(MaxPendingTextVecs, limit)})
	if err != nil {
		return nil, fmt.Errorf("graph: select pending text vectors: %w", err)
	}
	pending := make([]PendingTextVec, 0, len(rows))
	for _, row := range rows {
		pending = append(pending, PendingTextVec{
			VecID: getString(row, "vec_id"), Text: getString(row, "text"), Attempts: getInt(row, "attempts"),
		})
	}
	return pending, nil
}

// TextVecResult carries one success or failure from a worker batch.
type TextVecResult struct {
	PendingTextVec
	Embedding []float64
	ErrorCode string
}

// WriteTextVecBatch commits whole vectors and their retry-state changes in
// one transaction. It does not write to ProductText or take the ingest lock.
func (s *Store) WriteTextVecBatch(ctx context.Context, space string, now time.Time, batch []TextVecResult) error {
	var vectors, failures []map[string]any
	for _, entry := range batch {
		if entry.ErrorCode != "" {
			delay := min(24*time.Hour, (30*time.Second)<<min(entry.Attempts, 12))
			failures = append(failures, map[string]any{
				"id": entry.VecID, "attempts": entry.Attempts + 1,
				"next_at": now.Add(delay).UnixMilli(), "error_code": entry.ErrorCode,
			})
			continue
		}
		if len(entry.Embedding) != textVecDimension {
			return fmt.Errorf("graph: text vector dimension %d, want %d", len(entry.Embedding), textVecDimension)
		}
		sum := sha256.Sum256([]byte(space + "\x00" + entry.Text))
		vectors = append(vectors, map[string]any{
			"id": hex.EncodeToString(sum[:16]), "vec_id": entry.VecID, "embedding": entry.Embedding,
		})
	}

	err := s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
		if len(vectors) > 0 {
			if err := txRun(ctx, tx, `
UNWIND $rows AS row
MERGE (v:TextVec {id: row.id})
ON CREATE SET v.vec_id = row.vec_id, v.space = $space, v.created_at = $now, v.embedding = row.embedding
WITH row
MATCH (f:TextVecFailure {id: row.vec_id})
DELETE f`, map[string]any{"rows": vectors, "space": space, "now": now.UnixMilli()}); err != nil {
				return err
			}
		}
		if len(failures) > 0 {
			return txRun(ctx, tx, `
UNWIND $rows AS row
MERGE (f:TextVecFailure {id: row.id})
SET f.attempts = row.attempts, f.next_at = row.next_at, f.error_code = row.error_code`, map[string]any{"rows": failures})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("graph: write text vector batch: %w", err)
	}
	return nil
}

// CountTextVecs counts all vector nodes against the box-wide admission cap.
func (s *Store) CountTextVecs(ctx context.Context) (int, error) {
	return s.textVecCount(ctx, "MATCH (v:TextVec) RETURN count(v) AS n")
}

// TextVecStats describes coverage of distinct referenced texts in a space.
type TextVecStats struct {
	Referenced int
	Embedded   int
	Orphaned   int
}

func (s *Store) TextVecStats(ctx context.Context, space string) (TextVecStats, error) {
	rows, err := s.runRead(ctx, `
MATCH (p:ProductText)
WHERE p.vec_id IS NOT NULL
WITH DISTINCT p.vec_id AS vec_id
OPTIONAL MATCH (v:TextVec {vec_id: vec_id})
WHERE v.space = $space
RETURN count(vec_id) AS referenced, count(v) AS embedded`, map[string]any{"space": space})
	if err != nil {
		return TextVecStats{}, fmt.Errorf("graph: measure text vector coverage: %w", err)
	}
	orphaned, err := s.textVecCount(ctx, "MATCH (v:TextVec) WHERE v.orphan_since IS NOT NULL RETURN count(v) AS n")
	if err != nil {
		return TextVecStats{}, err
	}
	return TextVecStats{
		Referenced: getInt(rows[0], "referenced"), Embedded: getInt(rows[0], "embedded"), Orphaned: orphaned,
	}, nil
}

func (s *Store) textVecCount(ctx context.Context, cypher string) (int, error) {
	rows, err := s.runRead(ctx, cypher, nil)
	if err != nil {
		return 0, fmt.Errorf("graph: count text vectors: %w", err)
	}
	return getInt(rows[0], "n"), nil
}

// GCTextVecs changes at most 1,000 nodes per pass, with reference checks
// made inside each mutation. Expired orphans are removed before marking
// new orphans so a busy pass also makes room for new vectors.
func (s *Store) GCTextVecs(ctx context.Context, now time.Time) error {
	remaining := textVecGCLimit
	for _, op := range []struct {
		name, query string
	}{
		{"delete expired vectors", `
MATCH (v:TextVec) WHERE v.orphan_since < $cutoff
OPTIONAL MATCH (p:ProductText {vec_id: v.vec_id})
WITH v, p WHERE p IS NULL
WITH v LIMIT $limit
SET v.embedding = null
DETACH DELETE v
RETURN count(*) AS n`},
		{"restore referenced vectors", `
MATCH (v:TextVec) WHERE v.orphan_since IS NOT NULL
MATCH (:ProductText {vec_id: v.vec_id})
WITH DISTINCT v LIMIT $limit
SET v.orphan_since = null
RETURN count(*) AS n`},
		{"mark newly orphaned vectors", `
MATCH (v:TextVec) WHERE v.orphan_since IS NULL
OPTIONAL MATCH (p:ProductText {vec_id: v.vec_id})
WITH v, p WHERE p IS NULL
WITH v LIMIT $limit
SET v.orphan_since = $now
RETURN count(*) AS n`},
		{"delete unreferenced failure records", `
MATCH (f:TextVecFailure)
OPTIONAL MATCH (p:ProductText {vec_id: f.id})
WITH f, p WHERE p IS NULL
WITH f LIMIT $limit
DELETE f
RETURN count(*) AS n`},
	} {
		if remaining == 0 {
			break
		}
		var changed int
		err := s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
			rows, err := txRows(ctx, tx, op.query, map[string]any{
				"now": now.UnixMilli(), "cutoff": now.Add(-textVecGrace).UnixMilli(), "limit": remaining,
			})
			if err != nil {
				return err
			}
			changed = getInt(rows[0], "n")
			return nil
		})
		if err != nil {
			return fmt.Errorf("graph: text vector GC (%s): %w", op.name, err)
		}
		remaining -= changed
	}
	return nil
}
