package extract_test

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
)

func TestExtractionWireGolden(t *testing.T) {
	req := extract.Request{
		Chunks:    []extract.RequestChunk{{ID: "c1", Text: "Acme uses C++ <SDK>."}},
		Entities:  []extract.Label{{Name: "organization", Threshold: 0.4, Scope: "document"}},
		Relations: []extract.Label{{Name: "depends_on", Text: "depends on", Threshold: 0.25, Definition: "the head needs the tail to function, happen or exist."}},
	}
	for _, tc := range []struct {
		name, path, hash, response string
	}{
		{"sidecar", "/v1/extract", "8fddd3192fe93b6b26ce27241837c3d36d2b916bff26f88b21d9d84e80f8c3b0", `{"results":[]}`},
		{"openai", "/chat/completions", "b2d81a5fc28acd4dc86d32a7b214b325bcb89c30c6efcc0bcd570a154fde3c1d", `{"choices":[{"message":{"content":"{\"results\":[]}"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/healthz" && tc.name == "sidecar" {
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != tc.path {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != tc.hash {
					t.Errorf("body hash = %s, want %s (body %s)", got, tc.hash, body)
				}
				_, _ = io.WriteString(w, tc.response)
			}))
			defer srv.Close()
			cfg := config.Config{}
			cfg.Extractor.Sidecar.URL = srv.URL
			cfg.Extractor.OpenAI.BaseURL = srv.URL
			cfg.Extractor.OpenAI.Model = "test-model"
			var provider extract.Provider = extract.NewSidecar(cfg)
			if tc.name == "openai" {
				provider = extract.NewOpenAI(cfg)
			}
			if _, err := provider.Extract(t.Context(), req); err != nil {
				t.Fatal(err)
			}
		})
	}
}
