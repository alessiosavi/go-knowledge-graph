package judge_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	for _, status := range []int{http.StatusInternalServerError, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				_, _ = w.Write([]byte("rejected judge-key"))
			})
			j.SetBackoff([]time.Duration{0, 0})
			_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
			if err == nil || !strings.Contains(err.Error(), "retries exhausted") || strings.Contains(err.Error(), "judge-key") {
				t.Errorf("VerifyFact error = %v, want sanitized retries-exhausted error", err)
			}
			if calls.Load() != 3 {
				t.Errorf("calls = %d, want 3 (initial + 2 retries)", calls.Load())
			}
		})
	}
}

func TestVerifyRedactsProviderErrors(t *testing.T) {
	const key = `judge-fixture-"key\part"<&>`
	const urlSecret = "judge-query-fixture"
	quotedKey, _ := json.Marshal(key)
	escapedKey := string(quotedKey[1 : len(quotedKey)-1])

	for _, tc := range []struct {
		name, diagnostic string
		status           int
		body             func(string) string
	}{
		{"unauthorized", "HTTP 401: denied ", http.StatusUnauthorized, func(baseURL string) string {
			return "denied " + key + " or " + escapedKey + " at " + baseURL
		}},
		{"JSON without HTML escaping", `HTTP 401: {"error":{"message":"denied [redacted]"}}`, http.StatusUnauthorized, func(_ string) string {
			return `{"error":{"message":"denied judge-fixture-\"key\\part\"<&>"}}`
		}},
		{"query escaped key", "HTTP 401: denied [redacted]", http.StatusUnauthorized, func(_ string) string {
			return "denied " + url.QueryEscape(key)
		}},
		{"path escaped key", "HTTP 401: denied [redacted]", http.StatusUnauthorized, func(_ string) string {
			return "denied " + url.PathEscape(key)
		}},
		{"preview boundary", "HTTP 401", http.StatusUnauthorized, func(_ string) string {
			return strings.Repeat("x", 188) + key
		}},
		{"error message", "provider error", http.StatusOK, func(baseURL string) string {
			message, _ := json.Marshal("denied " + key + " or " + escapedKey + " at " + baseURL)
			return `{"error":{"message":` + string(message) + `}}`
		}},
		{"invalid verdict JSON", "decode verdict", http.StatusOK, func(baseURL string) string {
			return chatBody("invalid " + key + " at " + baseURL)
		}},
		{"invalid verdict status", "invalid verdict status", http.StatusOK, func(baseURL string) string {
			status, _ := json.Marshal(key + " at " + baseURL)
			return chatBody(`{"verdict":` + string(status) + `}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var baseURL string
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body(baseURL)))
			}))
			baseURL = "http://" + srv.Listener.Addr().String() + "/v1?api-key=" + urlSecret
			srv.Start()
			t.Cleanup(srv.Close)
			cfg := config.Config{}
			cfg.Judge.BaseURL = baseURL
			cfg.Judge.APIKey = key
			j := judge.NewClient(cfg)
			j.SetBackoff([]time.Duration{0})

			_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
			if err == nil {
				t.Fatal("VerifyFact succeeded, want provider error")
			}
			msg := err.Error()
			if strings.Contains(msg, "judge-fixt") || strings.Contains(msg, urlSecret) {
				t.Errorf("provider error exposed a credential: %v", err)
			}
			if !strings.Contains(msg, tc.diagnostic) || strings.Contains(msg, "retries exhausted") {
				t.Errorf("VerifyFact error = %v, want non-retryable %q diagnostic", err, tc.diagnostic)
			}
		})
	}
}

func TestVerifyRedactsBaseURLCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, userinfo, query, body, want string
	}{
		{
			name:  "query values with no API key",
			query: "?api-key=query%40fixture&api-key=query-fixture-long&second=query-fixture&empty=",
			body:  "denied query@fixture, query-fixture-long, query-fixture",
			want:  "denied [redacted], [redacted], [redacted]",
		},
		{
			name:     "userinfo and password",
			userinfo: "judge-user:pw%40fixture@",
			body:     "denied judge-user:pw%40fixture, judge-user:pw@fixture, pw@fixture",
			want:     "denied [redacted], [redacted], [redacted]",
		},
		{
			name: "no credentials",
			body: "denied by provider",
			want: "denied by provider",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.userinfo == "" && r.Header.Get("Authorization") != "" {
					t.Error("request sent an Authorization header with no configured credentials")
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			cfg := config.Config{}
			cfg.Judge.BaseURL = "http://" + tc.userinfo + srv.Listener.Addr().String() + "/v1" + tc.query
			j := judge.NewClient(cfg)
			j.SetBackoff(nil)

			_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
			if want := "judge: HTTP 401: " + tc.want; err == nil || err.Error() != want {
				t.Errorf("VerifyFact error = %v, want %q", err, want)
			}
		})
	}
}

func TestVerifyRedactsRequestFailures(t *testing.T) {
	const urlSecret = "judge-url-fixture"
	const redirectSecret = "judge-redirect-fixture"
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://127.0.0.1:bad/?token="+redirectSecret)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)

	for _, tc := range []struct {
		name, baseURL, want string
		wantAttempts        int
	}{
		{"invalid port", "http://127.0.0.1:bad/?api-key=" + urlSecret, "judge: provider request could not be built; check the judge base URL", 1},
		{"connection refused", unreachable.URL + "/?api-key=" + urlSecret, "judge: retries exhausted: judge: provider request failed: connection refused", 3},
		{"invalid redirect", redirect.URL + "/?api-key=" + urlSecret, "judge: retries exhausted: judge: provider request failed", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Judge.BaseURL = tc.baseURL
			j := judge.NewClient(cfg)
			j.SetBackoff([]time.Duration{0, 0})
			attempts := 0
			j.SetAttemptGate(func() error {
				attempts++
				return nil
			})

			_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
			if err == nil {
				t.Fatal("VerifyFact succeeded, want request error")
			}
			msg := err.Error()
			if strings.Contains(msg, urlSecret) || strings.Contains(msg, redirectSecret) || strings.Contains(msg, "http://") {
				t.Errorf("request error exposed a URL or credential: %v", err)
			}
			if msg != tc.want {
				t.Errorf("VerifyFact error = %q, want %q", msg, tc.want)
			}
			if attempts != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", attempts, tc.wantAttempts)
			}
		})
	}
}

func TestVerifyRedactsResponseReadFailure(t *testing.T) {
	var calls atomic.Int32
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "partial response with judge-key")
	})
	j.SetBackoff([]time.Duration{0, 0})

	_, err := j.VerifyFact(t.Context(), ingest.FactInput{})
	const want = "judge: retries exhausted: judge: read response failed"
	if err == nil || err.Error() != want {
		t.Errorf("VerifyFact error = %v, want %q", err, want)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (initial + 2 retries)", calls.Load())
	}
}

func TestVerifyRedactsCanceledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{}
	cfg.Judge.BaseURL = srv.URL + "/?api-key=judge-cancel-fixture"
	j := judge.NewClient(cfg)
	j.SetBackoff(nil) // Return the in-flight request error, without entering retry backoff.

	_, err := j.VerifyFact(ctx, ingest.FactInput{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("VerifyFact error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "judge-cancel-fixture") {
		t.Errorf("cancellation error exposed a credential: %v", err)
	}
}
