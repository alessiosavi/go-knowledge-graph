package graphtest

import (
	"context"
	"os"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// MigrationStore is for global, destructive extraction-migration tests on
// the disposable integration DB only. Run packages with -p 1 and never use
// t.Parallel: migrations intentionally delete ALL derived extraction data.
// Restore pre-existing document statuses/timestamps so later recovery tests
// do not process a backlog created by this test. Callers remove their own
// tagged document fixtures; the current schema marker is restored on failure.
func MigrationStore(t *testing.T) *graph.Store {
	t.Helper()
	st := Store(t)
	ctx := context.Background()
	docs, err := st.RunReadQuery(ctx, `MATCH (d:Document) RETURN d.id AS id, d.status AS status, d.error AS error, d.updated_at AS updated_at`, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		drv, err := neo4j.NewDriver(os.Getenv(URIEnv), neo4j.NoAuth())
		if err != nil {
			t.Error(err)
			return
		}
		defer drv.Close(ctx)
		sess := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
		defer sess.Close(ctx)
		for _, q := range []struct {
			query  string
			params map[string]any
		}{
			{`UNWIND $docs AS row MATCH (d:Document {id: row.id}) SET d.status = row.status, d.error = row.error, d.updated_at = row.updated_at`, map[string]any{"docs": docs.Rows}},
			{`MERGE (m:GkgMeta {id: 'extraction_schema'}) SET m.value = $version`, map[string]any{"version": graph.ExtractionSchemaVersion}},
		} {
			res, err := sess.Run(ctx, q.query, q.params)
			if err == nil {
				_, err = res.Consume(ctx)
			}
			if err != nil {
				t.Errorf("restore migration fixture state: %v", err)
			}
		}
	})
	return st
}
