package ingest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

// mode selection is exercised through the pure helpers.

func TestEvidenceSentenceExtraction(t *testing.T) {
	text := "Prima frase. La città perché è bella davvero. Dopo la frase."
	// Mention span inside the second sentence ("città" at byte offset 13).
	ev := ingest.EvidenceSentence(text, 13, 25)
	if !strings.Contains(ev, "La città perché è bella davvero.") {
		t.Errorf("evidence = %q, want the containing sentence", ev)
	}
	if strings.Contains(ev, "Prima frase") || strings.Contains(ev, "Dopo la frase") {
		t.Errorf("evidence = %q, want exactly the containing sentence", ev)
	}
}

func TestEvidenceSentenceClampsToText(t *testing.T) {
	text := "senza punteggiatura finale"
	if ev := ingest.EvidenceSentence(text, 0, 5); ev != text {
		t.Errorf("evidence = %q, want the whole text", ev)
	}
	if ev := ingest.EvidenceSentence("", 0, 0); ev != "" {
		t.Errorf("empty evidence = %q", ev)
	}
	// Out-of-range start must clamp, not panic.
	if ev := ingest.EvidenceSentence("abc", 5, 10); ev != "abc" {
		t.Errorf("clamped evidence = %q, want the whole text", ev)
	}
}

func TestEvidenceSentenceCapIsRuneSafe(t *testing.T) {
	long := strings.Repeat("€", 300) // 3 bytes each; a naive cut lands mid-rune
	ev := ingest.EvidenceSentence(long, 0, 10)
	if !utf8.ValidString(ev) {
		t.Errorf("capped evidence is invalid UTF-8: %q", ev)
	}
	if len(ev) > 500 {
		t.Errorf("capped evidence is %d bytes, want <= 500", len(ev))
	}
	mixed := "a" + strings.Repeat("à", 300)
	ev = ingest.EvidenceSentence(mixed, 0, 2)
	if !utf8.ValidString(ev) {
		t.Errorf("mixed-script cap is invalid UTF-8: %q", ev)
	}
	if len(ev) > 500 {
		t.Errorf("mixed-script cap is %d bytes, want <= 500", len(ev))
	}
}

func TestModeLowConfidenceSelectsFacts(t *testing.T) {
	// verify_floor 0.35: only facts strictly below are selected.
	if !ingest.ShouldJudge("low-confidence", 0.2, 0.35, 10, "k1") {
		t.Error("confidence 0.2 below the floor: want selected")
	}
	if ingest.ShouldJudge("low-confidence", 0.5, 0.35, 10, "k1") {
		t.Error("confidence 0.5 above the floor: want skipped")
	}
	if ingest.ShouldJudge("off", 0.2, 0.35, 10, "k1") {
		t.Error("mode off: nothing is selected")
	}
	if !ingest.ShouldJudge("all", 0.9, 0.35, 10, "k1") {
		t.Error("mode all: everything is selected")
	}
}

func TestModeSampleSelectionIsDeterministic(t *testing.T) {
	if ingest.ShouldJudge("sample", 0.9, 0.35, 0, "k1") {
		t.Error("sample pct 0: nothing selected")
	}
	if !ingest.ShouldJudge("sample", 0.9, 0.35, 100, "k1") {
		t.Error("sample pct 100: everything selected")
	}
	a := ingest.ShouldJudge("sample", 0.9, 0.35, 50, "fact-key")
	b := ingest.ShouldJudge("sample", 0.9, 0.35, 50, "fact-key")
	if a != b {
		t.Error("sample selection must be deterministic for the same fact")
	}
	selected := 0
	for i := 0; i < 100; i++ {
		if ingest.ShouldJudge("sample", 0.9, 0.35, 50, fmt.Sprintf("key-%d", i)) {
			selected++
		}
	}
	if selected < 20 || selected > 80 {
		t.Errorf("sample pct 50 selected %d/100, want a plausible subset", selected)
	}
}

// stubJudge records calls and returns configured verdicts per fact.
type stubJudge struct {
	verdicts map[string]ingest.Verdict
	err      error
	calls    []ingest.FactInput
}

