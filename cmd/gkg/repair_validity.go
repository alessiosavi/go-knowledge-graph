package main

import (
	"context"
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

func runRepairValidity(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repair-validity", flag.ContinueOnError)
	fs.SetOutput(stderr)
	classify := fs.Bool("classify", false, "classify the catalog inventory")
	finalize := fs.Bool("finalize", false, "finalize a frozen draft offline")
	preset := fs.String("preset", "catalog", "label preset to classify or plan")
	out := fs.String("out", "", "output directory")
	manifest := fs.String("manifest", "", "sealed bulk run's manifest")
	verdicts := fs.String("verdicts", "", "sealed bulk run's verdicts")
	rewriteVerdicts := fs.String("rewrite-verdicts", "", "sealed rewrite run's verdicts")
	noRewrites := fs.Bool("no-rewrites", false, "finalize a draft with no rewrite candidates")
	// A bare -plan selects planning. In finalize mode -plan DIR takes a path.
	plan := fs.String("plan", "", "plan a bulk run, or supply the draft directory when finalizing")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: gkg repair-validity -classify [-preset NAME] -out DIR")
		fmt.Fprintln(stderr, "       gkg repair-validity -plan [-preset NAME] -manifest M -verdicts V -out DIR")
		fmt.Fprintln(stderr, "       gkg repair-validity -finalize -plan DIR (-rewrite-verdicts V2 | -no-rewrites)")
		fs.PrintDefaults()
	}
	// Normalize the bare planning flag for flag.FlagSet, which cannot assign
	// both a boolean and a value-taking spelling to the same flag.
	const planMode = "\x00"
	parsed := append([]string(nil), args...)
	for i, arg := range parsed {
		if arg == "--" {
			break
		}
		if (arg == "-plan" || arg == "--plan") && (i+1 == len(parsed) || len(parsed[i+1]) > 0 && parsed[i+1][0] == '-') {
			parsed[i] = "-plan=" + planMode
		}
	}
	if err := fs.Parse(parsed); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	seen := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	valid := false
	switch {
	case *classify:
		valid = !*finalize && !seen["plan"] && *preset != "" && *out != "" &&
			!seen["manifest"] && !seen["verdicts"] && !seen["rewrite-verdicts"] && !seen["no-rewrites"]
	case *finalize:
		valid = *plan != "" && *plan != planMode && !seen["out"] && !seen["preset"] &&
			!seen["manifest"] && !seen["verdicts"] && (*noRewrites != (*rewriteVerdicts != ""))
	default:
		valid = *plan == planMode && *preset != "" && *out != "" && *manifest != "" && *verdicts != "" &&
			!seen["rewrite-verdicts"] && !seen["no-rewrites"]
	}
	if fs.NArg() != 0 || !valid {
		fs.Usage()
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "gkg repair-validity: %v\n", err)
		if errors.Is(err, judge.ErrValidityInput) || errors.Is(err, os.ErrExist) {
			return 2
		}
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *finalize {
		digest, err := judge.FinalizeValidity(ctx, *plan, *rewriteVerdicts)
		if err != nil {
			return fail(err)
		}
		if _, err := fmt.Fprintf(stdout, "final_digest=%s\n", digest); err != nil {
			return fail(err)
		}
		return 0
	}
	cfg, err := loadConfig()
	if err != nil {
		return fail(err)
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	defer st.Close(context.WithoutCancel(ctx))

	var message string
	if *classify {
		digest, err := judge.ClassifyValidity(ctx, st, cfg.Labels.PresetsDir, *preset, *out)
		if err != nil {
			return fail(err)
		}
		message = "inventory_digest=" + digest
	} else {
		summary, err := judge.PlanValidity(ctx, st, judge.NewClient(cfg).Model(), cfg.Labels.PresetsDir, *preset, *manifest, *verdicts, *out)
		if err != nil {
			return fail(err)
		}
		message = "draft_digest=" + summary.DraftDigest
	}
	if _, err := fmt.Fprintln(stdout, message); err != nil {
		return fail(err)
	}
	return 0
}
