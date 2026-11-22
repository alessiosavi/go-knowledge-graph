package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
	"github.com/alessiosavi/go-knowledge-graph/internal/llmusage"
)

const evalTestModel = "deepseek-v4.1-flash"

func newJudgeEvalFixture(t *testing.T, arm string) judgeEvalOptions {
	t.Helper()
	dir := t.TempDir()
	opt := judgeEvalOptions{
		in: filepath.Join(dir, "in.jsonl"), out: filepath.Join(dir, "out.jsonl"), arm: arm, family: "J",
		manifest: strings.Repeat("a", 64), ledger: filepath.Join(dir, "ledger.jsonl"), runID: "judge", allotment: "10",
	}
	setJudgeEvalLedger(t, &opt, 1)
	writeJudgeEvalInput(t, opt.in, frozenEvalRow("one"), frozenEvalRow("two"))
	return opt
}

func setJudgeEvalLedger(t *testing.T, opt *judgeEvalOptions, priceOut float64) {
	t.Helper()
	line := fmt.Sprintf("{\"ledger\":\"gkg-g1t2\",\"provider\":%q,\"model\":%q,\"cap_usd\":20,\"price_in_per_mtok\":1,\"price_out_per_mtok\":%g,\"output_ceiling_tokens\":1000000,\"ceiling_source\":\"fixture\",\"created\":\"2026-11-22T00:00:00Z\"}\n", opt.arm, evalTestModel, priceOut)
	opt.genesis = fmt.Sprintf("%x", sha256.Sum256([]byte(line)))
	if err := os.WriteFile(opt.ledger, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
}

func frozenEvalRow(id string) map[string]any {
	payload := ingest.CanonicalJudgeInput(ingest.FactInput{
		Head: "Acme \"星\"", Rel: "uses", Tail: "Zorbo", Evidence: "è <>&",
		DocContext: "Quillon Ltd", Confidence: 0.75, DocID: "doc-zorbo",
	})
	return map[string]any{
		"item_id": id, "box": "G", "domain": "general", "doc_id": "doc-zorbo", "cluster_id": "c",
		"chunk_id": "chunk", "head_key": "head", "rel": "uses", "tail_key": "tail", "confidence": 0.75,
		"judge_input_b64": base64.StdEncoding.EncodeToString(payload), "input_hash": fmt.Sprintf("%x", sha256.Sum256(payload)),
	}
}

func writeJudgeEvalInput(t *testing.T, path string, rows ...map[string]any) {
	t.Helper()
	var data bytes.Buffer
	for _, row := range rows {
		if err := json.NewEncoder(&data).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func readJudgeEvalRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows []map[string]any
	dec := json.NewDecoder(f)
	for dec.More() {
		var row map[string]any
		if err := dec.Decode(&row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func judgeEvalArgs(opt judgeEvalOptions) []string {
	return []string{"eval", "run", "--in", opt.in, "--out", opt.out, "--arm", opt.arm, "--manifest", opt.manifest,
		"--ledger", opt.ledger, "--ledger-genesis", opt.genesis, "--run-id", opt.runID, "--allotment", opt.allotment}
}

func judgeEvalTestConfig(t *testing.T, srv *httptest.Server) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", p)
	t.Setenv("GKG_JUDGE_BASE_URL", srv.URL)
	t.Setenv("GKG_JUDGE_AUTH", "static")
	t.Setenv("GKG_JUDGE_MODEL", evalTestModel)
	t.Setenv("GKG_JUDGE_API_KEY", "judge-key")
}

func runEvalDispatch(t *testing.T, opt judgeEvalOptions, want int) {
	t.Helper()
	var stderr bytes.Buffer
	if got := runJudge(judgeEvalArgs(opt), io.Discard, &stderr); got != want {
		t.Fatalf("exit = %d, want %d (%s)", got, want, stderr.String())
	}
}

const evalConfirmed = `{"choices":[{"message":{"content":"{\"verdict\":\"confirmed\",\"reason\":\"supported\",\"confidence\":0.9}"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"completion_tokens_details":{"reasoning_tokens":3},"total_tokens":14}}`

func TestJudgeEvalStaticWireGoldenAndVerdictKeys(t *testing.T) {
	opt := newJudgeEvalFixture(t, "DS")
	var sends atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		// TestVerifyFactV1WireGolden uses the same digest.
		const golden = "6738f4e8778a42b4a550866a9c2ca62196fb350d563a9913354b38e960278a81"
		if sum := fmt.Sprintf("%x", sha256.Sum256(body)); sum != golden {
			t.Errorf("request body digest = %s, want %s", sum, golden)
		}
		fmt.Fprint(w, evalConfirmed)
	}))
	defer srv.Close()
	judgeEvalTestConfig(t, srv)
	runEvalDispatch(t, opt, 0)
	rows := readJudgeEvalRows(t, opt.out)
	if sends.Load() != 2 || len(rows) != 2 {
		t.Fatalf("sends = %d, rows = %v", sends.Load(), rows)
	}
	for i, row := range rows {
		keys := strings.Fields("item_id arm model manifest status confidence reason error_class attempts statuses latency_ms usage calls")
		assertEvalKeys(t, row, keys)
		if row["arm"] != "DS" || row["model"] != evalTestModel || row["manifest"] != opt.manifest ||
			row["item_id"] != []string{"one", "two"}[i] || row["status"] != "confirmed" || row["confidence"] != 0.9 ||
			row["reason"] != "supported" || row["error_class"] != nil || row["attempts"] != float64(1) ||
			!reflect.DeepEqual(row["statuses"], []any{float64(200)}) ||
			!reflect.DeepEqual(row["usage"], map[string]any{"prompt": float64(10), "completion": float64(2), "reasoning": float64(3), "total": float64(14)}) {
			t.Fatalf("row = %v", row)
		}
		calls, ok := row["calls"].([]any)
		if !ok || len(calls) != 1 {
			t.Fatalf("calls = %v", row["calls"])
		}
		assertEvalKeys(t, calls[0].(map[string]any), strings.Fields("item_id host status latency_ms stream tools response_format tool_calls signature_in signature_echoed usage"))
	}
	ledger := readJudgeEvalRows(t, opt.ledger)
	if ledger[len(ledger)-1]["kind"] != "close" {
		t.Fatalf("ledger was not closed: %v", ledger)
	}
	for _, row := range ledger[1:] {
		if row["family"] != "J" {
			t.Fatalf("default family must be J: %v", row)
		}
	}
	runEvalDispatch(t, opt, 0)
	if sends.Load() != 2 || len(readJudgeEvalRows(t, opt.out)) != 2 {
		t.Fatal("completed replay sent or appended again")
	}
}

func TestJudgeEvalFamilyFlag(t *testing.T) {
	for _, tc := range []struct {
		family string
		exit   int
	}{
		{"J", 0},
		{"S", 0},
		{"A", 2},
		{"", 2},
	} {
		t.Run("family="+tc.family, func(t *testing.T) {
			opt := newJudgeEvalFixture(t, "DS")
			writeJudgeEvalInput(t, opt.in, frozenEvalRow("one"))
			var sends atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				fmt.Fprint(w, evalConfirmed)
			}))
			defer srv.Close()
			judgeEvalTestConfig(t, srv)
			var stderr bytes.Buffer
			args := append(judgeEvalArgs(opt), "--family", tc.family)
			if got := runJudge(args, io.Discard, &stderr); got != tc.exit {
				t.Fatalf("exit = %d, want %d (%s)", got, tc.exit, &stderr)
			}
			ledger := readJudgeEvalRows(t, opt.ledger)
			if tc.exit != 0 {
				if len(ledger) != 1 || sends.Load() != 0 || len(readJudgeEvalRows(t, opt.out)) != 0 {
					t.Fatal("invalid family caused a send or a ledger or verdict write")
				}
				return
			}
			if len(ledger) != 5 || sends.Load() != 1 {
				t.Fatalf("ledger = %v, sends = %d", ledger, sends.Load())
			}
			for _, row := range ledger[1:] {
				if row["family"] != tc.family {
					t.Fatalf("row = %v, want family %s", row, tc.family)
				}
			}
			rows := readJudgeEvalRows(t, opt.out)
			if len(rows) != 1 {
				t.Fatalf("verdicts = %v", rows)
			}
			calls, ok := rows[0]["calls"].([]any)
			if !ok || len(calls) != 1 {
				t.Fatalf("calls = %v", rows[0]["calls"])
			}
			call := calls[0].(map[string]any)
			if call["item_id"] != "one" || call["ask_id"] != nil {
				t.Fatalf("call = %v, want item_id", call)
			}
		})
	}
}

