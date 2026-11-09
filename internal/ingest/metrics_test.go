package ingest_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

func gather(t *testing.T, m *obs.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// TestPipelineMetricsIncrement pins the pipeline wiring: one processed
// document increments the status/chunk/entity/fact counters and observes
// the extractor latency on the wired registry.
func TestPipelineMetricsIncrement(t *testing.T) {
	env := newPipelineEnv(t)
	m := obs.New()
	env.p.SetMetrics(m)

	source, text := uniqueText(t)
	mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})

	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(`
# HELP documents_total Documents by pipeline status transition.
# TYPE documents_total counter
documents_total{status="chunked"} 1
documents_total{status="done"} 1
documents_total{status="extracted"} 1
documents_total{status="fetched"} 1
documents_total{status="merged"} 1
documents_total{status="received"} 1
`), "documents_total"); err != nil {
		t.Errorf("documents_total: %v", err)
	}
	scrape := gather(t, m)
	// Exact values for this deterministic fixture: one chunk; the mock
	// emits two person mentions and one 0.9-score fact above the floor.
	for _, want := range []string{"chunks_total 1", "entities_total 2", "facts_total 1", "extractor_errors_total 0"} {
		if !strings.Contains(scrape, want) {
			t.Errorf("scrape missing %q:\n%s", want, scrape)
		}
	}
	if !strings.Contains(scrape, "extract_seconds_count 1") {
		t.Errorf("scrape missing the extract_seconds observation:\n%s", scrape)
	}
}

// TestFactsDroppedCounter pins the store-floor wiring: a fact below the
// floor is audited, never written, and counted as dropped.
func TestFactsDroppedCounter(t *testing.T) {
	env := newPipelineEnv(t)
	env.mock.Score = 0.05 // below the default store floor (0.15)
	m := obs.New()
	env.p.SetMetrics(m)

	source, text := uniqueText(t)
	mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})

	scrape := gather(t, m)
	if !strings.Contains(scrape, "facts_dropped_total 1") {
		t.Errorf("scrape missing facts_dropped_total 1:\n%s", scrape)
	}
	if !strings.Contains(scrape, "facts_total 0") {
		t.Errorf("scrape should have no written facts:\n%s", scrape)
	}
}

// scriptedJudge answers with a scripted verdict or error.
type scriptedJudge struct {
	status string
	err    error
}

func (s scriptedJudge) VerifyFact(context.Context, ingest.FactInput) (ingest.Verdict, error) {
	if s.err != nil {
		return ingest.Verdict{}, s.err
	}
	return ingest.Verdict{Status: s.status}, nil
}

// TestJudgeCountersIncrement pins the judge wiring: verdict statuses,
// failures and latency are counted, and the document still finishes.
func TestJudgeCountersIncrement(t *testing.T) {
	run := func(t *testing.T, j ingest.FactJudge) string {
		t.Helper()
		env := newPipelineEnv(t)
		env.cfg.Judge.Mode = "all"
		env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
		env.p.SetJudge(j)
		m := obs.New()
		env.p.SetMetrics(m)
		source, text := uniqueText(t)
		mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
		return gather(t, m)
	}

	confirmed := run(t, scriptedJudge{status: "confirmed"})
	for _, want := range []string{`judge_verdicts_total{status="confirmed"} 1`, "judge_seconds_count 1", `documents_total{status="verifying"} 1`, `documents_total{status="done"} 1`} {
		if !strings.Contains(confirmed, want) {
			t.Errorf("scrape missing %q:\n%s", want, confirmed)
		}
	}

	failed := run(t, scriptedJudge{err: errors.New("judge down")})
	for _, want := range []string{"judge_failures_total 1", "judge_seconds_count 1", `documents_total{status="done"} 1`} {
		if !strings.Contains(failed, want) {
			t.Errorf("scrape missing %q:\n%s", want, failed)
		}
	}
}

// TestExtractorErrorCounter pins the failure path: an extractor error marks
// the document failed and increments extractor_errors_total.
func TestExtractorErrorCounter(t *testing.T) {
	env := newPipelineEnv(t)
	env.mock.Err = errors.New("extractor boom")
	m := obs.New()
	env.p.SetMetrics(m)

	source, text := uniqueText(t)
	res, err := env.p.Submit(context.Background(), ingest.Input{Kind: "text", Source: source, Text: text})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := env.p.Process(context.Background(), res.DocumentID); err == nil {
		t.Fatal("Process: want the extractor error")
	}
	scrape := gather(t, m)
	if !strings.Contains(scrape, "extractor_errors_total 1") {
		t.Errorf("scrape missing extractor_errors_total 1:\n%s", scrape)
	}
	if !strings.Contains(scrape, `documents_total{status="failed"} 1`) {
		t.Errorf("scrape missing documents_total{status=\"failed\"} 1:\n%s", scrape)
	}
}
