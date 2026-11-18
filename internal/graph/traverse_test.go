package graph_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestEntitySearchRowsByID(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	prefix := fmt.Sprintf("batch-read-%d", time.Now().UnixNano())
	first, second := prefix+"-first", prefix+"-second"
	rawRun(t, uri, fmt.Sprintf(`CREATE (:Entity {id:%q, key:%q, name:'First entity', type:'person', domain:'alpha', mention_count:3}),
	                                  (:Entity {id:%q, key:%q, name:'Second entity', type:'place', domain:'beta', mention_count:7})`, first, first, second, second))
	t.Cleanup(func() {
		rawRun(t, uri, fmt.Sprintf(`MATCH (e:Entity) WHERE e.id IN [%q, %q] DETACH DELETE e`, first, second))
	})

	rows, err := st.EntitySearchRowsByID(t.Context(), []string{prefix + "-missing", first, second, first})
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[graph.EntitySearchRow]int)
	for _, row := range rows {
		got[row]++
	}
	want := map[graph.EntitySearchRow]int{
		{ID: first, Name: "First entity", Type: "person", Domain: "alpha", MentionCount: 3}: 2,
		{ID: second, Name: "Second entity", Type: "place", Domain: "beta", MentionCount: 7}: 1,
	}
	if !maps.Equal(got, want) {
		t.Errorf("EntitySearchRowsByID row counts = %+v, want %+v", got, want)
	}
}

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

