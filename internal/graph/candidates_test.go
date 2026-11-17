package graph_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

type candidateFixture struct {
	st                   *graph.Store
	doc, chunk, id, hash string
	ents                 []graph.EntityUpsert
	fact                 graph.FactUpsert
}

func newCandidateFixture(t *testing.T, st *graph.Store) candidateFixture {
	t.Helper()
	doc, chunk := entitiesTestDoc(t, st, "Zorbo joined Quillon Ltd.")
	t.Cleanup(func() {
		if err := st.DeleteDocument(context.Background(), doc); err != nil && !errors.Is(err, graph.ErrNotFound) {
			t.Error(err)
		}
	})
	ents := []graph.EntityUpsert{
		{Key: "zorbo-" + doc, Name: "Zorbo", Type: "person"},
		{Key: "quillon-" + doc, Name: "Quillon Ltd", Type: "organization"},
	}
	fact := graph.FactUpsert{HeadKey: ents[0].Key, Rel: "joined", TailKey: ents[1].Key, Confidence: 0.35, LowConfidence: true,
		HeadMention: graph.Span{Start: 0, End: 5}, TailMention: graph.Span{Start: 13, End: 24}, JudgeInput: []byte(`{"evidence":"Zorbo joined Quillon Ltd."}`)}
	f := candidateFixture{st: st, doc: doc, chunk: chunk, ents: ents, fact: fact,
		id: graph.CandidateID(fact.HeadKey, fact.Rel, fact.TailKey, chunk), hash: fmt.Sprintf("%x", sha256.Sum256(fact.JudgeInput))}
	f.write(t, true)
	return f
}

func (f candidateFixture) write(t *testing.T, candidate bool) {
	t.Helper()
	if err := f.st.UpsertExtractionOpts(context.Background(), f.doc, f.chunk, "mock:1", f.ents, []graph.FactUpsert{f.fact}, graph.WriteOptions{FactsAsCandidates: candidate}); err != nil {
		t.Fatal(err)
	}
}

func (f candidateFixture) count(t *testing.T, label string) int64 {
	t.Helper()
	return rawCount(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (n:%s {doc_id: '%s'}) RETURN count(n) AS n", label, f.doc))
}

func TestCandidateNotVisible(t *testing.T) {
	f := newCandidateFixture(t, graphtest.Store(t))
	ctx := context.Background()
	if f.id != graph.FactAuditID(f.fact.HeadKey, f.fact.Rel, f.fact.TailKey, f.chunk) {
		t.Fatal("candidate identity differs from audit identity")
	}
	if n := f.count(t, "FactCandidate"); n != 1 {
		t.Fatalf("candidates = %d, want 1", n)
	}
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (n:FactCandidate {id: '%s'}) RETURN properties(n) AS p", f.id))
	p, _ := rows[0].Get("p")
	props := p.(map[string]any)
	for key, want := range map[string]any{"judge": "", "judge_input": string(f.fact.JudgeInput), "input_hash": f.hash, "head_key": f.fact.HeadKey, "tail_key": f.fact.TailKey, "rel": "joined", "doc_id": f.doc, "chunk_id": f.chunk, "domain": "", "extractor_version": "mock:1"} {
		if props[key] != want {
			t.Errorf("%s = %v, want %v", key, props[key], want)
		}
	}
	if props["created_at"] == nil {
		t.Error("missing created_at")
	}
	if n := rawCount(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = '%s' RETURN count(r) AS n", f.chunk)); n != 0 {
		t.Fatalf("candidate leaked %d edges", n)
	}
	for _, include := range []bool{false, true} {
		view, err := f.st.EntityView(ctx, f.fact.HeadKey, include)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Facts) != 0 || len(view.Together) != 1 || view.Together[0].Key != f.fact.TailKey || view.Together[0].SharedChunks != 1 {
			t.Fatalf("candidate view = %+v", view)
		}
		sub, err := f.st.Traverse(ctx, f.fact.HeadKey, "", "both", 2, 50, include)
		if err != nil || len(sub.Edges) != 0 {
			t.Fatalf("traverse = %+v, %v", sub, err)
		}
	}
}

