package graph_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func domainRefsFixture(t *testing.T, candidates bool) (*graph.Store, string, string) {
	t.Helper()
	st := graphtest.Store(t)
	ctx := context.Background()
	doc, chunk := entitiesTestDoc(t, st, "Acme-Max uses Quillon.")
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, doc); err != nil {
			t.Error(err)
		}
	})
	head := graph.DomainKey("zorbo", "acme-"+doc)
	tail := graph.DomainKey("zorbo", "quillon-"+doc)
	ents := []graph.EntityUpsert{
		{Key: head, Name: "Acme-Max", Type: "organization"},
		{Key: tail, Name: "Quillon", Type: "product"},
	}
	facts := []graph.FactUpsert{{HeadKey: head, TailKey: tail, Rel: "uses", Confidence: .9,
		HeadMention: graph.Span{Start: 0, End: 8}, TailMention: graph.Span{Start: 14, End: 21},
		JudgeInput: []byte("Acme-Max uses Quillon.")}}
	if err := st.UpsertExtractionOpts(ctx, doc, chunk, "mock:1", ents, facts, graph.WriteOptions{Domain: "zorbo", FactsAsCandidates: candidates}); err != nil {
		t.Fatal(err)
	}
	// Give the far endpoint a distinct property to catch copying the start's domain.
	rawRun(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (e:Entity {key:'%s'}) SET e.domain='quillon'`, tail))
	return st, head, tail
}

func TestEntityViewRelatedDomains(t *testing.T) {
	for _, candidates := range []bool{false, true} {
		t.Run(fmt.Sprintf("candidates=%t", candidates), func(t *testing.T) {
			st, head, tail := domainRefsFixture(t, candidates)
			domains := map[string]string{head: "zorbo", tail: "quillon"}
			for _, start := range []string{head, tail} {
				view, err := st.EntityViewOpts(context.Background(), start, graph.ViewOptions{Domain: domains[start], IncludeUnverified: candidates})
				if err != nil {
					t.Fatal(err)
				}
				other := tail
				if start == tail {
					other = head
				}
				if len(view.Facts) != 1 || len(view.Together) != 1 {
					t.Fatalf("view=%+v, want one fact and one co-mention", view)
				}
				fact := view.Facts[0]
				if fact.Other.Key != other || fact.Other.Domain != domains[other] || fact.Outgoing != (start == head) || fact.Unverified != candidates {
					t.Errorf("fact=%+v, want related entity %s in %s", fact, other, domains[other])
				}
				if got := view.Together[0]; got.Key != other || got.Domain != domains[other] || got.SharedChunks != 1 {
					t.Errorf("co-mention=%+v, want related entity %s in %s", got, other, domains[other])
				}
			}
		})
	}
}

func TestTraverseRelatedDomains(t *testing.T) {
	for _, candidates := range []bool{false, true} {
		t.Run(fmt.Sprintf("candidates=%t", candidates), func(t *testing.T) {
			st, head, tail := domainRefsFixture(t, candidates)
			domains := map[string]string{head: "zorbo", tail: "quillon"}
			for _, direction := range []string{"out", "in", "both"} {
				t.Run(direction, func(t *testing.T) {
					start := head
					if direction == "in" {
						start = tail
					}
					sub, err := st.TraverseOpts(context.Background(), start, "", direction, 2, 10, graph.ViewOptions{Domain: domains[start], IncludeUnverified: candidates})
					if err != nil {
						t.Fatal(err)
					}
					if len(sub.Nodes) != 2 || len(sub.Edges) != 1 {
						t.Fatalf("subgraph=%+v, want two nodes and one edge", sub)
					}
					for _, node := range sub.Nodes {
						if want, ok := domains[node.Key]; !ok || node.Domain != want {
							t.Errorf("node %s domain=%q, want %q", node.Key, node.Domain, want)
						}
					}
				})
			}
		})
	}
}
