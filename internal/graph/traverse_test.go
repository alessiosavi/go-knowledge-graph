package graph_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

// traverseFixture builds A -wrote-> B -visited-> C inside one document
// with evidence spans, returning the entity keys.
func traverseFixture(t *testing.T, st *graph.Store) (a, b, c string, docID string) {
	t.Helper()
	ctx := context.Background()
	fx := newFixture(t)
	a, b, c = fx.ents[0].Key, fx.ents[1].Key, graph.EntityKey("Paris "+uniqueSource(t))
	docID = testUpsert(t, st, "file", uniqueSource(t), "hash-a").ID
	text := "Ada Lovelace wrote the first algorithm in Paris."
	if err := st.PutChunks(ctx, docID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	ents := append(fx.ents, graph.EntityUpsert{Key: c, Name: "Paris " + uniqueSource(t), Type: "place"})
	facts := []graph.FactUpsert{
		{
			HeadKey: a, TailKey: b, Rel: "wrote", Confidence: 0.9,
			HeadMention: graph.Span{Start: 0, End: 12, Text: "Ada Lovelace"},
			TailMention: graph.Span{Start: 19, End: 32, Text: "first algorithm"},
		},
		{
			HeadKey: b, TailKey: c, Rel: "located_in", Confidence: 0.8,
			HeadMention: graph.Span{Start: 19, End: 32, Text: "first algorithm"},
			TailMention: graph.Span{Start: 36, End: 41, Text: "Paris"},
		},
	}
	if err := st.UpsertExtraction(ctx, docID, docID+"-c0", "mock:1", ents, facts); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	return a, b, c, docID
}

func TestTraverseReturnsEdgesWithEvidence(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	a, b, c, docID := traverseFixture(t, st)

	sub, err := st.Traverse(context.Background(), a, "", "out", 2, 50, false)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(sub.Nodes) != 3 {
		t.Fatalf("nodes = %+v, want 3", sub.Nodes)
	}
	if len(sub.Edges) != 2 {
		t.Fatalf("edges = %+v, want 2", sub.Edges)
	}
	e := sub.Edges[0]
	if e.From != a || e.To != b || e.Rel != "wrote" || e.Confidence != 0.9 {
		t.Errorf("edge 0 = %+v", e)
	}
	if e.DocID != docID || e.ChunkID != docID+"-c0" {
		t.Errorf("edge provenance = %q %q", e.DocID, e.ChunkID)
	}
	if !strings.Contains(e.Excerpt, "Ada Lovelace") {
		t.Errorf("edge excerpt = %q", e.Excerpt)
	}
	_ = c
	_ = uri
}

func TestTraverseInDepthTwoAdvancesFrontier(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	a, b, c, _ := traverseFixture(t, st)
	_ = b

	sub, err := st.Traverse(ctx, c, "", "in", 2, 50, false)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(sub.Edges) != 2 {
		t.Fatalf("edges = %+v, want 2 (B->C then A->B)", sub.Edges)
	}
	if len(sub.Nodes) != 3 {
		t.Fatalf("nodes = %+v, want 3 (the From endpoints must be enqueued)", sub.Nodes)
	}
	foundA := false
	for _, n := range sub.Nodes {
		if n.Key == a {
			foundA = true
		}
	}
	if !foundA {
		t.Errorf("nodes = %+v, want the A endpoint", sub.Nodes)
	}
}

func TestTraverseBothDepthTwoDeduplicatesEdges(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	a, _, _, _ := traverseFixture(t, st)

	sub, err := st.Traverse(ctx, a, "", "both", 2, 50, false)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	seen := map[string]int{}
	for _, e := range sub.Edges {
		seen[e.From+"|"+e.Rel+"|"+e.To+"|"+e.ChunkID]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("edge %s emitted %d times", k, n)
		}
	}
	if len(seen) != 2 {
		t.Errorf("unique edges = %d, want 2", len(seen))
	}
}

func TestTraverseSelfLoopNotDoubled(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	fx := newFixture(t)
	docID := testUpsert(t, st, "file", uniqueSource(t), "hash-a").ID
	text := "Ada Lovelace wrote Ada Lovelace notes."
	if err := st.PutChunks(ctx, docID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatal(err)
	}
	loop := fx.fact
	loop.TailKey = fx.ents[0].Key // self loop
	if err := st.UpsertExtraction(ctx, docID, docID+"-c0", "mock:1", fx.ents, []graph.FactUpsert{loop}); err != nil {
		t.Fatal(err)
	}
	sub, err := st.Traverse(ctx, fx.ents[0].Key, "", "both", 1, 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Edges) != 1 {
		t.Errorf("self-loop edges = %+v, want exactly 1", sub.Edges)
	}
}

