package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// sseEvent is one parsed event of an SSE response body.
type sseEvent struct{ name, data string }

// parseSSE splits an SSE body into events; ": ping" comments surface as
// name "comment".
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, ":"):
				ev.name = "comment"
				ev.data = strings.TrimSpace(strings.TrimPrefix(line, ":"))
			case strings.HasPrefix(line, "event:"):
				ev.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				ev.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		events = append(events, ev)
	}
	return events
}

func postStream(t *testing.T, h http.Handler, question string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"question": question})
	return doReq(t, h, http.MethodPost, "/v1/agent/ask/stream", string(body), map[string]string{"Content-Type": "application/json"})
}

// TestStreamHandlerFramingAndDone drives one full ask through the stream
// handler: a streamed tool-call turn, the executed tool event, streamed
// answer deltas, and the authoritative done payload.
func TestStreamHandlerFramingAndDone(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t)
	stub.withStream(
		sseTool(0, "c1", "search", fmt.Sprintf(`{"query":%q}`, fx.token)),
		sseFinish("tool_calls"),
		sseDone,
	)
	stub.withStream(sseContent("done "), sseContent("answer"), sseFinish("stop"), sseDone)
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) { cfg.Agent.Stream = true })

	rr := postStream(t, s.StreamHandler(), "who is Ada "+fx.token+"?")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := rr.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}

	events := parseSSE(t, rr.Body.String())
	var toolEvent, doneEvent *sseEvent
	var deltas []string
	for i := range events {
		switch events[i].name {
		case "tool":
			toolEvent = &events[i]
		case "delta":
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(events[i].data), &payload); err != nil {
				t.Errorf("delta payload %q: %v", events[i].data, err)
			}
			deltas = append(deltas, payload.Text)
		case "done":
			doneEvent = &events[i]
		}
	}
	if toolEvent == nil {
		t.Fatalf("no tool event in %+v", events)
	}
	var toolPayload struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(toolEvent.data), &toolPayload); err != nil {
		t.Fatalf("tool payload %q: %v", toolEvent.data, err)
	}
	if toolPayload.Name != "search" || !strings.Contains(string(toolPayload.Arguments), fx.token) {
		t.Errorf("tool event = %s, want the search call with the fixture query", toolEvent.data)
	}
	if strings.Join(deltas, "") != "done answer" {
		t.Errorf("deltas = %q, want the streamed answer fragments", deltas)
	}
	if doneEvent == nil {
		t.Fatalf("no done event in %+v", events)
	}
	var resp askResponse
	if err := json.Unmarshal([]byte(doneEvent.data), &resp); err != nil {
		t.Fatalf("done payload %q: %v", doneEvent.data, err)
	}
	if resp.Answer != "done answer" || resp.Steps != 1 || len(resp.Evidence) != 1 || resp.Evidence[0].Tool != "search" {
		t.Errorf("done payload = %+v, want the authoritative answer with one search evidence", resp)
	}
}

func TestStreamHandlerDisabled(t *testing.T) {
	s := NewServer(config.Config{}, nil, nil)
	rr := postStream(t, s.StreamHandler(), "anything")
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("code = %d, want 501", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON (no SSE on a disabled agent)", ct)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Error != "agent_disabled" {
		t.Errorf("body = %s, want error agent_disabled", rr.Body.String())
	}
}

