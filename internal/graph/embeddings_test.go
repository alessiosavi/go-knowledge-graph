package graph_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

// testEmbeddingDim matches the production default destination; the shared
// dev database keeps one entity_vec index across runs.
const testEmbeddingDim = 768

func TestEnsureVectorIndexIdempotent(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	if err := st.EnsureVectorIndex(ctx, testEmbeddingDim); err != nil {
		t.Fatalf("first EnsureVectorIndex: %v", err)
	}
	if err := st.EnsureVectorIndex(ctx, testEmbeddingDim); err != nil {
		t.Fatalf("second EnsureVectorIndex (idempotent): %v", err)
	}

	res, err := st.RunReadQuery(ctx, "SHOW VECTOR INDEX INFO", nil, 0)
	if err != nil {
		t.Fatalf("SHOW VECTOR INDEX INFO: %v", err)
	}
	var dim int64
	for _, row := range res.Rows {
		if row["index_name"] == "entity_vec" {
			dim, _ = row["dimension"].(int64)
		}
	}
	if dim == 0 {
		t.Fatalf("entity_vec not visible in SHOW VECTOR INDEX INFO: %v", res.Rows)
	}
	if err := st.EnsureVectorIndex(ctx, int(dim)+1); err == nil {
		t.Error("dimension mismatch on an existing index: want an error")
	}
}

func TestSetEntityEmbeddingAndVectorSearch(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	if err := st.EnsureVectorIndex(ctx, testEmbeddingDim); err != nil {
		t.Fatalf("EnsureVectorIndex: %v", err)
	}

	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	name := "embedprobe" + tag
	docRes := testUpsert(t, st, "file", uniqueSource(t), "hash-a")
	text := "the " + name + " waits"
	if err := st.PutChunks(ctx, docRes.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	key := graph.EntityKey("thing", name)
	if err := st.UpsertExtraction(ctx, docRes.ID, docRes.ID+"-c0", "mock:1",
		[]graph.EntityUpsert{{Key: key, Name: name, Type: "thing"}}, nil); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}

	id := graph.EntityID(key)
	vec := graphtest.Vector(name, testEmbeddingDim)
	if err := st.SetEntityEmbedding(ctx, id, vec); err != nil {
		t.Fatalf("SetEntityEmbedding: %v", err)
	}
	ids, err := st.EntityVectorSearch(ctx, vec, 5)
	if err != nil {
		t.Fatalf("EntityVectorSearch: %v", err)
	}
	if !slices.Contains(ids, id) {
		t.Errorf("vector search = %v, want the embedded entity %s", ids, id)
	}

	if err := st.SetEntityEmbedding(ctx, "00000000", vec); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("missing entity error = %v, want ErrNotFound", err)
	}
}

