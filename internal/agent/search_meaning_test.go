package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

type meaningSearchEmbedder struct {
	calls atomic.Int32
	embed func(context.Context, string) ([]float64, error)
}

func (*meaningSearchEmbedder) Space() string { return graph.ProductTextSpace }

func (e *meaningSearchEmbedder) EmbedQuery(ctx context.Context, q string) ([]float64, error) {
	e.calls.Add(1)
	if e.embed != nil {
		return e.embed(ctx, q)
	}
	return graphtest.ProductTextEmbedder{}.EmbedQuery(ctx, q)
}

type meaningSearchWireHit struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Match         string `json:"match"`
	ProductID     string `json:"product_id"`
	Key           string `json:"key"`
	Checked       *bool  `json:"checked"`
	Excerpt       string `json:"excerpt"`
	ExcerptCut    *bool  `json:"excerpt_cut"`
	TextTruncated *bool  `json:"text_truncated"`
	Snippet       string `json:"snippet"`
}

type meaningSearchWire struct {
	Hits             []meaningSearchWireHit `json:"hits"`
	ProductsError    string                 `json:"products_error"`
	MeaningError     string                 `json:"meaning_error"`
	MeaningPartial   bool                   `json:"meaning_partial"`
	MeaningTruncated bool                   `json:"meaning_truncated"`
	HitsOmitted      int                    `json:"hits_omitted"`
}

func meaningSearchHit(id, text string) graph.ProductTextHit {
	return graph.ProductTextHit{
		ID: id, Domain: "dental", Store: "t4.test", ProductID: "t4.test:" + id, Title: "Ceramic kit", Score: .91,
		Matched: []graph.MatchedProductText{
			{ProductText: graph.ProductText{Key: "material", Text: text}, Similarity: .91, TextID: "t4-text"},
			{ProductText: graph.ProductText{Key: "color", Text: "white", Checked: true}, Similarity: .83, TextID: "t4-color"},
		},
	}
}

