package judge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/llmauth"
)

func TestStaticJudgeRequestWire(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		const want = `{"model":"test","messages":[{"role":"system","content":"system"},{"role":"user","content":"{\"fact\":\"sample\"}"}],"response_format":{"type":"json_object"},"temperature":0}`
		if string(body) != want || r.Header.Get("Authorization") != "Bearer fake-static-key" {
			t.Errorf("request = %s; want %s with static authorization", body, want)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"verdict\":\"confirmed\",\"reason\":\"evidence\"}"}}]}`)
	}))
	defer upstream.Close()
	client := NewClient(config.Config{Judge: config.JudgeConfig{BaseURL: upstream.URL, APIKey: "fake-static-key", Model: "test"}})
	verdict, err := client.verify(t.Context(), "system", []byte(`{"fact":"sample"}`), client.settings)
	if err != nil || verdict.Reason != "evidence" {
		t.Fatalf("verdict=%+v err=%v", verdict, err)
	}
}

type judgeTokens func(context.Context) (string, time.Time, error)

func (f judgeTokens) Token(ctx context.Context) (string, time.Time, error) { return f(ctx) }

func fakeJudgeToken(token string) judgeTokens {
	return func(context.Context) (string, time.Time, error) { return token, time.Now().Add(time.Hour), nil }
}

func TestWithTokenProviderOverridesDefault(t *testing.T) {
	for _, auth := range []string{config.AuthStatic, config.AuthGCP} {
		t.Run(auth, func(t *testing.T) {
			cfg := config.Config{Judge: config.JudgeConfig{Auth: auth}}
			client := NewClientWithOptions(cfg, WithTokenProvider(fakeJudgeToken("replacement")), WithTokenProvider(nil))
			provider, ok := client.tokens.(judgeTokens)
			if !ok {
				t.Fatalf("token provider = %T, want injected provider", client.tokens)
			}
			if got, _, err := provider.Token(t.Context()); err != nil || got != "replacement" {
				t.Fatalf("Token = %q, %v", got, err)
			}
		})
	}
}

func TestCandidateBackfillStopsOnPermanentAuthFailure(t *testing.T) {
	st := graphtest.Store(t)
	const tag = "candidate-auth-permanent"
	doc, err := st.UpsertDocument(t.Context(), graph.DocumentInput{Kind: "text", Source: tag, Hash: tag, Domain: tag})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), doc.ID) })
	const evidence = "Acme uses Zorbo."
	if err := st.PutChunks(t.Context(), doc.ID, []graph.Chunk{{Text: evidence, End: len(evidence)}}); err != nil {
		t.Fatal(err)
	}
	ents := []graph.EntityUpsert{{Key: tag + "-1", Name: "Acme"}, {Key: tag + "-2", Name: "Acme"}, {Key: tag + "-3", Name: "Zorbo"}}
	payload := ingest.CanonicalJudgeInput(ingest.FactInput{Head: "Acme", Rel: "uses", Tail: "Zorbo", Evidence: evidence, Confidence: 0.8})
	facts := []graph.FactUpsert{
		{HeadKey: ents[0].Key, Rel: "uses", TailKey: ents[2].Key, JudgeInput: payload},
		{HeadKey: ents[1].Key, Rel: "uses", TailKey: ents[2].Key, JudgeInput: payload},
	}
	if err := st.UpsertExtractionOpts(t.Context(), doc.ID, doc.ID+"-c0", "mock:1", ents, facts, graph.WriteOptions{FactsAsCandidates: true, Domain: tag}); err != nil {
		t.Fatal(err)
	}
	client := NewClient(config.Config{Judge: config.JudgeConfig{BaseURL: "http://127.0.0.1:17749"}})
	client.client.Transport = rejudgeTestTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("provider was contacted after permanent auth failure")
		return nil, nil
	})
	failure := &llmauth.AuthPermanent{Detail: "fake metadata rejection"}
	fetches := 0
	client.tokens = judgeTokens(func(context.Context) (string, time.Time, error) {
		fetches++
		return "", time.Time{}, failure
	})
	cache := NewCached(client, st, client.Model())
	got, err := Backfill(t.Context(), st, cache, client, BackfillOptions{Domain: tag})
	if !IsProviderStop(err) || !errors.Is(err, failure) || fetches != 1 || got.Attempts != 0 || got.Failed != 0 || got.Judged != 0 {
		t.Fatalf("result=%+v err=%v token fetches=%d", got, err, fetches)
	}
	remaining, err := st.UnjudgedCandidates(t.Context(), tag, 0)
	if err != nil || len(remaining) != 2 {
		t.Fatalf("remaining=%+v err=%v", remaining, err)
	}
}

func TestGCPJudgeRetriesWithFreshTokenAndSettings(t *testing.T) {
	var attempts, fetches atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if got := r.Header.Get("Authorization"); got != fmt.Sprintf("Bearer fake-token-%d", n) {
			t.Errorf("attempt %d authorization = %q", n, got)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil || !strings.HasSuffix(string(data), `"response_format":{"type":"json_object"},"temperature":0.5,"max_tokens":512,"reasoning_effort":"low"}`) {
			t.Errorf("settings wire = %s, err=%v", data, err)
		}
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeFakeVerdict(t, w, "ok")
	}))
	defer upstream.Close()
	c := NewClient(config.Config{Judge: config.JudgeConfig{
		Auth: config.AuthGCP, BaseURL: upstream.URL,
		Temperature: new(0.5), MaxTokens: new(512), ReasoningEffort: new("low"),
	}})
	c.tokens = judgeTokens(func(context.Context) (string, time.Time, error) {
		return fmt.Sprintf("fake-token-%d", fetches.Add(1)), time.Now().Add(time.Hour), nil
	})
	c.SetBackoff([]time.Duration{0})
	got, err := c.VerifyFact(t.Context(), ingest.FactInput{})
	if err != nil || got.Status != "confirmed" || attempts.Load() != 2 || fetches.Load() != 2 {
		t.Fatalf("verdict=%+v err=%v attempts=%d token fetches=%d", got, err, attempts.Load(), fetches.Load())
	}
}

func TestGCPJudgeClassifiesAuthAndRejectsRedirect(t *testing.T) {
	const token = "fake-token+/="
	const safePreview = "permission denied: [redacted] and [redacted] and [redacted]"
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/chat/completions" {
					t.Error("followed redirect")
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
				fmt.Fprint(w, "permission denied: Authorization: "+r.Header.Get("Authorization")+
					` and "Authorization":"Bearer fake-token+\u002f=" and fake-token+\/=`+strings.Repeat("x", 220))
			}))
			defer upstream.Close()
			c := NewClient(config.Config{Judge: config.JudgeConfig{Auth: config.AuthGCP, BaseURL: upstream.URL}})
			c.tokens = fakeJudgeToken(token)
			c.SetBackoff([]time.Duration{0})
			_, err := c.VerifyFact(t.Context(), ingest.FactInput{})
			if err == nil || !IsProviderStop(err) || calls.Load() != 1 || strings.Contains(err.Error(), "fake-token") {
				t.Fatalf("err=%v calls=%d", err, calls.Load())
			}
			want := llmauth.PermanentClass
			if status == http.StatusTemporaryRedirect {
				want = ""
			}
			if llmauth.Class(err) != want {
				t.Errorf("class = %q, want %q", llmauth.Class(err), want)
			}
			if status != http.StatusTemporaryRedirect {
				auth, ok := errors.AsType[*llmauth.AuthPermanent](err)
				if !ok {
					t.Fatalf("expected permanent auth error, got %v", err)
				}
				_, preview, _ := strings.Cut(auth.Detail, ": ")
				if !strings.HasPrefix(preview, safePreview) || len(preview) > 200 {
					t.Errorf("detail = %q, want a redacted preview of at most 200 bytes", auth.Detail)
				}
			}
		})
	}
}

func writeFakeVerdict(t *testing.T, w http.ResponseWriter, reason string) {
	t.Helper()
	content, err := json.Marshal(verdictPayload{Verdict: "confirmed", Confidence: new(0.9), Reason: reason})
	if err != nil {
		t.Error(err)
	}
	fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
}

type authVerdictStore map[string]graph.JudgeVerdict

func (s authVerdictStore) JudgeVerdict(_ context.Context, key string) (graph.JudgeVerdict, bool, error) {
	v, ok := s[key]
	return v, ok, nil
}

func (s authVerdictStore) PutJudgeVerdict(_ context.Context, v graph.JudgeVerdict) error {
	s[v.Key] = v
	return nil
}

func TestGCPJudgeRedactsSentTokenAfterConcurrentRefresh(t *testing.T) {
	const oldToken, newToken = "old-token+/=", "new-token"
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			started, refreshed := make(chan struct{}), make(chan struct{})
			var fetches atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth := r.Header.Get("Authorization")
				switch auth {
				case "Bearer " + oldToken:
					close(started)
					select {
					case <-refreshed:
					case <-r.Context().Done():
						return
					}
					reason := "Authorization: " + auth + " and " + oldToken
					if success {
						writeFakeVerdict(t, w, reason)
					} else {
						w.WriteHeader(http.StatusBadRequest)
						fmt.Fprint(w, reason)
					}
				case "Bearer " + newToken:
					writeFakeVerdict(t, w, "refreshed")
				default:
					t.Errorf("unexpected Authorization header %q", auth)
					w.WriteHeader(http.StatusUnauthorized)
				}
			}))
			defer upstream.Close()
			defer close(refreshed)
			c := NewClient(config.Config{Judge: config.JudgeConfig{Auth: config.AuthGCP, BaseURL: upstream.URL}})
			c.tokens = judgeTokens(func(context.Context) (string, time.Time, error) {
				if fetches.Add(1) == 1 {
					return oldToken, time.Now().Add(time.Minute), nil
				}
				return newToken, time.Now().Add(time.Hour), nil
			})
			store := authVerdictStore{}
			cached := NewCached(c, store, c.Model())
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				got, err := cached.VerifyFact(ctx, ingest.FactInput{Head: "old"})
				if success && (err != nil || got.Reason != "[redacted] and [redacted]") {
					err = fmt.Errorf("verdict=%+v err=%v", got, err)
				} else if !success && (err == nil || !strings.Contains(err.Error(), "[redacted] and [redacted]") || strings.Contains(err.Error(), oldToken)) {
					err = fmt.Errorf("unsanitized preview: %v", err)
				} else if !success {
					err = nil
				}
				result <- err
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("old attempt did not reach provider")
			}
			if got, err := c.VerifyFact(ctx, ingest.FactInput{Head: "new"}); err != nil || got.Reason != "refreshed" {
				t.Fatalf("new attempt verdict=%+v, err=%v", got, err)
			}
			refreshed <- struct{}{}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if success {
				if len(store) != 1 {
					t.Fatalf("wrote %d verdicts", len(store))
				}
				for _, v := range store {
					if v.Reason != "[redacted] and [redacted]" {
						t.Errorf("persisted reason = %q", v.Reason)
					}
				}
			}
		})
	}
}

func TestV1CacheNamespacesAndRestoresLegacyKey(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeFakeVerdict(t, w, fmt.Sprintf("attempt %d", calls.Add(1)))
	}))
	defer upstream.Close()
	cfg := config.Config{Judge: config.JudgeConfig{BaseURL: upstream.URL, Model: "test"}}
	store := authVerdictStore{}
	in := ingest.FactInput{Head: "fact"}
	input := ingest.CanonicalJudgeInput(in)
	payload := ingest.JudgePayload{Bytes: input, InputHash: fmt.Sprintf("%x", sha256.Sum256(input))}
	run := func(cfg config.Config, want string) {
		t.Helper()
		client := NewClient(cfg)
		if cfg.Judge.Auth == config.AuthGCP {
			client.tokens = fakeJudgeToken("fake-token")
		}
		cache := NewCached(client, store, client.Model())
		if got, err := cache.VerifyFact(t.Context(), in); err != nil || got.Reason != want {
			t.Fatalf("VerifyFact: got=%+v err=%v want reason=%s", got, err, want)
		}
		if got, err := cache.VerifyPayload(t.Context(), payload, "doc"); err != nil || got.Reason != want {
			t.Fatalf("VerifyPayload: got=%+v err=%v want reason=%s", got, err, want)
		}
	}
	run(cfg, "attempt 1")
	if _, ok := store[RequestKey(input, cfg.Judge.Model)]; !ok {
		t.Fatal("legacy key was not written")
	}
	changed := cfg
	changed.Judge.Auth = config.AuthGCP
	run(changed, "attempt 2")
	changed = cfg
	changed.Judge.MaxTokens = new(512)
	run(changed, "attempt 3")
	changed.Judge.ReasoningEffort = new("low")
	run(changed, "attempt 4")
	changed.Judge.Temperature = new(0.5)
	run(changed, "attempt 5")
	changed.Judge.BaseURL = upstream.URL + "/other"
	run(changed, "attempt 6")
	query := changed
	query.Judge.BaseURL = changed.Judge.BaseURL + "/?ignored=yes"
	if NewClient(query).cacheNamespace != NewClient(changed).cacheNamespace {
		t.Fatal("query or trailing slash changed the route identity")
	}
	changed = cfg
	changed.Judge.Temperature = new(0.0)
	changed.Judge.BaseURL = "http://unused.invalid"
	run(changed, "attempt 1")
	run(cfg, "attempt 1")
	if calls.Load() != 6 || len(store) != 6 {
		t.Fatalf("calls=%d cache entries=%d, want 6", calls.Load(), len(store))
	}
}

func TestV3ClientAndCacheRejectGCP(t *testing.T) {
	c := NewClient(config.Config{Judge: config.JudgeConfig{Auth: config.AuthGCP}})
	cache := NewCached(c, nil, c.Model())
	if _, err := c.VerifyRequest(t.Context(), Request{}, nil); !errors.Is(err, ErrV3GCPAuth) {
		t.Fatalf("client error = %v", err)
	}
	if _, err := cache.VerifyRequest(t.Context(), Request{}, nil); !errors.Is(err, ErrV3GCPAuth) {
		t.Fatalf("cache error = %v", err)
	}
}

func TestGCPJudgeTokenFailureDoesNotDebitProviderAttempts(t *testing.T) {
	c := NewClient(config.Config{Judge: config.JudgeConfig{Auth: config.AuthGCP, BaseURL: "http://unused.invalid"}})
	var fetches, attempts int
	c.tokens = judgeTokens(func(context.Context) (string, time.Time, error) {
		fetches++
		return "", time.Time{}, &llmauth.AuthTransient{Detail: "fake metadata timeout"}
	})
	c.SetAttemptGate(func() error {
		attempts++
		return errors.New("unexpected provider attempt")
	})
	c.SetBackoff([]time.Duration{0})
	_, err := c.VerifyFact(t.Context(), ingest.FactInput{})
	if fetches != 2 || attempts != 0 || !IsProviderStop(err) || llmauth.Class(err) != llmauth.TransientClass {
		t.Fatalf("token fetches=%d provider attempts=%d error=%v", fetches, attempts, err)
	}
}

func TestV3RetainsFrozenWireSettings(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !strings.HasSuffix(string(body), `"response_format":{"type":"json_object"},"temperature":0}`) {
			t.Errorf("v3 wire settings changed: %s, err=%v", body, err)
		}
		writeFakeVerdict(t, w, "ok")
	}))
	defer upstream.Close()
	c := NewClient(config.Config{Judge: config.JudgeConfig{
		BaseURL: upstream.URL, Temperature: new(0.5), MaxTokens: new(512), ReasoningEffort: new("low"),
	}})
	req := Request{Profile: NewV2Profile(c.Model(), nil), RequestBytes: []byte("{}")}
	if _, err := c.VerifyRequest(t.Context(), req, nil); err != nil {
		t.Fatal(err)
	}
}

func TestEvalJudgeAttemptGateStopsBeforeVertexTokenFetch(t *testing.T) {
	client := NewClientWithOptions(config.Config{Judge: config.JudgeConfig{Auth: config.AuthGCP}},
		WithEvalTransport(rejudgeTestTransport(func(*http.Request) (*http.Response, error) {
			t.Error("provider called with stopped attempt gate")
			return nil, errors.New("unexpected provider request")
		})),
		WithTokenProvider(judgeTokens(func(context.Context) (string, time.Time, error) {
			t.Error("token provider called with stopped attempt gate")
			return "", time.Time{}, &llmauth.AuthPermanent{Detail: "unexpected token fetch"}
		})),
	)
	stop := errors.New("ledger stopped")
	client.SetAttemptGate(func() error { return stop })
	if _, err := client.VerifyFact(t.Context(), ingest.FactInput{}); !errors.Is(err, stop) {
		t.Fatalf("VerifyFact error = %v, want %v", err, stop)
	}
}
