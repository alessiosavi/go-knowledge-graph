package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

type productSearchToolStore struct {
	graphStore
	entities         []graph.EntitySearchRow
	vectorEntities   []graph.EntitySearchRow
	chunks           []graph.ChunkSearchRow
	vectorIDs        []string
	entityLag        time.Duration
	products         func(context.Context, string, string, int) ([]graph.Product, error)
	texts            func(context.Context, []float64, string, int, []string) (graph.ProductTextSearchResult, error)
	textSearchCalled bool
	chunkRead        int
}

func (s *productSearchToolStore) EntityTextSearchIn(_ context.Context, _, _ string, limit int) ([]graph.EntitySearchRow, error) {
	time.Sleep(s.entityLag)
	return s.entities[:min(limit, len(s.entities))], nil
}

func (s *productSearchToolStore) ChunkTextSearchInWindow(_ context.Context, _, _ string, limit int, _ graph.DateWindow) ([]graph.ChunkSearchRow, error) {
	s.chunkRead++
	return s.chunks[:min(limit, len(s.chunks))], nil
}

func (s *productSearchToolStore) EntityVectorSearch(context.Context, []float64, int) ([]string, error) {
	return s.vectorIDs, nil
}

func (s *productSearchToolStore) EntitySearchRowsByID(_ context.Context, ids []string) ([]graph.EntitySearchRow, error) {
	var rows []graph.EntitySearchRow
	for _, entity := range slices.Concat(s.entities, s.vectorEntities) {
		if slices.Contains(ids, entity.ID) {
			rows = append(rows, entity)
		}
	}
	return rows, nil
}

func (s *productSearchToolStore) SearchProductLinks(ctx context.Context, query, domain string, limit int) ([]graph.Product, error) {
	return s.products(ctx, query, domain, limit)
}

func (s *productSearchToolStore) SearchProductTexts(ctx context.Context, vec []float64, domain string, limit int, exclude []string) (graph.ProductTextSearchResult, error) {
	s.textSearchCalled = true
	return s.texts(ctx, vec, domain, limit, exclude)
}

type productSearchTestEmbedder struct{}

func (productSearchTestEmbedder) EmbedQuery(context.Context, string) ([]float64, error) {
	return []float64{1}, nil
}

