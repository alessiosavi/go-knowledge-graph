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
		tail := graph.EntityKey(fmt.Sprintf("work %d %s", i, uniqueSource(t)))
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
	key := graph.EntityKey(tag)
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

// TestDeleteChainRemovesAllVersions builds a three-version supersede chain
// (v1→v2→v3) and deletes it from the middle id: every version, its chunks,
// its fact edges and the entities only mentioned there must go, with an
// exact count.
func TestDeleteChainRemovesAllVersions(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	source := uniqueSource(t)
	versions := []graph.UpsertResult{
		testUpsert(t, st, "file", source, "hash-a"),
		testUpsert(t, st, "file", source, "hash-b"),
		testUpsert(t, st, "file", source, "hash-c"),
	}
	keys := make([]string, 0, len(versions)*2)
	for i, res := range versions {
		fx := newFixture(t)
		text := fmt.Sprintf("Ada Lovelace wrote algorithm number %d.", i)
		if err := st.PutChunks(ctx, res.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
			t.Fatalf("PutChunks v%d: %v", i+1, err)
		}
		if err := st.UpsertExtraction(ctx, res.ID, res.ID+"-c0", "mock:1", fx.ents, []graph.FactUpsert{fx.fact}); err != nil {
			t.Fatalf("UpsertExtraction v%d: %v", i+1, err)
		}
		for _, e := range fx.ents {
			keys = append(keys, e.Key)
		}
	}
	if err := st.Supersede(ctx, versions[0].ID, versions[1].ID); err != nil {
		t.Fatalf("Supersede v1->v2: %v", err)
	}
	if err := st.Supersede(ctx, versions[1].ID, versions[2].ID); err != nil {
		t.Fatalf("Supersede v2->v3: %v", err)
	}

	n, err := st.DeleteChain(ctx, versions[1].ID)
	if err != nil {
		t.Fatalf("DeleteChain: %v", err)
	}
	if n != 3 {
		t.Errorf("DeleteChain deleted = %d, want 3", n)
	}
	for i, res := range versions {
		if _, err := st.Document(ctx, res.ID); !errors.Is(err, graph.ErrNotFound) {
			t.Errorf("v%d Document after chain delete = %v, want ErrNotFound", i+1, err)
		}
		if got := rawCount(t, uri, fmt.Sprintf("MATCH (c:Chunk {doc_id: '%s'}) RETURN count(c) AS n", res.ID)); got != 0 {
			t.Errorf("v%d chunks after chain delete = %d, want 0", i+1, got)
		}
		if got := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", res.ID)); got != 0 {
			t.Errorf("v%d fact edges after chain delete = %d, want 0", i+1, got)
		}
	}
	for _, key := range keys {
		if got := rawCount(t, uri, fmt.Sprintf("MATCH (e:Entity {key: '%s'}) RETURN count(e) AS n", key)); got != 0 {
			t.Errorf("orphan entity %q survived chain delete", key)
		}
	}
	ids, err := st.SameIdentityDocumentIDs(ctx, "file", source)
	if err != nil {
		t.Fatalf("SameIdentityDocumentIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("identity still holds %d documents after chain delete", len(ids))
	}
}

// TestDeleteChainSingleVersion pins the count for an identity with a single
// version (no supersede history).
func TestDeleteChainSingleVersion(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	doc := testUpsert(t, st, "file", uniqueSource(t), "hash-a")
	text := "Ada Lovelace wrote the first algorithm."
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}

	n, err := st.DeleteChain(ctx, doc.ID)
	if err != nil {
		t.Fatalf("DeleteChain: %v", err)
	}
	if n != 1 {
		t.Errorf("DeleteChain deleted = %d, want 1", n)
	}
	if _, err := st.Document(ctx, doc.ID); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("Document after chain delete = %v, want ErrNotFound", err)
	}
}

// TestDeleteChainNotFound pins the missing-id contract.
func TestDeleteChainNotFound(t *testing.T) {
	st := graphtest.Store(t)
	n, err := st.DeleteChain(context.Background(), "missing-doc")
	if !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("DeleteChain(missing) = %v, want ErrNotFound", err)
	}
	if n != 0 {
		t.Errorf("DeleteChain(missing) = %d, want 0", n)
	}
}

// TestDeleteChainLeavesOtherIdentities pins cross-identity isolation: only
// the deleted chain disappears.
func TestDeleteChainLeavesOtherIdentities(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()

	aOld, aNew, _, _ := supersedePair(t, st)
	if err := st.Supersede(ctx, aOld.ID, aNew.ID); err != nil {
		t.Fatalf("Supersede A: %v", err)
	}
	bOld, bNew, _, bChunk := supersedePair(t, st)
	if err := st.Supersede(ctx, bOld.ID, bNew.ID); err != nil {
		t.Fatalf("Supersede B: %v", err)
	}

	n, err := st.DeleteChain(ctx, aNew.ID)
	if err != nil {
		t.Fatalf("DeleteChain A: %v", err)
	}
	if n != 2 {
		t.Errorf("DeleteChain A deleted = %d, want 2", n)
	}
	if _, err := st.Document(ctx, aOld.ID); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("A old after chain delete = %v, want ErrNotFound", err)
	}
	doc, err := st.Document(ctx, bNew.ID)
	if err != nil {
		t.Fatalf("B new must survive: %v", err)
	}
	if doc.Supersedes != bOld.ID {
		t.Errorf("B new supersedes = %q, want %q (untouched)", doc.Supersedes, bOld.ID)
	}
	if got := rawCount(t, uri, fmt.Sprintf("MATCH (c:Chunk {id: '%s'}) RETURN count(c) AS n", bChunk)); got != 1 {
		t.Errorf("B chunk after A chain delete = %d, want 1", got)
	}
	if got := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", bNew.ID)); got != 1 {
		t.Errorf("B fact edges after A chain delete = %d, want 1", got)
	}
}

// TestDeleteDocumentClearsSupersedesPointer pins that removing one version
// clears any supersedes pointer referencing it, so a surviving newer
// version cannot dangle.
func TestDeleteDocumentClearsSupersedesPointer(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	old, newer, _, newChunk := supersedePair(t, st)
	if err := st.Supersede(ctx, old.ID, newer.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	doc, err := st.Document(ctx, newer.ID)
	if err != nil {
		t.Fatalf("setup Document(new): %v", err)
	}
	if doc.Supersedes != old.ID {
		t.Fatalf("setup: newer.Supersedes = %q, want %q", doc.Supersedes, old.ID)
	}

	if err := st.DeleteDocument(ctx, old.ID); err != nil {
		t.Fatalf("DeleteDocument(old): %v", err)
	}
	doc, err = st.Document(ctx, newer.ID)
	if err != nil {
		t.Fatalf("Document(new) after delete: %v", err)
	}
	if doc.Supersedes != "" {
		t.Errorf("newer.Supersedes = %q after deleting the referenced version, want empty", doc.Supersedes)
	}
	if got := rawCount(t, uri, fmt.Sprintf("MATCH (c:Chunk {id: '%s'}) RETURN count(c) AS n", newChunk)); got != 1 {
		t.Errorf("newer chunk after delete = %d, want 1", got)
	}
}
