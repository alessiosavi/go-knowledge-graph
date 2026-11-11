package graph

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// ExtractionSchemaVersion versions derived extraction data. Version 2 uses
// name-only entity keys; older graphs must be re-extracted from stored chunks.
const ExtractionSchemaVersion = 2

const extractionSchemaMetaID = "extraction_schema"
const extractionMigrationBatch = 1000

// migrateExtractionSchema runs before workers/recovery, with one server as
// the database's only writer. Batches bound transaction size on large stores.
// The marker is written last, so a failed startup retries the migration.
func (s *Store) migrateExtractionSchema(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	recs, err := s.runRead(ctx, `MATCH (m:GkgMeta {id: $id}) RETURN m.value AS version`,
		map[string]any{"id": extractionSchemaMetaID})
	if err != nil {
		return fmt.Errorf("graph: read extraction schema: %w", err)
	}
	version := 1
	if len(recs) > 0 {
		version = getInt(recs[0], "version")
	}
	if version >= ExtractionSchemaVersion {
		return nil
	}
	recs, err = s.runRead(ctx, `MATCH (e:Entity) RETURN e.key LIMIT 1`, nil)
	if err != nil {
		return fmt.Errorf("graph: inspect extraction data: %w", err)
	}
	documents, entities := 0, 0
	if len(recs) > 0 {
		// Requeue before deleting entities: if startup fails after the last
		// entity batch, the next startup's no-entities path is still safe.
		documents, err = s.extractionMigrationBatches(ctx, `
MATCH (d:Document)
WHERE d.status IN ['done', 'extracted', 'merged', 'verifying']
WITH d LIMIT $batch
SET d.status = 'chunked', d.updated_at = $now
RETURN count(*) AS n`)
		if err != nil {
			return fmt.Errorf("graph: requeue extraction: %w", err)
		}
		for _, query := range []string{
			`MATCH ()-[m:MENTIONS]->() WITH m LIMIT $batch DELETE m RETURN count(*) AS n`,
			`MATCH (:Entity)-[r]->(:Entity) WITH r LIMIT $batch DELETE r RETURN count(*) AS n`,
			`MATCH (r:RejectedFact) WITH r LIMIT $batch DETACH DELETE r RETURN count(*) AS n`,
		} {
			if _, err := s.extractionMigrationBatches(ctx, query); err != nil {
				return fmt.Errorf("graph: clear extraction data: %w", err)
			}
		}
		entities, err = s.extractionMigrationBatches(ctx, `
MATCH (e:Entity) WITH e LIMIT $batch
SET e.embedding = null
DETACH DELETE e
RETURN count(*) AS n`)
		if err != nil {
			return fmt.Errorf("graph: clear extraction entities: %w", err)
		}
	}
	if err := s.runWrite(ctx, `MERGE (m:GkgMeta {id: $id}) SET m.value = $version`,
		map[string]any{"id": extractionSchemaMetaID, "version": ExtractionSchemaVersion}); err != nil {
		return fmt.Errorf("graph: set extraction schema: %w", err)
	}
	slog.Info("graph: upgraded extraction schema", "from", version, "to", ExtractionSchemaVersion,
		"documents_requeued", documents, "entities_removed", entities)
	return nil
}

// extractionMigrationBatches commits each bounded batch and stops when the
// query has no matching rows left. Queries must return the changed row count.
func (s *Store) extractionMigrationBatches(ctx context.Context, query string) (int, error) {
	total := 0
	for {
		n := 0
		err := s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
			res, err := tx.Run(ctx, query, map[string]any{
				"batch": extractionMigrationBatch, "now": time.Now().UTC().Format(timeLayout),
			})
			if err != nil {
				return err
			}
			recs, err := res.Collect(ctx)
			if err != nil {
				return err
			}
			n = getInt(recs[0], "n")
			return nil
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < extractionMigrationBatch {
			return total, nil
		}
	}
}
