package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

func TestMCPReadToolPreservesClientErrorsAfterDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"argument", argerr.New("invalid limit")},
		{"query", &graph.QueryError{Err: errors.New("invalid syntax")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Limits.ReadTimeoutMS = 1
			s := NewServer(cfg, nil, nil)
			addReadTool(s, &mcpgo.Tool{Name: "client-error"}, func(ctx context.Context, _ *mcpgo.CallToolRequest, _ struct{}) (*mcpgo.CallToolResult, any, error) {
				if _, ok := ctx.Deadline(); !ok {
					return nil, nil, errors.New("read handler received no deadline")
				}
				<-ctx.Done()
				return nil, nil, tc.err
			})
			got := callTool(t, connectServer(t, s), "client-error", map[string]any{})
			if !got.IsError || len(got.Content) != 1 {
				t.Fatalf("read returned %+v, want one tool error", got)
			}
			msg, ok := got.Content[0].(*mcpgo.TextContent)
			if !ok {
				t.Fatalf("read error has type %T, want text", got.Content[0])
			}
			if msg.Text != tc.err.Error() {
				t.Errorf("read error = %q, want %q", msg.Text, tc.err)
			}
		})
	}
}

func TestMCPToolRequestMetrics(t *testing.T) {
	m := obs.New()
	s := NewServer(config.Config{}, nil, &fakeSubmitter{})
	s.SetMetrics(m)
	addReadTool(s, &mcpgo.Tool{Name: "timed-read"}, func(ctx context.Context, _ *mcpgo.CallToolRequest, _ struct{}) (*mcpgo.CallToolResult, any, error) {
		if _, ok := ctx.Deadline(); !ok {
			return nil, nil, fmt.Errorf("read handler received no deadline")
		}
		return nil, nil, fmt.Errorf("store: %w", graph.ErrQueryTimeout)
	})
	cs := connectServer(t, s)
	for _, tc := range []struct {
		name    string
		args    map[string]any
		isError bool
	}{
		{"ingest", map[string]any{"text": "fixture"}, false},
		{"ingest", map[string]any{}, true},
		{"search", map[string]any{"query": 7}, true},
		{"search", map[string]any{"query": "fixture"}, true},
	} {
		if got := callTool(t, cs, tc.name, tc.args); got.IsError != tc.isError {
			t.Errorf("%s(%v) IsError = %t, want %t", tc.name, tc.args, got.IsError, tc.isError)
		}
	}
	if _, err := cs.CallTool(context.Background(), &mcpgo.CallToolParams{Name: "unknown-fixture-tool"}); err == nil {
		t.Error("unknown tool call succeeded")
	}
	got := callTool(t, cs, "timed-read", map[string]any{})
	if !got.IsError || len(got.Content) != 1 {
		t.Fatalf("timed read returned %+v, want one tool error", got)
	}
	if msg, ok := got.Content[0].(*mcpgo.TextContent); !ok || msg.Text != "the query exceeded the configured timeout" {
		t.Errorf("timed read error = %+v, want configured timeout message", got.Content)
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	const count = "gkg_request_duration_seconds_count"
	scrape := rec.Body.String()
	for _, want := range []string{
		count + `{op="ingest",outcome="ok",surface="mcp"} 1`,
		count + `{op="ingest",outcome="client_error",surface="mcp"} 1`,
		count + `{op="search",outcome="client_error",surface="mcp"} 1`,
		count + `{op="search",outcome="error",surface="mcp"} 1`,
		count + `{op="timed-read",outcome="error",surface="mcp"} 1`,
	} {
		if !strings.Contains(scrape, want+"\n") {
			t.Errorf("scrape missing %s", want)
		}
	}
	if got := strings.Count(scrape, count+"{"); got != 5 {
		t.Errorf("request histogram series = %d, want 5", got)
	}
}
