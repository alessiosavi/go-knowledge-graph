package reteval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

type fakeAsker struct {
	response agent.AskResponse
	calls    int
}

type selectiveAsker struct {
	fail  string
	calls int
}

func (f *selectiveAsker) Ask(_ context.Context, req agent.AskRequest) (agent.AskResponse, error) {
	f.calls++
	if req.Question == f.fail {
		return agent.AskResponse{}, errors.New("synthetic provider failure")
	}
	return agent.AskResponse{Answer: "Acme"}, nil
}

func (f *fakeAsker) Ask(_ context.Context, _ agent.AskRequest) (agent.AskResponse, error) {
	f.calls++
	return f.response, nil
}

func evalFixture(t *testing.T) (*graph.Store, string, []string) {
	t.Helper()
	st := graphtest.Store(t)
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ingest.SpoolDir = t.TempDir()
	cfg.Ingest.ChunkWords = 100
	mock := &extract.Mock{}
	p := ingest.NewPipeline(cfg, st, mock, nil, func(_ context.Context, in ingest.Input) (ingest.Loaded, error) {
		return ingest.Loaded{Text: in.Text}, nil
	})
	token := fmt.Sprintf("quillon%d", time.Now().UnixNano())
	var ids []string
	for i, text := range []string{"Acme " + token + " owns Zorbo", "Quillon " + token + " supplies Zorbo"} {
		r, err := p.Submit(context.Background(), ingest.Input{Kind: "text", Source: fmt.Sprintf("%s/%d.txt", token, i), Text: text})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := st.DeleteDocument(context.Background(), r.DocumentID); err != nil {
				t.Error(err)
			}
		})
		if err := p.Process(context.Background(), r.DocumentID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.DocumentID+"-c0")
	}
	if mock.Calls() != 2 {
		t.Fatalf("mock calls = %d", mock.Calls())
	}
	return st, token, ids
}

