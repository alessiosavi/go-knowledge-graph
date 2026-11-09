// Command gkg is the single binary for the go-knowledge-graph service:
// REST + MCP server, the ingestion pipeline and the evaluation harness.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/eval"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

const version = "v0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "gkg %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "migrate":
		return runMigrate(stdout, stderr)
	case "eval":
		return runEval(args[1:], stdout, stderr)
	case "pdf-extract":
		// Hidden subcommand: the isolated PDF parser (parent kills on
		// timeout; see ingest.PDFToText).
		return ingest.PDFExtract(os.Stdin, stdout, stderr)
	case "serve", "ingest", "mcp":
		fmt.Fprintln(stdout, "not implemented")
		return 2
	default:
		fmt.Fprintf(stderr, "gkg: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

// runEval runs the golden-set evaluation harness.
func runEval(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	golden := fs.String("golden", "eval/golden/sample.jsonl", "golden JSONL path")
	providerName := fs.String("provider", "", "extractor provider: mock|openai|sidecar (default: config extractor mode)")
	out := fs.String("out", "", "write the report to <out>.md and <out>.json")
	sweep := fs.String("sweep", "", "threshold sweep, e.g. entity=0.2:0.8:0.1 relation=0.1:0.6:0.1")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sweep != "" {
		if _, _, err := eval.ParseSweep(*sweep); err != nil {
			fmt.Fprintf(stderr, "gkg eval: %v\n", err)
			return 2 // usage error
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval: %v\n", err)
		return 1
	}
	name := *providerName
	if name == "" {
		name = cfg.Extractor.Mode
	}
	var prov extract.Provider
	switch name {
	case "mock":
		prov = &extract.Mock{}
	case "openai":
		prov = extract.NewOpenAI(cfg)
	case "sidecar":
		prov = extract.NewSidecar(cfg)
	default:
		fmt.Fprintf(stderr, "gkg eval: unknown provider %q\n", name)
		return 2
	}
	var factJudge ingest.FactJudge
	if mode := cfg.Judge.Mode; mode != "" && mode != "off" {
		factJudge = judge.NewClient(cfg)
	}

	report, err := eval.Run(ctx, cfg, prov, factJudge, *golden, *sweep, *out)
	if err != nil {
		fmt.Fprintf(stderr, "gkg eval: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "entities F1 (lenient): %.3f · relations F1 (lenient): %.3f · judge precision: %.3f · reference coverage: %.3f\n",
		report.AggregateEntities.F1, report.AggregateRelations.F1, report.JudgePrecision, report.ReferenceCoverage)
	if *out != "" {
		fmt.Fprintf(stdout, "report: %s.md %s.json\n", *out, *out)
	}
	return 0
}

// runMigrate opens Memgraph and bootstraps the schema, idempotently.
func runMigrate(stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(stderr, "gkg migrate: %v\n", err)
		return 1
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gkg migrate: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close(ctx) }()
	if err := st.Bootstrap(ctx); err != nil {
		fmt.Fprintf(stderr, "gkg migrate: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "schema ok")
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: gkg <command> [flags]

commands:
  serve     run the REST API, MCP server and ingestion workers
  ingest    ingest a file, URL or stdin into the knowledge graph
  eval      run the golden-set evaluation harness
  migrate   bootstrap the Memgraph schema
  version   print the version
  mcp       run the MCP server over stdio
`)
}
