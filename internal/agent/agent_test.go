package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// scriptedCall is one tool call inside a multi-call reply.
type scriptedCall struct{ id, name, args string }

// toolCallsReply is a function-calling response with several calls.
func toolCallsReply(calls ...scriptedCall) stubResponse {
	arr := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		arr = append(arr, map[string]any{
			"id": c.id, "type": "function",
			"function": map[string]string{"name": c.name, "arguments": c.args},
		})
	}
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": "", "tool_calls": arr}}},
	})
	return stubResponse{status: 200, body: string(b)}
}

// askServer wires an enabled agent over the fixture's store, pointed at the
// stub endpoint; mutate adjusts the configuration per test.
func askServer(t *testing.T, fx agentFixture, stubURL string, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Config{}
	cfg.Agent.Enabled = true
	cfg.Agent.BaseURL = stubURL
	cfg.Agent.APIKey = "sekret-key"
	cfg.Agent.Model = "stub-model"
	if mutate != nil {
		mutate(&cfg)
	}
	return NewServer(cfg, fx.st, nil)
}

// requestMessages extracts the messages array of one recorded request.
func requestMessages(t *testing.T, req map[string]any) []map[string]any {
	t.Helper()
	raw, ok := req["messages"].([]any)
	if !ok {
		t.Fatalf("request has no messages: %v", req)
	}
	msgs := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		msg, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("message is not an object: %v", m)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func TestAskRunsToolLoopAndReturnsEvidence(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t,
		toolCallReply("c1", "search", fmt.Sprintf(`{"query":%q}`, fx.token)),
		contentReply("Ada uses the engine; see the cited chunk."),
	)
	s := askServer(t, fx, stub.srv.URL, nil)

	resp, err := s.ask(context.Background(), "What does the graph know about Ada "+fx.token+"?")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if resp.Answer != "Ada uses the engine; see the cited chunk." {
		t.Errorf("Answer = %q", resp.Answer)
	}
	if resp.Steps != 1 || len(resp.Evidence) != 1 {
		t.Fatalf("steps=%d evidence=%d, want 1/1", resp.Steps, len(resp.Evidence))
	}
	ev := resp.Evidence[0]
	if ev.Tool != "search" {
		t.Errorf("evidence tool = %q, want search", ev.Tool)
	}
	hits := unmarshalResult[searchOut](t, json.RawMessage(ev.Result))
	found := false
	for _, h := range hits.Hits {
		if h.Kind == "entity" && h.ID == fx.entityID {
			found = true
		}
	}
	if !found {
		t.Errorf("evidence hits = %+v, want the seeded entity (raw tool result)", hits.Hits)
	}

	if len(stub.requests) != 2 {
		t.Fatalf("chat calls = %d, want 2 (tool round + final)", len(stub.requests))
	}
	first := requestMessages(t, stub.requests[0])
	if first[0]["role"] != "system" || !strings.Contains(fmt.Sprint(first[0]["content"]), "untrusted") {
		t.Errorf("system message = %+v, want the untrusted-data instruction", first[0])
	}
	if first[1]["role"] != "user" || !strings.Contains(fmt.Sprint(first[1]["content"]), fx.token) {
		t.Errorf("user message = %+v, want the question", first[1])
	}
	second := requestMessages(t, stub.requests[1])
	toolSeen := false
	for _, m := range second {
		if m["role"] == "tool" && m["tool_call_id"] == "c1" && strings.Contains(fmt.Sprint(m["content"]), fx.entityID) {
			toolSeen = true
		}
	}
	if !toolSeen {
		t.Errorf("second call messages lack the tool result for c1: %+v", second)
	}
}

func TestAskStopsAtMaxSteps(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t,
		toolCallReply("c1", "search", fmt.Sprintf(`{"query":%q}`, fx.token)),
		toolCallReply("c2", "search", fmt.Sprintf(`{"query":%q}`, fx.token)),
	)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.MaxSteps = 2 })

	resp, err := s.ask(context.Background(), "loop forever?")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if resp.Steps != 2 {
		t.Errorf("Steps = %d, want the max-steps budget 2", resp.Steps)
	}
	if !strings.Contains(resp.Answer, "step budget") {
		t.Errorf("Answer = %q, want the bounded-step notice", resp.Answer)
	}
	if len(resp.Evidence) != 2 {
		t.Errorf("evidence = %d, want 2 tool results", len(resp.Evidence))
	}
	if len(stub.requests) != 2 {
		t.Errorf("chat calls = %d, want 2 (no extra call past the budget)", len(stub.requests))
	}
}

