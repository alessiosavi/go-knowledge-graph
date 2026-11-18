package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

func TestExportedAsk(t *testing.T) {
	stub := newLLMStub(t, contentReply("Acme owns Zorbo."))
	cfg := config.Config{}
	cfg.Agent.BaseURL = stub.srv.URL
	cfg.Agent.Model = "stub-model"
	s := NewServer(cfg, nil, nil)
	if _, err := s.Ask(context.Background(), AskRequest{Question: "Who?", Domain: "BAD"}); err == nil || !strings.Contains(err.Error(), "invalid domain") {
		t.Fatalf("domain error=%v", err)
	}
	if len(stub.requests) != 0 {
		t.Fatal("scoped ask called provider")
	}
	r, err := s.Ask(context.Background(), AskRequest{Question: "Who owns Zorbo?"})
	if err != nil || !strings.Contains(r.Answer, "Acme") {
		t.Fatalf("Ask=%+v err=%v", r, err)
	}
}

func TestAskWithProgressJSONPlan(t *testing.T) {
	stub := newLLMStub(t,
		stubResponse{status: http.StatusBadRequest, body: `{"error":"tools unsupported"}`},
		contentReply(`{"tool_calls":[{"name":"search","arguments":{"query":"Ada"}},{"name":"get_chunks","arguments":{"ids":["ada-c0"]}}]}`),
		contentReply("Ada Lovelace."),
	)
	cfg := enabledCfg(stub.srv.URL)
	cfg.Agent.Stream = true
	s := NewServer(cfg, nil, nil)
	var tools []string
	resp, err := s.AskWithProgress(t.Context(), AskRequest{Question: "Who?"}, func(ctx context.Context, step int, tool string) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("progress callback missing ask deadline")
		}
		if step != 1 {
			t.Errorf("progress step = %d, want 1", step)
		}
		tools = append(tools, tool)
	})
	if err != nil || resp.Answer != "Ada Lovelace." || resp.Steps != 1 || len(resp.Evidence) != 2 {
		t.Fatalf("AskWithProgress = %+v, %v", resp, err)
	}
	if !slices.Equal(tools, []string{"search"}) {
		t.Errorf("progress tools = %v, want [search]", tools)
	}
}

func TestAskWithProgressProviderURLFailure(t *testing.T) {
	cfg := enabledCfg("http://example.invalid:bad-port")
	s := NewServer(cfg, nil, nil)
	req := AskRequest{Question: "Who?"}
	// Both surfaces classify an unbuildable request without quoting the endpoint.
	var endpoint *url.Error
	if _, err := s.Ask(t.Context(), req); err == nil || errors.As(err, &endpoint) || err.Error() != "the agent provider is unreachable" {
		t.Fatalf("Ask error = %v, want the unreachable classification", err)
	}
	if _, err := s.AskWithProgress(t.Context(), req, nil); err == nil || err.Error() != "the agent provider is unreachable" {
		t.Fatalf("AskWithProgress error = %v, want the unreachable classification", err)
	}
}

func TestAskWithProgressLogsProviderFailure(t *testing.T) {
	const key = "fake-agent-key"
	stub := newLLMStub(t, stubResponse{
		status: http.StatusUnauthorized,
		body:   `{"error":"account disabled for Bearer ` + key + `"}`,
	})
	cfg := enabledCfg(stub.srv.URL)
	cfg.Agent.APIKey = key
	s := NewServer(cfg, nil, nil)
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	_, err := s.AskWithProgress(t.Context(), AskRequest{Question: "Who?"}, nil)
	if err == nil || err.Error() != "the agent provider rejected the request (HTTP 401)" {
		t.Fatalf("AskWithProgress error = %v, want only the provider classification", err)
	}
	if got := log.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "401") || !strings.Contains(got, "account disabled") || strings.Contains(got, key) {
		t.Error("warning must include the provider status and diagnostic with the key redacted")
	}
}
