package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// stubSSE scripts one streamed reply: a status override (0 means 200
// text/event-stream) or raw SSE frames served in order.
type stubSSE struct {
	status int
	body   string
	frames []string
}

// llmStub is a scripted OpenAI-compatible endpoint: every request body is
// recorded, and the next script entry answers it (a plain "ok" answer when
// the script is exhausted). Requests carrying "stream":true are answered
// from streams instead of script.
type llmStub struct {
	srv      *httptest.Server
	requests []map[string]any
	headers  []http.Header
	script   []stubResponse
	streams  []stubSSE
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
		if isStream, _ := body["stream"].(bool); isStream {
			reply := stubSSE{frames: []string{sseContent("ok"), sseDone}}
			if len(s.streams) > 0 {
				reply, s.streams = s.streams[0], s.streams[1:]
			}
			if reply.status != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(reply.status)
				_, _ = w.Write([]byte(reply.body))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, frame := range reply.frames {
				_, _ = io.WriteString(w, frame)
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
			return
		}
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

// withStream appends one scripted streamed reply (raw SSE frames).
func (s *llmStub) withStream(frames ...string) *llmStub {
	s.streams = append(s.streams, stubSSE{frames: frames})
	return s
}

// withStreamStatus appends one streamed reply that fails at the HTTP layer.
func (s *llmStub) withStreamStatus(status int, body string) *llmStub {
	s.streams = append(s.streams, stubSSE{status: status, body: body})
	return s
}

// SSE frame builders for scripted streamed replies.
const sseDone = "data: [DONE]\n\n"

func sseDelta(delta map[string]any, finish string) string {
	choice := map[string]any{"delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	b, _ := json.Marshal(map[string]any{"choices": []map[string]any{choice}})
	return "data: " + string(b) + "\n\n"
}

func sseContent(text string) string { return sseDelta(map[string]any{"content": text}, "") }

func sseRole(role string) string { return sseDelta(map[string]any{"role": role}, "") }

func sseFinish(reason string) string { return sseDelta(map[string]any{}, reason) }

func sseTool(index int, id, name, args string) string {
	call := map[string]any{"index": index, "function": map[string]any{}}
	if id != "" {
		call["id"] = id
	}
	fn := call["function"].(map[string]any)
	if name != "" {
		fn["name"] = name
	}
	if args != "" {
		fn["arguments"] = args
	}
	return sseDelta(map[string]any{"tool_calls": []map[string]any{call}}, "")
}

func sseError(msg string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg}})
	return "data: " + string(b) + "\n\n"
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

func TestChatStreamAssemblesContentDeltas(t *testing.T) {
	stub := newLLMStub(t).withStream(
		sseRole("assistant"),
		sseContent("stream"),
		sseContent("ing"),
		sseContent(" works"),
		sseFinish("stop"),
		sseDone,
	)
	c := testClient(t, stub.srv.URL)

	var deltas []string
	reply, err := c.chatStream(context.Background(), chatRequest{
		Model:    "stub-model",
		Messages: []chatMessage{{Role: "user", Content: "hi"}},
	}, func(text string) error {
		deltas = append(deltas, text)
		return nil
	})
	if err != nil {
		t.Fatalf("chatStream: %v", err)
	}
	if reply.Content != "streaming works" {
		t.Errorf("Content = %q, want %q", reply.Content, "streaming works")
	}
	if strings.Join(deltas, "|") != "stream|ing| works" {
		t.Errorf("onDelta fragments = %q, want the three content fragments in order", deltas)
	}
	if stub.requests[0]["stream"] != true {
		t.Errorf("request stream = %v, want true", stub.requests[0]["stream"])
	}
	if got := stub.headers[0].Get("Authorization"); got != "Bearer sekret-key" {
		t.Errorf("Authorization = %q, want the bearer key", got)
	}
}

func TestChatSendsNoStreamKey(t *testing.T) {
	stub := newLLMStub(t, contentReply("plain"))
	c := testClient(t, stub.srv.URL)

	if _, err := c.chat(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if _, ok := stub.requests[0]["stream"]; ok {
		t.Errorf("non-streamed chat sent a stream key: %v", stub.requests[0])
	}
}

func TestChatStreamAssemblesFragmentedToolCalls(t *testing.T) {
	stub := newLLMStub(t).withStream(
		sseTool(0, "call_1", "sea", ""),
		sseTool(1, "call_2", "sta", ""),
		sseTool(0, "", "rch", `{"query":`),
		sseTool(1, "", "tus", "{}"),
		sseTool(0, "", "", `"ada"}`),
		sseFinish("tool_calls"),
		sseDone,
	)
	c := testClient(t, stub.srv.URL)

	reply, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("chatStream: %v", err)
	}
	if len(reply.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want 2", len(reply.ToolCalls))
	}
	first, second := reply.ToolCalls[0], reply.ToolCalls[1]
	if first.ID != "call_1" || first.Function.Name != "search" || first.Function.Arguments != `{"query":"ada"}` {
		t.Errorf("first call = %+v, want call_1/search/{query:ada}", first)
	}
	if second.ID != "call_2" || second.Function.Name != "status" || second.Function.Arguments != "{}" {
		t.Errorf("second call = %+v, want call_2/status/{}", second)
	}
}

func TestChatStreamErrorFrameClassified(t *testing.T) {
	stub := newLLMStub(t).withStream(sseError("boom"))
	c := testClient(t, stub.srv.URL)
	c.backoff = nil

	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err == nil {
		t.Fatal("chatStream: want an error for an error frame")
	}
	var pe *providerError
	if !errors.As(err, &pe) || pe.status != http.StatusBadGateway || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want a 502 providerError carrying the provider message", err)
	}
}

