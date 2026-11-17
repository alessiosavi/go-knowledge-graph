package graph_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestNewerSchemaRefused(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	docID, _ := entitiesTestDoc(t, st, "Zorbo wrote a Quillon note.")
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, docID); err != nil {
			t.Error(err)
		}
	})
	if err := st.SetStatus(ctx, docID, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	newer := graph.ExtractionSchemaVersion + 1
	rawRun(t, uri, fmt.Sprintf(`MATCH (m:GkgMeta {id: 'extraction_schema'}) SET m.value = %d`, newer))
	// If Bootstrap gets past its guard, it would recreate this missing
	// index. Check DDL as well as graph data for accidental writes.
	rawRun(t, uri, `DROP INDEX ON :Entity(id)`)
	t.Cleanup(func() {
		if !rawIndexExists(t, uri, "Entity", "id") {
			rawRun(t, uri, `CREATE INDEX ON :Entity(id)`)
		}
	})
	queries := []string{
		`MATCH (n) RETURN labels(n), properties(n) ORDER BY id(n)`,
		`MATCH (a)-[r]->(b) RETURN id(a), type(r), properties(r), id(b) ORDER BY id(r)`,
	}
	before := make([]any, len(queries))
	for i, query := range queries {
		before[i] = rawRecords(t, uri, query)
	}
	for _, name := range []string{"Bootstrap", "ExtractionSchemaCurrent", "MigrateExtractionSchema"} {
		t.Run(name, func(t *testing.T) {
			var err error
			switch name {
			case "Bootstrap":
				err = st.Bootstrap(ctx)
			case "ExtractionSchemaCurrent":
				var current bool
				current, err = st.ExtractionSchemaCurrent(ctx)
				if current {
					t.Error("newer schema reported as current")
				}
			case "MigrateExtractionSchema":
				_, err = st.MigrateExtractionSchema(ctx)
			}
			if !errors.Is(err, graph.ErrSchemaNewer) {
				t.Errorf("%s error = %v; want ErrSchemaNewer", name, err)
			}
			for i, query := range queries {
				if after := rawRecords(t, uri, query); !reflect.DeepEqual(before[i], after) {
					t.Errorf("%s modified data: %s", name, query)
				}
			}
			if rawIndexExists(t, uri, "Entity", "id") {
				t.Error("Bootstrap executed DDL on a newer schema")
			}
			doc, err := st.Document(ctx, docID)
			if err != nil || doc.Status != graph.StatusDone {
				t.Errorf("document = %+v, %v; want preserved done fixture", doc, err)
			}
			if got := rawCount(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) RETURN m.value AS n`); got != int64(newer) {
				t.Errorf("marker = %d; want %d", got, newer)
			}
		})
	}
}

func TestSchemaVersionMissingMarker(t *testing.T) {
	st := graphtest.MigrationStore(t)
	uri := os.Getenv(graphtest.URIEnv)
	rawRun(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m`)
	version, err := st.SchemaVersion(context.Background())
	if err != nil || version != 1 {
		t.Fatalf("SchemaVersion = %d, %v; want 1, nil", version, err)
	}
	if n := rawCount(t, uri, `MATCH (m:GkgMeta {id: 'extraction_schema'}) RETURN count(*) AS n`); n != 0 {
		t.Error("SchemaVersion wrote a missing marker")
	}
}
