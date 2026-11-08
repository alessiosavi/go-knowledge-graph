package graph_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestEntityKeyNormalization(t *testing.T) {
	cases := []struct{ typ, a, b string }{
		{"person", "Città", "città"},
		{"person", "Città", "Citta\u0300"}, // NFC vs NFD
		{"person", "  New   York  ", "new york"},
		{"thing", "«Città».", "città"}, // edge punctuation stripped
		{"person", "ADA  LOVELACE", "ada lovelace"},
	}
	for _, c := range cases {
		ka := graph.EntityKey(c.typ, c.a)
		kb := graph.EntityKey(c.typ, c.b)
		if ka != kb {
			t.Errorf("EntityKey(%s, %q) = %q, EntityKey(%s, %q) = %q; want equal", c.typ, c.a, ka, c.typ, c.b, kb)
		}
	}
	if got := graph.EntityKey("person", "Città"); got != "person:città" {
		t.Errorf("EntityKey(person, Città) = %q, want person:città", got)
	}
	if graph.EntityKey("person", "paris") == graph.EntityKey("place", "paris") {
		t.Error("type must discriminate keys")
	}
	id1 := graph.EntityID(graph.EntityKey("person", "Ada"))
	id2 := graph.EntityID(graph.EntityKey("person", "Ada"))
	if id1 != id2 || len(id1) != 32 {
		t.Errorf("EntityID not deterministic or wrong length: %q vs %q", id1, id2)
	}
	if graph.EntityID("k1") == graph.EntityID("k2") {
		t.Error("distinct keys must map to distinct ids")
	}
}

// entityFixture is a fresh two-entity fact whose entity names carry a
// per-run tag: entity keys merge globally by design, so tests on the shared
// dev database must not reuse another run's entities.
type entityFixture struct {
	ents []graph.EntityUpsert
	fact graph.FactUpsert
}

func newFixture(t *testing.T) entityFixture {
	t.Helper()
	tag := strconv.FormatInt(time.Now().UnixNano(), 36)
	headName := "Ada Lovelace " + tag
	tailName := "algorithm " + tag
	ents := []graph.EntityUpsert{
		{Key: graph.EntityKey("person", headName), Name: headName, Type: "person"},
		{Key: graph.EntityKey("work", tailName), Name: tailName, Type: "work"},
	}
	return entityFixture{
		ents: ents,
		fact: graph.FactUpsert{
			HeadKey: ents[0].Key, TailKey: ents[1].Key, Rel: "wrote",
			Confidence:  0.9,
			HeadMention: graph.Span{Start: 0, End: 12, Text: "Ada Lovelace"},
			TailMention: graph.Span{Start: 23, End: 42, Text: "the first algorithm"},
		},
	}
}

// entitiesTestDoc creates a document with one chunk carrying text and
// returns (docID, chunkID).
func entitiesTestDoc(t *testing.T, st *graph.Store, text string) (string, string) {
	t.Helper()
	res := testUpsert(t, st, "file", uniqueSource(t), "hash-a")
	chunks := []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}
	if err := st.PutChunks(context.Background(), res.ID, chunks); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	return res.ID, fmt.Sprintf("%s-c0", res.ID)
}

func TestUpsertExtractionCreatesMentionsAndFacts(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	text := "Ada Lovelace wrote the first algorithm."
	docID, chunkID := entitiesTestDoc(t, st, text)
	fx := newFixture(t)

	if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:1", fx.ents, []graph.FactUpsert{fx.fact}); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}

	view, err := st.EntityView(ctx, fx.ents[0].Key, false)
	if err != nil {
		t.Fatalf("EntityView: %v", err)
	}
	if view.Key != fx.ents[0].Key || view.Name != fx.ents[0].Name || view.Type != "person" {
		t.Errorf("view identity = %+v", view.EntityRef)
	}
	if view.MentionCount != 1 {
		t.Errorf("MentionCount = %d, want 1", view.MentionCount)
	}
	if len(view.Facts) != 1 {
		t.Fatalf("facts = %d, want 1", len(view.Facts))
	}
	f := view.Facts[0]
	if f.Rel != "wrote" || f.Other.Key != fx.ents[1].Key || f.Confidence != 0.9 {
		t.Errorf("fact = %+v", f)
	}
	if f.DocID != docID || f.ChunkID != chunkID {
		t.Errorf("fact provenance = doc %q chunk %q, want %q %q", f.DocID, f.ChunkID, docID, chunkID)
	}
	if !strings.Contains(f.Excerpt, "Ada Lovelace") {
		t.Errorf("excerpt = %q, want it to contain the mention", f.Excerpt)
	}

	// Mentions: one per entity span in this chunk.
	recs := rawRecords(t, uri, fmt.Sprintf("MATCH (c:Chunk {id: '%s'})-[m:MENTIONS]->(:Entity) RETURN count(m) AS n", chunkID))
	if got := testInt64(recs[0], "n"); got != 2 {
		t.Errorf("MENTIONS edges = %d, want 2", got)
	}
}