func assertEvalKeys(t *testing.T, row map[string]any, keys []string) {
	t.Helper()
	if len(row) != len(keys) {
		t.Fatalf("got %d keys, want %d: %v", len(row), len(keys), row)
	}
	for _, key := range keys {
		if _, ok := row[key]; !ok {
			t.Errorf("missing key %q in %v", key, row)
		}
	}
}

func TestJudgeEvalChecksEveryPayloadBeforeSending(t *testing.T) {
	opt := newJudgeEvalFixture(t, "DS")
	bad := frozenEvalRow("two")
	bad["input_hash"] = strings.Repeat("0", 64)
	writeJudgeEvalInput(t, opt.in, frozenEvalRow("one"), bad)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("sent a request before verifying the entire input")
	}))
	defer srv.Close()
	judgeEvalTestConfig(t, srv)
	runEvalDispatch(t, opt, 2)
	if len(readJudgeEvalRows(t, opt.out)) != 0 || len(readJudgeEvalRows(t, opt.ledger)) != 1 {
		t.Fatal("invalid input wrote an outcome or ledger row")
	}
}

func TestJudgeEvalUnusableOutputDoesNotAllot(t *testing.T) {
	opt := newJudgeEvalFixture(t, "DS")
	opt.out = filepath.Join(filepath.Dir(opt.out), "missing-parent", "verdicts.jsonl")
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("sent a request with unusable output")
	}))
	defer srv.Close()
	judgeEvalTestConfig(t, srv)
	runEvalDispatch(t, opt, 2)
	if rows := readJudgeEvalRows(t, opt.ledger); len(rows) != 1 {
		t.Fatalf("unusable output changed the ledger: %v", rows)
	}
}

