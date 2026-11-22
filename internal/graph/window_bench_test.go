package graph_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func BenchmarkTraverseWindow(b *testing.B) {
	st := graphtest.Store(b)
	ctx := b.Context()
	tag := fmt.Sprintf("traversewindow%d", time.Now().UnixNano())
	uri := os.Getenv(graphtest.URIEnv)
	b.Cleanup(func() {
		if err := rawExec(uri, fmt.Sprintf("MATCH (n) WHERE n.window_bench = %q DETACH DELETE n", tag)); err != nil {
			b.Error(err)
		}
	})
	// Matchable evidence is skewed toward the first branch. Eight first-hop
	// rows leave 32 slots at hop two, so the hub must top up its initial share.
	if err := rawExec(uri, fmt.Sprintf(`
WITH %q AS tag
CREATE (root:Entity {id:tag, key:tag, name:'Root', window_bench:tag})
WITH root, tag
UNWIND range(0, 2399) AS i
CREATE (d:Document {id:tag + 'd' + toString(i), window_bench:tag,
                    document_date:CASE WHEN i < 80 THEN '2101-01-01T00:00:00.000000000Z'
                                       ELSE '2101-01-02T00:00:00.000000000Z' END})
CREATE (d)-[:HAS_CHUNK]->(c:Chunk {id:d.id + '-c0', doc_id:d.id, text:'Traverse benchmark evidence', window_bench:tag})
CREATE (e:Entity {id:tag + 'e' + toString(i), key:tag + 'e' + toString(i), name:'Endpoint', window_bench:tag})
FOREACH (_ IN CASE WHEN i < 8 THEN [1] ELSE [] END |
  CREATE (root)-[:links {doc_id:d.id, chunk_id:c.id, confidence:0.9}]->(e))
WITH tag, i, d, c, e
WHERE i >= 8 AND i < 160
MATCH (branch:Entity {id:tag + 'e0'})
CREATE (branch)-[:links {doc_id:d.id, chunk_id:c.id, confidence:0.9}]->(e)`, tag)); err != nil {
		b.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		win  graph.DateWindow
	}{
		{"unwindowed", graph.DateWindow{}},
		{"narrow", parseWindow(b, "2101-01-01", "2101-01-01")},
		{"broad", parseWindow(b, "2101-01-01", "2101-01-02")},
	} {
		b.Run(tc.name, func(b *testing.B) {
			traverse := func() (graph.Subgraph, error) {
				return st.TraverseOpts(ctx, tag, "links", "both", 2, 40, graph.ViewOptions{DateWindow: tc.win})
			}
			if sub, err := traverse(); err != nil || len(sub.Edges) != 40 {
				b.Fatalf("TraverseOpts returned %d edges, want 40, err = %v", len(sub.Edges), err)
			}
			for b.Loop() {
				if _, err := traverse(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
