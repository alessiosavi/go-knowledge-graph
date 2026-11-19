package graph

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const entityRefProjection = `e.id AS id, e.key AS key, e.name AS name, e.type AS type, e.domain AS domain`

const entityByIDQuery = `MATCH (e:Entity {id: $q}) RETURN ` + entityRefProjection
const entityByKeyQuery = `MATCH (e:Entity {key: $q}) RETURN ` + entityRefProjection

const mergedSelectorQuery = `
MATCH (m:MergedEntity {old_id: $q})
RETURN m.survivor_id AS survivor_id
UNION ALL
MATCH (m:MergedEntity {old_key: $q})
WHERE m.old_id IS NULL OR m.old_id <> $q
RETURN m.survivor_id AS survivor_id`

// resolveEntitySelectorTx resolves live IDs before live keys, then accepts a
// single ledger row pointing directly to a live ID in the caller's scope.
func resolveEntitySelectorTx(ctx context.Context, tx neo4j.ManagedTransaction, selector, domain string) (EntityRef, Provenance, error) {
	notFound := func() (EntityRef, Provenance, error) {
		return EntityRef{}, Provenance{}, fmt.Errorf("graph: entity %s: %w", selector, ErrNotFound)
	}
	params := map[string]any{"q": selector}
	rows, err := txRows(ctx, tx, entityByIDQuery, params)
	if err == nil && len(rows) == 0 {
		rows, err = txRows(ctx, tx, entityByKeyQuery, params)
	}
	if err != nil {
		return EntityRef{}, Provenance{}, err
	}
	if len(rows) != 0 {
		ref := entityRefFromRecord(rows[0])
		if domain != "" && ref.Domain != domain {
			return notFound()
		}
		return ref, Provenance{}, nil
	}
	rows, err = txRows(ctx, tx, mergedSelectorQuery, params)
	if err != nil {
		return EntityRef{}, Provenance{}, err
	}
	// Multiple rows can match across columns even with both unique constraints.
	if len(rows) != 1 {
		return notFound()
	}
	survivor := getString(rows[0], "survivor_id")
	if survivor == "" {
		return notFound()
	}
	rows, err = txRows(ctx, tx, entityByIDQuery, map[string]any{"q": survivor})
	if err != nil {
		return EntityRef{}, Provenance{}, err
	}
	if len(rows) != 1 {
		return notFound()
	}
	ref := entityRefFromRecord(rows[0])
	if domain != "" && ref.Domain != domain {
		return notFound()
	}
	return ref, Provenance{Requested: selector, ResolvedVia: "merged"}, nil
}

func entityRefFromRecord(row *neo4j.Record) EntityRef {
	return EntityRef{
		ID: getString(row, "id"), Key: getString(row, "key"), Name: getString(row, "name"),
		Type: getString(row, "type"), Domain: getString(row, "domain"),
	}
}
