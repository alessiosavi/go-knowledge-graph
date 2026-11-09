package graph_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

// supersedePair builds two versions of one source, each with a chunk and an
// extracted fact, and returns (old, new, oldChunkID, newChunkID).
func supersedePair(t *testing.T, st *graph.Store) (graph.UpsertResult, graph.UpsertResult, string, string) {
	t.Helper()
	ctx := context.Background()
	source := uniqueSource(t)
	old := testUpsert(t, st, "file", source, "hash-a")
	newer := testUpsert(t, st, "file", source, "hash-b")
	if newer.PreviousID != old.ID {
		t.Fatalf("second version PreviousID = %q, want %q", newer.PreviousID, old.ID)
	}
	for i, res := range []graph.UpsertResult{old, newer} {
		fx := newFixture(t)
		text := fmt.Sprintf("Ada Lovelace wrote algorithm number %d.", i)
		if err := st.PutChunks(ctx, res.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
			t.Fatalf("PutChunks %d: %v", i, err)
		}
		if err := st.UpsertExtraction(ctx, res.ID, fmt.Sprintf("%s-c0", res.ID), "mock:1", fx.ents, []graph.FactUpsert{fx.fact}); err != nil {
			t.Fatalf("UpsertExtraction %d: %v", i, err)
		}
	}
	return old, newer, old.ID + "-c0", newer.ID + "-c0"
}

func rawCount(t *testing.T, uri, query string) int64 {
	t.Helper()
	return testInt64(rawRecords(t, uri, query)[0], "n")
}

