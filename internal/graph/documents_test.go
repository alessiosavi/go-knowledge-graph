package graph_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

// uniqueSource returns a source unique to this test run, so reruns against a
// shared dev database do not collide with earlier rows.
func uniqueSource(t *testing.T) string {
	t.Helper()
	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("file:%s-%d", name, time.Now().UnixNano())
}

func testUpsert(t *testing.T, st *graph.Store, kind, source, hash string) graph.UpsertResult {
	t.Helper()
	res, err := st.UpsertDocument(context.Background(), graph.DocumentInput{
		Kind: kind, Source: source, Title: "title", Hash: hash,
	})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	return res
}

func TestUpsertDocumentCreate(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	source := uniqueSource(t)
	res := testUpsert(t, st, "file", source, "hash-a")
	if res.Noop {
		t.Error("first upsert flagged Noop")
	}
	if res.PreviousID != "" {
		t.Errorf("PreviousID = %q, want empty", res.PreviousID)
	}
	if len(res.ID) != 32 {
		t.Errorf("ID = %q, want 32 hex chars", res.ID)
	}
	doc, err := st.Document(ctx, res.ID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.SourceKind != "file" || doc.Source != source || doc.Hash != "hash-a" || doc.Title != "title" {
		t.Errorf("Document fields = %+v", doc)
	}
	if doc.Status != graph.StatusReceived {
		t.Errorf("Status = %q, want %q", doc.Status, graph.StatusReceived)
	}
	if doc.CreatedAt.IsZero() || doc.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: %+v", doc)
	}
}

func TestUpsertSameHashNoop(t *testing.T) {
	st := graphtest.Store(t)
	source := uniqueSource(t)
	first := testUpsert(t, st, "file", source, "hash-a")
	second := testUpsert(t, st, "file", source, "hash-a")
	if !second.Noop {
		t.Error("same hash second upsert: want Noop")
	}
	if second.ID != first.ID {
		t.Errorf("Noop ID = %q, want %q", second.ID, first.ID)
	}
	if second.PreviousID != "" {
		t.Errorf("Noop PreviousID = %q, want empty", second.PreviousID)
	}
}

func TestUpsertChangedHashReturnsPreviousID(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	source := uniqueSource(t)
	first := testUpsert(t, st, "file", source, "hash-a")
	second := testUpsert(t, st, "file", source, "hash-b")
	if second.Noop {
		t.Error("changed hash: want Noop=false")
	}
	if second.ID == first.ID {
		t.Error("changed hash: new document must get a new id")
	}
	if second.PreviousID != first.ID {
		t.Errorf("PreviousID = %q, want %q", second.PreviousID, first.ID)
	}
	old, err := st.Document(ctx, first.ID)
	if err != nil {
		t.Fatalf("previous document must still exist until supersede: %v", err)
	}
	if old.Hash != "hash-a" {
		t.Errorf("old hash = %q, want hash-a", old.Hash)
	}
}

func TestPutChunksRoundtrip(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	res := testUpsert(t, st, "file", uniqueSource(t), "hash-a")
	chunks := []graph.Chunk{
		{Seq: 0, Text: "Città perché", Start: 0, End: 12, Hash: "h0", ExactKey: "e0",
			MinhashSig: []int64{1, -2, 3}, BandKeys: []int64{-4, 5}, SignatureVersion: 1},
		{Seq: 1, Text: "second chunk", Start: 14, End: 26, Hash: "h1", ExactKey: "e1",
			MinhashSig: []int64{7}, BandKeys: []int64{8}, SignatureVersion: 1},
	}
	if err := st.PutChunks(ctx, res.ID, chunks); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	got, err := st.Chunks(ctx, res.ID)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2", len(got))
	}
	for i, want := range chunks {
		g := got[i]
		wantID := fmt.Sprintf("%s-c%d", res.ID, want.Seq)
		if g.ID != wantID || g.DocID != res.ID || g.Seq != want.Seq {
			t.Errorf("chunk %d identity = %+v, want id %q seq %d", i, g, wantID, want.Seq)
		}
		if g.Text != want.Text || g.Start != want.Start || g.End != want.End {
			t.Errorf("chunk %d text/offsets = %+v", i, g)
		}
		if g.Hash != want.Hash || g.ExactKey != want.ExactKey {
			t.Errorf("chunk %d hash/exact key = %+v", i, g)
		}
		if !slices.Equal(g.MinhashSig, want.MinhashSig) || !slices.Equal(g.BandKeys, want.BandKeys) ||
			g.SignatureVersion != want.SignatureVersion {
			t.Errorf("chunk %d fingerprints = %+v", i, g)
		}
		if g.DuplicateOf != "" {
			t.Errorf("chunk %d duplicate_of = %q, want empty", i, g.DuplicateOf)
		}
	}
}

func TestStatusTransitions(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	res := testUpsert(t, st, "file", uniqueSource(t), "hash-a")

	if err := st.SetStatus(ctx, res.ID, graph.StatusFetched, ""); err != nil {
		t.Fatalf("SetStatus fetched: %v", err)
	}
	doc, err := st.Document(ctx, res.ID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusFetched {
		t.Errorf("after fetched: status = %q", doc.Status)
	}

	if err := st.SetStatus(ctx, res.ID, graph.StatusFailed, "boom"); err != nil {
		t.Fatalf("SetStatus failed: %v", err)
	}
	doc, err = st.Document(ctx, res.ID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusFailed || doc.Error != "boom" {
		t.Errorf("after failed: status = %q error = %q", doc.Status, doc.Error)
	}
}

func TestDocumentMissing(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	if _, err := st.Document(ctx, "does-not-exist"); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("Document(missing) error = %v, want ErrNotFound", err)
	}
	if err := st.SetStatus(ctx, "does-not-exist", graph.StatusDone, ""); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("SetStatus(missing) error = %v, want ErrNotFound", err)
	}
}

