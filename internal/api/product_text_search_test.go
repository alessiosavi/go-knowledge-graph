package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

func TestFindProductsQueryArguments(t *testing.T) {
	s := NewServer(config.Config{}, &graph.Store{}, nil)
	h := s.Handler()
	for _, field := range []string{"feature", "value", "min", "max", "cursor"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/products/search",
			strings.NewReader(`{"query":"ceramic","`+field+`":"1"}`)))
		if r.Code != 400 || !strings.Contains(r.Body.String(), "invalid_request") || !strings.Contains(r.Body.String(), field) {
			t.Errorf("%s conflict: HTTP %d %s", field, r.Code, r.Body)
		}
	}
	for _, tc := range []struct{ args, field string }{
		{`{"query":"ceramic","limit":21}`, "limit"},
		{`{"query":"ceramic","limit":0}`, "limit"},
		{`{"query":"ceramic","limit":-1}`, "limit"},
		{`{"query":"ceramic","domain":"Bad!"}`, "domain"},
		{`{"query":"` + strings.Repeat("é", 501) + `"}`, "query"},
	} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/products/search", strings.NewReader(tc.args)))
		if r.Code != 400 || !strings.Contains(r.Body.String(), "invalid_request") || !strings.Contains(r.Body.String(), tc.field) {
			t.Errorf("%s: HTTP %d %s", tc.field, r.Code, r.Body)
		}
	}
	for _, limit := range []string{"1.5", "1e1", `"10"`} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/products/search",
			strings.NewReader(`{"query":"ceramic","limit":`+limit+`}`)))
		if r.Code != 400 || !strings.Contains(r.Body.String(), "limit") {
			t.Errorf("limit %s: HTTP %d %s", limit, r.Code, r.Body)
		}
	}
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/products/search",
		strings.NewReader(`{"query":"  ceramic  ","feature":"","value":"","min":"","max":"","cursor":null}`)))
	var out map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil || r.Code != 200 || out["meaning_status"] != "disabled" ||
		out["mode"] != "query" || out["cursor"] != nil || string(r.Body.Bytes()) !=
		"{\"mode\":\"query\",\"hits\":[],\"exhausted\":false,\"candidates_truncated\":false,\"meaning_status\":\"disabled\",\"stores_not_materialized\":[]}\n" {
		t.Fatalf("disabled response HTTP %d %s (%v)", r.Code, r.Body, err)
	}
	for _, query := range []string{`null`, `"  "`} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/products/search",
			strings.NewReader(`{"query":`+query+`,"limit":51,"feature":"code","value":"1"}`)))
		if r.Code != 400 || !strings.Contains(r.Body.String(), "50") {
			t.Errorf("expected feature limit for %s: HTTP %d %s", query, r.Code, r.Body)
		}
	}
}

type productTextReadStore struct {
	graphStore
	check func(context.Context) error
	read  func(context.Context, []float64, string, int, []string) (graph.ProductTextSearchResult, error)
}

func (s productTextReadStore) CheckProductTextIndex(ctx context.Context) error { return s.check(ctx) }

func (s productTextReadStore) SearchProductTexts(ctx context.Context, vec []float64, domain string, limit int, excluded []string) (graph.ProductTextSearchResult, error) {
	return s.read(ctx, vec, domain, limit, excluded)
}

type productTextQueryEmbedder struct {
	space string
	embed func(context.Context, string) ([]float64, error)
}

func (e productTextQueryEmbedder) Space() string { return e.space }
func (e productTextQueryEmbedder) EmbedQuery(ctx context.Context, q string) ([]float64, error) {
	return e.embed(ctx, q)
}

