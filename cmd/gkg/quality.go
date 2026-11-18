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
	"path/filepath"
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
	duplicatesPath := fs.String("duplicates-out", "", "new private duplicate census JSONL file (mode 0600)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: gkg quality [--domain d] [--seed <file>|-] [--json] [--hygiene-sample N --sample-seed s --out <file>] [--duplicates-out <file>]")
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
	if *duplicatesPath != "" && *outPath != "" {
		same, err := sameOutputPath(*duplicatesPath, *outPath)
		if err != nil {
			fmt.Fprintf(stderr, "gkg quality: output paths: %v\n", err)
			return 2
		}
		if same {
			fmt.Fprintln(stderr, "gkg quality: --duplicates-out and --out require different files")
			return 2
		}
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
	var censusFile *os.File
	if *duplicatesPath != "" {
		censusFile, err = stageQualityCensus(*duplicatesPath)
		if err != nil {
			fmt.Fprintf(stderr, "gkg quality: %v\n", err)
			return 1
		}
		defer os.Remove(censusFile.Name())
		defer censusFile.Close()
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
	var report quality.Report
	var census quality.DuplicateSummary
	if censusFile == nil {
		report, err = quality.Compute(ctx, st, opt)
	} else {
		report, census, err = quality.ComputeWithDuplicates(ctx, st, opt, censusFile)
		if err == nil {
			err = censusFile.Close()
		}
	}
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
		if *duplicatesPath == "" {
			enc.SetIndent("", "  ")
			err = enc.Encode(report)
		} else {
			err = enc.Encode(struct {
				quality.Report
				DuplicateCensus quality.DuplicateSummary `json:"duplicate_census"`
			}{report, census})
		}
	} else {
		err = writeQualityText(stdout, report)
		if err == nil && *duplicatesPath != "" {
			_, err = fmt.Fprintf(stdout, "duplicate census groups A=%d other=%d, members A=%d other=%d, edges per group max=%d p95=%d\n",
				census.Groups.A, census.Groups.Other, census.Members.A, census.Members.Other, census.MaxEdges, census.P95Edges)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "gkg quality: report: %v\n", err)
		return 1
	}
	if censusFile != nil {
		// Publish only after the other outputs succeed. Link refuses to replace
		// a destination created while the report was running.
		if err := os.Link(censusFile.Name(), *duplicatesPath); err != nil {
			fmt.Fprintf(stderr, "gkg quality: publish census: %v\n", err)
			return 1
		}
	}
	return 0
}

func stageQualityCensus(path string) (*os.File, error) {
	if _, err := os.Lstat(path); err == nil {
		return nil, &os.PathError{Op: "write census", Path: path, Err: os.ErrExist}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".gkg-duplicates-*")
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}

func sameOutputPath(census, sample string) (bool, error) {
	censusAbs, err := filepath.Abs(census)
	if err != nil {
		return false, err
	}
	for range 40 {
		sampleAbs, err := filepath.Abs(sample)
		if err != nil {
			return false, err
		}
		if censusAbs == sampleAbs {
			return true, nil
		}
		// A symlinked parent can alias a path even when the file is absent.
		if filepath.Base(census) == filepath.Base(sample) {
			censusDir, censusErr := os.Stat(filepath.Dir(census))
			sampleDir, sampleErr := os.Stat(filepath.Dir(sample))
			if censusErr == nil && sampleErr == nil && os.SameFile(censusDir, sampleDir) {
				return true, nil
			}
		}
		// The sample writer follows symlinks, including a link to a target
		// that will be created. Resolve it before publishing either output.
		info, err := os.Lstat(sample)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return false, nil
		}
		target, err := os.Readlink(sample)
		if err != nil {
			return false, err
		}
		if !filepath.IsAbs(target) {
			dir, _ := filepath.Split(sample)
			target = dir + target
		}
		sample = target
	}
	return false, errors.New("too many symbolic links in --out path")
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