func (s *stubJudge) VerifyFact(_ context.Context, in ingest.FactInput) (ingest.Verdict, error) {
	s.calls = append(s.calls, in)
	if s.err != nil {
		return ingest.Verdict{}, s.err
	}
	if v, ok := s.verdicts[in.Head+"|"+in.Rel+"|"+in.Tail]; ok {
		return v, nil
	}
	return ingest.Verdict{Status: "confirmed", Confidence: 0.9}, nil
}

func judgeEnv(t *testing.T, mode string, j ingest.FactJudge) (pipelineEnv, *stubJudge) {
	t.Helper()
	env := newPipelineEnv(t)
	env.cfg.Judge.Mode = mode
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
	stub, _ := j.(*stubJudge)
	env.p.SetJudge(j)
	return env, stub
}

func TestRefutedFactMovedToAudit(t *testing.T) {
	stub := &stubJudge{}
	env := newPipelineEnv(t)
	env.cfg.Judge.Mode = "all"
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
	env.p.SetJudge(stub)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	source, text := uniqueText(t)

	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	if len(stub.calls) == 0 {
		t.Fatal("judge was not called")
	}
	// The first fact (first-built extraction) is refuted by the stub. The
	// judge sees mention text; the store keys are the normalized names.
	first := stub.calls[0]
	headKey, tailKey := graph.EntityKey(first.Head), graph.EntityKey(first.Tail)
	edgeQuery := fmt.Sprintf(
		"MATCH (:Entity {key: '%s'})-[r]->(:Entity {key: '%s'}) WHERE r.chunk_id CONTAINS '%s' RETURN count(r) AS n",
		headKey, tailKey, res.DocumentID)
	// Positive control: the edge exists before the refutation, so the
	// final absence check cannot pass vacuously.
	if n := rawCount(t, uri, edgeQuery); n == 0 {
		t.Fatalf("edge %s -> %s missing before the refutation", headKey, tailKey)
	}
	stub.verdicts = map[string]ingest.Verdict{
		first.Head + "|" + first.Rel + "|" + first.Tail: {Status: "refuted", Confidence: 0.9, Reason: "contradicted by the source"},
	}
	// Re-run with the refuting verdict: force a fresh extraction.
	res2, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text, Force: true})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := env.p.Process(ctx, res2.DocumentID); err != nil {
		t.Fatalf("Process: %v", err)
	}
	_ = res

	audits := rawCount(t, uri, fmt.Sprintf("MATCH (rf:RejectedFact) WHERE rf.doc_id = '%s' RETURN count(rf) AS n", res2.DocumentID))
	if audits != 1 {
		t.Fatalf("audit rows = %d, want 1", audits)
	}
	row := rawRecords(t, uri, fmt.Sprintf(
		"MATCH (rf:RejectedFact) WHERE rf.doc_id = '%s' RETURN rf.judge_verdict AS v, rf.reason AS reason, rf.head_key AS head, rf.extractor_version AS ever", res2.DocumentID))[0]
	if got := testString(row, "v"); got != "refuted" {
		t.Errorf("judge_verdict = %q", got)
	}
	if got := testString(row, "reason"); got != "contradicted by the source" {
		t.Errorf("reason = %q", got)
	}
	if got := testString(row, "ever"); got != "mock:1" {
		t.Errorf("extractor_version = %q", got)
	}
	// The edge for the refuted triple must be gone (no judge-filtered
	// query: a lingering unjudged edge must fail this too).
	edges := rawCount(t, uri, fmt.Sprintf(
		"MATCH (:Entity {key: '%s'})-[r]->(:Entity {key: '%s'}) WHERE r.chunk_id CONTAINS '%s' RETURN count(r) AS n",
		headKey, tailKey, res2.DocumentID))
	if edges != 0 {
		t.Errorf("refuted edges still present: %d", edges)
	}
}

