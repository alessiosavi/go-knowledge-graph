package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const generationModel = "DeepSeek-V4.1-Flash-1017"

func TestGenerationValidationBeforeAsk(t *testing.T) {
	stub := newLLMStub(t)
	cfg := enabledCfg(stub.srv.URL)
	cfg.Agent.Model = generationModel
	s := NewServer(cfg, nil, nil)
	for _, h := range []http.Handler{s.AskHandler(), s.StreamHandler()} {
		for _, fields := range []string{
			`{"temperature":-0.1}`, `{"temperature":2.01}`, `{"top_p":-0.1}`,
			`{"top_p":1.01}`, `{"top_k":3}`, `{"top_k":1.5}`, `{"top_p":"0.8"}`,
			`{"reasoning_effort":"low"}`, `{"reasoning_effort":""}`, `{"topP":0.8}`, `[]`,
		} {
			rr := postAsk(t, h, `{"question":"q","generation":`+fields+`}`)
			if rr.Code != 400 || rr.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("generation=%s: %d %s", fields, rr.Code, rr.Body)
			}
		}
	}
	cfg.Agent.TopKEnabled = true
	cfg.Agent.Model = "other-model"
	s = NewServer(cfg, nil, nil)
	for _, fields := range []string{`{"reasoning_effort":"high"}`, `{"top_k":0}`, `{"top_k":-8}`} {
		if rr := postAsk(t, s.AskHandler(), `{"question":"q","generation":`+fields+`}`); rr.Code != 400 {
			t.Fatalf("generation=%s: %d %s", fields, rr.Code, rr.Body)
		}
	}
	if len(stub.requests) != 0 {
		t.Fatalf("invalid requests reached provider: %d", len(stub.requests))
	}
}

func TestGenerationStatusAndDefaults(t *testing.T) {
	for _, model := range []string{generationModel, "other-model"} {
		stub := newLLMStub(t)
		cfg := enabledCfg(stub.srv.URL)
		cfg.Agent.Model = model
		cfg.Agent.TopKEnabled = model != generationModel
		s := NewServer(cfg, nil, nil)
		rr := doReq(t, s.StatusHandler(), "GET", "/v1/agent/status", "", nil)
		var status struct{ Generation generationCapabilities }
		if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		wantEfforts := []string{}
		if model == generationModel {
			wantEfforts = []string{"none", "high", "max"}
		}
		if status.Generation.TopK != cfg.Agent.TopKEnabled || !slices.Equal(status.Generation.ReasoningEfforts, wantEfforts) {
			t.Fatalf("%s status: %s", model, rr.Body)
		}
		for _, body := range []string{`{"question":"q"}`, `{"question":"q","generation":{}}`} {
			rr := postAsk(t, s.AskHandler(), body)
			if rr.Code != 200 {
				t.Fatal(rr.Body)
			}
			req := stub.requests[len(stub.requests)-1]
			if req["temperature"] != 0.0 {
				t.Fatalf("default temperature: %v", req["temperature"])
			}
			for _, field := range []string{"top_p", "top_k", "reasoning_effort"} {
				if _, exists := req[field]; exists {
					t.Errorf("default sent %s", field)
				}
			}
		}
	}
}