func TestStreamHandlerInvalidQuestion(t *testing.T) {
	s := NewServer(enabledCfg("http://127.0.0.1:1"), nil, nil)
	rr := doReq(t, s.StreamHandler(), http.MethodPost, "/v1/agent/ask/stream", `{}`, map[string]string{"Content-Type": "application/json"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, never SSE for an invalid body", ct)
	}
	if !strings.Contains(rr.Body.String(), "invalid_request") {
		t.Errorf("body = %s, want invalid_request", rr.Body.String())
	}
}

func TestStreamHandlerTimeout(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	cfg := enabledCfg(slow.URL)
	cfg.Agent.TimeoutS = 1
	cfg.Agent.Stream = true
	s := NewServer(cfg, nil, nil)
	s.llm.backoff = nil

	start := time.Now()
	rr := postStream(t, s.StreamHandler(), "hang?")
	elapsed := time.Since(start)
	close(release)

	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (SSE headers precede the error)", rr.Code)
	}
	events := parseSSE(t, rr.Body.String())
	if len(events) == 0 {
		t.Fatalf("no SSE events in %q", rr.Body.String())
	}
	last := events[len(events)-1]
	if last.name != "error" || !strings.Contains(last.data, "agent_timeout") {
		t.Errorf("last event = %+v, want error agent_timeout", last)
	}
	if elapsed > 2500*time.Millisecond {
		t.Errorf("stream handler took %v, want it bounded by the 1s timeout", elapsed)
	}
}

func TestStreamHandlerProviderError(t *testing.T) {
	stub := newLLMStub(t).withStreamStatus(http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	cfg := enabledCfg(stub.srv.URL)
	cfg.Agent.Stream = true
	s := NewServer(cfg, nil, nil)
	s.llm.backoff = nil

	rr := postStream(t, s.StreamHandler(), "anything")
	events := parseSSE(t, rr.Body.String())
	if len(events) == 0 {
		t.Fatalf("no SSE events in %q", rr.Body.String())
	}
	last := events[len(events)-1]
	if last.name != "error" || !strings.Contains(last.data, "agent_error") || !strings.Contains(last.data, "provider") {
		t.Errorf("last event = %+v, want the classified agent_error", last)
	}
	if strings.Contains(last.data, stub.srv.URL) || strings.Contains(last.data, "sekret-key") {
		t.Errorf("error event leaks endpoint/key: %s", last.data)
	}
}

func TestStreamHandlerStreamFlagOff(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t,
		toolCallReply("c1", "search", fmt.Sprintf(`{"query":%q}`, fx.token)),
		contentReply("plain answer"),
	)
	s := askServer(t, fx, stub.srv.URL, nil) // cfg.Agent.Stream stays false

	rr := postStream(t, s.StreamHandler(), "q "+fx.token)
	events := parseSSE(t, rr.Body.String())
	var doneEvent *sseEvent
	for i := range events {
		if events[i].name == "delta" {
			t.Errorf("delta event with streaming off: %+v", events[i])
		}
		if events[i].name == "tool" {
			// Tool events stay regardless of provider streaming.
		}
		if events[i].name == "done" {
			doneEvent = &events[i]
		}
	}
	if doneEvent == nil {
		t.Fatalf("no done event in %+v", events)
	}
	var resp askResponse
	if err := json.Unmarshal([]byte(doneEvent.data), &resp); err != nil {
		t.Fatalf("done payload %q: %v", doneEvent.data, err)
	}
	if resp.Answer != "plain answer" {
		t.Errorf("done answer = %q, want the non-streamed reply", resp.Answer)
	}
}

func TestStreamHandlerMaxTurns(t *testing.T) {
	fx := newAgentFixture(t)
	stub := newLLMStub(t)
	for i := 0; i < 2; i++ {
		stub.withStream(
			sseTool(0, fmt.Sprintf("c%d", i), "search", fmt.Sprintf(`{"query":%q}`, fx.token)),
			sseFinish("tool_calls"),
			sseDone,
		)
	}
	s := askServer(t, fx, stub.srv.URL, func(cfg *config.Config) {
		cfg.Agent.Stream = true
		cfg.Agent.MaxSteps = 2
	})

	rr := postStream(t, s.StreamHandler(), "loop forever?")
	events := parseSSE(t, rr.Body.String())
	if len(events) == 0 {
		t.Fatalf("no SSE events in %q", rr.Body.String())
	}
	last := events[len(events)-1]
	if last.name != "done" {
		t.Fatalf("last event = %+v, want done after the step budget", last)
	}
	var resp askResponse
	if err := json.Unmarshal([]byte(last.data), &resp); err != nil {
		t.Fatalf("done payload %q: %v", last.data, err)
	}
	if resp.Steps != 2 || !strings.Contains(resp.Answer, "step budget") {
		t.Errorf("done payload = %+v, want the bounded-step notice with steps 2", resp)
	}
}

