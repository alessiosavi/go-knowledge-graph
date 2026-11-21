package chunkread

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const (
	DefaultProductsLimit = 10
	MaxProductsLimit     = 50
	maxProductEvidence   = 8
	maxMatchBytes        = 2 << 20
	productIndexTimeout  = 5 * time.Second

	ProductsLimitError       = "products_limit must be an integer between 1 and 50"
	WithProductsDescription  = "Include exact product identifier matches on the full chunk text (default false)"
	ProductsLimitDescription = "Maximum matched products per chunk (1–50, default 10)"
	ProductsToolDescription  = " Set with_products to list exact product identifier matches, including cross-domain products. Retry a single chunk with a higher products_limit for more results. Use gkg products mentions-scan for the complete offline scan."
)

type MatchedIdentifier struct {
	Feature string `json:"feature"`
	Value   string `json:"value"`
}

type ChunkProduct struct {
	ID                  string              `json:"id"`
	Store               string              `json:"store"`
	ProductID           string              `json:"product_id"`
	Title               string              `json:"title"`
	Matched             []MatchedIdentifier `json:"matched"`
	OutsideReturnedText bool                `json:"outside_returned_text,omitempty"`
}

// ProductMentionIndexLoader is optional on the chunk Store.
type ProductMentionIndexLoader interface {
	LoadProductMentionIndex(context.Context) (*graph.ProductMentionIndex, error)
}

func enrichProducts(ctx context.Context, store Store, result *Result, fullTexts []string, limit int) error {
	var bytes int
	for _, full := range fullTexts {
		if len(full) > maxMatchBytes-bytes {
			result.ProductsError = "match_budget"
			return nil
		}
		bytes += len(full)
	}
	loader, ok := store.(ProductMentionIndexLoader)
	if !ok {
		result.ProductsError = "index_unavailable"
		return nil
	}
	loadCtx, cancel := context.WithTimeout(ctx, productIndexTimeout)
	index, err := loader.LoadProductMentionIndex(loadCtx)
	if err == nil {
		err = loadCtx.Err()
		if err == nil && index == nil {
			err = errors.New("product mention loader returned no index")
		}
	}
	cancel()
	if err != nil {
		if errors.Is(err, graph.ErrProductMentionIndexTooLarge) {
			result.ProductsError = "index_too_large"
		} else {
			slog.WarnContext(ctx, "get_chunks product mention index unavailable", "error", err)
			result.ProductsError = "index_unavailable"
		}
		return nil
	}

	pages := make([][]ChunkProduct, len(fullTexts))
	for i := range result.Chunks {
		item := &result.Chunks[i]
		products := collectChunkProducts(index.Match(fullTexts[i]), len(item.Text))
		empty := products[:0]
		item.Products, item.ProductsTotal = &empty, new(len(products))
		pages[i] = products[:min(limit, len(products))]
		item.ProductsOmittedForBudget = len(pages[i])
	}
	return fitProductEntries(result, pages)
}

// Start with every product trimmed, then restore the longest prefix that fits.
// This is equivalent to removing products from the last chunk first. Measuring
// only each changed item after encoding the envelope avoids re-encoding all
// chunk text, and any chunk dropping still happens after this step.
func fitProductEntries(result *Result, pages [][]ChunkProduct) error {
	wire, err := json.Marshal(result)
	if err != nil || len(wire) >= ResponseBudget {
		return err
	}
	remaining := ResponseBudget - len(wire)
	for i, products := range pages {
		if len(products) == 0 {
			continue
		}
		item := &result.Chunks[i]
		before, err := json.Marshal(item)
		if err != nil {
			return err
		}
		size := len(before)
		for kept := 1; kept <= len(products); kept++ {
			*item.Products = products[:kept]
			item.ProductsOmittedForBudget = len(products) - kept
			after, err := json.Marshal(item)
			if err != nil {
				return err
			}
			delta := len(after) - size
			if delta > remaining {
				*item.Products = products[:kept-1]
				item.ProductsOmittedForBudget++
				return nil
			}
			size = len(after)
			remaining -= delta
		}
	}
	return nil
}

func collectChunkProducts(matches []graph.ProductMentionMatch, prefixBytes int) []ChunkProduct {
	byID := make(map[string]*ChunkProduct)
	for _, match := range matches {
		product := match.Product
		out := byID[product.ID]
		if out == nil {
			out = &ChunkProduct{
				ID: product.ID, Store: product.Store, ProductID: product.ProductID, Title: product.Title,
				OutsideReturnedText: true,
			}
			byID[product.ID] = out
		}
		out.OutsideReturnedText = out.OutsideReturnedText && match.Span[1] > prefixBytes
		for _, evidence := range match.Evidence {
			value := MatchedIdentifier{Feature: evidence.Feature, Value: evidence.ValueFold}
			i, found := slices.BinarySearchFunc(out.Matched, value, compareIdentifiers)
			if found || i >= maxProductEvidence {
				continue
			}
			out.Matched = slices.Insert(out.Matched, i, value)
			out.Matched = out.Matched[:min(len(out.Matched), maxProductEvidence)]
		}
	}
	products := make([]ChunkProduct, 0, len(byID))
	for out := range maps.Values(byID) {
		products = append(products, *out)
	}
	slices.SortFunc(products, func(a, b ChunkProduct) int {
		return cmp.Or(cmp.Compare(a.Store, b.Store), cmp.Compare(a.ProductID, b.ProductID), cmp.Compare(a.ID, b.ID))
	})
	return products
}

func compareIdentifiers(a, b MatchedIdentifier) int {
	return cmp.Or(cmp.Compare(a.Feature, b.Feature), cmp.Compare(a.Value, b.Value))
}
