package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

func TestAskCapsToolCallsPerStep(t *testing.T) {
	fx := newAgentFixture(t)
	calls := make([]scriptedCall, 0, 10)
	for i := 0; i < 10; i++ {
		calls = append(calls, scriptedCall{
			id: fmt.Sprintf("c%d", i), name: "search",
			args: fmt.Sprintf(`{"query":%q}`, fx.token),
		})
	}
	stub := newLLMStub(t, toolCallsReply(calls...))
	s := askServer(t, fx, stub.srv.URL, nil)

	resp, err := s.ask(context.Background(), "ten calls in one turn")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if len(resp.Evidence) != 10 {
		t.Fatalf("evidence = %d, want one entry per requested call (10)", len(resp.Evidence))
	}
	skipped := 0
	for _, ev := range resp.Evidence {
		if strings.Contains(string(ev.Result), "budget") {
			skipped++
		}
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2 (calls beyond the per-step cap)", skipped)
	}
	if !strings.Contains(string(resp.Evidence[0].Result), fx.entityID) {
		t.Errorf("first evidence = %s, want the executed live search", resp.Evidence[0].Result)
	}
	if resp.Steps != 1 {
		t.Errorf("Steps = %d, want 1 (one model turn)", resp.Steps)
	}
	if resp.Answer != "ok" {
		t.Errorf("Answer = %q, want the second turn's answer", resp.Answer)
	}
	if len(stub.requests) != 2 {
		t.Errorf("chat calls = %d, want 2 (the turn, then the answer)", len(stub.requests))
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

// noopEmit counts nothing; it only enables the streaming path.
func noopEmit(streamEvent) error { return nil }

func TestStreamThisTurn(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t)
	on := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })
	if !on.streamThisTurn(true) || !on.streamThisTurn(false) {
		t.Errorf("streamThisTurn on = tools:%v toolsless:%v, want true/true",
			on.streamThisTurn(true), on.streamThisTurn(false))
	}
	on.llm.memoContentOnly()
	if on.streamThisTurn(true) {
		t.Error("streamThisTurn(tools) after content-only memo = true, want false")
	}
	if !on.streamThisTurn(false) {
		t.Error("streamThisTurn(toolsless) after content-only memo = false, want true (hybrid)")
	}
	on.llm.memoStreamUnsupported()
	if on.streamThisTurn(true) || on.streamThisTurn(false) {
		t.Error("streamThisTurn after unsupported memo = true, want false")
	}

	off := askServer(t, fx, stub.srv.URL, nil)
	if off.streamThisTurn(false) {
		t.Error("streamThisTurn with cfg.Agent.Stream=false = true, want false")
	}
}

func TestStreamUnsupportedMemosAndRetriesPlain(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t, contentReply("plain answer"))
	stub.withStreamStatus(http.StatusNotFound, `{"error":{"message":"streaming unavailable"}}`)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })

	resp, err := s.run(context.Background(), "first", noopEmit)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Answer != "plain answer" {
		t.Errorf("Answer = %q, want the non-streamed retry's answer", resp.Answer)
	}
	if len(stub.requests) != 2 || stub.requests[0]["stream"] != true || stub.requests[1]["stream"] != nil {
		t.Fatalf("requests = %v, want [stream, plain]", streamFlags(stub))
	}
	if unsupported, _ := s.llm.streamState(); !unsupported {
		t.Error("streamUnsupported not memoized after a payload rejection")
	}

	resp, err = s.run(context.Background(), "second", noopEmit)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if resp.Answer != "ok" {
		t.Errorf("second Answer = %q, want the default reply", resp.Answer)
	}
	if len(stub.requests) != 3 || stub.requests[2]["stream"] != nil {
		t.Errorf("second ask streamed again: %v", streamFlags(stub))
	}
}

func TestContentOnlyHybrid(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t,
		toolCallReply("c1", "search", fmt.Sprintf(`{"query":%q}`, fx.token)),
		contentReply("final answer"),
	)
	// The streamed turn completes with neither content nor tool calls.
	stub.withStream(sseRole("assistant"), sseFinish("stop"), sseDone)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })

	var events []streamEvent
	emit := func(ev streamEvent) error {
		events = append(events, ev)
		return nil
	}
	resp, err := s.run(context.Background(), "question", emit)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Answer != "final answer" {
		t.Errorf("Answer = %q, want the post-tool answer", resp.Answer)
	}
	if len(stub.requests) != 3 || stub.requests[0]["stream"] != true ||
		stub.requests[1]["stream"] != nil || stub.requests[2]["stream"] != nil {
		t.Fatalf("requests = %v, want exactly one stream attempt then plain turns", streamFlags(stub))
	}
	if _, contentOnly := s.llm.streamState(); !contentOnly {
		t.Error("contentOnlyStreaming not memoized after the empty streamed turn")
	}
	found := false
	for _, ev := range events {
		if ev.Kind == "tool" && ev.Tool == "search" {
			found = true
		}
	}
	if !found {
		t.Errorf("events = %+v, want the search tool event", events)
	}
	if !s.streamThisTurn(false) {
		t.Error("tools-less streaming disabled after the content-only memo, want still on (hybrid)")
	}
}