func TestSupersedeRemovesOldFacts(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	old, newer, oldChunk, newChunk := supersedePair(t, st)

	// Entities only mentioned by the old version must vanish with it.
	oldKeys := rawRecords(t, uri, fmt.Sprintf(
		"MATCH (c:Chunk {id: '%s'})-[:MENTIONS]->(e:Entity) RETURN e.key AS key", oldChunk))
	if len(oldKeys) != 2 {
		t.Fatalf("old chunk mentions = %d, want 2", len(oldKeys))
	}
	oldKey := testString(oldKeys[0], "key")

	if err := st.Supersede(ctx, old.ID, newer.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	doc, err := st.Document(ctx, old.ID)
	if err != nil {
		t.Fatalf("old Document: %v", err)
	}
	if doc.Status != graph.StatusSuperseded {
		t.Errorf("old status = %q, want superseded", doc.Status)
	}
	doc, err = st.Document(ctx, newer.ID)
	if err != nil {
		t.Fatalf("new Document: %v", err)
	}
	if doc.Supersedes != old.ID {
		t.Errorf("new supersedes = %q, want %q", doc.Supersedes, old.ID)
	}
	chunks, err := st.Chunks(ctx, old.ID)
	if err != nil {
		t.Fatalf("Chunks(old): %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("old chunks = %d, want 0", len(chunks))
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", old.ID)); n != 0 {
		t.Errorf("old fact edges = %d, want 0", n)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", newer.ID)); n != 1 {
		t.Errorf("new fact edges = %d, want 1 (must survive)", n)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (e:Entity {key: '%s'}) RETURN count(e) AS n", oldKey)); n != 0 {
		t.Errorf("orphan entity %q still present", oldKey)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Chunk {id: '%s'}) RETURN count(*) AS n", newChunk)); n != 1 {
		t.Errorf("new chunk missing after supersede")
	}
}

func TestSupersedeKeepsEntitiesStillMentionedElsewhere(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()

	d1 := testUpsert(t, st, "file", uniqueSource(t), "hash-a")
	d2 := testUpsert(t, st, "file", uniqueSource(t), "hash-b")
	fx := newFixture(t)
	shared := fx.ents[0]

	// d1 has shared + its own tail; d2 has shared + another tail.
	for i, res := range []graph.UpsertResult{d1, d2} {
		tail := graph.EntityKey("work", fmt.Sprintf("work %d %s", i, uniqueSource(t)))
		ents := []graph.EntityUpsert{shared, {Key: tail, Name: tail, Type: "work"}}
		fact := graph.FactUpsert{
			HeadKey: shared.Key, TailKey: tail, Rel: "wrote", Confidence: 0.9,
			HeadMention: graph.Span{Start: 0, End: 3, Text: "Ada"},
			TailMention: graph.Span{Start: 4, End: 8, Text: "work"},
		}
		text := fmt.Sprintf("Ada wrote work %d today.", i)
		if err := st.PutChunks(ctx, res.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
			t.Fatalf("PutChunks %d: %v", i, err)
		}
		if err := st.UpsertExtraction(ctx, res.ID, res.ID+"-c0", "mock:1", ents, []graph.FactUpsert{fact}); err != nil {
			t.Fatalf("UpsertExtraction %d: %v", i, err)
		}
	}

	if err := st.Supersede(ctx, d1.ID, d2.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	if n := rawCount(t, uri, fmt.Sprintf("MATCH (e:Entity {key: '%s'}) RETURN count(e) AS n", shared.Key)); n != 1 {
		t.Errorf("shared entity count = %d, want 1 (still mentioned by the new version)", n)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Chunk)-[m:MENTIONS]->(e:Entity {key: '%s'}) RETURN count(m) AS n", shared.Key)); n != 1 {
		t.Errorf("shared entity MENTIONS = %d, want 1", n)
	}
	mc := rawCount(t, uri, fmt.Sprintf("MATCH (e:Entity {key: '%s'}) RETURN e.mention_count AS n", shared.Key))
	if mc != 1 {
		t.Errorf("shared entity mention_count = %d, want 1", mc)
	}
}

func TestDeleteDocumentLeavesNoOrphans(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	doc := testUpsert(t, st, "file", uniqueSource(t), "hash-a")
	fx := newFixture(t)
	text := "Ada Lovelace wrote the first algorithm."
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	if err := st.UpsertExtraction(ctx, doc.ID, doc.ID+"-c0", "mock:1", fx.ents, []graph.FactUpsert{fx.fact}); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}

	if err := st.DeleteDocument(ctx, doc.ID); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if _, err := st.Document(ctx, doc.ID); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("Document after delete = %v, want ErrNotFound", err)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (c:Chunk {doc_id: '%s'}) RETURN count(c) AS n", doc.ID)); n != 0 {
		t.Errorf("chunks after delete = %d, want 0", n)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", doc.ID)); n != 0 {
		t.Errorf("facts after delete = %d, want 0", n)
	}
	for _, e := range fx.ents {
		if n := rawCount(t, uri, fmt.Sprintf("MATCH (e:Entity {key: '%s'}) RETURN count(e) AS n", e.Key)); n != 0 {
			t.Errorf("orphan entity %q survived delete", e.Key)
		}
	}
	if err := st.DeleteDocument(ctx, "missing-doc"); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("DeleteDocument(missing) = %v, want ErrNotFound", err)
	}
}

func TestSupersedeMissingOldErrorsAndLeavesNewIntact(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	newer := testUpsert(t, st, "file", uniqueSource(t), "hash-b")

	if err := st.Supersede(ctx, "missing-old", newer.ID); err == nil {
		t.Fatal("Supersede(missing old): want error")
	}
	doc, err := st.Document(ctx, newer.ID)
	if err != nil {
		t.Fatalf("Document(new): %v", err)
	}
	if doc.Supersedes != "" || doc.Status != graph.StatusReceived {
		t.Errorf("new document mutated by failed supersede: %+v", doc)
	}
	if err := st.Supersede(ctx, newer.ID, "missing-new"); err == nil {
		t.Fatal("Supersede(missing new): want error")
	}
}