func TestAgentSearchProductPlacement(t *testing.T) {
	for _, tc := range []struct {
		name, kind       string
		limit            int
		passages         bool
		wantProductLimit int
		wantTotalHits    int
		wantKinds        []string
	}{
		{"limit one", "auto", 1, true, 0, 1, []string{"entity"}},
		{"limit two with fused entities", "auto", 2, true, 1, 2, []string{"product", "chunk"}},
		{"limit twenty", "auto", 20, true, 3, 20, []string{"product", "product", "product", "entity"}},
		{"limit twenty no passages", "auto", 20, false, 3, 20, []string{"product", "product", "product", "entity"}},
		{"products only", "product", 20, true, 20, 5, []string{"product", "product", "product", "product", "product"}},
		{"entity only", "entity", 20, true, 0, 20, []string{"entity"}},
		{"chunk only", "chunk", 20, true, 0, 1, []string{"chunk"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &productSearchToolStore{vectorIDs: []string{"entity-00", "entity-01"}}
			for i := range 20 {
				st.entities = append(st.entities, graph.EntitySearchRow{ID: fmt.Sprintf("entity-%02d", i), Name: "Implant", Domain: "dental"})
			}
			if tc.passages {
				st.chunks = []graph.ChunkSearchRow{{ID: "passage", Text: "The implant kit includes a drill."}}
			}
			calls := make(chan int, 1)
			st.products = func(_ context.Context, query, domain string, limit int) ([]graph.Product, error) {
				calls <- limit
				if domain != "dental" || query != "implant kit" {
					return nil, fmt.Errorf("unexpected product query %q in domain %q", query, domain)
				}
				products := make([]graph.Product, 5)
				for i := range products {
					products[i] = graph.Product{ID: fmt.Sprintf("product-%d", i), Store: "example.com",
						ProductID: fmt.Sprintf("example.com:%d", i), Title: "Implant Kit", Brand: "Acme"}
				}
				return products, nil
			}
			s := &Server{store: st, emb: productSearchTestEmbedder{}}
			args := fmt.Sprintf(`{"query":"implant kit","kind":%q,"limit":%d,"since":"2024-01-01","until":"2025-01-01"}`, tc.kind, tc.limit)
			result, err := s.execTool(withDomain(t.Context(), "dental"), "search", json.RawMessage(args))
			if err != nil {
				t.Fatal(err)
			}
			out := unmarshalResult[searchOut](t, result)
			if out.ProductsError != "" || len(out.Hits) != tc.wantTotalHits {
				t.Fatalf("hits=%+v products_error=%q, want %d hits", out.Hits, out.ProductsError, tc.wantTotalHits)
			}
			limit := 0
			select {
			case limit = <-calls:
			default:
			}
			if limit != tc.wantProductLimit {
				t.Errorf("product leg requested %d hits, want %d", limit, tc.wantProductLimit)
			}
			kinds := make([]string, len(out.Hits))
			for i, hit := range out.Hits {
				kinds[i] = hit.Kind
				if hit.Kind == "product" && (hit.Store != "example.com" || hit.ProductID == "" || hit.Brand != "Acme" || hit.ID == "") {
					t.Errorf("incomplete product hit: %+v", hit)
				}
				if tc.kind != "auto" && hit.Kind != tc.kind {
					t.Errorf("kind=%s returned %+v", tc.kind, hit)
				}
			}
			if !slices.Equal(kinds[:len(tc.wantKinds)], tc.wantKinds) ||
				slices.Contains(kinds, "chunk") != (tc.passages && tc.limit > 1 && tc.kind != "entity" && tc.kind != "product") {
				t.Errorf("hit kinds=%v, want prefix %v and passage when appropriate", kinds, tc.wantKinds)
			}
			wantChunkReads := 0
			if tc.kind == "auto" || tc.kind == "chunk" {
				wantChunkReads = 1
			}
			if st.chunkRead != wantChunkReads {
				t.Errorf("chunk reads=%d, want %d", st.chunkRead, wantChunkReads)
			}
		})
	}
}

func TestAgentSearchAutoProductSlots(t *testing.T) {
	t.Setenv("GKG_AGENT_SEARCH_PRODUCTS", "5")
	t.Setenv("GKG_PRODUCT_TEXT_SEARCH", "off")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind         string
		limit        int
		wantProducts int
	}{
		{"auto", 20, 5},
		{"auto", 6, 3},
		{"product", 6, 6},
	} {
		t.Run(fmt.Sprintf("%s/limit_%d", tc.kind, tc.limit), func(t *testing.T) {
			st := &productSearchToolStore{
				chunks: []graph.ChunkSearchRow{{ID: "passage", Text: "The implant kit includes a drill."}},
				products: func(_ context.Context, _, _ string, limit int) ([]graph.Product, error) {
					products := make([]graph.Product, min(limit, 6))
					for i := range products {
						id := fmt.Sprintf("example.com:%d", i)
						products[i] = graph.Product{ID: id, Store: "example.com", ProductID: id, Title: "Implant Kit"}
					}
					return products, nil
				},
			}
			s := NewServer(cfg, nil, nil)
			s.store = st
			for i := range tc.limit {
				st.entities = append(st.entities, graph.EntitySearchRow{ID: fmt.Sprintf("entity-%02d", i), Name: "Implant"})
			}
			args := fmt.Sprintf(`{"query":"implant kit","kind":%q,"limit":%d}`, tc.kind, tc.limit)
			result, err := s.execTool(t.Context(), "search", json.RawMessage(args))
			if err != nil {
				t.Fatal(err)
			}
			out := unmarshalResult[searchOut](t, result)
			if out.ProductsError != "" || len(out.Hits) != tc.limit {
				t.Fatalf("got %d hits, products_error=%q, want %d hits", len(out.Hits), out.ProductsError, tc.limit)
			}
			products, passages := 0, 0
			for _, hit := range out.Hits {
				switch hit.Kind {
				case "product":
					products++
				case "chunk":
					passages++
				}
			}
			if products != tc.wantProducts {
				t.Errorf("got %d products, want %d", products, tc.wantProducts)
			}
			if tc.kind == "auto" && passages != 1 {
				t.Errorf("got %d passages, want 1", passages)
			}
		})
	}
}

