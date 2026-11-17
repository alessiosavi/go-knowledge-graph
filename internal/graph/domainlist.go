package graph

import (
	"context"
	"fmt"
)

// DomainCount describes one domain and its number of done documents.
type DomainCount struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Documents int    `json:"documents"`
}

// ListDomains includes empty domains, ordered by id, counting only done documents.
func (s *Store) ListDomains(ctx context.Context) ([]DomainCount, error) {
	recs, err := s.runRead(ctx, `MATCH (d:Domain)
 OPTIONAL MATCH (doc:Document {domain:d.id,status:'done'})
 RETURN d.id AS id,coalesce(d.name,d.id) AS name,count(doc) AS documents ORDER BY id`, nil)
	if err != nil {
		return nil, fmt.Errorf("graph: list domains: %w", err)
	}
	out := make([]DomainCount, 0, len(recs))
	for _, r := range recs {
		out = append(out, DomainCount{ID: getString(r, "id"), Name: getString(r, "name"), Documents: getInt(r, "documents")})
	}
	return out, nil
}
