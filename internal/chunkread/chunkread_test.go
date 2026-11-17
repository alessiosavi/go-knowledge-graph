package chunkread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

type fakeStore struct {
	chunks []graph.Chunk
	err    error
	calls  int
}

func (s *fakeStore) ChunkNeighbourhood(ctx context.Context, id string, n int) (graph.Chunk, []graph.Chunk, error) {
	s.calls++
	if err := ctx.Err(); err != nil {
		return graph.Chunk{}, nil, err
	}
	if s.err != nil {
		return graph.Chunk{}, nil, s.err
	}
	for _, c := range s.chunks {
		if c.ID != id {
			continue
		}
		var around []graph.Chunk
		for _, other := range s.chunks {
			if other.DocID == c.DocID && other.ID != id && other.Seq >= c.Seq-n && other.Seq <= c.Seq+n {
				around = append(around, other)
			}
		}
		return c, around, nil
	}
	return graph.Chunk{}, nil, fmt.Errorf("chunk %s: %w", id, graph.ErrNotFound)
}

func resultJSON(t *testing.T, result Result) []byte {
	t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadBounds(t *testing.T) {
	if MaxIDs != 20 || MaxNeighbours != 2 || ChunkTextCap != 4096 || ResponseBudget != 24576 {
		t.Fatal("chunk read bounds changed")
	}
	for _, tc := range []struct {
		name string
		req  Request
		want string
	}{
		{"empty", Request{}, "ids must hold 1–20 chunk ids"},
		{"too_many", Request{IDs: make([]string, 21)}, "ids must hold 1–20 chunk ids"},
		{"negative_neighbours", Request{IDs: []string{"c1"}, Neighbours: -1}, "neighbours"},
		{"too_many_neighbours", Request{IDs: []string{"c1"}, Neighbours: 3}, "neighbours"},
		{"long_id", Request{IDs: []string{strings.Repeat("z", 129)}}, "chunk ids must be at most 128 bytes"},
		{"long_later_id", Request{IDs: []string{"c1", strings.Repeat("z", 129)}}, "chunk ids must be at most 128 bytes"},
		{"long_unicode_id", Request{IDs: []string{strings.Repeat("界", 43)}}, "chunk ids must be at most 128 bytes"},
		{"count_before_id_length", Request{IDs: slices.Repeat([]string{strings.Repeat("z", 129)}, 21)}, "ids must hold 1–20 chunk ids"},
		{"neighbours_before_id_length", Request{IDs: []string{strings.Repeat("z", 129)}, Neighbours: 3}, "neighbours must be between 0 and 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			_, err := Read(context.Background(), st, tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if strings.HasPrefix(tc.name, "long_") && err.Error() != tc.want {
				t.Fatalf("error = %q, want exactly %q", err.Error(), tc.want)
			}
			if st.calls != 0 {
				t.Fatal("invalid request reached the store")
			}
		})
	}
	ids := make([]string, 20)
	for i := range ids {
		ids[i] = fmt.Sprintf("missing%02d", i) + strings.Repeat("z", 119)
	}
	result, err := Read(context.Background(), &fakeStore{}, Request{IDs: ids, Neighbours: 2})
	if err != nil || !slices.Equal(result.Meta.Missing, ids) {
		t.Fatalf("valid upper bounds: result=%+v err=%v", result, err)
	}
	if size := len(resultJSON(t, result)); size > ResponseBudget {
		t.Fatalf("20 missing 128-byte ids: serialized=%d exceeds %d", size, ResponseBudget)
	} else {
		t.Logf("20 missing 128-byte ids: serialized=%d", size)
	}
}

func TestReadTextCap(t *testing.T) {
	for _, text := range []string{strings.Repeat("x", 5000), strings.Repeat("界", 1666) + "ab", strings.Repeat("x", 4096)} {
		st := &fakeStore{chunks: []graph.Chunk{{ID: "c1", DocID: "d1", Seq: 1, Text: text}}}
		result, err := Read(context.Background(), st, Request{IDs: []string{"c1"}})
		if err != nil || len(result.Chunks) != 1 {
			t.Fatalf("Read: %+v, %v", result, err)
		}
		item := result.Chunks[0]
		wantTruncated := len(text) > ChunkTextCap
		if len(item.Text) > ChunkTextCap || !utf8.ValidString(item.Text) || !strings.HasPrefix(text, item.Text) || item.Truncated != wantTruncated {
			t.Fatalf("text bytes=%d valid=%v truncated=%v", len(item.Text), utf8.ValidString(item.Text), item.Truncated)
		}
		if wantTruncated && !slices.Equal(result.Meta.Truncated, []string{"c1"}) {
			t.Fatalf("truncated ids = %v", result.Meta.Truncated)
		}
		if !wantTruncated && len(result.Meta.Truncated) != 0 {
			t.Fatalf("uncut chunk listed in truncated: %v", result.Meta.Truncated)
		}
		t.Logf("input=%d text=%d serialized=%d", len(text), len(item.Text), len(resultJSON(t, result)))
	}
}

func TestReadBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		idLen int
	}{
		{"plain", strings.Repeat("x", 4096), 2},
		{"escaped", strings.Repeat("\"\\", 2048), 2},
		{"long_ids", strings.Repeat("x", 4096), 120},
		{"escaped_long_ids", strings.Repeat("\"\\", 2048), 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			var ids []string
			for i := 0; i < 8; i++ {
				id := fmt.Sprintf("c%d", i) + strings.Repeat("z", tc.idLen-2)
				ids = append(ids, id)
				st.chunks = append(st.chunks, graph.Chunk{ID: id, DocID: "d1", Seq: i, Text: tc.text})
			}
			result, err := Read(context.Background(), st, Request{IDs: ids})
			if err != nil {
				t.Fatal(err)
			}
			b := resultJSON(t, result)
			if len(b) > ResponseBudget || len(result.Chunks) == 0 || len(result.Chunks) == len(ids) {
				t.Fatalf("serialized bytes=%d chunks=%d", len(b), len(result.Chunks))
			}
			if result.Meta.Returned != len(result.Chunks) || !slices.Equal(result.Meta.OmittedForBudget, ids[len(result.Chunks):]) {
				t.Fatalf("meta = %+v", result.Meta)
			}
			for i, item := range result.Chunks {
				if item.ID != ids[i] || item.Text != tc.text {
					t.Fatalf("item %d is not the requested prefix", i)
				}
			}
			// The next item must exceed the budget even after removing its
			// id from omitted_for_budget. This checks the full JSON envelope.
			next := st.chunks[len(result.Chunks)]
			probe := result
			probe.Chunks = append(probe.Chunks, Item{ID: next.ID, DocID: next.DocID, Seq: next.Seq, Text: next.Text})
			probe.Meta.Returned++
			probe.Meta.OmittedForBudget = probe.Meta.OmittedForBudget[1:]
			if len(resultJSON(t, probe)) <= ResponseBudget {
				t.Fatal("omitted an item that still fits")
			}
			t.Logf("serialized=%d returned=%d omitted=%d", len(b), result.Meta.Returned, len(result.Meta.OmittedForBudget))
		})
	}
}

func TestReadOrderAndDeduplication(t *testing.T) {
	st := &fakeStore{}
	for i := 1; i <= 6; i++ {
		st.chunks = append(st.chunks, graph.Chunk{ID: fmt.Sprintf("c%d", i), DocID: "d1", Seq: i, Text: "Zorbo"})
	}
	for _, tc := range []struct {
		ids  []string
		n    int
		want []string
	}{
		{[]string{"c5", "c1"}, 1, []string{"c5", "c4", "c6", "c1", "c2"}},
		{[]string{"c5", "c4", "c5"}, 1, []string{"c5", "c4", "c6", "c3"}},
		{[]string{"c3"}, 2, []string{"c3", "c1", "c2", "c4", "c5"}},
	} {
		result, err := Read(context.Background(), st, Request{IDs: tc.ids, Neighbours: tc.n})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, item := range result.Chunks {
			got = append(got, item.ID)
			if item.NeighbourOf != "" && !slices.Contains(tc.ids, item.NeighbourOf) {
				t.Fatalf("invalid neighbour_of = %q", item.NeighbourOf)
			}
		}
		if !slices.Equal(got, tc.want) || result.Meta.Returned != len(tc.want) || result.Chunks[0].NeighbourOf != "" {
			t.Fatalf("order=%v meta=%+v, want %v", got, result.Meta, tc.want)
		}
		if result.Chunks[1].NeighbourOf != tc.ids[0] {
			t.Fatalf("neighbour_of = %q, want %s", result.Chunks[1].NeighbourOf, tc.ids[0])
		}
	}
}