func TestChatStreamHTTPErrorClassified(t *testing.T) {
	stub := newLLMStub(t).withStreamStatus(http.StatusNotFound, `{"error":{"message":"no deployment"}}`)
	c := testClient(t, stub.srv.URL)

	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	var pe *providerError
	if !errors.As(err, &pe) || pe.status != http.StatusNotFound || !strings.Contains(err.Error(), "no deployment") {
		t.Errorf("err = %v, want a 404 providerError with the provider detail", err)
	}
}

func TestChatStreamRetriesRetryableErrors(t *testing.T) {
	stub := newLLMStub(t).withStreamStatus(http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	c := testClient(t, stub.srv.URL)

	reply, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("chatStream: %v", err)
	}
	if reply.Content != "ok" {
		t.Errorf("Content = %q, want the default streamed reply", reply.Content)
	}
	if len(stub.requests) != 2 {
		t.Errorf("stream attempts = %d, want 2 (one retry)", len(stub.requests))
	}
}

func TestChatStreamClosedBeforeFirstEvent(t *testing.T) {
	stub := newLLMStub(t).withStream()
	c := testClient(t, stub.srv.URL)

	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if !errors.Is(err, errStreamClosedBeforeEvent) {
		t.Errorf("err = %v, want errStreamClosedBeforeEvent", err)
	}
}

func TestChatStreamPrematureCloseAfterEvents(t *testing.T) {
	stub := newLLMStub(t).withStream(sseContent("partial"))
	c := testClient(t, stub.srv.URL)

	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "completion marker") {
		t.Errorf("err = %v, want the premature-close error", err)
	}
	if errors.Is(err, errStreamClosedBeforeEvent) {
		t.Errorf("err = %v, must not classify as closed-before-first-event", err)
	}
}

func TestChatStreamResponseCap(t *testing.T) {
	stub := newLLMStub(t).withStream(sseContent(strings.Repeat("x", (4<<20)+64)), sseDone)
	c := testClient(t, stub.srv.URL)

	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("err = %v, want the streamed-response cap error", err)
	}
}

func TestChatStreamOnDeltaErrorAborts(t *testing.T) {
	stub := newLLMStub(t).withStream(sseContent("one"), sseContent("two"), sseDone)
	c := testClient(t, stub.srv.URL)
	sentinel := errors.New("writer gone")

	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, func(string) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the onDelta error", err)
	}
}

