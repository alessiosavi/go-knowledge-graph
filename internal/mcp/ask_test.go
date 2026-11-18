package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeAsker func(context.Context, string, string, func(context.Context, int, string)) (AskResult, error)

func (f fakeAsker) Ask(ctx context.Context, question, domain string, progress func(context.Context, int, string)) (AskResult, error) {
	return f(ctx, question, domain, progress)
}

func TestAskRegistration(t *testing.T) {
	for _, tc := range []struct {
		name           string
		enabled, wired bool
	}{
		{name: "disabled"},
		{name: "disabled with asker", wired: true},
		{name: "enabled without asker", enabled: true},
		{name: "enabled with asker", enabled: true, wired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Agent.Enabled = tc.enabled
			srv := NewServer(cfg, nil, nil)
			if tc.wired {
				srv.SetAsker(fakeAsker(func(context.Context, string, string, func(context.Context, int, string)) (AskResult, error) {
					return AskResult{}, nil
				}))
			}
			list, err := connectServer(t, srv).ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			got := slices.ContainsFunc(list.Tools, func(tool *mcpgo.Tool) bool { return tool.Name == "ask" })
			if want := tc.enabled && tc.wired; got != want {
				t.Fatalf("ask registered = %t, want %t", got, want)
			}
		})
	}
}

