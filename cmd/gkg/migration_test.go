package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestIngestRejectsOutdatedExtractionSchema(t *testing.T) {
	st, id, _ := migrationCommandFixture(t)
	ctx := context.Background()
	for _, marker := range []string{"missing", "old"} {
		t.Run(marker, func(t *testing.T) {
			migrationQuery(t, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`, nil)
			if marker == "old" {
				migrationQuery(t, `CREATE (:GkgMeta {id: 'extraction_schema', value: 1})`, nil)
			}
			queries := []string{
				`MATCH (n) RETURN labels(n), properties(n) ORDER BY id(n)`,
				`MATCH (a)-[r]->(b) RETURN id(a), type(r), properties(r), id(b) ORDER BY id(r)`,
			}
			before := make([][]*neo4j.Record, len(queries))
			for i, query := range queries {
				before[i] = migrationQuery(t, query, nil)
			}
			var out, errb bytes.Buffer
			code := run([]string{"ingest", "unused.md"}, &out, &errb)
			want := "gkg ingest: graph extraction schema is outdated; run \"gkg migrate\" (re-extracts all documents) or start \"gkg serve\" first\n"
			if code != 1 || errb.String() != want || out.Len() != 0 {
				t.Fatalf("ingest = %d, stdout %q, stderr %q; want exit 1 and %q", code, &out, &errb, want)
			}
			for i, query := range queries {
				if after := migrationQuery(t, query, nil); !reflect.DeepEqual(before[i], after) {
					t.Errorf("ingest modified store: %s", query)
				}
			}
			doc, err := st.Document(ctx, id)
			if err != nil || doc.Status != graph.StatusDone {
				t.Errorf("legacy document = %+v, %v; want done", doc, err)
			}
		})
	}
}

func TestMigrateReextractsOutdatedGraph(t *testing.T) {
	st, id, name := migrationCommandFixture(t)
	// A pre-existing in-flight document must also be processed, even though
	// it is not one of the terminal documents the migration requeues.
	otherID, otherName := migrationCommandDocument(t, st, graph.StatusChunked)
	var out, errb bytes.Buffer
	if code := run([]string{"migrate"}, &out, &errb); code != 0 || errb.Len() != 0 {
		t.Fatalf("migrate = %d, stdout %q, stderr %q", code, &out, &errb)
	}
	if out.String() != "schema ok\nre-extraction: processed=2 failed=0\n" {
		t.Errorf("migrate stdout = %q", &out)
	}
	for docID, entityName := range map[string]string{id: name, otherID: otherName} {
		assertMigratedCommandDocument(t, st, docID, entityName)
	}
	if current, err := st.ExtractionSchemaCurrent(context.Background()); err != nil || !current {
		t.Errorf("schema current = %v, %v; want true, nil", current, err)
	}
}

func TestMigrateWithoutExtractorLeavesRecoveryQueued(t *testing.T) {
	st, id, name := migrationCommandFixture(t)
	t.Setenv("GKG_EXTRACTOR_MODE", "")
	var out, errb bytes.Buffer
	if code := run([]string{"migrate"}, &out, &errb); code != 1 {
		t.Fatalf("migrate = %d, stdout %q, stderr %q; want exit 1", code, &out, &errb)
	}
	if out.String() != "schema ok\n" || !strings.Contains(errb.String(), "unknown provider") || !strings.Contains(errb.String(), `start "gkg serve" with a usable extractor to finish the re-extraction`) {
		t.Fatalf("missing extractor: stdout %q, stderr %q", &out, &errb)
	}
	assertCommandRecoveryQueued(t, st, id)
	// A later run must recover the work even though the schema marker is
	// already current after the first run's successful destructive upgrade.
	t.Setenv("GKG_EXTRACTOR_MODE", "mock")
	out.Reset()
	errb.Reset()
	if code := run([]string{"migrate"}, &out, &errb); code != 0 {
		t.Fatalf("retry migrate = %d, stdout %q, stderr %q", code, &out, &errb)
	}
	assertMigratedCommandDocument(t, st, id, name)
}

func TestMigrateExtractorFailureKeepsRecoveryQueued(t *testing.T) {
	st, id, name := migrationCommandFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "extractor unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("GKG_EXTRACTOR_MODE", "sidecar")
	t.Setenv("GKG_EXTRACTOR_SIDECAR_URL", srv.URL)
	// Also exercise a received submission: Process persists its chunks and
	// drops the spool before the unavailable extractor causes a failure.
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	pipe, _, err := buildPipeline(context.Background(), cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	queuedName := name + "Queued"
	queued, err := pipe.Submit(context.Background(), ingest.Input{Kind: "text", Text: queuedName + " wrote another note.", Source: queuedName})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(context.Background(), queued.DocumentID); err != nil {
			t.Error(err)
		}
	})
	var out, errb bytes.Buffer
	if code := run([]string{"migrate"}, &out, &errb); code != 1 {
		t.Fatalf("migrate = %d, stdout %q, stderr %q; want exit 1", code, &out, &errb)
	}
	if out.String() != "schema ok\nre-extraction: processed=0 failed=2\n" || !strings.Contains(errb.String(), "extractor unavailable") || !strings.Contains(errb.String(), `start "gkg serve" with a usable extractor to finish the re-extraction`) {
		t.Fatalf("failed extractor: stdout %q, stderr %q", &out, &errb)
	}
	assertCommandRecoveryQueued(t, st, id, queued.DocumentID)
	t.Setenv("GKG_EXTRACTOR_MODE", "mock")
	out.Reset()
	errb.Reset()
	if code := run([]string{"migrate"}, &out, &errb); code != 0 {
		t.Fatalf("retry migrate = %d, stdout %q, stderr %q", code, &out, &errb)
	}
	assertMigratedCommandDocument(t, st, id, name)
	assertMigratedCommandDocument(t, st, queued.DocumentID, queuedName)
}

func migrationCommandFixture(t *testing.T) (*graph.Store, string, string) {
	t.Helper()
	st := graphtest.MigrationStore(t)
	t.Chdir(t.TempDir())
	for key, value := range map[string]string{
		"GKG_CONFIG": "", "GKG_MEMGRAPH_URI": os.Getenv(graphtest.URIEnv),
		"GKG_MEMGRAPH_USER": "", "GKG_MEMGRAPH_PASSWORD": "",
		"GKG_EXTRACTOR_MODE": "mock", "GKG_CONVERTER_MODE": "off",
		"GKG_EMBEDDER_ENABLED": "false", "GKG_JUDGE_MODE": "off",
		"GKG_INGEST_SPOOL_DIR": t.TempDir(),
	} {
		t.Setenv(key, value)
	}
	// Isolate foreign fixtures from this command's global recovery set;
	// MigrationStore restores their statuses, errors and timestamps.
	migrationQuery(t, `MATCH (d:Document) SET d.status = 'failed'`, nil)
	id, name := migrationCommandDocument(t, st, graph.StatusDone)
	migrationQuery(t, `CREATE (:Entity {key: $key}), (:RejectedFact {doc_id: $doc})`, map[string]any{"key": "person:" + graph.EntityKey(name), "doc": id})
	t.Cleanup(func() {
		migrationQuery(t, `MATCH (r:RejectedFact {doc_id: $doc}) DETACH DELETE r`, map[string]any{"doc": id})
	})
	migrationQuery(t, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`, nil)
	return st, id, name
}

func migrationCommandDocument(t *testing.T, st *graph.Store, status graph.Status) (string, string) {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("CliMigration%d", time.Now().UnixNano())
	text := name + " wrote a stored note."
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: name, Hash: "stored"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, res.ID); err != nil {
			t.Error(err)
		}
	})
	if err := st.PutChunks(ctx, res.ID, []graph.Chunk{{Text: text, End: len(text)}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus(ctx, res.ID, status, ""); err != nil {
		t.Fatal(err)
	}
	return res.ID, name
}

func assertMigratedCommandDocument(t *testing.T, st *graph.Store, id, name string) {
	t.Helper()
	ctx := context.Background()
	doc, err := st.Document(ctx, id)
	if err != nil || doc.Status != graph.StatusDone {
		t.Fatalf("migrated document = %+v, %v; want done", doc, err)
	}
	view, err := st.EntityView(ctx, graph.EntityKey(name), false)
	if err != nil || view.Key != graph.EntityKey(name) || len(view.Facts) != 1 {
		t.Errorf("name-only entity = %+v, %v; want one fact", view, err)
	}
	rows := migrationQuery(t, `MATCH (e:Entity {key: $key}) RETURN e.key`, map[string]any{"key": "person:" + graph.EntityKey(name)})
	if len(rows) != 0 {
		t.Error("typed legacy entity survived")
	}
}

func assertCommandRecoveryQueued(t *testing.T, st *graph.Store, ids ...string) {
	t.Helper()
	docs, err := st.InFlight(context.Background())
	if err != nil || len(docs) != len(ids) {
		t.Fatalf("recovery documents = %+v, %v; want %d queued fixtures", docs, err, len(ids))
	}
	queued := make(map[string]graph.Status)
	for _, doc := range docs {
		queued[doc.ID] = doc.Status
	}
	for _, id := range ids {
		if queued[id] != graph.StatusChunked {
			t.Errorf("recovery document %s status = %s; want chunked", id, queued[id])
		}
	}
	if rows := migrationQuery(t, `MATCH (e:Entity) RETURN e.key LIMIT 1`, nil); len(rows) != 0 {
		t.Error("migration did not clear legacy entities before extractor failure")
	}
}

func migrationQuery(t *testing.T, query string, params map[string]any) []*neo4j.Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drv, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(ctx)
	sess := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, query, params)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := res.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
