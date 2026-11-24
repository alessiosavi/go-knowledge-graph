package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

const productSearchTimeout = 200 * time.Millisecond

// passageSearchStore remembers the chunk rows for a single search. If auto
// needs to recover a passage cut by rank fusion, the chunk search reuses them.
type passageSearchStore struct {
	search.Store
	passages []graph.ChunkSearchRow
}

func (s *passageSearchStore) ChunkTextSearchInWindow(ctx context.Context, query, domain string, limit int, window graph.DateWindow) ([]graph.ChunkSearchRow, error) {
	if len(s.passages) != 0 {
		return s.passages[:min(limit, len(s.passages))], nil
	}
	rows, err := s.Store.ChunkTextSearchInWindow(ctx, query, domain, limit, window)
	s.passages = rows
	return rows, err
}

func (s *Server) searchProductHits(ctx context.Context, hits []searchHitOut, query, domain string, limit int) ([]searchHitOut, string) {
	ctx, cancel := context.WithTimeout(ctx, productSearchTimeout)
	defer cancel()
	type result struct {
		products []graph.Product
		err      error
	}
	results := make(chan result, 1)
	go func() {
		products, err := s.store.SearchProductLinks(ctx, query, domain, limit)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		results <- result{products, err}
	}()

	var found result
	select {
	case found = <-results:
	case <-ctx.Done():
		found.err = ctx.Err()
	}
	switch {
	case graph.IsQueryTimeout(ctx, found.err):
		return hits, "timeout"
	case errors.Is(found.err, graph.ErrProductLinkSearchTooLarge):
		return hits, "index_too_large"
	case found.err != nil:
		slog.WarnContext(ctx, "agent: product search unavailable", "err", found.err)
		return hits, "index_unavailable"
	}
	for _, p := range found.products[:min(limit, len(found.products))] {
		hits = append(hits, searchHitOut{
			Kind: "product", ID: p.ID, Domain: p.Domain, Store: p.Store, ProductID: p.ProductID,
			Title: truncateSearchRunes(p.Title, 200), Brand: truncateSearchRunes(p.Brand, 80),
		})
	}
	return hits, ""
}

func truncateSearchRunes(text string, limit int) string {
	for offset := range text {
		if limit == 0 {
			return text[:offset]
		}
		limit--
	}
	return text
}

// Fit complete JSON before transcriptResult runs. Reserve space for the
// leading hit and, in a mixed result, the best passage before filling in
// other hits. Emit the selected hits in their original order.
func fitSearchHits(out searchOut) (searchOut, error) {
	if len(out.Hits) == 0 {
		return out, nil
	}
	frame, err := json.Marshal(searchOut{Hits: []searchHitOut{}, ProductsError: out.ProductsError})
	if err != nil {
		return searchOut{}, err
	}
	budget := toolResultCap - len(frame) + 1 // the first hit needs no comma
	passage := -1
	if out.Hits[0].Kind == "product" {
		passage = slices.IndexFunc(out.Hits, func(hit searchHitOut) bool { return hit.Kind == "chunk" })
	}
	priority := make([]int, 0, len(out.Hits))
	priority = append(priority, 0)
	if passage > 0 {
		priority = append(priority, passage)
	}
	for i := 1; i < len(out.Hits); i++ {
		if i != passage {
			priority = append(priority, i)
		}
	}
	keep := make([]bool, len(out.Hits))
	for _, i := range priority {
		hit := &out.Hits[i]
		raw, err := json.Marshal(hit)
		if err != nil {
			return searchOut{}, err
		}
		if len(raw)+1 > budget && (i == 0 || i == passage) {
			// Regular document titles can be large. Keep the passage text and
			// identity even when its full title would consume the budget.
			hit.Title = truncateSearchRunes(hit.Title, 200)
			raw, err = json.Marshal(hit)
			if err != nil {
				return searchOut{}, err
			}
			if len(raw)+1 > budget {
				return searchOut{}, errors.New("required search hit exceeds the agent result budget")
			}
		}
		if len(raw)+1 <= budget {
			keep[i] = true
			budget -= len(raw) + 1
		}
	}
	hits := out.Hits[:0]
	for i, hit := range out.Hits {
		if keep[i] {
			hits = append(hits, hit)
		}
	}
	out.Hits = hits
	return out, nil
}