func TestTraverseIncomingExcerptAnchorsOnTheNode(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	fx := newFixture(t)
	docID := testUpsert(t, st, "file", uniqueSource(t), "hash-a").ID
	// Mentions far apart so the 120-byte window can only show one side.
	pad := strings.Repeat("Background filler sentence. ", 8)
	text := pad + "Ada Lovelace wrote " + strings.Repeat("carefully ", 15) + "the first algorithm in Paris."
	adaStart := len(pad)
	algoStart := adaStart + len("Ada Lovelace wrote ") + len(strings.Repeat("carefully ", 15))
	if err := st.PutChunks(ctx, docID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatal(err)
	}
	fact := fx.fact
	fact.HeadMention = graph.Span{Start: adaStart, End: adaStart + 12, Text: "Ada Lovelace"}
	fact.TailMention = graph.Span{Start: algoStart, End: algoStart + len("the first algorithm"), Text: "the first algorithm"}
	if err := st.UpsertExtraction(ctx, docID, docID+"-c0", "mock:1", fx.ents, []graph.FactUpsert{fact}); err != nil {
		t.Fatal(err)
	}

	outSub, err := st.Traverse(ctx, fx.ents[0].Key, "", "out", 1, 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outSub.Edges) != 1 || !strings.Contains(outSub.Edges[0].Excerpt, "Ada Lovelace") {
		t.Errorf("outgoing excerpt = %+v, want the anchor's mention (Ada)", outSub.Edges)
	}
	inSub, err := st.Traverse(ctx, fx.ents[1].Key, "", "in", 1, 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inSub.Edges) != 1 {
		t.Fatalf("edges = %+v", inSub.Edges)
	}
	if !strings.Contains(inSub.Edges[0].Excerpt, "first algorithm") || strings.Contains(inSub.Edges[0].Excerpt, "Ada Lovelace") {
		t.Errorf("incoming excerpt = %q, want the anchor's mention (the algorithm) only", inSub.Edges[0].Excerpt)
	}
}

func TestTraverseFiltersRelationAndDirection(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	a, b, _, _ := traverseFixture(t, st)

	sub, err := st.Traverse(ctx, b, "wrote", "in", 1, 50, false)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(sub.Edges) != 1 || sub.Edges[0].From != a || sub.Edges[0].To != b {
		t.Errorf("incoming wrote edge = %+v", sub.Edges)
	}
	sub, err = st.Traverse(ctx, b, "visited", "out", 1, 50, false)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(sub.Edges) != 0 {
		t.Errorf("relation filter leaked edges: %+v", sub.Edges)
	}
}

func TestTraverseDepthCapRejectsThree(t *testing.T) {
	st := graphtest.Store(t)
	a, _, _, _ := traverseFixture(t, st)
	if _, err := st.Traverse(context.Background(), a, "", "out", 3, 50, false); err == nil {
		t.Fatal("depth 3: want the tool cap error")
	}
}

func TestTraverseExcludesLowConfidenceByDefault(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	fx := newFixture(t)
	docID := testUpsert(t, st, "file", uniqueSource(t), "hash-a").ID
	text := "Ada Lovelace wrote the note."
	if err := st.PutChunks(ctx, docID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatal(err)
	}
	low := fx.fact
	low.Confidence = 0.2
	low.LowConfidence = true
	if err := st.UpsertExtraction(ctx, docID, docID+"-c0", "mock:1", fx.ents, []graph.FactUpsert{low}); err != nil {
		t.Fatal(err)
	}

	sub, err := st.Traverse(ctx, fx.ents[0].Key, "", "out", 1, 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Edges) != 0 {
		t.Errorf("default traverse leaked low-confidence edges: %+v", sub.Edges)
	}
	sub, err = st.Traverse(ctx, fx.ents[0].Key, "", "out", 1, 50, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Edges) != 1 || !sub.Edges[0].LowConfidence {
		t.Errorf("opted-in traverse = %+v, want the low-confidence edge", sub.Edges)
	}
}

func TestTraverseMissingStartErrors(t *testing.T) {
	st := graphtest.Store(t)
	if _, err := st.Traverse(context.Background(), "no-such-entity", "", "out", 1, 10, false); err == nil {
		t.Fatal("missing start entity: want error")
	}
}

func TestEnsureTextIndexesIdempotent(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := st.EnsureTextIndexes(ctx); err != nil {
			t.Fatalf("EnsureTextIndexes run %d: %v", i+1, err)
		}
	}
	for _, ix := range []struct {
		label string
		props []string
	}{
		{"Entity", []string{"name", "aliases"}},
		{"Chunk", []string{"text"}},
	} {
		if !rawTextIndexExists(t, uri, ix.label, ix.props) {
			t.Errorf("text index on :%s(%v) missing", ix.label, ix.props)
		}
	}
}

// rawTextIndexExists queries SHOW INDEX INFO directly with independent
// matching for a text index (type contains "text").
func rawTextIndexExists(t *testing.T, uri, label string, props []string) bool {
	t.Helper()
	for _, r := range rawRecords(t, uri, "SHOW INDEX INFO") {
		tv, _ := r.Get("index type")
		if !strings.Contains(fmt.Sprint(tv), "text") {
			continue
		}
		lv, _ := r.Get("label")
		if fmt.Sprint(lv) != label {
			continue
		}
		pv, ok := r.Get("property")
		if !ok {
			pv, _ = r.Get("properties")
		}
		var list []string
		switch p := pv.(type) {
		case string:
			list = []string{p}
		case []any:
			for _, e := range p {
				list = append(list, fmt.Sprint(e))
			}
		}
		if len(list) != len(props) {
			continue
		}
		set := map[string]bool{}
		for _, e := range list {
			set[e] = true
		}
		all := true
		for _, want := range props {
			if !set[want] {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}
