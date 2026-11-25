package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
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
		{"get_product", map[string]string{"id": "string", "store": "string", "product_id": "string", "domain": "string", "with_mentions": "boolean", "mentions_limit": "integer", "with_texts": "boolean", "text_keys": "array"}, nil},
		{"find_products", map[string]string{"domain": "string", "feature": "string", "value": "string", "min": "string", "max": "string", "limit": "integer", "cursor": "string", "query": "[string null]"}, nil},
		{"list_features", map[string]string{"query": "string"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, ok := defs[tc.name]
			if !ok {
				t.Fatal("tool is missing")
			}
			schema := unmarshalResult[struct {
				Type       string
				Properties map[string]struct{ Type any }
				Required   []string
			}](t, def.Function.Parameters)
			if def.Type != "function" || schema.Type != "object" || !slices.Equal(schema.Required, tc.required) {
				t.Fatalf("unexpected tool schema: %+v", def)
			}
			got := make(map[string]string)
			for name, prop := range schema.Properties {
				got[name] = fmt.Sprint(prop.Type)
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
		if with["default"] != true || with["description"] != "Include passages with exact identifier matches (default true)" ||
			limit["default"] != float64(5) || limit["minimum"] != float64(1) || limit["maximum"] != float64(20) {
			t.Errorf("unexpected mention options: with_mentions=%v mentions_limit=%v", with, limit)
		}
		for _, phrase := range []string{"with_mentions defaults to true", "done documents", "exact", "catalogue_code", "product_reference", "mentions_partial", "TC 2SL", "gkg products mentions-scan", "pages_omitted_for_budget"} {
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
	for _, tc := range []struct{ args, message string }{
		{`{"id":"p","with_mentions":"true"}`, "with_mentions must be a boolean"},
		{`{"id":"p","with_mentions":1}`, "with_mentions must be a boolean"},
		{`{"id":"p","with_mentions":null}`, "with_mentions must be a boolean"},
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
		if !errors.As(err, &invalid) || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("get_product %s: got %v, want argument error containing %q", tc.args, err, tc.message)
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
		{"", `{"id":"agent-mention-product"}`, ids},
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
	// The escaped title and brand fit with either the page or the mention.
	// The required result overflows when it retains both.
	large := strings.Repeat("<", 1600)
	return graph.ProductDetail{
		Product:  graph.Product{ID: "product", Title: large, Brand: large},
		Features: []graph.ProductEvidence{{Raw: "trimmable"}},
		Pages:    []graph.ProductPage{{ID: "page", Title: large, ChunkIDs: []string{"chunk"}, ChunkCount: 1}},
	}, nil
}

func (s productMetadataStore) GetProductWithMentions(ctx context.Context, lookup graph.ProductLookup) (graph.ProductDetailWithMentions, graph.ProductMentionRead, error) {
	detail, err := s.GetProduct(ctx, lookup)
	page := detail.Pages[0]
	return graph.ProductDetailWithMentions{
		ProductDetail: detail,
		ProductMentionFields: &graph.ProductMentionFields{
			Mentions:      []graph.ProductChunkMention{{DocID: page.ID, DocTitle: page.Title, ChunkID: page.ChunkIDs[0]}},
			MentionsTotal: 1,
			MentionsKeys:  1,
		},
	}, graph.ProductMentionRead{}, err
}

func (productMetadataStore) ListProductFeatures(context.Context) (graph.ProductFeatures, error) {
	id := strings.Repeat("r", toolResultCap)
	return graph.ProductFeatures{ActiveReleaseID: &id, Features: []graph.ProductFeatureListing{}}, nil
}

func TestProductToolMetadataOverflow(t *testing.T) {
	store := productMetadataStore{}
	wire := getProductWire(t, store, `{"id":"product","with_mentions":false}`)
	if len(wire) > agentResultBudget {
		t.Fatalf("product without mentions exceeds the result budget: %d bytes", len(wire))
	}
	s := NewServer(config.Config{}, nil, nil)
	s.store = store
	for _, tc := range []struct{ tool, args string }{
		{"get_product", `{"id":"product"}`},
		{"list_features", `{}`},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			out, err := s.execTool(t.Context(), tc.tool, json.RawMessage(tc.args))
			const want = "product tool metadata exceeds the agent result budget"
			if err == nil || err.Error() != want {
				t.Fatalf("tool error = %v, want %q", err, want)
			}
			wire, err := json.Marshal(toolPayload(out, err))
			if err != nil || len(wire) > agentResultBudget || !json.Valid([]byte(transcriptResult(wire))) {
				t.Errorf("tool error payload is unbounded or invalid: marshal error=%v, bytes=%d", err, len(wire))
			}
		})
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
	const args = `{"id":"product","with_mentions":false,"mentions_limit":1}`
	if got := string(getProductWire(t, store, args)); got != before {
		t.Errorf("%s: got %s, want %s", args, got, before)
	}
}

func TestGetProductMentionsDisplayLimit(t *testing.T) {
	for _, tc := range []struct {
		args     string
		returned int
		matches  int
	}{
		{`{"id":"p"}`, 5, 22},
		{`{"id":"p","mentions_limit":1}`, 1, 22},
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
			before := getProductWire(t, store, `{"id":"p","with_mentions":false}`)
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
			Pages:    []graph.ProductPage{{ID: "first", ChunkIDs: []string{"c1"}}, {ID: "last"}},
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
	// Measure the encoded result with the first page and mention reserved.
	size := func(features int) int {
		t.Helper()
		fitted := store.detail
		fields := *fitted.ProductMentionFields
		fields.Mentions = fields.Mentions[:1]
		fitted.ProductMentionFields = &fields
		fitted.Pages = fitted.Pages[:1]
		fitted.Features = fitted.Features[:features]
		wire, err := json.Marshal(struct {
			graph.ProductDetailWithMentions
			FeaturesOmitted int `json:"features_omitted,omitempty"`
			MentionsOmitted int `json:"mentions_omitted_for_budget"`
			PagesOmitted    int `json:"pages_omitted_for_budget"`
		}{fitted, 2 - features, 1, 1})
		if err != nil {
			t.Fatal(err)
		}
		return len(wire)
	}
	for _, tc := range []struct {
		name         string
		titleBytes   int
		features     int
		wantBoundary bool
	}{
		{"at encoded budget", agentResultBudget - size(2), 2, true},
		{"one byte over", agentResultBudget - size(2) + 1, 1, false},
		{"all features trimmed", agentResultBudget - size(1) + 1, 0, false},
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
				PagesOmitted    int `json:"pages_omitted_for_budget"`
			}](t, json.RawMessage(wire))
			if out.ProductMentionFields == nil || len(out.Mentions) != 1 || out.MentionsOmitted != 1 || out.MentionsTotal != 7 || out.MentionsKeys != 1 {
				t.Fatalf("want one mention plus original counts: fields=%+v omitted=%d", out.ProductMentionFields, out.MentionsOmitted)
			}
			if out.Mentions[0].ChunkID != "first" || out.Mentions[0].Excerpt != store.detail.Mentions[0].Excerpt {
				t.Errorf("kept the wrong mention: %+v", out.Mentions)
			}
			if len(out.Pages) != 1 || out.PagesOmitted != 1 || out.PagesMissing || out.Pages[0].ID != "first" || !slices.Equal(out.Pages[0].ChunkIDs, []string{"c1"}) {
				t.Errorf("want the first page and its chunk ID: pages=%+v omitted=%d missing=%t", out.Pages, out.PagesOmitted, out.PagesMissing)
			}
			if !slices.EqualFunc(out.Features, store.detail.Features[:tc.features], func(got, want graph.ProductEvidence) bool { return got.Raw == want.Raw }) || out.FeaturesOmitted != 2-tc.features {
				t.Errorf("unexpected feature trimming: features=%+v omitted=%d", out.Features, out.FeaturesOmitted)
			}
		})
	}
}

