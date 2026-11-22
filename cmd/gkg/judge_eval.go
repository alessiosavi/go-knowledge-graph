package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
	"github.com/alessiosavi/go-knowledge-graph/internal/llmauth"
	"github.com/alessiosavi/go-knowledge-graph/internal/llmusage"
)

type judgeEvalOptions struct {
	in, out, arm, family, manifest, ledger, genesis, runID, allotment string
}

type judgeEvalItem struct {
	id      string
	payload ingest.JudgePayload
}

type judgeEvalVerdict struct {
	ItemID     string          `json:"item_id"`
	Arm        string          `json:"arm"`
	Model      string          `json:"model"`
	Manifest   string          `json:"manifest"`
	Status     string          `json:"status"`
	Confidence float64         `json:"confidence"`
	Reason     string          `json:"reason"`
	ErrorClass *string         `json:"error_class"`
	Attempts   int             `json:"attempts"`
	Statuses   []int           `json:"statuses"`
	LatencyMS  int64           `json:"latency_ms"`
	Usage      *llmusage.Usage `json:"usage"`
	Calls      []llmusage.Call `json:"calls"`
}

func runJudgeEval(args []string, stderr io.Writer) int {
	const usage = "usage: gkg judge eval run --in frozen.jsonl --out verdicts.jsonl --arm DS|GM [--family J|S] --manifest SHA --ledger L --ledger-genesis SHA --run-id R --allotment USD"
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(stderr, usage)
		if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
			return 0
		}
		return 2
	}
	fs := flag.NewFlagSet("judge eval run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }
	var opt judgeEvalOptions
	fs.StringVar(&opt.in, "in", "", "frozen judge input JSONL")
	fs.StringVar(&opt.out, "out", "", "resumable verdict JSONL")
	fs.StringVar(&opt.arm, "arm", "", "provider arm (DS or GM)")
	fs.StringVar(&opt.family, "family", "J", "ledger family (J for judge, S for smoke, probe or warm-up)")
	fs.StringVar(&opt.manifest, "manifest", "", "frozen manifest SHA")
	fs.StringVar(&opt.ledger, "ledger", "", "existing provider ledger")
	fs.StringVar(&opt.genesis, "ledger-genesis", "", "SHA-256 of the ledger header line")
	fs.StringVar(&opt.runID, "run-id", "", "run ID for ledger accounting")
	fs.StringVar(&opt.allotment, "allotment", "", "run allotment in USD")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || opt.in == "" || opt.out == "" || opt.manifest == "" ||
		opt.ledger == "" || opt.genesis == "" || opt.runID == "" || opt.allotment == "" {
		fs.Usage()
		return 2
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg judge eval run: %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runJudgeEvalRun(ctx, opt, cfg, stderr)
}

func runJudgeEvalRun(ctx context.Context, opt judgeEvalOptions, cfg config.Config, stderr io.Writer, options ...judge.Option) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "gkg judge eval run: %v\n", err)
		if errors.Is(err, llmusage.ErrLedgerStop) {
			return 4
		}
		return 2
	}
	if opt.family != "J" && opt.family != "S" {
		return fail(errors.New("family must be J or S"))
	}
	if (opt.arm != "DS" || cfg.Judge.Auth != config.AuthStatic) && (opt.arm != "GM" || cfg.Judge.Auth != config.AuthGCP) {
		return fail(errors.New("arm must match the configured judge auth (DS=static, GM=gcp)"))
	}
	model := cfg.Judge.Model
	if model == "" {
		model = judge.DefaultModel
	}
	base := strings.TrimRight(cfg.Judge.BaseURL, "/")
	if base == "" {
		base = judge.DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fail(errors.New("invalid judge base URL"))
	}
	items, ids, err := readJudgeEvalInput(opt.in)
	if err != nil {
		return fail(fmt.Errorf("read frozen input: %w", err))
	}
	out, done, err := openJudgeEvalResume(opt, model, ids)
	if err != nil {
		return fail(fmt.Errorf("resume verdicts: %w", err))
	}
	if out == nil {
		out, err = createEvalOutput(opt.out)
		if err != nil {
			return fail(fmt.Errorf("create verdicts: %w", err))
		}
	}
	defer out.Close()
	// Opening a ledger that aliases out would wait on our own output lock.
	if ledgerInfo, err := os.Stat(opt.ledger); err == nil {
		outInfo, err := out.Stat()
		if err != nil {
			return fail(fmt.Errorf("stat verdicts: %w", err))
		}
		if os.SameFile(outInfo, ledgerInfo) {
			return fail(errors.New("verdict output and ledger must be different files"))
		}
	}
	ledger, err := llmusage.Open(llmusage.Run{
		Path: opt.ledger, Genesis: opt.genesis, Provider: opt.arm, Model: model,
		RunID: opt.runID, Family: opt.family, Manifest: opt.manifest, AllotmentUSD: opt.allotment,
	})
	if err != nil {
		return fail(err)
	}
	defer ledger.Close()
	transport, err := llmusage.NewTransport(ledger, u.Host, llmusage.ItemIDKey)
	if err != nil {
		return fail(err)
	}
	defer transport.CloseIdleConnections()
	client := judge.NewClientWithOptions(cfg, append(options, judge.WithEvalTransport(transport))...)
	client.SetAttemptGate(ledger.Check)
	enc := json.NewEncoder(out)
	redactReason := llmauth.Redactor("Bearer " + cfg.Judge.APIKey)
	for _, item := range items {
		if done[item.id] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if err := ledger.Check(); err != nil {
			return fail(err)
		}
		started := time.Now()
		v, verifyErr := client.VerifyPayload(llmusage.WithID(ctx, item.id), item.payload)
		calls := transport.TakeCalls(item.id)
		if err := ledger.Check(); err != nil {
			return fail(err)
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		row := judgeEvalVerdict{
			ItemID: item.id, Arm: opt.arm, Model: model, Manifest: opt.manifest, Calls: calls,
			Status: v.Status, Confidence: v.Confidence, Reason: redactReason(v.Reason), LatencyMS: time.Since(started).Milliseconds(),
		}
		row.Attempts, row.Statuses = len(calls), make([]int, 0, len(calls))
		for _, call := range calls {
			row.Statuses = append(row.Statuses, call.Status)
			if u := call.Usage; u != nil {
				if row.Usage == nil {
					row.Usage = new(llmusage.Usage)
				}
				row.Usage.Prompt += u.Prompt
				row.Usage.Completion += u.Completion
				row.Usage.Reasoning += u.Reasoning
				row.Usage.Total += u.Total
			}
		}
		if verifyErr != nil {
			if errors.Is(verifyErr, llmusage.ErrLedgerStop) || errors.Is(verifyErr, llmusage.ErrLedger) {
				return fail(verifyErr)
			}
			class := judgeEvalErrorClass(verifyErr)
			fmt.Fprintf(stderr, "gkg judge eval run: item %q (%s, HTTP statuses %v): %v\n", item.id, class, row.Statuses, verifyErr)
			if exit := judgeRunErrorExitCode(verifyErr); exit != 1 {
				return exit
			}
			row.Status, row.ErrorClass = "invalid", &class
		}
		if err := enc.Encode(row); err != nil {
			return fail(fmt.Errorf("append verdict: %w", err))
		}
		if err := out.Sync(); err != nil {
			return fail(fmt.Errorf("sync verdict: %w", err))
		}
	}
	if err := out.Sync(); err != nil {
		return fail(fmt.Errorf("sync verdicts before completion: %w", err))
	}
	if err := ledger.Complete(); err != nil {
		return fail(err)
	}
	return 0
}

