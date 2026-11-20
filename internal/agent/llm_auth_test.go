package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/llmauth"
)

func TestStaticChatRequestWire(t *testing.T) {
	bodies := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		bodies <- string(body)
		if r.Header.Get("Authorization") != "Bearer fake-static-key" {
			t.Errorf("unexpected authorization header")
		}
		if strings.Contains(string(body), `"stream":true`) {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"answer"}}]}`)
		}
	}))
	defer upstream.Close()
	client := newClient(config.AgentConfig{BaseURL: upstream.URL, APIKey: "fake-static-key", Model: "test"})
	req := chatRequest{Model: client.model, Messages: []chatMessage{{Role: "user", Content: "question"}}}
	if _, err := client.chat(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := client.chatStream(t.Context(), req, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{"model":"test","messages":[{"role":"user","content":"question"}],"temperature":0}`,
		`{"model":"test","messages":[{"role":"user","content":"question"}],"temperature":0,"stream":true}`,
	} {
		if body := <-bodies; body != want {
			t.Errorf("request = %s; want %s", body, want)
		}
	}
}

type agentTokens func(context.Context) (string, time.Time, error)

func (f agentTokens) Token(ctx context.Context) (string, time.Time, error) { return f(ctx) }

func TestGCPAgentSettingsAndPerAttemptToken(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var attempts, fetches atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := attempts.Add(1)
				if got := r.Header.Get("Authorization"); got != fmt.Sprintf("Bearer attempt-token-%d", n) {
					t.Errorf("attempt %d authorization = %q", n, got)
				}
				var body chatRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Stream != stream || body.Temperature != 0 || body.MaxTokens == nil || *body.MaxTokens != 2048 ||
					body.ReasoningEffort == nil || *body.ReasoningEffort != "minimal" {
					t.Errorf("unexpected request settings %+v", body)
				}
				if n == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					fmt.Fprint(w, "Authorization: Bearer attempt-token-1")
					return
				}
				if stream {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"content":"answer"}}]}`)
				}
			}))
			defer upstream.Close()
			s := NewServer(config.Config{Agent: config.AgentConfig{
				Enabled: true, Stream: stream, BaseURL: upstream.URL, Auth: config.AuthGCP,
				Temperature: new(0.7), MaxTokens: new(2048), ReasoningEfforts: []string{"minimal", "low", "high"},
			}}, nil, nil)
			if caps := s.generationCapabilities(); !slices.Equal(caps.ReasoningEfforts, []string{"minimal", "low", "high"}) {
				t.Fatalf("reasoning capabilities = %+v", caps)
			}
			if defaultReq := s.chatRequest(nil, nil); defaultReq.Temperature != 0.7 {
				t.Fatalf("configured temperature not used: %+v", defaultReq)
			}
			s.llm.backoff = []time.Duration{0}
			s.llm.tokens = agentTokens(func(context.Context) (string, time.Time, error) {
				return fmt.Sprintf("attempt-token-%d", fetches.Add(1)), time.Now().Add(time.Hour), nil
			})
			body := `{"question":"question","generation":{"temperature":0,"reasoning_effort":"minimal"}}`
			handler, want := s.AskHandler(), `"answer":"answer"`
			if stream {
				handler, want = s.StreamHandler(), "event: done"
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
			if rec.Code != http.StatusOK || attempts.Load() != 2 || fetches.Load() != 2 || !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("status=%d attempts=%d tokens=%d body=%s", rec.Code, attempts.Load(), fetches.Load(), rec.Body)
			}
		})
	}
}

func TestGCPAgentAuthFailureBeforeAndAfterSSEHeaders(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stream        bool
		firstSucceeds bool
		wantStatus    int
	}{
		{"plain obtain fails", false, false, http.StatusBadGateway},
		{"stream obtain fails", true, false, http.StatusBadGateway},
		{"stream retry obtain fails", true, true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests, calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer upstream.Close()
			s := NewServer(config.Config{Agent: config.AgentConfig{
				Enabled: true, Stream: tc.stream, BaseURL: upstream.URL, Auth: config.AuthGCP,
			}}, nil, nil)
			rec := httptest.NewRecorder()
			s.llm.tokens = agentTokens(func(context.Context) (string, time.Time, error) {
				if calls.Add(1) == 1 && tc.stream {
					if rec.Flushed || rec.Header().Get("Content-Type") != "" {
						t.Error("response committed before the first token")
					}
					if tc.firstSucceeds {
						return "first-token", time.Now().Add(time.Hour), nil
					}
				}
				return "", time.Time{}, &llmauth.AuthTransient{Detail: "fake metadata timeout"}
			})
			s.llm.backoff = nil
			handler, wantRequests := s.AskHandler(), int32(0)
			if tc.stream {
				handler = s.StreamHandler()
			}
			if tc.firstSucceeds {
				s.llm.backoff = []time.Duration{0}
				wantRequests = 1
			}
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"question":"question"}`)))
			if rec.Code != tc.wantStatus || requests.Load() != wantRequests || !strings.Contains(rec.Body.String(), `"error":"auth_transient"`) {
				t.Fatalf("status=%d provider requests=%d body=%s", rec.Code, requests.Load(), rec.Body)
			}
			if tc.firstSucceeds {
				if calls.Load() != 2 || !strings.Contains(rec.Body.String(), "event: error") {
					t.Fatalf("tokens=%d body=%s", calls.Load(), rec.Body)
				}
			} else if rec.Flushed || strings.Contains(rec.Header().Get("Content-Type"), "event-stream") {
				t.Fatalf("auth failure committed SSE headers: %v", rec.Header())
			}
		})
	}
}