func TestFindProductsQueryToolArguments(t *testing.T) {
	s := NewServer(config.Config{}, &graph.Store{}, nil)
	for field, value := range map[string]any{
		"feature": "code", "value": "ceramic", "min": "1", "max": "5", "cursor": "previous",
		"limit": 21, "domain": "Bad!", "query": strings.Repeat("é", 501),
	} {
		args := map[string]any{"query": "ceramic"}
		args[field] = value
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.execTool(t.Context(), "find_products", raw)
		var invalid argerr.Error
		if !errors.As(err, &invalid) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: got %v, want an argument error naming the field", field, err)
		}
	}
	for _, limit := range []string{"0", "-1", "1.5", "1e1", `"10"`} {
		_, err := s.execTool(t.Context(), "find_products", json.RawMessage(`{"query":"ceramic","limit":`+limit+`}`))
		var invalid argerr.Error
		if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "limit") {
			t.Errorf("limit %s: got %v, want an argument error naming limit", limit, err)
		}
	}
	ctx := withDomain(t.Context(), "t3-scope")
	if _, err := s.execTool(ctx, "find_products", json.RawMessage(`{"query":"ceramic","domain":"other"}`)); err == nil ||
		!strings.Contains(err.Error(), "domain outside the request scope") {
		t.Fatalf("scope override: %v", err)
	}
	out, err := s.execTool(ctx, "find_products", json.RawMessage(`{"query":"ceramic","feature":"","value":"","cursor":null}`))
	raw, _ := json.Marshal(out)
	if err != nil || !strings.Contains(string(raw), `"meaning_status":"disabled"`) {
		t.Fatalf("disabled response = %s, %v", raw, err)
	}
}

