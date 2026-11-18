package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func runJudge(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: gkg judge <backfill|audit|rejudge> [flags]")
		return 2
	}
	switch args[0] {
	case "backfill":
		return runJudgeBackfill(args[1:], stdout, stderr)
	case "rejudge":
		return runJudgeRejudge(args[1:], stdout, stderr)
	case "audit":
		return runJudgeAudit(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprintln(stdout, "usage: gkg judge <backfill|audit|rejudge> [flags]")
		return 0
	default:
		fmt.Fprintf(stderr, "gkg judge: unknown command %q\n", args[0])
		return 2
	}
}

func runJudgeBackfill(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("judge backfill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opt judge.BackfillOptions
	fs.IntVar(&opt.Limit, "limit", 0, "maximum candidates (0 means all)")
	fs.IntVar(&opt.BudgetCalls, "budget-calls", 0, "required positive maximum provider attempts, including retries")
	fs.StringVar(&opt.Domain, "domain", "", "restrict candidates to this domain")
	fs.BoolVar(&opt.Force, "force", false, "take over a live backfill lease")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || opt.Limit < 0 || opt.BudgetCalls <= 0 {
		fmt.Fprintln(stderr, "usage: gkg judge backfill --budget-calls N [--limit N] [--domain d] [--force]")
		fmt.Fprintln(stderr, "gkg judge backfill: --budget-calls is required and must be > 0; limit must be non-negative; positional arguments are not supported")
		return 2
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg judge backfill: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg judge backfill: %v\n", err)
		return 1
	}
	defer st.Close(context.WithoutCancel(ctx))
	if err := st.Bootstrap(ctx); err != nil {
		fmt.Fprintf(stderr, "gkg judge backfill: %v\n", err)
		return 1
	}
	cached, client := newFactJudgeClient(cfg, st)
	r, runErr := judge.Backfill(ctx, st, cached, client, opt)
	if _, err := fmt.Fprintf(stdout, "judged=%d confirmed=%d unclear=%d refuted=%d vanished=%d orphaned=%d failed=%d cache_hits=%d attempts=%d stopped_for_budget=%t\n", r.Judged, r.Confirmed, r.Unclear, r.Refuted, r.Vanished, r.Orphaned, r.Failed, r.CacheHits, r.Attempts, r.StoppedForBudget); err != nil {
		fmt.Fprintf(stderr, "gkg judge backfill: %v\n", err)
		return 1
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "gkg judge backfill: %v\n", runErr)
		return 1
	}
	return 0
}

func runJudgeAudit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("judge audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sample := fs.Int("sample", 0, "number of confirmed typed facts to sample")
	seed := fs.Uint64("seed", 1, "PCG sample seed")
	out := fs.String("out", "", "write sample JSONL to this file")
	score := fs.String("score", "", "score labelled JSONL without connecting to the graph")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if fs.NArg() != 0 || (*score != "" && (seen["sample"] || seen["out"] || seen["seed"])) || (*score == "" && (*sample <= 0 || *out == "")) {
		fmt.Fprintln(stderr, "gkg judge audit: use --sample N [--seed s] --out file.jsonl or --score labelled.jsonl")
		return 2
	}
	if *score != "" {
		rows, err := readAuditRows(*score)
		if err != nil {
			fmt.Fprintf(stderr, "gkg judge audit: %v\n", err)
			return 2
		}
		p, lo, hi, n := judge.AuditScore(rows)
		if n == 0 {
			fmt.Fprintln(stderr, "gkg judge audit: no scored rows")
			return 2
		}
		if _, err := fmt.Fprintf(stdout, "precision=%.3f Wilson 95%%=[%.3f, %.3f] n=%d\n", p, lo, hi, n); err != nil {
			fmt.Fprintf(stderr, "gkg judge audit: %v\n", err)
			return 1
		}
		return 0
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg judge audit: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg judge audit: %v\n", err)
		return 1
	}
	defer st.Close(context.WithoutCancel(ctx))
	if err := st.Bootstrap(ctx); err != nil {
		fmt.Fprintf(stderr, "gkg judge audit: %v\n", err)
		return 1
	}
	rows, err := judge.AuditSample(ctx, st, *sample, *seed)
	if err != nil {
		fmt.Fprintf(stderr, "gkg judge audit: %v\n", err)
		return 1
	}
	if err := writeAuditRows(*out, rows); err != nil {
		fmt.Fprintf(stderr, "gkg judge audit: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "sampled=%d seed=%d out=%s\n", len(rows), *seed, *out); err != nil {
		fmt.Fprintf(stderr, "gkg judge audit: %v\n", err)
		return 1
	}
	return 0
}

func writeAuditRows(path string, rows []judge.AuditRow) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func readAuditRows(path string) ([]judge.AuditRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var rows []judge.AuditRow
	for line := 1; scanner.Scan(); line++ {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var row judge.AuditRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		switch row.Label {
		case "", "ok", "wrong", "noise", "skip":
		default:
			return nil, fmt.Errorf("line %d: invalid label %q (want ok|wrong|noise|skip)", line, row.Label)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}