func TestAskCountsEachToolCallAgainstTheBudget(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t, toolCallsReply(
		scriptedCall{"c1", "search", fmt.Sprintf(`{"query":%q}`, fx.token)},
		scriptedCall{"c2", "search", fmt.Sprintf(`{"query":%q}`, fx.token)},
		scriptedCall{"c3", "search", fmt.Sprintf(`{"query":%q}`, fx.token)},
	))
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.MaxSteps = 2 })

	resp, err := s.ask(context.Background(), "three calls, two allowed")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if resp.Steps != 2 {
		t.Errorf("Steps = %d, want 2", resp.Steps)
	}
	if len(resp.Evidence) != 3 {
		t.Fatalf("evidence = %d, want one entry per requested call (3)", len(resp.Evidence))
	}
	if !strings.Contains(string(resp.Evidence[2].Result), "budget") {
		t.Errorf("third evidence = %s, want the budget-exhausted marker", resp.Evidence[2].Result)
	}
	if len(stub.requests) != 1 {
		t.Errorf("chat calls = %d, want 1 (the round consumed the budget)", len(stub.requests))
	}
}

func TestAskToolErrorDegrades(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t,
		toolCallReply("c1", "get_entity", `{"id":"missing-entity","include_low_confidence":true}`),
		contentReply("I could not find it; the evidence shows the tool error."),
	)
	s := askServer(t, fx, stub.srv.URL, nil)

	resp, err := s.ask(context.Background(), "Who is missing-entity?")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if resp.Answer == "" || resp.Steps != 1 || len(resp.Evidence) != 1 {
		t.Fatalf("resp = %+v, want a degraded answer with one evidence entry", resp)
	}
	if !strings.Contains(string(resp.Evidence[0].Result), "error") {
		t.Errorf("evidence = %s, want the tool error recorded", resp.Evidence[0].Result)
	}
}

func TestAskFallsBackToJSONPlan(t *testing.T) {
	fx := newAgentFixture(t)
	plan := fmt.Sprintf(`{"tool_calls":[{"name":"search","arguments":{"query":%q}}]}`, fx.token)
	stub := newLLMStub(t,
		stubResponse{status: 400, body: `{"error":{"message":"tools are not supported by this model"}}`},
		contentReply(plan),
		contentReply("Final answer citing the chunk."),
	)
	s := askServer(t, fx, stub.srv.URL, nil)

	resp, err := s.ask(context.Background(), "fall back please "+fx.token)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if resp.Answer != "Final answer citing the chunk." {
		t.Errorf("Answer = %q", resp.Answer)
	}
	if resp.Steps != 1 || len(resp.Evidence) != 1 || resp.Evidence[0].Tool != "search" {
		t.Fatalf("resp = %+v, want one executed plan call with evidence", resp)
	}
	if !strings.Contains(string(resp.Evidence[0].Result), fx.entityID) {
		t.Errorf("evidence = %s, want the seeded entity from the live graph", resp.Evidence[0].Result)
	}

	if len(stub.requests) != 3 {
		t.Fatalf("chat calls = %d, want 3 (tools attempt + plan + final)", len(stub.requests))
	}
	if _, ok := stub.requests[0]["tools"]; !ok {
		t.Errorf("first call lacks tools: %v", stub.requests[0])
	}
	if _, ok := stub.requests[1]["tools"]; ok {
		t.Errorf("plan call must not send tools: %v", stub.requests[1])
	}
	if rf, ok := stub.requests[1]["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Errorf("plan call response_format = %v, want json_object", stub.requests[1]["response_format"])
	}
	if _, ok := stub.requests[2]["tools"]; ok {
		t.Errorf("final call must not send tools: %v", stub.requests[2])
	}
}

func TestClampSteps(t *testing.T) {
	cases := []struct{ in, want int }{{0, 6}, {-3, 6}, {1, 1}, {6, 6}, {12, 12}, {99, 12}}
	for _, tc := range cases {
		if got := clampSteps(tc.in); got != tc.want {
			t.Errorf("clampSteps(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
