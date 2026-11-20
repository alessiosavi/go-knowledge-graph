package judge_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

type backfillFixture struct {
	st                 *graph.Store
	doc, chunk, domain string
	inputs             []ingest.FactInput
	ids                []string
}

func backfillQuery(t *testing.T, q string, params map[string]any) {
	t.Helper()
	if err := backfillQueryErr(context.Background(), q, params); err != nil {
		t.Fatal(err)
	}
}

func backfillQueryErr(ctx context.Context, q string, params map[string]any) error {
	d, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		return err
	}
	defer d.Close(ctx)
	s := d.NewSession(ctx, neo4j.SessionConfig{})
	defer s.Close(ctx)
	r, err := s.Run(ctx, q, params)
	if err == nil {
		_, err = r.Consume(ctx)
	}
	return err
}

func newBackfillFixture(t *testing.T, n int) backfillFixture {
	t.Helper()
	st := graphtest.Store(t)
	tag := fmt.Sprintf("Acme-Max-%s-%d", t.Name(), time.Now().UnixNano())
	doc, err := st.UpsertDocument(t.Context(), graph.DocumentInput{Kind: "text", Source: tag, Hash: tag, Title: tag})
	if err != nil {
		t.Fatal(err)
	}
	f := backfillFixture{st: st, doc: doc.ID, chunk: doc.ID + "-c0", domain: tag}
	t.Cleanup(func() {
		if err := st.DeleteDocument(context.Background(), doc.ID); err != nil && !errors.Is(err, graph.ErrNotFound) {
			t.Error(err)
		}
	})
	if err := st.PutChunks(t.Context(), doc.ID, []graph.Chunk{{Text: "Acme-Max uses Zorbo. Another sentence.", End: 36}}); err != nil {
		t.Fatal(err)
	}
	ents := []graph.EntityUpsert{{Key: tag + "-head", Name: "Acme-Max", Type: "organization"}}
	var facts []graph.FactUpsert
	for i := 0; i < n; i++ {
		tail := fmt.Sprintf("Zorbo%d", i)
		in := ingest.FactInput{Head: "Acme-Max", Rel: "uses", Tail: tail, Confidence: 0.7, Evidence: "Acme-Max uses Zorbo.", DocContext: tag, DocID: doc.ID}
		f.inputs = append(f.inputs, in)
		ents = append(ents, graph.EntityUpsert{Key: tag + tail, Name: tail, Type: "organization"})
		fact := graph.FactUpsert{HeadKey: ents[0].Key, TailKey: tag + tail, Rel: "uses", Confidence: 0.7, HeadMention: graph.Span{Start: 0, End: 8}, TailMention: graph.Span{Start: 14, End: 19}, JudgeInput: ingest.CanonicalJudgeInput(in)}
		facts = append(facts, fact)
		f.ids = append(f.ids, graph.CandidateID(fact.HeadKey, fact.Rel, fact.TailKey, f.chunk))
	}
	if err := st.UpsertExtractionOpts(t.Context(), doc.ID, f.chunk, "mock:1", ents, facts, graph.WriteOptions{FactsAsCandidates: true}); err != nil {
		t.Fatal(err)
	}
	backfillQuery(t, "MATCH (c:FactCandidate {doc_id: $doc}) SET c.domain = $domain", map[string]any{"doc": doc.ID, "domain": f.domain})
	return f
}

