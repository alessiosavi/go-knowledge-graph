package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// ExtractionSchemaVersion versions derived extraction data. Version 2 uses
// name-only entity keys; version 3 links every mention and votes on its type.
// Version 4 assigns domains before clearing derived data for re-extraction.
// Older graphs must be re-extracted from stored chunks.
const ExtractionSchemaVersion = 4

// SchemaVersionV4 is the marker that enables the new ingestion write modes.
const SchemaVersionV4 = 4

// ErrSeedRequired prevents destructive migration before every source is assigned.
var ErrSeedRequired = errors.New("graph: v4 migration needs GKG_DOMAIN_SEED_FILE (a seed covering every document, or detect-all)")

type MigrationOptions struct {
	Seed      *domain.Seed
	DetectAll bool
	Params    domain.Params
}

var ErrSchemaNewer = errors.New("graph: extraction schema is newer than this binary")

type schemaNewerError struct{ version int }

func (e schemaNewerError) Error() string {
	return fmt.Sprintf("graph extraction schema v%d is newer than this binary (v%d); deploy a newer gkg or restore the pre-migration snapshot", e.version, ExtractionSchemaVersion)
}

func (e schemaNewerError) Unwrap() error { return ErrSchemaNewer }

const extractionSchemaMetaID = "extraction_schema"
const extractionMigrationBatch = 1000
const extractionSchemaVersionQuery = `MATCH (m:GkgMeta {id: $id}) RETURN m.value AS version`

// MigrationResult describes an extraction schema upgrade. Counts include
// completed batches even if a later batch fails; Migrated is true only after
// the new marker is committed.
type MigrationResult struct {
	From, To, DocumentsRequeued, EntitiesRemoved int
	Migrated                                     bool
}

func (s *Store) extractionSchemaVersion(ctx context.Context) (int, error) {
	return decodeExtractionSchemaVersion(s.runRead(ctx, extractionSchemaVersionQuery, map[string]any{"id": extractionSchemaMetaID}))
}

func decodeExtractionSchemaVersion(recs []*neo4j.Record, err error) (int, error) {
	if err != nil {
		return 0, fmt.Errorf("graph: read extraction schema: %w", err)
	}
	version := 1
	if len(recs) > 0 {
		version = getInt(recs[0], "version")
	}
	return version, nil
}

// SchemaVersion reads the extraction schema marker without modifying it.
// A missing marker is the legacy schema version 1, even on an empty store.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.extractionSchemaVersion(ctx)
}

// ExtractionSchemaCurrent checks the marker without modifying extraction data.
// A missing marker is the legacy schema, including on an empty store. It
// returns (true, nil) when the marker equals ExtractionSchemaVersion,
// (false, nil) when it is older (run the migration), and an error wrapping
// ErrSchemaNewer when it is newer: check the error before the boolean, since
// a newer graph must never be treated as one to migrate.
func (s *Store) ExtractionSchemaCurrent(ctx context.Context) (bool, error) {
	version, err := s.CheckExtractionSchema(ctx)
	return err == nil && version == ExtractionSchemaVersion, err
}

// CheckExtractionSchema reads the marker and rejects versions newer than
// this binary with ErrSchemaNewer. Older versions are returned for diagnostics.
func (s *Store) CheckExtractionSchema(ctx context.Context) (int, error) {
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		return 0, err
	}
	if version > ExtractionSchemaVersion {
		return 0, schemaNewerError{version: version}
	}
	return version, nil
}

// MigrateExtractionSchema clears outdated derived data and requeues its live
// documents. Call only where re-extraction follows, before workers/recovery,
// with this process as the database's only writer. Batches bound transaction
// size; the marker is written last so an interrupted upgrade can be retried.
func (s *Store) MigrateExtractionSchema(ctx context.Context) (MigrationResult, error) {
	return s.MigrateExtractionSchemaOpts(ctx, MigrationOptions{})
}

