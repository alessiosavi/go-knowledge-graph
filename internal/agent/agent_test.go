package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestSystemPromptSchema(t *testing.T) {
	for _, want := range []string{
		"Document{id, source, title, status, labels, domain, document_date, version_at, created_at}",
		"Chunk{id, doc_id, seq, text, domain}",
		"Entity{id, key, name, type, mention_count, domain}",
		"(:Document)-[:HAS_CHUNK]->(:Chunk)",
		"(:Chunk)-[:MENTIONS]->(:Entity)",
		"typed fact edges connect entities",
		"document_date is set only when the caller gave one; in Cypher use coalesce(document_date, version_at, created_at)",
		"Chunk search hits carry document_id, document_date and document_date_source; entity hits carry none.",
		"document_date_source 'caller' is the source's own date; 'ingest' is only when gkg received it and does not show how recent the content is. For a current-state question with conflicting comparable evidence, prefer the newest caller-dated source, cite its date, and mention any undated conflicting source.",
		"When the question names a period, pass since/until to search, get_entity, traverse or list_documents.",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	_, strategy, ok := strings.Cut(systemPrompt, "Strategy: ")
	if !ok {
		t.Fatal("system prompt missing strategy sentence")
	}
	strategy, _, _ = strings.Cut(strategy, "\n")
	search := strings.Index(strategy, "search")
	chunks := strings.Index(strategy, "get_chunks")
	cypher := strings.Index(strategy, "cypher")
	if search < 0 || chunks < 0 || cypher < 0 || search >= chunks || chunks >= cypher {
		t.Errorf("strategy = %q, want search before get_chunks before cypher", strategy)
	}
	// Every tool the strategy names must be one the model can call.
	defined := map[string]bool{}
	for _, d := range toolDefs() {
		defined[d.Function.Name] = true
	}
	for _, name := range []string{"search", "get_chunks", "cypher"} {
		if !defined[name] {
			t.Errorf("strategy names %q, which toolDefs does not define", name)
		}
	}
}

func TestSystemPromptPropertiesExist(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Ingest.SpoolDir = t.TempDir()
	p := ingest.NewPipeline(cfg, st, &extract.Mock{}, nil, func(_ context.Context, in ingest.Input) (ingest.Loaded, error) {
		return ingest.Loaded{Text: in.Text, Title: in.Title}, nil
	})
	// Unique entity names: entities merge globally by key, so a shared name
	// would reuse a node that other writers may have given more properties.
	tag := time.Now().UnixNano()
	res, err := p.Submit(ctx, ingest.Input{
		Kind: "text", Source: fmt.Sprintf("text:prompt-schema-%d", tag),
		Title: "Zorbo guide", Text: fmt.Sprintf("Zorbo%d uses Acme%d", tag, tag),
		DocumentDate: "2024-03-01T00:00:00.000000000Z",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), res.DocumentID) })
	// Domain assignment normally precedes processing under the v4 write gate.
	drv, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(ctx)
	session := drv.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)
	rows, err := session.Run(ctx, `MATCH (d:Document {id:$id}) SET d.domain='zorbo',d.domain_tok=$token`, map[string]any{"id": res.DocumentID, "token": domain.DomainToken("zorbo")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rows.Consume(ctx); err != nil {
		t.Fatal(err)
	}
	p.SetWriteMode(ingest.WriteMode{Domains: true})
	if err := p.Process(ctx, res.DocumentID); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// Parse the advertised properties so a nonexistent property added to the
	// prompt fails against real nodes from this ingestion, not unrelated data.
	declarations := regexp.MustCompile(`(?m)^(Document|Chunk|Entity)\{([^}]+)\}$`).FindAllStringSubmatch(systemPrompt, -1)
	if len(declarations) != 3 {
		t.Fatalf("prompt schema declarations = %d, want 3", len(declarations))
	}
	queries := map[string]string{
		"Document": "MATCH (n:Document {id: $doc}) RETURN keys(n) AS properties",
		"Chunk":    "MATCH (:Document {id: $doc})-[:HAS_CHUNK]->(n:Chunk) RETURN keys(n) AS properties",
		"Entity":   "MATCH (:Document {id: $doc})-[:HAS_CHUNK]->(:Chunk)-[:MENTIONS]->(n:Entity) RETURN keys(n) AS properties",
	}
	for _, decl := range declarations {
		t.Run(decl[1], func(t *testing.T) {
			result, err := st.RunReadQuery(ctx, queries[decl[1]], map[string]any{"doc": res.DocumentID}, 0)
			if err != nil {
				t.Fatalf("keys(n): %v", err)
			}
			if len(result.Rows) == 0 {
				t.Fatalf("no %s nodes for ingested document", decl[1])
			}
			properties := make(map[string]bool)
			for _, row := range result.Rows {
				keys, ok := row["properties"].([]any)
				if !ok {
					t.Fatalf("keys(n) = %T, want []any", row["properties"])
				}
				for _, key := range keys {
					name, ok := key.(string)
					if !ok {
						t.Fatalf("property key = %T, want string", key)
					}
					properties[name] = true
				}
			}
			for _, property := range strings.Split(decl[2], ",") {
				property = strings.TrimSpace(property)
				if !properties[property] {
					t.Errorf("prompt advertises %s.%s, absent from keys(n)", decl[1], property)
				}
			}
		})
	}
}

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

func TestAskGetChunksEvidenceWithoutCypher(t *testing.T) {
	fx := newAgentFixture(t)
	id := fx.docID + "-c0"
	stub := newLLMStub(t,
		toolCallReply("read1", "get_chunks", fmt.Sprintf(`{"ids":[%q]}`, id)),
		contentReply("The full chunk says the engine is used daily."),
	)
	s := askServer(t, fx, stub.srv.URL, nil)
	resp, err := s.ask(context.Background(), "Read the full text of chunk "+id)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Steps != 1 || len(resp.Evidence) != 1 || resp.Evidence[0].Tool != "get_chunks" {
		t.Fatalf("steps=%d evidence=%+v", resp.Steps, resp.Evidence)
	}
	var result struct {
		Chunks []struct{ ID, Text string }
		Meta   struct{ Returned int }
	}
	if err := json.Unmarshal(resp.Evidence[0].Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.Meta.Returned != 1 || len(result.Chunks) != 1 || result.Chunks[0].ID != id || !strings.HasSuffix(result.Chunks[0].Text, "daily.") {
		t.Fatalf("chunk evidence = %s", resp.Evidence[0].Result)
	}
	if len(stub.requests) != 2 {
		t.Fatalf("provider requests=%d, want 2", len(stub.requests))
	}
	tools, ok := stub.requests[0]["tools"].([]any)
	if !ok {
		t.Fatalf("request has no tools array: %v", stub.requests[0]["tools"])
	}
	getChunksSeen := false
	for _, tool := range tools {
		function := tool.(map[string]any)["function"].(map[string]any)
		if function["name"] == "get_chunks" {
			getChunksSeen = true
			if !strings.Contains(fmt.Sprint(function["description"]), "instead of cypher") {
				t.Fatalf("get_chunks description lacks routing guidance: %v", function["description"])
			}
		}
	}
	if !getChunksSeen {
		t.Fatal("provider request lacks get_chunks tool")
	}
	toolSeen := false
	for _, msg := range requestMessages(t, stub.requests[1]) {
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, call := range calls {
				if call.(map[string]any)["function"].(map[string]any)["name"] == "cypher" {
					t.Fatal("cypher was called to read chunk text")
				}
			}
		}
		if msg["role"] == "tool" && msg["tool_call_id"] == "read1" && strings.Contains(fmt.Sprint(msg["content"]), id) {
			toolSeen = true
		}
	}
	if !toolSeen {
		t.Fatal("full chunk result missing from provider transcript")
	}
}

func TestAskStopsAtMaxSteps(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t,
		toolCallReply("c1", "search", fmt.Sprintf(`{"query":%q}`, fx.token)),
		toolCallReply("c2", "search", fmt.Sprintf(`{"query":%q}`, fx.token)),
		contentReply("Answer from the gathered results."),
	)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.MaxSteps = 2 })

	resp, err := s.ask(context.Background(), "loop forever?")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if resp.Steps != 2 {
		t.Errorf("Steps = %d, want the max-steps budget 2", resp.Steps)
	}
	if resp.Answer != "Answer from the gathered results." {
		t.Errorf("Answer = %q, want the final turn’s answer", resp.Answer)
	}
	if len(resp.Evidence) != 2 {
		t.Errorf("evidence = %d, want 2 tool results", len(resp.Evidence))
	}
	if len(stub.requests) != 3 {
		t.Errorf("chat calls = %d, want 3 (two tool turns and one final turn)", len(stub.requests))
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
	cases := []struct{ in, want int }{{0, 6}, {-3, 6}, {1, 1}, {6, 6}, {12, 12}, {30, 30}, {31, 30}, {99, 30}}
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
	stub.withStreamStatus(http.StatusBadRequest, `{"error":{"message":"stream rejected"}}`)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })

	resp, err := s.run(context.Background(), "fallback chain "+fx.token, noopEmit)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Answer != "Final answer citing the chunk." {
		t.Errorf("Answer = %q", resp.Answer)
	}
	if len(stub.requests) != 5 {
		t.Fatalf("requests = %v, want [stream, plain400, plan, stream-final400, plain-final]", streamFlags(stub))
	}
	if stub.requests[0]["stream"] != true || stub.requests[1]["stream"] != nil {
		t.Errorf("fallback order = %v, want the stream attempt first, then plain", streamFlags(stub))
	}
	for _, i := range []int{0, 1} {
		if _, ok := stub.requests[i]["tools"]; !ok {
			t.Errorf("request[%d] must send tools", i)
		}
	}
	if _, ok := stub.requests[2]["tools"]; ok {
		t.Errorf("plan call must not send tools: %v", stub.requests[2])
	}
	if rf, ok := stub.requests[2]["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Errorf("plan call response_format = %v, want json_object", stub.requests[2]["response_format"])
	}
	if stub.requests[3]["stream"] != true || stub.requests[4]["stream"] != nil {
		t.Errorf("final fallback order = %v, want stream-final400 then plain-final", streamFlags(stub))
	}
	for _, i := range []int{3, 4} {
		if _, ok := stub.requests[i]["tools"]; ok {
			t.Errorf("final request[%d] must not send tools", i)
		}
	}
	if unsupported, _ := s.llm.streamState(); !unsupported {
		t.Error("streamUnsupported not memoized after the successful final retry")
	}
}

