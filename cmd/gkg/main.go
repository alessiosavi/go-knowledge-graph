// Command gkg is the single binary for the go-knowledge-graph service:
// REST + MCP server, the ingestion pipeline and the evaluation harness.
package main

import (
	"fmt"
	"io"
	"os"
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
	case "serve", "ingest", "eval", "migrate", "mcp":
		fmt.Fprintln(stdout, "not implemented")
		return 2
	default:
		fmt.Fprintf(stderr, "gkg: unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
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
