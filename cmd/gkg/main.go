// Command gkg is the single binary for the go-knowledge-graph service:
// REST + MCP server, the ingestion pipeline and the evaluation harness.
package main

import (
	"fmt"
	"os"
)

const version = "v0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Printf("gkg %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	case "serve", "ingest", "eval", "migrate", "mcp":
		fmt.Println("not implemented")
		return 2
	default:
		fmt.Fprintf(os.Stderr, "gkg: unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: gkg <command> [flags]

commands:
  serve     run the REST API, MCP server and ingestion workers
  ingest    ingest a file, URL or stdin into the knowledge graph
  eval      run the golden-set evaluation harness
  migrate   bootstrap the Memgraph schema
  version   print the version
  mcp       run the MCP server over stdio
`)
}
