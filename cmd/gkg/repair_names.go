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
)

func runRepairNames(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repair-names", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "count proposed entity name repairs without writing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gkg repair-names: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fail := func(err error) int {
		fmt.Fprintf(stderr, "gkg repair-names: %v\n", err)
		return 1
	}
	cfg, err := loadConfig()
	if err != nil {
		return fail(err)
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	defer st.Close(context.Background())

	// Repair supports older schemas and uses the standard error for newer ones.
	if _, err := st.ExtractionSchemaCurrent(ctx); err != nil {
		return fail(err)
	}

	scanned, changed, err := st.RepairEntityNames(ctx, *dryRun)
	if err != nil {
		fmt.Fprintf(stderr, "gkg repair-names: %v (scanned=%d changed=%d)\n", err, scanned, changed)
		return 1
	}
	if *dryRun {
		fmt.Fprintf(stdout, "scanned=%d would_change=%d\n", scanned, changed)
	} else {
		fmt.Fprintf(stdout, "scanned=%d changed=%d\n", scanned, changed)
	}
	return 0
}