func TestReupsertChunkReplacesItsFactsOnly(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	docID, chunkID := entitiesTestDoc(t, st, "Ada Lovelace wrote the first algorithm.")
	fx := newFixture(t)

	// Second chunk of the same document with its own fact.
	chunk2ID := fmt.Sprintf("%s-c1", docID)
	if err := st.PutChunks(ctx, docID, []graph.Chunk{{Seq: 1, Text: "notes notebook", Start: 0, End: 14}}); err != nil {
		t.Fatalf("PutChunks c1: %v", err)
	}
	tag := strconv.FormatInt(time.Now().UnixNano(), 36)
	parisFact := graph.FactUpsert{
		HeadKey: graph.EntityKey("place", "Paris "+tag), TailKey: graph.EntityKey("thing", "notebook "+tag),
		Rel: "in", Confidence: 0.8,
		HeadMention: graph.Span{Start: 0, End: 5, Text: "notes"},
		TailMention: graph.Span{Start: 6, End: 14, Text: "notebook"},
	}
	parisEnts := []graph.EntityUpsert{
		{Key: parisFact.HeadKey, Name: "Paris " + tag, Type: "place"},
		{Key: parisFact.TailKey, Name: "notebook " + tag, Type: "thing"},
	}
	if err := st.UpsertExtraction(ctx, docID, chunk2ID, "mock:1", parisEnts, []graph.FactUpsert{parisFact}); err != nil {
		t.Fatalf("UpsertExtraction c1: %v", err)
	}
	if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:1", fx.ents, []graph.FactUpsert{fx.fact}); err != nil {
		t.Fatalf("UpsertExtraction c0: %v", err)
	}

	// Re-extract chunk 0 with a different relation: the old edge must go,
	// chunk 1's edge must survive.
	newFact := fx.fact
	newFact.Rel = "debugged"
	if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:2", fx.ents, []graph.FactUpsert{newFact}); err != nil {
		t.Fatalf("re-UpsertExtraction: %v", err)
	}

	c0 := testInt64(rawRecords(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = '%s' RETURN count(r) AS n", chunkID))[0], "n")
	if c0 != 1 {
		t.Errorf("chunk0 fact edges = %d, want 1", c0)
	}
	c1 := testInt64(rawRecords(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = '%s' RETURN count(r) AS n", chunk2ID))[0], "n")
	if c1 != 1 {
		t.Errorf("chunk1 fact edges = %d, want 1 (must survive chunk0 re-extract)", c1)
	}
	rel := rawRecords(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = '%s' RETURN type(r) AS t", chunkID))[0]
	if got := testString(rel, "t"); got != "debugged" {
		t.Errorf("chunk0 relation = %q, want debugged", got)
	}
}

func TestDuplicateFactsWithinChunkCollapse(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	docID, chunkID := entitiesTestDoc(t, st, "Ada Lovelace wrote the first algorithm.")
	fx := newFixture(t)

	low := fx.fact
	low.Confidence = 0.4
	high := fx.fact
	high.Confidence = 0.9
	if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:1", fx.ents, []graph.FactUpsert{low, high}); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	recs := rawRecords(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = '%s' RETURN count(r) AS n, max(r.confidence) AS c", chunkID))
	if got := testInt64(recs[0], "n"); got != 1 {
		t.Errorf("fact edges = %d, want 1 (duplicates collapse)", got)
	}
	if got := testFloat(recs[0], "c"); got != 0.9 {
		t.Errorf("confidence = %v, want 0.9 (max kept)", got)
	}
}

func TestFactCarriesExtractorVersionAndLowConfidence(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	docID, chunkID := entitiesTestDoc(t, st, "Ada Lovelace wrote the first algorithm.")
	fx := newFixture(t)

	f := fx.fact
	f.Confidence = 0.3
	f.LowConfidence = true
	if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:v9", fx.ents, []graph.FactUpsert{f}); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	r := rawRecords(t, uri, fmt.Sprintf(
		"MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = '%s' RETURN r.extractor_version AS v, r.low_confidence AS l, r.judge AS j", chunkID))[0]
	if got := testString(r, "v"); got != "mock:v9" {
		t.Errorf("extractor_version = %q, want mock:v9", got)
	}
	if got, _ := r.Get("l"); got != true {
		t.Errorf("low_confidence = %v, want true", got)
	}
	if got := testString(r, "j"); got != "" {
		t.Errorf("judge = %q, want empty", got)
	}
}

