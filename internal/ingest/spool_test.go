package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// Embed the real store surface so unexpected graph calls fail the test;
// only the operations used by these durability scenarios are faked.
type spoolStore struct {
	*graph.Store
	mu             sync.Mutex
	docs           map[string]graph.Document
	chunks         map[string][]graph.Chunk
	beforeUpsert   func(graph.DocumentInput)
	older          []string
	readErr        error
	statusErr      error
	inFlightErr    error
	afterSupersede func()
}

func (s *spoolStore) Document(_ context.Context, id string) (*graph.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, s.readErr
	}
	d, ok := s.docs[id]
	if !ok {
		return nil, graph.ErrNotFound
	}
	return &d, nil
}
func (s *spoolStore) UpsertDocument(_ context.Context, in graph.DocumentInput) (graph.UpsertResult, error) {
	if s.beforeUpsert != nil {
		s.beforeUpsert(in)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := graph.DocumentID(in.Kind, in.Source, in.Hash)
	if d, ok := s.docs[id]; ok && d.Status != graph.StatusFailed && d.Status != graph.StatusSuperseded && !in.Force {
		return graph.UpsertResult{ID: id, Noop: true}, nil
	}
	s.docs[id] = graph.Document{ID: id, Hash: in.Hash, SourceKind: in.Kind, Source: in.Source, Title: in.Title, Labels: in.Labels, Status: graph.StatusReceived}
	return graph.UpsertResult{ID: id}, nil
}
func (s *spoolStore) SetStatus(_ context.Context, id string, status graph.Status, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusErr != nil {
		return s.statusErr
	}
	d := s.docs[id]
	d.Status, d.Error = status, msg
	s.docs[id] = d
	return nil
}
func (s *spoolStore) PutChunks(_ context.Context, id string, chunks []graph.Chunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range chunks {
		chunks[i].ID = fmt.Sprintf("%s-c%d", id, i)
	}
	s.chunks[id] = chunks
	return nil
}
func (s *spoolStore) DeleteChunksFrom(context.Context, string, int) error { return nil }
func (s *spoolStore) Chunks(_ context.Context, id string) ([]graph.Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chunks[id], nil
}
func (s *spoolStore) UpsertExtraction(context.Context, string, string, string, []graph.EntityUpsert, []graph.FactUpsert) error {
	return nil
}
func (s *spoolStore) WriteFactAudit(context.Context, graph.FactAudit) error { return nil }
func (s *spoolStore) OlderLiveVersions(context.Context, string) ([]string, error) {
	return s.older, nil
}
func (s *spoolStore) Supersede(ctx context.Context, old, _ string) error {
	err := s.SetStatus(ctx, old, graph.StatusSuperseded, "")
	if s.afterSupersede != nil {
		s.afterSupersede()
	}
	return err
}
func (s *spoolStore) InFlight(context.Context) ([]graph.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlightErr != nil {
		return nil, s.inFlightErr
	}
	var out []graph.Document
	for _, d := range s.docs {
		if d.Status != graph.StatusDone && d.Status != graph.StatusFailed && d.Status != graph.StatusSuperseded {
			out = append(out, d)
		}
	}
	return out, nil
}

type spoolProvider struct {
	extract.Mock
	before func()
	err    error
}

func (p *spoolProvider) Extract(ctx context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	if p.before != nil {
		p.before()
	}
	if p.err != nil {
		return nil, p.err
	}
	return p.Mock.Extract(ctx, req)
}

func spoolPipeline(t *testing.T) (*Pipeline, *spoolStore) {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ingest.SpoolDir = t.TempDir()
	cfg.Ingest.ChunkDedup = "off"
	st := &spoolStore{docs: make(map[string]graph.Document), chunks: make(map[string][]graph.Chunk)}
	p := NewPipeline(cfg, nil, &spoolProvider{}, nil, func(_ context.Context, in Input) (Loaded, error) {
		text := in.Text
		if len(in.Bytes) > 0 {
			text = string(in.Bytes)
		}
		return Loaded{Text: text, Title: in.Title}, nil
	})
	p.store = st
	return p, st
}
func spoolPath(p *Pipeline, id string) string {
	return filepath.Join(p.cfg.Ingest.SpoolDir, spoolName(id, p.store.(*spoolStore).docs[id].Hash))
}
func submitSpool(t *testing.T, p *Pipeline, in Input) Result {
	t.Helper()
	res, err := p.Submit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
func absentSpool(t *testing.T, p *Pipeline, id string) {
	t.Helper()
	if _, err := os.Stat(spoolPath(p, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("payload still present: %v", err)
	}
}

func TestSubmitSpoolsBeforeGraphAndQueue(t *testing.T) {
	p, st := spoolPipeline(t)
	st.beforeUpsert = func(in graph.DocumentInput) {
		id := graph.DocumentID(in.Kind, in.Source, in.Hash)
		path := filepath.Join(p.cfg.Ingest.SpoolDir, spoolName(id, in.Hash))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode: %v", info.Mode())
		}
		sub, err := p.spool.loadForTest(path)
		if err != nil || sub.Loaded.Text != "Converted upload." || sub.Loaded.Title != "Title" {
			t.Fatalf("payload: %+v %v", sub, err)
		}
		if len(p.jobs) != 0 {
			t.Fatal("queued before spool publication")
		}
	}
	res := submitSpool(t, p, Input{Kind: "file", Source: "notes.md", Bytes: []byte("Converted upload."), Title: "Title"})
	if id := <-p.jobs; id != res.DocumentID {
		t.Fatalf("queued %q", id)
	}
}

func (s *submissionSpool) loadForTest(path string) (spoolEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return spoolEntry{}, err
	}
	var sub spoolEntry
	err = json.Unmarshal(data, &sub)
	return sub, err
}

func TestSpoolRecoveryAndChunkedCleanup(t *testing.T) {
	for _, kind := range []string{"file", "text"} {
		for _, status := range []graph.Status{graph.StatusReceived, graph.StatusFetched} {
			t.Run(kind+"/"+string(status), func(t *testing.T) {
				p, st := spoolPipeline(t)
				in := Input{Kind: kind, Source: "notes", Text: "Ada wrote a note."}
				if kind == "file" {
					in.Bytes = []byte(in.Text)
					in.Text = ""
				}
				res := submitSpool(t, p, in)
				st.SetStatus(context.Background(), res.DocumentID, status, "")
				restarted := NewPipeline(p.cfg, nil, &spoolProvider{before: func() { absentSpool(t, p, res.DocumentID) }}, nil, nil)
				restarted.store = st
				if err := restarted.Recover(context.Background()); err != nil {
					t.Fatal(err)
				}
				if id := <-restarted.jobs; id != res.DocumentID {
					t.Fatalf("recovered %q", id)
				}
				if err := restarted.Process(context.Background(), res.DocumentID); err != nil {
					t.Fatal(err)
				}
				if st.docs[res.DocumentID].Status != graph.StatusDone {
					t.Fatal("not done")
				}
				absentSpool(t, p, res.DocumentID)
			})
		}
	}
}

func TestSpoolFailureAndSupersedeCleanup(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		p, st := spoolPipeline(t)
		res := submitSpool(t, p, Input{Text: "Original text."})
		// A corrupt converted payload is detected by the same hash check as reloads.
		sub := submission{loaded: Loaded{Text: "Changed text.", Hash: st.docs[res.DocumentID].Hash}}
		if err := p.spool.putLocked(res.DocumentID, sub); err != nil {
			t.Fatal(err)
		}
		p.pending = make(map[string]submission)
		if err := p.Process(context.Background(), res.DocumentID); err == nil || !strings.Contains(err.Error(), "source content changed") {
			t.Fatalf("hash mismatch: %v", err)
		}
		if st.docs[res.DocumentID].Status != graph.StatusFailed {
			t.Fatal("not failed")
		}
		absentSpool(t, p, res.DocumentID)
	})
	t.Run("superseded", func(t *testing.T) {
		p, st := spoolPipeline(t)
		old := submitSpool(t, p, Input{Source: "same", Text: "Old text."})
		next := submitSpool(t, p, Input{Source: "same", Text: "New text."})
		st.older = []string{old.DocumentID}
		if err := p.Process(context.Background(), next.DocumentID); err != nil {
			t.Fatal(err)
		}
		if st.docs[old.DocumentID].Status != graph.StatusSuperseded {
			t.Fatal("not superseded")
		}
		absentSpool(t, p, old.DocumentID)
	})
}

