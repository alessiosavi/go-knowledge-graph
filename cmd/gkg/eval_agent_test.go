package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

const agentEvalTestModel = "DeepSeek-V4.1-Flash-1017"

type agentEvalWireRequest struct {
	Model    string
	Messages []struct {
		Role, Content string
		ToolCalls     []struct {
			Extra json.RawMessage `json:"extra_content"`
		} `json:"tool_calls"`
	}
	Stream          bool
	Tools           []json.RawMessage
	ReasoningEffort string                `json:"reasoning_effort"`
	ResponseFormat  struct{ Type string } `json:"response_format"`
}

type agentEvalScript struct {
	mu       sync.Mutex
	requests []agentEvalWireRequest
}

func (s *agentEvalScript) sent() []agentEvalWireRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

func startAgentEvalProvider(t *testing.T, respond func(http.ResponseWriter, int, agentEvalWireRequest)) (*httptest.Server, *agentEvalScript) {
	t.Helper()
	script := new(agentEvalScript)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentEvalWireRequest
		if r.Method != "POST" || r.URL.Path != "/chat/completions" {
			t.Errorf("provider request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode provider request: %v", err)
			w.WriteHeader(400)
			return
		}
		script.mu.Lock()
		i := len(script.requests)
		script.requests = append(script.requests, req)
		script.mu.Unlock()
		respond(w, i, req)
	}))
	t.Cleanup(srv.Close)
	return srv, script
}

type agentEvalFixture struct {
	questions, out, ledger, genesis, manifest, runID, allotment string
}

func newAgentEvalFixture(t *testing.T, srv *httptest.Server, questions ...map[string]any) agentEvalFixture {
	t.Helper()
	uri := os.Getenv(graphtest.URIEnv)
	if uri == "" {
		t.Skipf("set %s to run eval-agent integration tests", graphtest.URIEnv)
	}
	dir := t.TempDir()
	f := agentEvalFixture{
		questions: filepath.Join(dir, "questions.jsonl"), out: filepath.Join(dir, "answers.jsonl"),
		ledger: filepath.Join(dir, "ledger.jsonl"), manifest: strings.Repeat("a", 64),
		runID: "agent-block", allotment: "10",
	}
	f.setLedger(t, "DS", agentEvalTestModel)
	writeJudgeEvalInput(t, f.questions, questions...)
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", cfg)
	t.Setenv("GKG_MEMGRAPH_URI", uri)
	t.Setenv("GKG_AGENT_ENABLED", "true")
	t.Setenv("GKG_AGENT_AUTH", "static")
	t.Setenv("GKG_AGENT_BASE_URL", srv.URL)
	t.Setenv("GKG_AGENT_MODEL", agentEvalTestModel)
	t.Setenv("GKG_AGENT_API_KEY", "fixture-key")
	t.Setenv("GKG_AGENT_STREAM", "true")
	t.Setenv("GKG_AGENT_REASONING_EFFORTS", "minimal,low,high")
	t.Setenv("GKG_EMBEDDER_ENABLED", "false")
	return f
}

func (f *agentEvalFixture) setLedger(t *testing.T, arm, model string) {
	t.Helper()
	header := fmt.Sprintf("{\"ledger\":\"gkg-g1t2\",\"provider\":%q,\"model\":%q,\"cap_usd\":20,\"price_in_per_mtok\":1,\"price_out_per_mtok\":1,\"output_ceiling_tokens\":1000000,\"ceiling_source\":\"fixture\",\"created\":\"2026-11-22T00:00:00Z\"}\n", arm, model)
	f.genesis = fmt.Sprintf("%x", sha256.Sum256([]byte(header)))
	if err := os.WriteFile(f.ledger, []byte(header), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f agentEvalFixture) dispatch(t *testing.T, want int, flags ...string) string {
	t.Helper()
	args := []string{"eval-agent", "--questions", f.questions, "--out", f.out, "--arm", "DS", "--manifest", f.manifest,
		"--ledger", f.ledger, "--ledger-genesis", f.genesis, "--run-id", f.runID, "--allotment", f.allotment}
	var stderr bytes.Buffer
	if code := run(append(args, flags...), io.Discard, &stderr); code != want {
		t.Fatalf("exit = %d, want %d\n%s", code, want, &stderr)
	}
	return stderr.String()
}

func agentEvalQuestion(id string, sse bool) map[string]any {
	return map[string]any{"id": id, "stratum": "P", "graph": "W", "lang": "en", "question": "Question " + id, "sse": sse}
}

func agentEvalTool(id, name, args string) string {
	return fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`, id, name, args)
}

func agentEvalReply(w http.ResponseWriter, answer string, tools ...string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"choices":[{"message":{"content":%q,"tool_calls":[%s]}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"completion_tokens_details":{"reasoning_tokens":3},"total_tokens":15}}`, answer, strings.Join(tools, ","))
}

