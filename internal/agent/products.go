package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
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
	graph.ProductDetailWithMentions
	FeaturesOmitted int `json:"features_omitted,omitempty"`
	MentionsOmitted int `json:"mentions_omitted_for_budget,omitempty"`
	PagesOmitted    int `json:"pages_omitted_for_budget,omitempty"`
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
	var args struct {
		graph.ProductLookup
		WithMentions  json.RawMessage `json:"with_mentions"`
		MentionsLimit json.RawMessage `json:"mentions_limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	var withMentions *bool
	if args.WithMentions != nil {
		if err := json.Unmarshal(args.WithMentions, &withMentions); err != nil || withMentions == nil {
			return nil, argerr.New("with_mentions must be a boolean")
		}
	}
	var limit *int
	if args.MentionsLimit != nil {
		if err := json.Unmarshal(args.MentionsLimit, &limit); err != nil || limit == nil || !graph.ValidMentionsLimit(*limit) {
			return nil, argerr.New(fmt.Sprintf("mentions_limit must be an integer between 1 and %d", graph.MaxMentionsLimit))
		}
	}
	if scope := requestDomain(ctx); scope != "" {
		if args.Domain != "" && args.Domain != scope {
			return nil, argerr.New("domain outside the request scope")
		}
		args.Domain, args.SeedDomain = scope, scope
	}
	var detail graph.ProductDetailWithMentions
	var err error
	// The agent can only read the exact keys it asks for.
	emptyTexts := args.WithTexts && len(args.TextKeys) == 0
	if emptyTexts {
		args.WithTexts = false
	}
	if withMentions == nil || *withMentions {
		detail, _, err = s.store.GetProductWithMentions(ctx, args.ProductLookup)
	} else {
		detail.ProductDetail, err = s.store.GetProduct(ctx, args.ProductLookup)
	}
	if errors.Is(err, graph.ErrProductReadInput) {
		return nil, argerr.Wrap(err)
	}
	if err != nil {
		return nil, err
	}
	if emptyTexts {
		detail.ProductTextFields = &graph.ProductTextFields{Texts: []graph.ProductText{}}
	}
	displayLimit := 0 // The shared read uses zero for its default display limit.
	if limit != nil {
		displayLimit = *limit
	}
	detail.LimitMentions(displayLimit)
	featureCount, pageCount, mentionCount := len(detail.Features), len(detail.Pages), 0
	if detail.ProductMentionFields != nil {
		mentionCount = len(detail.Mentions)
	}
	// Reserve the first page and mention when present. Fit features before
	// additional pages and mentions, preserving the order within each list.
	firstPage, firstMention := min(1, pageCount), min(1, mentionCount)
	extraPages, extraMentions := pageCount-firstPage, mentionCount-firstMention
	textCount := 0
	if detail.ProductTextFields != nil {
		textCount = min(8, len(detail.Texts))
	}
	return fitProductResult(textCount+featureCount+extraPages+extraMentions, func(n int) productDetailOut {
		texts := min(n, textCount)
		features := min(n-texts, featureCount)
		remaining := n - texts - features
		pages := firstPage + min(remaining, extraPages)
		mentions := firstMention + max(0, remaining-extraPages)
		out := productDetailOut{
			ProductDetailWithMentions: detail,
			FeaturesOmitted:           featureCount - features,
			MentionsOmitted:           mentionCount - mentions,
			PagesOmitted:              pageCount - pages,
		}
		out.Features = detail.Features[:features]
		out.Pages = detail.Pages[:pages]
		if detail.ProductTextFields != nil {
			out.ProductTextFields = &graph.ProductTextFields{
				Texts: detail.Texts[:texts], TextsOmitted: detail.TextsOmitted + len(detail.Texts) - texts,
			}
		}
		if detail.ProductMentionFields != nil {
			fields := *detail.ProductMentionFields
			fields.Mentions = fields.Mentions[:mentions]
			out.ProductMentionFields = &fields
		}
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
	if args.MeaningQuery() != "" {
		out, err := search.FindProductsQuery(ctx, s.store, s.emb, s.cfg.ProductText, args)
		if errors.Is(err, graph.ErrProductReadInput) {
			err = argerr.Wrap(err)
		}
		if err != nil {
			return nil, err
		}
		return fitProductQueryResult(out)
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

func fitProductQueryResult(out search.ProductQueryResult) (search.ProductQueryResult, error) {
	out.Hits = slices.Clone(out.Hits)
	for i := range out.Hits {
		hit := &out.Hits[i]
		hit.Matched = slices.Clone(hit.Matched[:min(1, len(hit.Matched))])
		if len(hit.Matched) == 0 {
			continue
		}
		match := &hit.Matched[0]
		runes := []rune(match.Text)
		if len(runes) > 160 {
			match.Text = string(runes[:160])
			match.ExcerptCut = true
		}
	}
	wire, err := json.Marshal(out)
	if err != nil || len(wire) <= agentResultBudget {
		return out, err
	}
	for i := range out.Hits {
		out.Hits[i].Title = ""
		out.Hits[i].Brand = ""
	}
	return fitProductResult(len(out.Hits), func(n int) search.ProductQueryResult {
		page := out
		page.Hits = out.Hits[:n]
		return page
	})
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
