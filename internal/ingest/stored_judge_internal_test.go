package ingest

import (
	"context"
	"encoding/hex"
	"math"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

func TestPrepareStoredJudgeInputs(t *testing.T) {
	metrics := obs.New()
	p := &Pipeline{metrics: metrics}
	scrape := func() string {
		rec := httptest.NewRecorder()
		metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}
	if strings.Contains(scrape(), "judge_input_total") {
		t.Fatal("new counters are visible before any opted-in event")
	}
	doc := &graph.Document{ID: "doc", Title: "Context"}
	chunk := graph.Chunk{Text: "Acme uses Zorbo."}
	f := graph.FactUpsert{HeadKey: "acme", Rel: "uses", TailKey: "zorbo", Confidence: 0.75}
	facts := []graph.FactUpsert{f, f, f}
	facts[2].Confidence = math.NaN()
	p.prepareStoredJudgeInputs(doc, chunk, facts)
	generations := make(map[string]bool, len(facts))
	for _, fact := range facts {
		gen, err := hex.DecodeString(fact.JudgeGen)
		if err != nil || len(gen) != 16 || generations[fact.JudgeGen] {
			t.Fatalf("generation = %q, want fresh 128-bit hex", fact.JudgeGen)
		}
		generations[fact.JudgeGen] = true
		if string(fact.JudgeInput) != string(CanonicalJudgeInput(factInputFor(doc, chunk, fact))) {
			t.Fatalf("stored fact = %+v", fact)
		}
	}
	if facts[2].JudgeInput != nil {
		t.Fatalf("non-finite confidence produced a payload: %s", facts[2].JudgeInput)
	}
	if !strings.Contains(scrape(), `judge_input_total{result="encode_error"} 1`) {
		t.Fatal("nil judge input was not counted")
	}

	j := &invalidStoredJudge{}
	p.cfg.Judge = config.JudgeConfig{StoreInputs: true, Mode: "all"}
	p.SetJudge(j)
	p.verifyFacts(t.Context(), doc, []judgeCandidate{
		{fact: facts[0], inputHash: "persisted hash"},
		{fact: facts[2]},
	}, "test")
	if j.legacyCalls != 0 || j.payloadCalls != 1 || j.payload.InputHash != "persisted hash" {
		t.Fatalf("online replay calls = %+v", j)
	}
	body := scrape()
	if !strings.Contains(body, `judge_input_total{result="invalid"} 1`) || !strings.Contains(body, "judge_failures_total 0") {
		t.Fatalf("invalid replay was not counted separately: %s", body)
	}
}

type invalidStoredJudge struct {
	legacyCalls, payloadCalls int
	payload                   JudgePayload
}

func (j *invalidStoredJudge) VerifyFact(context.Context, FactInput) (Verdict, error) {
	j.legacyCalls++
	return Verdict{}, ErrInvalidJudgePayload
}

func (j *invalidStoredJudge) VerifyPayload(_ context.Context, p JudgePayload, _ string) (Verdict, error) {
	j.payloadCalls++
	j.payload = p
	return Verdict{}, ErrInvalidJudgePayload
}
