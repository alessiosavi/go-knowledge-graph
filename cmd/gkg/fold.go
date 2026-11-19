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
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func runFold(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: gkg fold rebuild [-exceptions FILE | -clear-exceptions]"
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	if len(args) == 0 || args[0] != "rebuild" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("fold rebuild", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("exceptions", "", "replace the stored fold exceptions from FILE")
	clear := fs.Bool("clear-exceptions", false, "use an empty exception set")
	fs.Usage = func() {
		fmt.Fprintln(stderr, usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gkg fold rebuild: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	hasFile := false
	fs.Visit(func(f *flag.Flag) { hasFile = hasFile || f.Name == "exceptions" })
	if hasFile && *clear {
		fmt.Fprintln(stderr, "gkg fold rebuild: -exceptions and -clear-exceptions are mutually exclusive")
		return 2
	}
	fail := func(err error, code int) int {
		fmt.Fprintf(stderr, "gkg fold rebuild: %v\n", err)
		return code
	}
	var replacement *graph.FoldExceptions
	switch {
	case hasFile:
		if *path == "" {
			return fail(errors.New("-exceptions requires a file path"), 2)
		}
		file, err := os.Open(*path)
		if err != nil {
			return fail(err, 2)
		}
		ex, err := graph.ParseFoldExceptions(file)
		file.Close()
		if err != nil {
			return fail(fmt.Errorf("read %q: %w", *path, err), 2)
		}
		replacement = &ex
	case *clear:
		replacement = &graph.FoldExceptions{}
	}
	cfg, err := loadConfig()
	if err != nil {
		return fail(err, 1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		return fail(err, 1)
	}
	defer st.Close(context.Background())

	result, err := st.RebuildFold(ctx, replacement)
	for _, key := range result.MissingExceptions {
		fmt.Fprintf(stderr, "gkg fold rebuild: warning: exception key %q names no entity\n", key)
	}
	if err != nil {
		var usageErr argerr.Error
		if errors.As(err, &usageErr) {
			return fail(err, 2)
		}
		return fail(fmt.Errorf("%w (rows_written=%d rows_skipped=%d)", err, result.RowsWritten, result.RowsSkipped), 1)
	}
	fmt.Fprintf(stdout, "rows_written=%d rows_skipped=%d\n", result.RowsWritten, result.RowsSkipped)
	return 0
}
