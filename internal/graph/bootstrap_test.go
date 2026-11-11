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

func TestBootstrapMigratesExtractionSchema(t *testing.T) {
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
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	assertExtractionSchemaMarker(t, uri)
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
	// New data and a done document must survive a second Bootstrap: this
	// verifies the marker guard even when the graph has entities again.
	rawRun(t, uri, fmt.Sprintf(`CREATE (:Entity {key: 'new:%s'})`, tag))
	t.Cleanup(func() { rawRun(t, uri, fmt.Sprintf(`MATCH (e:Entity {key: 'new:%s'}) DETACH DELETE e`, tag)) })
	id := docs[graph.StatusDone].ID
	if err := st.SetStatus(ctx, id, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	before, _ := st.Document(ctx, id)
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := st.Document(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Errorf("second Bootstrap changed document: before %+v, after %+v, err %v", before, after, err)
	}
	if n := rawCount(t, uri, fmt.Sprintf(`MATCH (:Entity {key: 'new:%s'}) RETURN count(*) AS n`, tag)); n != 1 {
		t.Errorf("second Bootstrap removed current-schema entity")
	}
}

func TestBootstrapFreshStoreSetsMarker(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	// Bring the disposable DB to the no-entities state, independent of test
	// order or previous runs. MigrationStore restores foreign doc statuses.
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
	if err := st.Bootstrap(ctx); err != nil {
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
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	assertExtractionSchemaMarker(t, uri)
	after, err := st.Document(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Errorf("no-entities Bootstrap changed document: before %+v, after %+v, err %v", before, after, err)
	}
	chunks, err := st.Chunks(ctx, id)
	if err != nil || len(chunks) != 1 || chunks[0].Text != "Stored text without entities." {
		t.Errorf("no-entities Bootstrap changed chunks: %+v, err %v", chunks, err)
	}
}

func assertExtractionSchemaMarker(t *testing.T, uri string) {
	t.Helper()
	recs := rawRecords(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) RETURN m.value AS n`)
	if len(recs) != 1 || testInt64(recs[0], "n") != int64(graph.ExtractionSchemaVersion) {
		t.Fatalf("extraction schema marker = %+v, want version %d", recs, graph.ExtractionSchemaVersion)
	}
}
