package graph_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func BenchmarkTraverse(b *testing.B) {
	const (
		neighbours = 200
		leaves     = 5
		allEdges   = neighbours * (1 + leaves)
	)
	st := graphtest.Store(b)
	ctx := context.Background()
	source := uniqueSource(b)
	docID := testUpsert(b, st, "file", source, "bench-traverse").ID
	b.Cleanup(func() {
		if err := st.DeleteDocument(ctx, docID); err != nil {
			b.Error(err)
		}
	})
	entity := func(name string) graph.EntityUpsert {
		return graph.EntityUpsert{Key: graph.EntityKey(source + " " + name), Name: name, Type: "place"}
	}
	hub := entity("Hub")
	chunks := make([]graph.Chunk, neighbours)
	type extraction struct {
		entities []graph.EntityUpsert
		facts    []graph.FactUpsert
	}
	extractions := make([]extraction, neighbours)
	start := 0
	for n := range chunks {
		var text strings.Builder
		branch := entity(fmt.Sprintf("Neighbour %03d", n))
		ents := []graph.EntityUpsert{hub, branch}
		mention := func(e graph.EntityUpsert) graph.Span {
			offset := text.Len()
			text.WriteString(e.Name)
			text.WriteString(". ")
			return graph.Span{Start: offset, End: offset + len(e.Name), Text: e.Name}
		}
		hubSpan := mention(hub)
		branchSpan := mention(branch)
		head, tail := hub, branch
		headSpan, tailSpan := hubSpan, branchSpan
		if n%2 == 0 {
			head, tail = tail, head
			headSpan, tailSpan = tailSpan, headSpan
		}
		facts := []graph.FactUpsert{{
			HeadKey: head.Key, TailKey: tail.Key, Rel: "connects", Confidence: .9,
			HeadMention: headSpan, TailMention: tailSpan,
		}}
		for i := range leaves {
			leaf := entity(fmt.Sprintf("Leaf %03d %d", n, i))
			ents = append(ents, leaf)
			facts = append(facts, graph.FactUpsert{
				HeadKey: branch.Key, TailKey: leaf.Key, Rel: "connects", Confidence: .9,
				HeadMention: branchSpan, TailMention: mention(leaf),
			})
		}
		chunks[n] = graph.Chunk{Seq: n, Text: text.String(), Start: start, End: start + text.Len()}
		start += text.Len()
		extractions[n] = extraction{entities: ents, facts: facts}
	}
	if err := st.PutChunks(ctx, docID, chunks); err != nil {
		b.Fatal(err)
	}
	for n, fx := range extractions {
		chunkID := fmt.Sprintf("%s-c%d", docID, n)
		if err := st.UpsertExtraction(ctx, docID, chunkID, "benchmark:1", fx.entities, fx.facts); err != nil {
			b.Fatal(err)
		}
	}

	for depth, wantEdges := range []int{neighbours, allEdges} {
		depth++
		b.Run(fmt.Sprintf("depth=%d/both", depth), func(b *testing.B) {
			// Leave room for the duplicate back edge at the last branch.
			traverse := func() (graph.Subgraph, error) {
				return st.Traverse(ctx, graph.EntityID(hub.Key), "", "both", depth, allEdges+1, false)
			}
			sub, err := traverse()
			if err != nil || len(sub.Edges) != wantEdges {
				b.Fatalf("Traverse depth %d returned %d edges, want %d, err = %v", depth, len(sub.Edges), wantEdges, err)
			}
			for _, edge := range sub.Edges {
				if edge.ChunkID == "" || edge.Excerpt == "" {
					b.Fatalf("edge missing evidence: %+v", edge)
				}
			}
			for b.Loop() {
				if _, err := traverse(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
