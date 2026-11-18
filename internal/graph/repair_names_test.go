package graph_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestRepairEntityNameSkipsChangedName(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	uri := os.Getenv(graphtest.URIEnv)
	const old = "(Mira Zorbin)"
	docID, chunkID := entitiesTestDoc(t, st, old)
	t.Cleanup(func() {
		if err := st.DeleteDocument(context.Background(), docID); err != nil {
			t.Error(err)
		}
	})
	ents := []graph.EntityUpsert{{Key: graph.ScopedEntityKey(docID, old), Name: old, Type: "person",
		Mentions: []graph.MentionSpan{{Start: 0, End: len(old), Type: "person"}}}}
	if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:1", ents, nil); err != nil {
		t.Fatal(err)
	}
	query := fmt.Sprintf(`MATCH (e:Entity {id: '%s'}) RETURN e.id AS id, e.name AS name`, graph.EntityID(ents[0].Key))
	page := rawRecords(t, uri, query)
	if len(page) != 1 || page[0].AsMap()["name"] != old {
		t.Fatalf("read page: %v", page)
	}
	// Emulate ingest committing after the repair's read and before its write.
	ents[0].Name = "MIRA ZORBIN"
	if err := st.UpsertExtraction(ctx, docID, chunkID, "mock:1", ents, nil); err != nil {
		t.Fatal(err)
	}
	changed, err := graph.RepairEntityNamePageForTest(st, ctx, page, false)
	if err != nil || changed != 0 {
		t.Fatalf("repair with stale name = %d, %v, want zero changes", changed, err)
	}
	if rows := rawRecords(t, uri, query); len(rows) != 1 || rows[0].AsMap()["name"] != "MIRA ZORBIN" {
		t.Fatalf("repair overwrote ingest's newer name: %v", rows)
	}
}
