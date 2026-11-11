package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

type rebuildStore struct {
	*spoolStore
	resetIDs []string
	gcCalls  int
	resetErr map[string]error
	gcErr    error
}

func (s *rebuildStore) DoneDocumentIDs(context.Context) ([]string, error) {
	var ids []string
	for id, doc := range s.docs {
		if doc.Status == graph.StatusDone {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func (s *rebuildStore) ResetExtraction(_ context.Context, id, labels string) error {
	if err := s.resetErr[id]; err != nil {
		return err
	}
	doc, ok := s.docs[id]
	if !ok {
		return graph.ErrNotFound
	}
	if doc.Status != graph.StatusDone {
		return graph.ErrNotDone
	}
	s.resetIDs = append(s.resetIDs, id)
	doc.Status = graph.StatusChunked
	if labels != "" {
		doc.Labels = labels
	}
	s.docs[id] = doc
	return nil
}

func (s *rebuildStore) GC(context.Context) error {
	s.gcCalls++
	return s.gcErr
}

func rebuildPipeline(t *testing.T) (*Pipeline, *rebuildStore) {
	t.Helper()
	cfg := config.Config{}
	cfg.Labels.PresetsDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg.Labels.PresetsDir, "general.yaml"), []byte("entities:\n  - name: product\nrelations:\n  - name: uses\n"), 0600); err != nil {
		t.Fatal(err)
	}
	st := &rebuildStore{spoolStore: &spoolStore{docs: map[string]graph.Document{
		"a": {ID: "a", Status: graph.StatusDone, Labels: "general"},
		"b": {ID: "b", Status: graph.StatusDone, Labels: "general"},
	}}}
	p := NewPipeline(cfg, nil, nil, nil, nil)
	p.store = st
	p.queueCtx = t.Context()
	return p, st
}

func queuedID(t *testing.T, p *Pipeline) string {
	t.Helper()
	select {
	case id := <-p.jobs:
		return id
	case <-time.After(3 * time.Second):
		t.Fatal("reset document was not enqueued")
		return ""
	}
}

func TestRebuildSkipsBusyDocument(t *testing.T) {
	p, st := rebuildPipeline(t)
	before := st.docs["a"]
	p.docLock("a").Lock()
	defer p.docLock("a").Unlock()
	res, err := p.Rebuild(t.Context(), "", "")
	if err != nil || !slices.Equal(res.Busy, []string{"a"}) || !slices.Equal(res.Queued, []string{"b"}) {
		t.Fatalf("Rebuild = %+v, err=%v", res, err)
	}
	if st.docs["a"] != before || st.gcCalls != 1 || queuedID(t, p) != "b" {
		t.Fatalf("busy document changed or ready document not queued: %+v", st)
	}
}

func TestRebuildRejectsUnknownPresetFirst(t *testing.T) {
	for _, id := range []string{"", "missing", "a"} {
		t.Run(id, func(t *testing.T) {
			p, st := rebuildPipeline(t)
			for _, preset := range []string{"nope", "badscope"} {
				if err := os.WriteFile(filepath.Join(p.cfg.Labels.PresetsDir, "badscope.yaml"), []byte("entities:\n  - name: date\n    scope: invalid\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := p.Rebuild(t.Context(), id, preset); err == nil || errors.Is(err, graph.ErrNotFound) {
					t.Fatalf("invalid labels must fail before document lookup: %v", err)
				}
			}
			if len(st.resetIDs) != 0 || st.gcCalls != 0 || st.docs["a"].Status != graph.StatusDone || st.docs["b"].Status != graph.StatusDone {
				t.Fatalf("invalid preset mutated store: %+v", st)
			}
		})
	}
}

func TestRebuildUnknownDocumentAndNotDone(t *testing.T) {
	p, st := rebuildPipeline(t)
	if _, err := p.Rebuild(t.Context(), "missing", ""); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("unknown document: %v", err)
	}
	st.docs["c"] = graph.Document{ID: "c", Status: graph.StatusChunked}
	before := st.docs["c"]
	if _, err := p.Rebuild(t.Context(), "c", ""); !errors.Is(err, graph.ErrNotDone) {
		t.Fatalf("not done: %v", err)
	}
	if !reflect.DeepEqual(st.docs["c"], before) || len(st.resetIDs) != 0 {
		t.Fatal("refused rebuild changed documents")
	}
}

func TestRebuildQueuesCommittedResetsOnError(t *testing.T) {
	for _, gcFailure := range []bool{false, true} {
		p, st := rebuildPipeline(t)
		failure := errors.New("synthetic store failure")
		if gcFailure {
			st.gcErr = failure
		} else {
			st.resetErr = map[string]error{"b": failure}
		}
		res, err := p.Rebuild(t.Context(), "", "")
		if !errors.Is(err, failure) || !slices.Contains(res.Queued, "a") || queuedID(t, p) != "a" {
			t.Fatalf("committed reset lost on error: %+v err=%v", res, err)
		}
	}
}

func TestRebuildQueueOutlivesRequest(t *testing.T) {
	p, _ := rebuildPipeline(t)
	p.jobs = make(chan string, 1)
	p.jobs <- "blocker"
	ctx, cancel := context.WithCancel(t.Context())
	res, err := p.Rebuild(ctx, "a", "")
	cancel()
	if err != nil || !slices.Equal(res.Queued, []string{"a"}) {
		t.Fatalf("Rebuild = %+v, err=%v", res, err)
	}
	if queuedID(t, p) != "blocker" || queuedID(t, p) != "a" {
		t.Fatal("request cancellation lost queued document")
	}
}

// With labels omitted each document keeps its stored preset, so a preset that
// was removed or broken since extraction must refuse the request before any
// reset (Process would otherwise mark the reset document failed).
func TestRebuildRejectsInvalidStoredPresetFirst(t *testing.T) {
	for _, stored := range []string{"gone", "bad name"} {
		for _, id := range []string{"", "b"} {
			t.Run(stored+"/"+id, func(t *testing.T) {
				p, st := rebuildPipeline(t)
				doc := st.docs["b"]
				doc.Labels = stored
				st.docs["b"] = doc
				res, err := p.Rebuild(t.Context(), id, "")
				if err == nil || !strings.Contains(err.Error(), "document b") || errors.Is(err, ErrStore) {
					t.Fatalf("Rebuild = %+v, err=%v, want a preset error naming document b", res, err)
				}
				if len(st.resetIDs) != 0 || st.gcCalls != 0 || st.docs["a"].Status != graph.StatusDone || st.docs["b"].Status != graph.StatusDone {
					t.Fatalf("invalid stored preset mutated store: resets=%v gc=%d", st.resetIDs, st.gcCalls)
				}
				// An explicit valid preset replaces the stored one, so it may proceed.
				res, err = p.Rebuild(t.Context(), id, "general")
				if err != nil || len(res.Queued) == 0 || st.docs["b"].Labels != "general" {
					t.Fatalf("explicit preset Rebuild = %+v, err=%v, doc b = %+v", res, err, st.docs["b"])
				}
			})
		}
	}
}

// A document deleted between the bulk snapshot and its reset is skipped,
// not reported as a failure that stops the remaining targets.
func TestRebuildSkipsDocumentDeletedAfterSnapshot(t *testing.T) {
	p, st := rebuildPipeline(t)
	st.resetErr = map[string]error{"a": fmt.Errorf("graph: document a: %w", graph.ErrNotFound)}
	res, err := p.Rebuild(t.Context(), "", "")
	if err != nil || !slices.Equal(res.Queued, []string{"b"}) || len(res.Busy) != 0 {
		t.Fatalf("Rebuild = %+v, err=%v, want only b queued", res, err)
	}
	if queuedID(t, p) != "b" {
		t.Fatal("remaining document not queued")
	}
}
