package extract

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
)

func newTestOpenAI(t *testing.T, handler http.HandlerFunc) *OpenAI {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := config.Config{}
	cfg.Extractor.OpenAI.BaseURL = srv.URL
	cfg.Extractor.OpenAI.APIKey = "test-key"
	cfg.Extractor.OpenAI.Model = "deepseek-v4.1-flash"
	o := NewOpenAI(cfg)
	o.backoff = []time.Duration{0, 0} // no real waiting in tests
	return o
}

func chatBody(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
	})
	return string(b)
}

func TestOpenAIPayloadOmitsScope(t *testing.T) {
	payload := buildUserPayload(
		[]RequestChunk{{ID: "c1", Text: "€500"}},
		[]Label{{Name: "amount", Text: "monetary amount", Threshold: 0.4, Scope: "document"}},
		[]Label{{Name: "amount_of", Text: "amount of", Threshold: 0.25, Scope: "document"}},
	)
	var body struct {
		Entities  []map[string]any `json:"entities"`
		Relations []map[string]any `json:"relations"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "monetary amount") || strings.Contains(payload, `"amount of"`) {
		t.Errorf("label text leaked into OpenAI payload: %s", payload)
	}
	if strings.Contains(payload, `"scope"`) || strings.Contains(payload, `"Scope"`) {
		t.Errorf("scope leaked into OpenAI payload: %s", payload)
	}
	if len(body.Entities) != 1 || body.Entities[0]["name"] != "amount" || body.Entities[0]["threshold"] != 0.4 || len(body.Entities[0]) != 2 {
		t.Errorf("entity labels = %+v, want name and threshold only", body.Entities)
	}
	if len(body.Relations) != 1 || body.Relations[0]["name"] != "amount_of" || body.Relations[0]["threshold"] != 0.25 || len(body.Relations[0]) != 2 {
		t.Errorf("relation labels = %+v, want name and threshold only", body.Relations)
	}
}

func chunkTextOf(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v, want system+user", body.Messages)
	}
	var payload struct {
		Chunks []struct {
			ID   string `json:"ID"`
			Text string `json:"Text"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal([]byte(body.Messages[1].Content), &payload); err != nil {
		t.Fatalf("decode user payload: %v", err)
	}
	out := make(map[string]string, len(payload.Chunks))
	for _, c := range payload.Chunks {
		out[c.ID] = c.Text
	}
	return out
}

func TestOpenAIParsesValidResponse(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/chat/completions"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth = %q", got)
		}
		w.Write([]byte(chatBody(`{"results":[{"chunk_id":"c1","entities":[
			{"label":"person","text":"Ada Lovelace","start":0,"end":12,"score":0.9},
			{"label":"work","text":"it.","start":19,"end":22,"score":0.8}],
			"relations":[{"label":"wrote","head":0,"tail":1,"score":0.7}]}]}`)))
	})
	if o.Version() != "openai:deepseek-v4.1-flash" {
		t.Errorf("Version = %q", o.Version())
	}
	res, err := o.Extract(context.Background(), Request{
		Chunks:    []RequestChunk{{ID: "c1", Text: "Ada Lovelace wrote it."}},
		Entities:  []Label{{Name: "person"}, {Name: "work"}},
		Relations: []Label{{Name: "wrote"}},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res) != 1 || res[0].ChunkID != "c1" {
		t.Fatalf("results = %+v", res)
	}
	if len(res[0].Mentions) != 2 || res[0].Mentions[0].Label != "person" || res[0].Mentions[0].Start != 0 || res[0].Mentions[0].End != 12 {
		t.Errorf("mentions = %+v", res[0].Mentions)
	}
	if len(res[0].Candidates) != 1 || res[0].Candidates[0].Head != 0 || res[0].Candidates[0].Tail != 1 || res[0].Candidates[0].Score != 0.7 {
		t.Errorf("candidates = %+v", res[0].Candidates)
	}
}

func TestOpenAIDropsOutOfBoundsSpans(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		// entities: 0 invalid (negative), 1 invalid (end past text), 2 & 3
		// valid; relation 4->3 references entity indexes 1 and 2: after the
		// filter, entity 1 is gone, so the relation must be dropped; 2->3
		// remaps to 0->1.
		w.Write([]byte(chatBody(`{"results":[{"chunk_id":"c1","entities":[
			{"label":"person","text":"xx","start":-1,"end":3,"score":0.9},
			{"label":"person","text":"yy","start":5,"end":900,"score":0.9},
			{"label":"person","text":"Ada","start":0,"end":3,"score":0.9},
			{"label":"work","text":"it.","start":19,"end":22,"score":0.9}],
			"relations":[{"label":"wrote","head":1,"tail":2,"score":0.7},
			             {"label":"wrote","head":2,"tail":3,"score":0.6}]}]}`)))
	})
	res, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada Lovelace wrote it."}}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res[0].Mentions) != 2 {
		t.Fatalf("mentions = %+v, want 2 valid", res[0].Mentions)
	}
	if len(res[0].Candidates) != 1 {
		t.Fatalf("candidates = %+v, want only the remappable one", res[0].Candidates)
	}
	if got := res[0].Candidates[0]; got.Head != 0 || got.Tail != 1 {
		t.Errorf("candidate indexes = %d->%d, want remapped 0->1", got.Head, got.Tail)
	}
}

