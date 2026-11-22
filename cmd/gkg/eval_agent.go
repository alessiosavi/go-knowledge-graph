package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/llmusage"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

type evalAgentOptions struct {
	questions, out, arm, family, manifest, ledger, genesis, runID, allotment, only string
}

type evalAgentQuestion struct {
	id  string
	ask agent.AskRequest
	sse bool
}

type evalAgentAnswer struct {
	ID                    string               `json:"id"`
	Arm                   string               `json:"arm"`
	Model                 string               `json:"model"`
	Manifest              string               `json:"manifest"`
	Answer                string               `json:"answer"`
	Steps                 int                  `json:"steps"`
	LatencyMS             int64                `json:"latency_ms"`
	ErrorClass            *string              `json:"error_class"`
	Evidence              []agent.EvalEvidence `json:"evidence"`
	ToolErrors            int                  `json:"tool_errors"`
	UnknownTools          int                  `json:"unknown_tools"`
	Calls                 []llmusage.Call      `json:"calls"`
	SSE                   *evalAgentSSE        `json:"sse"`
	DiscardedExtraContent int                  `json:"discarded_extra_content"`
}

type evalAgentSSE struct {
	Tool  int    `json:"tool"`
	Delta int    `json:"delta"`
	Done  int    `json:"done"`
	Error int    `json:"error"`
	Last  string `json:"last"`
}

func runEvalAgent(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: gkg eval-agent --questions A.jsonl --out answers.jsonl --arm DS|GM --manifest SHA --ledger L --ledger-genesis SHA --run-id R --allotment USD [--family A|S] [--only id1,id2]")
	}
	var opt evalAgentOptions
	fs.StringVar(&opt.questions, "questions", "", "frozen question JSONL")
	fs.StringVar(&opt.out, "out", "", "resumable answer JSONL")
	fs.StringVar(&opt.arm, "arm", "", "provider arm (DS or GM)")
	fs.StringVar(&opt.family, "family", "A", "ledger family (A for agent, S for smoke, probe or warm-up)")
	fs.StringVar(&opt.manifest, "manifest", "", "frozen manifest SHA")
	fs.StringVar(&opt.ledger, "ledger", "", "existing provider ledger")
	fs.StringVar(&opt.genesis, "ledger-genesis", "", "SHA-256 of the ledger header line")
	fs.StringVar(&opt.runID, "run-id", "", "run ID for ledger accounting")
	fs.StringVar(&opt.allotment, "allotment", "", "run allotment in USD")
	fs.StringVar(&opt.only, "only", "", "comma-separated ask IDs")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || opt.questions == "" || opt.out == "" || opt.arm == "" ||
		opt.manifest == "" || opt.ledger == "" || opt.genesis == "" || opt.runID == "" || opt.allotment == "" {
		fs.Usage()
		return 2
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval-agent: %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runEvalAgentRun(ctx, opt, cfg, stderr)
}

