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
	entities  []graph.EntitySearchRow
	chunks    []graph.ChunkSearchRow
	vectorIDs []string
	products  func(context.Context, string, string, int) ([]graph.Product, error)
	chunkRead int
}

func (s *productSearchToolStore) EntityTextSearchIn(_ context.Context, _, _ string, limit int) ([]graph.EntitySearchRow, error) {
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
	for _, entity := range s.entities {
		if slices.Contains(ids, entity.ID) {
			rows = append(rows, entity)
		}
	}
	return rows, nil
}

func (s *productSearchToolStore) SearchProductLinks(ctx context.Context, query, domain string, limit int) ([]graph.Product, error) {
	return s.products(ctx, query, domain, limit)
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
	product := out.Hits[0]
	if utf8.RuneCountInString(product.Title) != 200 || utf8.RuneCountInString(product.Brand) != 80 {
		t.Fatalf("product title/brand lengths = %d/%d, want 200/80 runes",
			utf8.RuneCountInString(product.Title), utf8.RuneCountInString(product.Brand))
	}
	if out.Hits[1].Kind != "chunk" || out.Hits[1].ID != "passage" || !strings.Contains(out.Hits[1].Snippet, "kit passage") {
		t.Errorf("missing the passage text in the model-visible result")
	}
}
