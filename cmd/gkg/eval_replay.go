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

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/eval"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func runEvalSample(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval-sample", flag.ContinueOnError)
	fs.SetOutput(stderr)
	domain := fs.String("domain", "", "domain slug")
	out := fs.String("out", "", "create a private JSONL sample file")
	from := fs.String("from", "", "read inline text documents from a JSONL file")
	docs := fs.Int("docs", 0, "maximum documents to sample (0 exports all)")
	seed := fs.Int64("seed", 0, "document sampling seed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	provided := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if provided["from"] {
		for _, name := range []string{"docs", "seed"} {
			if provided[name] {
				fmt.Fprintf(stderr, "gkg eval-sample: -from cannot be combined with -%s\n", name)
				return 2
			}
		}
	}
	if !graph.ValidDomain(*domain) || *out == "" || *docs < 0 || fs.NArg() > 0 || (provided["from"] && *from == "") {
		fmt.Fprintln(stderr, "usage: gkg eval-sample -domain SLUG -out FILE [-docs N -seed S]")
		fmt.Fprintln(stderr, "       gkg eval-sample -from FILE.jsonl -domain SLUG -out FILE")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval-sample: %v\n", err)
		return 1
	}
	var counts eval.SampleCounts
	if provided["from"] {
		counts, err = eval.WriteSampleFromFile(ctx, cfg.Ingest, *domain, *from, *out)
	} else {
		st, openErr := graph.Open(ctx, cfg)
		if openErr != nil {
			fmt.Fprintf(stderr, "gkg eval-sample: %v\n", openErr)
			return 1
		}
		defer func() { _ = st.Close(context.Background()) }()
		counts, err = eval.WriteSample(ctx, st, *domain, *out, *docs, *seed)
	}
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval-sample: %v\n", err)
		var usageErr argerr.Error
		if provided["from"] && errors.As(err, &usageErr) {
			return 2
		}
		return 1
	}
	fmt.Fprintf(stdout, "documents=%d chunks=%d\n", counts.Documents, counts.Chunks)
	return 0
}

func runEvalReplay(input, output, preset string, opts eval.ReplayOptions, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval: %v\n", err)
		return 1
	}
	counts, err := eval.ReplayWithOptions(ctx, cfg, input, output, preset, version, opts, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval: %v\n", err)
		if errors.Is(err, os.ErrExist) {
			return 2
		}
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
