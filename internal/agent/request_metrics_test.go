package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

type documentListStore struct {
	graphStore
	list func(context.Context) ([]graph.Document, error)
}

func (s documentListStore) DocumentsInWindow(ctx context.Context, _ graph.Status, _ string, _ int, _ graph.DateWindow) ([]graph.Document, error) {
	return s.list(ctx)
}

func TestAgentReadToolPassesConfiguredDeadline(t *testing.T) {
	const budget = 1200 * time.Millisecond
	cfg := config.Config{}
	cfg.Limits.ReadTimeoutMS = int(budget / time.Millisecond)
	s := NewServer(cfg, nil, nil)
	s.store = documentListStore{list: func(ctx context.Context) ([]graph.Document, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("document store received no deadline")
		}
		if remaining := time.Until(deadline); remaining > budget {
			return nil, fmt.Errorf("document store received %v, want a deadline within %v", remaining, budget)
		}
		return []graph.Document{{ID: "fixture"}}, nil
	}}
	out, err := s.execTool(t.Context(), "list_documents", nil)
	if err != nil {
		t.Fatal(err)
	}
	if docs, ok := out.(documentsOut); !ok || len(docs.Documents) != 1 || docs.Documents[0].ID != "fixture" {
		t.Errorf("list_documents returned %+v, want the store's fixture document", out)
	}
}

func TestAgentReadToolPreservesClientErrorsAfterDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 0)
	defer cancel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"argument", argerr.New("invalid limit")},
		{"query", &graph.QueryError{Err: errors.New("invalid syntax")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(config.Config{}, nil, nil)
			s.store = documentListStore{list: func(context.Context) ([]graph.Document, error) { return nil, tc.err }}
			if _, err := s.execTool(ctx, "list_documents", nil); !errors.Is(err, tc.err) || errors.Is(err, graph.ErrQueryTimeout) {
				t.Errorf("list_documents returned %v, want the original client error %v", err, tc.err)
			}
		})
	}
}

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
