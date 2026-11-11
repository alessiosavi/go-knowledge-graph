package graph

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// ExtractionSchemaVersion versions derived extraction data. Version 2 uses
// name-only entity keys; version 3 links every mention and votes on its type.
// Older graphs must be re-extracted from stored chunks.
const ExtractionSchemaVersion = 3

const extractionSchemaMetaID = "extraction_schema"
const extractionMigrationBatch = 1000

// MigrationResult describes an extraction schema upgrade. Counts include
// completed batches even if a later batch fails; Migrated is true only after
// the new marker is committed.
type MigrationResult struct {
	From, To, DocumentsRequeued, EntitiesRemoved int
	Migrated                                     bool
}

func (s *Store) extractionSchemaVersion(ctx context.Context) (int, error) {
	recs, err := s.runRead(ctx, `MATCH (m:GkgMeta {id: $id}) RETURN m.value AS version`,
		map[string]any{"id": extractionSchemaMetaID})
	if err != nil {
		return 0, fmt.Errorf("graph: read extraction schema: %w", err)
	}
	version := 1
	if len(recs) > 0 {
		version = getInt(recs[0], "version")
	}
	return version, nil
}

// ExtractionSchemaCurrent checks the marker without modifying extraction data.
// A missing marker is the legacy schema, including on an empty store.
func (s *Store) ExtractionSchemaCurrent(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version, err := s.extractionSchemaVersion(ctx)
	return version >= ExtractionSchemaVersion, err
}

// MigrateExtractionSchema clears outdated derived data and requeues its live
// documents. Call only where re-extraction follows, before workers/recovery,
// with this process as the database's only writer. Batches bound transaction
// size; the marker is written last so an interrupted upgrade can be retried.
func (s *Store) MigrateExtractionSchema(ctx context.Context) (MigrationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	version, err := s.extractionSchemaVersion(ctx)
	if err != nil {
		return MigrationResult{}, err
	}
	result := MigrationResult{From: version, To: version}
	if version >= ExtractionSchemaVersion {
		return result, nil
	}
	result.To = ExtractionSchemaVersion
	recs, err := s.runRead(ctx, `MATCH (e:Entity) RETURN e.key LIMIT 1`, nil)
	if err != nil {
		return result, fmt.Errorf("graph: inspect extraction data: %w", err)
	}
	if len(recs) == 0 {
		recs, err = s.runRead(ctx, `MATCH (r:RejectedFact) RETURN r LIMIT 1`, nil)
		if err != nil {
			return result, fmt.Errorf("graph: inspect extraction audits: %w", err)
		}
	}
	if len(recs) > 0 {
		// Requeue before deleting entities: if startup fails after the last
		// entity batch, the next startup's no-entities path is still safe.
		result.DocumentsRequeued, err = s.extractionMigrationBatches(ctx, `
MATCH (d:Document)
WHERE d.status IN ['done', 'extracted', 'merged', 'verifying']
WITH d LIMIT $batch
SET d.status = 'chunked', d.updated_at = $now
RETURN count(*) AS n`)
		if err != nil {
			return result, fmt.Errorf("graph: requeue extraction: %w", err)
		}
		for _, query := range []string{
			`MATCH ()-[m:MENTIONS]->() WITH m LIMIT $batch DELETE m RETURN count(*) AS n`,
			`MATCH (:Entity)-[r]->(:Entity) WITH r LIMIT $batch DELETE r RETURN count(*) AS n`,
			`MATCH (r:RejectedFact) WITH r LIMIT $batch DETACH DELETE r RETURN count(*) AS n`,
		} {
			if _, err := s.extractionMigrationBatches(ctx, query); err != nil {
				return result, fmt.Errorf("graph: clear extraction data: %w", err)
			}
		}
		result.EntitiesRemoved, err = s.extractionMigrationBatches(ctx, `
MATCH (e:Entity) WITH e LIMIT $batch
SET e.embedding = null
DETACH DELETE e
RETURN count(*) AS n`)
		if err != nil {
			return result, fmt.Errorf("graph: clear extraction entities: %w", err)
		}
	}
	if err := s.runWrite(ctx, `MERGE (m:GkgMeta {id: $id}) SET m.value = $version`,
		map[string]any{"id": extractionSchemaMetaID, "version": ExtractionSchemaVersion}); err != nil {
		return result, fmt.Errorf("graph: set extraction schema: %w", err)
	}
	result.Migrated = true
	slog.Info("graph: upgraded extraction schema", "from", version, "to", ExtractionSchemaVersion,
		"documents_requeued", result.DocumentsRequeued, "entities_removed", result.EntitiesRemoved)
	return result, nil
}

// markEmptyExtractionSchema records the current extraction schema on a store
// without derived extraction data (a fresh database), so commands that do not
// migrate, such as gkg ingest, accept it and a later migration never mistakes
// its new data for legacy data. It never deletes or requeues anything.
func (s *Store) markEmptyExtractionSchema(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	version, err := s.extractionSchemaVersion(ctx)
	if err != nil || version >= ExtractionSchemaVersion {
		return err
	}
	for _, query := range []string{
		`MATCH (e:Entity) RETURN e.key LIMIT 1`,
		`MATCH (r:RejectedFact) RETURN r.id LIMIT 1`,
	} {
		recs, err := s.runRead(ctx, query, nil)
		if err != nil {
			return fmt.Errorf("graph: inspect extraction data: %w", err)
		}
		if len(recs) > 0 {
			return nil
		}
	}
	if err := s.runWrite(ctx, `MERGE (m:GkgMeta {id: $id}) SET m.value = $version`,
		map[string]any{"id": extractionSchemaMetaID, "version": ExtractionSchemaVersion}); err != nil {
		return fmt.Errorf("graph: set extraction schema: %w", err)
	}
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
