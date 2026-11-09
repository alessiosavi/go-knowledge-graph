package obs

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCountersIncrement(t *testing.T) {
	m := New()
	m.DocumentStatus("done")
	m.DocumentStatus("done")
	m.DocumentStatus("failed")
	m.Chunks(5)
	m.Entities(3)
	m.Facts(2)
	m.FactsDropped(1)
	m.JudgeVerdict("confirmed")
	m.JudgeVerdict("refuted")
	m.JudgeFailure()
	m.ExtractorError()
	m.ExtractSeconds(0.5)
	m.JudgeSeconds(0.25)

	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(`
# HELP documents_total Documents by pipeline status transition.
# TYPE documents_total counter
documents_total{status="done"} 2
documents_total{status="failed"} 1
`), "documents_total"); err != nil {
		t.Errorf("documents_total: %v", err)
	}
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(`
# HELP entities_total Entities merged into the graph.
# TYPE entities_total counter
entities_total 3
`), "entities_total"); err != nil {
		t.Errorf("entities_total: %v", err)
	}
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(`
# HELP facts_dropped_total Facts dropped below the store floor.
# TYPE facts_dropped_total counter
facts_dropped_total 1
`), "facts_dropped_total"); err != nil {
		t.Errorf("facts_dropped_total: %v", err)
	}
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(`
# HELP judge_verdicts_total Judge verdicts by status.
# TYPE judge_verdicts_total counter
judge_verdicts_total{status="confirmed"} 1
judge_verdicts_total{status="refuted"} 1
`), "judge_verdicts_total"); err != nil {
		t.Errorf("judge_verdicts_total: %v", err)
	}
}

func TestMetricsHandlerServesRegistry(t *testing.T) {
	m := New()
	m.DocumentStatus("done")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `documents_total{status="done"} 1`) {
		t.Errorf("scrape missing documents_total{status=\"done\"} 1:\n%s", rec.Body.String())
	}
}

func TestLogIsJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := LogTo(&buf)
	logger.Info("hello", "key", "value")
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	if parsed["msg"] != "hello" || parsed["key"] != "value" {
		t.Errorf("log record = %v", parsed)
	}
}

func TestRequestLoggerRecordsStatusAndPath(t *testing.T) {
	var buf bytes.Buffer
	handler := RequestLogger(LogTo(&buf))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/documents?limit=5", nil))
	out := buf.String()
	if !strings.Contains(out, `"status":204`) || !strings.Contains(out, `"path":"/v1/documents"`) {
		t.Errorf("request log = %s, want status 204 and the path", out)
	}
	if strings.Contains(out, "limit=5") {
		t.Errorf("request log leaks the query string: %s", out)
	}
}
