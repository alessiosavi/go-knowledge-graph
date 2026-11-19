package graph

import (
	"context"
	"fmt"
	"slices"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const nameCandidateLimit = 100

// EntityByName returns the highest ranked candidate in the first matching tier.
func (s *Store) EntityByName(ctx context.Context, name string) (EntityRef, error) {
	candidates, err := s.EntityCandidatesByName(ctx, name, "")
	if err != nil {
		return EntityRef{}, err
	}
	if len(candidates) == 0 {
		return EntityRef{}, fmt.Errorf("graph: entity %q: %w", name, ErrNotFound)
	}
	return candidates[0].EntityRef, nil
}

// EntityCandidatesByName returns one hit per domain. Exact matches use the
// v4 name_key index (including document-scoped entities) or the legacy
// display-name scan. Verified advisory folds are used only on an exact miss.
func (s *Store) EntityCandidatesByName(ctx context.Context, name, slug string) ([]EntityCandidate, error) {
	if slug != "" && !ValidDomain(slug) {
		return nil, argerr.Errorf("graph: invalid domain slug %q", slug)
	}
	var candidates []EntityCandidate
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		key := EntityKey(name)
		exact, err := exactNameCandidatesTx(ctx, tx, name, key, slug)
		if err != nil {
			return err
		}
		candidates = make([]EntityCandidate, 0, len(exact))
		for _, row := range exact {
			candidates = append(candidates, EntityCandidate{EntityRef: entityRefFromRecord(row), Match: EntityMatchExact})
		}
		if len(candidates) != 0 {
			return nil
		}
		record, err := txReadFoldRecord(ctx, tx)
		if err != nil || record == nil || record.Version != FoldKeyVersion {
			return err
		}
		hits, err := foldCandidatesTx(ctx, tx, nameQueryFolds(record, key, DisplayName(name), slug), record)
		if err != nil {
			return err
		}
		for _, hit := range canonicalNameCandidates(hits) {
			candidates = append(candidates, EntityCandidate{EntityRef: hit.EntityRef, Match: EntityMatchFold})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("graph: entity candidates by name: %w", err)
	}
	return candidates, nil
}

func exactNameCandidatesTx(ctx context.Context, tx neo4j.ManagedTransaction, name, key, slug string) ([]*neo4j.Record, error) {
	version, err := decodeExtractionSchemaVersion(txRows(ctx, tx, extractionSchemaVersionQuery,
		map[string]any{"id": extractionSchemaMetaID}))
	if err != nil {
		return nil, err
	}
	where := "toLower(trim(e.name)) = toLower(trim($name))"
	if version >= SchemaVersionV4 {
		where = "e.name_key = $k"
	}
	if slug != "" {
		where += " AND e.domain = $domain"
	}
	return txRows(ctx, tx, `MATCH (e:Entity) WHERE `+where+`
 WITH e ORDER BY coalesce(e.mention_count,0) DESC, e.id ASC
 WITH coalesce(e.domain,'') AS dom, collect(e)[0] AS e
 RETURN `+entityRefProjection+`
 ORDER BY coalesce(e.mention_count,0) DESC,e.id ASC
 LIMIT $limit`, map[string]any{"name": name, "k": key, "domain": slug, "limit": nameCandidateLimit})
}

func nameQueryFolds(record *FoldRecord, key, displayName, slug string) []string {
	var folds []string
	for _, scope := range record.Scopes {
		if slug != "" && scope != "@"+slug {
			continue
		}
		qualified := key
		if scope != "" {
			qualified = scope + ":" + key
		}
		if fold := FoldKeyFor(qualified, displayName, record.Exceptions); fold != nil {
			folds = append(folds, *fold)
		}
	}
	return folds
}

func canonicalNameCandidates(hits []foldCandidate) []foldCandidate {
	byDomain := make(map[string][]foldCandidate)
	for _, hit := range hits {
		byDomain[hit.Domain] = append(byDomain[hit.Domain], hit)
	}
	winners := make([]foldCandidate, 0, len(byDomain))
	for _, group := range byDomain {
		winner, _ := chooseCanonical(group)
		winners = append(winners, winner)
	}
	slices.SortFunc(winners, compareCanonical)
	return winners[:min(len(winners), nameCandidateLimit)]
}