func TestJudgeDownStillDone(t *testing.T) {
	stub := &stubJudge{err: errors.New("judge exploded")}
	env := newPipelineEnv(t)
	env.cfg.Judge.Mode = "all"
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
	env.p.SetJudge(stub)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	source, text := uniqueText(t)

	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	doc, err := env.st.Document(ctx, res.DocumentID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusDone {
		t.Fatalf("status = %q (%s), want done despite judge errors", doc.Status, doc.Error)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' AND r.judge = '' RETURN count(r) AS n", res.DocumentID)); n == 0 {
		t.Error("facts should stay unverified (judge='') when the judge is down")
	}
}

func TestConfirmedFactMarksEdge(t *testing.T) {
	stub := &stubJudge{}
	env := newPipelineEnv(t)
	env.cfg.Judge.Mode = "all"
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
	env.p.SetJudge(stub)
	uri := os.Getenv(graphtest.URIEnv)
	source, text := uniqueText(t)

	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' AND r.judge = 'confirmed' RETURN count(r) AS n", res.DocumentID)); n == 0 {
		t.Error("confirmed verdict not recorded on the edge")
	}
}

func TestJudgeOffByDefault(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	if env.cfg.Judge.Mode != "off" {
		t.Fatalf("default judge mode = %q, want off", env.cfg.Judge.Mode)
	}
	source, text := uniqueText(t)
	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' AND r.judge = '' RETURN count(r) AS n", res.DocumentID)); n == 0 {
		t.Error("judge off: facts must stay unverified")
	}
	_ = ctx
}

// scopedValueProvider links a person to a document-scoped amount.
type scopedValueProvider struct{}

func (scopedValueProvider) Name() string    { return "scoped" }
func (scopedValueProvider) Version() string { return "scoped:1" }
func (scopedValueProvider) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	out := make([]extract.ChunkResult, 0, len(req.Chunks))
	for _, c := range req.Chunks {
		out = append(out, extract.ChunkResult{
			ChunkID: c.ID,
			Mentions: []extract.Mention{
				{Label: "person", Text: "Ada", Start: 0, End: 3, Score: 0.9},
				{Label: "amount", Text: "€500", Start: 9, End: 15, Score: 0.9},
			},
			Candidates: []extract.Candidate{{Label: "paid", Head: 0, Tail: 1, Score: 0.9}},
		})
	}
	return out, nil
}

// TestJudgeSeesMentionTextForScopedValues: the judge gets names as they
// appear in the evidence, never the opaque "#<doc>:<name>" scoped key.
func TestJudgeSeesMentionTextForScopedValues(t *testing.T) {
	stub := &stubJudge{}
	env := newPipelineEnv(t)
	env.cfg.Judge.Mode = "all"
	env.cfg.Labels.PresetsDir = t.TempDir()
	preset := "entities:\n  - name: person\n    threshold: 0.4\n  - name: amount\n    threshold: 0.4\n    scope: document\nrelations:\n  - name: paid\n    threshold: 0.25\n"
	if err := os.WriteFile(filepath.Join(env.cfg.Labels.PresetsDir, "scoped.yaml"), []byte(preset), 0o644); err != nil {
		t.Fatal(err)
	}
	env.p = ingest.NewPipeline(env.cfg, env.st, scopedValueProvider{}, env.ix, textLoader)
	env.p.SetJudge(stub)
	source, _ := uniqueText(t)
	text := fmt.Sprintf("Ada paid €500 for run %d.", time.Now().UnixNano())

	_ = mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text, Labels: "scoped"})
	if len(stub.calls) != 1 {
		t.Fatalf("judge calls = %d, want 1", len(stub.calls))
	}
	if c := stub.calls[0]; c.Head != "Ada" || c.Tail != "€500" {
		t.Errorf("judge saw head %q tail %q, want the mention texts Ada and €500", c.Head, c.Tail)
	}
}

// dupProvider returns the same triple twice in one chunk (different
// confidences) to pin candidate dedupe.
type dupProvider struct{}

func (dupProvider) Name() string    { return "dup" }
func (dupProvider) Version() string { return "dup:1" }
func (dupProvider) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	out := make([]extract.ChunkResult, 0, len(req.Chunks))
	for _, c := range req.Chunks {
		out = append(out, extract.ChunkResult{
			ChunkID: c.ID,
			Mentions: []extract.Mention{
				{Label: "person", Text: "Ada", Start: 0, End: 3, Score: 0.9},
				{Label: "person", Text: "Bea", Start: 4, End: 7, Score: 0.9},
			},
			Candidates: []extract.Candidate{
				{Label: "relates", Head: 0, Tail: 1, Score: 0.5},
				{Label: "relates", Head: 0, Tail: 1, Score: 0.9},
			},
		})
	}
	return out, nil
}