func TestFindProductsQueryMeaningStatus(t *testing.T) {
	upstream := errors.New("upstream fault")
	for _, tc := range []struct {
		name, status string
		disabled     bool
		unsupported  bool
		timeout      bool
		indexErr     error
		embedErr     error
		readErr      error
		embedCalls   int
		readCalls    int
	}{
		{name: "disabled precedes unsupported space", status: "disabled", disabled: true, unsupported: true},
		{name: "unsupported space", status: "unavailable", unsupported: true},
		{name: "absent or mismatched index", status: "unavailable", indexErr: graph.ErrProductTextIndexUnavailable},
		{name: "embedding exceeds deadline", status: "timeout", timeout: true, embedCalls: 1},
		{name: "graph transaction deadline", status: "timeout", readErr: graph.ErrQueryTimeout, embedCalls: 1, readCalls: 1},
		{name: "embedder failure", status: "error", embedErr: upstream, embedCalls: 1},
		{name: "index inspection failure", status: "error", indexErr: upstream},
		{name: "vector search failure", status: "error", readErr: upstream, embedCalls: 1, readCalls: 1},
		{name: "ok", status: "ok", embedCalls: 1, readCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{ProductText: config.ProductTextConfig{Search: !tc.disabled, SearchTimeout: 250 * time.Millisecond}}
			if tc.timeout {
				cfg.ProductText.SearchTimeout = 30 * time.Millisecond
			}
			s := NewServer(cfg, nil, nil)
			var reads, embeds int
			store := productTextReadStore{
				check: func(context.Context) error { return tc.indexErr },
				read: func(_ context.Context, _ []float64, domain string, limit int, excluded []string) (graph.ProductTextSearchResult, error) {
					reads++
					if limit != 10 || domain != "t3-test" || len(excluded) != 0 {
						t.Errorf("graph arguments: domain=%q limit=%d excluded=%v", domain, limit, excluded)
					}
					return graph.ProductTextSearchResult{Hits: []graph.ProductTextHit{}, StoresNotMaterialized: []string{}, Exhausted: true}, tc.readErr
				},
			}
			s.store = store
			emb := productTextQueryEmbedder{space: graph.ProductTextSpace, embed: func(ctx context.Context, q string) ([]float64, error) {
				embeds++
				if q != "ceramic" {
					t.Errorf("query = %q", q)
				}
				if tc.timeout {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if tc.embedErr != nil {
					return nil, tc.embedErr
				}
				return graphtest.ProductTextEmbedder{}.EmbedQuery(ctx, q)
			}}
			if tc.unsupported {
				emb.space = "t3-unsupported"
			}
			s.SetEmbedder(emb)
			r := httptest.NewRecorder()
			s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/products/search",
				strings.NewReader(`{"query":"  ceramic  ","domain":"t3-test"}`)))
			if embeds != tc.embedCalls || reads != tc.readCalls {
				t.Errorf("embed calls %d, graph calls %d", embeds, reads)
			}
			if tc.status == "error" {
				if r.Code != 502 || !strings.Contains(r.Body.String(), `"error":"meaning_unavailable"`) ||
					strings.Contains(r.Body.String(), upstream.Error()) {
					t.Errorf("error response: HTTP %d %s", r.Code, r.Body)
				}
				return
			}
			var out search.ProductQueryResult
			if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil || r.Code != 200 || out.Mode != "query" ||
				out.MeaningStatus != tc.status || out.Hits == nil || len(out.Hits) != 0 || out.StoresNotMaterialized == nil {
				t.Errorf("response: HTTP %d %s (%v)", r.Code, r.Body, err)
			}
		})
	}
}

func TestProductTextReadREST(t *testing.T) {
	st := graphtest.ProductTextReads(t)
	var cfg config.Config
	cfg.ProductText.Search = true
	s := NewServer(cfg, st, nil)
	s.SetEmbedder(graphtest.ProductTextEmbedder{})
	handler := s.Handler()
	r := httptest.NewRecorder()
	handler.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/products/search",
		strings.NewReader(`{"query":"ceramic","domain":"t3-text-reads","limit":20}`)))
	var query search.ProductQueryResult
	if err := json.Unmarshal(r.Body.Bytes(), &query); err != nil || r.Code != 200 || query.MeaningStatus != "partial" ||
		len(query.Hits) != 1 || query.Hits[0].ID != graphtest.TextReadManyID || !query.Exhausted ||
		len(query.Hits[0].Matched) != 1 || query.Hits[0].Matched[0].Key != "key-1" {
		t.Fatalf("query: HTTP %d %s (%v)", r.Code, r.Body, err)
	}
	for _, tc := range []struct {
		path           string
		texts, omitted int
	}{
		{"/v1/products/" + graphtest.TextReadManyID, -1, 0},
		{"/v1/products/" + graphtest.TextReadManyID + "?with_texts=true", 64, 6},
		{"/v1/products?store=" + graphtest.TextReadStore + "&product_id=" + graphtest.TextReadStore + ":many&with_texts=true&text_keys=key-1&text_keys=key-2", 2, 0},
		{"/v1/products/" + graphtest.TextReadLargeID + "?with_texts=true", 2, 20},
	} {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, tc.path, nil))
		var out graph.ProductDetail
		if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil || r.Code != 200 {
			t.Fatalf("%s: HTTP %d %s (%v)", tc.path, r.Code, r.Body, err)
		}
		if tc.texts == -1 {
			if out.ProductTextFields != nil {
				t.Errorf("default lookup unexpectedly returned texts")
			}
			continue
		}
		if out.ProductTextFields == nil {
			t.Fatalf("%s did not return texts", tc.path)
		}
		wire, err := json.Marshal(out.ProductTextFields)
		if err != nil || len(wire) > 32<<10 || len(out.Texts) != tc.texts || out.TextsOmitted != tc.omitted {
			t.Errorf("%s: %d texts, %d omitted, %d bytes, marshal error %v", tc.path, len(out.Texts), out.TextsOmitted, len(wire), err)
		}
	}
}
