package graph_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestChunksContaining(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	quote := fmt.Sprintf("Zorbo%d", time.Now().UnixNano())
	var doneID string
	for i, status := range []graph.Status{graph.StatusDone, graph.StatusReceived} {
		d := testUpsert(t, st, "text", uniqueSource(t)+fmt.Sprint(i), "retrieval")
		t.Cleanup(func() {
			if err := st.DeleteDocument(ctx, d.ID); err != nil {
				t.Error(err)
			}
		})
		if err := st.PutChunks(ctx, d.ID, []graph.Chunk{{Seq: 0, Text: quote}, {Seq: 1, Text: "Again " + quote}}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetStatus(ctx, d.ID, status, ""); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			doneID = d.ID
			rawRun(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (d:Document {id: '%s'}) SET d.domain = 'quillon'", d.ID))
		}
	}
	hits, err := st.ChunksContaining(ctx, quote, "")
	if err != nil || len(hits) != 2 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	for _, h := range hits {
		if h.DocID != doneID || h.Source == "" || h.Domain != "quillon" {
			t.Errorf("gold metadata = %+v", h)
		}
	}
	for _, tc := range []struct {
		q, d string
		n    int
	}{{quote, "quillon", 2}, {quote, "other", 0}, {strings.ToLower(quote), "", 0}, {"unseenquillon", "", 0}} {
		h, err := st.ChunksContaining(ctx, tc.q, tc.d)
		if err != nil || len(h) != tc.n {
			t.Errorf("%+v = %+v, %v", tc, h, err)
		}
	}
	seen := map[string]bool{}
	if err := st.DoneChunkTexts(ctx, "quillon", func(id, text string) error {
		seen[id] = true
		if !strings.Contains(text, quote) {
			t.Error(text)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("streamed=%v", seen)
	}
	stop := errors.New("stop callback")
	if err := st.DoneChunkTexts(ctx, "quillon", func(string, string) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error = %v", err)
	}
}

// This benchmark helper measures a complete synthetic corpus, and removes it.
func TestChunksContainingSynthetic11000(t *testing.T) {
	if os.Getenv("GKG_TEST_SCALE") != "1" {
		t.Skip("set GKG_TEST_SCALE=1 to run the synthetic scale measurement")
	}
	st := graphtest.Store(t)
	ctx := context.Background()
	d := testUpsert(t, st, "text", uniqueSource(t), "synthetic-benchmark")
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, d.ID); err != nil {
			t.Error(err)
		}
		h, err := st.ChunksContaining(ctx, d.ID, "")
		if err != nil || len(h) != 0 {
			t.Errorf("benchmark cleanup: %v %v", h, err)
		}
	})
	chunks := make([]graph.Chunk, 11000)
	for i := range chunks {
		chunks[i] = graph.Chunk{Seq: i, Text: "Acme supplies Zorbo under Quillon terms. " + d.ID}
	}
	if err := st.PutChunks(ctx, d.ID, chunks); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus(ctx, d.ID, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	h, err := st.ChunksContaining(ctx, d.ID, "")
	elapsed := time.Since(start)
	if err != nil || len(h) != 11000 {
		t.Fatalf("matches=%d err=%v", len(h), err)
	}
	t.Logf("ChunksContaining: 11000 chunks, 11000 matches, elapsed=%s", elapsed)
	seen := map[string]bool{}
	if err := st.DoneChunkTexts(ctx, "", func(id, text string) error {
		if strings.Contains(text, d.ID) {
			if seen[id] {
				t.Errorf("duplicate chunk %s", id)
			}
			seen[id] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 11000 {
		t.Fatalf("streamed=%d", len(seen))
	}
}