func TestCandidateChunkClearUsesIndex(t *testing.T) {
	st := graphtest.Store(t)
	control := planOperators(t, st, "MATCH (f:FactCandidate {unindexed_chunk_id: 'absent'}) DELETE f")
	if !slices.Contains(control, "ScanAll") {
		t.Fatalf("unindexed negative control = %v, want ScanAll", control)
	}
	operators := planOperators(t, st, "MATCH (f:FactCandidate {chunk_id: 'absent'}) DELETE f")
	if !slices.Contains(operators, "ScanAllByLabelProperties") || slices.Contains(operators, "ScanAll") {
		t.Fatalf("chunk clear operators = %v, want ScanAllByLabelProperties and no ScanAll", operators)
	}
	t.Logf("chunk clear operators = %v; negative control = %v", operators, control)
}

func TestPromoteCandidateMissingEndpoint(t *testing.T) {
	f := newCandidateFixture(t, graphtest.Store(t))
	uri := os.Getenv(graphtest.URIEnv)
	q := fmt.Sprintf("MATCH (c:FactCandidate {id: '%s'}) RETURN properties(c)", f.id)
	before := rawRecords(t, uri, q)
	rawRun(t, uri, fmt.Sprintf("MATCH (e:Entity {key: '%s'}) DETACH DELETE e", f.fact.HeadKey))
	if ok, err := f.st.PromoteCandidate(context.Background(), f.id, f.hash); err != nil || ok {
		t.Errorf("promote without head = %v, %v, want false, nil", ok, err)
	}
	if after := rawRecords(t, uri, q); !reflect.DeepEqual(before, after) {
		t.Error("promotion without head consumed or changed the candidate")
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = '%s' RETURN count(r) AS n", f.chunk)); n != 0 {
		t.Errorf("promotion without head created %d edges", n)
	}
}

func TestCandidateCleanupPrunedChunks(t *testing.T) {
	st := graphtest.Store(t)
	f := newCandidateFixture(t, st)
	ctx := context.Background()
	chunks := make([]graph.Chunk, 4)
	for i := range chunks {
		chunks[i] = graph.Chunk{Seq: i, Text: "Zorbo joined Quillon Ltd.", End: 24}
	}
	if err := st.PutChunks(ctx, f.doc, chunks); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 4; i++ {
		f.chunk = fmt.Sprintf("%s-c%d", f.doc, i)
		f.write(t, true)
	}
	if n := f.count(t, "FactCandidate"); n != 4 {
		t.Fatalf("candidates before prune = %d, want 4", n)
	}
	if err := st.DeleteChunksFrom(ctx, f.doc, 2); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "FactCandidate"); n != 2 {
		t.Errorf("candidates after prune = %d, want 2", n)
	}
	for i := 0; i < 4; i++ {
		n := rawCount(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (c:FactCandidate {chunk_id: '%s-c%d'}) RETURN count(c) AS n", f.doc, i))
		want := int64(0)
		if i < 2 {
			want = 1
		}
		if n != want {
			t.Errorf("chunk %d candidates = %d, want %d", i, n, want)
		}
	}
	if kept, err := st.Chunks(ctx, f.doc); err != nil || len(kept) != 2 {
		t.Fatalf("chunks after prune = %+v, %v, want 2", kept, err)
	}
}

func TestCandidateRequiresJudgeInput(t *testing.T) {
	for _, input := range [][]byte{nil, {}} {
		t.Run(fmt.Sprintf("nil=%v", input == nil), func(t *testing.T) {
			f := newCandidateFixture(t, graphtest.Store(t))
			uri := os.Getenv(graphtest.URIEnv)
			q := fmt.Sprintf("MATCH (c:FactCandidate {id: '%s'}) RETURN properties(c)", f.id)
			before := rawRecords(t, uri, q)
			invalid := f.fact
			invalid.JudgeInput = input
			invalid.Confidence = 0.1 // Even a discarded duplicate must be validated.
			newKey := "unwritten-" + f.doc
			ents := append(f.ents, graph.EntityUpsert{Key: newKey, Name: "Acme-Max", Type: "organization"})
			err := f.st.UpsertExtractionOpts(context.Background(), f.doc, f.chunk, "mock:2", ents, []graph.FactUpsert{f.fact, invalid}, graph.WriteOptions{FactsAsCandidates: true})
			if !errors.Is(err, graph.ErrMissingJudgeInput) {
				t.Errorf("candidate write with empty judge input = %v, want ErrMissingJudgeInput", err)
			}
			if after := rawRecords(t, uri, q); !reflect.DeepEqual(before, after) {
				t.Error("invalid candidate write changed existing extraction")
			}
			if n := rawCount(t, uri, fmt.Sprintf("MATCH (e:Entity {key: '%s'}) RETURN count(e) AS n", newKey)); n != 0 {
				t.Errorf("invalid candidate write created %d entities", n)
			}
		})
	}
}