func judgeEvalErrorClass(err error) string {
	if class := llmauth.Class(err); class != "" {
		return class
	}
	if judge.IsProviderStop(err) {
		return "provider_stop"
	}
	if code := judge.ProviderResponseStatus(err); code != 0 {
		return fmt.Sprintf("provider_error_%dxx", code/100)
	}
	return "invalid_verdict"
}

func readJudgeEvalInput(path string) ([]judgeEvalItem, map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var items []judgeEvalItem
	ids := make(map[string]bool)
	err = scanEvalJSONL(f, func(raw []byte) error {
		var row struct {
			ID   string `json:"item_id"`
			B64  string `json:"judge_input_b64"`
			Hash string `json:"input_hash"`
		}
		if json.Unmarshal(raw, &row) != nil || row.ID == "" || ids[row.ID] {
			return errors.New("invalid or duplicate frozen item")
		}
		payload, err := base64.StdEncoding.DecodeString(row.B64)
		if err != nil {
			return errors.New("invalid base64 judge payload")
		}
		p := ingest.JudgePayload{Bytes: payload, InputHash: row.Hash}
		if err := judge.ValidatePayload(p); err != nil {
			return err
		}
		ids[row.ID] = true
		items = append(items, judgeEvalItem{id: row.ID, payload: p})
		return nil
	})
	return items, ids, err
}

func openJudgeEvalResume(opt judgeEvalOptions, model string, ids map[string]bool) (*os.File, map[string]bool, error) {
	f, err := os.OpenFile(opt.out, os.O_RDWR|os.O_APPEND, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := llmusage.LockOutput(f); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("lock verdict file: %w", err)
	}
	done := make(map[string]bool)
	err = scanEvalJSONL(f, func(raw []byte) error {
		var row judgeEvalVerdict
		if err := json.Unmarshal(raw, &row); err != nil {
			return errors.New("invalid verdict JSON")
		}
		if row.Arm != opt.arm || row.Model != model || row.Manifest != opt.manifest {
			return errors.New("verdict arm, model or manifest mismatch")
		}
		if !ids[row.ItemID] || done[row.ItemID] {
			return errors.New("unknown or duplicate verdict item")
		}
		switch row.Status {
		case "confirmed", "unclear", "refuted", "invalid":
			done[row.ItemID] = true
		default:
			return errors.New("verdict has no final status")
		}
		return nil
	})
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, done, nil
}

func createEvalOutput(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	err = llmusage.LockOutput(f)
	if err == nil {
		var dir *os.File
		dir, err = os.Open(filepath.Dir(path))
		if err == nil {
			err = errors.Join(dir.Sync(), dir.Close())
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func scanEvalJSONL(r io.Reader, visit func([]byte) error) error {
	reader := bufio.NewReader(r)
	for n := 1; ; n++ {
		raw, err := reader.ReadBytes('\n')
		if err == io.EOF && len(raw) == 0 {
			return nil
		}
		if err == nil {
			err = visit(raw)
		}
		if err != nil {
			return fmt.Errorf("line %d: %w", n, err)
		}
	}
}