func TestTraverseRanksEvidenceBeforeTakingShare(t *testing.T) {
	f := graphtest.MixedVisibility(t)
	// The low-confidence row has the highest numeric score. The new uses rows
	// tie the existing one on confidence and relation, but have an earlier key.
	rawRun(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (a:Entity {key:'%s'}), (b:Entity {key:'%s'})
CREATE (a)-[:a_low {doc_id:'%s', chunk_id:'%s', confidence:0.99, low_confidence:true}]->(b),
       (a)-[:uses {doc_id:'%s', chunk_id:'%s-c1', confidence:0.8}]->(b),
       (a)-[:uses {doc_id:'%s', chunk_id:'%s', confidence:0.8}]->(b)`,
		f.Key, f.ThirdKey, f.DocID, f.ChunkID, f.DocID, f.DocID, f.DocID, f.ChunkID))
	want := []string{"works_at", "advises", "consults", "uses", "uses", "uses", "a_low", "evaluates"}
	for _, limit := range []int{5, 50} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			sub, err := f.Store.Traverse(context.Background(), f.ID, "", "out", 1, limit, true)
			if err != nil {
				t.Fatal(err)
			}
			var rels []string
			for _, e := range sub.Edges {
				rels = append(rels, e.Rel)
			}
			if expected := want[:min(limit, len(want))]; !slices.Equal(rels, expected) {
				t.Fatalf("ranked relations = %v, want %v", rels, expected)
			}
			for i, chunkID := range []string{f.ChunkID, f.DocID + "-c1"} {
				if e := sub.Edges[3+i]; e.To != f.ThirdKey || e.ChunkID != chunkID {
					t.Errorf("tied edge %d = %+v, want endpoint %s and chunk %s", i, e, f.ThirdKey, chunkID)
				}
			}
		})
	}
}

func TestTraverseBatchesSecondHop(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	const scope = "traverse-scope"
	const otherDomain = "traverse-other"
	doc := testUpsert(t, st, "file", uniqueSource(t), "traverse-batch").ID
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, doc); err != nil {
			t.Error(err)
		}
	})
	if err := st.PutChunks(ctx, doc, []graph.Chunk{{Seq: 0, Text: "Typed evidence"}, {Seq: 1, Text: "Candidate evidence"}}); err != nil {
		t.Fatal(err)
	}
	var ents []graph.EntityUpsert
	entity := func(name string) string {
		key := graph.DomainKey(scope, name+"-"+doc)
		ents = append(ents, graph.EntityUpsert{Key: key, Name: name, Type: "place"})
		return key
	}
	root := entity("root")
	branches := []string{entity("z-hub"), entity("a-branch"), entity("b-branch")}
	candidate, candidateLeaf := entity("candidate"), entity("candidate-leaf")
	facts := []graph.FactUpsert{
		{HeadKey: root, TailKey: branches[0], Rel: "links", Confidence: .95},
		{HeadKey: root, TailKey: branches[1], Rel: "links", Confidence: .85},
		{HeadKey: branches[2], TailKey: root, Rel: "links", Confidence: 1},
	}
	for i, leaves := range []int{12, 9, 8} {
		for j := range leaves {
			leaf := entity(fmt.Sprintf("leaf-%d-%02d", i, j))
			facts = append(facts, graph.FactUpsert{HeadKey: branches[i], TailKey: leaf, Rel: "links", Confidence: .9})
		}
	}
	facts = append(facts, graph.FactUpsert{HeadKey: candidate, TailKey: candidateLeaf, Rel: "candidate_bridge", Confidence: .9})
	if err := st.UpsertExtractionOpts(ctx, doc, doc+"-c0", "mock:1", ents, facts, graph.WriteOptions{Domain: scope}); err != nil {
		t.Fatal(err)
	}
	var candidates []graph.FactUpsert
	for _, from := range []string{root, branches[1], branches[2]} {
		candidates = append(candidates, graph.FactUpsert{HeadKey: from, TailKey: candidate, Rel: "pending", Confidence: .8, JudgeInput: []byte("Candidate evidence")})
	}
	if err := st.UpsertExtractionOpts(ctx, doc, doc+"-c1", "mock:1", ents, candidates, graph.WriteOptions{Domain: scope, FactsAsCandidates: true}); err != nil {
		t.Fatal(err)
	}
	rawRun(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (n:Entity {key:'%s'}) SET n.domain='%s'`, branches[0], otherDomain))

	start := graph.EntityID(root)
	t.Run("fair budget", func(t *testing.T) {
		for _, tc := range []struct {
			limit  int
			counts []int
		}{
			{15, []int{5, 5, 2}}, // ceil(12 / 3) raised to the minimum of five.
			{22, []int{7, 7, 5}}, // ceil(19 / 3) rounded up to seven.
		} {
			sub, err := st.TraverseOpts(ctx, start, "links", "both", 2, tc.limit, graph.ViewOptions{Domain: scope})
			if err != nil || len(sub.Edges) != tc.limit {
				t.Fatalf("limit %d: got %d edges, err = %v", tc.limit, len(sub.Edges), err)
			}
			if got := []string{sub.Edges[0].To, sub.Edges[1].To, sub.Edges[2].From}; !slices.Equal(got, branches) {
				t.Errorf("first hop neighbours = %v, want %v", got, branches)
			}
			var got, want []string
			for _, e := range sub.Edges[len(branches):] {
				got = append(got, e.From)
			}
			for i, count := range tc.counts {
				for range count {
					want = append(want, branches[i])
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("limit %d: second hop expanded %v, want %v", tc.limit, got, want)
			}
		}
	})

	t.Run("back edge leaves a full share", func(t *testing.T) {
		// The incoming first-hop edge from b-branch ranks first in its
		// outgoing second-hop rows. All three branches should still get five.
		sub, err := st.Traverse(ctx, start, "links", "both", 2, 18, false)
		if err != nil || len(sub.Edges) != 18 {
			t.Fatalf("got %d edges, want 18, err = %v", len(sub.Edges), err)
		}
		var fromB int
		for _, e := range sub.Edges[3:] {
			if e.From == branches[2] {
				fromB++
			}
		}
		if fromB != 5 {
			t.Errorf("second hop returned %d new b-branch edges, want 5", fromB)
		}
	})

	t.Run("query count", func(t *testing.T) {
		queries := graph.CaptureReadQueries(st, func() {
			_, err := st.TraverseOpts(ctx, start, "", "both", 2, 35, graph.ViewOptions{IncludeUnverified: true})
			if err != nil {
				t.Fatal(err)
			}
		})
		var typedSizes, candidateSizes []int
		for _, q := range queries {
			if ids, ok := q.Params["ids"].([]string); ok {
				typedSizes = append(typedSizes, len(ids))
			}
			if anchors, ok := q.Params["anchors"].([]map[string]any); ok {
				candidateSizes = append(candidateSizes, len(anchors))
			}
		}
		wantTyped, wantCandidates := []int{1, 1, 3, 3, 1}, []int{1, 1, 2, 2}
		if len(queries) != 11 || !slices.Equal(typedSizes, wantTyped) || !slices.Equal(candidateSizes, wantCandidates) {
			t.Errorf("reads = %d, typed batch sizes = %v, candidate batch sizes = %v; want 2 setup reads, typed batches %v and candidate batches %v",
				len(queries), typedSizes, candidateSizes, wantTyped, wantCandidates)
		}
	})

	t.Run("candidate slots and domain", func(t *testing.T) {
		// Four first-hop rows leave 30 slots. The typed passes use 29 of
		// them, so the first eligible branch gets the remaining candidate slot.
		opt := graph.ViewOptions{Domain: scope, IncludeUnverified: true}
		sub, err := st.TraverseOpts(ctx, start, "", "both", 2, 34, opt)
		if err != nil {
			t.Fatal(err)
		}
		if sub.Hidden != (graph.HiddenCounts{Unjudged: 1}) {
			t.Errorf("start hidden counts = %+v, want one unjudged fact", sub.Hidden)
		}
		if len(sub.Edges) < 4 || sub.Edges[2].From != branches[2] || sub.Edges[2].To != root || !sub.Edges[3].Unverified {
			t.Fatalf("first hop edges = %+v, want the incoming typed edge before candidates", sub.Edges[:min(4, len(sub.Edges))])
		}
		var candidateSources []string
		for _, e := range sub.Edges {
			if e.Unverified {
				candidateSources = append(candidateSources, e.From)
			}
		}
		if want := []string{root, branches[1]}; !slices.Equal(candidateSources, want) {
			t.Errorf("candidate sources = %v, want %v", candidateSources, want)
		}
		nodes := make(map[string]string, len(sub.Nodes))
		for _, n := range sub.Nodes {
			nodes[n.Key] = n.Domain
		}
		if nodes[candidate] != scope || nodes[branches[0]] != otherDomain {
			t.Errorf("candidate and hub domains = %q, %q; want %q, %q", nodes[candidate], nodes[branches[0]], scope, otherDomain)
		}
		if _, ok := nodes[candidateLeaf]; ok {
			t.Error("traversal expanded through a candidate-only endpoint")
		}
		opt.Domain = otherDomain
		if _, err := st.TraverseOpts(ctx, start, "", "both", 2, 34, opt); !errors.Is(err, graph.ErrNotFound) {
			t.Errorf("mismatched start domain: got %v, want ErrNotFound", err)
		}
	})
}