func TestSupersedeRefusesAlreadySuperseded(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	old, newer, _, _ := supersedePair(t, st)
	if err := st.Supersede(ctx, old.ID, newer.ID); err != nil {
		t.Fatalf("first Supersede: %v", err)
	}
	err := st.Supersede(ctx, old.ID, newer.ID)
	if err == nil {
		t.Error("second Supersede(old already superseded): want error")
	} else if !errors.Is(err, graph.ErrSuperseded) {
		t.Errorf("second Supersede error = %v, want it to wrap ErrSuperseded", err)
	}
	if err := st.Supersede(ctx, newer.ID, newer.ID); err == nil {
		t.Error("self-supersede: want error")
	}
}

// TestPutChunksAfterSupersedeRejected pins that a stale worker cannot
// re-attach chunks (and thereby facts) to a superseded document.
func TestPutChunksAfterSupersedeRejected(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	old, newer, _, _ := supersedePair(t, st)
	if err := st.Supersede(ctx, old.ID, newer.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	err := st.PutChunks(ctx, old.ID, []graph.Chunk{{Seq: 0, Text: "stale text", Start: 0, End: 10}})
	if !errors.Is(err, graph.ErrSuperseded) {
		t.Fatalf("PutChunks on superseded document = %v, want ErrSuperseded", err)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (c:Chunk {doc_id: '%s'}) RETURN count(c) AS n", old.ID)); n != 0 {
		t.Errorf("chunks re-attached to superseded document: %d", n)
	}
}

// TestPutChunksConcurrentSupersedeNoResurrection pins that concurrent
// PutChunks and Supersede on one document cannot leave chunks attached to
// the superseded version: the writer either commits before the supersede
// (its chunks are then deleted) or observes the supersede atomically and is
// rejected with ErrSuperseded.
func TestPutChunksConcurrentSupersedeNoResurrection(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	old, newer, _, _ := supersedePair(t, st)

	done := make(chan error, 1)
	go func() {
		done <- st.PutChunks(ctx, old.ID, []graph.Chunk{{Seq: 7, Text: "late", Start: 0, End: 4}})
	}()
	time.Sleep(20 * time.Millisecond)
	if err := st.Supersede(ctx, old.ID, newer.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	err := <-done
	if err != nil && !errors.Is(err, graph.ErrSuperseded) {
		t.Fatalf("racing PutChunks = %v, want nil or ErrSuperseded", err)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (c:Chunk {doc_id: '%s'}) RETURN count(c) AS n", old.ID)); n != 0 {
		t.Errorf("chunks attached to superseded document after race: %d", n)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", old.ID)); n != 0 {
		t.Errorf("fact edges on superseded document after race: %d", n)
	}
}

// TestResubmitSupersededContentRevives pins the intended content-regression
// semantics: re-submitting the exact content of a superseded version
// revives that version's node (it becomes the current version again) and
// reports the currently live version as PreviousID.
func TestResubmitSupersededContentRevives(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	source := uniqueSource(t)
	v1 := testUpsert(t, st, "file", source, "hash-a")
	v2 := testUpsert(t, st, "file", source, "hash-b")
	if err := st.Supersede(ctx, v1.ID, v2.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	again := testUpsert(t, st, "file", source, "hash-a")
	if again.Noop {
		t.Error("resubmission of superseded content: want Noop=false (revival)")
	}
	if again.ID != v1.ID {
		t.Errorf("revived id = %q, want the original node %q", again.ID, v1.ID)
	}
	if again.PreviousID != v2.ID {
		t.Errorf("revival PreviousID = %q, want the live version %q", again.PreviousID, v2.ID)
	}
	doc, err := st.Document(ctx, v1.ID)
	if err != nil {
		t.Fatalf("Document(revived): %v", err)
	}
	if doc.Status != graph.StatusReceived {
		t.Errorf("revived status = %q, want received", doc.Status)
	}
}

func TestSupersedeZeroChunkDocument(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	source := uniqueSource(t)
	old := testUpsert(t, st, "file", source, "hash-a")
	newer := testUpsert(t, st, "file", source, "hash-b")
	if err := st.Supersede(ctx, old.ID, newer.ID); err != nil {
		t.Errorf("Supersede without chunks: %v", err)
	}
}

// TestUnfactedEntityLingersUntilGlobalGC pins the known limitation:
// entities merged without facts have no edges and are only pruned by the
// global GC sweep, not by supersede's touched-set GC.
func TestUnfactedEntityLingersUntilGlobalGC(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	old, newer, _, _ := supersedePair(t, st)

	tag := fmt.Sprintf("unfacted-%d", len(uniqueSource(t)))
	key := graph.EntityKey("thing", tag)
	if err := st.UpsertExtraction(ctx, old.ID, old.ID+"-c0", "mock:1",
		[]graph.EntityUpsert{{Key: key, Name: tag, Type: "thing"}}, nil); err != nil {
		t.Fatalf("UpsertExtraction (no facts): %v", err)
	}
	if err := st.Supersede(ctx, old.ID, newer.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (e:Entity {key: '%s'}) RETURN count(e) AS n", key)); n != 1 {
		t.Errorf("unfacted entity count = %d, want 1 (touched-set GC cannot see it)", n)
	}
	if err := st.GC(ctx); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (e:Entity {key: '%s'}) RETURN count(e) AS n", key)); n != 0 {
		t.Errorf("unfacted entity count after global GC = %d, want 0", n)
	}
}

func TestSetStatusCannotReviveSuperseded(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	old, newer, _, _ := supersedePair(t, st)
	if err := st.Supersede(ctx, old.ID, newer.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if err := st.SetStatus(ctx, old.ID, graph.StatusDone, ""); err != nil {
		t.Fatalf("SetStatus on superseded doc: %v", err)
	}
	doc, err := st.Document(ctx, old.ID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusSuperseded {
		t.Errorf("status = %q after late SetStatus, want superseded (terminal)", doc.Status)
	}
}

func TestInFlightListsNonTerminal(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()

	received := testUpsert(t, st, "file", uniqueSource(t), "hash-a")
	done := testUpsert(t, st, "file", uniqueSource(t), "hash-b")
	failed := testUpsert(t, st, "file", uniqueSource(t), "hash-c")
	old, newer, _, _ := supersedePair(t, st)
	if err := st.Supersede(ctx, old.ID, newer.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if err := st.SetStatus(ctx, done.ID, graph.StatusDone, ""); err != nil {
		t.Fatalf("SetStatus done: %v", err)
	}
	if err := st.SetStatus(ctx, failed.ID, graph.StatusFailed, "boom"); err != nil {
		t.Fatalf("SetStatus failed: %v", err)
	}

	mine := map[string]graph.Status{
		received.ID: graph.StatusReceived,
		done.ID:     graph.StatusDone,
		failed.ID:   graph.StatusFailed,
		old.ID:      graph.StatusSuperseded,
		newer.ID:    graph.StatusReceived,
	}
	docs, err := st.InFlight(ctx)
	if err != nil {
		t.Fatalf("InFlight: %v", err)
	}
	got := map[string]graph.Status{}
	position := map[string]int{}
	for i, d := range docs {
		if _, ok := mine[d.ID]; ok {
			got[d.ID] = d.Status
			position[d.ID] = i
		}
	}
	for id, want := range mine {
		status, present := got[id]
		switch want {
		case graph.StatusReceived:
			if !present {
				t.Errorf("in-flight document %s missing from InFlight", id)
			}
		default:
			if present {
				t.Errorf("terminal document %s (status %s) listed as in flight (status %s)", id, want, status)
			}
		}
	}
	// Ordering: oldest updated_at first — the older received document must
	// precede the newer one.
	if position[received.ID] > position[newer.ID] {
		t.Errorf("InFlight order: %s at %d after %s at %d; want oldest first",
			received.ID, position[received.ID], newer.ID, position[newer.ID])
	}
}
