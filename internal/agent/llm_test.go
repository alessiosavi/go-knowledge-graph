package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// stubResponse scripts one chat-completions answer.
type stubResponse struct {
	status int
	body   string
}

// llmStub is a scripted OpenAI-compatible endpoint: every request body is
// recorded, and the next script entry answers it (a plain "ok" answer when
// the script is exhausted).
type llmStub struct {
	srv      *httptest.Server
	requests []map[string]any
	headers  []http.Header
	script   []stubResponse
}

func newLLMStub(t *testing.T, replies ...stubResponse) *llmStub {
	t.Helper()
	s := &llmStub{script: replies}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("stub: decode request: %v", err)
		}
		s.requests = append(s.requests, body)
		s.headers = append(s.headers, r.Header.Clone())
		reply := stubResponse{status: http.StatusOK, body: `{"choices":[{"message":{"content":"ok"}}]}`}
		if len(s.script) > 0 {
			reply, s.script = s.script[0], s.script[1:]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// contentReply is a plain assistant-content response.
func contentReply(content string) stubResponse {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": content}}},
	})
	return stubResponse{status: http.StatusOK, body: string(b)}
}

// toolCallReply is a function-calling response with one call.
func toolCallReply(id, name, arguments string) stubResponse {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{
			"content": "",
			"tool_calls": []map[string]any{{
				"id": id, "type": "function",
				"function": map[string]string{"name": name, "arguments": arguments},
			}},
		}}},
	})
	return stubResponse{status: http.StatusOK, body: string(b)}
}

func testClient(t *testing.T, url string) *client {
	t.Helper()
	c := newClient(config.AgentConfig{BaseURL: url, APIKey: "sekret-key", Model: "stub-model"})
	c.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	return c
}

func TestClientChatSendsRequestAndParsesReply(t *testing.T) {
	stub := newLLMStub(t, contentReply("hello graph"))
	c := testClient(t, stub.srv.URL)

	reply, err := c.chat(context.Background(), chatRequest{
		Model:      "stub-model",
		Messages:   []chatMessage{{Role: "user", Content: "hi"}},
		Tools:      []toolDef{functionTool("search", "search the graph", map[string]any{"type": "object"})},
		ToolChoice: "auto",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if reply.Content != "hello graph" {
		t.Errorf("Content = %q, want %q", reply.Content, "hello graph")
	}

	req := stub.requests[0]
	if req["model"] != "stub-model" {
		t.Errorf("request model = %v, want stub-model", req["model"])
	}
	if req["tool_choice"] != "auto" {
		t.Errorf("request tool_choice = %v, want auto", req["tool_choice"])
	}
	if _, ok := req["tools"].([]any); !ok {
		t.Errorf("request carries no tools array: %v", req["tools"])
	}
	if got := stub.headers[0].Get("Authorization"); got != "Bearer sekret-key" {
		t.Errorf("Authorization = %q, want the bearer key", got)
	}
	if got := stub.headers[0].Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestClientChatParsesToolCalls(t *testing.T) {
	stub := newLLMStub(t, toolCallReply("call-1", "search", `{"query":"ada"}`))
	c := testClient(t, stub.srv.URL)

	reply, err := c.chat(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(reply.ToolCalls))
	}
	tc := reply.ToolCalls[0]
	if tc.ID != "call-1" || tc.Function.Name != "search" || tc.Function.Arguments != `{"query":"ada"}` {
		t.Errorf("tool call = %+v, want call-1/search/{query:ada}", tc)
	}
}

func TestClientChatRetriesRetryableErrors(t *testing.T) {
	stub := newLLMStub(t,
		stubResponse{status: http.StatusInternalServerError, body: `{"error":{"message":"boom"}}`},
		stubResponse{status: http.StatusTooManyRequests, body: `{"error":{"message":"slow down"}}`},
		contentReply("recovered"),
	)
	c := testClient(t, stub.srv.URL)

	reply, err := c.chat(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if reply.Content != "recovered" {
		t.Errorf("Content = %q, want recovered", reply.Content)
	}
	if len(stub.requests) != 3 {
		t.Errorf("requests = %d, want 3 (two retries then success)", len(stub.requests))
	}
}

func TestClientChatDoesNotRetryClientErrors(t *testing.T) {
	stub := newLLMStub(t, stubResponse{status: http.StatusBadRequest, body: `{"error":{"message":"tools not supported"}}`})
	c := testClient(t, stub.srv.URL)

	_, err := c.chat(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("chat: want an error for HTTP 400")
	}
	if len(stub.requests) != 1 {
		t.Errorf("requests = %d, want 1 (client errors are not retried)", len(stub.requests))
	}
	var pe *providerError
	if !errors.As(err, &pe) || pe.status != http.StatusBadRequest {
		t.Errorf("err = %v, want a providerError with status 400", err)
	}
}

func TestClientChatResponseCap(t *testing.T) {
	huge := `{"choices":[{"message":{"content":"` + strings.Repeat("x", (4<<20)+64) + `"}}]}`
	stub := newLLMStub(t, stubResponse{status: http.StatusOK, body: huge})
	c := testClient(t, stub.srv.URL)

	_, err := c.chat(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("err = %v, want the response-cap error", err)
	}
}

func TestClientChatErrorsNeverEchoEndpointOrKey(t *testing.T) {
	stub := newLLMStub(t, stubResponse{status: http.StatusBadRequest, body: `{"error":{"message":"broken"}}`})
	c := testClient(t, stub.srv.URL)

	_, err := c.chat(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("chat: want an error")
	}
	if msg := err.Error(); strings.Contains(msg, stub.srv.URL) || strings.Contains(msg, "sekret-key") {
		t.Errorf("error leaks endpoint/key: %q", msg)
	}

	// Transport failure: a closed listener's address must not surface.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	down := testClient(t, "http://"+addr)
	_, err = down.chat(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("chat against a closed listener: want an error")
	}
	if msg := err.Error(); strings.Contains(msg, addr) {
		t.Errorf("transport error leaks the endpoint: %q", msg)
	}
}
