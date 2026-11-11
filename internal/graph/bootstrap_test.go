package graph_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestBootstrapDoesNotMigrate(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	key := fmt.Sprintf("person:bootstrap-%d", time.Now().UnixNano())
	rawRun(t, uri, fmt.Sprintf(`CREATE (:Entity {key: '%s'})`, key))
	t.Cleanup(func() { rawRun(t, uri, fmt.Sprintf(`MATCH (e:Entity {key: '%s'}) DETACH DELETE e`, key)) })
	for _, marker := range []string{"missing", "old"} {
		t.Run(marker, func(t *testing.T) {
			rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
			if marker == "old" {
				rawRun(t, uri, `CREATE (:GkgMeta {id: 'extraction_schema', value: 1})`)
			}
			before := rawRecords(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) RETURN m.value AS n`)
			if err := st.Bootstrap(ctx); err != nil {
				t.Fatal(err)
			}
			after := rawRecords(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) RETURN m.value AS n`)
			if !reflect.DeepEqual(before, after) {
				t.Errorf("Bootstrap changed marker: before %+v, after %+v", before, after)
			}
			if n := rawCount(t, uri, fmt.Sprintf(`MATCH (:Entity {key: '%s'}) RETURN count(*) AS n`, key)); n != 1 {
				t.Error("Bootstrap removed legacy entity")
			}
			if current, err := st.ExtractionSchemaCurrent(ctx); err != nil || current {
				t.Errorf("ExtractionSchemaCurrent = %v, %v; want false, nil", current, err)
			}
		})
	}
}

