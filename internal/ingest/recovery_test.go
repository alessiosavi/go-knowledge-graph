package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

type recoveryLogs struct {
	slog.Handler
	started  chan slog.Record
	finished chan slog.Record
}

func (h recoveryLogs) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelInfo {
		switch r.Message {
		case "ingest: recovery enqueue started":
			h.started <- r.Clone()
		case "ingest: recovery enqueue finished":
			h.finished <- r.Clone()
		}
	}
	return h.Handler.Handle(ctx, r)
}

func TestRecoverQueuesBacklogWithoutWorkers(t *testing.T) {
	const recoveryTimeout = 10 * time.Second
	st := graphtest.Store(t)
	ctx := t.Context()
	existing, err := st.InFlight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("zorbo-recovery-%d-", time.Now().UnixNano())
	ids := make([]string, jobsBuffer+50)
	want := make(map[string]bool, len(existing)+len(ids))
	for _, doc := range existing {
		want[doc.ID] = true
	}
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%d", prefix, i)
		want[ids[i]] = true
	}
	d, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), recoveryTimeout)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	params := map[string]any{"ids": ids}
	_, err = neo4j.ExecuteQuery(ctx, d, `
UNWIND $ids AS id
CREATE (:Document {id: id, status: 'chunked', updated_at: '2026-01-01T00:00:00Z'})
`, params, neo4j.EagerResultTransformer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), recoveryTimeout)
		defer cancel()
		_, err := neo4j.ExecuteQuery(ctx, d, `
UNWIND $ids AS id
MATCH (d:Document {id: id})
DETACH DELETE d
`, params, neo4j.EagerResultTransformer)
		if err != nil {
			t.Error(err)
		}
	})

	logs := recoveryLogs{
		Handler:  slog.NewTextHandler(io.Discard, nil),
		started:  make(chan slog.Record, 2),
		finished: make(chan slog.Record, 2),
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(logs))
	defer slog.SetDefault(previous)

	cfg := config.Config{}
	cfg.Ingest.SpoolDir = t.TempDir()
	p := NewPipeline(cfg, st, nil, nil, nil)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	returned := make(chan error, 1)
	start := time.Now()
	go func() { returned <- p.Recover(ctx) }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(recoveryTimeout):
		t.Errorf("Recover still blocked after %s with no workers and %d in-flight documents", recoveryTimeout, len(want))
		cancel()
		select {
		case <-returned:
		case <-time.After(recoveryTimeout):
			t.Error("Recover did not return after cancellation")
		}
		return
	}
	t.Logf("Recover returned in %s with %d in-flight documents and no workers", time.Since(start), len(want))
	var finish slog.Record
	defer func() {
		cancel()
		if finish.Message == "" {
			select {
			case <-logs.finished:
			case <-time.After(recoveryTimeout):
				t.Error("recovery goroutine did not finish after cancellation")
			}
		}
	}()
	select {
	case record := <-logs.started:
		assertRecoveryCount(t, record, "documents", len(want))
	default:
		t.Fatal("recovery start was not logged")
	}

	drainDeadline := time.After(recoveryTimeout)
	total := len(want)
	for range total {
		select {
		case id := <-p.jobs:
			if !want[id] {
				t.Fatalf("unexpected or duplicate recovered id %q", id)
			}
			delete(want, id)
		case <-drainDeadline:
			t.Fatalf("%d recovered ids were not enqueued", len(want))
		}
	}
	select {
	case finish = <-logs.finished:
		assertRecoveryCount(t, finish, "queued", total)
	case <-drainDeadline:
		t.Fatal("recovery did not finish after enqueueing the backlog")
	}
	if len(p.jobs) != 0 || len(logs.started) != 0 || len(logs.finished) != 0 {
		t.Fatal("unexpected extra recovery jobs or INFO logs")
	}
}

func assertRecoveryCount(t *testing.T, r slog.Record, key string, want int) {
	t.Helper()
	got := int64(-1)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key && a.Value.Kind() == slog.KindInt64 {
			got = a.Value.Int64()
		}
		if a.Key == "doc" || a.Key == "id" {
			t.Errorf("recovery log contains document identifier: %v", a)
		}
		return true
	})
	if got != int64(want) {
		t.Errorf("%s count = %d, want %d", key, got, want)
	}
}

func TestRecoverCancellationStopsEnqueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, st := spoolPipeline(t)
		p.jobs = make(chan string, 1)
		for i := range 3 {
			id := fmt.Sprintf("plinko-%d", i)
			st.docs[id] = graph.Document{ID: id, Status: graph.StatusChunked}
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		returned := make(chan error, 1)
		go func() { returned <- p.Recover(ctx) }()
		synctest.Wait()
		select {
		case err := <-returned:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("Recover was blocked by its full queue")
		}
		if len(p.jobs) != 1 {
			t.Fatal("recovery did not fill the queue")
		}
		cancel()
		synctest.Wait()
		<-p.jobs
		synctest.Wait()
		if len(p.jobs) != 0 {
			t.Fatal("recovery enqueued another id after cancellation")
		}
	})
}

func TestSubmitOverlapsRecoveryOfSameDocument(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, _ := spoolPipeline(t)
		p.jobs = make(chan string, 1)
		p.load = DefaultLoader(nil, nil)
		in := Input{Source: "quillon-notes", Text: "Zorbo wrote a note."}
		first := submitSpool(t, p, in)
		// Keep the queue full so Recover still has this ID to enqueue.
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		returned := make(chan error, 1)
		go func() { returned <- p.Recover(ctx) }()
		synctest.Wait()
		select {
		case err := <-returned:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("Recover was blocked by its full queue")
		}
		if len(p.jobs) != 1 {
			t.Fatal("the original submission should still fill the queue")
		}

		<-p.jobs // Discard the original submission's queue entry.
		synctest.Wait()
		if len(p.jobs) != 1 {
			t.Fatal("recovery did not enqueue the document")
		}
		// Hold the recovery job until the forced submission has finished.
		staleID := <-p.jobs
		if staleID != first.DocumentID {
			t.Fatalf("recovery queued %q, want %q", staleID, first.DocumentID)
		}
		in.Force = true
		res := submitSpool(t, p, in)
		if res.Noop || res.DocumentID != first.DocumentID {
			t.Fatalf("overlapping submission result = %+v", res)
		}

		extractErr := errors.New("quillon extractor unavailable")
		provider := &spoolProvider{before: func() {
			lock := p.docLock(first.DocumentID)
			if lock.TryLock() {
				lock.Unlock()
				t.Error("processing did not hold the document lock during extraction")
			}
		}}
		provider.Err = extractErr
		p.prov = provider
		if err := p.Process(ctx, queuedID(t, p)); !errors.Is(err, extractErr) {
			t.Fatalf("forced submission error = %v, want %v", err, extractErr)
		}
		doc, err := p.store.Document(ctx, first.DocumentID)
		if err != nil || doc.Status != graph.StatusFailed || doc.Error != "extract: "+extractErr.Error() {
			t.Fatalf("after forced submission: %+v, %v", doc, err)
		}
		if err := p.Process(ctx, staleID); err != nil {
			t.Errorf("processing stale recovery job: %v", err)
		}
		current, err := p.store.Document(ctx, staleID)
		if err != nil || current.Status != graph.StatusFailed || current.Error != doc.Error {
			t.Errorf("after stale recovery job: %+v, %v, want failed with %q", current, err, doc.Error)
		}
		if calls := provider.Calls(); calls != 1 {
			t.Errorf("extractor calls = %d, want 1", calls)
		}

		provider.Err = nil
		in.Force = false
		retry := submitSpool(t, p, in)
		doc, err = p.store.Document(ctx, retry.DocumentID)
		if err != nil || retry.Noop || retry.DocumentID != first.DocumentID || doc.Status != graph.StatusReceived || doc.Error != "" {
			t.Fatalf("retry admission: %+v, document %+v, %v", retry, doc, err)
		}
		if err := p.Process(ctx, queuedID(t, p)); err != nil {
			t.Fatal(err)
		}
		doc, err = p.store.Document(ctx, retry.DocumentID)
		if err != nil || doc.Status != graph.StatusDone {
			t.Fatalf("retried document did not reach done: %+v, %v", doc, err)
		}
		if calls := provider.Calls(); calls != 2 {
			t.Fatalf("extractor calls after explicit retry = %d, want 2", calls)
		}
	})
}