// TestEntityDeleteClearsEmbedding pins that deleting a document clears its
// relationless entities' embeddings, so the vector index drops their
// entries: a deleted entity must never linger as a stale hit.
func TestEntityDeleteClearsEmbedding(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	if err := st.EnsureVectorIndex(ctx, testEmbeddingDim); err != nil {
		t.Fatalf("EnsureVectorIndex: %v", err)
	}

	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	name := "delprobe" + tag
	other := "delprobe-other" + tag
	docRes := testUpsert(t, st, "file", uniqueSource(t), "hash-a")
	text := "the " + name + " waits"
	if err := st.PutChunks(ctx, docRes.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	key := graph.EntityKey("thing", name)
	otherKey := graph.EntityKey("thing", other)
	// A fact creates the MENTIONS edges that make the entities part of the
	// document's subtree (a factless entity only global GC can reach).
	if err := st.UpsertExtraction(ctx, docRes.ID, docRes.ID+"-c0", "mock:1",
		[]graph.EntityUpsert{
			{Key: key, Name: name, Type: "thing"},
			{Key: otherKey, Name: other, Type: "thing"},
		},
		[]graph.FactUpsert{{
			HeadKey: key, TailKey: otherKey, Rel: "waits", Confidence: 0.9,
			HeadMention: graph.Span{Start: 4, End: 4 + len(name), Text: name},
			TailMention: graph.Span{Start: len(text) - 5, End: len(text), Text: "waits"},
		}}); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	id := graph.EntityID(key)
	vec := graphtest.Vector(name, testEmbeddingDim)
	if err := st.SetEntityEmbedding(ctx, id, vec); err != nil {
		t.Fatalf("SetEntityEmbedding: %v", err)
	}
	ids, err := st.EntityVectorSearch(ctx, vec, 5)
	if err != nil {
		t.Fatalf("vector search (seeding check): %v", err)
	}
	if !slices.Contains(ids, id) {
		t.Fatalf("seeding check failed: %v does not contain %s", ids, id)
	}

	if err := st.DeleteDocument(ctx, docRes.ID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	ids, err = st.EntityVectorSearch(ctx, vec, 5)
	if err != nil {
		t.Fatalf("vector search after delete: %v", err)
	}
	if slices.Contains(ids, id) {
		t.Errorf("deleted entity %s still returned by the vector index: %v", id, ids)
	}
}

// TestSetEntityEmbeddingsBatch pins the batched write path: one call stores
// several entities' embeddings and reports how many ids were missing.
func TestSetEntityEmbeddingsBatch(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	if err := st.EnsureVectorIndex(ctx, testEmbeddingDim); err != nil {
		t.Fatalf("EnsureVectorIndex: %v", err)
	}

	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	docRes := testUpsert(t, st, "file", uniqueSource(t), "hash-a")
	text := "the batch waits"
	if err := st.PutChunks(ctx, docRes.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	names := []string{"batchA" + tag, "batchB" + tag, "batchC" + tag}
	ents := make([]graph.EntityUpsert, 0, len(names))
	for _, name := range names {
		ents = append(ents, graph.EntityUpsert{Key: graph.EntityKey("thing", name), Name: name, Type: "thing"})
	}
	if err := st.UpsertExtraction(ctx, docRes.ID, docRes.ID+"-c0", "mock:1", ents, nil); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}

	ids := make([]string, 0, len(names))
	vecs := make([][]float64, 0, len(names))
	for _, name := range names {
		ids = append(ids, graph.EntityID(graph.EntityKey("thing", name)))
		vecs = append(vecs, graphtest.Vector(name, testEmbeddingDim))
	}
	missing, err := st.SetEntityEmbeddings(ctx, ids, vecs)
	if err != nil {
		t.Fatalf("SetEntityEmbeddings: %v", err)
	}
	if missing != 0 {
		t.Errorf("missing = %d, want 0", missing)
	}
	qr, err := st.RunReadQuery(ctx,
		fmt.Sprintf("MATCH (e:Entity) WHERE e.id IN ['%s'] RETURN count(e) AS total, count(e.embedding) AS embedded", strings.Join(ids, "', '")), nil, 0)
	if err != nil {
		t.Fatalf("embedding read: %v", err)
	}
	if len(qr.Rows) != 1 || qr.Rows[0]["total"] != int64(3) || qr.Rows[0]["embedded"] != int64(3) {
		t.Errorf("counts = %v, want 3 entities with 3 embeddings", qr.Rows)
	}

	// A stale id is reported, not silently dropped.
	missing, err = st.SetEntityEmbeddings(ctx, append(ids, "00000000"), append(vecs, vecs[0]))
	if err != nil {
		t.Fatalf("batch with stale id: %v", err)
	}
	if missing != 1 {
		t.Errorf("missing = %d, want 1 for the stale id", missing)
	}

	// Preconditions are checked before any write.
	if _, err := st.SetEntityEmbeddings(ctx, ids, vecs[:1]); err == nil {
		t.Error("length mismatch: want error")
	}
	if _, err := st.SetEntityEmbeddings(ctx, []string{ids[0]}, [][]float64{{}}); err == nil {
		t.Error("empty vector: want error")
	}
}

// readEmbeddingSpace reads the marker node directly (not through the
// setter), so the roundtrip test can discover the pre-existing value on
// the shared persistent dev database.
func readEmbeddingSpace(t *testing.T, st *graph.Store) string {
	t.Helper()
	res, err := st.RunReadQuery(context.Background(),
		"MATCH (m:GkgMeta {id: 'embedding_space'}) RETURN coalesce(m.space, '') AS space", nil, 0)
	if err != nil {
		t.Fatalf("read embedding space: %v", err)
	}
	if len(res.Rows) == 0 {
		return ""
	}
	s, _ := res.Rows[0]["space"].(string)
	return s
}

// TestSetEmbeddingSpaceRoundtripAndPrevious is state-agnostic (the dev DB
// carries the marker across runs): it compares against the pre-read value
// instead of assuming a fresh database.
func TestSetEmbeddingSpaceRoundtripAndPrevious(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	pre := readEmbeddingSpace(t, st)
	spaceA := "test-space-a-" + tag
	prev, err := st.SetEmbeddingSpace(ctx, spaceA)
	if err != nil {
		t.Fatalf("SetEmbeddingSpace(A): %v", err)
	}
	if prev != pre {
		t.Errorf("previous = %q, want the pre-existing value %q", prev, pre)
	}
	if got := readEmbeddingSpace(t, st); got != spaceA {
		t.Errorf("stored space = %q, want %q", got, spaceA)
	}

	prev, err = st.SetEmbeddingSpace(ctx, spaceA)
	if err != nil {
		t.Fatalf("SetEmbeddingSpace(A again): %v", err)
	}
	if prev != spaceA {
		t.Errorf("repeat previous = %q, want %q", prev, spaceA)
	}

	spaceB := "test-space-b-" + tag
	prev, err = st.SetEmbeddingSpace(ctx, spaceB)
	if err != nil {
		t.Fatalf("SetEmbeddingSpace(B): %v", err)
	}
	if prev != spaceA {
		t.Errorf("switch previous = %q, want %q", prev, spaceA)
	}
}

// TestSetEmbeddingSpaceConcurrent pins the single-marker contract under
// concurrency: the unique constraint plus a retry keep every concurrent
// call succeeding, with exactly one :GkgMeta node left behind.
func TestSetEmbeddingSpaceConcurrent(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = st.SetEmbeddingSpace(ctx, fmt.Sprintf("race-%d-%s", i, tag))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("SetEmbeddingSpace[%d]: %v", i, err)
		}
	}
	res, err := st.RunReadQuery(ctx, "MATCH (m:GkgMeta {id: 'embedding_space'}) RETURN count(m) AS n", nil, 0)
	if err != nil {
		t.Fatalf("marker count: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["n"] != int64(1) {
		t.Errorf("marker nodes = %v, want exactly 1", res.Rows)
	}
}

// TestBootstrapDeduplicatesGkgMeta pins the pre-constraint cleanup: a
// database that accumulated duplicate marker nodes (a pre-constraint race)
// is repaired by Bootstrap before the unique constraint is created.
func TestBootstrapDeduplicatesGkgMeta(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()

	t.Cleanup(func() {
		if err := st.Bootstrap(context.Background()); err != nil {
			t.Errorf("restore bootstrap: %v", err)
		}
	})

	// Precondition: drop the constraint and plant duplicates.
	if rawUniqueConstraintExists(t, uri, "GkgMeta", "id") {
		rawRun(t, uri, "DROP CONSTRAINT ON (n:GkgMeta) ASSERT n.id IS UNIQUE")
	}
	rawRun(t, uri, "MATCH (m:GkgMeta {id: 'embedding_space'}) DELETE m")
	rawRun(t, uri, "CREATE (:GkgMeta {id: 'embedding_space', space: 'dup-a'})")
	rawRun(t, uri, "CREATE (:GkgMeta {id: 'embedding_space', space: 'dup-b'})")

	if err := st.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap with duplicates present: %v", err)
	}
	res, err := st.RunReadQuery(ctx, "MATCH (m:GkgMeta {id: 'embedding_space'}) RETURN count(m) AS n", nil, 0)
	if err != nil {
		t.Fatalf("marker count: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["n"] != int64(1) {
		t.Errorf("marker nodes = %v, want the duplicates pruned to 1", res.Rows)
	}
	if !rawUniqueConstraintExists(t, uri, "GkgMeta", "id") {
		t.Error("Bootstrap did not create the unique constraint on (GkgMeta).id")
	}
}