func TestMigrateExtractionSchema(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	tag := fmt.Sprintf("schema-%d", time.Now().UnixNano())
	statuses := []graph.Status{graph.StatusDone, graph.StatusExtracted, graph.StatusMerged, graph.StatusVerifying,
		graph.StatusReceived, graph.StatusFetched, graph.StatusChunked, graph.StatusFailed, graph.StatusSuperseded}
	docs := make(map[graph.Status]*graph.Document)
	for _, status := range statuses {
		res := testUpsert(t, st, "text", tag+"-"+string(status), "hash")
		id := res.ID
		t.Cleanup(func() {
			if err := st.DeleteDocument(ctx, id); err != nil {
				t.Error(err)
			}
		})
		if err := st.PutChunks(ctx, id, []graph.Chunk{{Text: "Ada Lovelace wrote notes.", ExactKey: "fingerprint", MinhashSig: []int64{1, 2}, BandKeys: []int64{3}, SignatureVersion: 1}}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetStatus(ctx, id, status, ""); err != nil {
			t.Fatal(err)
		}
		doc, err := st.Document(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		docs[status] = doc
	}
	cid := docs[graph.StatusDone].ID + "-c0"
	rawRun(t, uri, fmt.Sprintf(`
MATCH (c:Chunk {id: '%s'})
CREATE (a:Entity {key: 'person:ada lovelace %s'}), (b:Entity {key: 'work:notes %s'}),
       (c)-[:MENTIONS]->(a), (a)-[:wrote {doc_id: '%s'}]->(b),
       (:RejectedFact {doc_id: '%s', chunk_id: '%s'})`, cid, tag, tag, docs[graph.StatusDone].ID, docs[graph.StatusDone].ID, cid))
	// More than one entity batch; the migration must clear even orphan nodes.
	rawRun(t, uri, fmt.Sprintf(`UNWIND range(0, 1000) AS i CREATE (:Entity {key: 'legacy:%s:' + toString(i)})`, tag))
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
	wantDocs := int(rawCount(t, uri, `MATCH (d:Document) WHERE d.status IN ['done', 'extracted', 'merged', 'verifying'] RETURN count(*) AS n`))
	wantEntities := int(rawCount(t, uri, `MATCH (e:Entity) RETURN count(e) AS n`))
	result, err := st.MigrateExtractionSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := graph.MigrationResult{From: 1, To: graph.ExtractionSchemaVersion, DocumentsRequeued: wantDocs, EntitiesRemoved: wantEntities, Migrated: true}
	if result != want {
		t.Errorf("migration result = %+v, want %+v", result, want)
	}
	assertExtractionSchemaMarker(t, uri)
	if current, err := st.ExtractionSchemaCurrent(ctx); err != nil || !current {
		t.Errorf("ExtractionSchemaCurrent = %v, %v; want true, nil", current, err)
	}
	for _, query := range []string{
		`MATCH (e:Entity) RETURN count(e) AS n`,
		`MATCH ()-[m:MENTIONS]->() RETURN count(m) AS n`,
		`MATCH (:Entity)-[r]->(:Entity) RETURN count(r) AS n`,
		fmt.Sprintf(`MATCH (r:RejectedFact {doc_id: '%s'}) RETURN count(r) AS n`, docs[graph.StatusDone].ID),
	} {
		if got := rawCount(t, uri, query); got != 0 {
			t.Errorf("remaining derived data = %d for %s", got, query)
		}
	}
	for i, status := range statuses {
		before := docs[status]
		after, err := st.Document(ctx, before.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := status
		if i < 4 {
			want = graph.StatusChunked
			if !after.UpdatedAt.After(before.UpdatedAt) {
				t.Errorf("%s updated_at did not advance", status)
			}
		} else if !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("untouched %s updated_at changed", status)
		}
		if after.Status != want {
			t.Errorf("%s became %s, want %s", status, after.Status, want)
		}
		chunks, err := st.Chunks(ctx, before.ID)
		if err != nil || len(chunks) != 1 {
			t.Fatalf("chunks = %+v, err = %v", chunks, err)
		}
		if chunks[0].Text != "Ada Lovelace wrote notes." || chunks[0].ExactKey != "fingerprint" || !reflect.DeepEqual(chunks[0].MinhashSig, []int64{1, 2}) || !reflect.DeepEqual(chunks[0].BandKeys, []int64{3}) || chunks[0].SignatureVersion != 1 {
			t.Errorf("chunk text/fingerprint changed: %+v", chunks[0])
		}
	}
	// New data and a done document must survive a second migration: this
	// verifies the marker guard even when the graph has entities again.
	rawRun(t, uri, fmt.Sprintf(`CREATE (:Entity {key: 'new:%s'})`, tag))
	t.Cleanup(func() { rawRun(t, uri, fmt.Sprintf(`MATCH (e:Entity {key: 'new:%s'}) DETACH DELETE e`, tag)) })
	id := docs[graph.StatusDone].ID
	if err := st.SetStatus(ctx, id, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	before, _ := st.Document(ctx, id)
	result, err = st.MigrateExtractionSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := (graph.MigrationResult{From: graph.ExtractionSchemaVersion, To: graph.ExtractionSchemaVersion}); result != want {
		t.Errorf("second migration result = %+v, want %+v", result, want)
	}
	after, err := st.Document(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Errorf("second migration changed document: before %+v, after %+v, err %v", before, after, err)
	}
	if n := rawCount(t, uri, fmt.Sprintf(`MATCH (:Entity {key: 'new:%s'}) RETURN count(*) AS n`, tag)); n != 1 {
		t.Errorf("second migration removed current-schema entity")
	}
}

func TestMigrateExtractionSchemaV2(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	docID, chunkID := entitiesTestDoc(t, st, "A widget.")
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, docID); err != nil {
			t.Error(err)
		}
	})
	if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:1", newFixture(t).ents, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus(ctx, docID, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) SET m.value = 2`)
	result, err := st.MigrateExtractionSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Migrated || result.From != 2 || result.To != 3 || result.DocumentsRequeued < 1 || result.EntitiesRemoved < 2 {
		t.Errorf("v2 migration = %+v, want v2 to v3 with derived data cleared and documents requeued", result)
	}
	assertExtractionSchemaMarker(t, uri)
	doc, err := st.Document(ctx, docID)
	if err != nil || doc.Status != graph.StatusChunked {
		t.Errorf("document = %+v, %v; want chunked", doc, err)
	}
	if n := rawCount(t, uri, `MATCH (e:Entity) RETURN count(e) AS n`); n != 0 {
		t.Errorf("entities after v2 migration = %d, want 0", n)
	}
}

