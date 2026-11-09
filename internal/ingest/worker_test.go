package ingest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func waitForStatus(t *testing.T, env pipelineEnv, docID string, want graph.Status, timeout time.Duration) *graph.Document {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		doc, err := env.st.Document(context.Background(), docID)
		if err != nil {
			t.Fatalf("Document: %v", err)
		}
		if doc.Status == want {
			return doc
		}
		if doc.Status == graph.StatusFailed {
			t.Fatalf("document failed: %s", doc.Error)
		}
		time.Sleep(25 * time.Millisecond)
	}
	doc, _ := env.st.Document(context.Background(), docID)
	t.Fatalf("document %s did not reach %q within %s (status %s, error %q)", docID, want, timeout, doc.Status, doc.Error)
	return nil
}

func TestWorkerProcessesSubmitted(t *testing.T) {
	env := newPipelineEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source, text := uniqueText(t)

	res, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	w := &ingest.Worker{Workers: 2, Pipeline: env.p}
	w.Start(ctx)
	waitForStatus(t, env, res.DocumentID, graph.StatusDone, 15*time.Second)
	if calls := env.mock.Calls(); calls != 1 {
		t.Errorf("provider calls = %d, want 1", calls)
	}
}

// TestRecoveryRequeuesInFlight covers both recovery shapes: a queued
// document requeued by Recover, and a document resumed from the chunked
// stage using only persisted state (no loader cache).
func TestRecoveryRequeuesInFlight(t *testing.T) {
	env := newPipelineEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Shape 1: submitted but never processed (process died), still cached.
	source1, text1 := uniqueText(t)
	res1, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source1, Text: text1})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Shape 2: a document that crashed after its chunks were persisted.
	source2 := fmt.Sprintf("text/%s-b-%d", t.Name(), time.Now().UnixNano())
	text2 := "Beta gamma delta epsilon zeta wrote earlier."
	res2, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source2, Text: text2})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := env.st.PutChunks(ctx, res2.DocumentID, []graph.Chunk{
		{Seq: 0, Text: text2, Start: 0, End: len(text2)},
	}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	if err := env.st.SetStatus(ctx, res2.DocumentID, graph.StatusChunked, ""); err != nil {
		t.Fatalf("SetStatus chunked: %v", err)
	}

	// Workers first (the wiring order): Recover applies backpressure on a
	// full queue, so the pool must already be draining.
	w := &ingest.Worker{Workers: 3, Pipeline: env.p}
	w.Start(ctx)
	if err := env.p.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The dev database can carry a large backlog of foreign in-flight
	// documents; our two must finish behind it.
	waitForStatus(t, env, res1.DocumentID, graph.StatusDone, 60*time.Second)
	waitForStatus(t, env, res2.DocumentID, graph.StatusDone, 60*time.Second)

	// One extraction per recovered document (the duplicate queue entry is
	// serialized by the per-document lock and no-ops).
	if calls := env.mock.Calls(); calls != 2 {
		t.Errorf("provider calls = %d, want 2 (one per recovered document)", calls)
	}
	docs, err := env.st.InFlight(ctx)
	if err != nil {
		t.Fatalf("InFlight: %v", err)
	}
	for _, d := range docs {
		if d.ID == res1.DocumentID || d.ID == res2.DocumentID {
			t.Errorf("document %s still in flight after recovery", d.ID)
		}
	}
}