func TestJudgeDedupesPerChunkFacts(t *testing.T) {
	stub := &stubJudge{}
	env := newPipelineEnv(t)
	env.cfg.Judge.Mode = "all"
	env.p = ingest.NewPipeline(env.cfg, env.st, dupProvider{}, env.ix, textLoader)
	env.p.SetJudge(stub)
	source, text := uniqueText(t)

	_ = mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	if len(stub.calls) != 1 {
		t.Fatalf("judge calls = %d, want 1 (per-chunk dedupe, max confidence kept)", len(stub.calls))
	}
	if stub.calls[0].Confidence != 0.9 {
		t.Errorf("judged confidence = %v, want the max 0.9", stub.calls[0].Confidence)
	}
}

// collidingRelProvider emits two relation labels that sanitize to the same
// Neo4j type ("works-at" / "works_at") for the same triple.
type collidingRelProvider struct{}

func (collidingRelProvider) Name() string    { return "colliding" }
func (collidingRelProvider) Version() string { return "colliding:1" }
func (collidingRelProvider) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	out := make([]extract.ChunkResult, 0, len(req.Chunks))
	for _, c := range req.Chunks {
		out = append(out, extract.ChunkResult{
			ChunkID: c.ID,
			Mentions: []extract.Mention{
				{Label: "person", Text: "Ada", Start: 0, End: 3, Score: 0.9},
				{Label: "person", Text: "Bea", Start: 4, End: 7, Score: 0.9},
			},
			Candidates: []extract.Candidate{
				{Label: "works-at", Head: 0, Tail: 1, Score: 0.9},
				{Label: "works_at", Head: 0, Tail: 1, Score: 0.2},
			},
		})
	}
	return out, nil
}

// TestJudgeDedupesSanitizedRelationCollisions pins that the judge dedupe
// keys on the sanitized relation exactly like the store: colliding labels
// collapse to one judged fact (the max-confidence one that was written).
func TestJudgeDedupesSanitizedRelationCollisions(t *testing.T) {
	stub := &stubJudge{}
	env := newPipelineEnv(t)
	env.cfg.Judge.Mode = "all"
	env.p = ingest.NewPipeline(env.cfg, env.st, collidingRelProvider{}, env.ix, textLoader)
	env.p.SetJudge(stub)
	source, text := uniqueText(t)

	_ = mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	if len(stub.calls) != 1 {
		t.Fatalf("judge calls = %d, want 1 (sanitized relation identity)", len(stub.calls))
	}
	if stub.calls[0].Confidence != 0.9 {
		t.Errorf("judged confidence = %v, want the written max 0.9", stub.calls[0].Confidence)
	}
}

func TestSamplePctZeroNeverJudges(t *testing.T) {
	stub := &stubJudge{}
	env := newPipelineEnv(t)
	env.cfg.Judge.Mode = "sample"
	env.cfg.Judge.SamplePct = 0
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
	env.p.SetJudge(stub)
	source, text := uniqueText(t)
	_ = mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	if len(stub.calls) != 0 {
		t.Errorf("sample pct 0 called the judge %d times", len(stub.calls))
	}
	_ = time.Second
}

type candidateProvider struct{}

func (candidateProvider) Name() string    { return "candidate" }
func (candidateProvider) Version() string { return "candidate:1" }
func (candidateProvider) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	var out []extract.ChunkResult
	for _, c := range req.Chunks {
		out = append(out, extract.ChunkResult{ChunkID: c.ID, Mentions: []extract.Mention{
			{Label: "person", Text: "Zorbo", Start: 0, End: 5, Score: 0.9},
			{Label: "organization", Text: "Quillon Ltd", Start: 13, End: 24, Score: 0.9},
		}, Candidates: []extract.Candidate{
			{Label: "joined", Head: 0, Tail: 1, Score: 0.9},
			{Label: "uses", Head: 0, Tail: 1, Score: 0.3},
			{Label: "owns", Head: 0, Tail: 1, Score: 0.8},
		}})
	}
	return out, nil
}