func TestReadMissingAndJSONKeys(t *testing.T) {
	st := &fakeStore{chunks: []graph.Chunk{{ID: "c1", DocID: "d1", Seq: 1, Text: "Quillon Ltd"}}}
	result, err := Read(context.Background(), st, Request{IDs: []string{"absent", "c1"}})
	if err != nil || len(result.Chunks) != 1 || !slices.Equal(result.Meta.Missing, []string{"absent"}) {
		t.Fatalf("missing: %+v, %v", result, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(resultJSON(t, result), &decoded); err != nil {
		t.Fatal(err)
	}
	assertKeys := func(obj map[string]any, want ...string) {
		t.Helper()
		var got []string
		for k := range obj {
			got = append(got, k)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("keys=%v want=%v", got, want)
		}
	}
	assertKeys(decoded, "chunks", "meta")
	meta := decoded["meta"].(map[string]any)
	assertKeys(meta, "returned", "missing", "truncated", "omitted_for_budget")
	for _, key := range []string{"missing", "truncated", "omitted_for_budget"} {
		if _, ok := meta[key].([]any); !ok {
			t.Errorf("meta.%s is not an array: %v", key, meta[key])
		}
	}
	assertKeys(decoded["chunks"].([]any)[0].(map[string]any), "id", "doc_id", "seq", "text")
}

func TestReadStoreErrors(t *testing.T) {
	want := errors.New("synthetic store failure")
	if _, err := Read(context.Background(), &fakeStore{err: want}, Request{IDs: []string{"c1"}}); !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Read(ctx, &fakeStore{}, Request{IDs: []string{"c1"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want cancellation", err)
	}
}

func TestReadBudgetIncludesAllMetadata(t *testing.T) {
	st := &fakeStore{}
	for i := 0; i < 7; i++ {
		st.chunks = append(st.chunks, graph.Chunk{ID: fmt.Sprintf("c%d", i), DocID: "d1", Seq: i, Text: strings.Repeat("x", 5000)})
	}
	st.chunks = append(st.chunks, graph.Chunk{ID: "c7", DocID: "d1", Seq: 7, Text: "small tail"})
	missing := strings.Repeat("\"\\", 50)
	result, err := Read(context.Background(), st, Request{IDs: []string{"c1", missing, "c4", "c7"}, Neighbours: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(resultJSON(t, result)) > ResponseBudget || !slices.Equal(result.Meta.Missing, []string{missing}) || !slices.Equal(result.Meta.OmittedForBudget, []string{"c5", "c6", "c7"}) {
		t.Fatalf("result metadata = %+v", result.Meta)
	}
	for i, item := range result.Chunks {
		if !item.Truncated || result.Meta.Truncated[i] != item.ID {
			t.Fatalf("truncation metadata = %+v", result.Meta)
		}
	}
	if len(result.Meta.Truncated) != len(result.Chunks) {
		t.Fatal("truncated includes omitted chunks")
	}
}

func TestReadDomain(t *testing.T) {
	st := &fakeStore{chunks: []graph.Chunk{{ID: "a", DocID: "zorbo", Domain: "a", Seq: 0, Text: "Quillon"}, {ID: "b", DocID: "zorbo", Domain: "b", Seq: 1, Text: "Acme"}}}
	got, err := Read(context.Background(), st, Request{IDs: []string{"a", "b"}, Neighbours: 1, Domain: "a"})
	if err != nil || len(got.Chunks) != 1 || got.Chunks[0].ID != "a" || !reflect.DeepEqual(got.Meta.Missing, []string{"b"}) {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if _, err := Read(context.Background(), st, Request{IDs: []string{"a"}, Domain: "BAD"}); err == nil {
		t.Fatal("invalid domain accepted")
	}
}

func TestReadDomainNeighbours(t *testing.T) {
	st := &fakeStore{chunks: []graph.Chunk{
		{ID: "before", DocID: "quillon", Domain: "a", Seq: 0, Text: "Acme"},
		{ID: "center", DocID: "quillon", Domain: "a", Seq: 1, Text: "Quillon"},
		{ID: "outside", DocID: "quillon", Domain: "b", Seq: 2, Text: "Zorbo"},
	}}
	for _, tc := range []struct {
		domain string
		want   []string
	}{
		{"a", []string{"center", "before"}},
		{"", []string{"center", "before", "outside"}},
	} {
		t.Run("domain="+tc.domain, func(t *testing.T) {
			got, err := Read(context.Background(), st, Request{IDs: []string{"center"}, Neighbours: 1, Domain: tc.domain})
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, item := range got.Chunks {
				ids = append(ids, item.ID)
				if item.ID != "center" && item.NeighbourOf != "center" {
					t.Errorf("neighbour_of=%q for %s", item.NeighbourOf, item.ID)
				}
			}
			if !slices.Equal(ids, tc.want) || got.Meta.Returned != len(tc.want) || len(got.Meta.Missing) != 0 {
				t.Fatalf("ids=%v meta=%+v, want %v without missing requested chunks", ids, got.Meta, tc.want)
			}
		})
	}
}
