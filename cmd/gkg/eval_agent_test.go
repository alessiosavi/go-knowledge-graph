package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/llmauth"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
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

func (f agentEvalFixture) args(flags ...string) []string {
	args := []string{"eval-agent", "--questions", f.questions, "--out", f.out, "--arm", "DS", "--manifest", f.manifest,
		"--ledger", f.ledger, "--ledger-genesis", f.genesis, "--run-id", f.runID, "--allotment", f.allotment}
	return append(args, flags...)
}

func (f agentEvalFixture) dispatch(t *testing.T, want int, flags ...string) string {
	t.Helper()
	var stderr bytes.Buffer
	if code := run(f.args(flags...), io.Discard, &stderr); code != want {
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

const evalAnswerTestDomain = "eval-agent-answer"

func seedEvalAnswerDomain(t *testing.T) {
	t.Helper()
	t.Setenv("GKG_AGENT_ANSWER_ENABLED", "true")
	params := map[string]any{"domain": evalAnswerTestDomain}
	prior := migrationQuery(t, `MATCH (s:ProductStore {store:'shop.example'}) RETURN s.domain AS domain`, nil)
	migrationQuery(t, `CREATE (:Domain:EvalAnswerFixture {id:$domain})
		MERGE (s:ProductStore {store:'shop.example'}) ON CREATE SET s:EvalAnswerFixture
		SET s.domain=$domain`, params)
	t.Cleanup(func() { migrationQuery(t, `MATCH (n:EvalAnswerFixture) DETACH DELETE n`, nil) })
	if len(prior) != 0 {
		domain, _ := prior[0].Get("domain")
		t.Cleanup(func() {
			migrationQuery(t, `MATCH (s:ProductStore {store:'shop.example'}) SET s.domain=$domain`, map[string]any{"domain": domain})
		})
	}
}

func TestEvalAgentAnswerRouteRow(t *testing.T) {
	const (
		product = "shop.example:eval-answer-lamp"
		reply   = "Pick the lamp [[" + product + "]] [[eval-answer-c0]]."
	)
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, i int, _ agentEvalWireRequest) {
		if i == 0 {
			agentEvalReply(w, "", agentEvalTool("product", "get_product", `{"id":"`+productobs.ProductID("shop.example", product)+`"}`),
				agentEvalTool("passage", "get_chunks", `{"ids":["eval-answer-c0"],"with_products":false}`))
			return
		}
		agentEvalReply(w, reply)
	})
	q := agentEvalQuestion("answer", false)
	q["domain"], q["reasoning_effort"] = evalAnswerTestDomain, "low"
	delete(q, "sse")
	unknown := agentEvalQuestion("unknown", false)
	unknown["domain"] = "eval-unknown-domain"
	foreign := agentEvalQuestion("foreign", false)
	foreign["domain"] = evalAnswerTestDomain
	foreign["context"] = map[string]any{"product_ids": []string{"other.example:lamp"}}
	malformed := agentEvalQuestion("malformed", false)
	malformed["domain"], malformed["context"] = evalAnswerTestDomain, "invalid context type"
	f := newAgentEvalFixture(t, srv, q, agentEvalQuestion("missing", false), unknown, foreign, malformed)
	seedEvalAnswerDomain(t)
	migrationQuery(t, `CREATE (:Product:EvalAnswerFixture {id:$id, domain:$domain, store:'shop.example', product_id:$product, title:'Fixture Lamp'}),
		(d:Document:EvalAnswerFixture {id:'eval-answer-doc', domain:$domain, title:'Fixture Manual', source:'manual'}),
		(d)-[:HAS_CHUNK]->(:Chunk:EvalAnswerFixture {id:'eval-answer-c0', doc_id:d.id, domain:$domain, seq:0, text:$text})`,
		map[string]any{"id": productobs.ProductID("shop.example", product), "domain": evalAnswerTestDomain,
			"product": product, "text": strings.Repeat("x", 5<<10)})

	f.dispatch(t, 0, "--route", "answer")
	rows := agentEvalRows(t, f, 5)
	row := rows[0]
	if row["id"] != "answer" || row["route"] != "answer" || row["arm"] != "DS" || row["model"] != agentEvalTestModel ||
		row["manifest"] != f.manifest || row["http_status"] != float64(200) || row["pre_cleaning_reply"] != reply {
		t.Fatalf("incorrect answer capture metadata: id=%v route=%v arm=%v model=%v manifest=%v status=%v reply=%q",
			row["id"], row["route"], row["arm"], row["model"], row["manifest"], row["http_status"], row["pre_cleaning_reply"])
	}
	if ms, ok := row["ms"].(float64); !ok || ms < 0 {
		t.Fatalf("invalid elapsed milliseconds: %v", row["ms"])
	}
	response := row["response"].(map[string]any)
	if response["summary"] != "Pick the lamp." || response["contract"] != "gkg-answer/1" ||
		response["domain"] != evalAnswerTestDomain || response["complete"] != true || len(response) != 12 {
		t.Fatal("capture does not contain the full cleaned answer response")
	}
	if alternatives, ok := response["alternatives"].([]any); !ok || len(alternatives) != 0 {
		t.Fatalf("alternatives = %v, want []", response["alternatives"])
	}
	products, passages := response["products"].([]any), response["passages"].([]any)
	if len(products) != 1 || products[0].(map[string]any)["product_id"] != product || products[0].(map[string]any)["title"] != "Fixture Lamp" ||
		len(passages) != 1 || passages[0].(map[string]any)["document_title"] != "Fixture Manual" {
		t.Fatalf("hydration failed: %d cards, %d passages", len(products), len(passages))
	}
	requests := script.sent()
	if len(requests) != 2 || requests[0].ReasoningEffort != "low" || requests[1].ReasoningEffort != "low" || requests[0].Stream {
		t.Fatalf("expected two non-streaming metered requests with reasoning effort low, got %d requests", len(requests))
	}
	ledger := readJudgeEvalRows(t, f.ledger)
	if len(ledger) != 7 || ledger[2]["id"] != "answer" || ledger[4]["id"] != "answer" || ledger[6]["kind"] != "close" {
		t.Fatalf("answer calls did not use the evaluation ledger: %v", ledger)
	}
	evidence := row["visible_evidence"].([]any)
	if len(evidence) != 2 {
		t.Fatalf("visible evidence entries = %d, want 2", len(evidence))
	}
	var transcript []string
	for _, msg := range requests[1].Messages {
		if msg.Role == "tool" {
			transcript = append(transcript, msg.Content)
		}
	}
	if len(transcript) != len(evidence) {
		t.Fatalf("transcript contains %d tool messages, want %d", len(transcript), len(evidence))
	}
	for i, tool := range []string{"get_product", "get_chunks"} {
		item := evidence[i].(map[string]any)
		if item["tool"] != tool || item["error"] != false || item["visible"] != transcript[i] {
			t.Errorf("evidence for %s differs from the model-visible tool message", tool)
		}
	}
	if !strings.Contains(transcript[1], `"truncated":true`) || !strings.Contains(transcript[1], strings.Repeat("x", 4<<10)+`"`) {
		t.Error("chunk transcript did not retain the capped text")
	}
	for i, want := range []map[string]any{
		{"error": "invalid_request", "detail": "domain_required"},
		{"error": "invalid_request", "detail": "unknown_domain"},
		{"error": "invalid_context", "detail": "context product IDs must have a valid store prefix bound to the domain and be at most 1 KiB"},
		{"error": "invalid_json", "detail": "json: cannot unmarshal string into Go struct field answerHTTPRequest.context of type agent.answerHTTPContext"},
	} {
		row := rows[i+1]
		if row["http_status"] != float64(400) || !reflect.DeepEqual(row["response"], want) ||
			row["pre_cleaning_reply"] != "" || !reflect.DeepEqual(row["visible_evidence"], []any{}) {
			t.Errorf("invalid question row = %v, want HTTP 400 with %v and no model output", row, want)
		}
	}
	f.dispatch(t, 0, "--route", "answer")
	if len(script.sent()) != 2 || !reflect.DeepEqual(agentEvalRows(t, f, 5), rows) || !reflect.DeepEqual(readJudgeEvalRows(t, f.ledger), ledger) {
		t.Fatal("same-route resume rewrote rows, changed the closed ledger or reached the provider")
	}
}

