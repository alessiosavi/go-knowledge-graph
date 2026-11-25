package search

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

var ErrProductMeaningUnavailable = errors.New("product meaning search failed")

type ProductTextStore interface {
	CheckProductTextIndex(context.Context) error
	SearchProductTexts(context.Context, []float64, string, int, []string) (graph.ProductTextSearchResult, error)
}

type ProductQueryResult struct {
	Mode                  string                 `json:"mode"`
	Hits                  []graph.ProductTextHit `json:"hits"`
	Exhausted             bool                   `json:"exhausted"`
	CandidatesTruncated   bool                   `json:"candidates_truncated"`
	MeaningStatus         string                 `json:"meaning_status"`
	StoresNotMaterialized []string               `json:"stores_not_materialized"`
}

// ProductMeaningAvailability returns an inactive status or an empty string
// when the configured embedder uses the product text index's space.
func ProductMeaningAvailability(enabled bool, emb QueryEmbedder) string {
	if !enabled {
		return "disabled"
	}
	spaced, ok := emb.(interface{ Space() string })
	if !ok || spaced.Space() != graph.ProductTextSpace {
		return "unavailable"
	}
	return ""
}

// ReadProductMeaning validates the vector and searches within the caller's
// deadline. Exclusions apply in the store before the limit.
func ReadProductMeaning(ctx context.Context, st ProductTextStore, vec []float64, domain string, limit int, excludeIDs []string) (graph.ProductTextSearchResult, error) {
	if err := ctx.Err(); err != nil {
		return graph.ProductTextSearchResult{}, err
	}
	if !validProductQueryVector(vec) {
		return graph.ProductTextSearchResult{}, errors.New("embedder returned an invalid query vector")
	}
	result, err := st.SearchProductTexts(ctx, vec, domain, limit, excludeIDs)
	if err == nil {
		err = ctx.Err()
	}
	return result, err
}

// FindProductsQuery is shared by REST, MCP and the agent. The leg's timeout
// covers checking the index, embedding the query once and reading the graph.
func FindProductsQuery(ctx context.Context, st ProductTextStore, emb QueryEmbedder, cfg config.ProductTextConfig, req graph.ProductSearchRequest) (ProductQueryResult, error) {
	out := ProductQueryResult{Mode: "query", Hits: []graph.ProductTextHit{}, StoresNotMaterialized: []string{}}
	query := req.MeaningQuery()
	limit, err := req.ValidateProductTextQuery(query)
	if err != nil {
		return out, err
	}
	if status := ProductMeaningAvailability(cfg.Search, emb); status != "" {
		out.MeaningStatus = status
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.SearchBudget())
	defer cancel()

	degrade := func(err error) (ProductQueryResult, error) {
		switch {
		case errors.Is(err, graph.ErrProductTextIndexUnavailable):
			out.MeaningStatus = "unavailable"
		case graph.IsQueryTimeout(ctx, err):
			out.MeaningStatus = "timeout"
		case errors.Is(ctx.Err(), context.Canceled):
			return out, ctx.Err()
		default:
			out.MeaningStatus = "error"
			return out, fmt.Errorf("%w: %w", ErrProductMeaningUnavailable, err)
		}
		return out, nil
	}
	if err := st.CheckProductTextIndex(ctx); err != nil {
		return degrade(err)
	}
	vec, err := emb.EmbedQuery(ctx, query)
	if err != nil {
		return degrade(err)
	}
	result, err := ReadProductMeaning(ctx, st, vec, req.Domain, limit, nil)
	if err != nil {
		return degrade(err)
	}
	out.Hits = result.Hits
	out.Exhausted = result.Exhausted
	out.CandidatesTruncated = result.CandidatesTruncated
	out.StoresNotMaterialized = result.StoresNotMaterialized
	out.MeaningStatus = "ok"
	if result.Partial {
		out.MeaningStatus = "partial"
	}
	return out, nil
}

func validProductQueryVector(vec []float64) bool {
	if len(vec) != 768 {
		return false
	}
	var nonzero bool
	for _, x := range vec {
		if math.IsInf(x, 0) || math.IsNaN(x) || math.Abs(x) > math.MaxFloat32 {
			return false
		}
		nonzero = nonzero || float32(x) != 0
	}
	return nonzero
}