func agentEvalStream(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, frame := range frames {
		fmt.Fprintf(w, "data: %s\n\n", frame)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

const agentEvalStreamTool = `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"cypher","arguments":"{}"}}]}}]}`
const agentEvalStreamAnswer = `{"choices":[{"index":0,"delta":{"content":"native answer"}}]}`
const agentEvalLargeArgs = `{"query":"RETURN range(1, 18000) AS payload"}`

func agentEvalRows(t *testing.T, f agentEvalFixture, n int) []map[string]any {
	t.Helper()
	rows := readJudgeEvalRows(t, f.out)
	if len(rows) != n {
		t.Fatalf("got %d answer rows, want %d", len(rows), n)
	}
	return rows
}

func agentEvalCalls(t *testing.T, row map[string]any, n int) []map[string]any {
	t.Helper()
	list, ok := row["calls"].([]any)
	if !ok || len(list) != n {
		t.Fatalf("calls = %v, want %d calls", row["calls"], n)
	}
	calls := make([]map[string]any, n)
	for i, raw := range list {
		calls[i] = raw.(map[string]any)
		if calls[i]["ask_id"] != row["id"] {
			t.Fatalf("wrong ask ID in call %v", calls[i])
		}
	}
	return calls
}

func TestEvalAgentToolEvidenceAndUsage(t *testing.T) {
	q := agentEvalQuestion("large", false)
	q["domain"], q["reasoning_effort"] = "fixture-domain", "low"
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, i int, _ agentEvalWireRequest) {
		if i == 0 {
			agentEvalReply(w, "", agentEvalTool("big", "cypher", agentEvalLargeArgs),
				agentEvalTool("bad", "cypher", "{}"), agentEvalTool("unknown", "nonexistent", "{}"))
			return
		}
		agentEvalReply(w, "with evidence")
	})
	f := newAgentEvalFixture(t, srv, q)
	f.dispatch(t, 0)
	row := agentEvalRows(t, f, 1)[0]
	if row["answer"] != "with evidence" || row["steps"] != float64(1) || row["tool_errors"] != float64(2) ||
		row["unknown_tools"] != float64(1) || row["error_class"] != nil {
		t.Fatalf("answer metadata = %v", row)
	}
	requests := script.sent()
	if len(requests) != 2 {
		t.Fatalf("sent %d requests", len(requests))
	}
	for _, req := range requests {
		if req.ReasoningEffort != "low" || req.Model != agentEvalTestModel || !strings.Contains(req.Messages[0].Content, "Domain scope: fixture-domain.") {
			t.Fatalf("model=%q effort=%q domain missing=%t", req.Model, req.ReasoningEffort, !strings.Contains(req.Messages[0].Content, "Domain scope: fixture-domain."))
		}
	}
	evidence := row["evidence"].([]any)
	if len(evidence) != 3 {
		t.Fatalf("got %d evidence items", len(evidence))
	}
	for i, item := range evidence {
		e := item.(map[string]any)
		visible := e["visible"].(string)
		if msg := requests[1].Messages[3+i]; msg.Role != "tool" || msg.Content != visible || e["error"] != (i != 0) {
			t.Fatalf("evidence %d differs from tool transcript or has wrong error flag", i)
		}
		if i == 0 && (len(visible) != 32<<10 || !strings.Contains(visible, "[Tool result truncated;")) {
			t.Fatalf("large result was not capped (size %d)", len(visible))
		}
	}
	calls := agentEvalCalls(t, row, 2)
	var totals [4]float64
	for _, call := range calls {
		usage := call["usage"].(map[string]any)
		for i, field := range []string{"prompt", "completion", "reasoning", "total"} {
			totals[i] += usage[field].(float64)
		}
	}
	if totals != [4]float64{20, 4, 6, 30} || calls[0]["tool_calls"] != float64(3) || calls[0]["stream"] != false {
		t.Fatalf("unexpected calls telemetry: %v", calls)
	}
	ledger := readJudgeEvalRows(t, f.ledger)
	if len(ledger) != 7 || ledger[1]["family"] != "A" || ledger[6]["kind"] != "close" ||
		ledger[2]["id"] != "large" || ledger[4]["id"] != "large" ||
		ledger[2]["attempt"] != float64(1) || ledger[4]["attempt"] != float64(2) ||
		ledger[3]["settled_usd"] != 0.000015 || ledger[5]["settled_usd"] != 0.000015 {
		t.Fatalf("ledger = %v", ledger)
	}
}

