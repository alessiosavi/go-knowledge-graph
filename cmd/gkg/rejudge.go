package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func runJudgeRejudge(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("judge rejudge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	plan := fs.Bool("plan", false, "freeze requests without calling the provider")
	judgePhase := fs.Bool("judge", false, "judge or resume a frozen manifest")
	var selectOpt judge.RejudgeSelection
	fs.StringVar(&selectOpt.Domain, "domain", "", "restrict the population to this domain")
	fs.IntVar(&selectOpt.Limit, "limit", 0, "maximum eligible edges to examine (0 means all)")
	ids := fs.String("ids", "", "restrict the plan to ids in an audit JSONL file")
	var opt judge.RejudgeOptions
	fs.StringVar(&opt.Out, "out", "", "output manifest or verdict JSONL path")
	fs.StringVar(&opt.Manifest, "manifest", "", "input manifest JSONL path")
	fs.IntVar(&opt.BudgetAttempts, "budget-attempts", 0, "positive cumulative provider attempt cap, retries included")
	fs.BoolVar(&opt.RaiseCap, "raise-cap", false, "authorize raising the saved attempt cap")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: gkg judge rejudge -plan -out manifest.jsonl [-domain DOMAIN] [-ids labelled.jsonl | -limit N]")
		fmt.Fprintln(stderr, "       gkg judge rejudge -judge -manifest manifest.jsonl -budget-attempts N -out verdicts.jsonl [-raise-cap]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if fs.NArg() != 0 || *plan == *judgePhase || opt.Out == "" ||
		(*plan && (seen["manifest"] || seen["budget-attempts"] || seen["raise-cap"] ||
			selectOpt.Limit < 0 || seen["ids"] && (*ids == "" || seen["limit"]))) ||
		(*judgePhase && (seen["ids"] || seen["limit"] || seen["domain"] || opt.Manifest == "" || opt.BudgetAttempts <= 0)) {
		fs.Usage()
		return 2
	}
	fail := func(code int, err error) int {
		fmt.Fprintf(stderr, "gkg judge rejudge: %v\n", err)
		return code
	}
	if *ids != "" {
		rows, err := readAuditRows(*ids)
		if err != nil {
			return fail(2, err)
		}
		seenIDs := make(map[string]bool, len(rows))
		for _, row := range rows {
			if row.ID == "" || seenIDs[row.ID] {
				return fail(2, fmt.Errorf("empty or duplicate labelled id %q", row.ID))
			}
			seenIDs[row.ID] = true
		}
		selectOpt.Labels = append([]judge.AuditRow{}, rows...)
	}
	cfg, err := loadConfig()
	if err != nil {
		return fail(1, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		return fail(1, err)
	}
	defer st.Close(context.WithoutCancel(ctx))
	// The store is provisioned by serve or migrate. Rejudge must never
	// bootstrap or alter extraction data or schema.
	client := judge.NewClient(cfg)
	if *plan {
		result, runErr := judge.PlanRejudge(ctx, st, client.Model(), cfg.Labels.PresetsDir, opt.Out, selectOpt)
		if err := writeRejudgePlanSummary(stdout, result); err != nil {
			return fail(1, err)
		}
		if runErr != nil {
			return fail(1, runErr)
		}
		return 0
	}
	result, runErr := judge.JudgeRejudge(ctx, st, client, cfg.Labels.PresetsDir, opt)
	summary := fmt.Sprintf("judged=%d resumed=%d planned=%d cache_hits=%d attempts=%d cap=%d effective_budget=%d stopped_for_budget=%t",
		result.Judged, result.Resumed, result.Planned, result.CacheHits, result.Attempts, result.Cap, result.EffectiveBudget, result.StoppedForBudget)
	if result.Gate == nil && result.Fingerprint != "" {
		summary += " fingerprint=" + result.Fingerprint
	}
	if _, err := fmt.Fprintln(stdout, summary); err != nil {
		return fail(1, err)
	}
	if runErr != nil {
		return fail(1, runErr)
	}
	if result.Gate != nil {
		if err := result.Gate.Write(stdout); err != nil {
			return fail(1, err)
		}
		if !result.Gate.Passed() {
			return 3
		}
	} else if judged := result.Judged + result.Resumed; result.StoppedForBudget || judged < result.Planned {
		message := fmt.Sprintf("incomplete: %d of %d rows judged", judged, result.Planned)
		if result.StoppedForBudget {
			message += "; budget reached"
		}
		return fail(4, errors.New(message))
	}
	return 0
}

func writeRejudgePlanSummary(w io.Writer, result judge.RejudgePlanResult) error {
	total := 0
	for _, count := range result.Unverifiable {
		total += count
	}
	if _, err := fmt.Fprintf(w, "planned=%d unverifiable=%d reconciled=%d mismatched=%d calls_needed=%d\n",
		result.Planned, total, result.Reconciled, result.Mismatched, result.CallsNeeded); err != nil {
		return err
	}
	for _, reason := range slices.Sorted(maps.Keys(result.Unverifiable)) {
		if _, err := fmt.Fprintf(w, "unverifiable reason=%q count=%d\n", reason, result.Unverifiable[reason]); err != nil {
			return err
		}
	}
	for _, mismatch := range result.Mismatches {
		if _, err := fmt.Fprintf(w, "mismatched %s reason=%q\n", mismatch.ID, mismatch.Reason); err != nil {
			return err
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(result.Headers)) {
		h := result.Headers[rel]
		if _, err := fmt.Fprintf(w, "relation=%q header_present=%d header_null=%d\n", rel, h.Present, h.Null); err != nil {
			return err
		}
	}
	return nil
}