func TestJudgeEvalRefusesOutputAliasingLedger(t *testing.T) {
	const child = "GKG_TA1_TEST_OUTPUT_ALIAS"
	if os.Getenv(child) == "" {
		// A blocking flock on this process's own output lock must fail the test.
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestJudgeEvalRefusesOutputAliasingLedger$", "-test.timeout=5s")
		cmd.Env = append(os.Environ(), child+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return
	}
	opt := newJudgeEvalFixture(t, "DS")
	if err := os.Symlink(filepath.Base(opt.out), filepath.Join(filepath.Dir(opt.out), "ledger-link.jsonl")); err != nil {
		t.Fatal(err)
	}
	opt.ledger = filepath.Join(filepath.Dir(opt.out), "ledger-link.jsonl")
	cfg := config.Config{Judge: config.JudgeConfig{Auth: config.AuthStatic, Model: evalTestModel}}
	var stderr bytes.Buffer
	if got := runJudgeEvalRun(t.Context(), opt, cfg, &stderr); got != 2 ||
		!strings.Contains(stderr.String(), "verdict output and ledger must be different files") {
		t.Fatalf("exit = %d (%s)", got, &stderr)
	}
}

func TestJudgeEvalResumeSkipsFinalAndRefusesForeignOutput(t *testing.T) {
	for _, field := range []string{"", "arm", "model", "manifest"} {
		name := field
		if name == "" {
			name = "matching"
		}
		t.Run(name, func(t *testing.T) {
			opt := newJudgeEvalFixture(t, "DS")
			var sends atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				fmt.Fprint(w, evalConfirmed)
			}))
			defer srv.Close()
			judgeEvalTestConfig(t, srv)
			first := map[string]any{"item_id": "one", "arm": "DS", "model": evalTestModel, "manifest": opt.manifest,
				"status": "confirmed", "confidence": 0.9, "reason": "", "error_class": nil, "attempts": 1,
				"statuses": []int{200}, "latency_ms": 0, "usage": nil, "calls": []any{}}
			wantExit, wantSends := 0, int32(1)
			if field != "" {
				first[field] = "foreign"
				wantExit, wantSends = 2, 0
			}
			writeJudgeEvalInput(t, opt.out, first)
			runEvalDispatch(t, opt, wantExit)
			rows := readJudgeEvalRows(t, opt.out)
			if sends.Load() != wantSends || len(rows) != 1+int(wantSends) || (wantExit == 0 && rows[1]["item_id"] != "two") {
				t.Fatalf("sends = %d, rows = %v", sends.Load(), rows)
			}
		})
	}
}