func TestRecoverSweepsOrphans(t *testing.T) {
	p, st := spoolPipeline(t)
	var keep []string
	for i, status := range []graph.Status{graph.StatusReceived, graph.StatusFetched, graph.StatusChunked, graph.StatusFailed, graph.StatusSuperseded, graph.StatusDone} {
		res := submitSpool(t, p, Input{Source: fmt.Sprint(i), Text: fmt.Sprintf("Document %d.", i)})
		st.SetStatus(context.Background(), res.DocumentID, status, "")
		if i < 2 {
			keep = append(keep, spoolPath(p, res.DocumentID))
		}
	}
	orphan := submitSpool(t, p, Input{Text: "Orphan."})
	delete(st.docs, orphan.DocumentID)
	for _, name := range []string{".spool-abandoned", "unrelated.txt"} {
		if err := os.WriteFile(filepath.Join(p.spool.dir, name), []byte("leftover"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(p.spool.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("retained %d files, want two payloads and unrelated file", len(entries))
	}
	for _, path := range keep {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSpoolWriteFailureRejectsSubmission(t *testing.T) {
	p, st := spoolPipeline(t)
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.spool.dir = path
	_, err := p.Submit(context.Background(), Input{Text: "No memory-only acceptance."})
	if !errors.Is(err, ErrSpool) || len(st.docs) != 0 || len(p.jobs) != 0 || len(p.pending) != 0 {
		t.Fatalf("submission: %v, docs/jobs/pending %d/%d/%d", err, len(st.docs), len(p.jobs), len(p.pending))
	}
}

func TestRecoverSpoolSweepFailureIsNonfatal(t *testing.T) {
	p, st := spoolPipeline(t)
	res := submitSpool(t, p, Input{Text: "Recover despite cleanup failure."})
	<-p.jobs
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.spool.dir = path
	if err := p.Recover(context.Background()); err != nil {
		t.Fatalf("cleanup error prevented recovery: %v", err)
	}
	select {
	case id := <-p.jobs:
		if id != res.DocumentID {
			t.Fatalf("recovered %q, want %q", id, res.DocumentID)
		}
	default:
		t.Fatal("in-flight document not requeued")
	}
	st.inFlightErr = errors.New("graph unavailable")
	if err := p.Recover(context.Background()); !errors.Is(err, st.inFlightErr) {
		t.Fatalf("InFlight error must remain fatal: %v", err)
	}
}

// Holding the processing lock simulates a worker stalled in extraction.
// Release it even on timeout, and drain Submit before the test cleans up.
func submitWithWorkerLock(t *testing.T, p *Pipeline, id string, in Input) (Result, error) {
	t.Helper()
	lock := p.docLock(id)
	lock.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := p.Submit(ctx, in)
		done <- outcome{res, err}
	}()
	select {
	case out := <-done:
		lock.Unlock()
		return out.res, out.err
	case <-ctx.Done():
		lock.Unlock()
		<-done
		t.Fatal("Submit waited for the worker's document lock")
		return Result{}, ctx.Err()
	}
}

func TestSubmitUnchangedWhileProcessingIsNoop(t *testing.T) {
	for _, status := range []graph.Status{graph.StatusReceived, graph.StatusFetched, graph.StatusChunked, graph.StatusExtracted, graph.StatusMerged, graph.StatusVerifying, graph.StatusDone} {
		for _, spool := range []string{"full", "unavailable"} {
			t.Run(string(status)+"/"+spool, func(t *testing.T) {
				p, st := spoolPipeline(t)
				in := Input{Text: "Unchanged text."}
				first := submitSpool(t, p, in)
				if err := st.SetStatus(context.Background(), first.DocumentID, status, ""); err != nil {
					t.Fatal(err)
				}
				if spool == "full" {
					p.spool.maxCount = 0
				} else {
					p.spool.dir = spoolPath(p, first.DocumentID) // A regular file.
				}
				res, err := submitWithWorkerLock(t, p, first.DocumentID, in)
				if err != nil || !res.Noop || res.DocumentID != first.DocumentID {
					t.Fatalf("duplicate submission: %+v, %v", res, err)
				}
				if len(p.jobs) != 1 || st.docs[first.DocumentID].Status != status {
					t.Fatal("no-op queued or reset the document")
				}
			})
		}
	}
}

func TestSubmitPayloadWhileProcessingIsBusy(t *testing.T) {
	for _, status := range []graph.Status{graph.StatusFetched, graph.StatusFailed, graph.StatusSuperseded} {
		t.Run(string(status), func(t *testing.T) {
			p, st := spoolPipeline(t)
			in := Input{Text: "Retry this text."}
			first := submitSpool(t, p, in)
			if err := st.SetStatus(context.Background(), first.DocumentID, status, ""); err != nil {
				t.Fatal(err)
			}
			p.forget(first.DocumentID)
			in.Force = status == graph.StatusFetched
			_, err := submitWithWorkerLock(t, p, first.DocumentID, in)
			if !errors.Is(err, ErrBusy) {
				t.Fatalf("submission with active worker: %v, want ErrBusy", err)
			}
			if len(p.jobs) != 1 || st.docs[first.DocumentID].Status != status {
				t.Fatal("busy submission queued or reset the document")
			}
			absentSpool(t, p, first.DocumentID)
		})
	}
}

// A stale read simulates another admission publishing before TryLock succeeds.
type staleAdmissionStore struct {
	*spoolStore
	reads int
}

func (s *staleAdmissionStore) Document(ctx context.Context, id string) (*graph.Document, error) {
	s.reads++
	if s.reads == 1 {
		return nil, graph.ErrNotFound
	}
	return s.spoolStore.Document(ctx, id)
}

func TestSubmitRechecksStatusBeforeSpooling(t *testing.T) {
	p, st := spoolPipeline(t)
	in := Input{Text: "Already admitted text."}
	first := submitSpool(t, p, in)
	p.spool.maxCount = 0 // Any payload write would fail.
	racing := &staleAdmissionStore{spoolStore: st}
	p.store = racing
	res, err := p.Submit(context.Background(), in)
	if err != nil || !res.Noop || res.DocumentID != first.DocumentID {
		t.Fatalf("concurrent admission: %+v, %v", res, err)
	}
	if racing.reads != 2 || len(p.jobs) != 1 {
		t.Fatalf("status reads/jobs = %d/%d, want 2/1", racing.reads, len(p.jobs))
	}
}

func TestSubmitFailedOrSupersededWithoutWorkerIsAccepted(t *testing.T) {
	for _, status := range []graph.Status{graph.StatusFailed, graph.StatusSuperseded} {
		t.Run(string(status), func(t *testing.T) {
			p, st := spoolPipeline(t)
			in := Input{Text: "Retry this text."}
			first := submitSpool(t, p, in)
			if err := st.SetStatus(context.Background(), first.DocumentID, status, ""); err != nil {
				t.Fatal(err)
			}
			p.forget(first.DocumentID)
			res := submitSpool(t, p, in)
			if res.Noop || res.DocumentID != first.DocumentID || st.docs[res.DocumentID].Status != graph.StatusReceived {
				t.Fatalf("retry submission: %+v, status %q", res, st.docs[res.DocumentID].Status)
			}
			if _, err := os.Stat(spoolPath(p, res.DocumentID)); err != nil {
				t.Fatalf("retry payload not persisted: %v", err)
			}
		})
	}
}

func TestSpoolCapsAndNoop(t *testing.T) {
	for _, limit := range []string{"count", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			p, st := spoolPipeline(t)
			first := submitSpool(t, p, Input{Text: "First."})
			if limit == "count" {
				p.spool.maxCount = 1
			} else {
				info, err := os.Stat(spoolPath(p, first.DocumentID))
				if err != nil {
					t.Fatal(err)
				}
				p.spool.maxBytes = info.Size()
			}
			_, err := p.Submit(context.Background(), Input{Text: "Second."})
			if !errors.Is(err, ErrSpoolFull) || len(st.docs) != 1 || len(p.jobs) != 1 {
				t.Fatalf("cap: %v, docs %d jobs %d", err, len(st.docs), len(p.jobs))
			}
			res, err := p.Submit(context.Background(), Input{Text: "First."})
			if err != nil || !res.Noop {
				t.Fatalf("noop at capacity: %+v %v", res, err)
			}
			if _, err := p.Submit(context.Background(), Input{Text: "First.", Force: true}); err != nil {
				t.Fatalf("replacement at capacity: %v", err)
			}
		})
	}
}

func TestSpoolConcurrentAdmissionCap(t *testing.T) {
	p, st := spoolPipeline(t)
	p.spool.maxCount = 2
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			_, err := p.Submit(context.Background(), Input{Text: fmt.Sprintf("Concurrent %d.", i)})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	accepted := 0
	for err := range errs {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrSpoolFull) {
			t.Fatal(err)
		}
	}
	if accepted != 2 || len(st.docs) != 2 {
		t.Fatalf("accepted %d documents, want 2", accepted)
	}
}

func TestTransientGraphFailurePreservesPayload(t *testing.T) {
	p, st := spoolPipeline(t)
	res := submitSpool(t, p, Input{Text: "Keep this text."})
	st.readErr = errors.New("temporarily unavailable")
	if err := p.Process(context.Background(), res.DocumentID); err == nil {
		t.Fatal("want graph error")
	}
	if _, err := os.Stat(spoolPath(p, res.DocumentID)); err != nil {
		t.Fatalf("payload lost on transient graph error: %v", err)
	}
	st.readErr = nil
	st.statusErr = errors.New("cannot record failure")
	if err := p.Process(context.Background(), res.DocumentID); err == nil {
		t.Fatal("want status error")
	}
	if _, err := os.Stat(spoolPath(p, res.DocumentID)); err != nil {
		t.Fatal(err)
	}
}

func TestSupersedeCleanupPreservesConcurrentResubmission(t *testing.T) {
	p, st := spoolPipeline(t)
	oldInput := Input{Source: "same", Text: "Old text."}
	old := submitSpool(t, p, oldInput)
	next := submitSpool(t, p, Input{Source: "same", Text: "New text."})
	st.older = []string{old.DocumentID}
	st.afterSupersede = func() { submitSpool(t, p, oldInput) }
	if err := p.Process(context.Background(), next.DocumentID); err != nil {
		t.Fatal(err)
	}
	if st.docs[old.DocumentID].Status != graph.StatusReceived {
		t.Fatal("resubmission not received")
	}
	if _, err := os.Stat(spoolPath(p, old.DocumentID)); err != nil {
		t.Fatalf("resubmitted payload removed: %v", err)
	}
}

func TestForcePresetSurvivesRestart(t *testing.T) {
	p, st := spoolPipeline(t)
	p.cfg.Labels.PresetsDir = t.TempDir()
	preset := []byte("entities:\n  - name: widget\n    threshold: 0.4\nrelations: []\n")
	if err := os.WriteFile(filepath.Join(p.cfg.Labels.PresetsDir, "custom.yaml"), preset, 0o600); err != nil {
		t.Fatal(err)
	}
	first := submitSpool(t, p, Input{Text: "Widget text."})
	st.SetStatus(context.Background(), first.DocumentID, graph.StatusDone, "")
	res := submitSpool(t, p, Input{Text: "Widget text.", Labels: "custom", Force: true})
	if res.Noop || st.docs[res.DocumentID].Labels != "custom" {
		t.Fatal("force did not persist preset")
	}
	restarted := NewPipeline(p.cfg, nil, &spoolProvider{}, nil, nil)
	restarted.store = st
	if err := restarted.Process(context.Background(), res.DocumentID); err != nil {
		t.Fatal(err)
	}
	if st.docs[res.DocumentID].Status != graph.StatusDone {
		t.Fatal("not done")
	}
}

func TestExtractionFailureReleasesSpool(t *testing.T) {
	p, st := spoolPipeline(t)
	p.prov = &spoolProvider{err: errors.New("inference unavailable")}
	res := submitSpool(t, p, Input{Text: "Document text."})
	if err := p.Process(context.Background(), res.DocumentID); err == nil {
		t.Fatal("want extraction failure")
	}
	if st.docs[res.DocumentID].Status != graph.StatusFailed {
		t.Fatal("not failed")
	}
	absentSpool(t, p, res.DocumentID)
}
