package search_test

import (
	"context"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

func BenchmarkSearchQuery(b *testing.B) {
	fx := newSearchFixture(b)
	ctx := context.Background()
	b.Cleanup(func() {
		if err := fx.st.DeleteDocument(ctx, fx.docID); err != nil {
			b.Error(err)
		}
	})
	for _, kind := range []string{"entity", "chunk", "auto"} {
		b.Run(kind, func(b *testing.B) {
			hits, err := search.Query(ctx, fx.st, nil, fx.token, kind, 10)
			if err != nil {
				b.Fatal(err)
			}
			found := make(map[string]bool, len(hits))
			for _, hit := range hits {
				found[hit.ID] = true
			}
			if kind != "chunk" && !found[fx.entityID] || kind != "entity" && !found[fx.chunkID] {
				b.Fatalf("Query %s did not return the fixture: %+v", kind, hits)
			}
			for b.Loop() {
				if _, err := search.Query(ctx, fx.st, nil, fx.token, kind, 10); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
