// Command gkg is the single binary for the go-knowledge-graph service:
// REST + MCP server, the ingestion pipeline and the evaluation harness.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
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
	case "pdf-extract":
		// Hidden subcommand: the isolated PDF parser (parent kills on
		// timeout; see ingest.PDFToText).
		return ingest.PDFExtract(os.Stdin, stdout, stderr)
	case "serve", "ingest", "eval", "mcp":
		fmt.Fprintln(stdout, "not implemented")
		return 2
	default:
		fmt.Fprintf(stderr, "gkg: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
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
