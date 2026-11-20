package mcp

import (
	"context"
	"encoding/json"

	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func (s *Server) handleGetProduct(ctx context.Context, _ *mcpgo.CallToolRequest, args graph.ProductLookup) (*mcpgo.CallToolResult, graph.ProductDetail, error) {
	if s.store == nil {
		return nil, graph.ProductDetail{}, errNoStore
	}
	out, err := s.store.GetProduct(ctx, args)
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