func TestEvalAgentPlanFallbackEvidence(t *testing.T) {
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, i int, _ agentEvalWireRequest) {
		switch i {
		case 0:
			http.Error(w, "tools unsupported", 400)
		case 1:
			agentEvalReply(w, `{"tool_calls":[{"name":"cypher","arguments":`+agentEvalLargeArgs+`},{"name":"missing","arguments":{}}]}`)
		default:
			agentEvalReply(w, "plan answer")
		}
	})
	f := newAgentEvalFixture(t, srv, agentEvalQuestion("plan", false))
	f.dispatch(t, 0)
	row := agentEvalRows(t, f, 1)[0]
	calls := agentEvalCalls(t, row, 3)
	if row["answer"] != "plan answer" || row["steps"] != float64(1) || row["unknown_tools"] != float64(1) ||
		row["tool_errors"] != float64(1) || calls[0]["status"] != float64(400) || calls[1]["response_format"] != "json_object" {
		t.Fatalf("plan metadata = %v", row)
	}
	requests := script.sent()
	if len(requests) != 3 || requests[1].ResponseFormat.Type != "json_object" || len(requests[1].Tools) != 0 {
		t.Fatalf("plan request missing, got %d requests", len(requests))
	}
	evidence := row["evidence"].([]any)
	if len(evidence) != 2 {
		t.Fatalf("got %d evidence items", len(evidence))
	}
	for i, raw := range evidence {
		item := raw.(map[string]any)
		visible := item["visible"].(string)
		if !strings.Contains(requests[2].Messages[2].Content, "\n"+visible+"\n\n") ||
			item["error"] != (i == 1) || (i == 0 && len(visible) != 32<<10) {
			t.Fatalf("wrong plan copy or error flag for item %d (size %d)", i, len(visible))
		}
	}
}

func TestEvalAgentSSEMemoIsolation(t *testing.T) {
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, i int, _ agentEvalWireRequest) {
		switch i {
		case 0:
			http.Error(w, "stream unsupported", 400)
		case 1:
			agentEvalReply(w, "", agentEvalTool("a", "cypher", "{}"))
		case 2:
			agentEvalReply(w, "fallback answer")
		case 3:
			agentEvalStream(w, agentEvalStreamTool)
		default:
			agentEvalStream(w, agentEvalStreamAnswer)
		}
	})
	f := newAgentEvalFixture(t, srv, agentEvalQuestion("fallback", true), agentEvalQuestion("native", true))
	f.dispatch(t, 0)
	rows := agentEvalRows(t, f, 2)
	for i, row := range rows {
		want := map[string]any{"tool": float64(1), "delta": float64(i), "done": float64(1), "error": float64(0), "last": "done"}
		if !reflect.DeepEqual(row["sse"], want) || row["answer"] != []string{"fallback answer", "native answer"}[i] || row["steps"] != float64(1) {
			t.Fatalf("unexpected SSE row: %v", row)
		}
	}
	first, second := agentEvalCalls(t, rows[0], 3), agentEvalCalls(t, rows[1], 2)
	if first[0]["status"] != float64(400) || second[0]["status"] != float64(200) ||
		second[0]["tool_calls"] != float64(1) || second[1]["status"] != float64(200) {
		t.Fatalf("A8 inputs missing: %v / %v", first, second)
	}
	var streams []bool
	for _, req := range script.sent() {
		streams = append(streams, req.Stream)
	}
	if !slices.Equal(streams, []bool{true, false, false, true, true}) {
		t.Fatalf("stream flags = %v", streams)
	}
}