func TestTraverseTopsUpCappedFrontier(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	doc := testUpsert(t, st, "file", uniqueSource(t), "traverse-top-up").ID
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, doc); err != nil {
			t.Error(err)
		}
	})
	if err := st.PutChunks(ctx, doc, []graph.Chunk{{Seq: 0, Text: "Typed evidence"}}); err != nil {
		t.Fatal(err)
	}
	var ents []graph.EntityUpsert
	entity := func(name string) string {
		key := graph.EntityKey(name + "-" + doc)
		ents = append(ents, graph.EntityUpsert{Key: key, Name: name, Type: "place"})
		return key
	}
	root := entity("root")
	var facts []graph.FactUpsert
	var hubLeaves []string
	for i := range 10 {
		branch := entity(fmt.Sprintf("branch-%02d", i))
		facts = append(facts, graph.FactUpsert{HeadKey: root, TailKey: branch, Rel: "links", Confidence: .9})
		leaves := 1
		if i == 0 {
			leaves = 100
		}
		for j := range leaves {
			leaf := entity(fmt.Sprintf("leaf-%02d-%02d", i, j))
			facts = append(facts, graph.FactUpsert{HeadKey: branch, TailKey: leaf, Rel: "links", Confidence: .9})
			if i == 0 {
				hubLeaves = append(hubLeaves, leaf)
			}
		}
	}
	if err := st.UpsertExtraction(ctx, doc, doc+"-c0", "mock:1", ents, facts); err != nil {
		t.Fatal(err)
	}
	sub, err := st.Traverse(ctx, graph.EntityID(root), "links", "out", 2, 50, false)
	if err != nil || len(sub.Edges) != 50 {
		t.Fatalf("got %d edges, want 50, err = %v", len(sub.Edges), err)
	}
	// Ten first-hop rows and 14 first-pass rows leave 26 top-up slots.
	for i, e := range sub.Edges[24:] {
		if want := hubLeaves[5+i]; e.To != want {
			t.Fatalf("top-up edge %d ends at %q, want %q", i, e.To, want)
		}
	}
}

