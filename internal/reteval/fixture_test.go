package reteval_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunk"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/reteval"
)

func TestPublicRetrievalFixture(t *testing.T) {
	root := filepath.Join("..", "..", "eval", "retrieval")
	set, err := reteval.Load(filepath.Join(root, "questions.json"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	wantMetricRows := 0
	for _, q := range set.Questions {
		counts[q.Stratum]++
		if q.Stratum != "null" {
			for _, kind := range q.Kinds {
				if kind == "chunk" {
					wantMetricRows++
				}
			}
		}
		if q.Stratum == "multi" {
			sources := map[string]bool{}
			for _, a := range q.Gold {
				sources[a.Source] = true
			}
			if len(sources) < 2 {
				t.Fatalf("%s must have anchors in different documents", q.ID)
			}
		}
	}
	if want := (map[string]int{"single": 5, "multi": 4, "global": 3, "null": 3}); !reflect.DeepEqual(counts, want) {
		t.Fatalf("stratum counts = %v, want %v", counts, want)
	}
	paths, err := filepath.Glob(filepath.Join(root, "synthetic", "*.md"))
	if err != nil || len(paths) != 4 {
		t.Fatalf("synthetic paths = %v: %v", paths, err)
	}
	paths = append(paths, filepath.Join(root, "rfc7231-trimmed.txt"))
	texts := map[string]string{}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		texts[filepath.ToSlash(rel)] = string(b)
		if strings.HasSuffix(path, ".md") {
			if n := len(chunk.Split(string(b), chunk.Options{TargetWords: 200, OverlapSentences: 1})); n < 3 {
				t.Fatalf("%s: %d chunks at 200 words, want >= 3", rel, n)
			}
		} else {
			if len(b) > 81920 {
				t.Fatalf("RFC fixture exceeds 80 KB: %d bytes", len(b))
			}
			for _, required := range []string{"This document is subject to BCP 78", "Copyright (c) 2014 IETF Trust", "4.  Request Methods", "6.  Response Status Codes"} {
				if !strings.Contains(string(b), required) {
					t.Fatalf("RFC fixture lacks %q", required)
				}
			}
		}
	}
	// Validate anchors against the actual 200-word chunks, before any DB writes.
	for _, q := range set.Questions {
		anchors := append([]reteval.Anchor(nil), q.Gold...)
		for _, item := range q.AnswerItems {
			anchors = append(anchors, item.Evidence...)
		}
		for _, a := range anchors {
			found := false
			for _, c := range chunk.Split(texts[a.Source], chunk.Options{TargetWords: 200, OverlapSentences: 1}) {
				found = found || strings.Contains(c.Text, a.Quote)
			}
			if !found {
				t.Fatalf("%s: anchor missing from fixture chunks: %+v", q.ID, a)
			}
		}
		for _, absent := range q.Absent {
			for source, text := range texts {
				if strings.Contains(reteval.Fold(text), reteval.Fold(absent)) {
					t.Fatalf("%s: absent term %q occurs in %s", q.ID, absent, source)
				}
			}
		}
	}

	st := graphtest.Store(t)
	ctx := t.Context()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ingest.SpoolDir = t.TempDir()
	cfg.Ingest.ChunkWords = 200
	cfg.Ingest.OverlapSentences = 1
	cfg.Ingest.ChunkDedup = "off"
	cfg.Judge.Mode = "off"
	mock := &extract.Mock{}
	p := ingest.NewPipeline(cfg, st, mock, nil, func(_ context.Context, in ingest.Input) (ingest.Loaded, error) {
		return ingest.Loaded{Text: in.Text}, nil
	})
	prefix := "public-retrieval-" + rand.Text() + "/"
	var ids []string
	// DeleteDocument removes chunks, mentions, facts and newly orphaned entities.
	// Register before ingestion so partially processed documents are cleaned too.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var chunkIDs []string
		for _, id := range ids {
			chunks, err := st.Chunks(cleanupCtx, id)
			if err != nil {
				t.Errorf("list fixture chunks before cleanup %s: %v", id, err)
			}
			for _, c := range chunks {
				chunkIDs = append(chunkIDs, c.ID)
			}
			if err := st.DeleteDocument(cleanupCtx, id); err != nil {
				t.Errorf("delete fixture document %s: %v", id, err)
			}
		}
		params := map[string]any{"ids": ids, "chunk_ids": chunkIDs}
		rows, err := st.RunReadQuery(cleanupCtx, `MATCH (n)
WHERE n.id IN $ids OR n.id IN $chunk_ids OR n.doc_id IN $ids OR n.chunk_id IN $chunk_ids
RETURN count(n) AS n`, params, 0)
		if err != nil || len(rows.Rows) != 1 || rows.Rows[0]["n"] != int64(0) {
			t.Errorf("fixture cleanup left documents or derived nodes: %+v, err=%v", rows.Rows, err)
		}
		rows, err = st.RunReadQuery(cleanupCtx, `MATCH (a)-[r]->(b)
WHERE r.doc_id IN $ids OR r.chunk_id IN $chunk_ids
   OR a.id IN $ids OR a.id IN $chunk_ids OR a.doc_id IN $ids OR a.chunk_id IN $chunk_ids
   OR b.id IN $ids OR b.id IN $chunk_ids OR b.doc_id IN $ids OR b.chunk_id IN $chunk_ids
RETURN count(r) AS n`, params, 0)
		if err != nil || len(rows.Rows) != 1 || rows.Rows[0]["n"] != int64(0) {
			t.Errorf("fixture cleanup left relationships: %+v, err=%v", rows.Rows, err)
		}
	})
	totalChunks := 0
	for _, path := range paths {
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		res, err := p.Submit(ctx, ingest.Input{Kind: "text", Source: prefix + rel, Text: texts[rel]})
		if err != nil {
			t.Fatalf("submit %s: %v", rel, err)
		}
		ids = append(ids, res.DocumentID)
		if res.Noop {
			t.Fatalf("fixture source unexpectedly reused: %s", rel)
		}
		if err := p.Process(ctx, res.DocumentID); err != nil {
			t.Fatalf("process %s: %v", rel, err)
		}
		chunks, err := st.Chunks(ctx, res.DocumentID)
		if err != nil || len(chunks) < 3 {
			t.Fatalf("%s chunks = %d: %v", rel, len(chunks), err)
		}
		totalChunks += len(chunks)
		t.Logf("%s: %d chunks", rel, len(chunks))
	}
	if mock.Calls() == 0 {
		t.Fatal("real pipeline did not invoke mock extractor")
	}
	// Fully qualify gold sources so other runs cannot satisfy these anchors.
	for i := range set.Questions {
		q := &set.Questions[i]
		for j := range q.Gold {
			q.Gold[j].Source = prefix + q.Gold[j].Source
		}
		for j := range q.AnswerItems {
			for k := range q.AnswerItems[j].Evidence {
				q.AnswerItems[j].Evidence[k].Source = prefix + q.AnswerItems[j].Evidence[k].Source
			}
		}
	}
	report, err := reteval.Run(ctx, reteval.Deps{Store: st}, set, reteval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Invalid) != 0 || len(report.Failed) != 0 || len(report.Questions) != len(set.Questions) {
		t.Fatalf("questions=%d invalid=%v failed=%v", len(report.Questions), report.Invalid, report.Failed)
	}
	var recall, mrr float64
	n := 0
	for _, q := range report.Questions {
		if q.Stratum == "null" {
			if len(q.Search) != 0 {
				t.Errorf("null question %s has search metrics", q.ID)
			}
			continue
		}
		for _, s := range q.Search {
			if s.Kind == "chunk" && s.Variant == "question" {
				recall += s.Recall["10"]
				mrr += s.MRR
				n++
				t.Logf("%s: R@10=%.4f MRR=%.4f ranks=%v", q.ID, s.Recall["10"], s.MRR, s.Ranks)
			}
		}
	}
	if n == 0 || n != wantMetricRows {
		t.Fatalf("chunk/question metrics = %d, want %d nonzero rows", n, wantMetricRows)
	}
	recall /= float64(n)
	mrr /= float64(n)
	t.Logf("%d documents, %d chunks; chunk/question R@10=%.6f MRR=%.6f", len(ids), totalChunks, recall, mrr)
	b, err := os.ReadFile(filepath.Join(root, "floor.json"))
	if err != nil {
		t.Fatal(err)
	}
	var floor struct {
		ChunkRecallAt10 float64 `json:"chunk_recall_at_10"`
		ChunkMRR        float64 `json:"chunk_mrr"`
	}
	if err := json.Unmarshal(b, &floor); err != nil {
		t.Fatal(err)
	}
	if floor.ChunkRecallAt10 <= 0 || floor.ChunkRecallAt10 > 1 || floor.ChunkMRR <= 0 || floor.ChunkMRR > 1 {
		t.Fatalf("invalid floor: %+v", floor)
	}
	if recall < floor.ChunkRecallAt10 || mrr < floor.ChunkMRR {
		t.Errorf("chunk/question R@10=%.6f MRR=%.6f below floor %+v", recall, mrr, floor)
	}
}