func TestRunSearchLevel(t *testing.T) {
	st, token, _ := evalFixture(t)
	gold := []Anchor{{Source: "0.txt", Quote: token + " owns"}}
	set := QuestionSet{Schema: 1, Questions: []Question{
		{ID: "s", Stratum: "single", Question: token, Keywords: token, Gold: gold},
		{ID: "m", Stratum: "multi", Question: token, Gold: append(gold, Anchor{Quote: token + " supplies"})},
		{ID: "n", Stratum: "null", Question: "unseenquillon", Absent: []string{"unseenquillon"}},
		{ID: "bad-anchor", Stratum: "single", Question: token, Gold: []Anchor{{Quote: "unseenquillon"}}},
		{ID: "bad-null", Stratum: "null", Question: token, Absent: []string{token}},
	}}
	r, err := Run(context.Background(), Deps{Store: st}, set, Options{Ks: []int{1, 5, 10}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Invalid) != 2 || r.Invalid["bad-anchor"] == "" || r.Invalid["bad-null"] == "" {
		t.Fatalf("invalid = %v", r.Invalid)
	}
	for _, key := range []string{"chunk/question/recall@5", "chunk/keywords/recall@5", "auto/question/mrr"} {
		if stat, ok := r.Strata["single"][key]; !ok || stat.N != 1 {
			t.Errorf("missing/wrong stat %s: %+v", key, r.Strata)
		}
	}
	if len(r.Questions) != 3 {
		t.Fatalf("valid question count = %d", len(r.Questions))
	}
}

func TestGlobalExtrasFolded(t *testing.T) {
	st, token, ids := evalFixture(t)
	set := QuestionSet{Schema: 1, Questions: []Question{{ID: "g", Stratum: "global", Question: token, AnswerItems: []AnswerItem{{Names: []string{"Acme"}, Evidence: []Anchor{{Quote: token + " owns"}}}}}}}
	f := &fakeAsker{response: agent.AskResponse{Answer: "- Acme\n- ZORBÓ\n- Unseenquillon", Evidence: []agent.EvidenceItem{{Result: json.RawMessage(fmt.Sprintf(`{"id":%q}`, ids[0]))}}, Steps: 2}}
	r, err := Run(context.Background(), Deps{Store: st, Agent: f}, set, Options{Agent: true})
	if err != nil {
		t.Fatal(err)
	}
	a := r.Questions[0].Agent
	if a.ItemRecall == nil || *a.ItemRecall != 1 || a.ItemPrecision == nil || *a.ItemPrecision != .5 {
		t.Fatalf("metrics = %+v", a)
	}
	if len(a.UnverifiedExtras) != 1 || Fold(a.UnverifiedExtras[0]) != "zorbo" {
		t.Fatalf("unverified = %v", a.UnverifiedExtras)
	}
	if a.EvidenceCited == nil || !*a.EvidenceCited || a.Steps != 2 || f.calls != 1 {
		t.Fatalf("agent = %+v, calls=%d", a, f.calls)
	}
	if stat, ok := r.Strata["global"]["agent/tool_calls"]; !ok || stat.Mean != 1 || stat.N != 1 {
		t.Fatalf("tool-call metric = %+v, present=%v", stat, ok)
	}
}

func TestRunReplay(t *testing.T) {
	st, token, _ := evalFixture(t)
	set := QuestionSet{Schema: 1, Questions: []Question{{ID: "s", Stratum: "single", Question: token, Gold: []Anchor{{Quote: token + " owns"}}, Answer: []AnswerPart{{Names: []string{"Acme"}}}}, {ID: "n", Stratum: "null", Question: "unseenquillon", Absent: []string{"unseenquillon"}}}}
	stored := Report{Schema: 1, Questions: []QuestionResult{{ID: "s", Agent: &AgentResult{Answer: "Acme", AnswerRecall: new(float64), Steps: 3}}, {ID: "n", Agent: &AgentResult{Answer: "I cannot answer, but Acme owns Zorbo"}}}}
	path := questionFile(t, stored)
	f := &fakeAsker{}
	r, err := Run(context.Background(), Deps{Store: st, Agent: f}, set, Options{ReplayPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != "replay" || f.calls != 0 || *r.Questions[0].Agent.AnswerRecall != 1 || r.Questions[0].Agent.Steps != 3 {
		t.Fatalf("replay = %+v calls=%d", r, f.calls)
	}
	a := r.Questions[1].Agent
	if a.Abstained == nil || !*a.Abstained || !a.NeedsReview {
		t.Fatalf("mixed abstention = %+v", a)
	}
	if !strings.Contains(Markdown(r), "replay") {
		t.Fatal("summary missing source")
	}
	// A replay must not silently turn missing answers into a search-only run.
	stored.Questions = stored.Questions[:1]
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "missing.json"), mustJSON(t, stored), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Deps{Store: st}, set, Options{ReplayPath: filepath.Join(filepath.Dir(path), "missing.json")}); err == nil {
		t.Fatal("missing replay answer accepted")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEvidenceCitedExactChunkID(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		want         bool
	}{
		{"prefix", `{"id":"acme-c10"}`, false},
		{"exact", `{"id":"acme-c1"}`, true},
		{"search-prefix", `{"hits":[{"kind":"chunk","id":"acme-c10"}]}`, false},
		{"search-exact", `{"hits":[{"kind":"chunk","id":"acme-c10"},{"kind":"chunk","id":"acme-c1"}]}`, true},
		{"fact-prefix", `{"facts":[{"chunk_id":"acme-c10"}]}`, false},
		{"fact-exact", `{"facts":[{"chunk_id":"acme-c1"}]}`, true},
		{"text-is-not-id", `{"id":"acme-c10","snippet":"acme-c1"}`, false},
		{"empty", `null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &AgentResult{Evidence: []agent.EvidenceItem{{Result: json.RawMessage(tc.result)}}}
			g := resolvedGold{anchors: [][]string{{"acme-c1"}}}
			if err := scoreAgent(context.Background(), nil, Question{Stratum: "single"}, g, a); err != nil {
				t.Fatal(err)
			}
			if a.EvidenceCited == nil || *a.EvidenceCited != tc.want {
				t.Fatalf("evidence_cited=%v, want %t; result=%s", a.EvidenceCited, tc.want, tc.result)
			}
		})
	}
}

func TestRunContinuesAfterAskFailure(t *testing.T) {
	st, token, _ := evalFixture(t)
	for _, failedFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("failed-first-%t", failedFirst), func(t *testing.T) {
			good := Question{ID: "scored", Stratum: "single", Question: token, Gold: []Anchor{{Quote: token + " owns"}}, Answer: []AnswerPart{{Names: []string{"Acme"}}}}
			bad := good
			bad.ID, bad.Question = "failed", token+" unavailable"
			questions := []Question{good, bad}
			if failedFirst {
				questions = []Question{bad, good}
			}
			f := &selectiveAsker{fail: bad.Question}
			r, err := Run(context.Background(), Deps{Store: st, Agent: f}, QuestionSet{Schema: 1, Questions: questions}, Options{Agent: true})
			if err != nil {
				t.Fatalf("partial run aborted: %v", err)
			}
			if len(r.Questions) != 1 || r.Questions[0].ID != "scored" || len(r.Invalid) != 0 || f.calls != 2 {
				t.Fatalf("questions=%+v invalid=%v calls=%d", r.Questions, r.Invalid, f.calls)
			}
			var report struct {
				Failed []struct{ ID, Error string } `json:"failed"`
			}
			if err := json.Unmarshal(mustJSON(t, r), &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Failed) != 1 || report.Failed[0].ID != "failed" || !strings.Contains(report.Failed[0].Error, "synthetic provider failure") {
				t.Fatalf("failed=%+v", report.Failed)
			}
			for _, metric := range []string{"chunk/question/recall@5", "agent/answer_recall"} {
				if stat := r.Strata["single"][metric]; stat.N != 1 || stat.Mean != 1 {
					t.Errorf("%s=%+v", metric, stat)
				}
			}
		})
	}
}

func TestRunNoQuestionScored(t *testing.T) {
	st, token, _ := evalFixture(t)
	for _, tc := range []struct {
		name string
		qs   []Question
	}{
		{"failed", []Question{{ID: "failed", Stratum: "single", Question: token, Gold: []Anchor{{Quote: token + " owns"}}, Answer: []AnswerPart{{Names: []string{"Acme"}}}}}},
		{"invalid", []Question{{ID: "invalid", Stratum: "single", Question: token, Gold: []Anchor{{Quote: "unseenquillon"}}, Answer: []AnswerPart{{Names: []string{"Acme"}}}}}},
		{"empty", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &selectiveAsker{fail: token}
			r, err := Run(context.Background(), Deps{Store: st, Agent: f}, QuestionSet{Schema: 1, Questions: tc.qs}, Options{Agent: true})
			if err == nil || !strings.Contains(err.Error(), "no question scored") || r.Schema != 1 || len(r.Questions) != 0 || len(r.Strata) != 0 {
				t.Fatalf("report=%+v err=%v", r, err)
			}
		})
	}
}

func TestRunRequiresAnswerBeforeAnyAsk(t *testing.T) {
	st, token, _ := evalFixture(t)
	valid := Question{ID: "valid", Stratum: "single", Question: token, Gold: []Anchor{{Quote: token + " owns"}}, Answer: []AnswerPart{{Names: []string{"Acme"}}}}
	single, multi := valid, valid
	single.ID, single.Answer = "missing-single", nil
	multi.ID, multi.Stratum, multi.Answer = "missing-multi", "multi", nil
	set := QuestionSet{Schema: 1, Questions: []Question{valid, single, multi}}
	// Loading remains valid for a search-only evaluation.
	if _, err := Load(questionFile(t, set)); err != nil {
		t.Fatalf("search-only load rejected: %v", err)
	}
	stored := Report{Schema: 1}
	for _, q := range set.Questions {
		stored.Questions = append(stored.Questions, QuestionResult{ID: q.ID, Agent: &AgentResult{Answer: "Acme"}})
	}
	for _, opt := range []Options{{Agent: true}, {ReplayPath: questionFile(t, stored)}} {
		name := "agent"
		if opt.ReplayPath != "" {
			name = "replay"
		}
		t.Run(name, func(t *testing.T) {
			f := &fakeAsker{response: agent.AskResponse{Answer: "Acme"}}
			_, err := Run(context.Background(), Deps{Store: st, Agent: f}, set, opt)
			if err == nil || !strings.Contains(err.Error(), "answer") || !strings.Contains(err.Error(), "missing-single") || !strings.Contains(err.Error(), "missing-multi") || f.calls != 0 {
				t.Fatalf("answer preflight: err=%v asks=%d", err, f.calls)
			}
		})
	}
}

func TestRunNullOmitsSearch(t *testing.T) {
	st := graphtest.Store(t)
	token := fmt.Sprintf("unseenquillon%d", time.Now().UnixNano())
	set := QuestionSet{Schema: 1, Questions: []Question{{ID: "n", Stratum: "null", Question: token, Keywords: token, Absent: []string{token}}}}
	r, err := Run(context.Background(), Deps{Store: st}, set, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Questions) != 1 {
		t.Fatalf("questions=%+v", r.Questions)
	}
	var q map[string]json.RawMessage
	if err := json.Unmarshal(mustJSON(t, r.Questions[0]), &q); err != nil {
		t.Fatal(err)
	}
	if r.Questions[0].Search != nil || q["search"] != nil || len(r.Strata["null"]) != 0 {
		t.Fatalf("null search=%+v json=%s strata=%+v", r.Questions[0].Search, mustJSON(t, q), r.Strata)
	}
}

func TestRunDomainScopesSearchGoldAndNull(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	word := fmt.Sprintf("quillon%d", time.Now().UnixNano())
	for _, slug := range []string{"a", "b"} {
		doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: word + slug, Hash: word, Domain: slug})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.DeleteDocument(ctx, doc.ID) })
		text := word + " owns Zorbo"
		if slug == "b" {
			text += " " + word + "absent"
		}
		if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text}}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetStatus(ctx, doc.ID, graph.StatusDone, ""); err != nil {
			t.Fatal(err)
		}
	}
	set := QuestionSet{Schema: 1, Questions: []Question{
		{ID: "s", Stratum: "single", Question: word, Domain: "a", Gold: []Anchor{{Quote: word + " owns"}}, Kinds: []string{"chunk"}},
		{ID: "n", Stratum: "null", Question: "Where is Zorbo?", Domain: "a", Absent: []string{word + "absent"}},
	}}
	report, err := Run(ctx, Deps{Store: st}, set, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Questions) != 2 || len(report.Invalid) != 0 {
		t.Fatalf("report=%+v", report)
	}
	if len(report.Questions[0].CrossDomain) != 1 || report.Questions[0].Search[0].Recall["5"] != 1 {
		t.Fatalf("scoped gold/search=%+v", report.Questions[0])
	}
	report, err = Run(ctx, Deps{Store: st}, set, Options{Domain: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Invalid) != 1 || report.Invalid["n"] == "" || report.Questions[0].Domain != "b" {
		t.Fatalf("override=%+v", report)
	}
	if set.Questions[0].Domain != "a" {
		t.Fatal("Run changed its caller's question scope")
	}
}