func TestJudgeEvalResumePartialVerdict(t *testing.T) {
	const partial = `{"item_id":"two","arm":"DS"`
	for _, tc := range []struct {
		name, tail, wantError string
		locked, ledgerAlias   bool
	}{
		{name: "partial", tail: partial},
		{name: "malformed complete", tail: partial + "\n", wantError: "line 2: invalid verdict JSON"},
		{name: "locked partial", tail: partial, locked: true, wantError: "lock verdict file"},
		{name: "aliased ledger", tail: partial, ledgerAlias: true, wantError: "verdict output and ledger must be different files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := newJudgeEvalFixture(t, "DS")
			var sends atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				fmt.Fprint(w, evalConfirmed)
			}))
			defer srv.Close()
			judgeEvalTestConfig(t, srv)
			first := fmt.Sprintf("{\"item_id\":\"one\",\"arm\":\"DS\",\"model\":%q,\"manifest\":%q,\"status\":\"confirmed\"}\n", evalTestModel, opt.manifest)
			writeEvalFile(t, opt.out, []byte(first+tc.tail))
			if tc.ledgerAlias {
				opt.ledger = opt.out
			}
			if tc.locked {
				f, err := os.OpenFile(opt.out, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := llmusage.LockOutput(f); err != nil {
					t.Fatal(err)
				}
			}
			var stderr bytes.Buffer
			code := runJudge(judgeEvalArgs(opt), io.Discard, &stderr)
			data, err := os.ReadFile(opt.out)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" {
				if code != 2 || !strings.Contains(stderr.String(), tc.wantError) || sends.Load() != 0 || string(data) != first+tc.tail {
					t.Fatalf("exit=%d stderr=%q sends=%d verdicts=%q", code, &stderr, sends.Load(), data)
				}
				return
			}
			wantStderr := fmt.Sprintf("gkg judge eval run: dropped %d bytes from partial verdict line\n", len(tc.tail))
			if code != 0 || stderr.String() != wantStderr || sends.Load() != 1 || !bytes.HasPrefix(data, []byte(first)) {
				t.Fatalf("exit=%d stderr=%q sends=%d verdicts=%q", code, &stderr, sends.Load(), data)
			}
			rows := readJudgeEvalRows(t, opt.out)
			if len(rows) != 2 || rows[1]["item_id"] != "two" || rows[1]["status"] != "confirmed" {
				t.Fatalf("want the recovered verdict for item two, got %v", rows)
			}
		})
	}
}

type evalJudgeToken struct{}