func TestEvalAgentAnswerContextForwarded(t *testing.T) {
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, _ agentEvalWireRequest) {
		agentEvalReply(w, "I can compare them.")
	})
	q := agentEvalQuestion("context", true)
	q["domain"] = evalAnswerTestDomain
	q["context"] = agent.AnswerContext{
		ProductIDs: []string{"shop.example:lamp", "shop.example:shade"},
		History:    []agent.HistoryTurn{{Role: "user", Text: "Find a lamp and shade."}, {Role: "assistant", Text: "I found both."}},
	}
	f := newAgentEvalFixture(t, srv, q)
	seedEvalAnswerDomain(t)
	f.dispatch(t, 0, "--route", "answer")
	requests := script.sent()
	if len(requests) != 1 || requests[0].Stream || len(requests[0].Messages) != 3 {
		t.Fatalf("expected a single non-streaming answer request with context, got %v", requests)
	}
	msg := requests[0].Messages[1]
	const want = "Conversation context (same shop):\nProduct IDs:\nshop.example:lamp\nshop.example:shade\nHistory:\nuser: Find a lamp and shade.\nassistant: I found both."
	if msg.Role != "user" || msg.Content != want {
		t.Fatalf("context message = %q %q, want user %q", msg.Role, msg.Content, want)
	}
}

