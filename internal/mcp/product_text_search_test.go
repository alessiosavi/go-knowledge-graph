package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

func TestFindProductsQueryToolArguments(t *testing.T) {
	s := NewServer(config.Config{}, &graph.Store{}, nil)
	session := connectServer(t, s)
	for field, value := range map[string]any{
		"feature": "code", "value": "ceramic", "min": "1", "max": "5", "cursor": "previous",
		"limit": 21, "domain": "Bad!", "query": strings.Repeat("é", 501),
	} {
		args := map[string]any{"query": "ceramic"}
		args[field] = value
		result := callTool(t, session, "find_products", args)
		wire, err := json.Marshal(result.Content)
		if err != nil || !result.IsError || !strings.Contains(string(wire), field) {
			t.Errorf("%s: %+v %s %v", field, result, wire, err)
		}
	}
	for _, limit := range []any{0, -1, 1.5, json.Number("1e1"), "10"} {
		result := callTool(t, session, "find_products", map[string]any{"query": "ceramic", "limit": limit})
		wire, err := json.Marshal(result.Content)
		if err != nil || !result.IsError || !strings.Contains(string(wire), "limit") {
			t.Errorf("limit %v: %+v %s %v", limit, result, wire, err)
		}
	}
	result := callTool(t, session, "find_products", map[string]any{
		"query": "ceramic", "feature": "", "value": "", "min": nil, "max": "", "cursor": nil,
	})
	wire, err := json.Marshal(result.StructuredContent)
	if err != nil || result.IsError || !strings.Contains(string(wire), `"meaning_status":"disabled"`) {
		t.Fatalf("disabled response: %+v %s %v", result, wire, err)
	}
	for _, query := range []any{nil, "  "} {
		result := callTool(t, session, "find_products", map[string]any{
			"query": query, "limit": 51, "feature": "code", "value": "1",
		})
		wire, err := json.Marshal(result.Content)
		if err != nil || !result.IsError || !strings.Contains(string(wire), "50") {
			t.Errorf("expected feature limit for %v: %+v %s %v", query, result, wire, err)
		}
	}
}

func TestProductTextReadMCP(t *testing.T) {
	st := graphtest.ProductTextReads(t)
	cfg := config.Config{ProductText: config.ProductTextConfig{Search: true}}
	s := NewServer(cfg, st, nil)
	s.SetEmbedder(graphtest.ProductTextEmbedder{})
	session := connectServer(t, s)
	r := callTool(t, session, "find_products", map[string]any{"query": "ceramic", "domain": graphtest.TextReadDomain})
	wire, err := json.Marshal(r.StructuredContent)
	var query search.ProductQueryResult
	if err != nil || r.IsError || json.Unmarshal(wire, &query) != nil || query.MeaningStatus != "partial" ||
		len(query.Hits) != 1 || len(query.Hits[0].Matched) != 1 || query.Hits[0].Matched[0].Key != "key-1" {
		t.Fatalf("query: %+v %s (%v)", r, wire, err)
	}
	for _, tc := range []struct {
		id             string
		keys           []string
		texts, omitted int
	}{
		{graphtest.TextReadManyID, nil, 64, 6},
		{graphtest.TextReadManyID, []string{"key-1", "missing"}, 1, 0},
		{graphtest.TextReadLargeID, nil, 2, 20},
	} {
		args := map[string]any{"id": tc.id, "with_texts": true}
		if tc.keys != nil {
			args["text_keys"] = tc.keys
		}
		r := callTool(t, session, "get_product", args)
		wire, err := json.Marshal(r.StructuredContent)
		var out graph.ProductDetail
		if err != nil || r.IsError || json.Unmarshal(wire, &out) != nil ||
			out.ProductTextFields == nil || len(out.Texts) != tc.texts || out.TextsOmitted != tc.omitted {
			t.Fatalf("lookup %v: %+v (encoded %d bytes, %v)", args, r, len(wire), err)
		}
	}
}