func TestAgentProductSearchFailureKeepsRegularHits(t *testing.T) {
	for _, tc := range []struct {
		name, wantStatus string
		search           func(context.Context) error
	}{
		{"failed", "index_unavailable", func(context.Context) error { return errors.New("test failure") }},
		{"capped", "index_too_large", func(context.Context) error { return graph.ErrProductLinkSearchTooLarge }},
		{"timeout", "timeout", func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &productSearchToolStore{chunks: []graph.ChunkSearchRow{{ID: "passage", Text: "Implant kit passage."}}}
			st.products = func(ctx context.Context, _, _ string, _ int) ([]graph.Product, error) {
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 200*time.Millisecond {
					t.Error("product request missing its 200 ms deadline")
				}
				return nil, tc.search(ctx)
			}
			s := &Server{store: st}
			result, err := s.execTool(t.Context(), "search", json.RawMessage(`{"query":"implant kit"}`))
			if err != nil {
				t.Fatal(err)
			}
			out := unmarshalResult[searchOut](t, result)
			if out.ProductsError != tc.wantStatus || len(out.Hits) != 1 || out.Hits[0].ID != "passage" {
				t.Fatalf("out=%+v, want the passage and error %q", out, tc.wantStatus)
			}
		})
	}
}

func TestAgentProductSearchFitsModelPayload(t *testing.T) {
	title := strings.Repeat("<&\u2028界", 1500)
	brand := strings.Repeat("界\u2028<&", 100)
	s := NewServer(config.Config{Agent: config.AgentConfig{
		BaseURL: "https://no-network.invalid", APIKey: "test", Model: "stub",
	}}, nil, nil)
	st := &productSearchToolStore{
		chunks: []graph.ChunkSearchRow{{ID: "passage", DocID: "doc", Title: title, Text: "Searchable implant kit passage."}},
		products: func(context.Context, string, string, int) ([]graph.Product, error) {
			return []graph.Product{{ID: "best", Title: title, Brand: brand, Store: "example.com",
				ProductID: "example.com:" + strings.Repeat("\u2029", 250)}}, nil
		},
	}
	for i := range searchMaxLimit {
		st.entities = append(st.entities, graph.EntitySearchRow{ID: fmt.Sprintf("entity-%03d", i), Name: title})
	}
	s.store = st
	replies := []stubResponse{
		toolCallReply("c1", "search", `{"query":"implant kit","limit":100}`),
		contentReply("Answer from both sources."),
	}
	var visible string
	requests := 0
	s.llm.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var request struct{ Messages []chatMessage }
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		for _, msg := range request.Messages {
			if msg.Role == "tool" {
				visible = msg.Content
			}
		}
		reply := replies[requests]
		requests++
		return &http.Response{StatusCode: reply.status, Body: io.NopCloser(strings.NewReader(reply.body))}, nil
	})
	resp, err := s.ask(t.Context(), "What is in the implant kit?")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(resp.Evidence) != 1 || visible != string(resp.Evidence[0].Result) || len(visible) > toolResultCap {
		t.Fatalf("requests=%d evidence=%d model bytes=%d, want an intact tool result in the model transcript", requests, len(resp.Evidence), len(visible))
	}
	out := unmarshalResult[searchOut](t, json.RawMessage(visible))
	if len(out.Hits) < 2 || out.Hits[0].ID != "best" || out.Hits[0].Kind != "product" {
		t.Fatalf("want the leading product and some regular hits, got %d hits", len(out.Hits))
	}
	if out.HitsOmitted <= 0 || len(out.Hits)+out.HitsOmitted != searchMaxLimit {
		t.Errorf("kept %d hits and reported %d omitted, want %d total", len(out.Hits), out.HitsOmitted, searchMaxLimit)
	}
	wire := unmarshalResult[struct {
		Hits []map[string]json.RawMessage `json:"hits"`
	}](t, json.RawMessage(visible))
	for _, key := range []string{"score", "snippet"} {
		if _, ok := wire.Hits[0][key]; ok {
			t.Errorf("product hit includes %s", key)
		}
		if _, ok := wire.Hits[1][key]; !ok {
			t.Errorf("passage hit is missing %s", key)
		}
	}
	product := out.Hits[0]
	if utf8.RuneCountInString(product.Title) != 200 || utf8.RuneCountInString(product.Brand) != 80 {
		t.Fatalf("product title/brand lengths = %d/%d, want 200/80 runes",
			utf8.RuneCountInString(product.Title), utf8.RuneCountInString(product.Brand))
	}
	passage := out.Hits[1]
	if passage.Kind != "chunk" || passage.ID != "passage" || passage.Snippet == nil || !strings.Contains(*passage.Snippet, "kit passage") {
		t.Errorf("missing the passage text in the model-visible result")
	}
}

