package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/quality"
)

func runQuality(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quality", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scope := fs.String("domain", "", "restrict done documents to this domain slug")
	seedPath := fs.String("seed", "", "domain seed JSON file, or - for stdin; longest prefix of the full source")
	asJSON := fs.Bool("json", false, "print aggregate JSON (undefined metrics are null; text prints n/a)")
	sampleN := fs.Int("hygiene-sample", 0, "sample up to N dropped and N renamed names; requires --out")
	sampleSeed := fs.Uint64("sample-seed", 1, "deterministic hygiene sample seed")
	outPath := fs.String("out", "", "private hygiene sample file (mode 0600); requires --hygiene-sample")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: gkg quality [--domain d] [--seed <file>|-] [--json] [--hygiene-sample N --sample-seed s --out <file>]")
		fmt.Fprintln(stderr, "Report read-only metrics over done documents and their derived data; no graph writes or Bootstrap DDL.")
		fmt.Fprintln(stderr, "Noise and samples use the built-in hygiene preset plus ingest.stoplist_file, with entity.type as the label.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *sampleN < 0 || (*sampleN > 0) != (*outPath != "") {
		fmt.Fprintln(stderr, "gkg quality: no positional arguments; --hygiene-sample must be positive and paired with --out")
		return 2
	}
	if *scope != "" && !domain.ValidDomain(*scope) {
		fmt.Fprintln(stderr, "gkg quality: invalid domain slug")
		return 2
	}
	opt := quality.Options{Domain: *scope, HygieneSample: *sampleN, SampleSeed: *sampleSeed}
	if *seedPath != "" {
		var input io.Reader = os.Stdin
		if *seedPath != "-" {
			file, err := os.Open(*seedPath)
			if err != nil {
				fmt.Fprintf(stderr, "gkg quality: seed: %v\n", err)
				return 2
			}
			defer file.Close()
			input = file
		}
		seed, err := domain.LoadSeed(input)
		if err != nil {
			fmt.Fprintf(stderr, "gkg quality: %v\n", err)
			return 2
		}
		opt.Seed = &seed
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(stderr, "gkg quality: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg quality: %v\n", err)
		return 1
	}
	defer st.Close(context.Background())
	// Compute reads SchemaVersion directly. Bootstrap would modify even an empty
	// database, so this read-only command deliberately does not call it.
	report, err := quality.Compute(ctx, st, opt)
	if err != nil {
		fmt.Fprintf(stderr, "gkg quality: %v\n", err)
		return 1
	}
	if *sampleN > 0 {
		if err := writeHygieneSample(*outPath, report.Sample); err != nil {
			fmt.Fprintf(stderr, "gkg quality: write sample: %v\n", err)
			return 1
		}
		if len(report.Sample.Dropped) < *sampleN || len(report.Sample.Renamed) < *sampleN {
			fmt.Fprintf(stderr, "gkg quality: sample availability: dropped=%d renamed=%d requested_per_kind=%d\n", report.Sample.AvailableDropped, report.Sample.AvailableRenamed, *sampleN)
		}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(report)
	} else {
		err = writeQualityText(stdout, report)
	}
	if err != nil {
		fmt.Fprintf(stderr, "gkg quality: report: %v\n", err)
		return 1
	}
	return 0
}

func writeHygieneSample(path string, sample quality.HygieneSample) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	// OpenFile's mode does not restrict an already existing file.
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(sample); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func writeQualityText(w io.Writer, r quality.Report) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	// UseNumber keeps large counts literal (1234567, not 1.234567e+06).
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		return err
	}
	var write func(string, map[string]any) error
	write = func(prefix string, m map[string]any) error {
		keys := make([]string, 0, len(m))
		for key := range m {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			name := strings.TrimPrefix(prefix+"."+key, ".")
			value := m[key]
			if nested, ok := value.(map[string]any); ok {
				if err := write(name, nested); err != nil {
					return err
				}
				continue
			}
			if value == nil {
				value = "n/a"
			}
			if _, err := fmt.Fprintf(w, "%s: %v\n", name, value); err != nil {
				return err
			}
		}
		return nil
	}
	return write("", values)
}