func TestOpenAIRetriesOn429ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(chatBody(`{"results":[{"chunk_id":"c1","entities":[],"relations":[]}]}`)))
	})
	res, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "text"}}})
	if err != nil {
		t.Fatalf("Extract after retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("HTTP calls = %d, want 2", calls.Load())
	}
	if len(res) != 1 {
		t.Errorf("results = %+v", res)
	}
}

func TestOpenAIBatchesBy8Chunks(t *testing.T) {
	var sizes []int
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		texts := chunkTextOf(t, r)
		sizes = append(sizes, len(texts))
		w.Write([]byte(chatBody(`{"results":[]}`)))
	})
	chunks := make([]RequestChunk, 17)
	for i := range chunks {
		chunks[i] = RequestChunk{ID: string(rune('a' + i)), Text: "text"}
	}
	res, err := o.Extract(context.Background(), Request{Chunks: chunks})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(sizes) != 3 || sizes[0] != 8 || sizes[1] != 8 || sizes[2] != 1 {
		t.Errorf("batch sizes = %v, want [8 8 1]", sizes)
	}
	// The contract promises one result per submitted chunk, even when the
	// model omitted them all.
	if len(res) != 17 {
		t.Errorf("results = %d, want 17 (empty results synthesized for omissions)", len(res))
	}
	for i, r := range res {
		if r.ChunkID != chunks[i].ID {
			t.Errorf("result %d chunk = %q, want %q (submitted order)", i, r.ChunkID, chunks[i].ID)
		}
	}
}

func TestOpenAIFiltersLabelsOutsideTheRequest(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatBody(`{"results":[{"chunk_id":"c1","entities":[
			{"label":"wizard","text":"Ada","start":0,"end":3,"score":0.9},
			{"label":"person","text":"Ada","start":0,"end":3,"score":0.9}],
			"relations":[{"label":"hexes","head":1,"tail":1,"score":0.9},
			             {"label":"wrote","head":1,"tail":1,"score":0.9}]}]}`)))
	})
	res, err := o.Extract(context.Background(), Request{
		Chunks:    []RequestChunk{{ID: "c1", Text: "Ada"}},
		Entities:  []Label{{Name: "person", Threshold: 0.4}},
		Relations: []Label{{Name: "wrote", Threshold: 0.25}},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res[0].Mentions) != 1 || res[0].Mentions[0].Label != "person" {
		t.Errorf("mentions = %+v, want only the requested label", res[0].Mentions)
	}
	if len(res[0].Candidates) != 1 || res[0].Candidates[0].Label != "wrote" {
		t.Errorf("relations = %+v, want only the requested label", res[0].Candidates)
	}
}

func TestOpenAIPropagatesErrorAfterRetries(t *testing.T) {
	var calls atomic.Int32
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "text"}}}); err == nil {
		t.Fatal("Extract after persistent 500: want error")
	}
	if calls.Load() != 3 {
		t.Errorf("HTTP calls = %d, want 3 (initial + 2 retries)", calls.Load())
	}
}

func TestOpenAIStopsOnClientError(t *testing.T) {
	var calls atomic.Int32
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})
	if _, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "text"}}}); err == nil {
		t.Fatal("Extract on 400: want error")
	}
	if calls.Load() != 1 {
		t.Errorf("HTTP calls = %d, want 1 (no retry on 4xx)", calls.Load())
	}
}

func TestOpenAIUnversionedScoreDefaultsHalf(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatBody(`{"results":[{"chunk_id":"c1","entities":[
			{"label":"person","text":"Ada","start":0,"end":3}],
			"relations":[{"label":"wrote","head":0,"tail":0}]}]}`)))
	})
	res, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res[0].Mentions[0].Score != 0.5 {
		t.Errorf("mention score = %v, want 0.5 default", res[0].Mentions[0].Score)
	}
	if res[0].Candidates[0].Score != 0.5 {
		t.Errorf("relation score = %v, want 0.5 default", res[0].Candidates[0].Score)
	}
}

func TestOpenAIClampsScores(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatBody(`{"results":[{"chunk_id":"c1","entities":[
			{"label":"person","text":"Ada","start":0,"end":3,"score":1.7},
			{"label":"person","text":"Ada","start":0,"end":3,"score":-2}],
			"relations":[]}]}`)))
	})
	res, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res[0].Mentions[0].Score != 1 || res[0].Mentions[1].Score != 0 {
		t.Errorf("scores = %v, %v; want clamped 1 and 0", res[0].Mentions[0].Score, res[0].Mentions[1].Score)
	}
}

