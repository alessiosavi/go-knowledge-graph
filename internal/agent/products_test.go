package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
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
		{"get_product", map[string]string{"id": "string", "store": "string", "product_id": "string", "domain": "string"}, nil},
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
		{"get_product", `{"id":"product","domain":"other"}`, "domain outside the request scope"},
		{"list_features", `{"query":false}`, ""},
	} {
		_, err := s.execTool(ctx, tc.tool, json.RawMessage(tc.args))
		var invalid argerr.Error
		if !errors.As(err, &invalid) || (tc.message != "" && err.Error() != tc.message) {
			t.Errorf("%s %s = %v, want argument error %q", tc.tool, tc.args, err, tc.message)
		}
	}
}

func TestGetProductUsesAgentRequestScope(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	drv, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(ctx)
	session := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)
	write := func(query string) error {
		rows, err := session.Run(ctx, query, nil)
		if err == nil {
			_, err = rows.Consume(ctx)
		}
		return err
	}
	if err := write(`
CREATE (:Product {id:'agent-ref-product', domain:'agent-catalog'}),
       (a:ProductRef {id:'agent-ref-product'}),
       (:Document {id:'agent-local-page', domain:'agent-catalog', status:'done', source:'local', source_kind:'text'})-[:REFS]->(a),
       (:Document {id:'agent-remote-page', domain:'agent-other', status:'done', source:'remote', source_kind:'text'})-[:REFS]->(a)`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := write(`MATCH (n) WHERE n.id IN ['agent-ref-product','agent-local-page','agent-remote-page'] DETACH DELETE n`); err != nil {
			t.Error(err)
		}
	}()
	s := NewServer(config.Config{}, st, nil)
	const args = `{"id":"agent-ref-product"}`
	for _, tc := range []struct {
		scope, args string
		pages       []string
	}{
		{"", args, []string{"agent-local-page", "agent-remote-page"}},
		{"agent-catalog", args, []string{"agent-local-page"}},
		{"", `{"id":"agent-ref-product","domain":"agent-other"}`, []string{"agent-remote-page"}},
	} {
		res, err := s.execTool(withDomain(ctx, tc.scope), "get_product", json.RawMessage(tc.args))
		if err != nil {
			t.Errorf("scope %q, args %s: %v", tc.scope, tc.args, err)
			continue
		}
		out := unmarshalResult[productDetailOut](t, res)
		var pages []string
		for _, page := range out.Pages {
			pages = append(pages, page.ID)
		}
		if !slices.Equal(pages, tc.pages) || out.PagesMissing {
			t.Errorf("scope %q, args %s: pages %v, pages_missing %v, want %v", tc.scope, tc.args, pages, out.PagesMissing, tc.pages)
		}
	}
	if _, err := s.execTool(withDomain(ctx, "agent-other"), "get_product", json.RawMessage(args)); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("out-of-scope seed: %v, want not found", err)
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