func (f backfillFixture) cached(j *judge.Client) *judge.Cached {
	return judge.NewCached(j, f.st, j.Model())
}
func (f backfillFixture) opt() judge.BackfillOptions { return judge.BackfillOptions{Domain: f.domain} }
func (f backfillFixture) count(t *testing.T, label string) int {
	t.Helper()
	r, err := f.st.RunReadQuery(t.Context(), fmt.Sprintf("MATCH (n:%s {doc_id: $doc}) RETURN count(n) AS n", label), map[string]any{"doc": f.doc}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return int(r.Rows[0]["n"].(int64))
}

func TestBackfillJudgesUnjudged(t *testing.T) {
	f := newBackfillFixture(t, 3)
	var order []string
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		in, err := ingest.DecodeJudgeInput([]byte(body.Messages[1].Content))
		if err != nil {
			t.Error(err)
			return
		}
		order = append(order, in.Tail)
		statuses := map[string]string{"Zorbo0": "confirmed", "Zorbo1": "unclear", "Zorbo2": "refuted"}
		_, _ = w.Write([]byte(chatBody(fmt.Sprintf(`{"verdict":%q,"reason":"synthetic evidence"}`, statuses[in.Tail]))))
	})
	got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
	if err != nil || got.Judged != 3 || got.Confirmed != 1 || got.Unclear != 1 || got.Refuted != 1 || got.Vanished != 0 || got.Attempts != 3 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	var want []string
	ids := slices.Clone(f.ids)
	slices.Sort(ids)
	for _, id := range ids {
		i := slices.Index(f.ids, id)
		want = append(want, f.inputs[i].Tail)
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order=%v want=%v", order, want)
	}
	if f.count(t, "FactCandidate") != 1 || f.count(t, "RejectedFact") != 1 {
		t.Fatal("verdicts not applied")
	}
	for _, query := range []string{
		"MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = $doc RETURN r.judge_model AS model",
		"MATCH (c:FactCandidate {doc_id:$doc}) RETURN c.judge_model AS model",
		"MATCH (a:RejectedFact {doc_id:$doc}) RETURN a.judge_model AS model",
	} {
		rows, err := f.st.RunReadQuery(t.Context(), query, map[string]any{"doc": f.doc}, 0)
		if err != nil || len(rows.Rows) != 1 || rows.Rows[0]["model"] != j.Model() {
			t.Errorf("candidate backfill provenance on %s = %+v, %v", query, rows, err)
		}
	}
	for _, in := range f.inputs {
		v, ok, err := f.st.JudgeVerdict(t.Context(), judge.RequestKey(ingest.CanonicalJudgeInput(in), j.Model()))
		if err != nil || !ok || v.DocID != f.doc {
			t.Fatalf("DocID missing from cached verdict: %+v found=%v err=%v", v, ok, err)
		}
	}
	// Unclear is a judged state; --force only takes over the lease.
	opt := f.opt()
	opt.Force = true
	again, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, opt)
	if err != nil || again.Judged != 0 || again.Attempts != 0 {
		t.Fatalf("unclear rejudged: %+v %v", again, err)
	}
	if err := f.st.DeleteDocument(t.Context(), f.doc); err != nil {
		t.Fatal(err)
	}
	for _, in := range f.inputs {
		_, ok, err := f.st.JudgeVerdict(t.Context(), judge.RequestKey(ingest.CanonicalJudgeInput(in), j.Model()))
		if err != nil || ok {
			t.Fatalf("document deletion left cache: found=%v err=%v", ok, err)
		}
	}
	t.Log("candidates=3 applied=3; confirmed=1 unclear=1 refuted=1; attempts=3; document-owned verdicts=3")
}

type cancelAfterCache struct {
	*graph.Store
	cancel context.CancelFunc
}

func (s cancelAfterCache) PutJudgeVerdict(ctx context.Context, v graph.JudgeVerdict) error {
	err := s.Store.PutJudgeVerdict(ctx, v)
	if err == nil {
		s.cancel()
	}
	return err
}

