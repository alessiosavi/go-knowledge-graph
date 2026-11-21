package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestProductToolSchemas(t *testing.T) {
	defs := make(map[string]toolDef)
	for _, def := range toolDefs() {
		defs[def.Function.Name] = def
	}
	for _, tc := range []struct {
		name     string
		fields   map[string]string
		required []string
	}{
		{"get_product", map[string]string{"id": "string", "store": "string", "product_id": "string"}, nil},
		{"find_products", map[string]string{"domain": "string", "feature": "string", "value": "string", "min": "string", "max": "string", "limit": "integer", "cursor": "string"}, []string{"feature"}},
		{"list_features", map[string]string{"query": "string"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, ok := defs[tc.name]
			if !ok {
				t.Fatal("tool is missing")
			}
			schema := unmarshalResult[struct {
				Type       string
				Properties map[string]struct{ Type string }
				Required   []string
			}](t, def.Function.Parameters)
			if def.Type != "function" || schema.Type != "object" || !slices.Equal(schema.Required, tc.required) {
				t.Fatalf("unexpected tool schema: %+v", def)
			}
			got := make(map[string]string)
			for name, prop := range schema.Properties {
				got[name] = prop.Type
			}
			if !maps.Equal(got, tc.fields) {
				t.Errorf("schema fields = %v, want %v", got, tc.fields)
			}
		})
	}
}

func TestProductToolArgumentErrors(t *testing.T) {
	// These calls must fail argument validation before any database access.
	s := NewServer(config.Config{}, &graph.Store{}, nil)
	ctx := withDomain(t.Context(), "catalog")
	for _, tc := range []struct{ tool, args, message string }{
		{"find_products", `{"value":"red"}`, ""},
		{"find_products", `{"feature":"color","value":"red","min":"1"}`, ""},
		{"find_products", `{"domain":"other","feature":"color","value":"red"}`, "domain outside the request scope"},
		{"get_product", `{"store":"example.test"}`, ""},
		{"list_features", `{"query":false}`, ""},
	} {
		_, err := s.execTool(ctx, tc.tool, json.RawMessage(tc.args))
		var invalid argerr.Error
		if !errors.As(err, &invalid) || (tc.message != "" && err.Error() != tc.message) {
			t.Errorf("%s %s = %v, want argument error %q", tc.tool, tc.args, err, tc.message)
		}
	}
}

type productMetadataStore struct{ graphStore }

func (productMetadataStore) GetProduct(context.Context, graph.ProductLookup) (graph.ProductDetail, error) {
	large := strings.Repeat("<", 4<<10)
	return graph.ProductDetail{Product: graph.Product{Title: large, Brand: large}, Features: []graph.ProductEvidence{}, Pages: []graph.ProductPage{}, PagesMissing: true}, nil
}

func (productMetadataStore) ListProductFeatures(context.Context) (graph.ProductFeatures, error) {
	id := strings.Repeat("r", toolResultCap)
	return graph.ProductFeatures{ActiveReleaseID: &id, Features: []graph.ProductFeatureListing{}}, nil
}

func TestProductToolMetadataOverflow(t *testing.T) {
	s := NewServer(config.Config{}, nil, nil)
	s.store = productMetadataStore{}
	for _, tool := range []string{"get_product", "list_features"} {
		out, err := s.execTool(t.Context(), tool, json.RawMessage(`{"id":"product"}`))
		wire, encErr := json.Marshal(toolPayload(out, err))
		if err == nil || encErr != nil || len(wire) > agentResultBudget || !json.Valid([]byte(transcriptResult(wire))) {
			t.Errorf("%s returned an unbounded result: tool error=%v, marshal error=%v, bytes=%d", tool, err, encErr, len(wire))
		}
	}
}

// The agent clamp must stay within the graph's accepted range, or every clamped
// find_products call fails validation.
func TestProductSearchLimitsWithinGraphRange(t *testing.T) {
	if productSearchDefaultLimit < 1 || productSearchDefaultLimit > productSearchMaxLimit {
		t.Fatalf("agent default limit %d outside 1..%d", productSearchDefaultLimit, productSearchMaxLimit)
	}
	if productSearchMaxLimit > graph.MaxProductSearchLimit {
		t.Fatalf("agent max limit %d exceeds graph max %d", productSearchMaxLimit, graph.MaxProductSearchLimit)
	}
}
