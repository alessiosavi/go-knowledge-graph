package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestBudgetFinalTurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		final stubResponse
		want  string
	}{
		{"answer", contentReply("# Gathered answer"), "# Gathered answer"},
		{"empty", contentReply("  "), noAnswerText},
		{"error", stubResponse{status: 503}, noAnswerText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newLLMStub(t, toolCallReply("c1", "unknown", `{}`), tc.final)
			cfg := enabledCfg(stub.srv.URL)
			cfg.Agent.MaxSteps = 1
			s := NewServer(cfg, nil, nil)
			s.llm.backoff = nil
			resp, err := s.ask(context.Background(), "question")
			if err != nil || resp.Answer != tc.want || resp.Steps != 1 || resp.AnswerHTML == "" {
				t.Fatalf("response=%+v err=%v", resp, err)
			}
			if len(stub.requests) != 2 {
				t.Fatalf("requests=%d, want one tool turn and one final turn", len(stub.requests))
			}
			last := stub.requests[1]
			if last["tools"] != nil || last["tool_choice"] != nil {
				t.Error("final turn must omit tools")
			}
			msgs := requestMessages(t, last)
			if !strings.Contains(msgs[len(msgs)-1]["content"].(string), "answer") {
				t.Error("missing final-answer instruction")
			}
		})
	}
}

func TestBudgetFinalTurnStreams(t *testing.T) {
	stub := newLLMStub(t)
	stub.withStream(sseTool(0, "c1", "unknown", `{}`), sseDone)
	stub.withStream(sseContent("# Final"), sseDone)
	cfg := enabledCfg(stub.srv.URL)
	cfg.Agent.MaxSteps = 1
	cfg.Agent.Stream = true
	s := NewServer(cfg, nil, nil)
	var deltas string
	resp, err := s.run(context.Background(), "q", func(ev streamEvent) error {
		if ev.Kind == "delta" {
			deltas += ev.Text
		}
		return nil
	})
	if err != nil || deltas != "# Final" || resp.AnswerHTML != "<h1>Final</h1>\n" || resp.Steps != 1 {
		t.Fatalf("response=%+v deltas=%q err=%v", resp, deltas, err)
	}
	if len(stub.requests) != 2 || stub.requests[1]["stream"] != true || stub.requests[1]["tools"] != nil {
		t.Fatalf("requests=%v", stub.requests)
	}
}

func TestTranscriptResultCap(t *testing.T) {
	for _, raw := range []string{"small", strings.Repeat("x", 32<<10), strings.Repeat("€", 20<<10)} {
		got := transcriptResult([]byte(raw))
		if len(got) > 32<<10 || !utf8.ValidString(got) {
			t.Fatalf("invalid transcript copy: %d bytes", len(got))
		}
		if len(raw) <= 32<<10 && got != raw {
			t.Error("short result changed")
		}
		if len(raw) > 32<<10 && (!strings.Contains(got, "truncated") || !strings.Contains(got, "narrow")) {
			t.Error("missing truncation guidance")
		}
	}
}

func TestContextBudgetStopsToolsAndKeepsEvidence(t *testing.T) {
	for _, plan := range []bool{false, true} {
		t.Run(map[bool]string{false: "tools", true: "plan"}[plan], func(t *testing.T) {
			largeName := strings.Repeat("x", 40<<10)
			calls := []scriptedCall{{"c1", largeName, `{}`}, {"c2", largeName, `{}`}, {"c3", "unknown", `{}`}}
			first := toolCallsReply(calls...)
			if plan {
				b, _ := json.Marshal(map[string]any{"tool_calls": []map[string]any{{"name": largeName, "arguments": map[string]any{}}, {"name": largeName, "arguments": map[string]any{}}, {"name": "unknown", "arguments": map[string]any{}}}})
				first = contentReply(string(b))
			}
			stub := newLLMStub(t, first, contentReply("Budget answer"))
			cfg := enabledCfg(stub.srv.URL)
			cfg.Agent.ContextBudgetKB = 64
			// Unknown tool dispatch never dereferences the store.
			s := NewServer(cfg, &graph.Store{}, nil)
			var resp askResponse
			var err error
			if plan {
				resp, err = s.askWithPlan(context.Background(), "q", nil)
			} else {
				resp, err = s.ask(context.Background(), "q")
			}
			if err != nil || resp.Steps != 1 || resp.Answer != "Budget answer" || resp.AnswerHTML == "" {
				t.Fatalf("response=%+v err=%v", resp, err)
			}
			if len(resp.Evidence) != 3 || len(resp.Evidence[0].Result) <= 32<<10 || len(resp.Evidence[1].Result) <= 32<<10 || !strings.Contains(string(resp.Evidence[2].Result), "context budget") {
				t.Fatal("full evidence or budget skip missing")
			}
			if len(stub.requests) != 2 || stub.requests[1]["tools"] != nil {
				t.Fatalf("requests=%d, want final turn without tools", len(stub.requests))
			}
			msgs := requestMessages(t, stub.requests[1])
			if !plan {
				count := 0
				for _, msg := range msgs {
					if msg["role"] == "tool" {
						count++
						if len(msg["content"].(string)) > 32<<10 {
							t.Error("uncapped transcript result")
						}
					}
				}
				if count != 3 {
					t.Errorf("paired results=%d, want 3", count)
				}
			} else if !strings.Contains(msgs[len(msgs)-1]["content"].(string), "truncated") {
				t.Error("plan transcript is uncapped")
			}
		})
	}
}