func TestGCPAgentClassifiesVertexAuthAndRejectsRedirects(t *testing.T) {
	const token = "fake-secret-token+/="
	const safePreview = "permission denied: [redacted] and [redacted] and [redacted]"
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if r.URL.Path != "/chat/completions" {
					t.Error("followed redirect")
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
				fmt.Fprint(w, "permission denied: Authorization: "+r.Header.Get("Authorization")+
					` and "Authorization":"Bearer fake-secret-token+\u002f=" and fake-secret-token+\/=`+strings.Repeat("x", 220))
			}))
			defer upstream.Close()
			s := NewServer(config.Config{Agent: config.AgentConfig{
				Enabled: true, Stream: true, Auth: config.AuthGCP, BaseURL: upstream.URL,
			}}, nil, nil)
			s.llm.tokens = agentTokens(func(context.Context) (string, time.Time, error) {
				return token, time.Now().Add(time.Hour), nil
			})
			for _, tc := range []struct {
				handler http.Handler
				code    int
			}{
				{s.AskHandler(), http.StatusBadGateway},
				{s.StreamHandler(), http.StatusOK},
			} {
				rec := httptest.NewRecorder()
				tc.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"question":"question"}`)))
				body := rec.Body.String()
				if rec.Code != tc.code || strings.Contains(body, "fake-secret-token") || strings.Contains(body, "Authorization") {
					t.Errorf("unexpected response status=%d body=%s", rec.Code, body)
				}
				if tc.code == http.StatusOK {
					var ok bool
					body, ok = strings.CutPrefix(body, "event: error\ndata: ")
					if !ok {
						t.Fatalf("expected SSE error event, got %s", body)
					}
				}
				var failure errorBody
				if err := json.Unmarshal([]byte(body), &failure); err != nil {
					t.Fatal(err)
				}
				if status != http.StatusTemporaryRedirect {
					_, preview, _ := strings.Cut(failure.Detail, "): ")
					if failure.Error != llmauth.PermanentClass || !strings.HasPrefix(preview, safePreview) || len(preview) > 200 {
						t.Errorf("failure = %+v, want a permanent auth error with a redacted preview of at most 200 bytes", failure)
					}
				}
			}
			if attempts.Load() != 2 {
				t.Fatalf("expected one provider call per handler, got %d", attempts.Load())
			}
		})
	}
}

func TestGCPAgentRedactsPlainAndStreamErrors(t *testing.T) {
	const token = "test-token+/="
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		message := "Authorization: " + r.Header.Get("Authorization") + " and " + token
		if req.Stream {
			fmt.Fprintf(w, "data: {\"error\":{\"message\":%q}}\n\n", message)
		} else {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, message)
		}
	}))
	defer upstream.Close()
	c := newClient(config.AgentConfig{Auth: config.AuthGCP, BaseURL: upstream.URL})
	c.backoff = nil
	c.tokens = agentTokens(func(context.Context) (string, time.Time, error) {
		return token, time.Now().Add(time.Hour), nil
	})
	_, plain := c.chat(t.Context(), chatRequest{})
	_, stream := c.chatStream(t.Context(), chatRequest{}, nil)
	for _, err := range []error{plain, stream} {
		if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "Authorization") ||
			!strings.Contains(err.Error(), "[redacted] and [redacted]") {
			t.Errorf("redacted error = %v", err)
		}
	}
}
