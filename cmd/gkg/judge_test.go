package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func TestJudgeAllCachedReExtraction(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ingest.SpoolDir = t.TempDir()
	cfg.Ingest.ChunkDedup = "off"
	cfg.Judge.Mode = "all"
	cfg.Judge.Model = "test-model"
	var calls atomic.Int32
	var (
		keysMu sync.Mutex
		keys   []string
	)
	statuses := []string{"confirmed", "unclear", "refuted"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if len(body.Messages) != 2 || body.Messages[1].Role != "user" {
			t.Errorf("messages: %+v", body.Messages)
			return
		}
		keysMu.Lock()
		keys = append(keys, judge.RequestKey([]byte(body.Messages[1].Content), body.Model))
		keysMu.Unlock()
		n := calls.Add(1)
		status := statuses[(int(n)-1)%len(statuses)]
		content, _ := json.Marshal(map[string]any{"verdict": status, "reason": "synthetic evidence", "confidence": 0.8})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(content)}}}})
	}))
	defer srv.Close()
	cfg.Judge.BaseURL = srv.URL
	prov := &extract.Mock{}
	p := ingest.NewPipeline(cfg, st, prov, nil, func(_ context.Context, in ingest.Input) (ingest.Loaded, error) {
		return ingest.Loaded{Text: in.Text}, nil
	})
	p.SetJudge(newFactJudge(cfg, st))
	tag := fmt.Sprintf("Acme-Max%d", time.Now().UnixNano())
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			_ = st.DeleteDocument(context.Background(), id)
		}
		keysMu.Lock()
		defer keysMu.Unlock()
		for _, key := range keys {
			migrationQuery(t, `MATCH (v:JudgeVerdict {key: $key}) DELETE v`, map[string]any{"key": key})
		}
	})
	check := func(id, status string) {
		t.Helper()
		doc, err := st.Document(ctx, id)
		if err != nil || doc.Status != graph.StatusDone {
			t.Fatalf("document = %+v err=%v", doc, err)
		}
		rows, err := st.RunReadQuery(ctx, `MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = $id RETURN r.judge AS judge`, map[string]any{"id": id}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if status == "refuted" {
			if len(rows.Rows) != 0 {
				t.Fatalf("refuted edge remains: %+v", rows.Rows)
			}
			audits, err := st.RunReadQuery(ctx, `MATCH (r:RejectedFact {doc_id: $id}) RETURN r.judge_verdict AS judge, r.reason AS reason`, map[string]any{"id": id}, 0)
			if err != nil || len(audits.Rows) != 1 || audits.Rows[0]["judge"] != status || audits.Rows[0]["reason"] != "synthetic evidence" {
				t.Fatalf("rejected audit = %+v err=%v", audits.Rows, err)
			}
		} else if len(rows.Rows) != 1 || rows.Rows[0]["judge"] != status {
			t.Fatalf("typed edges = %+v, want %s", rows.Rows, status)
		}
	}
	for i, status := range statuses {
		res, err := p.Submit(ctx, ingest.Input{Kind: "text", Source: fmt.Sprintf("%s-%d", tag, i), Text: fmt.Sprintf("%s uses Zorbo%d.", tag, i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.DocumentID)
		if err := p.Process(ctx, res.DocumentID); err != nil {
			t.Fatal(err)
		}
		check(res.DocumentID, status)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("initial provider calls=%d, want 3", got)
	}
	// A fresh wrapper and pipeline can only reuse the persistent graph cache.
	p = ingest.NewPipeline(cfg, st, prov, nil, nil)
	p.SetJudge(newFactJudge(cfg, st))
	for i, id := range ids {
		// Rebuild resets extraction and requeues the document.
		res, err := p.Rebuild(ctx, id, "")
		if err != nil || len(res.Queued) != 1 || res.Queued[0] != id {
			t.Fatalf("rebuild = %+v err=%v", res, err)
		}
		if err := p.Process(ctx, id); err != nil {
			t.Fatal(err)
		}
		check(id, statuses[i])
	}
	if got := calls.Load(); got != 3 || prov.Calls() != 6 {
		t.Fatalf("after re-extraction: provider=%d extractor=%d, want 3 and 6", got, prov.Calls())
	}
	t.Log("facts=3; initial judge calls=3; re-extraction judge calls=0; extractor calls=6")
}
