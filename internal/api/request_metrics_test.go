package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

func TestRequestMetricsUseMountedPatterns(t *testing.T) {
	m := obs.New()
	s := NewServer(config.Config{}, nil, nil)
	s.SetMetrics(m)
	s.Mount("GET /v1/stream/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
		fmt.Fprint(w, r.PathValue("id"))
	}))
	s.Mount("GET /v1/teapot", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusTeapot, "teapot", "test client error")
	}))
	h := s.Handler()
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/v1/stream/private-id", 200},
		{"GET", "/v1/teapot", http.StatusTeapot},
		{"GET", "/v1/documents/private-id", 503},
		{"GET", "/healthz", 200},
		{"GET", "/v1/unmatched", 404},
		{"POST", "/v1/stream/private-id", 405},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status {
			t.Errorf("%s %s status = %d, want %d", tc.method, tc.path, rec.Code, tc.status)
		}
		if tc.status == 200 && strings.HasPrefix(tc.path, "/v1/") && (!rec.Flushed || rec.Body.String() != "private-id") {
			t.Errorf("stream response = %q (flushed %t)", rec.Body.String(), rec.Flushed)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	const count = "gkg_request_duration_seconds_count"
	scrape := rec.Body.String()
	for _, want := range []string{
		count + `{op="GET /v1/stream/{id}",outcome="ok",surface="rest"} 1`,
		count + `{op="GET /v1/teapot",outcome="client_error",surface="rest"} 1`,
		count + `{op="GET /v1/documents/{id}",outcome="error",surface="rest"} 1`,
	} {
		if !strings.Contains(scrape, want+"\n") {
			t.Errorf("scrape missing %s", want)
		}
	}
	if got := strings.Count(scrape, count+"{"); got != 3 {
		t.Errorf("request histogram series = %d, want 3", got)
	}
}
