package embed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

func TestEmbedOpenAIParsesVectors(t *testing.T) {
	var gotAuth, gotModel string
	var gotInput []string
	var gotDimensions *int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q, want /v1/embeddings", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Model      string   `json:"model"`
			Input      []string `json:"input"`
			Dimensions *int     `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		gotModel, gotInput, gotDimensions = req.Model, req.Input, req.Dimensions
		w.Header().Set("Content-Type", "application/json")
		// Deliberately out of input order: the provider must sort by index.
		fmt.Fprint(w, `{"data":[{"index":1,"embedding":[0.5,0.6]},{"index":0,"embedding":[0.1,0.2]}]}`)
	}))
	defer ts.Close()

	cfg := config.Config{}
	cfg.Embedder.BaseURL = ts.URL + "/v1/"
	cfg.Embedder.APIKey = "ek"
	cfg.Embedder.Model = "test-embed"
	cfg.Embedder.Dim = 2
	p := NewOpenAI(cfg)
	if p.Name() != "openai" {
		t.Errorf("Name = %q, want openai", p.Name())
	}
	vecs, err := p.Embed(context.Background(), []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotAuth != "Bearer ek" {
		t.Errorf("auth = %q, want Bearer ek", gotAuth)
	}
	if gotModel != "test-embed" {
		t.Errorf("model = %q, want test-embed", gotModel)
	}
	if gotDimensions == nil || *gotDimensions != 2 {
		t.Errorf("dimensions = %v, want the configured dim 2 requested from the provider", gotDimensions)
	}
	if len(gotInput) != 2 || gotInput[0] != "alpha" || gotInput[1] != "beta" {
		t.Errorf("input = %v, want [alpha beta]", gotInput)
	}
	if len(vecs) != 2 || len(vecs[0]) != 2 || len(vecs[1]) != 2 {
		t.Fatalf("vectors = %v, want two 2-dim vectors", vecs)
	}
	if vecs[0][0] != 0.1 || vecs[1][0] != 0.5 {
		t.Errorf("vectors = %v, want input order (sorted by index)", vecs)
	}
}

func TestEmbedOpenAIDefaultModel(t *testing.T) {
	var gotModel string
	var gotDimensions *int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model      string `json:"model"`
			Dimensions *int   `json:"dimensions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel, gotDimensions = req.Model, req.Dimensions
		fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,2]}]}`)
	}))
	defer ts.Close()

	cfg := config.Config{}
	cfg.Embedder.BaseURL = ts.URL
	p := NewOpenAI(cfg)
	if _, err := p.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotModel != defaultModel {
		t.Errorf("model = %q, want the default %q", gotModel, defaultModel)
	}
	if gotDimensions != nil {
		t.Errorf("dimensions = %v, want it omitted when no dim is configured", *gotDimensions)
	}
}

func TestEmbedOpenAIErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		dim     int
	}{
		{"http 500", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"boom"}`)
		}, 0},
		{"malformed json", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `not json`)
		}, 0},
		{"count mismatch", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,2]}]}`)
		}, 0},
		{"dimension mismatch", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[3,4]}]}`)
		}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(tc.handler)
			defer ts.Close()
			cfg := config.Config{}
			cfg.Embedder.BaseURL = ts.URL
			cfg.Embedder.Dim = tc.dim
			p := NewOpenAI(cfg)
			if _, err := p.Embed(context.Background(), []string{"a", "b"}); err == nil {
				t.Fatal("want error")
			}
		})
	}

	p := NewOpenAI(config.Config{})
	if _, err := p.Embed(context.Background(), nil); err == nil {
		t.Error("empty input: want error")
	}
}