func TestAskValidationErrorsAndMetrics(t *testing.T) {
	cfg := config.Config{}
	cfg.Agent.Enabled = true
	srv := NewServer(cfg, nil, nil)
	metrics := obs.New()
	srv.SetMetrics(metrics)
	srv.SetAsker(fakeAsker(func(ctx context.Context, question, domain string, _ func(context.Context, int, string)) (AskResult, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("MCP imposed a deadline on the asker")
		}
		switch question {
		case "timeout":
			return AskResult{}, fmt.Errorf("agent: %w", context.DeadlineExceeded)
		case "provider":
			return AskResult{}, errors.New("the agent provider rejected the request (HTTP 401)")
		case "Where?":
			if domain != "shop-1" {
				t.Errorf("domain = %q, want shop-1", domain)
			}
			return AskResult{Answer: "**Here.**", Steps: 2}, nil
		default:
			t.Errorf("unexpected invocation with question %q and domain %q", question, domain)
			return AskResult{}, nil
		}
	}))
	cs := connectServer(t, srv)
	for _, tc := range []struct {
		args  map[string]any
		error string
	}{
		{map[string]any{}, ""},
		{map[string]any{"question": 17}, ""},
		{map[string]any{"question": " \t"}, "question is required"},
		{map[string]any{"question": "Where?", "domain": "BAD"}, "invalid domain slug"},
		{map[string]any{"question": "timeout"}, "the agent timed out before answering"},
		{map[string]any{"question": "provider"}, "the agent provider rejected the request (HTTP 401)"},
	} {
		res := callTool(t, cs, "ask", tc.args)
		if !res.IsError || tc.error != "" && toolErrorText(res) != tc.error {
			t.Errorf("ask(%v) returned %+v, want tool error %q", tc.args, res, tc.error)
		}
	}
	got := structured[askOut](t, callTool(t, cs, "ask", map[string]any{"question": "  Where? ", "domain": "shop-1"}))
	if got.Answer != "**Here.**" || got.Steps != 2 {
		t.Errorf("ask result = %+v", got)
	}
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	for outcome, count := range map[string]int{"client_error": 4, "error": 2, "ok": 1} {
		want := fmt.Sprintf("gkg_request_duration_seconds_count{op=\"ask\",outcome=%q,surface=\"mcp\"} %d\n", outcome, count)
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestAskEvidence(t *testing.T) {
	value, err := json.Marshal(strings.Repeat("界<\"\n", 8000))
	if err != nil {
		t.Fatal(err)
	}
	items := []AskEvidence{
		{Tool: "search", Arguments: json.RawMessage(`{"query":"ada"}`), Result: json.RawMessage(`{"hits":[1]}`)},
		{Tool: "get_chunks", Arguments: json.RawMessage(`{"ids":["a"]}`), Result: value},
		{Tool: "get_entity", Arguments: value, Result: json.RawMessage(`{"name":"Ada"}`)},
	}
	cfg := config.Config{}
	cfg.Agent.Enabled = true
	srv := NewServer(cfg, nil, nil)
	srv.SetAsker(fakeAsker(func(_ context.Context, question, _ string, _ func(context.Context, int, string)) (AskResult, error) {
		res := AskResult{Answer: "An answer."}
		switch question {
		case "small":
			res.Evidence = items[:1]
		case "fits":
			res.Evidence = append([]AskEvidence{
				{Tool: "get_chunks", Result: json.RawMessage(`"` + strings.Repeat("a", 24<<10) + `"`)},
			}, items[0])
		case "large":
			res.Evidence = items
		case "many":
			res.Evidence = slices.Repeat(items[:1], 1000)
		}
		return res, nil
	}))
	cs := connectServer(t, srv)
	get := func(question string, include bool) map[string]json.RawMessage {
		return structured[map[string]json.RawMessage](t, callTool(t, cs, "ask", map[string]any{
			"question": question, "include_evidence": include,
		}))
	}
	got := structured[map[string]json.RawMessage](t, callTool(t, cs, "ask", map[string]any{"question": "large"}))
	if len(got) != 2 || got["answer"] == nil || got["steps"] == nil {
		t.Errorf("default response = %v, want answer and steps only", got)
	}
	if got := get("empty", true); string(got["evidence"]) != "[]" || got["truncated"] != nil {
		t.Errorf("empty evidence response = %v", got)
	}
	if got := get("small", true); got["truncated"] != nil || !strings.Contains(string(got["evidence"]), `"result":{"hits":[1]}`) {
		t.Errorf("small evidence response = %v", got)
	}
	if got := get("fits", true); got["truncated"] != nil || len(got["evidence"]) < 24<<10 {
		t.Error("evidence that fits the aggregate budget should be kept in full")
	}
	for _, question := range []string{"large", "many"} {
		got := get(question, true)
		if size := len(got["evidence"]); size <= 2 || size > 32<<10 {
			t.Fatalf("%s evidence size = %d, want a non-empty array up to 32 KiB", question, size)
		}
		if string(got["truncated"]) != "true" {
			t.Errorf("%s evidence lacks truncation flag", question)
		}
		if question == "large" {
			var rows []struct {
				Tool      string          `json:"tool"`
				Result    json.RawMessage `json:"result"`
				Truncated bool            `json:"truncated"`
			}
			if err := json.Unmarshal(got["evidence"], &rows); err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 || rows[0].Truncated || !rows[1].Truncated || !rows[2].Truncated {
				t.Fatalf("large evidence should mark only the shortened items")
			}
			var preview string
			if err := json.Unmarshal(rows[1].Result, &preview); err != nil || len(preview) < 10 {
				t.Errorf("get_chunks result preview = %d bytes, decode error = %v", len(preview), err)
			}
			if rows[2].Tool != "get_entity" || string(rows[2].Result) != `{"name":"Ada"}` {
				t.Error("truncating large arguments should preserve the small result")
			}
		}
	}
}

func TestAskEvidenceWire(t *testing.T) {
	const limit = 32 << 10
	large := AskEvidence{Tool: "search", Result: json.RawMessage(`"` + strings.Repeat("a", 40<<10) + `"`)}
	for _, tc := range []struct {
		name, answer string
		items        []AskEvidence
		oversized    bool
	}{
		{name: "answer and evidence", answer: strings.Repeat("界<\"\n", 2350), items: []AskEvidence{large}},
		{name: "fair previews", answer: strings.Repeat("a", 2000), items: slices.Repeat([]AskEvidence{large}, 3)},
		{name: "oversized answer", answer: strings.Repeat("a", limit+1), items: []AskEvidence{large}, oversized: true},
		{name: "only room for truncation flag", answer: strings.Repeat("a", limit-len(`{"answer":"","steps":3,"truncated":true}`)), items: []AskEvidence{large}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			cfg.Agent.Enabled = true
			srv := NewServer(cfg, nil, nil)
			srv.SetAsker(fakeAsker(func(context.Context, string, string, func(context.Context, int, string)) (AskResult, error) {
				return AskResult{Answer: tc.answer, Steps: 3, Evidence: tc.items}, nil
			}))
			res := callTool(t, connectServer(t, srv), "ask", map[string]any{"question": "Where?", "include_evidence": true})
			wire, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Structured json.RawMessage         `json:"structuredContent"`
				Content    []struct{ Text string } `json:"content"`
			}
			if err := json.Unmarshal(wire, &result); err != nil {
				t.Fatal(err)
			}
			// The SDK serializes the payload twice. The cap applies to one copy.
			if len(result.Content) != 1 || result.Content[0].Text != string(result.Structured) {
				t.Fatal("text content should repeat the structured payload")
			}
			if size := len(result.Structured); !tc.oversized && size > limit {
				t.Errorf("structuredContent = %d bytes, want at most %d", size, limit)
			}
			var out struct {
				Answer    string
				Steps     int
				Truncated bool
				Evidence  json.RawMessage
			}
			if err := json.Unmarshal(result.Structured, &out); err != nil {
				t.Fatal(err)
			}
			if out.Answer != tc.answer || out.Steps != 3 || !out.Truncated {
				t.Error("expected preserved answer and steps and a truncation flag")
			}
			if tc.oversized || tc.name == "only room for truncation flag" {
				if len(out.Evidence) != 0 {
					t.Fatal("expected no evidence when the answer and envelope exhaust the budget")
				}
				return
			}
			var previews []struct{ Result string }
			if err := json.Unmarshal(out.Evidence, &previews); err != nil {
				t.Fatal(err)
			}
			if len(previews) != len(tc.items) {
				t.Fatalf("got %d evidence previews, want %d", len(previews), len(tc.items))
			}
			if tc.name == "fair previews" {
				shortest, longest := limit, 0
				for _, item := range previews {
					shortest = min(shortest, len(item.Result))
					longest = max(longest, len(item.Result))
				}
				if longest-shortest > 32 {
					t.Errorf("preview lengths range from %d to %d, want roughly equal shares", shortest, longest)
				}
				arrayBudget := limit - (len(result.Structured) - len(out.Evidence))
				if size := len(out.Evidence); size < arrayBudget*95/100 {
					t.Errorf("evidence = %d bytes, want at least 95%% of %d", size, arrayBudget)
				}
				t.Logf("structuredContent = %d bytes, previews range from %d to %d bytes", len(result.Structured), shortest, longest)
			}
		})
	}
}