func TestBackfillResumeNoRepeatCalls(t *testing.T) {
	f := newBackfillFixture(t, 1)
	calls := 0
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := judge.NewCached(j, cancelAfterCache{f.st, cancel}, j.Model())
	_, err := judge.Backfill(ctx, f.st, c, j, f.opt())
	if !errors.Is(err, context.Canceled) || f.count(t, "FactCandidate") != 1 {
		t.Fatalf("crash err=%v, candidate must remain", err)
	}
	key := judge.RequestKey(ingest.CanonicalJudgeInput(f.inputs[0]), j.Model())
	backfillQuery(t, "MATCH (v:JudgeVerdict {key:$key}) SET v.model = 'cached-model'", map[string]any{"key": key})
	got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
	if err != nil || got.Confirmed != 1 || got.CacheHits != 1 || got.Attempts != 0 || calls != 1 {
		t.Fatalf("resume=%+v err=%v total calls=%d", got, err, calls)
	}
	rows, err := f.st.RunReadQuery(t.Context(),
		"MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = $doc RETURN r.judge_model AS model", map[string]any{"doc": f.doc}, 0)
	if err != nil || len(rows.Rows) != 1 || rows.Rows[0]["model"] != "cached-model" {
		t.Fatalf("resumed edge provenance = %+v, %v", rows, err)
	}
	t.Log("resume: applied=1 cache_hits=1 attempts=0")
}

func TestBackfillSkipsVanished(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed=%v", changed), func(t *testing.T) {
			f := newBackfillFixture(t, 1)
			j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
				q := "MATCH (c:FactCandidate {id: $id}) DELETE c"
				if changed {
					q = "MATCH (c:FactCandidate {id: $id}) SET c.input_hash = 'new-input-hash'"
				}
				backfillQuery(t, q, map[string]any{"id": f.ids[0]})
				_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
			})
			got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
			if err != nil || got.Vanished != 1 || got.Confirmed != 0 {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			rows, err := f.st.RunReadQuery(t.Context(), "MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = $doc RETURN r", map[string]any{"doc": f.doc}, 0)
			if err != nil || len(rows.Rows) != 0 {
				t.Fatalf("stale verdict created an edge: %+v %v", rows, err)
			}
		})
	}
}

func TestBackfillIsolatesCandidateFailures(t *testing.T) {
	for _, failure := range []string{"provider", "judge_input"} {
		t.Run(failure, func(t *testing.T) {
			f := newBackfillFixture(t, 3)
			cs, err := f.st.UnjudgedCandidates(t.Context(), f.domain, 0)
			if err != nil || len(cs) != 3 {
				t.Fatalf("candidates=%+v err=%v", cs, err)
			}
			if failure == "judge_input" {
				backfillQuery(t, "MATCH (c:FactCandidate {id: $id}) SET c.judge_input = '{'", map[string]any{"id": cs[0].ID})
			}
			var order []string
			badInput, err := ingest.DecodeJudgeInput([]byte(cs[0].JudgeInput))
			if err != nil {
				t.Fatal(err)
			}
			j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct{ Messages []struct{ Content string } }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				in, err := ingest.DecodeJudgeInput([]byte(body.Messages[1].Content))
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				order = append(order, in.Tail)
				if in.Tail == badInput.Tail {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
			})
			got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
			if err != nil || got.Judged != 2 || got.Confirmed != 2 || got.Vanished != 0 || got.Failed != 1 {
				t.Fatalf("failure blocked later candidates: result=%+v err=%v", got, err)
			}
			want := make([]string, 0, 3)
			for i, c := range cs {
				if failure == "judge_input" && i == 0 {
					continue
				}
				in, err := ingest.DecodeJudgeInput([]byte(c.JudgeInput))
				if err != nil {
					t.Fatal(err)
				}
				want = append(want, in.Tail)
			}
			if !reflect.DeepEqual(order, want) || got.Attempts != len(want) {
				t.Fatalf("order=%v want=%v attempts=%d", order, want, got.Attempts)
			}
			remaining, err := f.st.UnjudgedCandidates(t.Context(), f.domain, 0)
			if err != nil || len(remaining) != 1 || remaining[0].ID != cs[0].ID {
				t.Fatalf("failed candidate must remain retryable: %+v err=%v", remaining, err)
			}
			t.Logf("%s: first candidate failed, later candidates confirmed=2 attempts=%d", failure, got.Attempts)
		})
	}
}