func TestMigrateFreshStoreSetsMarker(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	// Bring the disposable DB to the no-entities state, independent of test
	// order or previous runs. MigrationStore restores foreign doc statuses.
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
	if _, err := st.MigrateExtractionSchema(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := entitiesTestDoc(t, st, "Stored text without entities.")
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, id); err != nil {
			t.Error(err)
		}
	})
	if err := st.SetStatus(ctx, id, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	before, err := st.Document(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
	result, err := st.MigrateExtractionSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := (graph.MigrationResult{From: 1, To: graph.ExtractionSchemaVersion, Migrated: true}); result != want {
		t.Errorf("empty migration result = %+v, want %+v", result, want)
	}
	assertExtractionSchemaMarker(t, uri)
	after, err := st.Document(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Errorf("no-entities migration changed document: before %+v, after %+v, err %v", before, after, err)
	}
	chunks, err := st.Chunks(ctx, id)
	if err != nil || len(chunks) != 1 || chunks[0].Text != "Stored text without entities." {
		t.Errorf("no-entities migration changed chunks: %+v, err %v", chunks, err)
	}
}

// TestBootstrapMarksEmptyStore: a fresh store (no derived extraction data)
// gets the current marker from Bootstrap alone, so gkg ingest accepts it.
func TestBootstrapMarksEmptyStore(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	// Clear derived data left by other tests on the disposable DB.
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
	if _, err := st.MigrateExtractionSchema(ctx); err != nil {
		t.Fatal(err)
	}
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
	if current, err := st.ExtractionSchemaCurrent(ctx); err != nil || current {
		t.Fatalf("before Bootstrap: ExtractionSchemaCurrent = %v, %v; want false, nil", current, err)
	}
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	assertExtractionSchemaMarker(t, uri)
	if current, err := st.ExtractionSchemaCurrent(ctx); err != nil || !current {
		t.Errorf("after Bootstrap: ExtractionSchemaCurrent = %v, %v; want true, nil", current, err)
	}
}

func TestMigrateRejectedFactOnly(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	// Normalize derived data before setting up the audit-only legacy store.
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
	if _, err := st.MigrateExtractionSchema(ctx); err != nil {
		t.Fatal(err)
	}
	id, _ := entitiesTestDoc(t, st, "Stored audit-only document.")
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, id); err != nil {
			t.Error(err)
		}
	})
	if err := st.SetStatus(ctx, id, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	rawRun(t, uri, fmt.Sprintf(`CREATE (:RejectedFact {doc_id: '%s'})`, id))
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
	if n := rawCount(t, uri, `MATCH (e:Entity) RETURN count(e) AS n`); n != 0 {
		t.Fatalf("audit-only fixture has %d entities", n)
	}
	wantDocs := int(rawCount(t, uri, `MATCH (d:Document) WHERE d.status IN ['done', 'extracted', 'merged', 'verifying'] RETURN count(*) AS n`))
	result, err := st.MigrateExtractionSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := (graph.MigrationResult{From: 1, To: graph.ExtractionSchemaVersion, DocumentsRequeued: wantDocs, Migrated: true}); result != want {
		t.Errorf("audit-only migration result = %+v, want %+v", result, want)
	}
	if n := rawCount(t, uri, `MATCH (r:RejectedFact) RETURN count(r) AS n`); n != 0 {
		t.Errorf("%d legacy audits survived", n)
	}
	doc, err := st.Document(ctx, id)
	if err != nil || doc.Status != graph.StatusChunked {
		t.Errorf("audit-only document = %+v, %v; want chunked", doc, err)
	}
	assertExtractionSchemaMarker(t, uri)
}

func assertExtractionSchemaMarker(t *testing.T, uri string) {
	t.Helper()
	recs := rawRecords(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) RETURN m.value AS n`)
	if len(recs) != 1 || testInt64(recs[0], "n") != int64(graph.ExtractionSchemaVersion) {
		t.Fatalf("extraction schema marker = %+v, want version %d", recs, graph.ExtractionSchemaVersion)
	}
}
