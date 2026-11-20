package graph

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

// ErrProductData marks a rejected product registry or release operation.
var ErrProductData = errors.New("invalid product data")

type FeatureRegistryImport struct {
	Rev       string
	Digest    string
	Unchanged bool
}

// ImportFeatureRegistry records the original registry bytes and the indexed
// feature revisions in one transaction.
func (s *Store) ImportFeatureRegistry(ctx context.Context, data []byte) (FeatureRegistryImport, error) {
	registry, err := productobs.ParseRegistry(data)
	if err != nil {
		return FeatureRegistryImport{}, fmt.Errorf("%w: %v", ErrProductData, err)
	}
	result := FeatureRegistryImport{Rev: registry.Digest()[:16], Digest: registry.Digest()}
	definitions := registry.FeatureDefinitions()
	features := make([]map[string]any, 0, len(definitions))
	byID := make(map[string]map[string]any, len(definitions))
	ids := make([]string, 0, len(definitions))
	for _, f := range definitions {
		props := map[string]any{
			"key": result.Rev + "/" + f.ID, "rev": result.Rev, "id": f.ID, "name": f.Name,
			"definition": f.Definition, "definition_digest": productDigest([]byte(f.Definition)),
			"kind": f.Kind, "cardinality": f.Cardinality, "canonical_unit": f.CanonicalUnit, "replaces": "",
		}
		features = append(features, props)
		byID[f.ID] = props
		ids = append(ids, f.ID)
	}
	params := map[string]any{
		"rev": result.Rev, "digest": result.Digest, "json": string(data),
		"at": time.Now().UTC().Format(timeLayout),
	}
	err = s.runProductWrite(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := txRows(ctx, tx, `MATCH (r:FeatureRegistry {rev:$rev}) RETURN r.digest AS digest`, params)
		if err != nil {
			return err
		}
		result.Unchanged = len(rows) != 0
		if result.Unchanged {
			if getString(rows[0], "digest") != result.Digest {
				return fmt.Errorf("%w: registry revision %q already has a different digest", ErrProductData, result.Rev)
			}
			return nil
		}
		// Snapshot isolation alone permits two new revisions with conflicting
		// feature semantics to pass the same historical check.
		if err := txRun(ctx, tx, `MERGE (m:GkgMeta {id:'feature_registry_import'})
			SET m.seq = coalesce(m.seq, 0) + 1`, nil); err != nil {
			return err
		}
		if err := txCheckFeatureHistory(ctx, tx, registry, ids, byID); err != nil {
			return err
		}
		if err := txRun(ctx, tx, `MERGE (r:FeatureRegistry {rev:$rev})
			ON CREATE SET r.digest=$digest,r.registry_json=$json,r.imported_at=$at`, params); err != nil {
			return err
		}
		return txRun(ctx, tx, `UNWIND $features AS props CREATE (f:FeatureRev) SET f = props`, map[string]any{"features": features})
	})
	return result, err
}

func txCheckFeatureHistory(ctx context.Context, tx neo4j.ManagedTransaction, registry *productobs.Registry, ids []string, byID map[string]map[string]any) error {
	rows, err := txRows(ctx, tx, `
MATCH (f:FeatureRev) WHERE f.id IN $ids
WITH f.rev AS rev, collect({id:f.id, definition_digest:f.definition_digest,
    kind:f.kind, cardinality:f.cardinality, canonical_unit:f.canonical_unit}) AS features
OPTIONAL MATCH (r:FeatureRegistry {rev:rev})
RETURN rev, features, r.registry_json AS registry_json ORDER BY rev`, map[string]any{"ids": ids})
	if err != nil {
		return err
	}
	for _, row := range rows {
		rev := getString(row, "rev")
		previous, err := productobs.ParseRegistry([]byte(getString(row, "registry_json")))
		if err != nil {
			return fmt.Errorf("read stored registry %q: %w", rev, err)
		}
		value, _ := row.Get("features")
		for _, entry := range value.([]any) {
			prior := entry.(map[string]any)
			id := prior["id"].(string)
			for _, field := range []string{"definition_digest", "kind", "cardinality", "canonical_unit"} {
				if byID[id][field] != prior[field] {
					return fmt.Errorf("%w: feature %q changes %s from registry %q", ErrProductData, id, field, rev)
				}
			}
			if field := registry.MappingSemanticChange(previous, id); field != "" {
				return fmt.Errorf("%w: feature %q changes %s from registry %q", ErrProductData, id, field, rev)
			}
		}
	}
	return nil
}

// Concurrent creation of a shared pointer, fence or store binding can fail
// the unique constraint at commit. A fresh transaction sees the winning row.
func (s *Store) runProductWrite(ctx context.Context, write func(neo4j.ManagedTransaction) error) error {
	err := s.runWriteTxLocked(ctx, write)
	if err != nil && isConstraintViolation(err) {
		return s.runWriteTxLocked(ctx, write)
	}
	return err
}

func productDigest(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