func TestDocumentsList(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	a := testUpsert(t, st, "file", uniqueSource(t), "hash-a")
	b := testUpsert(t, st, "file", uniqueSource(t), "hash-b")

	// Local ordering assertion: the newer document appears before the older
	// one. Avoids assuming this test is the database's only writer.
	docs, err := st.Documents(ctx, 0)
	if err != nil {
		t.Fatalf("Documents: %v", err)
	}
	ia, ib := -1, -1
	for i, d := range docs {
		if d.ID == a.ID {
			ia = i
		}
		if d.ID == b.ID {
			ib = i
		}
	}
	if ia < 0 || ib < 0 {
		t.Fatalf("seeded documents missing from list (a=%d, b=%d)", ia, ib)
	}
	if ib > ia {
		t.Errorf("order: newer b at %d, older a at %d; want b before a", ib, ia)
	}

	limited, err := st.Documents(ctx, 1)
	if err != nil {
		t.Fatalf("Documents(1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("Documents(1) returned %d rows, want 1", len(limited))
	}
}

// TestUpsertConcurrentSameHash pins that concurrent upserts of one source
// are serialized: exactly one run creates, all others see the no-op.
func TestUpsertConcurrentSameHash(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	source := uniqueSource(t)
	const workers = 8

	start := make(chan struct{})
	results := make([]graph.UpsertResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = st.UpsertDocument(ctx, graph.DocumentInput{
				Kind: "file", Source: source, Hash: "hash-a",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	created := 0
	for _, r := range results {
		if !r.Noop {
			created++
		}
	}
	if created != 1 {
		t.Errorf("non-noop upserts = %d, want exactly 1", created)
	}
	for i, r := range results {
		if r.ID != results[0].ID {
			t.Errorf("worker %d returned id %q, want %q", i, r.ID, results[0].ID)
		}
	}
}

// TestUpsertConcurrentDistinctHashes pins that concurrent versions of one
// source chain: exactly one sees no previous document; every other run
// supersedes the then-latest live version.
func TestUpsertConcurrentDistinctHashes(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	source := uniqueSource(t)
	const workers = 8

	start := make(chan struct{})
	results := make([]graph.UpsertResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = st.UpsertDocument(ctx, graph.DocumentInput{
				Kind: "file", Source: source, Hash: fmt.Sprintf("hash-%d", i),
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	creators := 0
	for _, r := range results {
		if r.PreviousID == "" {
			creators++
		}
	}
	if creators != 1 {
		t.Errorf("results with no PreviousID = %d, want exactly 1 (concurrent versions must chain)", creators)
	}
}

// TestUpsertNoopDuringSupersedeWindow pins that a status bump on the old
// version (which rewrites updated_at) cannot flip the no-op answer while
// both versions are still live awaiting supersede.
func TestUpsertNoopDuringSupersedeWindow(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	source := uniqueSource(t)
	a := testUpsert(t, st, "file", source, "hash-a")
	b := testUpsert(t, st, "file", source, "hash-b")
	if b.PreviousID != a.ID {
		t.Fatalf("PreviousID = %q, want %q", b.PreviousID, a.ID)
	}
	// Touch the old version after the new one was created.
	if err := st.SetStatus(ctx, a.ID, graph.StatusDone, ""); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	again := testUpsert(t, st, "file", source, "hash-b")
	if !again.Noop || again.ID != b.ID {
		t.Errorf("resubmitted live content = %+v, want Noop at %q", again, b.ID)
	}
}

// TestUpsertRetriesFailedDocument pins that resubmitting identical content
// after a failure retries it (reusing the node) instead of answering Noop.
func TestUpsertRetriesFailedDocument(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	source := uniqueSource(t)
	first := testUpsert(t, st, "file", source, "hash-a")
	if err := st.SetStatus(ctx, first.ID, graph.StatusFailed, "boom"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	retry := testUpsert(t, st, "file", source, "hash-a")
	if retry.Noop {
		t.Error("failed document resubmitted: want Noop=false so the pipeline retries")
	}
	if retry.ID != first.ID {
		t.Errorf("retry id = %q, want reuse of %q", retry.ID, first.ID)
	}
	if retry.PreviousID != "" {
		t.Errorf("retry PreviousID = %q, want empty (same node)", retry.PreviousID)
	}
	doc, err := st.Document(ctx, first.ID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusReceived {
		t.Errorf("status after retry = %q, want %q", doc.Status, graph.StatusReceived)
	}
}

func TestPutChunksMissingDocument(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	err := st.PutChunks(ctx, "no-such-doc", []graph.Chunk{{Seq: 0, Text: "x"}})
	if !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("PutChunks(missing) error = %v, want ErrNotFound", err)
	}
	if err := st.PutChunks(ctx, "no-such-doc", nil); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("PutChunks(missing, empty) error = %v, want ErrNotFound", err)
	}
}

func TestChunksMissingDocument(t *testing.T) {
	st := graphtest.Store(t)
	if _, err := st.Chunks(context.Background(), "no-such-doc"); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("Chunks(missing) error = %v, want ErrNotFound", err)
	}
}
