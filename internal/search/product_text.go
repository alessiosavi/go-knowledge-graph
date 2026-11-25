package search

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

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

// FindProductsQuery is shared by REST, MCP and the agent. The leg's timeout
// covers checking the index, embedding the query once and reading the graph.
func FindProductsQuery(ctx context.Context, st ProductTextStore, emb QueryEmbedder, cfg config.ProductTextConfig, req graph.ProductSearchRequest) (ProductQueryResult, error) {
	out := ProductQueryResult{Mode: "query", Hits: []graph.ProductTextHit{}, StoresNotMaterialized: []string{}}
	query := req.MeaningQuery()
	limit, err := req.ValidateProductTextQuery(query)
	if err != nil {
		return out, err
	}
	if !cfg.Search {
		out.MeaningStatus = "disabled"
		return out, nil
	}
	spaced, ok := emb.(interface{ Space() string })
	if !ok || spaced.Space() != graph.ProductTextSpace {
		out.MeaningStatus = "unavailable"
		return out, nil
	}
	timeout := cfg.SearchTimeout
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
	if err := ctx.Err(); err != nil {
		return degrade(err)
	}
	if !validProductQueryVector(vec) {
		return degrade(errors.New("embedder returned an invalid query vector"))
	}
	result, err := st.SearchProductTexts(ctx, vec, req.Domain, limit, nil)
	if err != nil {
		return degrade(err)
	}
	if err := ctx.Err(); err != nil {
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