func TestChatStreamErrorsNeverEchoEndpointOrKey(t *testing.T) {
	stub := newLLMStub(t).withStreamStatus(http.StatusBadRequest, `{"error":{"message":"broken"}}`)
	c := testClient(t, stub.srv.URL)

	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err == nil {
		t.Fatal("chatStream: want an error")
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
	_, err = down.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err == nil {
		t.Fatal("chatStream against a closed listener: want an error")
	}
	if msg := err.Error(); strings.Contains(msg, addr) {
		t.Errorf("transport error leaks the endpoint: %q", msg)
	}
}

func TestStreamFallbackEligible(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"payload 400", &providerError{status: 400}, true},
		{"payload 404", &providerError{status: 404}, true},
		{"payload 405", &providerError{status: 405}, true},
		{"payload 415", &providerError{status: 415}, true},
		{"payload 422", &providerError{status: 422}, true},
		{"auth 401", &providerError{status: 401}, false},
		{"auth 403", &providerError{status: 403}, false},
		{"server 500", &providerError{status: 500}, false},
		{"transport", &providerError{status: 0}, false},
		{"closed before first event", errStreamClosedBeforeEvent, true},
		{"premature close", errors.New("agent: stream ended before the completion marker"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamFallbackEligible(tc.err); got != tc.want {
				t.Errorf("streamFallbackEligible(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestChatStreamNoRetryAfterDeliveredDeltas(t *testing.T) {
	stub := newLLMStub(t).withStream(sseContent("first"), sseError("cut"))
	c := testClient(t, stub.srv.URL) // the 1ms backoff would retry without the fix

	var deltas []string
	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, func(text string) error {
		deltas = append(deltas, text)
		return nil
	})
	if err == nil {
		t.Fatal("chatStream: want the classified error after the cut")
	}
	if len(stub.requests) != 1 {
		t.Errorf("stream attempts = %d, want 1 (no retry once fragments were delivered)", len(stub.requests))
	}
	if strings.Join(deltas, "|") != "first" {
		t.Errorf("deltas = %q, want just the first attempt's fragments", deltas)
	}
}

func TestChatStreamCutFirstFrameFallsBack(t *testing.T) {
	stub := newLLMStub(t).withStream(`data: {"choices":[{"delta":{"content":"cu`)
	c := testClient(t, stub.srv.URL)

	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if !errors.Is(err, errStreamClosedBeforeEvent) {
		t.Errorf("err = %v, want errStreamClosedBeforeEvent for a first frame cut mid-JSON", err)
	}
}

func TestChatStreamTruncatedFinalLineClassifiedAsCut(t *testing.T) {
	stub := newLLMStub(t).withStream(sseContent("partial"), `data: {"choices":[{"delta":{"content":"cu`)
	c := testClient(t, stub.srv.URL)

	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "completion marker") {
		t.Errorf("err = %v, want the premature-close classification for a cut final line", err)
	}
	if strings.Contains(err.Error(), "decode stream frame") {
		t.Errorf("err = %v, must not blame the truncated JSON", err)
	}
}

func TestChatStreamNameBytesCountTowardCap(t *testing.T) {
	stub := newLLMStub(t).withStream(sseTool(0, "call_1", strings.Repeat("n", (4<<20)+64), ""), sseDone)
	c := testClient(t, stub.srv.URL)

	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("err = %v, want the cap error for unbounded tool-call name bytes", err)
	}
}

func TestChatStreamRawByteBound(t *testing.T) {
	old := streamRawCap
	streamRawCap = 64
	t.Cleanup(func() { streamRawCap = old })

	stub := newLLMStub(t).withStream(sseRole("assistant"), sseContent("ok"), sseDone)
	c := testClient(t, stub.srv.URL)

	_, err := c.chatStream(context.Background(), chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("err = %v, want the cap error once the raw byte bound is exhausted", err)
	}
}
