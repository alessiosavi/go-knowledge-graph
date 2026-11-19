package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const mergeFoldsUsage = "gkg merge-folds -plan -out FILE [-domain SLUG] [-timeout DURATION]"

func runMergeFolds(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("merge-folds", flag.ContinueOnError)
	fs.SetOutput(stderr)
	plan := fs.Bool("plan", false, "report proposed fold merge groups")
	out := fs.String("out", "", "create a private JSONL file at FILE")
	slug := fs.String("domain", "", "restrict the report to the key scope @SLUG")
	timeout := fs.Duration("timeout", 10*time.Minute, "time limit for connecting and reading the graph")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s\n", mergeFoldsUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if !*plan || *out == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	fail := func(err error, code int) int {
		fmt.Fprintf(stderr, "gkg merge-folds: %v\n", err)
		return code
	}
	if *slug != "" && !graph.ValidDomain(*slug) {
		return fail(fmt.Errorf("invalid domain %q", *slug), 2)
	}
	if *timeout <= 0 {
		return fail(errors.New("-timeout must be positive"), 2)
	}

	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail(err, 1)
	}
	complete := false
	defer func() {
		if !complete {
			_ = file.Close()
			if err := os.Remove(*out); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(stderr, "gkg merge-folds: remove incomplete report: %v\n", err)
			}
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return fail(err, 1)
	}

	cfg, err := loadConfig()
	if err != nil {
		return fail(err, 1)
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, *timeout)
	defer cancel()
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		return fail(err, 1)
	}
	defer st.Close(context.Background())

	result, err := st.PlanMergeFolds(ctx, *slug)
	if err != nil {
		var usageErr argerr.Error
		if errors.As(err, &usageErr) {
			return fail(err, 2)
		}
		return fail(err, 1)
	}
	if err := writeMergeFoldPlan(file, result); err != nil {
		return fail(err, 1)
	}
	if err := file.Close(); err != nil {
		return fail(err, 1)
	}
	complete = true

	s := result.Summary
	_, err = fmt.Fprintf(stdout, "groups=%d members=%d type_mismatches=%d blocking_groups=%d max_rewire=%d p95_rewire=%d stale_hints=%d skipped_rows=%d\n",
		s.Groups, s.Members, s.TypeMismatches, s.BlockingGroups, s.MaxRewire, s.P95Rewire, s.StaleHints, result.Header.SkippedRows)
	if err != nil {
		return fail(err, 1)
	}
	return 0
}

func writeMergeFoldPlan(out io.Writer, plan graph.MergeFoldPlan) error {
	digest := sha256.New()
	enc := json.NewEncoder(io.MultiWriter(out, digest))
	if err := enc.Encode(plan.Header); err != nil {
		return err
	}
	for _, group := range plan.Groups {
		if err := enc.Encode(group); err != nil {
			return err
		}
	}
	return json.NewEncoder(out).Encode(struct {
		SHA256 string `json:"sha256"`
	}{SHA256: fmt.Sprintf("%x", digest.Sum(nil))})
}