func TestEvalAgentSSESignatures(t *testing.T) {
	const signature = `{ "thought_signature" : "synthetic" }`
	const withSignature = `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"cypher","arguments":"{"},"extra_content":` + signature + `}]}}]}`
	const argumentsTail = `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}`
	// The agent consumes only the first choice and discards its incomplete
	// extra_content. The ledger also sees the valid signature in choice 1.
	const withDiscard = `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"cypher","arguments":"{}"},"extra_content":"{\"broken\":"}]}},{"index":1,"delta":{"tool_calls":[{"index":0,"extra_content":` + signature + `}]}}]}`
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, i int, _ agentEvalWireRequest) {
		switch i {
		case 0:
			agentEvalStream(w, withSignature, argumentsTail)
		case 2:
			agentEvalStream(w, withDiscard)
		default:
			agentEvalStream(w, agentEvalStreamAnswer)
		}
	})
	f := newAgentEvalFixture(t, srv, agentEvalQuestion("echo", true), agentEvalQuestion("missing-echo", true))
	f.dispatch(t, 0, "--family", "S")
	rows := agentEvalRows(t, f, 2)
	for i, row := range rows {
		calls := agentEvalCalls(t, row, 2)
		if calls[0]["signature_in"] != true || calls[0]["signature_echoed"] != nil || calls[1]["signature_echoed"] != (i == 0) ||
			row["discarded_extra_content"] != float64(i) {
			t.Fatalf("signature telemetry: %v", row)
		}
	}
	sent := script.sent()
	if len(sent) != 4 || string(sent[1].Messages[2].ToolCalls[0].Extra) != signature || len(sent[3].Messages[2].ToolCalls[0].Extra) != 0 {
		t.Fatal("the observed signature flags differ from the captured provider requests")
	}
	for _, r := range readJudgeEvalRows(t, f.ledger)[1:] {
		if r["family"] != "S" {
			t.Fatalf("smoke call used the wrong ledger family: %v", r)
		}
	}
}

func TestEvalAgentNoAnswerText(t *testing.T) {
	srv, _ := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, _ agentEvalWireRequest) {
		agentEvalReply(w, " \t ")
	})
	f := newAgentEvalFixture(t, srv, agentEvalQuestion("blank", false))
	f.dispatch(t, 0)
	row := agentEvalRows(t, f, 1)[0]
	if row["answer"] != agent.NoAnswerText || row["error_class"] != nil || row["steps"] != float64(0) ||
		!reflect.DeepEqual(row["evidence"], []any{}) || row["sse"] != nil {
		t.Fatalf("blank answer = %v", row)
	}
}

