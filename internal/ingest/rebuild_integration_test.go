package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

// Restrict bulk selection to this fixture: the throwaway database can contain
// other tests' documents, whose presets and source loaders belong to those tests.
// All reads, extraction writes, resets and GC still use the real graph store.
type rebuildFixtureStore struct {
	*graph.Store
	ids []string
}

func (s rebuildFixtureStore) DoneDocumentIDs(ctx context.Context) ([]string, error) {
	ids, err := s.Store.DoneDocumentIDs(ctx)
	return slices.DeleteFunc(ids, func(id string) bool { return !slices.Contains(s.ids, id) }), err
}

type rebuildJudge struct{ calls int }

func (j *rebuildJudge) VerifyFact(context.Context, FactInput) (Verdict, error) {
	j.calls++
	return Verdict{Status: "confirmed"}, nil
}

func TestRebuildEndToEnd(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ingest.SpoolDir = t.TempDir()
	cfg.Ingest.ChunkDedup = "off"
	cfg.Labels.PresetsDir = t.TempDir()
	cfg.Judge.Mode = "all"
	if err := os.WriteFile(filepath.Join(cfg.Labels.PresetsDir, "technology.yaml"), []byte("entities:\n  - name: technology\nrelations:\n  - name: uses\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mock, judge := &extract.Mock{}, &rebuildJudge{}
	loads := 0
	p := NewPipeline(cfg, st, mock, nil, func(_ context.Context, in Input) (Loaded, error) {
		loads++
		return Loaded{Text: in.Text}, nil
	})
	p.SetJudge(judge)
	name := fmt.Sprintf("SharedRebuild%d", time.Now().UnixNano())
	var ids []string
	before := map[string][]graph.Chunk{}
	for i, preset := range []string{"", "technology"} {
		res, err := p.Submit(ctx, Input{Kind: "text", Source: fmt.Sprintf("%s-%d", name, i), Text: fmt.Sprintf("%s uses %s-service%d.", name, name, i), Labels: preset})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.DocumentID)
		t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), res.DocumentID) })
		if err := p.Process(ctx, res.DocumentID); err != nil {
			t.Fatal(err)
		}
		before[res.DocumentID], err = st.Chunks(ctx, res.DocumentID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if loads != 2 || mock.Calls() != 2 || judge.calls != 2 {
		t.Fatalf("initial passes: loads=%d extractions=%d judgments=%d", loads, mock.Calls(), judge.calls)
	}
	// A fresh pipeline has no original payload or loader; only persisted chunks
	// can supply the next extraction. Consume its actual queue below.
	p = NewPipeline(cfg, st, mock, nil, nil)
	p.store = rebuildFixtureStore{Store: st, ids: ids}
	p.queueCtx = ctx
	p.SetJudge(judge)
	result, err := p.Rebuild(ctx, "", "")
	if err != nil || len(result.Queued) != 2 || len(result.Busy) != 0 {
		t.Fatalf("bulk rebuild = %+v, err=%v", result, err)
	}
	for range result.Queued {
		if err := p.Process(ctx, queuedID(t, p)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		doc, err := st.Document(ctx, id)
		if err != nil || doc.Status != graph.StatusDone {
			t.Fatalf("rebuilt document = %+v, err=%v", doc, err)
		}
		chunks, err := st.Chunks(ctx, id)
		if err != nil || !reflect.DeepEqual(chunks, before[id]) {
			t.Fatalf("stored chunks changed: before=%+v after=%+v err=%v", before[id], chunks, err)
		}
	}
	if loads != 2 || mock.Calls() != 4 || judge.calls != 4 {
		t.Fatalf("rebuild passes: loads=%d extractions=%d judgments=%d", loads, mock.Calls(), judge.calls)
	}
	// Every fixture entity key starts with the run's unique name, so the
	// invariants ignore rows other tests left in a shared database.
	prefix := map[string]any{"prefix": graph.EntityKey(name)}
	fixture, err := st.RunReadQuery(ctx, `MATCH (e:Entity) WHERE e.key STARTS WITH $prefix RETURN count(e) AS n`, prefix, 0)
	if err != nil || len(fixture.Rows) != 1 || fixture.Rows[0]["n"] != int64(3) {
		t.Fatalf("fixture entities = %+v err=%v, want the shared name and two services", fixture.Rows, err)
	}
	for _, query := range []string{
		`MATCH (e:Entity) WHERE e.key STARTS WITH $prefix
OPTIONAL MATCH (:Chunk)-[m:MENTIONS]->(e)
WITH e, count(m) AS n WHERE n = 0 RETURN e.key AS key`,
		`MATCH (e:Entity) WHERE e.key STARTS WITH $prefix
WITH e.key AS key, count(e) AS n WHERE n > 1 RETURN key, n`,
	} {
		rows, err := st.RunReadQuery(ctx, query, prefix, 0)
		if err != nil || len(rows.Rows) != 0 {
			t.Errorf("fixture invariant failed: %s: rows=%+v err=%v", query, rows.Rows, err)
		}
	}
	shared, err := st.RunReadQuery(ctx, `
MATCH (e:Entity {key: $key})
OPTIONAL MATCH (:Chunk)-[m:MENTIONS]->(e)
RETURN e.id AS id, e.mention_count AS stored, count(m) AS actual`, map[string]any{"key": graph.EntityKey(name)}, 0)
	if err != nil || len(shared.Rows) != 1 || shared.Rows[0]["stored"] != int64(2) || shared.Rows[0]["actual"] != int64(2) {
		t.Fatalf("shared entity must be one node with an accurate count: %+v err=%v", shared.Rows, err)
	}
}
