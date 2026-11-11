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

func newTestSidecar(t *testing.T, handler http.HandlerFunc) *Sidecar {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := config.Config{}
	cfg.Extractor.Sidecar.URL = srv.URL
	cfg.Extractor.Sidecar.Model = "glinerel-314m"
	return NewSidecar(cfg)
}

func TestSidecarTimeout(t *testing.T) {
	for _, seconds := range []int{0, 300, 42} {
		cfg := config.Config{}
		cfg.Extractor.Sidecar.TimeoutS = seconds
		want := seconds
		if want == 0 {
			want = 300
		}
		if got := NewSidecar(cfg).client.Timeout; got != time.Duration(want)*time.Second {
			t.Errorf("timeout %d: got %v, want %ds", seconds, got, want)
		}
	}
}

func TestSidecarParsesContract(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Write([]byte(`{"status":"ok","model":"glinerel-314m"}`))
		case "/v1/extract":
			var body struct {
				Chunks []struct {
					ID   string `json:"id"`
					Text string `json:"text"`
				} `json:"chunks"`
				Entities []struct {
					Label     string  `json:"label"`
					Threshold float64 `json:"threshold"`
				} `json:"entities"`
				Relations []struct {
					Label     string  `json:"label"`
					Threshold float64 `json:"threshold"`
				} `json:"relations"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if len(body.Chunks) != 1 || body.Chunks[0].ID != "c1" {
				t.Errorf("chunks = %+v", body.Chunks)
			}
			if len(body.Entities) != 1 || body.Entities[0].Label != "person" || body.Entities[0].Threshold != 0.4 {
				t.Errorf("entities = %+v", body.Entities)
			}
			if len(body.Relations) != 1 || body.Relations[0].Label != "wrote" || body.Relations[0].Threshold != 0.25 {
				t.Errorf("relations = %+v", body.Relations)
			}
			w.Write([]byte(`{"results":[{"chunk_id":"c1","entities":[
				{"label":"person","text":"Ada","start":0,"end":3,"score":0.9}],
				"relations":[]}]}`))
		default:
			http.NotFound(w, r)
		}
	})

	if s.Version() != "sidecar:glinerel-314m" {
		t.Errorf("Version = %q", s.Version())
	}
	res, err := s.Extract(context.Background(), Request{
		Chunks:    []RequestChunk{{ID: "c1", Text: "Ada"}},
		Entities:  []Label{{Name: "person", Threshold: 0.4}},
		Relations: []Label{{Name: "wrote", Threshold: 0.25}},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res) != 1 || res[0].ChunkID != "c1" || len(res[0].Mentions) != 1 || res[0].Mentions[0].Text != "Ada" {
		t.Errorf("results = %+v", res)
	}
}

func TestSidecarHealthGating(t *testing.T) {
	var posts atomic.Int32
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		posts.Add(1)
	})
	_, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err == nil {
		t.Fatal("unhealthy sidecar: want error")
	}
	if posts.Load() != 0 {
		t.Errorf("extraction attempted against an unhealthy sidecar: %d posts", posts.Load())
	}
}

func TestSidecarConnectionRefusedIsUnhealthy(t *testing.T) {
	cfg := config.Config{}
	cfg.Extractor.Sidecar.URL = "http://127.0.0.1:1"
	cfg.Extractor.Sidecar.Model = "glinerel-314m"
	s := NewSidecar(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.Extract(ctx, Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}}); err == nil {
		t.Fatal("unreachable sidecar: want error")
	}
}

func TestSidecarSynthesizesMissingResults(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.Write([]byte(`{"results":[]}`))
	})
	res, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}, {ID: "c2", Text: "Bea"}}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res) != 2 || res[0].ChunkID != "c1" || res[1].ChunkID != "c2" {
		t.Errorf("results = %+v, want one (empty) entry per submitted chunk", res)
	}
}

func TestSidecarSendsSnakeCaseChunkKeys(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		chunks, ok := body["chunks"].([]any)
		if !ok || len(chunks) != 1 {
			t.Fatalf("chunks = %v", body["chunks"])
		}
		chunk := chunks[0].(map[string]any)
		if _, ok := chunk["id"]; !ok {
			t.Errorf("chunk keys = %v, want lowercase wire keys (id/text)", chunk)
		}
		if _, ok := chunk["text"]; !ok {
			t.Errorf("chunk keys = %v, want lowercase wire keys (id/text)", chunk)
		}
		if _, ok := chunk["ID"]; ok {
			t.Errorf("chunk keys = %v, capitalized keys break the Python contract", chunk)
		}
		w.Write([]byte(`{"results":[]}`))
	})
	if _, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
}

func TestSidecarSkipsEmptyRequestsEntirely(t *testing.T) {
	var hits atomic.Int32
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"status":"ok"}`))
	})
	res, err := s.Extract(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Extract(empty): %v", err)
	}
	if len(res) != 0 {
		t.Errorf("results = %+v, want none", res)
	}
	if hits.Load() != 0 {
		t.Errorf("HTTP hits = %d, want 0 for an empty request", hits.Load())
	}
}

func TestSidecarRejectsOversizedResponse(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		big := make([]byte, (32<<20)+1024)
		for i := range big {
			big[i] = 'x'
		}
		w.Write(big)
	})
	_, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized sidecar response error = %v, want an explicit cap error", err)
	}
}

func TestSidecarBatchesBy8Chunks(t *testing.T) {
	var sizes []int
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		var body struct {
			Chunks []struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"chunks"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		sizes = append(sizes, len(body.Chunks))
		w.Write([]byte(`{"results":[]}`))
	})
	chunks := make([]RequestChunk, 17)
	for i := range chunks {
		chunks[i] = RequestChunk{ID: string(rune('a' + i)), Text: "text"}
	}
	res, err := s.Extract(context.Background(), Request{Chunks: chunks})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(sizes) != 3 || sizes[0] != 8 || sizes[1] != 8 || sizes[2] != 1 {
		t.Errorf("batch sizes = %v, want [8 8 1]", sizes)
	}
	if len(res) != 17 {
		t.Errorf("results = %d, want 17", len(res))
	}
}

func TestSidecarHealthFailureSurfacesBody(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"detail":"model load failed: weights missing"}`))
	})
	_, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err == nil || !strings.Contains(err.Error(), "weights missing") {
		t.Fatalf("health error = %v, want the service's diagnostic body", err)
	}
}

func TestSidecarServerErrorPropagates(t *testing.T) {
	s := newTestSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"detail":"model exploded"}`))
	})
	_, err := s.Extract(context.Background(), Request{Chunks: []RequestChunk{{ID: "c1", Text: "Ada"}}})
	if err == nil {
		t.Fatal("sidecar 500: want error")
	}
}
