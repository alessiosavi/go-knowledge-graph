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
			Kind: "product", ID: p.ID, Domain: &p.Domain, Store: p.Store, ProductID: p.ProductID,
			Title: truncateSearchRunes(p.Title, 200), Brand: truncateSearchRunes(p.Brand, 80),
		})
	}
	return hits, ""
}

func (s *Server) searchMeaningHits(ctx context.Context, future *search.QueryEmbedding, hits []searchHitOut, domain string, limit int) ([]searchHitOut, string) {
	remaining := limit - len(hits)
	excluded := make([]string, 0, len(hits))
	for _, hit := range hits {
		excluded = append(excluded, hit.ID)
	}
	vec, err := future.Wait(ctx)
	var result graph.ProductTextSearchResult
	if err == nil {
		result, err = search.ReadProductMeaning(ctx, s.store, vec, domain, remaining, excluded)
	}
	switch {
	case graph.IsQueryTimeout(ctx, err):
		return hits, "timeout"
	case err != nil:
		slog.WarnContext(ctx, "agent: product meaning search unavailable", "err", err)
		return hits, "unavailable"
	}
	for _, p := range result.Hits[:min(remaining, len(result.Hits))] {
		if len(p.Matched) == 0 {
			continue
		}
		matched := p.Matched[0] // the graph ranks products by their best matched text
		excerpt := truncateSearchRunes(matched.Text, 160)
		hits = append(hits, searchHitOut{
			Kind: "product", ID: p.ID, Domain: &p.Domain, Store: p.Store, ProductID: p.ProductID,
			Title: truncateSearchRunes(p.Title, 200), Brand: truncateSearchRunes(p.Brand, 80), Score: &p.Score,
			Match: "meaning", Key: &matched.Key, Checked: &matched.Checked, Excerpt: &excerpt,
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
	frame := searchOut{Hits: []searchHitOut{}, ProductsError: out.ProductsError, MeaningError: out.MeaningError}
	raw, err := json.Marshal(frame)
	if err != nil {
		return searchOut{}, err
	}
	budget := toolResultCap - len(raw) + 1 // the first hit needs no comma
	sizes := make([]int, len(out.Hits))
	total := 0
	for i, hit := range out.Hits {
		sizes[i], err = searchHitSize(hit)
		if err != nil {
			return searchOut{}, err
		}
		total += sizes[i]
	}
	if total <= budget {
		return out, nil
	}
	matchedProducts := out.Hits[0].Match != ""
	if matchedProducts {
		// Keep identifying and matched evidence on every product before
		// considering dropping any products.
		for i, hit := range out.Hits {
			if hit.Kind != "product" {
				break
			}
			hit.Domain, hit.Score = nil, nil
			hit.Title, hit.Brand, hit.Store = "", "", ""
			out.Hits[i] = hit
			total -= sizes[i]
			sizes[i], err = searchHitSize(hit)
			if err != nil {
				return searchOut{}, err
			}
			total += sizes[i]
		}
		if total <= budget {
			return out, nil
		}
	}

	// Reserve enough room for the largest possible omission count before
	// choosing hits. The field's actual count is set after selection.
	frame.HitsOmitted = len(out.Hits)
	raw, err = json.Marshal(frame)
	if err != nil {
		return searchOut{}, err
	}
	budget = toolResultCap - len(raw) + 1

	passage := -1
	if out.Hits[0].Kind == "product" {
		passage = slices.IndexFunc(out.Hits, func(hit searchHitOut) bool { return hit.Kind == "chunk" })
	}
	if passage >= 0 {
		if sizes[0]+sizes[passage] > budget {
			hit := &out.Hits[passage]
			hit.Title = truncateSearchRunes(hit.Title, 200)
			sizes[passage], err = searchHitSize(*hit)
			if err != nil {
				return searchOut{}, err
			}
		}
		budget -= sizes[passage]
		if budget < 0 {
			return searchOut{}, errors.New("required search hit exceeds the agent result budget")
		}
	}
	if !matchedProducts && sizes[0] > budget {
		hit := &out.Hits[0]
		if hit.Kind == "product" {
			*hit, sizes[0], err = compactSearchProduct(*hit, budget)
		} else {
			hit.Title = truncateSearchRunes(hit.Title, 200)
			sizes[0], err = searchHitSize(*hit)
		}
		if err != nil {
			return searchOut{}, err
		}
		if hit.Kind != "product" && sizes[0] > budget {
			return searchOut{}, errors.New("required search hit exceeds the agent result budget")
		}
	}

	hits := out.Hits[:0]
	omitProducts := false
	for i, hit := range out.Hits {
		if i == passage {
			hits = append(hits, hit) // already paid for
			continue
		}
		if hit.Match != "" && (omitProducts || sizes[i] > budget) {
			// Keep a prefix of the ranked products even when a later
			// product would fit in the space left by an omitted one.
			omitProducts = true
			continue
		}
		if sizes[i] <= budget {
			hits = append(hits, hit)
			budget -= sizes[i]
		}
	}
	out.HitsOmitted = len(out.Hits) - len(hits)
	out.Hits = hits
	return out, nil
}

func searchHitSize(hit searchHitOut) (int, error) {
	raw, err := json.Marshal(hit)
	return len(raw) + 1, err // include a separator between hits
}

func compactSearchProduct(hit searchHitOut, budget int) (searchHitOut, int, error) {
	compact := searchHitOut{Kind: hit.Kind, ID: hit.ID, Title: hit.Title}
	size, err := searchHitSize(compact)
	if err != nil || size <= budget {
		return compact, size, err
	}
	compact.Title = ""
	emptySize, err := searchHitSize(compact)
	if err != nil || emptySize >= budget {
		return compact, emptySize, err
	}
	// JSON can use up to six bytes per title rune, including HTML escapes.
	compact.Title = truncateSearchRunes(hit.Title, (budget-emptySize)/6)
	size, err = searchHitSize(compact)
	return compact, size, err
}
