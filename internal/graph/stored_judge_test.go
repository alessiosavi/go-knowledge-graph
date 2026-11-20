package graph_test

import (
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestApplyStoredFactVerdict(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	f := newCandidateFixture(t, st)
	uri := os.Getenv(graphtest.URIEnv)
	const gen = "00000000000000000000000000000001"
	const next = "00000000000000000000000000000002"
	f.fact.JudgeGen = gen
	ref := graph.StoredFactRef{
		HeadKey: f.fact.HeadKey, Rel: f.fact.Rel, TailKey: f.fact.TailKey, ChunkID: f.chunk,
		JudgeGen: gen, InputHash: f.hash,
	}
	match := fmt.Sprintf("MATCH (a:Entity)-[r:joined]->(b:Entity) WHERE r.chunk_id = '%s'", f.chunk)
	props := func() []map[string]any {
		rows := rawRecords(t, uri, match+" RETURN properties(r) AS p")
		var out []map[string]any
		for _, row := range rows {
			p, _ := row.Get("p")
			out = append(out, p.(map[string]any))
		}
		return out
	}
	write := func(payload, enabled bool) {
		t.Helper()
		fact := f.fact
		if !payload {
			fact.JudgeInput = nil
		}
		if err := st.UpsertExtractionOpts(ctx, f.doc, f.chunk, "mock:1", f.ents, []graph.FactUpsert{fact}, graph.WriteOptions{StoreJudgeInput: enabled}); err != nil {
			t.Fatal(err)
		}
	}
	apply := func(status string, done, want bool) {
		t.Helper()
		if applied, err := st.ApplyStoredFactVerdict(ctx, ref, status, "provider reason", graph.StoredFactVerdictOptions{RequireDone: done}); err != nil || applied != want {
			t.Fatalf("ApplyStoredFactVerdict(%s, done=%v) = %v, %v; want %v, nil", status, done, applied, err, want)
		}
	}
	noAudit := func() {
		t.Helper()
		if n := f.count(t, "RejectedFact"); n != 0 {
			t.Fatalf("audits = %d, want 0", n)
		}
	}

	write(true, false)
	legacy := props()
	if len(legacy) != 1 || len(legacy[0]) != 12 || legacy[0]["judge"] != "" {
		t.Fatalf("legacy edge properties = %v", legacy)
	}
	write(false, true)
	legacy[0]["judge_gen"] = gen
	if after := props(); !reflect.DeepEqual(legacy, after) {
		t.Fatalf("nil payload should add only the generation: %v", after)
	}
	write(true, true)
	before := props()
	if len(before) != 1 || before[0]["judge_input"] != string(f.fact.JudgeInput) || before[0]["judge_gen"] != gen || before[0]["input_hash"] != f.hash {
		t.Fatalf("stored edge properties = %v", before)
	}
	if err := st.SetFactJudge(ctx, ref.HeadKey, ref.Rel, ref.TailKey, ref.ChunkID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	audit := graph.FactAudit{HeadKey: ref.HeadKey, Rel: ref.Rel, TailKey: ref.TailKey, ChunkID: ref.ChunkID, DocID: f.doc}
	if err := st.RejectFact(ctx, ref.HeadKey, ref.Rel, ref.TailKey, ref.ChunkID, audit); err != nil {
		t.Fatal(err)
	}
	if after := props(); !reflect.DeepEqual(before, after) {
		t.Fatalf("legacy setters mutated an opted-in edge: %v", after)
	}
	noAudit()

	// Rewrite in place. All guard fields except the generation still match.
	rawRun(t, uri, match+fmt.Sprintf(" SET r.judge_gen = '%s'", next))
	apply("refuted", false, false)
	noAudit()
	if rows := props(); len(rows) != 1 || rows[0]["judge_gen"] != next || rows[0]["judge"] != "" {
		t.Fatalf("stale verdict changed the new edge: %v", rows)
	}
	ref.JudgeGen = next
	ref.InputHash = "wrong-hash"
	apply("refuted", false, false)
	noAudit()
	ref.InputHash = f.hash

	rawRun(t, uri, match+" CREATE (a)-[dup:joined]->(b) SET dup = properties(r)")
	for _, status := range []string{"confirmed", "refuted"} {
		if applied, err := st.ApplyStoredFactVerdict(ctx, ref, status, "", graph.StoredFactVerdictOptions{}); err == nil || applied {
			t.Fatalf("duplicate %s = %v, %v; want error and no apply", status, applied, err)
		}
	}
	if rows := props(); len(rows) != 2 || rows[0]["judge"] != "" || rows[1]["judge"] != "" {
		t.Fatalf("duplicate apply mutated the edges: %v", rows)
	}
	noAudit()

	write(true, true)
	ref.JudgeGen = gen
	if err := st.SetStatus(ctx, f.doc, graph.StatusReceived, ""); err != nil {
		t.Fatal(err)
	}
	apply("confirmed", true, false)
	if err := st.SetStatus(ctx, f.doc, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	apply("confirmed", true, true)
	want := before[0]
	delete(want, "judge_input")
	for _, status := range []string{"confirmed", "unclear"} {
		if status == "unclear" {
			write(true, true)
			apply(status, false, true)
		}
		want["judge"] = status
		if rows := props(); len(rows) != 1 || !reflect.DeepEqual(want, rows[0]) {
			t.Fatalf("%s edge properties = %v, want %v", status, rows, want)
		}
		apply("refuted", false, false)
		noAudit()
	}

	write(true, true)
	rawRun(t, uri, match+" SET r.confidence = 0.72, r.extractor_version = 'row:2'")
	apply("refuted", false, true)
	apply("refuted", false, false)
	if rows := props(); len(rows) != 0 {
		t.Fatalf("refuted edges = %v", rows)
	}
	rows := rawRecords(t, uri, fmt.Sprintf("MATCH (n:RejectedFact {doc_id:'%s'}) RETURN properties(n) AS p", f.doc))
	if len(rows) != 1 {
		t.Fatalf("refuted audit count = %d", len(rows))
	}
	p, _ := rows[0].Get("p")
	auditProps := p.(map[string]any)
	for key, value := range map[string]any{
		"id":       graph.FactAuditID(ref.HeadKey, ref.Rel, ref.TailKey, ref.ChunkID),
		"head_key": ref.HeadKey, "tail_key": ref.TailKey, "doc_id": f.doc, "chunk_id": f.chunk,
		"score": 0.72, "extractor_version": "row:2", "judge_verdict": "refuted", "reason": "provider reason",
	} {
		if auditProps[key] != value {
			t.Errorf("audit %s = %v, want %v", key, auditProps[key], value)
		}
	}
}
