package api

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

func TestDocsServesEmbeddedAssets(t *testing.T) {
	h := NewServer(config.Config{}, nil, nil).Handler()
	for _, tc := range []struct {
		path, contentType string
		code              int
	}{
		{"/docs", "text/html", 301},
		{"/docs/", "text/html", 200},
		{"/docs/index.html", "", 301},
		{"/docs/swagger-ui.css", "text/css", 200},
		{"/docs/swagger-ui-bundle.js", "text/javascript", 200},
		{"/docs/swagger-initializer.js", "text/javascript", 200},
		{"/docs/openapi.yaml", "application/yaml", 200},
		{"/docs/nope", "text/plain", 404},
		{"/docs/LICENSE", "text/plain", 404},
		{"/docs/VERSION", "text/plain", 404},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rr := do(t, h, http.MethodGet, tc.path, nil, nil)
			if rr.Code != tc.code {
				t.Fatalf("code = %d, want %d", rr.Code, tc.code)
			}
			if tc.contentType != "" && !strings.HasPrefix(rr.Header().Get("Content-Type"), tc.contentType) {
				t.Errorf("Content-Type = %q, want %s", rr.Header().Get("Content-Type"), tc.contentType)
			}
			const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
			if got := rr.Header().Get("Content-Security-Policy"); got != csp {
				t.Errorf("CSP = %q, want %q", got, csp)
			}
			if tc.path == "/docs" && rr.Header().Get("Location") != "/docs/" {
				t.Errorf("Location = %q", rr.Header().Get("Location"))
			}
			if tc.path == "/docs/" && !strings.Contains(rr.Body.String(), `id="swagger-ui"`) {
				t.Error("missing Swagger UI root")
			}
		})
	}
	rr := do(t, h, http.MethodGet, "/docs/openapi.yaml", nil, nil)
	spec, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rr.Body.Bytes(), spec) {
		t.Error("served spec differs from docs/openapi.yaml")
	}
	rr = do(t, h, http.MethodHead, "/docs/openapi.yaml", nil, nil)
	if rr.Code != 200 || rr.Body.Len() != 0 {
		t.Errorf("HEAD spec: code=%d bytes=%d", rr.Code, rr.Body.Len())
	}
}

func TestDocsBearerPolicy(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.APIKey = "sekret"
	s := NewServer(cfg, nil, nil)
	h := s.Handler()
	for _, path := range []string{"/docs", "/docs/", "/docs/index.html", "/docs/swagger-ui.css", "/docs/swagger-ui-bundle.js", "/docs/swagger-initializer.js", "/docs/openapi.yaml"} {
		if !authExempt(path) {
			t.Errorf("%s must be bearer exempt", path)
		}
		rr := do(t, h, http.MethodGet, path, nil, nil)
		if rr.Code != 200 && rr.Code != 301 {
			t.Errorf("%s without bearer: %d", path, rr.Code)
		}
	}
	for _, path := range []string{"/docs/nope", "/docs/openapi.yaml/extra", "/docs/swagger-ui.css/extra", "/docs/LICENSE", "/docs/VERSION", "/docs2", "/v1/documents", "/v1/agent/status", "/v1/agent/ask", "/mcp"} {
		if authExempt(path) {
			t.Errorf("%s must not be bearer exempt", path)
		}
		rr := do(t, h, http.MethodGet, path, nil, nil)
		if rr.Code != 401 {
			t.Errorf("%s without bearer: %d, want 401", path, rr.Code)
		}
		if strings.HasPrefix(path, "/docs/") && rr.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s unauthorized response missing CSP", path)
		}
	}
	// Every registered REST data route retains the bearer requirement.
	for _, pattern := range s.RoutePatterns() {
		method, path, _ := strings.Cut(pattern, " ")
		if !strings.HasPrefix(path, "/v1/") {
			continue
		}
		path = strings.ReplaceAll(path, "{id}", "fixture-id")
		rr := do(t, h, method, path, nil, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without bearer: %d, want 401", method, path, rr.Code)
		}
	}
	rr := do(t, h, http.MethodGet, "/docs/nope", nil, map[string]string{"Authorization": "Bearer sekret"})
	if rr.Code != 404 {
		t.Errorf("unknown docs path with bearer: %d, want 404", rr.Code)
	}
}