func TestChunkTextSearchPunctuation(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	docID := testUpsert(t, st, "file", uniqueSource(t), "hash-search-punctuation").ID
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, docID); err != nil {
			t.Error(err)
		}
	})
	// Hit assertions use invented words that no other fixture contains, so a
	// shared test database cannot crowd the fixture out of the index's top
	// 1,000 hits; queries made of common words only check for errors.
	// The text holds a precomposed "è" (Zorbèllo) and a decomposed one
	// (Quillore + U+0300 + x): queries are not normalized, so each is found
	// by its own spelling.
	text := "The l'Acmezorb-Maxquill valve and Zorbquill's pump meet at a:b. Not here. Zorbèllo. Quillore\u0300x. İzorbul Ⅻ."
	if err := st.PutChunks(ctx, docID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	for _, tt := range []struct {
		query   string
		wantHit bool
	}{
		{"l'Acmezorb-Maxquill", true},
		{"Zorbquill's", true},
		{"a:b", false},
		{"(x", false},
		{`"x`, false},
		{"x AND", false},
		{"Or not", false},
		{"*", false},
		{"  Caffè  Ünïcode ", false},
		{"C++ / Go", false},
		{"x and", false},
		{"not", false},
		{"x aNd", false},
		{"oR nOt", false},
		{"Zorbquill AND", true},
		{"zorbquill or not", true},
		{"Zorbèllo", true},
		{"ZORBÈLLO", true},
		{"Quillore\u0300x", true},
		{"İzorbul", true},
		{"İZORBUL", true},
		{"Ⅻ", true},
	} {
		t.Run(tt.query, func(t *testing.T) {
			rows, err := st.ChunkTextSearch(ctx, tt.query, 1000)
			if err != nil {
				t.Fatalf("ChunkTextSearch(%q): %v", tt.query, err)
			}
			if tt.query == "*" && len(rows) != 0 {
				t.Errorf("punctuation-only query returned %d hits, want 0", len(rows))
			}
			if tt.wantHit {
				for _, row := range rows {
					if row.ID == docID+"-c0" {
						return
					}
				}
				t.Errorf("ChunkTextSearch(%q) missing fixture chunk %s-c0", tt.query, docID)
			}
		})
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
		{"Entity", []string{"name", "domain_tok"}},
		{"Chunk", []string{"text", "domain_tok"}},
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

// TestChunkTextSearchLimitAndOrder pins that the hit list is cut to limit
// by score (the cut now happens before the Document join) and that the
// join still fills the title.
func TestChunkTextSearchLimitAndOrder(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	docID := testUpsert(t, st, "file", uniqueSource(t), "hash-a").ID
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, docID); err != nil {
			t.Error(err)
		}
	})
	token := fmt.Sprintf("quarkchunk%d", time.Now().UnixNano())
	filler := strings.Repeat("Background filler sentence about something else. ", 6)
	texts := []string{
		filler + token + " appears once here.",
		token + " " + token + " " + token,
		filler + token + " appears once again.",
		filler + "and " + token + " once more.",
	}
	chunks := make([]graph.Chunk, len(texts))
	for i, text := range texts {
		chunks[i] = graph.Chunk{Seq: i, Text: text, Start: 0, End: len(text)}
	}
	if err := st.PutChunks(ctx, docID, chunks); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	rows, err := st.ChunkTextSearch(ctx, token, 2)
	if err != nil {
		t.Fatalf("ChunkTextSearch: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d (%+v), want 2", len(rows), rows)
	}
	if want := docID + "-c1"; rows[0].ID != want {
		t.Errorf("top hit = %s, want the densest chunk %s", rows[0].ID, want)
	}
	for _, r := range rows {
		if r.DocID != docID || r.Title != "title" {
			t.Errorf("row %+v: want doc %s with title %q", r, docID, "title")
		}
	}
}