func TestAgentProductOnlySearchReportsOmittedHits(t *testing.T) {
	st := &productSearchToolStore{
		products: func(_ context.Context, _, _ string, limit int) ([]graph.Product, error) {
			products := make([]graph.Product, limit)
			for i := range products {
				products[i] = graph.Product{ID: fmt.Sprintf("product-%03d", i), Store: "example.com",
					ProductID: fmt.Sprintf("example.com:%d", i), Title: "Implant Kit " + strings.Repeat("<", 188)}
			}
			return products, nil
		},
	}
	s := &Server{store: st}
	result, err := s.execTool(t.Context(), "search", json.RawMessage(`{"query":"implant kit","kind":"product","limit":100}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	out := unmarshalResult[searchOut](t, json.RawMessage(raw))
	if len(raw) > toolResultCap || len(out.Hits) == 0 || out.HitsOmitted <= 0 || len(out.Hits)+out.HitsOmitted != searchMaxLimit {
		t.Errorf("result has %d bytes, %d hits and %d omitted, want a capped result accounting for all %d hits",
			len(raw), len(out.Hits), out.HitsOmitted, searchMaxLimit)
	}
}

func TestCompactSearchProductShortensEscapedTitle(t *testing.T) {
	want := searchHitOut{Kind: "product", ID: "best", Title: "<&"}
	budget, err := searchHitSize(want)
	if err != nil {
		t.Fatal(err)
	}
	hit := searchHitOut{Kind: "product", ID: "best", Title: "<&\u2028界"}
	got, size, err := compactSearchProduct(hit, budget)
	if err != nil || got != want || size != budget {
		t.Fatalf("hit=%+v size=%d err=%v, want %+v at %d bytes", got, size, err, want, budget)
	}
}

func TestAgentProductSearchKeepsPassageWithOversizedProduct(t *testing.T) {
	st := &productSearchToolStore{
		chunks: []graph.ChunkSearchRow{{ID: "passage", Text: "Implant kit passage."}},
		products: func(context.Context, string, string, int) ([]graph.Product, error) {
			return []graph.Product{{ID: "best", Domain: "dental", Store: strings.Repeat("s", toolResultCap),
				ProductID: strings.Repeat("p", toolResultCap), Title: "Implant Kit", Brand: "Acme"}}, nil
		},
	}
	s := &Server{store: st}
	result, err := s.execTool(t.Context(), "search", json.RawMessage(`{"query":"implant kit","limit":2}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	out := unmarshalResult[struct {
		Hits        []map[string]json.RawMessage `json:"hits"`
		HitsOmitted *int                         `json:"hits_omitted"`
	}](t, json.RawMessage(raw))
	if len(raw) > toolResultCap || len(out.Hits) != 2 || out.HitsOmitted != nil {
		t.Fatalf("result has %d bytes, %d hits and hits_omitted=%v, want both hits within budget", len(raw), len(out.Hits), out.HitsOmitted)
	}
	product := out.Hits[0]
	if len(product) != 3 || string(product["kind"]) != `"product"` || string(product["id"]) != `"best"` || string(product["title"]) != `"Implant Kit"` {
		t.Errorf("want only the product kind, id and title, got %s", raw)
	}
	if passage := out.Hits[1]; string(passage["id"]) != `"passage"` || string(passage["snippet"]) != `"Implant kit passage."` {
		t.Errorf("want the passage and its text, got %v", passage)
	}
}