func TestBackfillRemovesOrphanedCandidate(t *testing.T) {
	for _, endpoint := range []string{"head", "tail"} {
		t.Run(endpoint, func(t *testing.T) {
			f := newBackfillFixture(t, 1)
			cs, err := f.st.UnjudgedCandidates(t.Context(), f.domain, 0)
			if err != nil || len(cs) != 1 {
				t.Fatalf("candidates=%+v err=%v", cs, err)
			}
			key := cs[0].HeadKey
			if endpoint == "tail" {
				key = cs[0].TailKey
			}
			j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
				backfillQuery(t, "MATCH (e:Entity {key: $key}) DETACH DELETE e", map[string]any{"key": key})
				_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
			})
			got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
			if err != nil || got.Vanished != 0 || got.Orphaned != 1 || got.Confirmed != 0 || f.count(t, "FactCandidate") != 0 {
				t.Fatalf("orphan not removed distinctly: result=%+v err=%v", got, err)
			}
			again, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
			if err != nil || again.Judged != 0 || again.Attempts != 0 || again.CacheHits != 0 {
				t.Fatalf("orphan occupied rerun: result=%+v err=%v", again, err)
			}
		})
	}
}

func TestBackfillKeepsChangedOrphan(t *testing.T) {
	f := newBackfillFixture(t, 1)
	cs, err := f.st.UnjudgedCandidates(t.Context(), f.domain, 0)
	if err != nil || len(cs) != 1 {
		t.Fatalf("candidates=%+v err=%v", cs, err)
	}
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
		backfillQuery(t, "MATCH (e:Entity {key: $key}) DETACH DELETE e", map[string]any{"key": cs[0].HeadKey})
		backfillQuery(t, "MATCH (c:FactCandidate {id: $id}) SET c.input_hash = 'changed'", map[string]any{"id": cs[0].ID})
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
	})
	got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
	if err != nil || got.Vanished != 1 || got.Orphaned != 0 || f.count(t, "FactCandidate") != 1 {
		t.Fatalf("changed orphan consumed: result=%+v err=%v", got, err)
	}
}

func TestBackfillDocumentVanishedDuringCall(t *testing.T) {
	f := newBackfillFixture(t, 1)
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
		if err := f.st.DeleteDocument(t.Context(), f.doc); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
	})
	got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
	if err != nil || got.Vanished != 1 || got.Confirmed != 0 || got.Attempts != 1 {
		t.Fatalf("deleted document: result=%+v err=%v", got, err)
	}
	if n := f.count(t, "JudgeVerdict"); n != 0 {
		t.Fatalf("deleted document retained %d verdicts", n)
	}
	rows, err := f.st.RunReadQuery(t.Context(), "MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = $doc RETURN r", map[string]any{"doc": f.doc}, 0)
	if err != nil || len(rows.Rows) != 0 || f.count(t, "FactCandidate") != 0 {
		t.Fatalf("deleted document retained derived facts: %+v err=%v", rows, err)
	}
	t.Log("document deleted in provider handler: vanished=1 verdicts=0 edges=0 candidates=0")
}

func TestBackfillFailureDoesNotHideLeaseLoss(t *testing.T) {
	f := newBackfillFixture(t, 1)
	const owner = "Quillon-failed-call-owner"
	t.Cleanup(func() { _ = f.st.ReleaseLease(context.Background(), "judge-backfill", owner) })
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
		ok, err := f.st.AcquireLease(t.Context(), "judge-backfill", owner, 120*time.Second, true)
		if err != nil || !ok {
			t.Errorf("takeover=%v err=%v", ok, err)
		}
		w.WriteHeader(http.StatusBadRequest)
	})
	got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
	if err == nil || !strings.Contains(err.Error(), "lease lost") || got.Failed != 0 || got.Judged != 0 || f.count(t, "FactCandidate") != 1 {
		t.Fatalf("candidate error hid lost lease: result=%+v err=%v", got, err)
	}
}