func TestEvalAgentAnswerDisabledFailsFast(t *testing.T) {
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, _ agentEvalWireRequest) {
		http.Error(w, "unexpected provider send", http.StatusBadRequest)
	})
	for _, tc := range []struct {
		name, flag, setting string
	}{
		{"agent-disabled", "GKG_AGENT_ENABLED", "agent.enabled=false"},
		{"answer-disabled", "GKG_AGENT_ANSWER_ENABLED", "agent.answer_enabled=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := agentEvalQuestion("disabled", false)
			q["domain"] = evalAnswerTestDomain
			f := newAgentEvalFixture(t, srv, q)
			t.Setenv("GKG_AGENT_ANSWER_ENABLED", "true")
			t.Setenv(tc.flag, "false")
			before, err := os.ReadFile(f.ledger)
			if err != nil {
				t.Fatal(err)
			}

			var stderr bytes.Buffer
			if code := run(f.args("--route", "answer"), io.Discard, &stderr); code == 0 || !strings.Contains(stderr.String(), tc.setting) {
				t.Errorf("exit = %d, stderr = %q, want a refusal naming %s", code, stderr.String(), tc.setting)
			}
			if _, err := os.Stat(f.out); !os.IsNotExist(err) {
				t.Errorf("want absent output file, stat returned %v", err)
			}
			after, err := os.ReadFile(f.ledger)
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("disabled answer changed the ledger: %s (%v)", after, err)
			}
		})
	}
	if len(script.sent()) != 0 {
		t.Fatal("disabled answer reached the provider")
	}
}