func TestEntityViewExcludesLowConfidenceByDefault(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	docID, chunkID := entitiesTestDoc(t, st, "Ada Lovelace wrote the first algorithm.")
	fx := newFixture(t)

	f := fx.fact
	f.Confidence = 0.2
	f.LowConfidence = true
	if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:1", fx.ents, []graph.FactUpsert{f}); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}

	view, err := st.EntityView(ctx, fx.ents[0].Key, false)
	if err != nil {
		t.Fatalf("EntityView: %v", err)
	}
	if len(view.Facts) != 0 {
		t.Errorf("default view returned %d low-confidence facts, want 0", len(view.Facts))
	}

	view, err = st.EntityView(ctx, fx.ents[0].Key, true)
	if err != nil {
		t.Fatalf("EntityView(include): %v", err)
	}
	if len(view.Facts) != 1 || !view.Facts[0].LowConfidence {
		t.Errorf("opted-in view = %+v, want the low-confidence fact", view.Facts)
	}
}

func TestEntityViewGroupsFactsWithExcerpt(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	docID, _ := entitiesTestDoc(t, st, "Ada Lovelace wrote the first algorithm.")

	tag := strconv.FormatInt(time.Now().UnixNano(), 36)
	rootName := "Ada Lovelace " + tag
	workName := "algorithm " + tag
	placeName := "Paris " + tag
	root := graph.EntityKey("person", rootName)
	other := graph.EntityKey("work", workName)
	paris := graph.EntityKey("place", placeName)
	ents := []graph.EntityUpsert{
		{Key: root, Name: rootName, Type: "person"},
		{Key: other, Name: workName, Type: "work"},
		{Key: paris, Name: placeName, Type: "place"},
	}
	fact := func(rel, tail string, conf float64) graph.FactUpsert {
		return graph.FactUpsert{
			HeadKey: root, TailKey: tail, Rel: rel, Confidence: conf,
			HeadMention: graph.Span{Start: 0, End: 12, Text: "Ada Lovelace"},
			TailMention: graph.Span{Start: 23, End: 42, Text: "the first algorithm"},
		}
	}
	c0 := docID + "-c0"
	if err := st.UpsertExtraction(ctx, docID, c0, "mock:1", ents, []graph.FactUpsert{
		fact("wrote", other, 0.6),
		fact("visited", paris, 0.7),
	}); err != nil {
		t.Fatalf("UpsertExtraction c0: %v", err)
	}
	// A second chunk re-states the wrote fact: group wrote has size 2.
	c1 := docID + "-c1"
	if err := st.PutChunks(ctx, docID, []graph.Chunk{{Seq: 1, Text: "Ada Lovelace wrote it.", Start: 0, End: 23}}); err != nil {
		t.Fatalf("PutChunks c1: %v", err)
	}
	if err := st.UpsertExtraction(ctx, docID, c1, "mock:1", ents, []graph.FactUpsert{
		fact("wrote", other, 0.9),
	}); err != nil {
		t.Fatalf("UpsertExtraction c1: %v", err)
	}

	view, err := st.EntityView(ctx, root, false)
	if err != nil {
		t.Fatalf("EntityView: %v", err)
	}
	if len(view.Facts) != 3 {
		t.Fatalf("facts = %d, want 3", len(view.Facts))
	}
	if view.Facts[0].Rel != "wrote" || view.Facts[1].Rel != "wrote" {
		t.Errorf("group order: got %q, %q first; want the wrote group (size 2) first", view.Facts[0].Rel, view.Facts[1].Rel)
	}
	for _, f := range view.Facts {
		if f.Excerpt == "" {
			t.Errorf("fact %+v has no excerpt", f)
		}
	}
}