func TestStreamHandlerClientCanceled(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	cfg := enabledCfg(slow.URL)
	cfg.Agent.TimeoutS = 30
	cfg.Agent.Stream = true
	s := NewServer(cfg, nil, nil)
	s.llm.backoff = nil

	front := httptest.NewServer(s.StreamHandler())
	t.Cleanup(front.Close)

	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]string{"question": "cancel me"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	cancel()

	start := time.Now()
	_, _ = bufio.NewReader(resp.Body).ReadString('\n')
	_ = resp.Body.Close()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("read after cancel took %v, want the stream to end promptly", elapsed)
	}
}

func TestStreamHandlerHeartbeat(t *testing.T) {
	old := streamPing
	streamPing = 40 * time.Millisecond
	t.Cleanup(func() { streamPing = old })

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	cfg := enabledCfg(slow.URL)
	cfg.Agent.TimeoutS = 30
	cfg.Agent.Stream = true
	s := NewServer(cfg, nil, nil)
	s.llm.backoff = nil

	front := httptest.NewServer(s.StreamHandler())
	t.Cleanup(front.Close)

	body, _ := json.Marshal(map[string]string{"question": "slow one"})
	resp, err := front.Client().Post(front.URL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	found := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, ": ping") {
			found = true
			break
		}
	}
	if !found {
		t.Error("no heartbeat comment within 3s")
	}
}

func TestStatusStreamField(t *testing.T) {
	status := func(h http.Handler) (enabled, stream bool) {
		rr := doReq(t, h, http.MethodGet, "/v1/agent/status", "", nil)
		var body struct {
			Enabled bool `json:"enabled"`
			Stream  bool `json:"stream"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("status body %q: %v", rr.Body.String(), err)
		}
		return body.Enabled, body.Stream
	}

	disabled := NewServer(config.Config{}, nil, nil)
	if enabled, stream := status(disabled.StatusHandler()); enabled || stream {
		t.Errorf("disabled: enabled=%v stream=%v, want false/false", enabled, stream)
	}

	onCfg := enabledCfg("http://127.0.0.1:1")
	onCfg.Agent.Stream = true
	on := NewServer(onCfg, nil, nil)
	if enabled, stream := status(on.StatusHandler()); !enabled || !stream {
		t.Errorf("enabled+stream on: enabled=%v stream=%v, want true/true", enabled, stream)
	}
	on.llm.memoStreamUnsupported()
	if enabled, stream := status(on.StatusHandler()); !enabled || stream {
		t.Errorf("after unsupported memo: enabled=%v stream=%v, want true/false", enabled, stream)
	}

	offCfg := enabledCfg("http://127.0.0.1:1")
	off := NewServer(offCfg, nil, nil) // Stream stays false
	if enabled, stream := status(off.StatusHandler()); !enabled || stream {
		t.Errorf("stream flag off: enabled=%v stream=%v, want true/false", enabled, stream)
	}
}

// TestStreamBearerPolicy pins that the mounted handler inherits the REST
// bearer policy (the 501 with a valid token proves the route is reached).
func TestStreamBearerPolicy(t *testing.T) {
	apiCfg := config.Config{}
	apiCfg.Server.APIKey = "sekret"
	rest := api.NewServer(apiCfg, nil, nil)
	agentSrv := NewServer(config.Config{}, nil, nil)
	rest.Mount("POST /v1/agent/ask/stream", agentSrv.StreamHandler())
	h := rest.Handler()

	rr := postStream(t, h, "hi")
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("without token: code = %d, want 401", rr.Code)
	}
	rr = doReq(t, h, http.MethodPost, "/v1/agent/ask/stream", `{"question":"hi"}`, map[string]string{
		"Authorization": "Bearer sekret",
	})
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("with token (disabled agent): code = %d, want the handler's 501", rr.Code)
	}
}