func runEvalAgentRun(ctx context.Context, opt evalAgentOptions, cfg config.Config, stderr io.Writer, options ...agent.EvalOption) int {
	report := func(err error, code int) int {
		fmt.Fprintf(stderr, "gkg eval-agent: %v\n", err)
		return code
	}
	fail := func(err error) int {
		if errors.Is(err, llmusage.ErrLedgerStop) {
			return report(err, 4)
		}
		return report(err, 2)
	}
	if opt.family != "A" && opt.family != "S" {
		return fail(errors.New("family must be A or S"))
	}
	if (opt.arm != "DS" || cfg.Agent.Auth != config.AuthStatic) && (opt.arm != "GM" || cfg.Agent.Auth != config.AuthGCP) {
		return fail(errors.New("arm must match the configured agent auth (DS=static, GM=gcp)"))
	}
	if !cfg.Agent.Enabled {
		return fail(errors.New("agent is not enabled (agent.enabled=false)"))
	}
	model, host, err := agent.EvalEndpoint(cfg.Agent)
	if err != nil {
		return fail(err)
	}
	questions, err := readEvalAgentQuestions(opt, cfg.Agent)
	if err != nil {
		return fail(fmt.Errorf("read frozen questions: %w", err))
	}

	// Require the ledger to exist before creating an answer file that could
	// otherwise also create the ledger (for example through a dangling symlink).
	ledgerInfo, err := os.Stat(opt.ledger)
	if err != nil {
		return fail(fmt.Errorf("stat ledger: %w", err))
	}
	out, done, err := openEvalAgentResume(opt, model, ledgerInfo)
	if err != nil {
		return fail(fmt.Errorf("resume answers: %w", err))
	}
	if out == nil {
		out, err = createEvalOutput(opt.out)
		if err != nil {
			return fail(fmt.Errorf("create answers: %w", err))
		}
	}
	defer out.Close()
	ledger, err := llmusage.Open(llmusage.Run{
		Path: opt.ledger, Genesis: opt.genesis, Provider: opt.arm, Model: model, RunID: opt.runID,
		Family: opt.family, Manifest: opt.manifest, AllotmentUSD: opt.allotment,
	})
	if err != nil {
		return fail(err)
	}
	defer ledger.Close()
	// Close only after every selected ask has a durable answer row.
	complete := func() int {
		if err := out.Sync(); err != nil {
			return fail(fmt.Errorf("sync answers: %w", err))
		}
		if err := ledger.Complete(); err != nil {
			return fail(err)
		}
		return 0
	}
	questions = slices.DeleteFunc(questions, func(q evalAgentQuestion) bool { return done[q.id] })
	if len(questions) == 0 {
		return complete()
	}
	// Open and read the frozen graph using the normal agent tools. Building
	// the ingestion pipeline would also record an embedding-space marker.
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	defer st.Close(context.Background())
	var emb search.QueryEmbedder
	if cfg.Embedder.Enabled {
		emb, err = embedderFromConfig(cfg)
		if err != nil {
			return fail(err)
		}
	}
	transport, err := llmusage.NewTransport(ledger, host, llmusage.AskIDKey)
	if err != nil {
		return fail(err)
	}
	defer transport.CloseIdleConnections()
	options = append(options, agent.WithEvalTransport(transport, ledger.Check))
	newServer := func() *agent.Server { return agent.NewEvalServer(cfg, st, emb, options...) }
	var plain *agent.Server
	enc := json.NewEncoder(out)
	for _, q := range questions {
		if err := ledger.Check(); err != nil {
			return report(err, 4)
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		var server *agent.Server
		if q.sse {
			server = newServer()
			if !server.EvalStreamMemoEmpty() {
				return fail(errors.New("fresh agent has a populated stream memo"))
			}
		} else {
			if plain == nil {
				plain = newServer()
			}
			server = plain
		}
		row := evalAgentAnswer{ID: q.id, Arm: opt.arm, Model: model, Manifest: opt.manifest}
		started := time.Now()
		var observation agent.EvalObservation
		var response agent.AskResponse
		var askErr error
		if q.sse {
			row.SSE = new(evalAgentSSE)
			response, observation, askErr = askEvalAgentStream(ctx, server, q, row.SSE)
			if askErr != nil && row.SSE.Last == "" {
				// A preflight failure can return HTTP 502 before any event.
				row.SSE.Error, row.SSE.Last = 1, "error"
			}
		} else {
			askCtx := agent.WithEvalObservation(llmusage.WithID(ctx, q.id), &observation)
			response, askErr = server.Ask(askCtx, q.ask)
		}
		row.LatencyMS = time.Since(started).Milliseconds()
		if err := ledger.Check(); err != nil {
			return report(err, 4)
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		row.Calls = transport.TakeCalls(q.id)
		row.Evidence = observation.Evidence
		if row.Evidence == nil {
			row.Evidence = []agent.EvalEvidence{}
		}
		row.Steps = observation.Steps
		row.ToolErrors, row.UnknownTools = observation.ToolErrors, observation.UnknownTools
		row.DiscardedExtraContent = observation.DiscardedExtraContent

		class, permanent := agent.EvalErrorClass(askErr)
		if class != "" {
			row.ErrorClass = &class
		} else {
			row.Answer = response.Answer
		}
		if err := enc.Encode(row); err != nil {
			return fail(fmt.Errorf("append answer: %w", err))
		}
		if err := out.Sync(); err != nil {
			return fail(fmt.Errorf("sync answer: %w", err))
		}
		if permanent {
			return fail(errors.New("agent provider stopped on permanent authentication failure"))
		}
	}
	return complete()
}

func readEvalAgentQuestions(opt evalAgentOptions, cfg config.AgentConfig) ([]evalAgentQuestion, error) {
	f, err := os.Open(opt.questions)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var questions []evalAgentQuestion
	ids := make(map[string]bool)
	err = scanEvalJSONL(f, func(raw []byte) error {
		var row struct {
			ID              string  `json:"id"`
			Question        string  `json:"question"`
			Domain          string  `json:"domain"`
			SSE             *bool   `json:"sse"`
			ReasoningEffort *string `json:"reasoning_effort"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if strings.TrimSpace(row.ID) == "" || ids[row.ID] || row.SSE == nil {
			return errors.New("question needs a unique nonempty id and a boolean sse field")
		}
		req := agent.AskRequest{Question: row.Question, Domain: row.Domain}
		if row.ReasoningEffort != nil {
			req.Generation = &agent.GenerationSettings{ReasoningEffort: row.ReasoningEffort}
		}
		req, err := agent.NormalizeEvalAsk(cfg, req)
		if err != nil {
			return fmt.Errorf("ask %q: %w", row.ID, err)
		}
		ids[row.ID] = true
		questions = append(questions, evalAgentQuestion{id: row.ID, ask: req, sse: *row.SSE})
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	if len(questions) == 0 {
		return nil, errors.New("no questions")
	}
	if opt.only == "" {
		return questions, nil
	}
	selected := make(map[string]bool)
	for _, id := range strings.Split(opt.only, ",") {
		id = strings.TrimSpace(id)
		if !ids[id] {
			return nil, fmt.Errorf("unknown --only id %q", id)
		}
		selected[id] = true
	}
	return slices.DeleteFunc(questions, func(q evalAgentQuestion) bool { return !selected[q.id] }), nil
}

func openEvalAgentResume(opt evalAgentOptions, model string, ledgerInfo os.FileInfo) (*os.File, map[string]bool, error) {
	f, err := os.OpenFile(opt.out, os.O_RDWR|os.O_APPEND, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	outInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("stat answers: %w", err)
	}
	// Locking an output that aliases the ledger would deadlock on ledger open.
	if os.SameFile(outInfo, ledgerInfo) {
		_ = f.Close()
		return nil, nil, errors.New("answer output and ledger must be different files")
	}
	if err := llmusage.LockOutput(f); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("lock answer file: %w", err)
	}
	done := make(map[string]bool)
	err = scanEvalJSONL(f, func(raw []byte) error {
		var row struct{ ID, Arm, Model, Manifest string }
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if row.Arm != opt.arm || row.Model != model || row.Manifest != opt.manifest {
			return errors.New("answer arm, model or manifest mismatch")
		}
		if row.ID == "" || done[row.ID] {
			return errors.New("missing or duplicate answer id")
		}
		done[row.ID] = true
		return nil
	}, nil)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, done, nil
}

func askEvalAgentStream(ctx context.Context, server *agent.Server, q evalAgentQuestion, counts *evalAgentSSE) (agent.AskResponse, agent.EvalObservation, error) {
	payload, err := json.Marshal(q.ask)
	if err != nil {
		return agent.AskResponse{}, agent.EvalObservation{}, err
	}
	handler := server.StreamHandler()
	observed := make(chan agent.EvalObservation, 1)
	// Use a real loopback HTTP connection while keeping httptest (which
	// imports testing) out of the gkg binary.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return agent.AskResponse{}, agent.EvalObservation{}, err
	}
	path := "/" + rand.Text()
	mux := http.NewServeMux()
	mux.Handle("POST "+path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var observation agent.EvalObservation
		askCtx := agent.WithEvalObservation(llmusage.WithID(r.Context(), q.id), &observation)
		handler.ServeHTTP(w, r.WithContext(askCtx))
		observed <- observation
	}))
	local := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = local.Serve(listener) }()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+listener.Addr().String()+path, bytes.NewReader(payload))
	var answer agent.AskResponse
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		var resp *http.Response
		resp, err = client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				answer, err = readEvalAgentSSE(resp.Body, counts)
			} else {
				err = fmt.Errorf("agent stream handler returned HTTP %d", resp.StatusCode)
			}
			_ = resp.Body.Close()
		}
	}
	// Wait for the handler (and any active ask) before reading observations
	// or checking the ledger. Shutdown also closes the loopback listener.
	_ = local.Shutdown(context.Background())
	var observation agent.EvalObservation
	select {
	case observation = <-observed:
		if observation.Err != nil {
			err = observation.Err
		}
	default: // The local request was canceled before reaching the handler.
	}
	return answer, observation, err
}

func readEvalAgentSSE(body io.Reader, counts *evalAgentSSE) (agent.AskResponse, error) {
	reader := bufio.NewReader(body)
	var event string
	var data strings.Builder
	var answer agent.AskResponse
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return agent.AskResponse{}, err
			}
			if counts.Done == 0 {
				return agent.AskResponse{}, errors.New("agent stream ended without a parsed done event")
			}
			return answer, nil
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimPrefix(strings.TrimPrefix(line, "event:"), " ")
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteByte('\n')
		case line == "" && event != "":
			counts.Last = event
			switch event {
			case "tool":
				counts.Tool++
			case "delta":
				counts.Delta++
			case "error":
				counts.Error++
			case "done":
				var parsed *agent.AskResponse
				if json.Unmarshal([]byte(data.String()), &parsed) == nil && parsed != nil {
					counts.Done++
					answer = *parsed
				}
			}
			event = ""
			data.Reset()
		}
	}
}