func TestFindProductsQueryEncodedBudget(t *testing.T) {
	out := search.ProductQueryResult{Mode: "query", MeaningStatus: "ok", StoresNotMaterialized: []string{}}
	const key = "material"
	text := strings.Repeat("<", 200)
	for i := range 20 {
		out.Hits = append(out.Hits, graph.ProductTextHit{
			ID: fmt.Sprintf("t3-%02d", i), ProductID: strings.Repeat("&", 140), Title: strings.Repeat("&", 4000),
			Matched: []graph.MatchedProductText{
				{ProductText: graph.ProductText{Key: key, Text: text, Checked: true}},
				{ProductText: graph.ProductText{Key: "color", Text: "orange"}},
			},
		})
	}
	fitted, err := fitProductQueryResult(out)
	wire, marshalErr := json.Marshal(fitted)
	if err != nil || marshalErr != nil || len(wire) > agentResultBudget || len(fitted.Hits) == 0 ||
		len(fitted.Hits) >= len(out.Hits) {
		t.Fatalf("budget: hits=%d bytes=%d fit=%v marshal=%v", len(fitted.Hits), len(wire), err, marshalErr)
	}
	for i, hit := range fitted.Hits {
		if hit.ID != out.Hits[i].ID || hit.ProductID != out.Hits[i].ProductID || len(hit.Matched) != 1 {
			t.Fatalf("hit %d lost its identity or match", i)
		}
		entry := hit.Matched[0]
		if entry.Key != key || !entry.Checked || entry.Text != text[:160] || !entry.ExcerptCut {
			t.Errorf("match %d: %+v", i, entry)
		}
	}
	if len(out.Hits[0].Matched) != 2 || out.Hits[0].Matched[0].Text != text {
		t.Fatal("fitting mutated the original result")
	}
}

func TestGetProductTextsAgent(t *testing.T) {
	st := graphtest.ProductTextReads(t)
	cfg := config.Config{ProductText: config.ProductTextConfig{Search: true}}
	s := NewServer(cfg, st, graphtest.ProductTextEmbedder{})
	ctx := withDomain(t.Context(), graphtest.TextReadDomain)
	for _, tc := range []struct {
		id, args       string
		texts, omitted int
	}{
		{graphtest.TextReadManyID, "", 0, 0},
		{graphtest.TextReadManyID, `,"text_keys":["key-1","key-2","key-3","key-4","key-5","key-6","key-7","key-8","key-9","missing"]`, 8, 1},
		{graphtest.TextReadLargeID, `,"text_keys":["key-1","key-2","key-3","key-4"]`, 2, 2},
	} {
		args := fmt.Sprintf(`{"id":%q,"with_texts":true,"with_mentions":false%s}`, tc.id, tc.args)
		got, err := s.execTool(ctx, "get_product", json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		out := unmarshalResult[productDetailOut](t, got)
		wire, err := json.Marshal(got)
		if err != nil || len(wire) > agentResultBudget || out.ProductTextFields == nil || out.Texts == nil ||
			len(out.Texts) != tc.texts || out.TextsOmitted != tc.omitted {
			t.Fatalf("%s: got %d texts, %d omitted, %d bytes, marshal error %v", args, len(out.Texts), out.TextsOmitted, len(wire), err)
		}
	}
	raw := json.RawMessage(`{"query":"ceramic"}`)
	got, err := s.execTool(ctx, "find_products", raw)
	if err != nil {
		t.Fatal(err)
	}
	out := unmarshalResult[search.ProductQueryResult](t, got)
	if out.MeaningStatus != "partial" || len(out.Hits) != 1 || out.Hits[0].ID != graphtest.TextReadManyID ||
		len(out.Hits[0].Matched) != 1 || len([]rune(out.Hits[0].Matched[0].Text)) != 160 {
		t.Fatalf("query in scope = %+v", out)
	}
	got, err = s.execTool(withDomain(t.Context(), "t3-other"), "find_products", raw)
	if err != nil || len(unmarshalResult[search.ProductQueryResult](t, got).Hits) != 0 {
		t.Fatalf("query in unrelated scope = %+v (%v)", got, err)
	}
}