func TestEvalAgentAnswerPermanentAuthNotRecorded(t *testing.T) {
	const id = "retry-question"
	const reply = "A lamp is available."
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, i int, _ agentEvalWireRequest) {
		if i == 0 {
			http.Error(w, "credential rejected", http.StatusUnauthorized)
			return
		}
		agentEvalReply(w, reply)
	})
	q := agentEvalQuestion(id, false)
	q["domain"] = evalAnswerTestDomain
	f := newAgentEvalFixture(t, srv, q)
	seedEvalAnswerDomain(t)

	var stderr bytes.Buffer
	if code := run(f.args("--route", "answer"), io.Discard, &stderr); code == 0 || !strings.Contains(stderr.String(), "permanent") ||
		!strings.Contains(stderr.String(), "auth") {
		t.Errorf("exit = %d, stderr = %q, want a permanent authentication failure", code, stderr.String())
	}
	if rows := readJudgeEvalRows(t, f.out); len(rows) != 0 {
		t.Errorf("permanent failure recorded %d answer rows, want 0", len(rows))
	}
	if ledger := readJudgeEvalRows(t, f.ledger); len(ledger) != 4 || ledger[1]["kind"] != "allot" ||
		ledger[2]["kind"] != "reserve" || ledger[2]["id"] != id || ledger[2]["attempt"] != float64(1) ||
		ledger[3]["kind"] != "settle" || ledger[3]["id"] != id || ledger[3]["attempt"] != float64(1) {
		t.Fatalf("want the failed attempt retained in an open run, got %v", ledger)
	}

	f.dispatch(t, 0, "--route", "answer")
	row := agentEvalRows(t, f, 1)[0]
	if n := len(script.sent()); n != 2 || row["id"] != id || row["http_status"] != float64(http.StatusOK) || row["pre_cleaning_reply"] != reply {
		t.Fatalf("want a successful resumed answer after two provider sends, got %d sends and row %v", n, row)
	}
	if ledger := readJudgeEvalRows(t, f.ledger); len(ledger) != 7 || ledger[4]["kind"] != "reserve" || ledger[4]["id"] != id ||
		ledger[4]["attempt"] != float64(2) || ledger[5]["kind"] != "settle" || ledger[6]["kind"] != "close" {
		t.Fatalf("want a second attempt and a closed run after resume, got %v", ledger)
	}
}

func TestEvalAgentResumeRefusesOtherRoute(t *testing.T) {
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, _ agentEvalWireRequest) {
		http.Error(w, "unexpected request", http.StatusBadRequest)
	})
	for _, tc := range []struct{ existing, requested string }{{"", "answer"}, {"ask", "answer"}, {"answer", "ask"}} {
		t.Run(fmt.Sprintf("%q-to-%s", tc.existing, tc.requested), func(t *testing.T) {
			q := agentEvalQuestion("resume", false)
			q["domain"] = evalAnswerTestDomain
			f := newAgentEvalFixture(t, srv, q)
			t.Setenv("GKG_AGENT_ANSWER_ENABLED", "true")
			row := map[string]any{"id": "resume", "arm": "DS", "model": agentEvalTestModel, "manifest": f.manifest}
			if tc.existing != "" {
				row["route"] = tc.existing
			}
			writeJudgeEvalInput(t, f.out, row)
			if stderr := f.dispatch(t, 2, "--route", tc.requested); !strings.Contains(stderr, "resume answers: line 1: answer route mismatch") {
				t.Fatalf("wrong route refusal: %s", stderr)
			}
			if !reflect.DeepEqual(agentEvalRows(t, f, 1)[0], row) || len(readJudgeEvalRows(t, f.ledger)) != 1 {
				t.Fatal("refusal changed the answer file or opened a ledger run")
			}
		})
	}
	if len(script.sent()) != 0 {
		t.Fatal("refused invocation reached the provider")
	}
}