func (evalJudgeToken) Token(context.Context) (string, time.Time, error) {
	return "fixture-token", time.Now().Add(time.Hour), nil
}

func TestJudgeEvalProviderExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name, arm, auth, class string
		status, exit, attempts int
	}{
		{"Vertex401", "GM", config.AuthGCP, "auth_permanent", 401, 2, 1},
		{"Static401", "DS", config.AuthStatic, "provider_stop", 401, 3, 1},
		{"Static503", "DS", config.AuthStatic, "provider_stop", 503, 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := newJudgeEvalFixture(t, tc.arm)
			var sends atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				if tc.arm == "GM" && r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("missing fake Vertex token")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, r.Header.Get("Authorization"))
			}))
			defer srv.Close()
			cfg := config.Config{Judge: config.JudgeConfig{Auth: tc.auth, BaseURL: srv.URL, Model: evalTestModel, APIKey: "judge-key"}}
			options := []judge.Option{func(c *judge.Client) { c.SetBackoff([]time.Duration{0}) }}
			if tc.arm == "GM" {
				options = append(options, judge.WithTokenProvider(evalJudgeToken{}))
			}
			var stderr bytes.Buffer
			if got := runJudgeEvalRun(t.Context(), opt, cfg, &stderr, options...); got != tc.exit {
				t.Fatalf("exit = %d, want %d (%s)", got, tc.exit, &stderr)
			}
			if sends.Load() != int32(tc.attempts) || len(readJudgeEvalRows(t, opt.out)) != 0 {
				t.Fatalf("sends = %d, output should have no final verdict", sends.Load())
			}
			diagnostic := stderr.String()
			if !strings.Contains(diagnostic, `item "one"`) || !strings.Contains(diagnostic, tc.class) ||
				!strings.Contains(diagnostic, fmt.Sprintf("HTTP %d", tc.status)) ||
				strings.Contains(diagnostic, "fixture-token") || strings.Contains(diagnostic, "judge-key") {
				t.Fatalf("missing or unredacted provider-stop diagnostic: %q", diagnostic)
			}
		})
	}
}

func TestJudgeEvalInvalidItemReportsClassAndRedactedError(t *testing.T) {
	for _, tc := range []struct {
		name, body, class, detail string
		status                    int
	}{
		{"http400", "malformed request: Bearer judge-key", "provider_error_4xx", "HTTP 400: malformed request", 400},
		{"errorEnvelope", `{"error":{"message":"upstream failed: judge-key"}}`, "provider_error_2xx", "provider error: upstream failed", 200},
		{"oversized", strings.Repeat("x", (4<<20)+1), "provider_error_2xx", "response exceeds the 4 MiB cap", 200},
		{"badVerdict", `{"choices":[{"message":{"content":"invalid verdict judge-key"}}]}`, "invalid_verdict", "decode verdict", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := newJudgeEvalFixture(t, "DS")
			writeJudgeEvalInput(t, opt.in, frozenEvalRow("one"))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			judgeEvalTestConfig(t, srv)
			var stderr bytes.Buffer
			if got := runJudge(judgeEvalArgs(opt), io.Discard, &stderr); got != 0 {
				t.Fatalf("exit = %d (%s)", got, &stderr)
			}
			rows := readJudgeEvalRows(t, opt.out)
			if len(rows) != 1 || rows[0]["status"] != "invalid" || rows[0]["error_class"] != tc.class {
				t.Errorf("want an invalid row with class %q, got %v", tc.class, rows)
			}
			diagnostic := stderr.String()
			if !strings.Contains(diagnostic, `item "one"`) || !strings.Contains(diagnostic, tc.detail) || strings.Contains(diagnostic, "judge-key") {
				t.Errorf("missing or unredacted item diagnostic: %q", diagnostic)
			}
		})
	}
}

