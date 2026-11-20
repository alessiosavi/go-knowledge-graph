package graph

import (
	"context"
	"fmt"
)

// DomainCount describes a domain's done documents, products and store binding.
type DomainCount struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Documents     int    `json:"documents"`
	Products      int    `json:"products"`
	ProductsBound bool   `json:"products_bound"`
}

// ListDomains includes empty domains, ordered by id, counting only done documents.
func (s *Store) ListDomains(ctx context.Context) ([]DomainCount, error) {
	recs, err := s.runRead(ctx, `MATCH (d:Domain)
 OPTIONAL MATCH (doc:Document {domain:d.id,status:'done'})
 WITH d,count(DISTINCT doc) AS documents
 OPTIONAL MATCH (p:Product {domain:d.id})
 WITH d,documents,count(DISTINCT p) AS products
 OPTIONAL MATCH (s:ProductStore {domain:d.id})
 RETURN d.id AS id,coalesce(d.name,d.id) AS name,documents,products,count(s)>0 AS products_bound
 ORDER BY id`, nil)
	if err != nil {
		return nil, fmt.Errorf("graph: list domains: %w", err)
	}
	out := make([]DomainCount, 0, len(recs))
	for _, r := range recs {
		out = append(out, DomainCount{
			ID: getString(r, "id"), Name: getString(r, "name"), Documents: getInt(r, "documents"),
			Products: getInt(r, "products"), ProductsBound: getBool(r, "products_bound"),
		})
	}
	return out, nil
}