type checkingCandidateJudge struct {
	t     *testing.T
	calls []ingest.FactInput
	fail  bool
}

func (j *checkingCandidateJudge) VerifyFact(_ context.Context, in ingest.FactInput) (ingest.Verdict, error) {
	j.t.Helper()
	j.calls = append(j.calls, in)
	if in.DocID == "" {
		j.t.Fatal("judge input lost DocID")
	}
	rows := rawRecords(j.t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (n:FactCandidate {doc_id: '%s', rel: '%s'}) RETURN n.judge_input AS payload, n.input_hash AS hash, n.judge AS judge", in.DocID, in.Rel))
	if len(rows) != 1 {
		j.t.Fatalf("candidates before judge = %d, want 1", len(rows))
	}
	payload := ingest.CanonicalJudgeInput(in)
	if got := testString(rows[0], "payload"); got != string(payload) {
		j.t.Fatalf("stored payload = %s, judge payload = %s", got, payload)
	}
	if got := testString(rows[0], "hash"); got != fmt.Sprintf("%x", sha256.Sum256(payload)) {
		j.t.Fatalf("input_hash = %s", got)
	}
	if got := testString(rows[0], "judge"); got != "" {
		j.t.Fatalf("candidate already judged: %q", got)
	}
	decoded, err := ingest.DecodeJudgeInput(payload)
	if err != nil {
		j.t.Fatal(err)
	}
	decoded.DocID = in.DocID
	if !reflect.DeepEqual(decoded, in) {
		j.t.Fatalf("payload round-trip = %+v, want %+v", decoded, in)
	}
	if j.fail {
		return ingest.Verdict{}, errors.New("judge unavailable")
	}
	status := map[string]string{"joined": "confirmed", "uses": "unclear", "owns": "refuted"}[in.Rel]
	return ingest.Verdict{Status: status, Reason: "synthetic verdict"}, nil
}

func TestPipelineFactCandidates(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("judge_error=%v", fail), func(t *testing.T) {
			env := newPipelineEnv(t)
			env.cfg.Judge.Mode = "all"
			env.cfg.Ingest.ChunkWords = 200
			env.cfg.Ingest.ChunkDedup = "off"
			env.p = ingest.NewPipeline(env.cfg, env.st, candidateProvider{}, env.ix, textLoader)
			env.p.SetWriteMode(ingest.WriteMode{RequireConfirmation: true})
			judge := &checkingCandidateJudge{t: t, fail: fail}
			env.p.SetJudge(judge)
			metrics := obs.New()
			env.p.SetMetrics(metrics)
			source, _ := uniqueText(t)
			res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: "Zorbo joined Quillon Ltd.", Title: "Synthetic document"})
			t.Cleanup(func() { _ = env.st.DeleteDocument(context.Background(), res.DocumentID) })
			if len(judge.calls) != 3 {
				t.Fatalf("judge calls = %d, want 3", len(judge.calls))
			}
			uri := os.Getenv(graphtest.URIEnv)
			counts := []int64{
				rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", res.DocumentID)),
				rawCount(t, uri, fmt.Sprintf("MATCH (n:FactCandidate {doc_id:'%s'}) RETURN count(n) AS n", res.DocumentID)),
				rawCount(t, uri, fmt.Sprintf("MATCH (n:RejectedFact {doc_id:'%s'}) RETURN count(n) AS n", res.DocumentID)),
			}
			want := []int64{1, 1, 1}
			if fail {
				want = []int64{0, 3, 0}
			}
			if !reflect.DeepEqual(counts, want) {
				t.Fatalf("edges/candidates/audits = %v, want %v", counts, want)
			}
			if scrape := gather(t, metrics); !strings.Contains(scrape, fmt.Sprintf("\nfacts_total %d\n", want[0])) {
				t.Errorf("facts_total must count promoted edges (%d):\n%s", want[0], scrape)
			}
			if !fail {
				if n := rawCount(t, uri, fmt.Sprintf("MATCH (n:FactCandidate {doc_id:'%s',judge:'unclear'}) RETURN count(n) AS n", res.DocumentID)); n != 1 {
					t.Errorf("unclear candidates = %d", n)
				}
				if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' AND r.judge = 'confirmed' RETURN count(r) AS n", res.DocumentID)); n != 1 {
					t.Errorf("confirmed edges = %d", n)
				}
			} else if n := rawCount(t, uri, fmt.Sprintf("MATCH (n:FactCandidate {doc_id:'%s',judge:''}) RETURN count(n) AS n", res.DocumentID)); n != 3 {
				t.Errorf("unjudged candidates = %d, want 3", n)
			}
			doc, err := env.st.Document(context.Background(), res.DocumentID)
			if err != nil || doc.Status != graph.StatusDone {
				t.Fatalf("document = %+v, %v", doc, err)
			}
		})
	}
}

