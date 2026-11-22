package search_test

import (
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

func BenchmarkSearchWindow(b *testing.B) {
	st := graphtest.Store(b)
	ctx := b.Context()
	tag := uniqueToken(b)
	write := windowTestWriter(b)
	params := map[string]any{"tag": tag, "token": domain.DomainToken(tag)}
	b.Cleanup(func() {
		if err := write("MATCH (n) WHERE n.window_bench = $tag DETACH DELETE n", params); err != nil {
			b.Error(err)
		}
	})
	// Search sees 800 matches before the Cypher window, below Tantivy's
	// cap. Each case can fill the same limit after filtering.
	if err := write(`
UNWIND range(0, 2399) AS i
CREATE (d:Document {id:$tag + 'd' + toString(i), window_bench:$tag, domain:$tag,
                    document_date:CASE WHEN i < 80 THEN '2102-01-01T00:00:00.000000000Z'
                                       ELSE '2102-01-02T00:00:00.000000000Z' END})
CREATE (d)-[:HAS_CHUNK]->(:Chunk {id:d.id + '-c0', doc_id:d.id, window_bench:$tag, domain:$tag, domain_tok:$token,
                                text:CASE WHEN i < 800 THEN $tag + ' matching catalogue passage'
                                          ELSE 'unrelated catalogue passage' END})`, params); err != nil {
		b.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		win  graph.DateWindow
	}{
		{"unwindowed", graph.DateWindow{}},
		{"narrow", searchWindow(b, "2102-01-01", "2102-01-01")},
		{"broad", searchWindow(b, "2102-01-01", "2102-01-02")},
	} {
		b.Run(tc.name, func(b *testing.B) {
			query := func() ([]search.Hit, error) {
				return search.Query(ctx, st, nil, tag, "chunk", 20, search.InDomain(tag), search.InDateWindow(tc.win))
			}
			if hits, err := query(); err != nil || len(hits) != 20 {
				b.Fatalf("search returned %d hits, want 20, err = %v", len(hits), err)
			}
			for b.Loop() {
				if _, err := query(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