func TestBackfillBudgetCountsRetries(t *testing.T) {
	f := newBackfillFixture(t, 1)
	calls := 0
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
	})
	j.SetBackoff([]time.Duration{0, 0})
	opt := f.opt()
	opt.BudgetCalls = 2
	got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, opt)
	if err != nil || !got.StoppedForBudget || got.Attempts != 2 || got.Judged != 0 || calls != 2 || f.count(t, "FactCandidate") != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", got, err, calls)
	}
	// A later run has its own gate and budget.
	got, err = judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
	if err != nil || got.Confirmed != 1 || got.Attempts != 1 {
		t.Fatalf("resume=%+v %v", got, err)
	}
	t.Log("retry budget=2: attempts=2, candidate stays unjudged; rerun applies it")
}

func TestBackfillDoneDocument(t *testing.T) {
	f := newBackfillFixture(t, 1)
	if err := f.st.SetStatus(t.Context(), f.doc, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
	})
	got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
	if err != nil || got.Confirmed != 1 {
		t.Fatalf("done document: %+v %v", got, err)
	}
}

func TestBackfillLimitAndDomain(t *testing.T) {
	f := newBackfillFixture(t, 3)
	other := newBackfillFixture(t, 1)
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
	})
	opt := f.opt()
	opt.Limit = 2
	got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, opt)
	if err != nil || got.Judged != 2 || f.count(t, "FactCandidate") != 1 || other.count(t, "FactCandidate") != 1 {
		t.Fatalf("limit/domain: %+v %v", got, err)
	}
}

func TestBackfillCacheMatchesIngest(t *testing.T) {
	st := graphtest.Store(t)
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ingest.SpoolDir = t.TempDir()
	cfg.Ingest.ChunkDedup = "off"
	cfg.Judge.Mode = "all"
	var payloads []string
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		payloads = append(payloads, string(b))
		_, _ = w.Write([]byte(chatBody(`{"verdict":"unclear"}`)))
	})
	p := ingest.NewPipeline(cfg, st, &extract.Mock{}, nil, func(_ context.Context, in ingest.Input) (ingest.Loaded, error) {
		return ingest.Loaded{Text: in.Text}, nil
	})
	p.SetWriteMode(ingest.WriteMode{RequireConfirmation: true})
	p.SetJudge(judge.NewCached(j, st, j.Model()))
	tag := fmt.Sprintf("Acme-Max%d", time.Now().UnixNano())
	res, err := p.Submit(t.Context(), ingest.Input{Kind: "text", Source: tag, Text: tag + " uses Zorbo."})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), res.DocumentID) })
	if err := p.Process(t.Context(), res.DocumentID); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash window after the ingest verdict was cached.
	backfillQuery(t, "MATCH (c:FactCandidate {doc_id: $doc}) SET c.judge = '', c.domain = $domain", map[string]any{"doc": res.DocumentID, "domain": tag})
	got, err := judge.Backfill(t.Context(), st, judge.NewCached(j, st, j.Model()), j, judge.BackfillOptions{Domain: tag})
	if err != nil || got.CacheHits != 1 || got.Attempts != 0 || len(payloads) != 1 {
		t.Fatalf("ingest cache hit: %+v %v payloads=%d", got, err, len(payloads))
	}
	backfillQuery(t, "MATCH (v:JudgeVerdict {doc_id: $doc}) DELETE v", map[string]any{"doc": res.DocumentID})
	backfillQuery(t, "MATCH (c:FactCandidate {doc_id: $doc}) SET c.judge = ''", map[string]any{"doc": res.DocumentID})
	got, err = judge.Backfill(t.Context(), st, judge.NewCached(j, st, j.Model()), j, judge.BackfillOptions{Domain: tag})
	if err != nil || got.Attempts != 1 || len(payloads) != 2 || payloads[0] != payloads[1] {
		t.Fatalf("wire payload mismatch: %+v %v payloads=%v", got, err, payloads)
	}
	t.Log("ingest/backfill payload bytes identical; ingest cache reused with 0 attempts")
}

