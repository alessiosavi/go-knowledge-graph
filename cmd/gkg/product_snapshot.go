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
	"strconv"
	"strings"
	"syscall"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const productsImportUsage = "usage: gkg products import -store S -release ID -snapshot-seq N FILE.jsonl -report OUT.json [-allow-shrink] [-texts-only]"

func printProductsUsage(out io.Writer) {
	for _, line := range []string{productsEmitUsage, productsImportUsage, releaseUsage,
		productsPushUsage, productsRemoveUsage, productsPagesReportUsage, productsMentionsScanUsage, productsMentionsParityUsage, "usage: gkg products delete-store S", "usage: gkg products unbind S"} {
		fmt.Fprintln(out, line)
	}
}

func runProductImport(args []string, stdout, stderr io.Writer) int {
	fs := productFlags("products import", productsImportUsage, stderr)
	var input graph.ProductSnapshotInput
	fs.StringVar(&input.Store, "store", "", "bound store ID")
	fs.StringVar(&input.Release, "release", "", "active release ID")
	fs.BoolVar(&input.AllowShrink, "allow-shrink", false, "approve a drop of more than 10%")
	fs.BoolVar(&input.TextsOnly, "texts-only", false, "materialize only texts for an unchanged snapshot")
	seq := fs.String("snapshot-seq", "", "increasing positive sequence")
	path := fs.String("report", "", "write an import report to this file")
	if err := parseProductFlags(fs, args); err != nil {
		return productFlagExit(err)
	}
	value, err := strconv.ParseInt(*seq, 10, 64)
	if err != nil || value <= 0 || input.Store == "" || input.Release == "" || *path == "" || fs.NArg() != 1 || fs.Arg(0) == "" {
		fs.Usage()
		return 2
	}
	input.Seq = value

	temp, err := stageProductOutput("-report", *path, []string{fs.Arg(0)})
	if err != nil {
		return productCommandError(fs.Name(), stderr, err)
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report := startProductImportReport(input)
	stageReport := func() error {
		if _, err := temp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := temp.Truncate(0); err != nil {
			return err
		}
		if err := json.NewEncoder(temp).Encode(report); err != nil {
			return err
		}
		return temp.Sync()
	}
	importErr := importProducts(ctx, input, fs.Arg(0), &report, stageReport, runWithProductStore)
	if importErr != nil {
		if reportErr := stageReport(); reportErr != nil {
			fmt.Fprintf(stderr, "gkg %s: %v\n", fs.Name(), errors.Join(importErr, reportErr))
			return 1
		}
	}
	if err := finishProductOutput(temp, *path); err != nil {
		if importErr != nil {
			fmt.Fprintf(stderr, "gkg %s: %v\n", fs.Name(), errors.Join(importErr, err))
			return 1
		}
		return productCommandError(fs.Name(), stderr, fmt.Errorf("snapshot %s, but report publication failed: %w", report.Status, err))
	}
	if importErr != nil {
		return productCommandError(fs.Name(), stderr, importErr)
	}
	printProductImportResult(stdout, report)
	return 0
}

func startProductImportReport(input graph.ProductSnapshotInput) graph.ProductImportReport {
	return graph.ProductImportReport{Store: input.Store, Release: input.Release, SnapshotSeq: input.Seq,
		AllowShrink: input.AllowShrink, Rejections: []graph.ProductImportRejection{},
		UnmappedKeys: map[string]int{}, ParseStatus: map[string]map[string]int{}}
}

func importProducts(ctx context.Context, input graph.ProductSnapshotInput, path string, report *graph.ProductImportReport, stageReport func() error,
	withStore func(context.Context, func(context.Context, *graph.Store) error) error) (err error) {
	defer func() {
		if err == nil {
			return
		}
		report.Status = "failed"
		if errors.Is(err, graph.ErrProductData) {
			report.Status = "rejected"
		}
		report.Error = err.Error()
	}()

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open records: %w", err)
	}
	defer f.Close()
	report.SnapshotDigest, err = productobs.ScanImportRecords(ctx, f, input.Store, report.Reject, func(_ int, _ productobs.Record) error {
		report.ProductCount++
		return nil
	})
	if err != nil {
		return err
	}
	if err := report.RejectionError(); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind records: %w", err)
	}
	return withStore(ctx, func(ctx context.Context, st *graph.Store) error {
		return st.ImportProductSnapshot(ctx, input, f, report, stageReport)
	})
}

func printProductImportResult(w io.Writer, report graph.ProductImportReport) {
	fmt.Fprintf(w, "%s store=%s release=%s seq=%d products=%d edges=%d text_action=%s text_count=%d\n",
		report.Status, report.Store, report.Release, report.SnapshotSeq, report.ProductCount, report.EdgeCount,
		report.TextAction, report.TextCount)
}

func runProductReleaseActivate(args []string, stdout, stderr io.Writer) int {
	fs := productFlags("products release activate", releaseActivateUsage, stderr)
	type materialization struct {
		store, file string
		seq         int64
	}
	var files []materialization
	fs.Func("materialize", "import STORE=SEQ:FILE after activation (repeatable)", func(arg string) error {
		store, rest, ok := strings.Cut(arg, "=")
		seq, file, hasFile := strings.Cut(rest, ":")
		n, err := strconv.ParseInt(seq, 10, 64)
		if !ok || !hasFile || store == "" || file == "" || err != nil || n <= 0 {
			return errors.New("expected STORE=SEQ:FILE with a positive sequence")
		}
		files = append(files, materialization{store, file, n})
		return nil
	})
	if err := parseProductFlags(fs, args); err != nil {
		return productFlagExit(err)
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		fs.Usage()
		return 2
	}
	id := fs.Arg(0)
	return withProductStore(fs.Name(), stderr, func(ctx context.Context, st *graph.Store) error {
		if err := st.ActivateProductRelease(ctx, id); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "activated id=%s\n", id)
		for _, file := range files {
			input := graph.ProductSnapshotInput{Store: file.store, Release: id, Seq: file.seq}
			report := startProductImportReport(input)
			err := importProducts(ctx, input, file.file, &report, nil, func(ctx context.Context, run func(context.Context, *graph.Store) error) error {
				return run(ctx, st)
			})
			if err != nil {
				return fmt.Errorf("materialize store %q from %q: %w", file.store, file.file, err)
			}
			printProductImportResult(stdout, report)
		}
		return nil
	})
}

func runProductStoreAdmin(args []string, stdout, stderr io.Writer) int {
	command := "products " + args[0]
	fs := productFlags(command, "usage: gkg "+command+" S", stderr)
	if err := fs.Parse(args[1:]); err != nil {
		return productFlagExit(err)
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		fs.Usage()
		return 2
	}
	store := fs.Arg(0)
	return withProductStore(command, stderr, func(ctx context.Context, st *graph.Store) error {
		if args[0] == "delete-store" {
			if err := st.DeleteProductStore(ctx, store); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "deleted store=%s\n", store)
		} else {
			if err := st.UnbindProductStore(ctx, store); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "unbound store=%s\n", store)
		}
		return nil
	})
}

// Permit flags on either side of the single positional argument.
func parseProductFlags(fs *flag.FlagSet, args []string) error {
	var options, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		options = append(options, arg)
		name, _, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		opt := fs.Lookup(name)
		if hasValue || opt == nil {
			continue
		}
		if b, ok := opt.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
			if i+1 == len(args) {
				return fs.Parse(options)
			}
			i++
			options = append(options, args[i])
		}
	}
	return fs.Parse(append(append(options, "--"), positional...))
}
