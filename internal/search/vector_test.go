package search_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

func TestEntitySearchBreaksRRFTiesByMentions(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	if err := st.EnsureVectorIndex(ctx, 768); err != nil {
		t.Fatal(err)
	}
	for _, moreMentions := range []string{"text", "vector"} {
		t.Run(moreMentions, func(t *testing.T) {
			query := uniqueToken(t)
			slug := "tie-" + query
			text := graph.EntityUpsert{Key: graph.DomainKey(slug, query), Name: query, Type: "technology"}
			vector := graph.EntityUpsert{Key: graph.DomainKey(slug, query+"vector"), Name: query + "vector", Type: "person"}
			textID, vectorID := graph.EntityID(text.Key), graph.EntityID(vector.Key)
			// Give the more-mentioned entity the larger id so id ordering
			// cannot pass this test by accident.
			if (moreMentions == "text") != (textID > vectorID) {
				text.Key, vector.Key = vector.Key, text.Key
				textID, vectorID = vectorID, textID
			}
			winner := text
			if moreMentions == "vector" {
				winner = vector
			}
			seedSearchEntities(t, st, slug, query, []graph.EntityUpsert{text, vector}, []graph.EntityUpsert{winner})
			vec := graphtest.Vector(query, 768)
			if err := st.SetEntityEmbedding(ctx, vectorID, vec); err != nil {
				t.Fatal(err)
			}
			hits, err := search.Query(ctx, st, vecEmbedder{vec}, query, "entity", 2, search.InDomain(slug))
			if err != nil {
				t.Fatal(err)
			}
			want := []string{graph.EntityID(winner.Key), min(textID, vectorID)}
			if len(hits) != 2 || hits[0].ID != want[0] || hits[1].ID != want[1] ||
				hits[0].Score != search.RRF(0) || hits[1].Score != search.RRF(0) {
				t.Fatalf("hits = %+v, want %v tied at score %v", hits, want, search.RRF(0))
			}
		})
	}
}

func BenchmarkScopedEntitySearchVector(b *testing.B) {
	st := graphtest.Store(b)
	ctx := context.Background()
	const limit, dim = 20, 768
	if err := st.EnsureVectorIndex(ctx, dim); err != nil {
		b.Fatal(err)
	}
	query := fmt.Sprintf("searchbench%d", time.Now().UnixNano())
	slug := "in-" + query
	vec := graphtest.Vector(query, dim)
	var ids []string
	var vectors [][]float64
	for _, group := range []struct {
		domain string
		count  int
	}{
		{"out-" + query, 4 * limit},
		{slug, limit},
	} {
		var ents []graph.EntityUpsert
		for i := 0; i < group.count; i++ {
			name := fmt.Sprintf("%s-%d", group.domain, i)
			key := graph.DomainKey(group.domain, name)
			ents = append(ents, graph.EntityUpsert{Key: key, Name: name, Type: "person"})
			ids = append(ids, graph.EntityID(key))
			v := slices.Clone(vec)
			v[dim-1] += float64(len(ids)) * 0.001
			vectors = append(vectors, v)
		}
		seedSearchEntities(b, st, group.domain, group.domain, ents)
	}
	if missing, err := st.SetEntityEmbeddings(ctx, ids, vectors); err != nil || missing != 0 {
		b.Fatalf("SetEntityEmbeddings: %d missing, %v", missing, err)
	}
	// No text hits. The 80 closer out-of-domain neighbours make a scoped
	// query resolve all 100 vector ids before filling its 20-hit limit.
	query += "unseen"
	for b.Loop() {
		if hits, err := search.Query(ctx, st, vecEmbedder{vec}, query, "entity", limit, search.InDomain(slug)); err != nil || len(hits) != limit {
			b.Fatalf("Query: %d hits, want %d; %v", len(hits), limit, err)
		}
	}
}

func seedSearchEntities(t testing.TB, st *graph.Store, slug, source string, entitiesByChunk ...[]graph.EntityUpsert) {
	t.Helper()
	ctx := context.Background()
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: source, Hash: source, Domain: slug})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, doc.ID); err != nil {
			t.Error(err)
		}
	})
	chunks := make([]graph.Chunk, len(entitiesByChunk))
	for i, ents := range entitiesByChunk {
		var text strings.Builder
		for j := range ents {
			start := text.Len()
			text.WriteString(ents[j].Name)
			ents[j].Mentions = []graph.MentionSpan{{Start: start, End: text.Len(), Type: ents[j].Type}}
			text.WriteByte(' ')
		}
		chunks[i] = graph.Chunk{Seq: i, Text: text.String()}
	}
	if err := st.PutChunks(ctx, doc.ID, chunks); err != nil {
		t.Fatal(err)
	}
	for i, ents := range entitiesByChunk {
		if err := st.UpsertExtractionOpts(ctx, doc.ID, fmt.Sprintf("%s-c%d", doc.ID, i), "mock", ents, nil, graph.WriteOptions{Domain: slug}); err != nil {
			t.Fatal(err)
		}
	}
}