func TestEvalAgentLedgerStop(t *testing.T) {
	if os.Getenv(graphtest.URIEnv) == "" {
		t.Skipf("set %s to run eval-agent integration tests", graphtest.URIEnv)
	}
	const child = "GKG_TB_TEST_LEDGER_STOP"
	for _, name := range []string{"plain", "sse", "start"} {
		t.Run(name, func(t *testing.T) {
			if os.Getenv(child) != name {
				cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEvalAgentLedgerStop$/^"+name+"$", "-test.v")
				cmd.Env = append(os.Environ(), child+"="+name)
				out, err := cmd.CombinedOutput()
				if err != nil || bytes.Contains(out, []byte("--- SKIP:")) {
					t.Fatalf("ledger-stop subprocess: %v\n%s", err, out)
				}
				return
			}
			srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, req agentEvalWireRequest) {
				if req.Stream {
					agentEvalStream(w, agentEvalStreamTool)
					return
				}
				// No usage: keep the first reservation counted so its
				// tool follow-up would exceed this run's allotment.
				fmt.Fprintf(w, `{"choices":[{"message":{"tool_calls":[%s]}}]}`, agentEvalTool("a", "cypher", "{}"))
			})
			f := newAgentEvalFixture(t, srv, agentEvalQuestion("stopped", name == "sse"), agentEvalQuestion("later", false))
			f.allotment = "1.1"
			wantSends := 1
			if name == "start" {
				f.allotment, wantSends = "20.1", 0
			}
			f.dispatch(t, 4)
			agentEvalRows(t, f, 0)
			if n := len(script.sent()); n != wantSends {
				t.Fatalf("provider saw %d sends, want %d", n, wantSends)
			}
			ledger := readJudgeEvalRows(t, f.ledger)
			if name == "start" {
				if len(ledger) != 1 {
					t.Fatalf("start refusal wrote ledger rows: %v", ledger)
				}
			} else if len(ledger) != 4 || ledger[2]["kind"] != "reserve" || ledger[2]["id"] != "stopped" ||
				ledger[3]["kind"] != "settle" || ledger[3]["settled_usd"] != nil {
				t.Fatalf("unexpected ledger after stop: %v", ledger)
			}
		})
	}
}

func TestEvalAgentOnlyResumeAndRefusals(t *testing.T) {
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, _ agentEvalWireRequest) {
		agentEvalReply(w, "answer")
	})
	first, other := agentEvalQuestion("warmup-first", false), agentEvalQuestion("other", false)
	f := newAgentEvalFixture(t, srv, first, other)
	f.dispatch(t, 0, "--only", "warmup-first")
	if row := agentEvalRows(t, f, 1)[0]; row["id"] != "warmup-first" {
		t.Fatalf("wrong selected row: %v", row)
	}
	f.dispatch(t, 0, "--only", "warmup-first")
	if len(script.sent()) != 1 {
		t.Fatal("resume sent an already answered id")
	}
	f.runID = "agent-block-remainder"
	f.dispatch(t, 0, "--only", "warmup-first,other")
	if rows := agentEvalRows(t, f, 2); rows[1]["id"] != "other" {
		t.Fatalf("wrong resumed rows: %v", rows)
	}

	t.Run("unknown-only", func(t *testing.T) {
		f.dispatch(t, 2, "--only", "absent")
	})
	t.Run("unsupported-effort", func(t *testing.T) {
		q := agentEvalQuestion("bad-effort", true)
		q["reasoning_effort"] = "max"
		bad := f
		bad.out, bad.questions = filepath.Join(t.TempDir(), "answers.jsonl"), filepath.Join(t.TempDir(), "questions.jsonl")
		writeJudgeEvalInput(t, bad.questions, first, q)
		if stderr := bad.dispatch(t, 2); !strings.Contains(stderr, "reasoning_effort") {
			t.Fatalf("wrong refusal: %s", stderr)
		}
		agentEvalRows(t, bad, 0)
	})
	for _, field := range []string{"arm", "model", "manifest"} {
		t.Run("foreign-"+field, func(t *testing.T) {
			bad := f
			bad.out = filepath.Join(t.TempDir(), "foreign.jsonl")
			row := agentEvalRows(t, f, 2)[0]
			row[field] = "foreign"
			writeJudgeEvalInput(t, bad.out, row)
			if stderr := bad.dispatch(t, 2); !strings.Contains(stderr, "mismatch") {
				t.Fatalf("wrong refusal: %s", stderr)
			}
			agentEvalRows(t, bad, 1)
		})
	}
	t.Run("ledger-model", func(t *testing.T) {
		bad := f
		bad.ledger, bad.out = filepath.Join(t.TempDir(), "ledger.jsonl"), filepath.Join(t.TempDir(), "answers.jsonl")
		bad.setLedger(t, "DS", "foreign-model")
		bad.dispatch(t, 2)
		if _, err := os.Stat(bad.out); !os.IsNotExist(err) {
			t.Fatalf("refused ledger created output: %v", err)
		}
	})
	t.Run("arm-auth", func(t *testing.T) {
		f.dispatch(t, 2, "--arm", "GM")
	})
	if len(script.sent()) != 2 {
		t.Fatalf("refused invocation sent to the provider: %d total sends", len(script.sent()))
	}
}

