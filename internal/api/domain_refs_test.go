package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestEntityDomainsREST(t *testing.T) {
	for _, candidates := range []bool{false, true} {
		t.Run(fmt.Sprintf("candidates=%t", candidates), func(t *testing.T) {
			st := graphtest.Store(t)
			ctx := context.Background()
			doc, err := st.UpsertDocument(ctx, graph.DocumentInput{
				Kind: "text", Source: fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()), Hash: "rest-domains", Domain: "zorbo",
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := st.DeleteDocument(ctx, doc.ID); err != nil {
					t.Error(err)
				}
			})
			text := "Acme-Max uses Quillon."
			if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text, End: len(text)}}); err != nil {
				t.Fatal(err)
			}
			head := graph.DomainKey("zorbo", "acme-"+doc.ID)
			tail := graph.DomainKey("zorbo", "quillon-"+doc.ID)
			ents := []graph.EntityUpsert{
				{Key: head, Name: "Acme-Max", Type: "organization"},
				{Key: tail, Name: "Quillon", Type: "product"},
			}
			facts := []graph.FactUpsert{{HeadKey: head, TailKey: tail, Rel: "uses", Confidence: .9,
				HeadMention: graph.Span{Start: 0, End: 8}, TailMention: graph.Span{Start: 14, End: 21},
				JudgeInput: []byte(text)}}
			if err := st.UpsertExtractionOpts(ctx, doc.ID, doc.ID+"-c0", "mock:1", ents, facts,
				graph.WriteOptions{Domain: "zorbo", FactsAsCandidates: candidates}); err != nil {
				t.Fatal(err)
			}
			// Distinct node properties catch copying the viewed entity's domain.
			adminGraphWrite(t, fmt.Sprintf(`MATCH (e:Entity {key:'%s'}) SET e.domain='quillon'`, tail))
			cfg := config.Config{}
			cfg.Facts.AllowUnverifiedReads = candidates
			h := api.NewServer(cfg, st, nil).Handler()
			type ref struct {
				Key    string `json:"key"`
				Domain string `json:"domain"`
			}
			domains := map[string]string{head: "zorbo", tail: "quillon"}
			checkRef := func(t *testing.T, label string, got ref, key string) {
				t.Helper()
				if got.Key != key || got.Domain != domains[key] {
					t.Errorf("%s = %+v, want key %q in domain %q", label, got, key, domains[key])
				}
			}
			for _, start := range []string{head, tail} {
				t.Run(domains[start], func(t *testing.T) {
					other := tail
					if start == tail {
						other = head
					}
					r := httptest.NewRequest(http.MethodGet, "/v1/entities/"+url.PathEscape(start)+fmt.Sprintf("?include_unverified=%t", candidates), nil)
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != http.StatusOK {
						t.Fatalf("entity HTTP %d: %s", w.Code, w.Body.String())
					}
					var view struct {
						ref
						Facts    []struct{ Other ref } `json:"facts"`
						Together []ref                 `json:"together"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
						t.Fatal(err)
					}
					checkRef(t, "viewed entity", view.ref, start)
					if len(view.Facts) != 1 || len(view.Together) != 1 {
						t.Fatalf("entity body = %s, want one fact and one co-mention", w.Body.String())
					}
					checkRef(t, "fact other", view.Facts[0].Other, other)
					checkRef(t, "co-mention", view.Together[0], other)

					body := fmt.Sprintf(`{"entity":%q,"direction":"both","include_unverified":%t}`, start, candidates)
					r = httptest.NewRequest(http.MethodPost, "/v1/traverse", strings.NewReader(body))
					r.Header.Set("Content-Type", "application/json")
					w = httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != http.StatusOK {
						t.Fatalf("traverse HTTP %d: %s", w.Code, w.Body.String())
					}
					var sub struct {
						Nodes []ref `json:"nodes"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &sub); err != nil {
						t.Fatal(err)
					}
					if len(sub.Nodes) != 2 {
						t.Fatalf("traverse body = %s, want both endpoints", w.Body.String())
					}
					seen := map[string]bool{}
					for _, node := range sub.Nodes {
						want, ok := domains[node.Key]
						if !ok {
							t.Fatalf("traverse returned unexpected node key %q", node.Key)
						}
						if node.Domain != want {
							t.Errorf("traverse node %q domain = %q, want %q", node.Key, node.Domain, want)
						}
						seen[node.Key] = true
					}
					if !seen[head] || !seen[tail] {
						t.Errorf("traverse nodes = %+v, want both endpoints", sub.Nodes)
					}
				})
			}
		})
	}
}