func TestEvalAgentAskRouteUnchanged(t *testing.T) {
	srv, _ := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, _ agentEvalWireRequest) {
		agentEvalReply(w, "Legacy answer.")
	})
	q := agentEvalQuestion("legacy", false)
	q["context"] = "ignored by ask"
	timing := regexp.MustCompile(`"latency_ms":[0-9]+`)
	baseline := `{"id":"legacy","arm":"DS","model":%q,"manifest":%q,"answer":"Legacy answer.","steps":0,"latency_ms":0,"error_class":null,"evidence":[],"tool_errors":0,"unknown_tools":0,"calls":[{"ask_id":"legacy","host":%q,"status":200,"latency_ms":0,"stream":false,"tools":true,"response_format":"","tool_calls":0,"signature_in":false,"signature_echoed":null,"usage":{"prompt":10,"completion":2,"reasoning":3,"total":15}}],"sse":null,"discarded_extra_content":0}` + "\n"
	for _, route := range []string{"default", "ask"} {
		t.Run(route, func(t *testing.T) {
			f := newAgentEvalFixture(t, srv, q)
			if route == "default" {
				f.dispatch(t, 0)
			} else {
				f.dispatch(t, 0, "--route", route)
			}
			data, err := os.ReadFile(f.out)
			if err != nil {
				t.Fatal(err)
			}
			if matches := timing.FindAll(data, -1); len(matches) != 2 {
				t.Fatalf("want row and call timings, got %q", matches)
			}
			got := timing.ReplaceAllString(string(data), `"latency_ms":0`)
			if want := fmt.Sprintf(baseline, agentEvalTestModel, f.manifest, strings.TrimPrefix(srv.URL, "http://")); got != want {
				t.Fatalf("ask JSONL changed\n got: %s\nwant: %s", got, want)
			}
		})
	}
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

func TestEvalAgentBlocks(t *testing.T) {
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, _ agentEvalWireRequest) {
		agentEvalReply(w, "answer")
	})
	effort := agentEvalQuestion("E-minimal", true)
	effort["stratum"], effort["reasoning_effort"] = "E", "minimal"
	first := newAgentEvalFixture(t, srv, agentEvalQuestion("a", false), effort, agentEvalQuestion("b", false))
	t.Setenv("GKG_AGENT_REASONING_EFFORTS", "")
	first.runID = "R1"
	second := first
	second.runID, second.out = "R2", filepath.Join(t.TempDir(), "answers.jsonl")

	before := readJudgeEvalRows(t, first.ledger)
	for _, block := range []struct {
		f  agentEvalFixture
		id string
	}{{first, "a"}, {second, "b"}} {
		block.f.dispatch(t, 0, "--only", block.id)
		if row := agentEvalRows(t, block.f, 1)[0]; row["id"] != block.id {
			t.Fatalf("wrong selected row: %v", row)
		}
		after := readJudgeEvalRows(t, block.f.ledger)
		if len(after) != len(before)+4 || !reflect.DeepEqual(after[:len(before)], before) {
			t.Fatalf("block %s did not append four rows to the existing ledger: %v", block.f.runID, after)
		}
		for i, kind := range []string{"allot", "reserve", "settle", "close"} {
			if row := after[len(before)+i]; row["kind"] != kind || row["run_id"] != block.f.runID {
				t.Fatalf("block %s: want %s row, got %v", block.f.runID, kind, row)
			}
		}
		before = after
	}
	sent := script.sent()
	if len(sent) != 2 {
		t.Fatalf("block flow sent %d requests, want 2", len(sent))
	}
	for i, id := range []string{"a", "b"} {
		if len(sent[i].Messages) < 2 || sent[i].Messages[1].Content != "Question "+id {
			t.Fatalf("request %d did not ask question %q", i, id)
		}
	}
}