func TestEvalAgentNoBootstrap(t *testing.T) {
	srv, _ := startAgentEvalProvider(t, func(w http.ResponseWriter, i int, _ agentEvalWireRequest) {
		if i == 0 {
			agentEvalReply(w, "", agentEvalTool("a", "cypher", `{"query":"RETURN 42 AS value"}`))
			return
		}
		agentEvalReply(w, "read")
	})
	f := newAgentEvalFixture(t, srv, agentEvalQuestion("read-only", false))
	// Ensure a Bootstrap call would visibly change the graph even when
	// another test in this package already bootstrapped this database.
	for _, row := range migrationQuery(t, "SHOW INDEX INFO", nil) {
		label, _ := row.Get("label")
		props, _ := row.Get("property")
		if label == "Document" && (props == "domain" || reflect.DeepEqual(props, []any{"domain"})) {
			migrationQuery(t, "DROP INDEX ON :Document(domain)", nil)
			t.Cleanup(func() { migrationQuery(t, "CREATE INDEX ON :Document(domain)", nil) })
			break
		}
	}
	t.Setenv("GKG_EMBEDDER_ENABLED", "true")
	t.Setenv("GKG_EMBEDDER_PROVIDER", "openai")
	t.Setenv("GKG_EMBEDDER_BASE_URL", srv.URL)
	before := graphtest.Snapshot(t)
	f.dispatch(t, 0)
	row := agentEvalRows(t, f, 1)[0]
	if row["answer"] != "read" || row["tool_errors"] != float64(0) {
		t.Fatalf("read-only ask failed: %v", row)
	}
	before.AssertUnchanged(t, "eval-agent")
}

func TestEvalAgentAnswerFixtureShape(t *testing.T) {
	const signatureTool = `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"cypher","arguments":"{\"query\":\"RETURN 42\"}"},"extra_content":{"signature":"synthetic"}}]}}]}`
	const usage = `{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`
	srv, _ := startAgentEvalProvider(t, func(w http.ResponseWriter, i int, _ agentEvalWireRequest) {
		switch i {
		case 0:
			agentEvalStream(w, signatureTool, usage)
		case 1:
			agentEvalStream(w, agentEvalStreamAnswer, usage)
		case 2:
			http.Error(w, "rejected", 404)
		case 3:
			agentEvalStream(w, agentEvalStreamAnswer, `{"error":{"message":"in-band failure"}}`)
		default:
			agentEvalReply(w, "plain")
		}
	})
	f := newAgentEvalFixture(t, srv, agentEvalQuestion("stream", true), agentEvalQuestion("failed", false),
		agentEvalQuestion("failed-sse", true), agentEvalQuestion("plain", false))
	f.dispatch(t, 0)
	rows := agentEvalRows(t, f, 4)
	if rows[1]["error_class"] != "provider_4xx" || rows[1]["answer"] != "" {
		t.Fatalf("model failure was not recorded: %v", rows[1])
	}
	if row := rows[2]; row["error_class"] != "provider_5xx" || row["answer"] != "" ||
		!reflect.DeepEqual(row["sse"], map[string]any{"tool": float64(0), "delta": float64(1), "done": float64(0), "error": float64(1), "last": "error"}) ||
		agentEvalCalls(t, row, 1)[0]["status"] != float64(200) {
		t.Fatalf("SSE in-band failure was not recorded: %v", row)
	}
	fixture := readJudgeEvalRows(t, "testdata/eval-agent-answer.jsonl")[0]
	data, err := os.ReadFile(f.out)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
		var row map[string]any
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&row); err != nil {
			t.Fatal(err)
		}
		assertAgentEvalJSONShape(t, fmt.Sprintf("row %d", i), row, fixture)
	}
}