func TestStreamClosedBeforeFirstEventFallsBack(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t, contentReply("plain answer"))
	stub.withStream() // 200 text/event-stream, then an immediate close
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })

	resp, err := s.run(context.Background(), "q", noopEmit)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Answer != "plain answer" {
		t.Errorf("Answer = %q, want the non-streamed retry's answer", resp.Answer)
	}
	if len(stub.requests) != 2 || stub.requests[0]["stream"] != true || stub.requests[1]["stream"] != nil {
		t.Fatalf("requests = %v, want [stream, plain]", streamFlags(stub))
	}
	if unsupported, _ := s.llm.streamState(); !unsupported {
		t.Error("streamUnsupported not memoized after the closed-before-first-event stream")
	}
}

func TestEmitErrorAbortsTheAsk(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t)
	stub.withStream(
		sseTool(0, "c1", "search", fmt.Sprintf(`{"query":%q}`, fx.token)),
		sseFinish("tool_calls"),
		sseDone,
	)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })

	sentinel := errors.New("writer gone")
	_, err := s.run(context.Background(), "q "+fx.token, func(streamEvent) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the emit error", err)
	}
	if len(stub.requests) != 1 {
		t.Errorf("requests = %d, want 1 (the ask must abort at the first tool event)", len(stub.requests))
	}
}

func TestStreamFallbackThenPlan(t *testing.T) {
	fx := newAgentFixture(t)
	plan := fmt.Sprintf(`{"tool_calls":[{"name":"search","arguments":{"query":%q}}]}`, fx.token)
	stub := newLLMStub(t,
		stubResponse{status: 400, body: `{"error":{"message":"tools are not supported by this model"}}`},
		contentReply(plan),
		contentReply("Final answer citing the chunk."),
	)
	stub.withStreamStatus(http.StatusBadRequest, `{"error":{"message":"stream rejected"}}`)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })

	resp, err := s.run(context.Background(), "fallback chain "+fx.token, noopEmit)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Answer != "Final answer citing the chunk." {
		t.Errorf("Answer = %q", resp.Answer)
	}
	if len(stub.requests) != 4 {
		t.Fatalf("requests = %v, want [stream, plain400, plan, final]", streamFlags(stub))
	}
	if stub.requests[0]["stream"] != true || stub.requests[1]["stream"] != nil {
		t.Errorf("fallback order = %v, want the stream attempt first, then plain", streamFlags(stub))
	}
	if _, ok := stub.requests[2]["tools"]; ok {
		t.Errorf("plan call must not send tools: %v", stub.requests[2])
	}
	if rf, ok := stub.requests[2]["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Errorf("plan call response_format = %v, want json_object", stub.requests[2]["response_format"])
	}
	if _, ok := stub.requests[3]["tools"]; ok {
		t.Errorf("final call must not send tools: %v", stub.requests[3])
	}
	if unsupported, _ := s.llm.streamState(); !unsupported {
		t.Error("streamUnsupported not memoized after the 400")
	}
}

func TestStreamAuthNoFallback(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t)
	stub.withStreamStatus(http.StatusUnauthorized, `{"error":{"message":"nope"}}`)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })
	s.llm.backoff = nil

	_, err := s.run(context.Background(), "q", noopEmit)
	if err == nil {
		t.Fatal("run: want an error")
	}
	var pe *providerError
	if !errors.As(err, &pe) || pe.status != http.StatusUnauthorized {
		t.Errorf("err = %v, want the classified 401", err)
	}
	if len(stub.requests) != 1 {
		t.Errorf("requests = %d, want 1 (auth failures must not retry non-streamed)", len(stub.requests))
	}
	if unsupported, _ := s.llm.streamState(); unsupported {
		t.Error("auth failure must not memoize streamUnsupported")
	}
}

func TestPlanFinalCallStreams(t *testing.T) {
	fx := newAgentFixture(t)
	plan := fmt.Sprintf(`{"tool_calls":[{"name":"search","arguments":{"query":%q}}]}`, fx.token)
	stub := newLLMStub(t, contentReply(plan))
	stub.withStream(sseContent("Final "), sseContent("answer."), sseFinish("stop"), sseDone)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })

	var deltas []string
	toolSeen := false
	emit := func(ev streamEvent) error {
		if ev.Kind == "delta" {
			deltas = append(deltas, ev.Text)
		}
		if ev.Kind == "tool" && ev.Tool == "search" {
			toolSeen = true
		}
		return nil
	}
	resp, err := s.askWithPlan(context.Background(), "q "+fx.token, emit)
	if err != nil {
		t.Fatalf("askWithPlan: %v", err)
	}
	if !toolSeen {
		t.Error("no tool event for the executed plan call")
	}
	if resp.Answer != "Final answer." {
		t.Errorf("Answer = %q, want the streamed assembly", resp.Answer)
	}
	if strings.Join(deltas, "|") != "Final |answer." {
		t.Errorf("deltas = %q, want the two streamed fragments", deltas)
	}
	if len(stub.requests) != 2 || stub.requests[1]["stream"] != true {
		t.Errorf("requests = %v, want [plan(plain), final(stream)]", streamFlags(stub))
	}
}

// streamFlags renders the stream flag of each recorded request for
// failure messages.
func streamFlags(stub *llmStub) string {
	flags := make([]string, 0, len(stub.requests))
	for _, req := range stub.requests {
		if req["stream"] == true {
			flags = append(flags, "stream")
		} else {
			flags = append(flags, "plain")
		}
	}
	return strings.Join(flags, ",")
}
