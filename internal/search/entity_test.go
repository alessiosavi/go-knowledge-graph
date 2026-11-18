package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestTopHitsBreaksExactTiesByMentionsThenID(t *testing.T) {
	hits := topHits([]Hit{
		{Kind: "entity", ID: "a", Score: 0.5, mentions: 1},
		{Kind: "entity", ID: "c", Score: 0.5, mentions: 2},
		{Kind: "entity", ID: "b", Score: 0.5, mentions: 2},
		{Kind: "entity", ID: "lower", Score: 0.499999, mentions: 100},
		{Kind: "entity", ID: "higher", Score: 0.500001},
	}, 5)
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	if want := []string{"higher", "b", "c", "a", "lower"}; !slices.Equal(ids, want) {
		t.Fatalf("topHits order = %v, want %v", ids, want)
	}
}

func TestTopHitsKeepsIDAroundTiedChunk(t *testing.T) {
	const score = 0.5
	chunk := Hit{Kind: "chunk", ID: "b", Score: score}
	moreMentioned := Hit{Kind: "entity", ID: "c", Score: score, mentions: 2}
	lessMentioned := Hit{Kind: "entity", ID: "a", Score: score, mentions: 1}
	t.Run("entity_and_chunk", func(t *testing.T) {
		hits := topHits([]Hit{moreMentioned, chunk}, 2)
		if want := []Hit{chunk, moreMentioned}; !slices.Equal(hits, want) {
			t.Fatalf("topHits = %+v, want %+v", hits, want)
		}
	})
	t.Run("chunk_between_entities_at_cutoff", func(t *testing.T) {
		hits := topHits([]Hit{moreMentioned, lessMentioned, chunk}, 2)
		if want := []Hit{lessMentioned, chunk}; !slices.Equal(hits, want) {
			t.Fatalf("topHits = %+v, want %+v", hits, want)
		}
	})
}

func TestHitJSONFields(t *testing.T) {
	h := Hit{Domain: "d", Kind: "entity", ID: "e", Title: "Entity", Score: 0.5, Snippet: "person", mentions: 42}
	got, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"Domain":"d","Kind":"entity","ID":"e","DocID":"","Title":"Entity","Score":0.5,"Snippet":"person"}`
	if string(got) != want {
		t.Fatalf("Hit JSON = %s, want %s", got, want)
	}
}

func TestEntityHitsBatchesAndFiltersVectorIDs(t *testing.T) {
	for _, staleCount := range []int{1, 9} {
		t.Run(fmt.Sprintf("%d_stale", staleCount), func(t *testing.T) {
			var stale []string
			for i := range staleCount {
				stale = append(stale, fmt.Sprintf("stale-%d", i))
			}
			st := &countingEntityStore{
				text: []graph.EntitySearchRow{
					{ID: "text", Domain: "scope", Name: "Text", MentionCount: 2},
					{ID: "cutoff", Domain: "scope", Name: "Cutoff", MentionCount: 1},
				},
				vector: append(slices.Clone(stale), "other", "first", "second", "text", "cutoff", "late"),
				// The batch may return rows in a different order than the index.
				rows: []graph.EntitySearchRow{
					{ID: "late", Domain: "scope", Name: "Late", MentionCount: 10},
					{ID: "second", Domain: "scope", Name: "Second", MentionCount: 3},
					{ID: "other", Domain: "other", Name: "Other", MentionCount: 20},
					{ID: "first", Domain: "scope", Name: "First", Type: "person", MentionCount: 2},
				},
			}
			const limit = 3
			candidates, err := entityCandidates(context.Background(), st, fixedQueryVector{}, "query", limit, "scope")
			if err != nil {
				t.Fatal(err)
			}
			hits := topHits(candidates, limit)
			wantIDs := append(slices.Clone(stale), "other", "first", "second", "late")
			if st.vectorLimit != 5*limit || len(st.lookups) != 1 || !slices.Equal(st.lookups[0], wantIDs) {
				t.Fatalf("vector request limit = %d, lookups = %v, want limit %d and one lookup for %v", st.vectorLimit, st.lookups, 5*limit, wantIDs)
			}
			want := []Hit{
				{Domain: "scope", Kind: "entity", ID: "text", Title: "Text", Score: RRF(0) + RRF(2), mentions: 2},
				{Domain: "scope", Kind: "entity", ID: "first", Title: "First", Score: RRF(0), Snippet: "person", mentions: 2},
				{Domain: "scope", Kind: "entity", ID: "second", Title: "Second", Score: RRF(1), mentions: 3},
			}
			if !slices.Equal(hits, want) {
				t.Fatalf("entityHits = %+v, want %+v", hits, want)
			}
		})
	}
}

func TestEntityHitsKeepsKnownIDsAfterBatchError(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	st := &countingEntityStore{
		text:   []graph.EntitySearchRow{{ID: "known", Name: "Known", Domain: "scope", MentionCount: 3}},
		vector: []string{"unknown-a", "known", "unknown-b"},
		err:    errors.New("lookup failed"),
	}
	hits, err := entityCandidates(context.Background(), st, fixedQueryVector{}, "query", 3, "scope")
	want := []Hit{{Domain: "scope", Kind: "entity", ID: "known", Title: "Known", Score: RRF(0) + RRF(0), mentions: 3}}
	if err != nil || !slices.Equal(hits, want) {
		t.Fatalf("entityCandidates = %+v, %v, want %+v, nil", hits, err, want)
	}
	if got := logs.String(); strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, st.err.Error()) {
		t.Fatalf("logs = %q, want one warning about the lookup failure", got)
	}
	st.vector = []string{"known"}
	st.lookups = nil
	logs.Reset()
	hits, err = entityCandidates(context.Background(), st, fixedQueryVector{}, "query", 3, "scope")
	if err != nil || !slices.Equal(hits, want) || len(st.lookups) != 0 || logs.Len() != 0 {
		t.Fatalf("known-only vector hits = %+v, %v, lookups = %v, logs = %q, want %+v, nil and no lookups or logs", hits, err, st.lookups, logs.String(), want)
	}
}

type fixedQueryVector struct{}

func (fixedQueryVector) EmbedQuery(context.Context, string) ([]float64, error) {
	return []float64{1}, nil
}

type countingEntityStore struct {
	text        []graph.EntitySearchRow
	vector      []string
	rows        []graph.EntitySearchRow
	err         error
	vectorLimit int
	lookups     [][]string
}

func (s *countingEntityStore) EntityTextSearchIn(context.Context, string, string, int) ([]graph.EntitySearchRow, error) {
	return s.text, nil
}

func (s *countingEntityStore) EntityVectorSearch(_ context.Context, _ []float64, limit int) ([]string, error) {
	s.vectorLimit = limit
	return s.vector, nil
}

func (s *countingEntityStore) EntitySearchRowsByID(_ context.Context, ids []string) ([]graph.EntitySearchRow, error) {
	s.lookups = append(s.lookups, slices.Clone(ids))
	return s.rows, s.err
}
