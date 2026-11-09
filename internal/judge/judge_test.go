package judge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func newTestJudge(t *testing.T, handler http.HandlerFunc) *judge.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := config.Config{}
	cfg.Judge.BaseURL = srv.URL
	cfg.Judge.APIKey = "judge-key"
	cfg.Judge.Model = "deepseek-v4.1-flash"
	return judge.NewClient(cfg)
}

func chatBody(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
	})
	return string(b)
}

func TestVerifyParsesVerdicts(t *testing.T) {
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer judge-key" {
			t.Errorf("auth = %q", got)
		}
		var body struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Messages) != 2 || !strings.Contains(body.Messages[1].Content, "wrote") ||
			!strings.Contains(body.Messages[1].Content, "La città perché è bella.") {
			t.Errorf("user payload missing fact/evidence: %+v", body.Messages)
		}
		w.Write([]byte(chatBody(`{"verdict":"confirmed","confidence":0.9,"reason":"stated verbatim"}`)))
	})

	verdict, err := j.VerifyFact(context.Background(), ingest.FactInput{
		Head: "person:ada", Rel: "wrote", Tail: "work:algorithm", Confidence: 0.6,
		Evidence:   "La città perché è bella.",
		DocContext: "Sample document",
	})
	if err != nil {
		t.Fatalf("VerifyFact: %v", err)
	}
	if verdict.Status != "confirmed" || verdict.Confidence != 0.9 || verdict.Reason == "" {
		t.Errorf("verdict = %+v", verdict)
	}
}

func TestVerifyAllStatusesAndDefaultConfidence(t *testing.T) {
	for _, tc := range []struct {
		content string
		want    string
	}{
		{`{"verdict":"refuted","reason":"contradicted"}`, "refuted"},
		{`{"verdict":"unclear","confidence":0.4,"reason":"ambiguous"}`, "unclear"},
	} {
		j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(chatBody(tc.content)))
		})
		verdict, err := j.VerifyFact(context.Background(), ingest.FactInput{Head: "a", Rel: "r", Tail: "b"})
		if err != nil {
			t.Fatalf("VerifyFact(%s): %v", tc.want, err)
		}
		if verdict.Status != tc.want {
			t.Errorf("status = %q, want %q", verdict.Status, tc.want)
		}
	}
}

func TestVerifyInvalidStatusErrors(t *testing.T) {
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatBody(`{"verdict":"maybe","confidence":0.5}`)))
	})
	if _, err := j.VerifyFact(context.Background(), ingest.FactInput{Head: "a", Rel: "r", Tail: "b"}); err == nil {
		t.Fatal("invalid verdict status: want error")
	}
}

func TestVerifyIgnoresCorrectedField(t *testing.T) {
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatBody(`{"verdict":"unclear","confidence":0.5,"reason":"r","corrected":{"rel":"wrote_at"}}`)))
	})
	verdict, err := j.VerifyFact(context.Background(), ingest.FactInput{Head: "a", Rel: "r", Tail: "b"})
	if err != nil {
		t.Fatalf("VerifyFact: %v", err)
	}
	if verdict.Status != "unclear" {
		t.Errorf("verdict = %+v; corrected must be parsed but never stored in v0", verdict)
	}
}

func TestVerifyRetriesOn500ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(chatBody(`{"verdict":"confirmed","confidence":0.8}`)))
	})
	j.SetBackoff([]time.Duration{0, 0})
	if _, err := j.VerifyFact(context.Background(), ingest.FactInput{Head: "a", Rel: "r", Tail: "b"}); err != nil {
		t.Fatalf("VerifyFact after retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestVerifyErrorAfterRetries(t *testing.T) {
	var calls atomic.Int32
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	j.SetBackoff([]time.Duration{0, 0})
	if _, err := j.VerifyFact(context.Background(), ingest.FactInput{Head: "a", Rel: "r", Tail: "b"}); err == nil {
		t.Fatal("persistent 500: want error")
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (initial + 2 retries)", calls.Load())
	}
}