func TestEntityViewByIDAndMissing(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	docID, chunkID := entitiesTestDoc(t, st, "Ada Lovelace wrote the first algorithm.")
	fx := newFixture(t)
	if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:1", fx.ents, []graph.FactUpsert{fx.fact}); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	key := fx.ents[0].Key
	byID, err := st.EntityView(ctx, graph.EntityID(key), false)
	if err != nil {
		t.Fatalf("EntityView by id: %v", err)
	}
	if byID.Key != key {
		t.Errorf("view by id key = %q, want %q", byID.Key, key)
	}
	if _, err := st.EntityView(ctx, "person:does-not-exist", false); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("EntityView(missing) error = %v, want ErrNotFound", err)
	}
}

func TestUpsertExtractionMissingChunk(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	fx := newFixture(t)
	err := st.UpsertExtraction(ctx, "no-doc", "no-doc-c0", "mock:1", fx.ents, []graph.FactUpsert{fx.fact})
	if !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("UpsertExtraction(missing chunk) error = %v, want ErrNotFound", err)
	}
}

// TestUpsertExtractionConcurrentSameEntities pins that concurrent chunk
// merges sharing entities cannot abort on the Entity(key) unique
// constraint (Memgraph aborts one of two concurrent MERGEs at commit).
func TestUpsertExtractionConcurrentSameEntities(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	fx := newFixture(t)
	docID := testUpsert(t, st, "file", uniqueSource(t), "hash-a").ID

	const chunks = 12
	all := make([]graph.Chunk, 0, chunks)
	for i := 0; i < chunks; i++ {
		text := fmt.Sprintf("Ada Lovelace wrote the algorithm %d.", i)
		all = append(all, graph.Chunk{Seq: i, Text: text, Start: 0, End: len(text)})
	}
	if err := st.PutChunks(ctx, docID, all); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}

	start := make(chan struct{})
	errs := make([]error, chunks)
	var wg sync.WaitGroup
	for i := 0; i < chunks; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			chunkID := fmt.Sprintf("%s-c%d", docID, i)
			errs[i] = st.UpsertExtraction(ctx, docID, chunkID, "mock:1", fx.ents, []graph.FactUpsert{fx.fact})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("chunk %d upsert: %v", i, err)
		}
	}
	edges := testInt64(rawRecords(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", docID))[0], "n")
	if edges != chunks {
		t.Errorf("fact edges = %d, want %d", edges, chunks)
	}
	mc := testInt64(rawRecords(t, uri, fmt.Sprintf("MATCH (e:Entity {key: '%s'}) RETURN e.mention_count AS n", fx.ents[0].Key))[0], "n")
	if mc != chunks {
		t.Errorf("mention_count = %d, want %d", mc, chunks)
	}
}

// TestReupsertDroppedEntityRefreshesMentionCount pins that re-extracting a
// chunk which no longer mentions an entity refreshes that entity's
// mention_count too (not only newly touched entities).
func TestReupsertDroppedEntityRefreshesMentionCount(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	docID, c0 := entitiesTestDoc(t, st, "first chunk")
	c1 := docID + "-c1"
	if err := st.PutChunks(ctx, docID, []graph.Chunk{{Seq: 1, Text: "second chunk", Start: 0, End: 12}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}

	tag := strconv.FormatInt(time.Now().UnixNano(), 36)
	e := graph.EntityKey("person", "E "+tag)
	x := graph.EntityKey("work", "X "+tag)
	y := graph.EntityKey("work", "Y "+tag)
	ents := []graph.EntityUpsert{
		{Key: e, Name: "E " + tag, Type: "person"},
		{Key: x, Name: "X " + tag, Type: "work"},
		{Key: y, Name: "Y " + tag, Type: "work"},
	}
	mkFact := func(head, tail string) graph.FactUpsert {
		return graph.FactUpsert{HeadKey: head, TailKey: tail, Rel: "links", Confidence: 0.9,
			HeadMention: graph.Span{Start: 0, End: 5}, TailMention: graph.Span{Start: 6, End: 11}}
	}
	if err := st.UpsertExtraction(ctx, docID, c0, "mock:1", ents, []graph.FactUpsert{mkFact(e, x)}); err != nil {
		t.Fatalf("c0: %v", err)
	}
	if err := st.UpsertExtraction(ctx, docID, c1, "mock:1", ents, []graph.FactUpsert{mkFact(e, y)}); err != nil {
		t.Fatalf("c1: %v", err)
	}

	// Re-extract c0 with unrelated entities: E and X lose their c0 mentions.
	p := graph.EntityKey("thing", "P "+tag)
	q := graph.EntityKey("thing", "Q "+tag)
	newEnts := []graph.EntityUpsert{
		{Key: p, Name: "P " + tag, Type: "thing"},
		{Key: q, Name: "Q " + tag, Type: "thing"},
	}
	if err := st.UpsertExtraction(ctx, docID, c0, "mock:2", newEnts, []graph.FactUpsert{mkFact(p, q)}); err != nil {
		t.Fatalf("re-extract c0: %v", err)
	}

	for _, tc := range []struct {
		key  string
		want int64
	}{{e, 1}, {x, 0}} {
		got := testInt64(rawRecords(t, uri, fmt.Sprintf("MATCH (n:Entity {key: '%s'}) RETURN n.mention_count AS n", tc.key))[0], "n")
		actual := testInt64(rawRecords(t, uri, fmt.Sprintf("MATCH (:Chunk)-[m:MENTIONS]->(n:Entity {key: '%s'}) RETURN count(m) AS n", tc.key))[0], "n")
		if got != tc.want || got != actual {
			t.Errorf("mention_count[%s] = %d (actual MENTIONS edges %d), want %d", tc.key, got, actual, tc.want)
		}
	}
}

func TestUpsertExtractionSkipsEmptyEntityKeys(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	// Shared dev databases may carry empty-key nodes from pre-fix runs.
	if err := rawExec(uri, "MATCH (e:Entity {key: ''}) DETACH DELETE e"); err != nil {
		t.Fatalf("cleanup empty-key entities: %v", err)
	}
	docID, chunkID := entitiesTestDoc(t, st, "text")
	fx := newFixture(t)
	ents := append([]graph.EntityUpsert{{Key: "", Name: "ghost", Type: "thing"}}, fx.ents...)
	if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:1", ents, []graph.FactUpsert{fx.fact}); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	got := testInt64(rawRecords(t, uri, "MATCH (e:Entity {key: ''}) RETURN count(e) AS n")[0], "n")
	if got != 0 {
		t.Errorf("empty-key entities = %d, want 0", got)
	}
}

func TestUpsertExtractionRejectsInvalidFacts(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	docID, chunkID := entitiesTestDoc(t, st, "text")
	fx := newFixture(t)

	cases := []struct {
		name   string
		mutate func(f graph.FactUpsert) graph.FactUpsert
	}{
		{"empty relation", func(f graph.FactUpsert) graph.FactUpsert { f.Rel = ""; return f }},
		{"relation without alphanumeric content", func(f graph.FactUpsert) graph.FactUpsert { f.Rel = "!!!"; return f }},
		{"empty head key", func(f graph.FactUpsert) graph.FactUpsert { f.HeadKey = ""; return f }},
		{"empty tail key", func(f graph.FactUpsert) graph.FactUpsert { f.TailKey = ""; return f }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := tc.mutate(fx.fact)
			if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:1", fx.ents, []graph.FactUpsert{bad}); err == nil {
				t.Fatal("invalid fact: want error")
			}
			edges := testInt64(rawRecords(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = '%s' RETURN count(r) AS n", chunkID))[0], "n")
			if edges != 0 {
				t.Errorf("fact edges after rejected call = %d, want 0", edges)
			}
		})
	}
}

func testString(r *neo4j.Record, key string) string {
	v, _ := r.Get(key)
	s, _ := v.(string)
	return s
}

func testInt64(r *neo4j.Record, key string) int64 {
	v, _ := r.Get(key)
	n, _ := v.(int64)
	return n
}

func testFloat(r *neo4j.Record, key string) float64 {
	v, _ := r.Get(key)
	switch f := v.(type) {
	case float64:
		return f
	case int64:
		return float64(f)
	}
	return 0
}
