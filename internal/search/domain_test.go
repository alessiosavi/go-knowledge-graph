package search_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

func TestScopedSearchVectorOverfetch(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	tag := fmt.Sprintf("zorbo%d", time.Now().UnixNano())
	if err := st.EnsureVectorIndex(ctx, 768); err != nil {
		t.Fatal(err)
	}
	vec := graphtest.Vector(tag, 768)
	want := ""
	// The four closest neighbours belong to a. The fifth belongs to b:
	// a scoped limit of one must over-fetch to reach it.
	for i := 0; i < 5; i++ {
		slug := "a"
		if i == 4 {
			slug = "b"
		}
		source := fmt.Sprintf("%s-%d", tag, i)
		doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: source, Hash: source, Domain: slug})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: source}}); err != nil {
			t.Fatal(err)
		}
		key := graph.DomainKey(slug, source)
		if err := st.UpsertExtractionOpts(ctx, doc.ID, doc.ID+"-c0", "mock", []graph.EntityUpsert{{Key: key, Name: source}}, nil, graph.WriteOptions{Domain: slug}); err != nil {
			t.Fatal(err)
		}
		v := append([]float64(nil), vec...)
		v[767] += float64(i) * 0.005
		if err := st.SetEntityEmbedding(ctx, graph.EntityID(key), v); err != nil {
			t.Fatal(err)
		}
		if slug == "b" {
			want = graph.EntityID(key)
		}
	}
	hits, err := search.Query(ctx, st, vecEmbedder{vec}, tag+"unseen", "entity", 1, search.InDomain("b"))
	if err != nil || len(hits) != 1 || hits[0].ID != want || hits[0].Domain != "b" {
		t.Fatalf("scoped vector=%+v %v", hits, err)
	}
}