func TestPipelineFactCandidatesCachedReExtraction(t *testing.T) {
	env := newPipelineEnv(t)
	env.cfg.Judge.Mode = "all"
	env.cfg.Ingest.ChunkWords = 200
	env.cfg.Ingest.ChunkDedup = "off"
	inner := &checkingCandidateJudge{t: t}
	newPipeline := func() *obs.Metrics {
		env.p = ingest.NewPipeline(env.cfg, env.st, candidateProvider{}, env.ix, textLoader)
		env.p.SetWriteMode(ingest.WriteMode{RequireConfirmation: true})
		env.p.SetJudge(judge.NewCached(inner, env.st, "synthetic-model"))
		metrics := obs.New()
		env.p.SetMetrics(metrics)
		return metrics
	}
	metrics := newPipeline()
	source, _ := uniqueText(t)
	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: "Zorbo joined Quillon Ltd.", Title: "Acme-Max " + source})
	ctx := context.Background()
	t.Cleanup(func() { _ = env.st.DeleteDocument(ctx, res.DocumentID) })
	uri := os.Getenv(graphtest.URIEnv)
	edgeQuery := fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN type(r), properties(r)", res.DocumentID)
	wantEdges := rawRecords(t, uri, edgeQuery)
	check := func(m *obs.Metrics, cacheResult string) {
		t.Helper()
		if rows := rawRecords(t, uri, edgeQuery); len(rows) != 1 || !reflect.DeepEqual(rows, wantEdges) {
			t.Errorf("promoted edges = %+v, want one identical confirmed edge", rows)
		}
		for label, want := range map[string]int64{"FactCandidate": 1, "RejectedFact": 1} {
			if n := rawCount(t, uri, fmt.Sprintf("MATCH (n:%s {doc_id: '%s'}) RETURN count(n) AS n", label, res.DocumentID)); n != want {
				t.Errorf("%s count = %d, want %d", label, n, want)
			}
		}
		if n := rawCount(t, uri, fmt.Sprintf("MATCH (n:FactCandidate {doc_id: '%s', judge: 'unclear'}) RETURN count(n) AS n", res.DocumentID)); n != 1 {
			t.Errorf("unclear candidates = %d, want 1", n)
		}
		if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' AND r.judge = 'confirmed' RETURN count(r) AS n", res.DocumentID)); n != 1 {
			t.Errorf("confirmed edges = %d, want 1", n)
		}
		scrape := gather(t, m)
		for _, want := range []string{"\nfacts_total 1\n", fmt.Sprintf("judge_cache_total{result=%q} 3", cacheResult)} {
			if !strings.Contains(scrape, want) {
				t.Errorf("metrics missing %q:\n%s", want, scrape)
			}
		}
		for _, query := range []string{
			"MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = $doc RETURN r.judge_model AS model",
			"MATCH (c:FactCandidate {doc_id:$doc}) RETURN c.judge_model AS model",
			"MATCH (a:RejectedFact {doc_id:$doc}) RETURN a.judge_model AS model",
		} {
			rows, err := env.st.RunReadQuery(ctx, query, map[string]any{"doc": res.DocumentID}, 0)
			if err != nil || len(rows.Rows) != 1 || rows.Rows[0]["model"] != "synthetic-model" {
				t.Errorf("candidate provenance on %s for %s = %+v, %v", query, cacheResult, rows, err)
			}
		}
	}
	if len(inner.calls) != 3 {
		t.Fatalf("initial judge provider calls = %d, want 3", len(inner.calls))
	}
	check(metrics, "miss")
	inner.calls = nil
	inner.fail = true // A cache miss on the second pass must leave a visible failure.
	metrics = newPipeline()
	requeued, err := env.p.Rebuild(ctx, res.DocumentID, "") // ResetExtraction + requeue.
	if err != nil || !reflect.DeepEqual(requeued.Queued, []string{res.DocumentID}) {
		t.Fatalf("requeue = %+v, %v", requeued, err)
	}
	if edges := rawRecords(t, uri, edgeQuery); len(edges) != 0 {
		t.Fatalf("ResetExtraction left %d edges", len(edges))
	}
	if err := env.p.Process(ctx, res.DocumentID); err != nil {
		t.Fatal(err)
	}
	if len(inner.calls) != 0 {
		t.Errorf("re-extraction judge provider calls = %d, want 0", len(inner.calls))
	}
	check(metrics, "hit")
	t.Log("initial provider calls=3; re-extraction provider calls=0; each pass: 1 confirmed edge, 1 unclear candidate, 1 audit, facts_total=1")
}