func TestAskProgress(t *testing.T) {
	cfg := config.Config{}
	cfg.Agent.Enabled = true
	srv := NewServer(cfg, nil, nil)
	srv.SetAsker(fakeAsker(func(ctx context.Context, question, _ string, progress func(context.Context, int, string)) (AskResult, error) {
		if (question == "with token") != (progress != nil) {
			return AskResult{}, errors.New("unexpected progress callback")
		}
		if progress != nil {
			progress(ctx, 1, "search")
			progress(ctx, 2, "get_chunks")
		}
		return AskResult{Answer: "Ada.", Steps: 2}, nil
	}))
	updates := make(chan *mcpgo.ProgressNotificationParams, 4)
	cs := connectServerWithOptions(t, srv, &mcpgo.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcpgo.ProgressNotificationClientRequest) {
			updates <- req.Params
		},
	})
	_ = structured[askOut](t, callTool(t, cs, "ask", map[string]any{"question": "without token"}))
	select {
	case got := <-updates:
		t.Fatalf("progress without a token: %+v", got)
	default:
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcpgo.CallToolParams{
		Name: "ask", Arguments: map[string]any{"question": "with token"},
		Meta: mcpgo.Meta{"progressToken": "ask-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = structured[askOut](t, res)
	for i, tool := range []string{"search", "get_chunks"} {
		select {
		case got := <-updates:
			if got.ProgressToken != "ask-token" || got.Progress != float64(i+1) || got.Message != tool {
				t.Errorf("progress = %+v, want step %d, %s", got, i+1, tool)
			}
		case <-ctx.Done():
			t.Fatalf("waiting for %s progress: %v", tool, ctx.Err())
		}
	}
}
