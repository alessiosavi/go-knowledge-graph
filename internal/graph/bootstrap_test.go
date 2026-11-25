package graph_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
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

func TestBootstrapEntityKeyWithCompositeConstraint(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	uri := os.Getenv(graphtest.URIEnv)
	key := fmt.Sprintf("bootstrap-composite-%d", time.Now().UnixNano())
	const composite = `CONSTRAINT ON (e:Entity) ASSERT e.key, e.id IS UNIQUE`
	rawRun(t, uri, fmt.Sprintf(`CREATE (:Entity {key:'%s', id:'original'})`, key))
	t.Cleanup(func() {
		rawRun(t, uri, fmt.Sprintf(`MATCH (e:Entity {key:'%s'}) DELETE e`, key))
		if err := st.Bootstrap(context.Background()); err != nil {
			t.Errorf("restore bootstrap: %v", err)
		}
	})
	rawRun(t, uri, "CREATE "+composite)
	t.Cleanup(func() { rawRun(t, uri, "DROP "+composite) })
	rawRun(t, uri, `DROP CONSTRAINT ON (e:Entity) ASSERT e.key IS UNIQUE`)
	for range 2 {
		if err := st.Bootstrap(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := rawCount(t, uri, fmt.Sprintf(`MATCH (:Entity {key:'%s', id:'original'}) RETURN count(*) AS n`, key)); n != 1 {
		t.Errorf("original entity count after repeat Bootstrap = %d, want 1", n)
	}
	err := rawExec(uri, fmt.Sprintf(`CREATE (:Entity {key:'%s', id:'different'})`, key))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Errorf("duplicate key with different ID = %v, want a constraint error", err)
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
	result, err := st.MigrateExtractionSchemaOpts(ctx, graph.MigrationOptions{DetectAll: true})
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
	result, err = st.MigrateExtractionSchemaOpts(ctx, graph.MigrationOptions{DetectAll: true})
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
	result, err := st.MigrateExtractionSchemaOpts(ctx, graph.MigrationOptions{DetectAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Migrated || result.From != 2 || result.To != graph.ExtractionSchemaVersion || result.DocumentsRequeued < 1 || result.EntitiesRemoved < 2 {
		t.Errorf("v2 migration = %+v, want v2 to v4 with derived data cleared and documents requeued", result)
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

func TestMigrateDocumentWithoutEntities(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	// Bring the disposable DB to the no-entities state, independent of test
	// order or previous runs. MigrationStore restores foreign doc statuses.
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
	if _, err := st.MigrateExtractionSchemaOpts(ctx, graph.MigrationOptions{DetectAll: true}); err != nil {
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
	wantDocs := int(rawCount(t, uri, `MATCH (d:Document) WHERE d.status IN ['done', 'extracted', 'merged', 'verifying'] RETURN count(*) AS n`))
	result, err := st.MigrateExtractionSchemaOpts(ctx, graph.MigrationOptions{DetectAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := (graph.MigrationResult{From: 1, To: graph.ExtractionSchemaVersion, DocumentsRequeued: wantDocs, Migrated: true}); result != want {
		t.Errorf("empty migration result = %+v, want %+v", result, want)
	}
	assertExtractionSchemaMarker(t, uri)
	after, err := st.Document(ctx, id)
	if err != nil || after.Status != graph.StatusChunked || after.Domain == "" {
		t.Errorf("document-only migration did not assign and requeue: before %+v, after %+v, err %v", before, after, err)
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
	// A truly empty store has no Documents either.
	wipeGraph(t, uri)
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
	if _, err := st.MigrateExtractionSchemaOpts(ctx, graph.MigrationOptions{DetectAll: true}); err != nil {
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
	if _, err := st.MigrateExtractionSchemaOpts(ctx, graph.MigrationOptions{DetectAll: true}); err != nil {
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
	result, err := st.MigrateExtractionSchemaOpts(ctx, graph.MigrationOptions{DetectAll: true})
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

// TestTextIndexesV2 covers upgrading an existing graph as well as a repeat
// bootstrap. The synthetic corpus also measures startup index build cost.
func TestTextIndexesV2(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	t.Cleanup(func() {
		if err := st.Bootstrap(ctx); err != nil {
			t.Error(err)
		}
	})
	uri := os.Getenv(graphtest.URIEnv)
	doc, cid := entitiesTestDoc(t, st, "Acme-Max in Zorbo.")
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, doc); err != nil {
			t.Error(err)
		}
	})
	// Exercise replacement with existing indexed data, including legacy indexes.
	for _, name := range []string{"chunk_text_v2", "entity_text_v2"} {
		if rawNamedTextIndex(t, uri, name) {
			rawRun(t, uri, "DROP TEXT INDEX "+name)
		}
	}
	for _, ix := range []struct{ name, label, props string }{{"chunk_text", "Chunk", "text"}, {"entity_text", "Entity", "name, aliases"}} {
		if !rawNamedTextIndex(t, uri, ix.name) {
			rawRun(t, uri, fmt.Sprintf("CREATE TEXT INDEX %s ON :%s(%s)", ix.name, ix.label, ix.props))
		}
	}
	rawRun(t, uri, fmt.Sprintf(`MATCH (d:Document {id:'%s'}) UNWIND range(1,10999) AS i CREATE (c:Chunk {id:d.id+'-c'+toString(i),doc_id:d.id,seq:i,text:'Acme-Max builds Quillon tools in Zorbo.',domain:'a',domain_tok:'dkca978112ca1bbdca'}) CREATE (d)-[:HAS_CHUNK {seq:i}]->(c)`, doc))
	if n := rawCount(t, uri, fmt.Sprintf(`MATCH (:Document {id:'%s'})-[:HAS_CHUNK]->(c) RETURN count(c) AS n`, doc)); n != 11000 {
		t.Fatalf("synthetic chunks=%d, want 11000", n)
	}
	if err := st.UpsertExtraction(ctx, doc, cid, "mock:1", []graph.EntityUpsert{{Key: "acme-" + doc, Name: "Acme-Max", Mentions: []graph.MentionSpan{{Start: 0, End: 8}}}}, nil); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := st.EnsureTextIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("v2 text index build on 11000 synthetic chunks: %s", time.Since(start))
	for i := 0; i < 2; i++ {
		if err := st.Bootstrap(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"chunk_text_v2", "entity_text_v2"} {
		if !rawNamedTextIndex(t, uri, name) {
			t.Errorf("missing %s", name)
		}
	}
	for _, name := range []string{"chunk_text", "entity_text"} {
		if rawNamedTextIndex(t, uri, name) {
			t.Errorf("legacy index %s remains", name)
		}
	}
	if !rawTextIndexExists(t, uri, "Chunk", []string{"text", "domain_tok"}) || !rawTextIndexExists(t, uri, "Entity", []string{"name", "domain_tok"}) {
		t.Error("v2 index fields differ")
	}
	for _, ix := range []struct{ label, prop string }{{"Document", "domain"}, {"Chunk", "domain"}, {"Entity", "name_key"}, {"Domain", "id"}} {
		if !rawIndexExists(t, uri, ix.label, ix.prop) {
			t.Errorf("missing index %s(%s)", ix.label, ix.prop)
		}
	}
	if !rawUniqueConstraintExists(t, uri, "Domain", "id") {
		t.Error("missing Domain(id) constraint")
	}
	// V2 entity/chunk unscoped search still quotes punctuation and operator words.
	if hits, err := st.ChunkTextSearch(ctx, "Acme-Max", 1); err != nil || len(hits) != 1 {
		t.Errorf("chunk search=%+v,%v", hits, err)
	}
	if hits, err := st.EntityTextSearch(ctx, "Acme-Max", 1); err != nil || len(hits) != 1 {
		t.Errorf("entity search=%+v,%v", hits, err)
	}
}

func TestBootstrapProductSchema(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	for range 2 {
		if err := st.Bootstrap(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []struct{ label, prop string }{
		{"FeatureRegistry", "rev"}, {"FeatureRev", "key"}, {"ProductRelease", "id"},
		{"ProductStore", "store"}, {"Product", "id"}, {"ProductRef", "id"},
		{"ProductText", "id"}, {"TextVec", "id"},
	} {
		if !rawUniqueConstraintExists(t, uri, key.label, key.prop) {
			t.Errorf("missing unique constraint %s(%s)", key.label, key.prop)
		}
		if !rawIndexExists(t, uri, key.label, key.prop) {
			t.Errorf("missing index %s(%s)", key.label, key.prop)
		}
	}
	for _, index := range []struct {
		label string
		props []string
	}{
		{"Product", []string{"store", "product_id"}},
		{"Product", []string{"domain"}},
		{"FeatureRev", []string{"id"}},
		{"ProductStore", []string{"domain"}},
		{"ProductText", []string{"vec_id"}},
		{"ProductText", []string{"store"}},
		{"TextVec", []string{"vec_id"}},
		{"TextVec", []string{"space"}},
	} {
		if !rawIndexExists(t, uri, index.label, index.props...) {
			t.Errorf("missing index %s(%v)", index.label, index.props)
		}
	}
	if rawVectorIndexExists(t, uri, "TextVec") {
		t.Fatal("unexpected TextVec vector index after Bootstrap")
	}
	t.Run("detects_TextVec_vector_index", func(t *testing.T) {
		const index = "bootstrap_text_vec_probe"
		rawRun(t, uri, "CREATE VECTOR INDEX "+index+` ON :TextVec(embedding)
WITH CONFIG {"dimension": 768, "capacity": 16, "metric": "cos"}`)
		t.Cleanup(func() { rawRun(t, uri, "DROP VECTOR INDEX "+index) })
		if !rawVectorIndexExists(t, uri, "TextVec") {
			t.Fatal("failed to detect the test-created TextVec vector index")
		}
	})
}

func rawVectorIndexExists(t *testing.T, uri, label string) bool {
	t.Helper()
	for _, record := range rawRecords(t, uri, "SHOW VECTOR INDEX INFO") {
		if got, _ := record.Get("label"); strings.TrimPrefix(fmt.Sprint(got), ":") == label {
			return true
		}
	}
	return false
}

func rawNamedTextIndex(t *testing.T, uri, name string) bool {
	t.Helper()
	for _, r := range rawRecords(t, uri, "SHOW INDEX INFO") {
		typ, _ := r.Get("index type")
		if strings.Contains(fmt.Sprint(typ), "name: "+name+")") {
			return true
		}
	}
	return false
}
