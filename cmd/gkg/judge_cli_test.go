package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func TestJudgeFlags(t *testing.T) {
	for _, args := range [][]string{{"judge"}, {"judge", "bogus"}, {"judge", "backfill", "--limit", "-1"}, {"judge", "backfill", "--budget-calls", "-1"}, {"judge", "backfill", "extra"}, {"judge", "backfill", "--bogus"}, {"judge", "audit"}, {"judge", "audit", "--sample", "0", "--out", "x"}, {"judge", "audit", "--sample", "1"}, {"judge", "audit", "--score", "x", "--sample", "1"}, {"judge", "audit", "--score", "x", "--out", "y"}, {"judge", "audit", "--score", "x", "--seed", "1"}, {"judge", "audit", "--bogus"}} {
		var out, err bytes.Buffer
		if code := run(args, &out, &err); code != 2 {
			t.Errorf("%v exit=%d stderr=%s", args, code, &err)
		}
	}
	for _, sub := range []string{"backfill", "audit"} {
		var out, err bytes.Buffer
		if code := run([]string{"judge", sub, "--help"}, &out, &err); code != 0 {
			t.Fatalf("help %s: exit=%d %s", sub, code, &err)
		}
	}
	var out bytes.Buffer
	usage(&out)
	if !strings.Contains(out.String(), "judge") {
		t.Fatal("usage missing judge")
	}
}

func TestJudgeAuditScoreCLI(t *testing.T) {
	// Scoring must work offline without graph or provider configuration.
	t.Setenv("GKG_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	path := filepath.Join(t.TempDir(), "labelled.jsonl")
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for i := 0; i < 200; i++ {
		label := "ok"
		if i >= 190 {
			label = "wrong"
		}
		if err := enc.Encode(judge.AuditRow{ID: "Acme-Max", Label: label}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := run([]string{"judge", "audit", "--score", path}, &out, &stderr); code != 0 || !strings.Contains(out.String(), "0.950") || !strings.Contains(out.String(), "200") || !strings.Contains(out.String(), "0.910") || !strings.Contains(out.String(), "0.973") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &stderr)
	}
	for _, text := range []string{"{", `{"label":"maybe"}`} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		stderr.Reset()
		if code := run([]string{"judge", "audit", "--score", path}, &out, &stderr); code != 2 {
			t.Fatalf("invalid JSONL exit=%d %s", code, &stderr)
		}
	}
}

func TestJudgeBackfillRequiresBudget(t *testing.T) {
	t.Setenv("GKG_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	for _, budget := range [][]string{nil, {"--budget-calls", "0"}, {"--budget-calls", "-1"}} {
		args := append([]string{"judge", "backfill"}, budget...)
		var out, stderr bytes.Buffer
		if code := run(args, &out, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage:") || !strings.Contains(stderr.String(), "budget-calls") {
			t.Errorf("%v exit=%d stdout=%s stderr=%s", args, code, &out, &stderr)
		}
	}
}

func TestJudgeAuditScoreNoScoredRows(t *testing.T) {
	t.Setenv("GKG_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	file := filepath.Join(t.TempDir(), "unscored.jsonl")
	for _, content := range []string{"", `{"label":"skip"}`, `{"label":""}`, "{\"label\":\"skip\"}\n{\"label\":\"\"}\n"} {
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		if code := run([]string{"judge", "audit", "--score", file}, &out, &stderr); code != 2 || !strings.Contains(out.String()+stderr.String(), "no scored rows") || strings.Contains(out.String(), "precision=") {
			t.Errorf("content=%q exit=%d stdout=%s stderr=%s", content, code, &out, &stderr)
		}
	}
}

func TestJudgeAuditSampleCLI(t *testing.T) {
	_ = graphtest.Store(t)
	t.Setenv("GKG_MEMGRAPH_URI", os.Getenv(graphtest.URIEnv))
	path := filepath.Join(t.TempDir(), "sample.jsonl")
	var out, stderr bytes.Buffer
	if code := run([]string{"judge", "audit", "--sample", "3", "--seed", "42", "--out", path}, &out, &stderr); code != 0 {
		t.Fatalf("sample exit=%d stdout=%s stderr=%s", code, &out, &stderr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestJudgeBackfillCLI(t *testing.T) {
	st := graphtest.Store(t)
	tag := fmt.Sprintf("Quillon-%d", time.Now().UnixNano())
	doc, err := st.UpsertDocument(t.Context(), graph.DocumentInput{Kind: "text", Source: tag, Hash: tag})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), doc.ID) })
	if err := st.PutChunks(t.Context(), doc.ID, []graph.Chunk{{Text: "Acme-Max uses Zorbo.", End: 20}}); err != nil {
		t.Fatal(err)
	}
	in := ingest.FactInput{Head: "Acme-Max", Rel: "uses", Tail: "Zorbo", Evidence: "Acme-Max uses Zorbo.", DocContext: tag, DocID: doc.ID, Confidence: 0.8}
	ents := []graph.EntityUpsert{{Key: tag + "head", Name: "Acme-Max"}, {Key: tag + "tail", Name: "Zorbo"}}
	fact := graph.FactUpsert{HeadKey: ents[0].Key, TailKey: ents[1].Key, Rel: "uses", Confidence: 0.8, JudgeInput: ingest.CanonicalJudgeInput(in)}
	if err := st.UpsertExtractionOpts(t.Context(), doc.ID, doc.ID+"-c0", "mock:1", ents, []graph.FactUpsert{fact}, graph.WriteOptions{FactsAsCandidates: true}); err != nil {
		t.Fatal(err)
	}
	migrationQuery(t, "MATCH (c:FactCandidate {doc_id: $doc}) SET c.domain = $domain", map[string]any{"doc": doc.ID, "domain": tag})
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"confirmed\"}"}}]}`))
	}))
	defer srv.Close()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", cfgPath)
	t.Setenv("GKG_MEMGRAPH_URI", os.Getenv(graphtest.URIEnv))
	t.Setenv("GKG_JUDGE_BASE_URL", srv.URL)
	var out, stderr bytes.Buffer
	if code := run([]string{"judge", "backfill", "--domain", tag, "--budget-calls", "1"}, &out, &stderr); code != 0 || calls != 1 || !strings.Contains(out.String(), "confirmed=1") || !strings.Contains(out.String(), "attempts=1") {
		t.Fatalf("exit=%d calls=%d stdout=%s stderr=%s", code, calls, &out, &stderr)
	}
	candidates, err := st.UnjudgedCandidates(t.Context(), tag, 0)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("remaining=%+v %v", candidates, err)
	}
}