func TestBudgetFinalTurnPreservesContextErrors(t *testing.T) {
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(ctxErr.Error(), func(t *testing.T) {
			stub := newLLMStub(t, toolCallReply("c1", "unknown", `{}`))
			cfg := enabledCfg(stub.srv.URL)
			cfg.Agent.MaxSteps = 1
			s := NewServer(cfg, nil, nil)
			s.llm.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if len(stub.requests) == 1 {
					return nil, ctxErr
				}
				return http.DefaultTransport.RoundTrip(req)
			})
			_, err := s.ask(context.Background(), "q")
			if !errors.Is(err, ctxErr) {
				t.Fatalf("err=%v, want %v", err, ctxErr)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnswerHTMLSuccessPaths(t *testing.T) {
	for _, plan := range []string{"", `{"answer":"# Title"}`, `{"tool_calls":[{"name":"unknown","arguments":{}}]}`} {
		replies := []stubResponse{contentReply("# Title")}
		if plan != "" {
			replies = []stubResponse{{status: 400}, contentReply(plan), contentReply("# Title")}
		}
		stub := newLLMStub(t, replies...)
		s := NewServer(enabledCfg(stub.srv.URL), nil, nil)
		rr := postAsk(t, s.AskHandler(), `{"question":"q"}`)
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if rr.Code != 200 || payload["answer_html"] != "<h1>Title</h1>\n" {
			t.Fatalf("status=%d payload=%v", rr.Code, payload)
		}
	}
}

func TestContextBudgetAcrossToolTurns(t *testing.T) {
	name := strings.Repeat("x", 400)
	stub := newLLMStub(t,
		toolCallReply("c1", name, `{}`), toolCallReply("c2", name, `{}`), toolCallReply("c3", name, `{}`),
		contentReply("Gathered answer"),
	)
	cfg := enabledCfg(stub.srv.URL)
	cfg.Agent.MaxSteps = 30
	cfg.Agent.ContextBudgetKB = 1
	s := NewServer(cfg, &graph.Store{}, nil)
	resp, err := s.ask(context.Background(), "q")
	if err != nil || resp.Answer != "Gathered answer" || resp.Steps != 3 || len(resp.Evidence) != 3 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(stub.requests) != 4 || stub.requests[3]["tools"] != nil {
		t.Fatalf("requests=%d, want three tool turns and a final turn", len(stub.requests))
	}
}

func TestThirtyToolTurnsThenFinalAnswer(t *testing.T) {
	replies := make([]stubResponse, 0, 31)
	for i := 0; i < 30; i++ {
		replies = append(replies, toolCallReply("call", "unknown", `{}`))
	}
	replies = append(replies, contentReply("Final after thirty"))
	stub := newLLMStub(t, replies...)
	cfg := enabledCfg(stub.srv.URL)
	cfg.Agent.MaxSteps = 100
	s := NewServer(cfg, nil, nil)
	resp, err := s.ask(context.Background(), "q")
	if err != nil || resp.Steps != 30 || len(resp.Evidence) != 30 || resp.Answer != "Final after thirty" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(stub.requests) != 31 || stub.requests[30]["tools"] != nil {
		t.Fatal("hard cap must allow thirty tool turns plus one final tools-less turn")
	}
}

func TestPlanContextBudgetFinalFailure(t *testing.T) {
	name := strings.Repeat("x", 40<<10)
	plan, _ := json.Marshal(map[string]any{"tool_calls": []map[string]any{{"name": name, "arguments": map[string]any{}}}})
	stub := newLLMStub(t, contentReply(string(plan)), stubResponse{status: 503})
	cfg := enabledCfg(stub.srv.URL)
	cfg.Agent.ContextBudgetKB = 1
	s := NewServer(cfg, &graph.Store{}, nil)
	s.llm.backoff = nil
	resp, err := s.askWithPlan(context.Background(), "q", nil)
	if err != nil || resp.Answer != noAnswerText || resp.Steps != 1 || resp.AnswerHTML == "" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestContextBudgetDefault(t *testing.T) {
	for _, kb := range []int{0, -1, 256} {
		cfg := config.Config{}
		cfg.Agent.ContextBudgetKB = kb
		s := NewServer(cfg, nil, nil)
		if s.contextBudgetReached((256<<10)-1) || !s.contextBudgetReached(256<<10) {
			t.Errorf("budget=%d, want threshold at 256 KiB", kb)
		}
	}
}

func TestAnswerHTMLStreamDone(t *testing.T) {
	stub := newLLMStub(t).withStream(sseContent("# Title"), sseDone)
	cfg := enabledCfg(stub.srv.URL)
	cfg.Agent.Stream = true
	s := NewServer(cfg, nil, nil)
	rr := postStream(t, s.StreamHandler(), "q")
	found := false
	for _, ev := range parseSSE(t, rr.Body.String()) {
		if ev.name != "done" {
			continue
		}
		found = true
		var payload map[string]any
		if err := json.Unmarshal([]byte(ev.data), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["answer_html"] != "<h1>Title</h1>\n" || payload["answer"] != "# Title" {
			t.Fatalf("done=%v", payload)
		}
	}
	if rr.Code != 200 || !found {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}
