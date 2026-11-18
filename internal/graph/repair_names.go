package graph

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// RepairEntityNames cleans stored display names in id order. In dry-run mode
// changed counts proposed repairs. Otherwise it counts committed updates.
func (s *Store) RepairEntityNames(ctx context.Context, dryRun bool) (scanned, changed int, err error) {
	const pageSize = 1000
	last := ""
	for {
		rows, err := s.runRead(ctx, `MATCH (e:Entity) WHERE e.id > $last
RETURN e.id AS id, e.name AS name ORDER BY id LIMIT $limit`, map[string]any{"last": last, "limit": pageSize})
		if err != nil {
			return scanned, changed, fmt.Errorf("graph: read entity names for repair: %w", err)
		}
		scanned += len(rows)
		n, err := s.repairEntityNamePage(ctx, rows, dryRun)
		if err != nil {
			return scanned, changed, err
		}
		changed += n
		if len(rows) < pageSize {
			return scanned, changed, nil
		}
		last = getString(rows[len(rows)-1], "id")
	}
}

func (s *Store) repairEntityNamePage(ctx context.Context, rows []*neo4j.Record, dryRun bool) (int, error) {
	updates := make([]map[string]any, 0)
	for _, row := range rows {
		old := getString(row, "name")
		name := DisplayName(old)
		if name == "" || name == old {
			continue
		}
		nameKey := EntityKey(name) // Same derivation as UpsertExtractionOpts.
		if nameKey != EntityKey(old) {
			continue // Legacy possessive cleanup can change the normalized name.
		}
		updates = append(updates, map[string]any{"id": getString(row, "id"), "old": old, "name": name, "name_key": nameKey})
	}
	if dryRun || len(updates) == 0 {
		return len(updates), nil
	}
	changed := 0
	if err := s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
		result, err := tx.Run(ctx, `UNWIND $updates AS u
MATCH (e:Entity {id: u.id}) WHERE e.name = u.old
SET e.name = u.name, e.name_key = u.name_key
RETURN count(e) AS changed`, map[string]any{"updates": updates})
		if err != nil {
			return err
		}
		row, err := result.Single(ctx)
		if err != nil {
			return err
		}
		changed = getInt(row, "changed")
		return nil
	}); err != nil {
		return 0, fmt.Errorf("graph: repair entity names: %w", err)
	}
	return changed, nil
}