// MigrateExtractionSchemaOpts assigns live documents before clearing derived
// data. Assignments and the assigned phase are durable restart boundaries.
// The caller must stop every other database writer for the whole migration.
func (s *Store) MigrateExtractionSchemaOpts(ctx context.Context, opt MigrationOptions) (MigrationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	version, err := s.extractionSchemaVersion(ctx)
	if err != nil {
		return MigrationResult{}, err
	}
	result := MigrationResult{From: version, To: version}
	if version > ExtractionSchemaVersion {
		return result, schemaNewerError{version: version}
	}
	if version == ExtractionSchemaVersion {
		return result, nil
	}
	result.To = ExtractionSchemaVersion

	hasData, err := s.hasExtractionData(ctx)
	if err != nil {
		return result, err
	}
	if hasData {
		recs, err := s.runRead(ctx, `MATCH (m:GkgMeta {id:$id}) RETURN m.v4_phase AS phase`, map[string]any{"id": extractionSchemaMetaID})
		if err != nil {
			return result, fmt.Errorf("graph: migration phase: %w", err)
		}
		assigned := len(recs) > 0 && getString(recs[0], "phase") == "assigned"
		if !assigned {
			if opt.Seed == nil && !opt.DetectAll {
				documents, err := s.runRead(ctx, `MATCH (d:Document) RETURN d.id LIMIT 1`, nil)
				if err != nil {
					return result, fmt.Errorf("graph: inspect migration documents: %w", err)
				}
				if len(documents) > 0 {
					return result, ErrSeedRequired
				}
			}
			docs, err := s.ProfilesFromMentions(ctx)
			if err != nil {
				return result, err
			}
			// Resolve every document before the first write, including coverage errors.
			assignments := make([]Assignment, 0, len(docs))
			corpus := make([]domain.DocProfile, 0, len(docs))
			var unmatched []string
			params := opt.Params
			if params == (domain.Params{}) {
				params = domain.DefaultParams()
			}
			for _, doc := range docs {
				a := Assignment{DocID: doc.DocID, Profile: doc.Profile, CreatedBy: "auto"}
				decision := domain.Decision{Action: "seed", Params: params}
				if opt.Seed != nil {
					var ok bool
					decision.Domain, ok = opt.Seed.DomainFor(doc.Source)
					if !ok {
						unmatched = append(unmatched, doc.Source)
						continue
					}
					if !domain.ValidDomain(decision.Domain) {
						return result, fmt.Errorf("%w: invalid domain %q", ErrSeedRequired, decision.Domain)
					}
					a.CreatedBy = "seed"
					a.DomainName = opt.Seed.Names[decision.Domain]
				} else {
					// Always replay from the start in id order; partially persisted domains
					// must not bias decisions after an interruption in the assign phase.
					decision = domain.Decide(doc, corpus, params)
					if decision.Action == "prefix" {
						decision.Action = "attach"
					}
					if decision.Action == "new" {
						a.DomainName = domain.DomainName(doc, nil, corpus)
					}
				}
				a.Domain = decision.Domain
				a.Decision = DecisionRecord{OpID: "migration-v4", DocID: doc.DocID, Domain: a.Domain, PreviousDomain: doc.Domain, Action: decision.Action, By: "admin", Top: decision.Top, Params: decision.Params, Note: decision.Note}
				assignments = append(assignments, a)
				doc.Domain = a.Domain
				corpus = append(corpus, doc)
			}
			if len(unmatched) > 0 {
				return result, fmt.Errorf("%w: %d unmatched sources (up to 20): %s", ErrSeedRequired, len(unmatched), strings.Join(unmatched[:min(20, len(unmatched))], ", "))
			}
			slog.Info("graph: validated v4 migration assignments", "documents", len(assignments))
			done, err := s.runRead(ctx, `MATCH (d:DomainDecision {op_id:'migration-v4'}) RETURN DISTINCT d.doc_id AS id,d.domain AS domain`, nil)
			if err != nil {
				return result, fmt.Errorf("graph: migration decisions: %w", err)
			}
			resolved := make(map[string]string, len(assignments))
			for _, a := range assignments {
				resolved[a.DocID] = a.Domain
			}
			completed := make(map[string]bool, len(done))
			for _, r := range done {
				id, recorded := getString(r, "id"), getString(r, "domain")
				if recomputed, ok := resolved[id]; ok && recomputed != recorded {
					return result, fmt.Errorf("graph: seed changed after the interrupted assign phase: document %s was assigned to %q, recomputed %q", id, recorded, recomputed)
				}
				completed[id] = true
			}
			for _, a := range assignments {
				if completed[a.DocID] {
					continue
				}
				if err := s.assignDomainUnlocked(ctx, a, nil); err != nil {
					return result, fmt.Errorf("graph: migration assign %s: %w", a.DocID, err)
				}
			}
			if err := s.runWrite(ctx, `MERGE (m:GkgMeta {id:$id}) SET m.v4_phase='assigned'`, map[string]any{"id": extractionSchemaMetaID}); err != nil {
				return result, fmt.Errorf("graph: migration assigned phase: %w", err)
			}
		}
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
			`MATCH (c:FactCandidate) WITH c LIMIT $batch DELETE c RETURN count(*) AS n`,
			// Rebuild every chunk as an original, independent of recovery order.
			`MATCH (c:Chunk) WHERE coalesce(c.duplicate_of,'')<>'' WITH c LIMIT $batch SET c.duplicate_of='' RETURN count(*) AS n`,
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
	if err := s.runWrite(ctx, `MERGE (m:GkgMeta {id: $id}) SET m.value = $version REMOVE m.v4_phase`,
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
	hasData, err := s.hasExtractionData(ctx)
	if err != nil {
		return err
	}
	if hasData {
		return nil
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

// hasExtractionData includes documents, even when all mentions are absent.
func (s *Store) hasExtractionData(ctx context.Context) (bool, error) {
	for _, query := range []string{
		`MATCH (d:Document) RETURN d.id LIMIT 1`,
		`MATCH (e:Entity) RETURN e.key LIMIT 1`,
		`MATCH (r:RejectedFact) RETURN r.id LIMIT 1`,
		`MATCH (c:FactCandidate) RETURN c.id LIMIT 1`,
	} {
		recs, err := s.runRead(ctx, query, nil)
		if err != nil {
			return false, fmt.Errorf("graph: inspect extraction data: %w", err)
		}
		if len(recs) > 0 {
			return true, nil
		}
	}
	return false, nil
}