func TestEvalAgentInterruptedBlock(t *testing.T) {
	const child = "GKG_TB_TEST_INTERRUPTED_BLOCK"
	if os.Getenv(child) == "1" {
		var stderr bytes.Buffer
		if code := run(flag.Args(), io.Discard, &stderr); code != 4 || !strings.Contains(stderr.String(), "eval ledger budget exhausted") {
			t.Fatalf("exit = %d, want ledger stop (4): %s", code, &stderr)
		}
		return
	}
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, i int, _ agentEvalWireRequest) {
		if i == 1 {
			// The follow-up carries this transcript and exceeds the allotment.
			// The first turn settles cheaply, so a fresh ask for b still fits.
			args := fmt.Sprintf(`{"payload":%q}`, strings.Repeat("x", 128<<10))
			agentEvalReply(w, "", agentEvalTool("large", "nonexistent", args))
			return
		}
		agentEvalReply(w, "answer")
	})
	f := newAgentEvalFixture(t, srv, agentEvalQuestion("a", false), agentEvalQuestion("b", false), agentEvalQuestion("c", false))
	f.runID, f.allotment = "R1", "1.1"
	flags := []string{"--only", "a,b"}
	// A ledger stop disables metered sends for the rest of the process.
	args := append([]string{"-test.run=^TestEvalAgentInterruptedBlock$", "--"}, f.args(flags...)...)
	cmd := exec.CommandContext(t.Context(), os.Args[0], args...)
	cmd.Env = append(os.Environ(), child+"=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ledger-stop subprocess: %v\n%s", err, out)
	}
	first := agentEvalRows(t, f, 1)[0]
	if first["id"] != "a" || len(script.sent()) != 2 {
		t.Fatalf("interrupted block left row %v and sent %d requests", first, len(script.sent()))
	}
	before := readJudgeEvalRows(t, f.ledger)
	if len(before) != 6 || before[5]["id"] != "b" {
		t.Fatalf("wrong ledger after stop: %v", before)
	}
	for i, kind := range []string{"allot", "reserve", "settle", "reserve", "settle"} {
		if row := before[i+1]; row["kind"] != kind || row["run_id"] != f.runID {
			t.Fatalf("interrupted block: want %s row, got %v", kind, row)
		}
	}

	f.dispatch(t, 0, flags...)
	rows := agentEvalRows(t, f, 2)
	sent := script.sent()
	if !reflect.DeepEqual(rows[0], first) || rows[1]["id"] != "b" || len(sent) != 3 ||
		len(sent[2].Messages) < 2 || sent[2].Messages[1].Content != "Question b" {
		t.Fatalf("resume wrote %v and sent %d requests", rows, len(sent))
	}
	after := readJudgeEvalRows(t, f.ledger)
	if len(after) != len(before)+3 || !reflect.DeepEqual(after[:len(before)], before) ||
		after[6]["kind"] != "reserve" || after[6]["id"] != "b" || after[6]["attempt"] != float64(2) ||
		after[7]["kind"] != "settle" || after[7]["id"] != "b" ||
		after[8]["kind"] != "close" || after[8]["run_id"] != f.runID {
		t.Fatalf("wrong ledger after resume: %v", after)
	}
}

func TestEvalAgentAlreadyAnsweredBlock(t *testing.T) {
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, _ agentEvalWireRequest) {
		http.Error(w, "unexpected provider send", http.StatusBadRequest)
	})
	f := newAgentEvalFixture(t, srv, agentEvalQuestion("a", false), agentEvalQuestion("b", false))
	answer := map[string]any{"id": "a", "arm": "DS", "model": agentEvalTestModel, "manifest": f.manifest}
	writeJudgeEvalInput(t, f.out, answer)

	f.dispatch(t, 0, "--only", "a")
	before := readJudgeEvalRows(t, f.ledger)
	if len(before) != 3 || before[1]["kind"] != "allot" || before[2]["kind"] != "close" || before[2]["run_id"] != f.runID {
		t.Fatalf("already answered block did not close: %v", before)
	}
	f.dispatch(t, 0, "--only", "a")
	if len(script.sent()) != 0 || !reflect.DeepEqual(readJudgeEvalRows(t, f.ledger), before) ||
		!reflect.DeepEqual(agentEvalRows(t, f, 1)[0], answer) {
		t.Fatal("already answered block changed the answers or closed ledger, or sent to the provider")
	}
}