func TestEvalAgentGCPPermanentAuth(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%t", sse), func(t *testing.T) {
			srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, i int, _ agentEvalWireRequest) {
				if i == 0 {
					agentEvalReply(w, "before failure")
					return
				}
				http.Error(w, "credential rejected", 401)
			})
			f := newAgentEvalFixture(t, srv, agentEvalQuestion("before", false), agentEvalQuestion("auth-failure", sse), agentEvalQuestion("later", false))
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Agent.Auth, cfg.Agent.APIKey, cfg.Agent.Model = "gcp", "", "google/gemini-3.5-flash-lite"
			f.setLedger(t, "GM", cfg.Agent.Model)
			opt := evalAgentOptions{
				questions: f.questions, out: f.out, arm: "GM", family: "A", manifest: f.manifest,
				ledger: f.ledger, genesis: f.genesis, runID: f.runID, allotment: f.allotment,
			}
			var stderr bytes.Buffer
			if code := runEvalAgentRun(t.Context(), opt, cfg, &stderr, agent.WithEvalTokenProvider(evalJudgeToken{})); code != 2 {
				t.Fatalf("exit = %d, want 2 (%s)", code, &stderr)
			}
			rows := agentEvalRows(t, f, 2)
			if len(script.sent()) != 2 || rows[0]["answer"] != "before failure" || rows[1]["id"] != "auth-failure" ||
				rows[1]["arm"] != "GM" || rows[1]["model"] != cfg.Agent.Model || rows[1]["answer"] != "" || rows[1]["error_class"] != "auth" ||
				agentEvalCalls(t, rows[1], 1)[0]["status"] != float64(401) {
				t.Fatalf("unexpected GM auth outcome: %v", rows)
			}
			ledger := readJudgeEvalRows(t, f.ledger)
			if ledger[len(ledger)-1]["kind"] == "close" {
				t.Fatal("auth refusal closed an incomplete run")
			}
		})
	}
}

func assertAgentEvalJSONShape(t *testing.T, path string, actual, fixture any) {
	t.Helper()
	switch sample := fixture.(type) {
	case map[string]any:
		if actual == nil && (strings.HasSuffix(path, ".sse") || strings.HasSuffix(path, ".usage")) {
			return
		}
		obj, ok := actual.(map[string]any)
		if !ok || len(obj) != len(sample) {
			t.Fatalf("%s: got %T with %d keys, want object with %d keys", path, actual, len(obj), len(sample))
		}
		for key, value := range sample {
			got, exists := obj[key]
			if !exists {
				t.Fatalf("%s missing key %s", path, key)
			}
			child := path + "." + key
			switch key {
			case "arguments": // Raw JSON with provider-defined keys.
				if _, ok := got.(map[string]any); !ok {
					t.Fatalf("%s = %T, want object for these scripted arguments", child, got)
				}
			case "signature_echoed":
				if _, ok := got.(bool); got != nil && !ok {
					t.Fatalf("%s = %T, want boolean or null", child, got)
				}
			case "error_class":
				if _, ok := got.(string); got != nil && !ok {
					t.Fatalf("%s = %T, want string or null", child, got)
				}
			default:
				assertAgentEvalJSONShape(t, child, got, value)
			}
		}
	case []any:
		items, ok := actual.([]any)
		if !ok {
			t.Fatalf("%s = %T, want array", path, actual)
		}
		for i, item := range items {
			assertAgentEvalJSONShape(t, fmt.Sprintf("%s[%d]", path, i), item, sample[0])
		}
	case float64:
		if n, ok := actual.(json.Number); !ok {
			t.Fatalf("%s = %T, want integer", path, actual)
		} else if _, err := strconv.ParseInt(string(n), 10, 64); err != nil {
			t.Fatalf("%s = %s, want integer", path, n)
		}
	default:
		if reflect.TypeOf(actual) != reflect.TypeOf(fixture) {
			t.Fatalf("%s = %T, want %T", path, actual, fixture)
		}
	}
}
