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
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
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
	// Mentions and stored texts are optional enrichments.
	schema.Required = slices.DeleteFunc(schema.Required, func(name string) bool {
		return name == "mentions" || strings.HasPrefix(name, "mentions_") || name == "texts"
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

func findProductsInputSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[graph.ProductSearchRequest](nil)
	if err != nil {
		panic(err)
	}
	// These string fields predate query mode. Go decodes null to their empty
	// string, so accept both representations in the SDK's pre-validation.
	for _, name := range []string{"feature", "cursor"} {
		prop := schema.Properties[name]
		prop.Types, prop.Type = []string{prop.Type, "null"}, ""
	}
	return schema
}

func findProductsOutputSchema() *jsonschema.Schema {
	feature, err := jsonschema.For[graph.ProductSearchResult](nil)
	if err != nil {
		panic(err)
	}
	query, err := jsonschema.For[search.ProductQueryResult](nil)
	if err != nil {
		panic(err)
	}
	return &jsonschema.Schema{Type: "object", OneOf: []*jsonschema.Schema{feature, query}}
}

func (s *Server) handleFindProducts(ctx context.Context, req *mcpgo.CallToolRequest, args graph.ProductSearchRequest) (*mcpgo.CallToolResult, any, error) {
	if s.store == nil {
		return nil, nil, errNoStore
	}
	var out any
	var err error
	if args.MeaningQuery() != "" {
		// Validate integers before SDK normalization, as on REST and agent.
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, nil, argerr.Errorf("bad arguments: %w", err)
		}
		out, err = search.FindProductsQuery(ctx, s.store, s.emb, s.cfg.ProductText, args)
	} else {
		out, err = s.store.FindProducts(ctx, args)
	}
	if errors.Is(err, graph.ErrProductReadInput) {
		err = argerr.Wrap(err)
	}
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
