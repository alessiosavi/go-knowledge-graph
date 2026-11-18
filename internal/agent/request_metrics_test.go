package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

func TestAgentToolRequestMetrics(t *testing.T) {
	m := obs.New()
	s := NewServer(config.Config{}, &graph.Store{}, nil)
	s.SetMetrics(m)
	ctx := context.Background()
	for _, tc := range []struct {
		name, args string
		wantError  bool
	}{
		{"search", `{"query":"!!!"}`, false},
		{"get_chunks", `{"ids":[]}`, true},
		{"search", `{"query":4}`, true},
		{"unknown-fixture-tool", `{}`, true},
	} {
		if _, err := s.execTool(ctx, tc.name, json.RawMessage(tc.args)); (err != nil) != tc.wantError {
			t.Errorf("%s(%s) error = %v, want error %t", tc.name, tc.args, err, tc.wantError)
		}
	}
	unavailable := NewServer(config.Config{}, nil, nil)
	unavailable.SetMetrics(m)
	if _, err := unavailable.execTool(ctx, "status", nil); err == nil {
		t.Error("status succeeded with no store")
	}
	expired, cancel := context.WithTimeout(ctx, 0)
	defer cancel()
	if _, err := s.execTool(expired, "list_documents", nil); !errors.Is(err, graph.ErrQueryTimeout) {
		t.Errorf("list_documents with an expired ask deadline returned %v, want a query timeout", err)
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	const count = "gkg_request_duration_seconds_count"
	scrape := rec.Body.String()
	for _, want := range []string{
		count + `{op="search",outcome="ok",surface="agent_tool"} 1`,
		count + `{op="search",outcome="client_error",surface="agent_tool"} 1`,
		count + `{op="get_chunks",outcome="client_error",surface="agent_tool"} 1`,
		count + `{op="status",outcome="error",surface="agent_tool"} 1`,
		count + `{op="list_documents",outcome="error",surface="agent_tool"} 1`,
	} {
		if !strings.Contains(scrape, want+"\n") {
			t.Errorf("scrape missing %s", want)
		}
	}
	if got := strings.Count(scrape, count+"{"); got != 5 {
		t.Errorf("request histogram series = %d, want 5", got)
	}
}