type candidateChangingJudge struct {
	change func(ingest.FactInput)
	status string
}

func (j candidateChangingJudge) VerifyFact(_ context.Context, in ingest.FactInput) (ingest.Verdict, error) {
	j.change(in)
	return ingest.Verdict{Status: j.status}, nil
}

func TestPipelineLogsDiscardedCandidateVerdicts(t *testing.T) {
	for _, state := range []string{"missing", "changed"} {
		for _, status := range []string{"confirmed", "unclear", "refuted"} {
			t.Run(state+"/"+status, func(t *testing.T) {
				env := newPipelineEnv(t)
				env.cfg.Judge.Mode = "all"
				env.cfg.Ingest.ChunkWords = 200
				env.cfg.Ingest.ChunkDedup = "off"
				env.p = ingest.NewPipeline(env.cfg, env.st, candidateProvider{}, env.ix, textLoader)
				env.p.SetWriteMode(ingest.WriteMode{RequireConfirmation: true})
				var logs bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(previous) })
				var ids []string
				env.p.SetJudge(candidateChangingJudge{status: status, change: func(in ingest.FactInput) {
					rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (c:FactCandidate {doc_id: '%s', rel: '%s'}) RETURN c.id AS id", in.DocID, in.Rel))
					if len(rows) != 1 {
						t.Fatalf("candidates before verdict = %d, want 1", len(rows))
					}
					id := testString(rows[0], "id")
					ids = append(ids, id)
					query := fmt.Sprintf("MATCH (c:FactCandidate {id: '%s'}) DELETE c", id)
					if state == "changed" {
						query = fmt.Sprintf("MATCH (c:FactCandidate {id: '%s'}) SET c.input_hash = 'changed'", id)
					}
					rawRecords(t, os.Getenv(graphtest.URIEnv), query)
				}})
				metrics := obs.New()
				env.p.SetMetrics(metrics)
				source, _ := uniqueText(t)
				res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: "Zorbo joined Quillon Ltd."})
				t.Cleanup(func() { _ = env.st.DeleteDocument(context.Background(), res.DocumentID) })
				seen := make(map[string]bool)
				for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					if line == "" {
						continue
					}
					var record map[string]any
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatal(err)
					}
					if record["msg"] == "ingest: discarded candidate verdict" && record["level"] == "INFO" && record["status"] == status {
						id, _ := record["candidate_id"].(string)
						seen[id] = true
					}
				}
				for _, id := range ids {
					if !seen[id] {
						t.Errorf("missing INFO discard log with candidate_id=%s status=%s:\n%s", id, status, logs.String())
					}
				}
				if scrape := gather(t, metrics); !strings.Contains(scrape, "\nfacts_total 0\n") {
					t.Errorf("discarded verdict counted a typed fact edge:\n%s", scrape)
				}
			})
		}
	}
}