func TestJudgeEvalSumsUsageOverRetriedAttempts(t *testing.T) {
	opt := newJudgeEvalFixture(t, "DS")
	writeJudgeEvalInput(t, opt.in, frozenEvalRow("one"))
	var sends atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if sends.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"usage":{"prompt_tokens":2,"completion_tokens":1,"completion_tokens_details":{"reasoning_tokens":2},"total_tokens":5}}`)
			return
		}
		fmt.Fprint(w, evalConfirmed)
	}))
	defer srv.Close()
	cfg := config.Config{Judge: config.JudgeConfig{Auth: config.AuthStatic, BaseURL: srv.URL, Model: evalTestModel}}
	var stderr bytes.Buffer
	if got := runJudgeEvalRun(t.Context(), opt, cfg, &stderr, func(c *judge.Client) { c.SetBackoff([]time.Duration{0}) }); got != 0 {
		t.Fatalf("exit = %d (%s)", got, &stderr)
	}
	rows := readJudgeEvalRows(t, opt.out)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0]["statuses"], []any{float64(503), float64(200)}) ||
		!reflect.DeepEqual(rows[0]["usage"], map[string]any{"prompt": float64(12), "completion": float64(3), "reasoning": float64(5), "total": float64(19)}) {
		t.Fatalf("rows = %v", rows)
	}
}

func TestJudgeEvalLedgerStop(t *testing.T) {
	const child = "GKG_TA1_TEST_LEDGER_STOP"
	for _, tc := range []struct {
		name      string
		allotment string
		retry     bool
		sends     int32
		rows      int
	}{
		{"nextItem", "1.1", false, 1, 1},
		{"retry", "1.1", true, 1, 0},
		{"start", "20.1", false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each invocation needs a fresh process-wide stop flag.
			if os.Getenv(child) != tc.name {
				cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestJudgeEvalLedgerStop$/^"+tc.name+"$")
				cmd.Env = append(os.Environ(), child+"="+tc.name)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("child: %v\n%s", err, out)
				}
				return
			}
			opt := newJudgeEvalFixture(t, "DS")
			opt.allotment = tc.allotment
			var sends atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				if tc.retry {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"verdict\":\"confirmed\"}"}}]}`)
			}))
			defer srv.Close()
			judgeEvalTestConfig(t, srv)
			if tc.retry {
				cfg, err := loadConfig()
				if err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				if got := runJudgeEvalRun(t.Context(), opt, cfg, &stderr, func(c *judge.Client) { c.SetBackoff([]time.Duration{0}) }); got != 4 {
					t.Fatalf("exit = %d (%s)", got, &stderr)
				}
			} else {
				runEvalDispatch(t, opt, 4)
			}
			rows := readJudgeEvalRows(t, opt.out)
			if sends.Load() != tc.sends || len(rows) != tc.rows || (tc.rows > 0 && rows[0]["item_id"] != "one") {
				t.Fatalf("sends = %d, rows = %v", sends.Load(), rows)
			}
			for _, row := range readJudgeEvalRows(t, opt.ledger) {
				if row["kind"] == "close" {
					t.Fatal("ledger stop closed an incomplete run")
				}
			}
		})
	}
}

func TestJudgeEvalRejectsUnmeteredRedirect(t *testing.T) {
	opt := newJudgeEvalFixture(t, "DS")
	writeJudgeEvalInput(t, opt.in, frozenEvalRow("one"))
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("redirect sent an unmetered request")
		fmt.Fprint(w, evalConfirmed)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	judgeEvalTestConfig(t, srv)
	runEvalDispatch(t, opt, 0)
	rows := readJudgeEvalRows(t, opt.out)
	if len(rows) != 1 || rows[0]["status"] != "invalid" || rows[0]["error_class"] != "provider_error_3xx" ||
		rows[0]["usage"] != nil || !reflect.DeepEqual(rows[0]["statuses"], []any{float64(307)}) {
		t.Fatalf("rows = %v", rows)
	}
}
