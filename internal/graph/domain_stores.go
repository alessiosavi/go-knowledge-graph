package graph

import (
	"context"
	"fmt"
)

// DomainStores reports whether a Domain node exists and lists its bound stores in order.
func (s *Store) DomainStores(ctx context.Context, slug string) (exists bool, stores []string, err error) {
	stores = []string{}
	rows, err := s.runRead(ctx, `
MATCH (:Domain {id:$domain})
OPTIONAL MATCH (s:ProductStore {domain:$domain})
RETURN s.store AS store ORDER BY store`, map[string]any{"domain": slug})
	if err != nil {
		return false, stores, fmt.Errorf("graph: domain stores: %w", err)
	}
	for _, row := range rows {
		if store := getString(row, "store"); store != "" {
			stores = append(stores, store)
		}
	}
	return len(rows) != 0, stores, nil
}
