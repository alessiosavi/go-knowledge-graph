package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/alessiosavi/go-knowledge-graph/internal/eval"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func runEvalSample(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval-sample", flag.ContinueOnError)
	fs.SetOutput(stderr)
	domain := fs.String("domain", "", "domain slug")
	out := fs.String("out", "", "create a private JSONL sample file")
	docs := fs.Int("docs", 0, "maximum documents to sample (0 exports all)")
	seed := fs.Int64("seed", 0, "document sampling seed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !graph.ValidDomain(*domain) || *out == "" || *docs < 0 || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gkg eval-sample -domain SLUG -out FILE [-docs N -seed S]")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval-sample: %v\n", err)
		return 1
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval-sample: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close(context.Background()) }()
	counts, err := eval.WriteSample(ctx, st, *domain, *out, *docs, *seed)
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval-sample: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "documents=%d chunks=%d\n", counts.Documents, counts.Chunks)
	return 0
}

func runEvalReplay(input, output, preset string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval: %v\n", err)
		return 1
	}
	counts, err := eval.Replay(ctx, cfg, input, output, preset, version, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "chunks=%d ok=%d empty=%d errors=%d occurrences=%d stored_winners=%d duplicate_roots_outside_sample=%d duplicate_root_cycles=%d\n",
		counts.Chunks, counts.OK, counts.Empty, counts.Errors, counts.Occurrences, counts.StoredWinners,
		counts.DuplicateRootsOutsideSample, counts.DuplicateRootCycles)
	if counts.Errors > 0 {
		return 1
	}
	return 0
}

func runPreset(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("preset check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: gkg preset check FILE [-stoplist FILE]") }
	stoplist := fs.String("stoplist", "", "ingest stoplist overlay file")
	if len(args) < 2 || args[0] != "check" || args[1] == "" {
		fs.Usage()
		return 2
	}
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if err := ingest.CheckPresetFile(args[1], *stoplist); err != nil {
		fmt.Fprintf(stderr, "gkg preset check: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "preset ok")
	return 0
}
