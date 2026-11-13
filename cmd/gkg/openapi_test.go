package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/docs"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
	"go.yaml.in/yaml/v4"
)

// TestOpenAPIRoutes covers the production wiring (including conditional metrics
// and the unconditional agent mounts), not a second list of expected routes.
func TestOpenAPIRoutes(t *testing.T) {
	s := newAPIServer(config.Config{}, nil, nil, nil, obs.New())
	// Build the actual mux too, so conflicting/invalid registrations fail here.
	_ = s.Handler()
	if findings := openAPIRouteFindings(s.RoutePatterns(), docs.OpenAPI); len(findings) != 0 {
		t.Fatalf("OpenAPI drift:\n%s", strings.Join(findings, "\n"))
	}
}

func openAPIRouteFindings(patterns []string, data []byte) []string {
	type operation struct {
		Tags      []string             `yaml:"tags"`
		Responses map[string]yaml.Node `yaml:"responses"`
	}
	var spec struct {
		OpenAPI string                          `yaml:"openapi"`
		Paths   map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return []string{fmt.Sprintf("parse spec: %v", err)}
	}
	if spec.OpenAPI != "3.0.3" || len(spec.Paths) == 0 {
		return []string{"expected OpenAPI 3.0.3 with paths"}
	}
	registered := map[string]bool{}
	var findings []string
	for _, pattern := range patterns {
		method, path, hasMethod := strings.Cut(pattern, " ")
		if !hasMethod {
			path = pattern
		}
		switch path {
		case "/ui", "/ui/", "/docs", "/docs/", "/mcp":
			continue
		}
		if !hasMethod {
			findings = append(findings, "REST pattern needs explicit method: "+pattern)
			continue
		}
		registered[method+" "+path] = true
	}
	documented := map[string]bool{}
	for path, item := range spec.Paths {
		for method, node := range item {
			switch method {
			case "get", "put", "post", "delete", "options", "head", "patch", "trace":
			default:
				continue
			}
			key := strings.ToUpper(method) + " " + path
			documented[key] = true
			var op operation
			if err := node.Decode(&op); err != nil {
				findings = append(findings, fmt.Sprintf("%s: decode operation: %v", key, err))
				continue
			}
			if len(op.Tags) == 0 || strings.TrimSpace(op.Tags[0]) == "" {
				findings = append(findings, key+": missing tag")
			}
			if len(op.Responses) == 0 {
				findings = append(findings, key+": missing response")
			}
			if !registered[key] {
				findings = append(findings, "spec operation not registered: "+key)
			}
		}
	}
	for key := range registered {
		if !documented[key] {
			findings = append(findings, "registered route missing from spec: "+key)
		}
	}
	slices.Sort(findings)
	return findings
}

func TestOpenAPIRouteGuardRejectsDrift(t *testing.T) {
	const valid = `openapi: 3.0.3
paths:
  /v1/fixture:
    get:
      tags: [fixture]
      responses:
        '200': {description: OK}
`
	for _, tc := range []struct {
		name, spec string
		patterns   []string
		want       string
	}{
		{"matching", valid, []string{"GET /v1/fixture", "GET /ui", "GET /ui/", "GET /docs", "GET /docs/", "/mcp"}, ""},
		{"new registration", valid, []string{"GET /v1/fixture", "POST /v1/new"}, "registered route missing from spec: POST /v1/new"},
		{"stale spec", valid, nil, "spec operation not registered: GET /v1/fixture"},
		{"changed method", valid, []string{"POST /v1/fixture"}, "registered route missing from spec: POST /v1/fixture"},
		{"no tag", strings.ReplaceAll(valid, "tags: [fixture]", "tags: []"), []string{"GET /v1/fixture"}, "missing tag"},
		{"no response", strings.ReplaceAll(valid, "responses:\n        '200': {description: OK}", "responses: {}"), []string{"GET /v1/fixture"}, "missing response"},
		{"bad yaml", "paths: [", nil, "parse spec"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(openAPIRouteFindings(tc.patterns, []byte(tc.spec)), "\n")
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("findings=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestAgentMountsRemainGuardedWhenDisabled(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.APIKey = "test-key"
	s := newAPIServer(cfg, nil, nil, nil, obs.New())
	h := s.Handler()
	for _, tc := range []struct {
		method, path string
		authorized   int
	}{
		{"POST", "/v1/agent/ask", 501}, {"POST", "/v1/agent/ask/stream", 501}, {"GET", "/v1/agent/status", 200},
	} {
		t.Run(tc.path, func(t *testing.T) {
			for _, authorized := range []bool{false, true} {
				r := httptest.NewRequest(tc.method, tc.path, nil)
				if authorized {
					r.Header.Set("Authorization", "Bearer test-key")
				}
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, r)
				want := http.StatusUnauthorized
				if authorized {
					want = tc.authorized
				}
				if rr.Code != want {
					t.Errorf("authorized=%v code=%d, want %d", authorized, rr.Code, want)
				}
			}
		})
	}
}
