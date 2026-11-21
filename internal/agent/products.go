package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const (
	productSearchDefaultLimit = 10
	productSearchMaxLimit     = 20
	// transcriptResult adds no bytes to a complete result. Reserve 256 bytes
	// for fixed chat/plan framing plus a 512-byte margin. The cap is on
	// decoded result text; call IDs, arguments, and HTTP JSON escaping are
	// outside that text.
	productResultFrameReserve = 256
	productResultMargin       = 512
	agentResultBudget         = toolResultCap - productResultFrameReserve - productResultMargin
)

type productDetailOut struct {
	graph.ProductDetail
	FeaturesOmitted int `json:"features_omitted,omitempty"`
}

type productFeatureOut struct {
	graph.ProductFeature
	Released      bool     `json:"released"`
	NumericSearch bool     `json:"numeric_search"`
	Stores        []string `json:"stores"`
}

type productFeaturesOut struct {
	ActiveReleaseID *string             `json:"active_release_id"`
	Features        []productFeatureOut `json:"features"`
	Truncated       bool                `json:"truncated,omitempty"`
	Omitted         int                 `json:"omitted,omitempty"`
}

func (s *Server) toolGetProduct(ctx context.Context, raw json.RawMessage) (any, error) {
	var args graph.ProductLookup
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	detail, err := s.store.GetProduct(ctx, args)
	if errors.Is(err, graph.ErrProductReadInput) {
		return nil, argerr.Wrap(err)
	}
	if err != nil {
		return nil, err
	}
	if scope := requestDomain(ctx); scope != "" && detail.Domain != scope {
		return nil, graph.ErrNotFound
	}
	return fitProductResult(len(detail.Features), func(n int) productDetailOut {
		out := productDetailOut{ProductDetail: detail, FeaturesOmitted: len(detail.Features) - n}
		out.Features = detail.Features[:n]
		return out
	})
}

func (s *Server) toolFindProducts(ctx context.Context, raw json.RawMessage) (any, error) {
	var args graph.ProductSearchRequest
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if scope := requestDomain(ctx); scope != "" {
		if args.Domain != "" && args.Domain != scope {
			return nil, argerr.New("domain outside the request scope")
		}
		args.Domain = scope
	}
	limit := productSearchDefaultLimit
	if args.Limit != nil && *args.Limit > 0 {
		limit = min(*args.Limit, productSearchMaxLimit)
	}
	args.Limit = &limit
	out, err := s.store.FindProductsBudget(ctx, args, agentResultBudget)
	if errors.Is(err, graph.ErrProductReadInput) {
		return nil, argerr.Wrap(err)
	}
	return out, err
}

func (s *Server) toolListFeatures(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	listed, err := s.store.ListProductFeatures(ctx)
	if err != nil {
		return nil, err
	}
	var scopeStores map[string]bool
	if scope := requestDomain(ctx); scope != "" {
		// Store bindings and the registry come from separate read snapshots.
		stores, err := s.store.ListProductStores(ctx)
		if err != nil {
			return nil, err
		}
		scopeStores = make(map[string]bool)
		for _, store := range stores.Stores {
			if store.Domain == scope {
				scopeStores[store.Store] = true
			}
		}
	}
	query := strings.ToLower(args.Query)
	out := productFeaturesOut{ActiveReleaseID: listed.ActiveReleaseID, Features: []productFeatureOut{}}
	// The graph listing orders definitions by ID.
	for _, feature := range listed.Features {
		if query != "" && !strings.Contains(strings.ToLower(feature.ID), query) && !strings.Contains(strings.ToLower(feature.Name), query) {
			continue
		}
		item := productFeatureOut{ProductFeature: feature.ProductFeature, Stores: []string{}}
		inScope := scopeStores == nil
		for _, key := range feature.Keys {
			if scopeStores != nil && !scopeStores[key.Store] {
				continue
			}
			inScope = true
			if key.Released {
				item.Released = true
				item.NumericSearch = item.NumericSearch || key.NumericSearch
				item.Stores = append(item.Stores, key.Store)
			}
		}
		if inScope {
			slices.Sort(item.Stores)
			item.Stores = slices.Compact(item.Stores)
			out.Features = append(out.Features, item)
		}
	}
	return fitProductResult(len(out.Features), func(n int) productFeaturesOut {
		page := out
		page.Features = out.Features[:n]
		page.Omitted = len(out.Features) - n
		page.Truncated = page.Omitted > 0
		return page
	})
}

// fitProductResult checks the complete encoding before looking for the largest
// prefix that fits with omission fields present. Each additional entry costs
// more than the omission count can shrink.
func fitProductResult[T any](count int, prefix func(int) T) (T, error) {
	full := prefix(count)
	wire, err := json.Marshal(full)
	if err != nil || len(wire) <= agentResultBudget {
		return full, err
	}
	base := prefix(0)
	wire, err = json.Marshal(base)
	if err != nil {
		return base, err
	}
	if len(wire) > agentResultBudget {
		return base, errors.New("product tool metadata exceeds the agent result budget")
	}
	low, high := 0, count
	for high-low > 1 {
		middle := low + (high-low)/2
		candidate := prefix(middle)
		wire, err := json.Marshal(candidate)
		if err != nil {
			return candidate, err
		}
		if len(wire) <= agentResultBudget {
			low = middle
		} else {
			high = middle
		}
	}
	return prefix(low), nil
}
