package graphtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// DatedEvidenceFixture has a searchable chunk and a fact for the same entity
// in each of two dated documents. Domain is also the shared search token.
type DatedEvidenceFixture struct {
	Store                          *graph.Store
	Domain, EntityID, OldID, NewID string
}

func DatedEvidence(t *testing.T) DatedEvidenceFixture {
	t.Helper()
	st := Store(t)
	ctx := t.Context()
	tag := fmt.Sprintf("datedevidence%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if _, err := st.DeleteEmptyDomain(context.Background(), tag); err != nil {
			t.Error(err)
		}
	})
	head := graph.EntityUpsert{Key: graph.DomainKey(tag, "fixture person"), Name: "Fixture person", Type: "person"}
	tail := graph.EntityUpsert{Key: graph.DomainKey(tag, "fixture company"), Name: "Fixture company", Type: "organization"}
	add := func(date string) string {
		t.Helper()
		stored, err := graph.ParseDocumentDate(date, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := st.UpsertDocument(ctx, graph.DocumentInput{
			Kind: "text", Source: tag + "-" + date, Hash: date, DocumentDate: stored,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := st.DeleteDocument(context.Background(), doc.ID); err != nil {
				t.Error(err)
			}
		})
		if err := st.AssignDomain(ctx, graph.Assignment{DocID: doc.ID, Domain: tag}); err != nil {
			t.Fatal(err)
		}
		if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Text: tag + " states where the fixture person works."}}); err != nil {
			t.Fatal(err)
		}
		facts := []graph.FactUpsert{{HeadKey: head.Key, TailKey: tail.Key, Rel: "works_at", Confidence: .9}}
		if err := st.UpsertExtractionOpts(ctx, doc.ID, doc.ID+"-c0", "mock:1",
			[]graph.EntityUpsert{head, tail}, facts, graph.WriteOptions{Domain: tag}); err != nil {
			t.Fatal(err)
		}
		return doc.ID
	}
	return DatedEvidenceFixture{
		Store: st, Domain: tag, EntityID: graph.EntityID(head.Key),
		OldID: add("2024-03-01T17:00:00Z"), NewID: add("2026-03-01T17:00:00Z"),
	}
}