func TestToolsRejectedStreamingStaysOn(t *testing.T) {
	fx := newAgentFixture(t)
	plan := fmt.Sprintf(`{"tool_calls":[{"name":"search","arguments":{"query":%q}}]}`, fx.token)
	toolsRejected := stubResponse{status: http.StatusBadRequest, body: `{"error":{"message":"tools are not supported by this model"}}`}
	stub := newLLMStub(t, toolsRejected, contentReply(plan))
	stub.withStreamStatus(http.StatusBadRequest, toolsRejected.body)
	stub.withStream(sseContent("Final "), sseContent("answer."), sseFinish("stop"), sseDone)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })

	var deltas []string
	resp, err := s.run(context.Background(), "q "+fx.token, func(ev streamEvent) error {
		if ev.Kind == "delta" {
			deltas = append(deltas, ev.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Answer != "Final answer." {
		t.Errorf("Answer = %q, want the streamed final answer", resp.Answer)
	}
	if len(stub.requests) != 4 {
		t.Fatalf("requests = %v, want [stream400, plain400, plan, stream-final]", streamFlags(stub))
	}
	if stub.requests[3]["stream"] != true {
		t.Errorf("final request stream = %v, want true", stub.requests[3]["stream"])
	}
	if _, ok := stub.requests[3]["tools"]; ok {
		t.Error("final request must not send tools")
	}
	if strings.Join(deltas, "|") != "Final |answer." {
		t.Errorf("deltas = %q, want [Final  answer.]", deltas)
	}
	if unsupported, _ := s.llm.streamState(); unsupported {
		t.Error("streamUnsupported = true, want false after both requests rejected tools")
	}
	rr := doReq(t, s.StatusHandler(), http.MethodGet, "/v1/agent/status", "", nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"stream":true`) {
		t.Errorf("status code = %d, body = %s, want stream:true", rr.Code, rr.Body.String())
	}

	stub.script = append(stub.script, toolsRejected, contentReply(plan))
	stub.withStreamStatus(http.StatusBadRequest, toolsRejected.body)
	stub.withStream(sseContent("Final "), sseContent("answer."), sseFinish("stop"), sseDone)
	resp, err = s.run(context.Background(), "second "+fx.token, noopEmit)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if resp.Answer != "Final answer." {
		t.Errorf("second Answer = %q, want the streamed final answer", resp.Answer)
	}
	if len(stub.requests) != 8 {
		t.Errorf("requests after second ask = %v, want two [stream400, plain400, plan, stream-final] sequences", streamFlags(stub))
	}
	if len(stub.requests) < 5 {
		t.Fatal("second ask sent no request")
	}
	if stub.requests[4]["stream"] != true {
		t.Errorf("second ask first request stream = %v, want true", stub.requests[4]["stream"])
	}
	if _, ok := stub.requests[4]["tools"]; !ok {
		t.Error("second ask first request must send tools")
	}
}

func TestStreamAndPlainNotFoundNoMemo(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t, stubResponse{status: http.StatusNotFound, body: `{"error":{"message":"deployment not found"}}`})
	stub.withStreamStatus(http.StatusNotFound, `{"error":{"message":"deployment not found"}}`)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })

	_, err := s.run(context.Background(), "q", noopEmit)
	var pe *providerError
	if !errors.As(err, &pe) || pe.status != http.StatusNotFound {
		t.Errorf("err = %v, want the classified 404", err)
	}
	if len(stub.requests) != 2 || stub.requests[0]["stream"] != true || stub.requests[1]["stream"] != nil {
		t.Errorf("requests = %v, want [stream404, plain404]", streamFlags(stub))
	}
	if unsupported, _ := s.llm.streamState(); unsupported {
		t.Error("streamUnsupported = true, want false after both requests returned 404")
	}
}

func TestEmptyStreamRetryContentOnlyNoMemo(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t, contentReply("plain answer"))
	stub.withStream(sseRole("assistant"), sseFinish("stop"), sseDone)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })

	resp, err := s.run(context.Background(), "first", noopEmit)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Answer != "plain answer" {
		t.Errorf("Answer = %q, want the non-streamed retry's answer", resp.Answer)
	}
	if len(stub.requests) != 2 || stub.requests[0]["stream"] != true || stub.requests[1]["stream"] != nil {
		t.Errorf("requests = %v, want [stream, plain]", streamFlags(stub))
	}
	if _, contentOnly := s.llm.streamState(); contentOnly {
		t.Error("contentOnly = true, want false after a content-only retry")
	}
	resp, err = s.run(context.Background(), "second", noopEmit)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if resp.Answer != "ok" {
		t.Errorf("second Answer = %q, want the default streamed reply", resp.Answer)
	}
	if len(stub.requests) != 3 || stub.requests[2]["stream"] != true {
		t.Errorf("requests = %v, want the second ask to stream again", streamFlags(stub))
	}
}

func TestEmptyStreamRetryErrorNoMemo(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t, stubResponse{status: http.StatusServiceUnavailable})
	stub.withStream(sseRole("assistant"), sseFinish("stop"), sseDone)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })
	s.llm.backoff = nil

	_, err := s.run(context.Background(), "first", noopEmit)
	var pe *providerError
	if !errors.As(err, &pe) || pe.status != http.StatusServiceUnavailable {
		t.Errorf("err = %v, want the classified 503", err)
	}
	if len(stub.requests) != 2 || stub.requests[0]["stream"] != true || stub.requests[1]["stream"] != nil {
		t.Errorf("requests = %v, want [stream, plain503]", streamFlags(stub))
	}
	if unsupported, contentOnly := s.llm.streamState(); unsupported || contentOnly {
		t.Errorf("streamUnsupported = %v, contentOnly = %v, want false/false after a failed retry", unsupported, contentOnly)
	}
	resp, err := s.run(context.Background(), "second", noopEmit)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if resp.Answer != "ok" {
		t.Errorf("second Answer = %q, want the default streamed reply", resp.Answer)
	}
	if len(stub.requests) != 3 || stub.requests[2]["stream"] != true {
		t.Errorf("requests = %v, want the second ask to stream again", streamFlags(stub))
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
	resp, err := s.askWithPlan(context.Background(), AskRequest{Question: "q " + fx.token}, askEvents{tool: emit, delta: emit})
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

// ask drives the bounded tool loop over the JSON endpoint (no streaming:
// emit is nil) and returns the full answer with its evidence.
func (s *Server) ask(ctx context.Context, question string) (AskResponse, error) {
	return s.run(ctx, question, nil)
}

func (s *Server) run(ctx context.Context, question string, emit func(streamEvent) error) (AskResponse, error) {
	return s.runWithEvents(ctx, AskRequest{Question: question}, askEvents{tool: emit, delta: emit})
}

// A bare product code must route to find_products on an identifier feature,
// not to get_product (seen live on 2026-11-21: the agent passed a code to
// get_product and never searched by catalogue code).
func TestSystemPromptProductCodeLookup(t *testing.T) {
	for _, want := range []string{
		"product code",
		"find_products",
		"kind code",
		"list_features",
		"get_product needs",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if !strings.HasPrefix(planSystemPrompt, systemPrompt) {
		t.Error("plan prompt must extend the system prompt")
	}
}

func TestProductEvidencePromptRouting(t *testing.T) {
	rules := []string{
		"For any question about a product: read its product record (get_product) AND what documents say about it (pages, mentions, or get_chunks with_products). Cite the record and at least one passage about that product and say whether they agree; if no passage exists, say so — unless mentions_error, mentions_partial or pages_partial shows the check was incomplete; then say the passages could not be fully checked.",
		"Documents may be Italian and product data English: search with Italian and English keywords, as two searches in the same step.",
		"Search can return product hits (kind product) with an id for get_product.",
		"Pass a product page's chunk_ids to get_chunks to read its passages.",
		"If the passages you rely on list products and the question is about a product, call get_product on that product before answering, even when search returned no product hits.",
		"Cite the chunk ids of the passages you used.",
	}
	for _, prompt := range []struct{ name, text string }{{"system", systemPrompt}, {"plan", planSystemPrompt}} {
		for _, rule := range rules {
			if count := strings.Count(prompt.text, rule); count != 1 {
				t.Errorf("%s prompt includes %q %d times, want once", prompt.name, rule, count)
			}
		}
		if !strings.Contains(prompt.text, "- "+strings.Join(rules, " ")+"\n") {
			t.Errorf("%s prompt must include the product evidence rules on one routing line", prompt.name)
		}
	}
	_, hint, _ := strings.Cut(planSystemPrompt, "For product questions,")
	for _, want := range []string{"with_mentions (boolean, default true)", "mentions_limit (1–20, default 5", "with_products (boolean, default true)"} {
		if count := strings.Count(hint, want); count != 1 {
			t.Errorf("plan prompt tool argument hint includes %q %d times, want once", want, count)
		}
	}
}

func TestPlanPromptIndependentCalls(t *testing.T) {
	const rule = "In a single plan, call only tools that need no id or chunk_ids from another call's result; if you could not read passages, say they could not be read — not that none exist."
	if count := strings.Count(planSystemPrompt, rule); count != 1 {
		t.Errorf("plan prompt includes the independent-calls rule %d times, want once", count)
	}
	if strings.Contains(systemPrompt, rule) {
		t.Error("tool-calling prompt must allow calls that depend on earlier results")
	}
}