func TestPromoteCandidate(t *testing.T) {
	f := newCandidateFixture(t, graphtest.Store(t))
	ctx := context.Background()
	f.write(t, false)
	if n := f.count(t, "FactCandidate"); n != 0 {
		t.Fatalf("direct rewrite left %d candidates", n)
	}
	if err := f.st.SetFactJudge(ctx, f.fact.HeadKey, f.fact.Rel, f.fact.TailKey, f.chunk, "confirmed"); err != nil {
		t.Fatal(err)
	}
	q := fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = '%s' RETURN type(r), properties(r)", f.chunk)
	want := rawRecords(t, os.Getenv(graphtest.URIEnv), q)
	f.write(t, true)
	ok, err := f.st.PromoteCandidate(ctx, f.id, f.hash)
	if err != nil || !ok {
		t.Fatalf("promote = %v, %v", ok, err)
	}
	if got := rawRecords(t, os.Getenv(graphtest.URIEnv), q); !reflect.DeepEqual(got, want) {
		t.Fatalf("promoted edge differs: got %+v, want %+v", got, want)
	}
	if n := f.count(t, "FactCandidate"); n != 0 {
		t.Errorf("remaining candidates = %d", n)
	}
	if ok, err := f.st.PromoteCandidate(ctx, f.id, f.hash); err != nil || ok {
		t.Fatalf("repeat promote = %v, %v", ok, err)
	}
}