func TestOpenAIDropsEmptyAndMismatchedSpans(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		// 1: zero-length span; 2: text not at the offsets; 3: rune offsets
		// for a multi-byte chunk (Città is 6 bytes, 5 runes); 4: exact.
		w.Write([]byte(chatBody(`{"results":[{"chunk_id":"c1","entities":[
			{"label":"person","text":"Ada","start":1,"end":1,"score":0.9},
			{"label":"person","text":"Zorp","start":0,"end":3,"score":0.9},
			{"label":"place","text":"Città","start":0,"end":5,"score":0.9},
			{"label":"place","text":"Città","start":0,"end":6,"score":0.9}],
			"relations":[]}]}`)))
	})
	res, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Città è"}}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res[0].Mentions) != 1 {
		t.Fatalf("mentions = %+v, want only the byte-exact one", res[0].Mentions)
	}
	m := res[0].Mentions[0]
	if m.Text != "Città" || m.Start != 0 || m.End != 6 {
		t.Errorf("mention = %+v, want Città [0:6]", m)
	}
}

func TestOpenAINetworkErrorRetries(t *testing.T) {
	var calls atomic.Int32
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("hijacking unsupported")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			_ = conn.Close()
			return
		}
		w.Write([]byte(chatBody(`{"results":[]}`)))
	})
	if _, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "text"}}}); err != nil {
		t.Fatalf("Extract after network retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("HTTP calls = %d, want 2", calls.Load())
	}
}

func TestOpenAISurvivesCancelledBackoff(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	o.backoff = []time.Duration{10 * time.Second, 10 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := o.Extract(ctx, Request{Chunks: []RequestChunk{{ID: "c1", Text: "text"}}})
	if err == nil {
		t.Fatal("Extract with cancelled ctx: want error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Extract took %s, want prompt cancellation during backoff", elapsed)
	}
}

func TestOpenAIRejectsOversizedResponse(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		big := make([]byte, (8<<20)+1024)
		for i := range big {
			big[i] = 'x'
		}
		w.Write(big)
	})
	_, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "text"}}})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response error = %v, want an explicit cap error", err)
	}
}

func TestOpenAIErrorTextPreviewsBadContent(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatBody("not json at all")))
	})
	_, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "text"}}})
	if err == nil || !strings.Contains(err.Error(), "not json at all") {
		t.Fatalf("decode error = %v, want a content preview", err)
	}
}

func TestOpenAIEmptyChoices(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	})
	if _, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "text"}}}); err == nil {
		t.Fatal("empty choices: want error")
	}
}

func TestOpenAIRequestShape(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model          string            `json:"model"`
			Temperature    float64           `json:"temperature"`
			ResponseFormat map[string]string `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Model != "deepseek-v4.1-flash" || body.Temperature != 0 ||
			body.ResponseFormat["type"] != "json_object" {
			t.Errorf("request shape = %+v", body)
		}
		w.Write([]byte(chatBody(`{"results":[]}`)))
	})
	if _, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "text"}}}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
}

func TestOpenAIDefaultBaseURLAndNoAuthHeader(t *testing.T) {
	cfg := config.Config{}
	o := NewOpenAI(cfg)
	if o.baseURL != "https://api.openai.com/v1" {
		t.Errorf("default baseURL = %q", o.baseURL)
	}
	cfg2 := config.Config{}
	cfg2.Extractor.OpenAI.BaseURL = "http://localhost:9999/v1/"
	o2 := NewOpenAI(cfg2)
	if o2.baseURL != "http://localhost:9999/v1" {
		t.Errorf("trailing slash not trimmed: %q", o2.baseURL)
	}

	var sawAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth.Store(r.Header.Get("Authorization"))
		w.Write([]byte(chatBody(`{"results":[]}`)))
	}))
	t.Cleanup(srv.Close)
	cfg3 := config.Config{}
	cfg3.Extractor.OpenAI.BaseURL = srv.URL
	o3 := NewOpenAI(cfg3)
	o3.backoff = []time.Duration{0, 0}
	if _, err := o3.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "x"}}}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := sawAuth.Load(); got != nil && got != "" {
		t.Errorf("Authorization header sent without a key: %q", got)
	}
}

func TestOpenAIDropsMentionsWithEmptyLabelOrText(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatBody(`{"results":[{"chunk_id":"c1","entities":[
			{"label":"","text":"Ada","start":0,"end":3,"score":0.9},
			{"label":"person","text":"","start":0,"end":3,"score":0.9},
			{"label":"person","text":"Ada","start":0,"end":3,"score":0.9}],
			"relations":[]}]}`)))
	})
	res, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res[0].Mentions) != 1 || res[0].Mentions[0].Label != "person" {
		t.Errorf("mentions = %+v, want only the complete one", res[0].Mentions)
	}
}

func TestOpenAIUnknownChunkIDsIgnored(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chatBody(`{"results":[{"chunk_id":"not-submitted","entities":[{"label":"person","text":"Ada","start":0,"end":3,"score":0.9}],"relations":[]}]}`)))
	})
	res, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res) != 1 || res[0].ChunkID != "c1" || len(res[0].Mentions) != 0 {
		t.Errorf("results = %+v, want exactly one empty result for the submitted chunk", res)
	}
}

func TestOpenAIErrorBodyIsReported(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	})
	_, err := o.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "text"}}})
	if err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("error = %v, want the provider message", err)
	}
}