func TestEvalAgentRefusals(t *testing.T) {
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, _ agentEvalWireRequest) {
		http.Error(w, "unexpected provider send", http.StatusBadRequest)
	})
	first := agentEvalQuestion("warmup-first", false)
	f := newAgentEvalFixture(t, srv, first)
	t.Run("unknown-only", func(t *testing.T) {
		f.dispatch(t, 2, "--only", "absent")
	})
	t.Run("unsupported-effort", func(t *testing.T) {
		t.Setenv("GKG_AGENT_REASONING_EFFORTS", "")
		q := agentEvalQuestion("E-minimal", true)
		q["stratum"], q["reasoning_effort"] = "E", "minimal"
		bad := f
		bad.out, bad.questions = filepath.Join(t.TempDir(), "answers.jsonl"), filepath.Join(t.TempDir(), "questions.jsonl")
		writeJudgeEvalInput(t, bad.questions, first, q)
		before := readJudgeEvalRows(t, bad.ledger)
		for _, tc := range []struct {
			name  string
			flags []string
		}{
			{"with-only", []string{"--only", "warmup-first,E-minimal"}},
			{"without-only", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if stderr := bad.dispatch(t, 2, tc.flags...); !strings.Contains(stderr, `line 2: ask "E-minimal": unsupported reasoning_effort`) {
					t.Fatalf("wrong refusal: %s", stderr)
				}
				agentEvalRows(t, bad, 0)
				if after := readJudgeEvalRows(t, bad.ledger); !reflect.DeepEqual(after, before) {
					t.Fatalf("question refusal changed the ledger: %v", after)
				}
			})
		}
	})
	for _, field := range []string{"arm", "model", "manifest"} {
		t.Run("foreign-"+field, func(t *testing.T) {
			bad := f
			bad.out = filepath.Join(t.TempDir(), "foreign.jsonl")
			row := map[string]any{"id": "warmup-first", "arm": "DS", "model": agentEvalTestModel, "manifest": f.manifest}
			row[field] = "foreign"
			writeJudgeEvalInput(t, bad.out, row)
			if stderr := bad.dispatch(t, 2); !strings.Contains(stderr, "mismatch") {
				t.Fatalf("wrong refusal: %s", stderr)
			}
			agentEvalRows(t, bad, 1)
		})
	}
	t.Run("arm-auth", func(t *testing.T) {
		f.dispatch(t, 2, "--arm", "GM")
	})
	if len(script.sent()) != 0 {
		t.Fatalf("refused invocation sent to the provider: %d total sends", len(script.sent()))
	}
}

func TestEvalAgentOutputBeforeLedger(t *testing.T) {
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, _ agentEvalWireRequest) {
		http.Error(w, "unexpected provider send", http.StatusBadRequest)
	})
	t.Run("output-create", func(t *testing.T) {
		f := newAgentEvalFixture(t, srv, agentEvalQuestion("one", false))
		f.out = filepath.Join(filepath.Dir(f.out), "missing", "answers.jsonl")
		before := readJudgeEvalRows(t, f.ledger)
		if stderr := f.dispatch(t, 2); !strings.Contains(stderr, "create answers") {
			t.Fatalf("wrong refusal: %s", stderr)
		}
		if after := readJudgeEvalRows(t, f.ledger); !reflect.DeepEqual(after, before) {
			t.Fatalf("output creation failure changed the ledger: %v", after)
		}
	})
	t.Run("ledger-model", func(t *testing.T) {
		f := newAgentEvalFixture(t, srv, agentEvalQuestion("one", false))
		f.setLedger(t, "DS", "foreign-model")
		if stderr := f.dispatch(t, 2); !strings.Contains(stderr, "model mismatch") {
			t.Fatalf("wrong refusal: %s", stderr)
		}
		if info, err := os.Stat(f.out); err != nil || info.Size() != 0 {
			t.Fatalf("ledger refusal should leave an empty answers file: %v, %v", info, err)
		}
		if rows := readJudgeEvalRows(t, f.ledger); len(rows) != 1 {
			t.Fatalf("ledger refusal wrote rows: %v", rows)
		}
	})
	if len(script.sent()) != 0 {
		t.Fatal("refused invocation sent to the provider")
	}
}