func TestApplyStaleVerdict(t *testing.T) {
	f := newCandidateFixture(t, graphtest.Store(t))
	ctx := context.Background()
	audit := graph.FactAudit{HeadKey: f.fact.HeadKey, Rel: f.fact.Rel, TailKey: f.fact.TailKey, DocID: f.doc, ChunkID: f.chunk, JudgeVerdict: "refuted"}
	apply := func() {
		t.Helper()
		for name, fn := range map[string]func() (bool, error){
			"promote": func() (bool, error) { return f.st.PromoteCandidate(ctx, f.id, f.hash) },
			"unclear": func() (bool, error) { return f.st.SetCandidateJudge(ctx, f.id, f.hash, "unclear") },
			"refute":  func() (bool, error) { return f.st.RefuteCandidate(ctx, f.id, f.hash, audit) },
		} {
			if ok, err := fn(); err != nil || ok {
				t.Errorf("stale %s = %v, %v", name, ok, err)
			}
		}
		if n := f.count(t, "RejectedFact"); n != 0 {
			t.Errorf("stale verdict wrote %d audits", n)
		}
		if n := rawCount(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = '%s' RETURN count(r) AS n", f.chunk)); n != 0 {
			t.Errorf("stale verdict wrote %d edges", n)
		}
	}
	rawRun(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (n:FactCandidate {id: '%s'}) DELETE n", f.id))
	apply()
	f.fact.JudgeInput = []byte(`{"evidence":"Changed evidence."}`)
	f.write(t, true)
	q := fmt.Sprintf("MATCH (n:FactCandidate {id: '%s'}) RETURN properties(n)", f.id)
	before := rawRecords(t, os.Getenv(graphtest.URIEnv), q)
	apply()
	if after := rawRecords(t, os.Getenv(graphtest.URIEnv), q); !reflect.DeepEqual(before, after) {
		t.Fatal("stale verdict modified recreated candidate")
	}
}

func TestUnclearCandidate(t *testing.T) {
	f := newCandidateFixture(t, graphtest.Store(t))
	ok, err := f.st.SetCandidateJudge(context.Background(), f.id, f.hash, "unclear")
	if err != nil || !ok {
		t.Fatalf("unclear = %v, %v", ok, err)
	}
	n := rawCount(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (n:FactCandidate {id: '%s', judge: 'unclear'}) RETURN count(n) AS n", f.id))
	if n != 1 {
		t.Fatalf("unclear candidates = %d", n)
	}
}

func TestRefuteCandidate(t *testing.T) {
	f := newCandidateFixture(t, graphtest.Store(t))
	audit := graph.FactAudit{HeadKey: f.fact.HeadKey, Rel: f.fact.Rel, TailKey: f.fact.TailKey, DocID: f.doc, ChunkID: f.chunk, Score: f.fact.Confidence, JudgeVerdict: "refuted", Reason: "contradicted", ExtractorVersion: "mock:1"}
	ok, err := f.st.RefuteCandidate(context.Background(), f.id, f.hash, audit)
	if err != nil || !ok {
		t.Fatalf("refute = %v, %v", ok, err)
	}
	if f.count(t, "FactCandidate") != 0 || f.count(t, "RejectedFact") != 1 {
		t.Fatal("refutation did not replace candidate with audit")
	}
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (r:RejectedFact {doc_id: '%s'}) RETURN r.reason, r.score, r.judge_verdict", f.doc))
	if !reflect.DeepEqual(rows[0].Values, []any{"contradicted", f.fact.Confidence, "refuted"}) {
		t.Fatalf("audit = %+v", rows[0])
	}
}

func TestCandidateCleanup(t *testing.T) {
	for _, op := range []string{"rewrite", "reset", "delete", "supersede", "migration"} {
		t.Run(op, func(t *testing.T) {
			var st *graph.Store
			if op == "migration" {
				st = graphtest.MigrationStore(t)
			} else {
				st = graphtest.Store(t)
			}
			f := newCandidateFixture(t, st)
			ctx := context.Background()
			var err error
			switch op {
			case "rewrite":
				err = st.UpsertExtractionOpts(ctx, f.doc, f.chunk, "mock:2", nil, nil, graph.WriteOptions{FactsAsCandidates: true})
			case "reset":
				if err = st.SetStatus(ctx, f.doc, graph.StatusDone, ""); err == nil {
					err = st.ResetExtraction(ctx, f.doc, "")
				}
			case "delete":
				err = st.DeleteDocument(ctx, f.doc)
			case "supersede":
				other, _ := entitiesTestDoc(t, st, "Replacement.")
				t.Cleanup(func() { _ = st.DeleteDocument(ctx, other) })
				err = st.Supersede(ctx, f.doc, other)
			case "migration":
				rawRun(t, os.Getenv(graphtest.URIEnv), "MATCH (m:GkgMeta {id: 'extraction_schema'}) SET m.value = 2")
				_, err = st.MigrateExtractionSchema(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if n := f.count(t, "FactCandidate"); n != 0 {
				t.Errorf("%s left %d candidates", op, n)
			}
		})
	}
}

func TestCandidateOnlyMigration(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	rawRun(t, uri, "MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m")
	if _, err := st.MigrateExtractionSchema(ctx); err != nil {
		t.Fatal(err)
	}
	doc, _ := entitiesTestDoc(t, st, "Stored candidate-only document.")
	t.Cleanup(func() { _ = st.DeleteDocument(ctx, doc) })
	if err := st.SetStatus(ctx, doc, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	rawRun(t, uri, fmt.Sprintf("CREATE (:FactCandidate {id: 'candidate-only', doc_id: '%s'})", doc))
	rawRun(t, uri, "MATCH (m:GkgMeta {id: 'extraction_schema'}) DELETE m")
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := st.SchemaVersion(ctx); err != nil || v != 1 {
		t.Fatalf("bootstrap marked candidate-only store current: %d, %v", v, err)
	}
	if _, err := st.MigrateExtractionSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rawCount(t, uri, "MATCH (n:FactCandidate) RETURN count(n) AS n"); n != 0 {
		t.Errorf("migration left %d candidates", n)
	}
	if d, err := st.Document(ctx, doc); err != nil || d.Status != graph.StatusChunked {
		t.Fatalf("migration document = %+v, %v", d, err)
	}
}
