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
		{"get_product", map[string]string{"id": "string", "store": "string", "product_id": "string", "domain": "string", "with_mentions": "boolean", "mentions_limit": "integer"}, nil},
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

func TestGetProductMentionsSchema(t *testing.T) {
	for _, def := range toolDefs() {
		if def.Function.Name != "get_product" {
			continue
		}
		schema := unmarshalResult[struct{ Properties map[string]map[string]any }](t, def.Function.Parameters)
		with, limit := schema.Properties["with_mentions"], schema.Properties["mentions_limit"]
		if with["default"] != false || limit["default"] != float64(5) || limit["minimum"] != float64(1) || limit["maximum"] != float64(20) {
			t.Errorf("unexpected mention options: with_mentions=%v mentions_limit=%v", with, limit)
		}
		for _, phrase := range []string{"done documents", "exact", "catalogue_code", "product_reference", "mentions_partial", "TC 2SL", "gkg products mentions-scan"} {
			if !strings.Contains(def.Function.Description, phrase) {
				t.Errorf("get_product description missing %q", phrase)
			}
		}
		return
	}
	t.Fatal("get_product tool missing")
}

func TestGetProductMentionsArgumentErrors(t *testing.T) {
	s := NewServer(config.Config{}, nil, nil)
	s.store = agentMentionStore{}
	for _, tc := range []struct{ args, field string }{
		{`{"id":"p","with_mentions":"true"}`, "with_mentions"},
		{`{"id":"p","with_mentions":1}`, "with_mentions"},
		{`{"id":"p","with_mentions":null}`, "with_mentions"},
		{`{"id":"p","mentions_limit":0}`, "mentions_limit"},
		{`{"id":"p","with_mentions":false,"mentions_limit":21}`, "mentions_limit"},
		{`{"id":"p","with_mentions":true,"mentions_limit":-1}`, "mentions_limit"},
		{`{"id":"p","mentions_limit":2.0}`, "mentions_limit"},
		{`{"id":"p","mentions_limit":1e1}`, "mentions_limit"},
		{`{"id":"p","mentions_limit":"5"}`, "mentions_limit"},
		{`{"id":"p","mentions_limit":null}`, "mentions_limit"},
	} {
		_, err := s.execTool(t.Context(), "get_product", json.RawMessage(tc.args))
		var invalid argerr.Error
		if !errors.As(err, &invalid) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("get_product %s: got %v, want %s argument error", tc.args, err, tc.field)
		}
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

func TestGetProductRejectsInvalidDomain(t *testing.T) {
	s := NewServer(config.Config{}, &graph.Store{}, nil)
	_, err := s.execTool(t.Context(), "get_product", json.RawMessage(`{"id":"product","domain":"Bad!"}`))
	var invalid argerr.Error
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "invalid domain slug") {
		t.Errorf("get_product error = %v, want invalid domain argument error", err)
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

func TestGetProductMentionsUsesAgentRequestScope(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	drv, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close(context.Background()) })
	session := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	write := func(ctx context.Context, query string) {
		t.Helper()
		rows, err := session.Run(ctx, query, nil)
		if err == nil {
			_, err = rows.Consume(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	write(ctx, `
MERGE (m:GkgMeta {id:'product_release'}) ON CREATE SET m.value='agent-mention-release'
CREATE (:ProductStore {store:'agent-mention.test', domain:'agent-catalog'}),
       (:Product {id:'agent-mention-product', store:'agent-mention.test', domain:'agent-catalog'})
       -[:HAS_FEATURE {release_id:m.value, value_fold:'AGENT12345'}]->(:FeatureRev {id:'catalogue_code', key:'agent-mention-key'})`)
	t.Cleanup(func() {
		write(context.Background(), `MATCH (n) WHERE n.id='agent-mention-product' OR n.key='agent-mention-key'
 OR (n:ProductStore AND n.store='agent-mention.test') OR (n:GkgMeta AND n.id='product_release' AND n.value='agent-mention-release')
 DETACH DELETE n`)
	})
	var ids []string
	for _, domain := range []string{"agent-catalog", "agent-other"} {
		doc, err := st.UpsertDocument(ctx, graph.DocumentInput{
			Domain: domain, Source: "agent-mentions/" + domain, Hash: domain, Kind: "text", DocumentDate: storedAgentEvidenceDate,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := st.DeleteDocument(context.Background(), doc.ID); err != nil {
				t.Error(err)
			}
		})
		if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: "Catalogue lists AGENT12345."}}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetStatus(ctx, doc.ID, graph.StatusDone, ""); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, doc.ID)
	}
	s := NewServer(config.Config{}, st, nil)
	const args = `{"id":"agent-mention-product","with_mentions":true}`
	for _, tc := range []struct {
		scope, args string
		want        []string
	}{
		{"", args, ids},
		{"agent-catalog", args, ids[:1]},
		{"", `{"id":"agent-mention-product","with_mentions":true,"domain":"agent-other"}`, ids[1:]},
	} {
		res, err := s.execTool(withDomain(ctx, tc.scope), "get_product", json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("scope %q, args %s: %v", tc.scope, tc.args, err)
		}
		out := unmarshalResult[graph.ProductDetailWithMentions](t, res)
		if out.ProductMentionFields == nil || out.MentionsTotal != len(tc.want) || out.MentionsKeys != 1 {
			t.Errorf("scope %q, args %s: fields=%+v error=%s", tc.scope, tc.args, out.ProductMentionFields, out.MentionsError)
			continue
		}
		var got []string
		for _, mention := range out.Mentions {
			got = append(got, mention.DocID)
			assertAgentEvidenceDate(t, mention, true)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("scope %q, args %s: documents=%v want %v", tc.scope, tc.args, got, tc.want)
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

type agentMentionStore struct {
	graphStore
	detail graph.ProductDetailWithMentions
}

func (s agentMentionStore) GetProduct(context.Context, graph.ProductLookup) (graph.ProductDetail, error) {
	return s.detail.ProductDetail, nil
}

func (s agentMentionStore) GetProductWithMentions(context.Context, graph.ProductLookup) (graph.ProductDetailWithMentions, graph.ProductMentionRead, error) {
	return s.detail, graph.ProductMentionRead{}, nil
}

func getProductWire(t *testing.T, store graphStore, args string) []byte {
	t.Helper()
	s := NewServer(config.Config{}, nil, nil)
	s.store = store
	out, err := s.execTool(t.Context(), "get_product", json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestGetProductMentionsOffWire(t *testing.T) {
	store := agentMentionStore{detail: graph.ProductDetailWithMentions{
		ProductDetail: graph.ProductDetail{
			Product:      graph.Product{ID: "product", Title: "Product"},
			Features:     []graph.ProductEvidence{},
			Pages:        []graph.ProductPage{},
			PagesMissing: true,
		},
		MentionsError: graph.MentionsIndexUnavailable,
	}}
	const before = `{"id":"product","store":"","product_id":"","domain":"","title":"Product","brand":"","category":"","subcategory":"","link":"","features":[],"pages":[],"pages_missing":true}`
	for _, args := range []string{`{"id":"product"}`, `{"id":"product","with_mentions":false,"mentions_limit":1}`} {
		if got := string(getProductWire(t, store, args)); got != before {
			t.Errorf("%s: got %s, want %s", args, got, before)
		}
	}
}

func TestGetProductMentionsDisplayLimit(t *testing.T) {
	for _, tc := range []struct {
		args     string
		returned int
		matches  int
	}{
		{`{"id":"p","with_mentions":true}`, 5, 22},
		{`{"id":"p","with_mentions":true,"mentions_limit":1}`, 1, 22},
		{`{"id":"p","with_mentions":true,"mentions_limit":20}`, 20, 22},
		{`{"id":"p","with_mentions":true}`, 0, 0},
	} {
		store := agentMentionStore{detail: graph.ProductDetailWithMentions{ProductMentionFields: &graph.ProductMentionFields{
			Mentions: make([]graph.ProductChunkMention, tc.matches), MentionsTotal: tc.matches, MentionsKeys: 1, MentionsPartial: true,
		}}}
		wire := getProductWire(t, store, tc.args)
		out := unmarshalResult[graph.ProductDetailWithMentions](t, json.RawMessage(wire))
		if out.ProductMentionFields == nil || out.Mentions == nil || len(out.Mentions) != tc.returned || out.MentionsTotal != tc.matches || out.MentionsKeys != 1 || !out.MentionsPartial || out.MentionsError != "" || strings.Contains(string(wire), "mentions_omitted_for_budget") {
			t.Errorf("%s: expected %d of %d matches without budget omissions, got %s", tc.args, tc.returned, tc.matches, wire)
		}
	}
}

func TestGetProductMentionsErrorPassthrough(t *testing.T) {
	for _, code := range []string{graph.MentionsIndexTooLarge, graph.MentionsIndexUnavailable, graph.MentionsSearchUnavailable} {
		t.Run(code, func(t *testing.T) {
			store := agentMentionStore{detail: graph.ProductDetailWithMentions{
				ProductDetail: graph.ProductDetail{
					Product:  graph.Product{ID: "p"},
					Features: []graph.ProductEvidence{{Raw: "ABC12345"}},
					Pages:    []graph.ProductPage{{ID: "page"}},
				},
				MentionsError: code,
			}}
			wire := getProductWire(t, store, `{"id":"p","with_mentions":true}`)
			out := unmarshalResult[map[string]json.RawMessage](t, json.RawMessage(wire))
			if string(out["mentions_error"]) != `"`+code+`"` {
				t.Errorf("mentions_error: got %s, want %s", out["mentions_error"], code)
			}
			delete(out, "mentions_error")
			before := getProductWire(t, store, `{"id":"p"}`)
			if want := unmarshalResult[map[string]json.RawMessage](t, json.RawMessage(before)); !maps.EqualFunc(out, want, slices.Equal) {
				t.Errorf("mentions error changed the ordinary product detail: %s vs %s", wire, before)
			}
		})
	}
}

func TestGetProductMentionsEncodedBudget(t *testing.T) {
	store := agentMentionStore{detail: graph.ProductDetailWithMentions{
		ProductDetail: graph.ProductDetail{
			Features: []graph.ProductEvidence{{Raw: "first"}, {Raw: "last"}},
			Pages:    []graph.ProductPage{{ID: "page"}},
		},
		ProductMentionFields: &graph.ProductMentionFields{
			Mentions: []graph.ProductChunkMention{
				{ChunkID: "first", Excerpt: "界\n\"\\<"},
				{ChunkID: "last", Excerpt: "&"},
			},
			MentionsTotal: 7,
			MentionsKeys:  1,
		},
	}}
	const args = `{"id":"p","with_mentions":true}`
	// Measure all fields and escaping in the exact shapes after dropping mentions.
	size := func(remaining int) int {
		t.Helper()
		fitted := store.detail
		fields := *fitted.ProductMentionFields
		fields.Mentions = fields.Mentions[:remaining]
		fitted.ProductMentionFields = &fields
		wire, err := json.Marshal(struct {
			graph.ProductDetailWithMentions
			Omitted int `json:"mentions_omitted_for_budget"`
		}{fitted, 2 - remaining})
		if err != nil {
			t.Fatal(err)
		}
		return len(wire)
	}
	for _, tc := range []struct {
		name               string
		titleBytes         int
		features, mentions int
		wantBoundary       bool
	}{
		{"at encoded budget", agentResultBudget - size(1), 2, 1, true},
		{"one byte over", agentResultBudget - size(1) + 1, 2, 0, false},
		{"features trimmed after mentions", agentResultBudget - size(0) + 1, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := store
			store.detail.Title = strings.Repeat("x", tc.titleBytes)
			wire := getProductWire(t, store, args)
			if len(wire) > agentResultBudget || (tc.wantBoundary && len(wire) != agentResultBudget) || !json.Valid([]byte(transcriptResult(wire))) {
				t.Errorf("invalid result size %d (budget %d)", len(wire), agentResultBudget)
			}
			out := unmarshalResult[struct {
				graph.ProductDetailWithMentions
				MentionsOmitted int `json:"mentions_omitted_for_budget"`
				FeaturesOmitted int `json:"features_omitted"`
			}](t, json.RawMessage(wire))
			if out.ProductMentionFields == nil || out.Mentions == nil || len(out.Mentions) != tc.mentions || out.MentionsOmitted != 2-tc.mentions || out.MentionsTotal != 7 || out.MentionsKeys != 1 {
				t.Errorf("mention trimming changed the wire contract: fields=%+v omitted=%d", out.ProductMentionFields, out.MentionsOmitted)
			}
			if out.ProductMentionFields != nil && len(out.Mentions) > 0 && out.Mentions[0].ChunkID != "first" {
				t.Errorf("kept the wrong mention: %+v", out.Mentions)
			}
			if len(out.Features) != tc.features || out.FeaturesOmitted != 2-tc.features || len(out.Pages) != 1 || out.Features[0].Raw != "first" {
				t.Errorf("unexpected feature or page trimming: features=%+v omitted=%d pages=%+v", out.Features, out.FeaturesOmitted, out.Pages)
			}
		})
	}
}