func meaningSearchResult(t *testing.T, ctx context.Context, s *Server, args string) (meaningSearchWire, []byte) {
	t.Helper()
	result, err := s.execTool(ctx, "search", json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return unmarshalResult[meaningSearchWire](t, json.RawMessage(raw)), raw
}

func TestAgentSearchMeaningExcludesTitlesAndKeepsPassage(t *testing.T) {
	st := &productSearchToolStore{
		vectorIDs: []string{"t4-entity-0", "t4-entity-1", "t4-entity-2", "t4-entity-3", "t4-entity-4", "t4-entity-5"},
		chunks:    []graph.ChunkSearchRow{{ID: "t4-passage", Text: "Ceramic kit passage."}},
		products: func(_ context.Context, query, domain string, limit int) ([]graph.Product, error) {
			if query != "ceramic kit" || domain != "dental" || limit != 3 {
				t.Errorf("title lookup got query=%q domain=%q limit=%d", query, domain, limit)
			}
			return []graph.Product{{ID: "t4-title-b", ProductID: "t4.test:b"}, {ID: "t4-title-a", ProductID: "t4.test:a"}}, nil
		},
	}
	for _, id := range st.vectorIDs {
		st.entities = append(st.entities, graph.EntitySearchRow{ID: id, Name: "Ceramic", Domain: "dental"})
	}
	text := strings.Repeat("é", 200)
	st.texts = func(_ context.Context, vec []float64, domain string, limit int, exclude []string) (graph.ProductTextSearchResult, error) {
		if len(vec) != 768 || vec[0] != 1 || domain != "dental" || limit != 1 || !slices.Equal(exclude, []string{"t4-title-b", "t4-title-a"}) {
			t.Errorf("meaning lookup got dim=%d domain=%q limit=%d exclude=%v", len(vec), domain, limit, exclude)
		}
		ranked := []graph.ProductTextHit{meaningSearchHit("t4-title-b", "duplicate"), meaningSearchHit("t4-meaning", text)}
		ranked = slices.DeleteFunc(ranked, func(hit graph.ProductTextHit) bool { return slices.Contains(exclude, hit.ID) })
		return graph.ProductTextSearchResult{Hits: ranked[:min(limit, len(ranked))]}, nil
	}
	emb := &meaningSearchEmbedder{}
	s := &Server{store: st, emb: emb, cfg: config.Config{ProductText: config.ProductTextConfig{Search: true}}}
	out, _ := meaningSearchResult(t, withDomain(t.Context(), "dental"), s, `{"query":"ceramic kit","limit":6}`)
	if len(out.Hits) != 6 || out.MeaningError != "" || out.ProductsError != "" {
		t.Fatalf("want six hits and no errors, got %+v", out)
	}
	if a, b := out.Hits[0], out.Hits[1]; a.ID != "t4-title-b" || b.ID != "t4-title-a" || a.Match != "title" || b.Match != "title" {
		t.Fatalf("title rank or match changed: %+v, %+v", a, b)
	}
	if h := out.Hits[2]; h.ID != "t4-meaning" || h.Match != "meaning" || h.ProductID != "t4.test:t4-meaning" ||
		h.Key != "material" || h.Checked == nil || *h.Checked || h.Excerpt != strings.Repeat("é", 160) {
		t.Fatalf("meaning hit lost its evidence: %+v", h)
	}
	if h := out.Hits[2]; h.ExcerptCut == nil || !*h.ExcerptCut || h.TextTruncated == nil || *h.TextTruncated {
		t.Errorf("want excerpt_cut=true and text_truncated=false for the agent's 160-rune cut, got %+v", h)
	}
	if h := out.Hits[5]; h.Kind != "chunk" || h.ID != "t4-passage" || st.chunkRead != 1 {
		t.Fatalf("want one lookup and a reserved passage, got %+v in %d reads", h, st.chunkRead)
	}
	if n := emb.calls.Load(); n != 1 {
		t.Fatalf("EmbedQuery calls = %d, want 1", n)
	}
}

func TestAgentSearchMeaningCompletenessWithoutHits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result graph.ProductTextSearchResult
		want   string
	}{
		{"complete", graph.ProductTextSearchResult{Exhausted: true}, `{"hits":[]}`},
		{"missing vectors", graph.ProductTextSearchResult{Partial: true, Exhausted: true}, `{"hits":[],"meaning_partial":true}`},
		{"unmaterialized store", graph.ProductTextSearchResult{
			Partial: true, StoresNotMaterialized: []string{"t4.test"}, Exhausted: true,
		}, `{"hits":[],"meaning_partial":true}`},
		{"candidate cap", graph.ProductTextSearchResult{CandidatesTruncated: true}, `{"hits":[],"meaning_truncated":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &productSearchToolStore{
				products: func(context.Context, string, string, int) ([]graph.Product, error) { return nil, nil },
				texts: func(context.Context, []float64, string, int, []string) (graph.ProductTextSearchResult, error) {
					return tc.result, nil
				},
			}
			s := &Server{store: st, emb: &meaningSearchEmbedder{}, cfg: config.Config{ProductText: config.ProductTextConfig{Search: true}}}
			_, raw := meaningSearchResult(t, t.Context(), s, `{"query":"ceramic","kind":"product"}`)
			if string(raw) != tc.want {
				t.Errorf("search result = %s, want %s", raw, tc.want)
			}
		})
	}
}

func TestAgentSearchMeaningPreservesMatchTruncation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		excerptCut    bool
		textTruncated bool
	}{
		{"complete match", false, false},
		{"excerpt already cut", true, false},
		{"input text truncated", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &productSearchToolStore{
				products: func(context.Context, string, string, int) ([]graph.Product, error) { return nil, nil },
				texts: func(context.Context, []float64, string, int, []string) (graph.ProductTextSearchResult, error) {
					hit := meaningSearchHit("t4-meaning", "Ceramic")
					hit.Matched[0].ExcerptCut = tc.excerptCut
					hit.Matched[0].TextTruncated = tc.textTruncated
					return graph.ProductTextSearchResult{Hits: []graph.ProductTextHit{hit}, Exhausted: true}, nil
				},
			}
			s := &Server{store: st, emb: &meaningSearchEmbedder{}, cfg: config.Config{ProductText: config.ProductTextConfig{Search: true}}}
			out, _ := meaningSearchResult(t, t.Context(), s, `{"query":"ceramic","kind":"product"}`)
			if len(out.Hits) != 1 {
				t.Fatalf("want one meaning hit, got %+v", out)
			}
			h := out.Hits[0]
			if h.Excerpt != "Ceramic" || h.ExcerptCut == nil || *h.ExcerptCut != tc.excerptCut ||
				h.TextTruncated == nil || *h.TextTruncated != tc.textTruncated {
				t.Errorf("want Ceramic excerpt with excerpt_cut=%t and text_truncated=%t, got %+v", tc.excerptCut, tc.textTruncated, h)
			}
		})
	}
}

func TestAgentSearchMeaningTitleSaturation(t *testing.T) {
	for _, kind := range []string{"auto", "product"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := &productSearchToolStore{
					products: func(_ context.Context, _, _ string, limit int) ([]graph.Product, error) {
						hits := make([]graph.Product, limit)
						for i := range hits {
							hits[i].ID = fmt.Sprintf("t4-title-%d", i)
						}
						return hits, nil
					},
					texts: func(context.Context, []float64, string, int, []string) (graph.ProductTextSearchResult, error) {
						t.Error("meaning searched with zero free slots")
						return graph.ProductTextSearchResult{}, nil
					},
				}
				emb := &meaningSearchEmbedder{}
				s := &Server{store: st, emb: emb, cfg: config.Config{ProductText: config.ProductTextConfig{Search: true}}}
				out, _ := meaningSearchResult(t, t.Context(), s, fmt.Sprintf(`{"query":"ceramic","kind":%q,"limit":4}`, kind))
				synctest.Wait()
				want := 2
				if kind == "product" {
					want = 4
				}
				if len(out.Hits) != want || out.MeaningError != "" || emb.calls.Load() != 1 {
					t.Fatalf("want %d title hits and one embedding, got %+v and %d embeddings", want, out, emb.calls.Load())
				}
				for _, h := range out.Hits {
					if h.Match != "title" {
						t.Fatalf("want title match, got %+v", h)
					}
				}
			})
		})
	}
}

func TestAgentSearchMeaningSkipsPunctuationOnlyQuery(t *testing.T) {
	for _, kind := range []string{"auto", "product"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := &productSearchToolStore{
					products: func(context.Context, string, string, int) ([]graph.Product, error) {
						return nil, nil
					},
					texts: func(context.Context, []float64, string, int, []string) (graph.ProductTextSearchResult, error) {
						return graph.ProductTextSearchResult{Hits: []graph.ProductTextHit{meaningSearchHit("t4-meaning", "Ceramic")}}, nil
					},
				}
				emb := &meaningSearchEmbedder{}
				s := &Server{store: st, emb: emb, cfg: config.Config{ProductText: config.ProductTextConfig{Search: true}}}
				_, raw := meaningSearchResult(t, t.Context(), s, fmt.Sprintf(`{"query":"?!…","kind":%q}`, kind))
				synctest.Wait()
				if n := emb.calls.Load(); n != 0 {
					t.Errorf("EmbedQuery calls = %d, want 0", n)
				}
				if st.textSearchCalled {
					t.Error("SearchProductTexts called for punctuation-only query")
				}
				if want := `{"hits":[]}`; string(raw) != want {
					t.Errorf("punctuation-only search = %s, want %s", raw, want)
				}
			})
		})
	}
}

func TestAgentSearchMeaningDeadlines(t *testing.T) {
	const vectorEntityID = "t4-vector-entity"
	for _, tc := range []struct {
		name, kind, status            string
		read, embed, lexical, title   time.Duration
		slowGraph                     bool
		autoOverride, productOverride time.Duration
		wantElapsed, wantGraphBudget  time.Duration
		wantVectorEntity              bool
	}{
		{"entity wait expires first", "auto", "", 400, 260, 100, 0, false, 0, 0, 260, 140, false},
		{"embedding finishes before entity cutoff", "auto", "", 400, 150, 100, 0, false, 0, 0, 150, 250, true},
		{"auto wait and graph share the default deadline", "auto", "timeout", 2000, 1080, 0, 20, true, 0, 0, 1320, 240, false},
		{"auto override includes remaining embedding", "auto", "timeout", 2000, 1060, 0, 20, true, 100, 0, 1120, 60, false},
		{"auto times out awaiting the embedding", "auto", "timeout", 2000, 1500, 0, 20, false, 0, 0, 1320, 0, false},
		{"product times out awaiting the embedding", "product", "timeout", 2000, 1900, 0, 100, false, 0, 0, 1500, 0, false},
		{"product override includes title and embedding", "product", "timeout", 2000, 260, 0, 100, true, 0, 400, 400, 140, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var graphBudget time.Duration
				st := &productSearchToolStore{
					chunks:         []graph.ChunkSearchRow{{ID: "t4-passage", Text: "Ceramic kit passage."}},
					entityLag:      tc.lexical * time.Millisecond,
					vectorIDs:      []string{vectorEntityID},
					vectorEntities: []graph.EntitySearchRow{{ID: vectorEntityID, Name: "Ceramic", Domain: "dental"}},
					products: func(context.Context, string, string, int) ([]graph.Product, error) {
						time.Sleep(tc.title * time.Millisecond)
						return []graph.Product{{ID: "t4-title"}}, nil
					},
					texts: func(ctx context.Context, _ []float64, _ string, _ int, _ []string) (graph.ProductTextSearchResult, error) {
						deadline, _ := ctx.Deadline()
						graphBudget = time.Until(deadline)
						if tc.slowGraph {
							<-ctx.Done()
							return graph.ProductTextSearchResult{}, ctx.Err()
						}
						return graph.ProductTextSearchResult{Hits: []graph.ProductTextHit{meaningSearchHit("t4-meaning", "Ceramic")}}, nil
					},
				}
				ended := make(chan error, 1)
				emb := &meaningSearchEmbedder{embed: func(ctx context.Context, q string) ([]float64, error) {
					select {
					case <-time.After(tc.embed * time.Millisecond):
						ended <- nil
						return graphtest.ProductTextEmbedder{}.EmbedQuery(ctx, q)
					case <-ctx.Done():
						ended <- ctx.Err()
						return nil, ctx.Err()
					}
				}}
				cfg := config.Config{Limits: config.LimitsConfig{ReadTimeoutMS: int(tc.read)}, ProductText: config.ProductTextConfig{
					Search: true, SearchTimeout: tc.productOverride * time.Millisecond, AutoTimeout: tc.autoOverride * time.Millisecond,
				}}
				s := &Server{store: st, emb: emb, cfg: cfg}
				out, _ := meaningSearchResult(t, t.Context(), s, fmt.Sprintf(`{"query":"ceramic","kind":%q,"limit":4}`, tc.kind))
				if elapsed := time.Since(start); elapsed != tc.wantElapsed*time.Millisecond {
					t.Errorf("search elapsed %s, want %dms", elapsed, tc.wantElapsed)
				}
				if out.MeaningError != tc.status || out.ProductsError != "" || len(out.Hits) == 0 || out.Hits[0].ID != "t4-title" {
					t.Errorf("want the title and meaning_error=%q, got %+v", tc.status, out)
				}
				if tc.kind == "auto" && !slices.ContainsFunc(out.Hits, func(h meaningSearchWireHit) bool { return h.ID == "t4-passage" }) {
					t.Error("missing regular passage")
				}
				if found := slices.ContainsFunc(out.Hits, func(h meaningSearchWireHit) bool { return h.Kind == "entity" && h.ID == vectorEntityID }); found != tc.wantVectorEntity {
					t.Errorf("vector-only entity present = %t, want %t", found, tc.wantVectorEntity)
				}
				if tc.wantGraphBudget == 0 && st.textSearchCalled {
					t.Error("SearchProductTexts called after the embedding wait timed out")
				}
				if graphBudget != tc.wantGraphBudget*time.Millisecond {
					t.Errorf("graph budget %s, want %dms", graphBudget, tc.wantGraphBudget)
				}
				if n := emb.calls.Load(); n != 1 {
					t.Errorf("EmbedQuery calls = %d, want 1", n)
				}
				if err := <-ended; tc.embed > tc.wantElapsed && !errors.Is(err, context.Canceled) {
					t.Errorf("outstanding embedding at tool return got %v, want cancellation", err)
				}
			})
		})
	}
}

func TestAgentSearchMeaningParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		ended := make(chan error, 1)
		started := make(chan struct{})
		emb := &meaningSearchEmbedder{embed: func(ctx context.Context, _ string) ([]float64, error) {
			close(started)
			<-ctx.Done()
			ended <- ctx.Err()
			return nil, ctx.Err()
		}}
		st := &productSearchToolStore{products: func(context.Context, string, string, int) ([]graph.Product, error) {
			return nil, nil
		}}
		s := &Server{store: st, emb: emb, cfg: config.Config{ProductText: config.ProductTextConfig{Search: true}}}
		go func() {
			select {
			case <-started:
			case <-time.After(time.Second):
			}
			cancel()
		}()
		_, err := s.execTool(ctx, "search", json.RawMessage(`{"query":"ceramic","kind":"product"}`))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("search error = %v, want context.Canceled", err)
		}
		if err := <-ended; !errors.Is(err, context.Canceled) || emb.calls.Load() != 1 {
			t.Errorf("embedding calls=%d, error=%v", emb.calls.Load(), err)
		}
	})
}

func TestAgentSearchMeaningErrors(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		emb          search.QueryEmbedder
		textErr      error
	}{
		{"unsupported space", "unavailable", productSearchTestEmbedder{}, nil},
		{"bad vector", "unavailable", &meaningSearchEmbedder{embed: func(context.Context, string) ([]float64, error) { return []float64{1}, nil }}, nil},
		{"embedder failed", "unavailable", &meaningSearchEmbedder{embed: func(context.Context, string) ([]float64, error) { return nil, errors.New("upstream failure") }}, nil},
		{"missing index", "unavailable", &meaningSearchEmbedder{}, graph.ErrProductTextIndexUnavailable},
		{"database timeout", "timeout", &meaningSearchEmbedder{}, graph.ErrQueryTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &productSearchToolStore{
				products: func(context.Context, string, string, int) ([]graph.Product, error) {
					return nil, graph.ErrProductLinkSearchTooLarge
				},
				texts: func(context.Context, []float64, string, int, []string) (graph.ProductTextSearchResult, error) {
					if tc.textErr == nil {
						t.Error("unusable embedding reached the graph")
					}
					return graph.ProductTextSearchResult{}, tc.textErr
				},
			}
			s := &Server{store: st, emb: tc.emb, cfg: config.Config{ProductText: config.ProductTextConfig{Search: true}}}
			out, _ := meaningSearchResult(t, t.Context(), s, `{"query":"ceramic","kind":"product"}`)
			if out.MeaningError != tc.status || out.ProductsError != "index_too_large" || len(out.Hits) != 0 {
				t.Errorf("want independent title and meaning errors, got %+v", out)
			}
		})
	}
}

func TestAgentSearchMeaningFlagOffGolden(t *testing.T) {
	st := &productSearchToolStore{
		chunks: []graph.ChunkSearchRow{{ID: "t4-passage", DocID: "t4-doc", Domain: "dental", Title: "Guide", Text: "Ceramic kit passage."}},
		products: func(context.Context, string, string, int) ([]graph.Product, error) {
			return []graph.Product{{ID: "t4-title", Domain: "dental", Store: "t4.test", ProductID: "t4.test:1", Title: "Ceramic", Brand: "Acme"}}, nil
		},
	}
	emb := &meaningSearchEmbedder{}
	s := &Server{store: st, emb: emb}
	_, auto := meaningSearchResult(t, t.Context(), s, `{"query":"ceramic","limit":2}`)
	_, product := meaningSearchResult(t, t.Context(), s, `{"query":"ceramic","kind":"product"}`)
	const title = `{"domain":"dental","kind":"product","id":"t4-title","title":"Ceramic","store":"t4.test","product_id":"t4.test:1","brand":"Acme"}`
	const passage = `{"domain":"dental","kind":"chunk","id":"t4-passage","title":"Guide","score":0.016666666666666666,"snippet":"Ceramic kit passage.","document_id":"t4-doc"}`
	if want := `{"hits":[` + title + `,` + passage + `]}`; string(auto) != want {
		t.Errorf("auto search bytes\ngot  %s\nwant %s", auto, want)
	}
	if want := `{"hits":[` + title + `]}`; string(product) != want {
		t.Errorf("product search bytes\ngot  %s\nwant %s", product, want)
	}
	if n := emb.calls.Load(); n != 1 {
		t.Errorf("flag-off calls = %d, want only the regular auto embedding", n)
	}
}

func TestAgentSearchMeaningFitsEncodedMixedResult(t *testing.T) {
	wide := strings.Repeat("<&\u2028", 10000)
	st := &productSearchToolStore{
		chunks: []graph.ChunkSearchRow{{ID: "t4-passage", DocID: "t4-doc", Title: wide, Text: "Ceramic kit passage."}},
		products: func(context.Context, string, string, int) ([]graph.Product, error) {
			return []graph.Product{{ID: "t4-title", ProductID: "t4.test:1", Title: wide, Brand: wide}}, nil
		},
		texts: func(context.Context, []float64, string, int, []string) (graph.ProductTextSearchResult, error) {
			top := meaningSearchHit("t4-meaning", strings.Repeat("<", 200))
			top.Matched[0].Checked = true
			top.Matched[0].TextTruncated = true
			bottom := meaningSearchHit("t4-omitted", "Ceramic")
			bottom.ProductID = wide
			return graph.ProductTextSearchResult{
				Hits: []graph.ProductTextHit{top, bottom}, Partial: true, CandidatesTruncated: true,
			}, nil
		},
	}
	for i := range 100 {
		st.entities = append(st.entities, graph.EntitySearchRow{ID: fmt.Sprintf("t4-entity-%03d", i), Name: wide})
	}
	s := &Server{store: st, emb: &meaningSearchEmbedder{}, cfg: config.Config{ProductText: config.ProductTextConfig{Search: true}}}
	out, raw := meaningSearchResult(t, t.Context(), s, `{"query":"ceramic","limit":100}`)
	if len(raw) > toolResultCap || len(out.Hits) < 3 || out.HitsOmitted <= 0 || len(out.Hits)+out.HitsOmitted != 100 {
		t.Fatalf("got %d encoded bytes, %d hits, %d omitted", len(raw), len(out.Hits), out.HitsOmitted)
	}
	if !out.MeaningPartial || !out.MeaningTruncated {
		t.Errorf("fitted result lost completeness flags: meaning_partial=%t meaning_truncated=%t", out.MeaningPartial, out.MeaningTruncated)
	}
	if h := out.Hits[0]; h.ID != "t4-title" || h.Match != "title" || h.ProductID != "t4.test:1" {
		t.Errorf("title lost essential fields: %+v", h)
	}
	if h := out.Hits[1]; h.ID != "t4-meaning" || h.Match != "meaning" || h.ProductID != "t4.test:t4-meaning" ||
		h.Key != "material" || h.Checked == nil || !*h.Checked || h.Excerpt != strings.Repeat("<", 160) ||
		h.ExcerptCut == nil || !*h.ExcerptCut || h.TextTruncated == nil || !*h.TextTruncated {
		t.Errorf("meaning hit lost essential fields: %+v", h)
	}
	passage := false
	for _, h := range out.Hits {
		if h.ID == "t4-omitted" {
			t.Error("oversized lower-ranked product was kept")
		}
		if h.ID == "t4-passage" && h.Snippet == "Ceramic kit passage." {
			passage = true
		}
	}
	if !passage {
		t.Error("the reserved passage lost its evidence or was dropped")
	}
}