func TestBackfillLeaseTakeover(t *testing.T) {
	f := newBackfillFixture(t, 1)
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
		ok, err := f.st.AcquireLease(t.Context(), "judge-backfill", "Quillon-owner", 120*time.Second, true)
		if err != nil || !ok {
			t.Errorf("takeover=%v %v", ok, err)
		}
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
	})
	t.Cleanup(func() { _ = f.st.ReleaseLease(context.Background(), "judge-backfill", "Quillon-owner") })
	got, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
	if err == nil || !strings.Contains(err.Error(), "lease lost") || got.Confirmed != 0 || f.count(t, "FactCandidate") != 1 {
		t.Fatalf("old owner applied: %+v %v", got, err)
	}
	// Cleanup by the displaced run must leave the new owner's live lease.
	got, err = judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt())
	if err == nil || got.Attempts != 0 {
		t.Fatalf("live owner not respected: %+v %v", got, err)
	}
	opt := f.opt()
	opt.Force = true
	got, err = judge.Backfill(t.Context(), f.st, f.cached(j), j, opt)
	if err != nil || got.Confirmed != 1 || got.CacheHits != 1 || got.Attempts != 0 {
		t.Fatalf("forced recovery: %+v %v", got, err)
	}
}

func TestBackfillRenewsLease(t *testing.T) {
	f := newBackfillFixture(t, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
		case <-r.Context().Done():
		}
	})
	results := make(chan error, 1)
	go func() { _, err := judge.Backfill(t.Context(), f.st, f.cached(j), j, f.opt()); results <- err }()
	<-entered
	readExpiry := func() int64 {
		t.Helper()
		r, err := f.st.RunReadQuery(t.Context(), "MATCH (l:JudgeLease {id: 'judge-backfill'}) RETURN l.expires_at AS expires", nil, 0)
		if err != nil || len(r.Rows) != 1 {
			t.Fatalf("lease=%+v err=%v", r, err)
		}
		return r.Rows[0]["expires"].(int64)
	}
	first := readExpiry()
	if remaining := time.Until(time.UnixMilli(first)); remaining < 115*time.Second || remaining > 121*time.Second {
		t.Errorf("TTL=%s want 120s", remaining)
	}
	deadline := time.NewTimer(36 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	renewed := false
wait:
	for {
		select {
		case <-poll.C:
			if readExpiry() > first+20000 {
				renewed = true
				break wait
			}
		case <-deadline.C:
			break wait
		}
	}
	close(release)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if !renewed {
		t.Fatal("120s lease was not renewed after 30s during provider call")
	}
	t.Log("TTL=120s; renewed at 30s while provider request remained in flight")
}

func TestBackfillCachedAfterBudget(t *testing.T) {
	f := newBackfillFixture(t, 2)
	ids := slices.Clone(f.ids)
	slices.Sort(ids)
	i := slices.Index(f.ids, ids[1])
	in := f.inputs[i]
	if err := f.st.PutJudgeVerdict(t.Context(), graph.JudgeVerdict{Key: judge.RequestKey(ingest.CanonicalJudgeInput(in), "deepseek-v4.1-flash"), Status: "confirmed", DocID: f.doc}); err != nil {
		t.Fatal(err)
	}
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(chatBody(`{"verdict":"confirmed"}`)))
	})
	c := f.cached(j)
	metrics := cacheResults{}
	c.SetCacheMetrics(metrics)
	opt := f.opt()
	opt.BudgetCalls = 1
	got, err := judge.Backfill(t.Context(), f.st, c, j, opt)
	if err != nil || got.Confirmed != 2 || got.Attempts != 1 || got.CacheHits != 1 || metrics["hit"] != 1 || metrics["miss"] != 1 {
		t.Fatalf("cache after budget: %+v %v metrics=%v", got, err, metrics)
	}
}

func TestUnjudgedCandidateWithoutDomain(t *testing.T) {
	f := newBackfillFixture(t, 1)
	backfillQuery(t, "MATCH (c:FactCandidate {doc_id: $doc}) REMOVE c.domain", map[string]any{"doc": f.doc})
	cs, err := f.st.UnjudgedCandidates(t.Context(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cs {
		if c.ID == f.ids[0] {
			found = true
			if c.Domain != "" {
				t.Fatalf("domain=%q", c.Domain)
			}
		}
	}
	if !found {
		t.Fatal("candidate lacking domain was excluded")
	}
}
