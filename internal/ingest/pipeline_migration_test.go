package ingest_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func TestPipelineRecoversExtractionSchemaMigration(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	tag := fmt.Sprintf("migration-%d", time.Now().UnixNano())
	type fixture struct {
		oldID, newID, name, text string
	}
	var fixtures []fixture
	for _, status := range []graph.Status{graph.StatusDone, graph.StatusMerged, graph.StatusVerifying} {
		source := tag + "-" + string(status)
		name := "Migrated" + string(status) + fmt.Sprint(time.Now().UnixNano())
		text := name + " wrote a stored note."
		old, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: source, Hash: "old"})
		if err != nil {
			t.Fatal(err)
		}
		fresh, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: source, Hash: "new"})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{old.ID, fresh.ID} {
			t.Cleanup(func() {
				if err := st.DeleteDocument(ctx, id); err != nil {
					t.Error(err)
				}
			})
			if err := st.PutChunks(ctx, id, []graph.Chunk{{Text: text, End: len(text)}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.SetStatus(ctx, old.ID, graph.StatusDone, ""); err != nil {
			t.Fatal(err)
		}
		if err := st.SetStatus(ctx, fresh.ID, status, ""); err != nil {
			t.Fatal(err)
		}
		// Stable version ordering independent of clock precision. A typed
		// legacy entity proves Bootstrap takes the migration path.
		rawRecords(t, uri, fmt.Sprintf(`MATCH (d:Document {id: '%s'}) SET d.version_at = '2000-01-01T00:00:00.000000000Z' RETURN d.id`, old.ID))
		rawRecords(t, uri, fmt.Sprintf(`MATCH (d:Document {id: '%s'}) SET d.version_at = '2001-01-01T00:00:00.000000000Z' RETURN d.id`, fresh.ID))
		rawRecords(t, uri, fmt.Sprintf(`CREATE (e:Entity {key: 'person:%s'}) RETURN e.key`, graph.EntityKey(name)))
		fixtures = append(fixtures, fixture{old.ID, fresh.ID, name, text})
	}
	rawRecords(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m RETURN count(*)`)
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	// This is the exact persisted set Recover uses to enqueue work.
	inflight, err := st.InFlight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queued := make(map[string]graph.Status)
	for _, doc := range inflight {
		queued[doc.ID] = doc.Status
	}
	mock := &extract.Mock{}
	// A restarted pipeline has no submission cache, loader, or prior spool.
	// Process must use only the existing chunk text, then finish superseding.
	p := ingest.NewPipeline(testConfig(t), st, mock, nil, nil)
	for _, fx := range fixtures {
		if queued[fx.newID] != graph.StatusChunked {
			t.Fatalf("migration did not requeue %s as chunked: %+v", fx.newID, queued)
		}
		if err := p.Process(ctx, fx.newID); err != nil {
			t.Fatal(err)
		}
		doc, err := st.Document(ctx, fx.newID)
		if err != nil || doc.Status != graph.StatusDone || doc.Supersedes != fx.oldID {
			t.Fatalf("re-extracted document = %+v, err %v; want done, superseding %s", doc, err, fx.oldID)
		}
		old, err := st.Document(ctx, fx.oldID)
		if err != nil || old.Status != graph.StatusSuperseded {
			t.Errorf("old document = %+v, err %v; want superseded", old, err)
		}
		view, err := st.EntityView(ctx, graph.EntityKey(fx.name), false)
		if err != nil || len(view.Facts) != 1 {
			t.Errorf("name-only entity = %+v, err %v; want one fact", view, err)
		}
		if n := rawCount(t, uri, fmt.Sprintf(`MATCH (:Entity {key: 'person:%s'}) RETURN count(*) AS n`, graph.EntityKey(fx.name))); n != 0 {
			t.Error("typed legacy key survived migration")
		}
		chunks, err := st.Chunks(ctx, fx.newID)
		if err != nil || len(chunks) != 1 || chunks[0].Text != fx.text {
			t.Errorf("stored chunks = %+v, err %v", chunks, err)
		}
	}
	if mock.Calls() != len(fixtures) {
		t.Errorf("extractions = %d, want %d", mock.Calls(), len(fixtures))
	}
}