func TestEvalAgentOutputLedgerAlias(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(oldname, newname string) error
	}{{name: "same-path"}, {"hard-link", os.Link}, {"symlink", os.Symlink}} {
		t.Run(tc.name, func(t *testing.T) {
			const child = "GKG_TB_TEST_LEDGER_ALIAS"
			if os.Getenv(child) != tc.name {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEvalAgentOutputLedgerAlias$/^"+tc.name+"$", "-test.v")
				cmd.Env = append(os.Environ(), child+"="+tc.name)
				out, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("alias refusal timed out: %v", ctx.Err())
				}
				if err != nil || bytes.Contains(out, []byte("--- SKIP:")) {
					t.Fatalf("alias subprocess: %v\n%s", err, out)
				}
				return
			}
			dir := t.TempDir()
			opt := evalAgentOptions{
				questions: filepath.Join(dir, "questions.jsonl"), out: filepath.Join(dir, "answers.jsonl"),
				ledger: filepath.Join(dir, "ledger.jsonl"), arm: "DS", family: "A",
				manifest: strings.Repeat("a", 64), runID: "alias", allotment: "10",
			}
			cfg := config.Config{Agent: config.AgentConfig{
				Enabled: true, Auth: config.AuthStatic, Model: agentEvalTestModel, BaseURL: "http://127.0.0.1:1",
			}}
			writeJudgeEvalInput(t, opt.questions, agentEvalQuestion("one", false))
			// Empty answers pass resume parsing and reach the conflicting lock.
			opt.genesis = fmt.Sprintf("%x", sha256.Sum256(nil))
			if err := os.WriteFile(opt.ledger, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if tc.link == nil {
				opt.out = opt.ledger
			} else if err := tc.link(opt.ledger, opt.out); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			if code := runEvalAgentRun(t.Context(), opt, cfg, &stderr); code != 2 ||
				!strings.Contains(stderr.String(), "answer output and ledger must be different files") {
				t.Fatalf("wrong alias refusal: exit %d, %s", code, &stderr)
			}
			if len(readJudgeEvalRows(t, opt.ledger)) != 0 {
				t.Fatal("alias refusal wrote to the ledger")
			}
		})
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

type evalAgentFailingToken struct{}

func (evalAgentFailingToken) Token(context.Context) (string, time.Time, error) {
	return "", time.Time{}, &llmauth.AuthTransient{Detail: "fixture token unavailable"}
}

func TestEvalAgentSSEPreflightFailure(t *testing.T) {
	srv, script := startAgentEvalProvider(t, func(w http.ResponseWriter, _ int, _ agentEvalWireRequest) {
		http.Error(w, "unexpected provider send", http.StatusBadRequest)
	})
	f := newAgentEvalFixture(t, srv, agentEvalQuestion("preflight", true))
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
	if code := runEvalAgentRun(t.Context(), opt, cfg, &stderr, agent.WithEvalTokenProvider(evalAgentFailingToken{})); code != 0 {
		t.Fatalf("exit = %d, want 0 (%s)", code, &stderr)
	}
	row := agentEvalRows(t, f, 1)[0]
	wantSSE := map[string]any{"tool": float64(0), "delta": float64(0), "done": float64(0), "error": float64(1), "last": "error"}
	if row["error_class"] != "auth" || row["answer"] != "" || !reflect.DeepEqual(row["sse"], wantSSE) {
		t.Fatalf("unexpected SSE preflight failure: %v", row)
	}
	agentEvalCalls(t, row, 0)
	if len(script.sent()) != 0 {
		t.Fatal("preflight failure sent to the provider")
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