func TestGenerationAcrossTurns(t *testing.T) {
	const body = `{"question":"q","generation":{"temperature":2,"top_p":0,"top_k":7,"reasoning_effort":"max"}}`
	for _, stream := range []bool{false, true} {
		for _, path := range []string{"tools", "budget", "plan"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, path), func(t *testing.T) {
				stub := newLLMStub(t)
				cfg := enabledCfg(stub.srv.URL)
				cfg.Agent.Model = generationModel
				cfg.Agent.Stream, cfg.Agent.TopKEnabled = stream, true
				if path == "budget" {
					cfg.Agent.MaxSteps = 1
				}
				wantCalls := 2
				if path == "plan" {
					rejection := stubResponse{400, `{"error":{"message":"function calling is not supported by this model"}}`}
					stub.script = append(stub.script, rejection, contentReply(`{"tool_calls":[{"name":"unknown","arguments":{}}]}`))
					wantCalls++
					if stream {
						stub.withStreamStatus(400, rejection.body)
						wantCalls++
					}
				} else if stream {
					stub.withStream(
						sseDelta(map[string]any{"reasoning_content": "private "}, ""),
						sseDelta(map[string]any{"reasoning_content": "continuation"}, ""),
						sseTool(0, "c1", "unknown", "{}"), sseDone,
					)
				} else {
					reply := toolCallReply("c1", "unknown", "{}")
					reply.body = strings.Replace(reply.body, `"tool_calls":`, `"reasoning_content":"private continuation","tool_calls":`, 1)
					stub.script = append(stub.script, reply)
				}
				stub.script = append(stub.script, contentReply("final answer"))
				stub.withStream(sseContent("final answer"), sseDone)
				s := NewServer(cfg, nil, nil)
				handler := s.AskHandler()
				if stream {
					handler = s.StreamHandler()
				}
				rr := postAsk(t, handler, body)
				if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"answer":"final answer"`) || len(stub.requests) != wantCalls {
					t.Fatalf("%d requests=%d body=%s", rr.Code, len(stub.requests), rr.Body)
				}
				if strings.Contains(rr.Body.String(), "private ") || strings.Contains(rr.Body.String(), "continuation") {
					t.Fatal("provider reasoning leaked into the answer or an SSE event")
				}
				if path == "tools" || path == "budget" {
					msgs := requestMessages(t, stub.requests[1])
					continuation := msgs[2]["reasoning_content"]
					if (path == "tools" && continuation != "private continuation") || (path == "budget" && continuation != nil) {
						t.Errorf("tool continuation mismatch for %s", path)
					}
				}
				for i, req := range stub.requests {
					want := map[string]any{"temperature": 2.0, "top_p": 0.0, "top_k": 7.0, "reasoning_effort": "max"}
					for key, value := range want {
						if req[key] != value {
							t.Errorf("turn %d, %s=%v want %v", i, key, req[key], value)
						}
					}
				}
			})
		}
	}
}

func TestGenerationRejectionIsTerminal(t *testing.T) {
	const rejection = `{"error":{"message":"reasoning_effort rejected for this tool request"}}`
	for _, path := range []string{"json", "sse", "sse error frame", "final", "ambiguous"} {
		t.Run(path, func(t *testing.T) {
			stub := newLLMStub(t)
			cfg := enabledCfg(stub.srv.URL)
			cfg.Agent.Model, cfg.Agent.Stream = generationModel, true
			cfg.Agent.MaxSteps = 1
			wantCalls := 1
			switch path {
			case "sse":
				stub.withStreamStatus(422, rejection)
			case "sse error frame":
				stub.withStream(sseError("invalid reasoning_effort"))
			case "final":
				stub.script = append(stub.script, toolCallReply("c1", "unknown", "{}"), stubResponse{400, rejection})
				wantCalls++
			case "ambiguous":
				stub.script = append(stub.script, stubResponse{400, "upstream validation failed"})
			default:
				stub.script = append(stub.script, stubResponse{400, rejection})
			}
			s := NewServer(cfg, nil, nil)
			h := s.AskHandler()
			if strings.HasPrefix(path, "sse") {
				h = s.StreamHandler()
			}
			rr := postAsk(t, h, `{"question":"q","generation":{"reasoning_effort":"high"}}`)
			if len(stub.requests) != wantCalls || !strings.Contains(rr.Body.String(), `"error":"agent_error"`) {
				t.Fatalf("requests=%d response=%s", len(stub.requests), rr.Body)
			}
			if a, b := s.llm.streamState(); a || b {
				t.Fatalf("rejection affected future asks: %v %v", a, b)
			}
		})
	}
}

func TestGenerationConcurrentAsks(t *testing.T) {
	// Hold both model turns in flight to catch settings shared across asks.
	type received struct {
		Messages    []chatMessage
		Temperature float64
		Stream      bool
	}
	requests := make(chan received, 2)
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req received
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		requests <- req
		<-release
		if req.Stream {
			fmt.Fprint(w, sseContent("answer")+sseDone)
		} else {
			fmt.Fprint(w, contentReply("answer").body)
		}
	}))
	defer provider.Close()
	cfg := enabledCfg(provider.URL)
	cfg.Agent.Stream = true
	s := NewServer(cfg, nil, nil)
	var wg sync.WaitGroup
	for i, h := range []http.Handler{s.AskHandler(), s.StreamHandler()} {
		wg.Go(func() {
			rr := postAsk(t, h, fmt.Sprintf(`{"question":"q%d","generation":{"temperature":%d}}`, i, i))
			if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"answer":"answer"`) {
				t.Errorf("response=%s", rr.Body)
			}
		})
	}
	actual := map[string]float64{}
	for range 2 {
		select {
		case req := <-requests:
			actual[req.Messages[1].Content] = req.Temperature
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("both asks did not reach the provider")
		}
	}
	close(release)
	wg.Wait()
	if !reflect.DeepEqual(actual, map[string]float64{"q0": 0, "q1": 1}) {
		t.Fatalf("per-ask temperature: %v", actual)
	}
}

func TestGenerationStreamFallbackIsRequestScoped(t *testing.T) {
	for _, mode := range []string{"unsupported", "content only"} {
		t.Run(mode, func(t *testing.T) {
			stub := newLLMStub(t)
			cfg := enabledCfg(stub.srv.URL)
			cfg.Agent.Stream, cfg.Agent.MaxSteps = true, 1
			s := NewServer(cfg, nil, nil)
			// Exercise two custom asks followed by the legacy default. Only
			// the last ask should learn a process-wide capability.
			for attempt, body := range []string{
				`{"question":"q","generation":{"top_p":0.9}}`,
				`{"question":"q","generation":{"top_p":0.9}}`,
				`{"question":"q"}`,
			} {
				start := len(stub.requests)
				if mode == "unsupported" {
					stub.withStreamStatus(400, `{"error":{"message":"streaming unsupported"}}`)
					stub.script = append(stub.script, contentReply("answer"))
				} else {
					stub.withStream(sseDone)
					stub.script = append(stub.script, toolCallReply("c1", "unknown", "{}"))
					stub.withStream(sseContent("answer"), sseDone)
				}
				rr := postAsk(t, s.StreamHandler(), body)
				if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"answer":"answer"`) {
					t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
				}
				calls := stub.requests[start:]
				if len(calls) < 2 || calls[0]["stream"] != true || calls[1]["stream"] != nil {
					t.Fatalf("unexpected negotiated calls: %d", len(calls))
				}
				unsupported, contentOnly := s.llm.streamState()
				wantUnsupported := attempt == 2 && mode == "unsupported"
				wantContentOnly := attempt == 2 && mode == "content only"
				if unsupported != wantUnsupported || contentOnly != wantContentOnly {
					t.Fatalf("attempt %d: unsupported=%v contentOnly=%v, want %v/%v", attempt, unsupported, contentOnly, wantUnsupported, wantContentOnly)
				}
			}
		})
	}
}
