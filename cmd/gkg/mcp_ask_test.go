package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"
)

func callHTTPAsk(t *testing.T, cfg config.Config, opts *mcpgo.ClientOptions, token string) *mcpgo.CallToolResult {
	t.Helper()
	server := httptest.NewServer(newAPIServer(cfg, nil, nil, nil, nil).Handler())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client := mcpgo.NewClient(&mcpgo.Implementation{Name: "gkg-test"}, opts)
	session, err := client.Connect(ctx, &mcpgo.StreamableClientTransport{
		Endpoint: server.URL + "/mcp", HTTPClient: server.Client(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	params := &mcpgo.CallToolParams{
		Name: "ask", Arguments: map[string]any{"question": "Who is Ada?", "domain": "history"},
	}
	if token != "" {
		params.SetProgressToken(token)
	}
	res, err := session.CallTool(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestHTTPMCPAskRunsAgent(t *testing.T) {
	var turns atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream   bool
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode provider request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Stream || len(req.Messages) < 2 || !strings.Contains(req.Messages[0].Content, "Domain scope: history") {
			t.Error("ask should send a non-streaming provider request with the scoped prompt")
		}
		w.Header().Set("Content-Type", "application/json")
		switch turns.Add(1) {
		case 1:
			_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[
				{"id":"one","type":"function","function":{"name":"search","arguments":"{\"query\":\"Ada\"}"}},
				{"id":"two","type":"function","function":{"name":"get_entity","arguments":"{\"name\":\"Ada\"}"}}
			]}}]}`)
		case 2:
			_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[
				{"id":"three","type":"function","function":{"name":"get_chunks","arguments":"{\"ids\":[\"ada-c0\"]}"}}
			]}}]}`)
		case 3:
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"**Ada Lovelace.**"}}]}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(provider.Close)
	cfg := config.Config{}
	cfg.Agent.Enabled = true
	cfg.Agent.Stream = true
	cfg.Agent.BaseURL = provider.URL
	cfg.Limits.ReadTimeoutMS = 1
	updates := make(chan *mcpgo.ProgressNotificationParams, 4)
	res := callHTTPAsk(t, cfg, &mcpgo.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcpgo.ProgressNotificationClientRequest) {
			updates <- req.Params
		},
	}, "http-ask")
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || string(raw) != `{"answer":"**Ada Lovelace.**","steps":2}` || turns.Load() != 3 {
		t.Fatalf("ask result = %s, error = %t, provider turns = %d", raw, res.IsError, turns.Load())
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for i, tool := range []string{"search", "get_chunks"} {
		select {
		case update := <-updates:
			if update.Progress != float64(i+1) || update.Message != tool || update.ProgressToken != "http-ask" {
				t.Errorf("step %d progress = %+v, want %s", i+1, update, tool)
			}
		case <-timer.C:
			t.Fatalf("no progress received for %s", tool)
		}
	}
}

func TestHTTPMCPAskAgentErrors(t *testing.T) {
	const key = "synthetic-secret-key"
	for _, tc := range []struct {
		name, want string
	}{
		{"provider", "the agent provider rejected the request (HTTP 401)"},
		{"timeout", "the agent timed out before answering"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.name == "timeout" {
					select {
					case <-release:
					case <-r.Context().Done():
					}
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = fmt.Fprintf(w, "Request to http://%s%s rejected, credential: %s", r.Host, r.URL.Path, key)
			}))
			t.Cleanup(provider.Close)
			defer close(release)
			cfg := config.Config{}
			cfg.Agent.Enabled = true
			cfg.Agent.BaseURL = provider.URL
			cfg.Agent.APIKey = key
			cfg.Agent.TimeoutS = 1
			res := callHTTPAsk(t, cfg, nil, "")
			if !res.IsError || len(res.Content) != 1 {
				t.Fatalf("ask returned %+v, want a tool error", res)
			}
			msg, ok := res.Content[0].(*mcpgo.TextContent)
			if !ok || msg.Text != tc.want {
				t.Errorf("error = %+v, want %q", res.Content[0], tc.want)
			}
		})
	}
}
