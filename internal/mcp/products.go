package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

type getProductArgs struct {
	graph.ProductLookup
	WithMentions  bool `json:"with_mentions,omitempty" jsonschema:"Include passages with exact identifier matches (default false)"`
	MentionsLimit *int `json:"mentions_limit,omitempty" jsonschema:"Maximum returned passages (1–20, default 5)"`
}

const getProductDescription = graph.ProductLookupDescription +
	" Set with_mentions to include passages from done documents with exact identifier matches. Domain also filters passages." +
	" Candidate search may miss separators inside stored letter runs (151L-Z, A-B12345), spaces inside stored runs (TC 2SL)," +
	" fullwidth forms, case-mapping differences (ß, İ), and tokens of 40 bytes or more. Use gkg products mentions-scan for the exact offline scan."

func getProductOutputSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[graph.ProductDetailWithMentions](nil)
	if err != nil {
		panic(err)
	}
	// The four fields are emitted together only when enrichment succeeds.
	schema.Required = slices.DeleteFunc(schema.Required, func(name string) bool {
		return name == "mentions" || strings.HasPrefix(name, "mentions_")
	})
	return schema
}

func (s *Server) handleGetProduct(ctx context.Context, req *mcpgo.CallToolRequest, args getProductArgs) (*mcpgo.CallToolResult, any, error) {
	// Validate the original JSON integer before the SDK's numeric normalization.
	var raw struct {
		MentionsLimit json.RawMessage `json:"mentions_limit"`
	}
	if len(req.Params.Arguments) != 0 {
		if err := json.Unmarshal(req.Params.Arguments, &raw); err != nil {
			return nil, nil, argerr.Errorf("bad arguments: %w", err)
		}
	}
	var limit *int
	if raw.MentionsLimit != nil {
		if err := json.Unmarshal(raw.MentionsLimit, &limit); err != nil || limit == nil || !graph.ValidMentionsLimit(*limit) {
			return nil, nil, argerr.New("mentions_limit must be an integer between 1 and 20")
		}
	}
	if s.store == nil {
		return nil, nil, errNoStore
	}
	var out any
	var err error
	if args.WithMentions {
		product, _, readErr := s.store.GetProductWithMentions(ctx, args.ProductLookup)
		displayLimit := 0
		if limit != nil {
			displayLimit = *limit
		}
		product.LimitMentions(displayLimit)
		out, err = product, readErr
	} else {
		out, err = s.store.GetProduct(ctx, args.ProductLookup)
	}
	if errors.Is(err, graph.ErrProductReadInput) {
		err = argerr.Wrap(err)
	}
	return nil, out, err
}

func (s *Server) handleFindProducts(ctx context.Context, _ *mcpgo.CallToolRequest, args graph.ProductSearchRequest) (*mcpgo.CallToolResult, graph.ProductSearchResult, error) {
	if s.store == nil {
		return nil, graph.ProductSearchResult{}, errNoStore
	}
	out, err := s.store.FindProducts(ctx, args)
	if err != nil {
		return nil, out, err
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, out, err
	}
	// The SDK duplicates structured output as text by default. Keep the
	// compatibility copy for small pages and send large pages only once.
	if len(data) > 256<<10 {
		return &mcpgo.CallToolResult{Content: []mcpgo.Content{}}, out, nil
	}
	return nil, out, nil
}

func (s *Server) handleListFeatures(ctx context.Context, _ *mcpgo.CallToolRequest, _ struct{}) (*mcpgo.CallToolResult, graph.ProductFeatures, error) {
	if s.store == nil {
		return nil, graph.ProductFeatures{}, errNoStore
	}
	out, err := s.store.ListProductFeatures(ctx)
	return nil, out, err
}
