// Command mcpsmoke drives a gkg MCP server over stdio with the official SDK
// client: tools/list must return exactly the seven agent tools and an
// optional search call must answer with at least one hit. It is the MCP
// smoke check of scripts/acceptance.sh and of the design's criterion 2
// (the real-agent-client check stays manual, see docs/agent-setup.md).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"

	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"
)

var wantTools = []string{
	"ingest", "status", "search", "get_entity", "traverse", "list_documents", "cypher",
}

func main() {
	query := flag.String("query", "", "full-text query expected to return at least one hit")
	timeout := flag.Duration("timeout", 90*time.Second, "overall timeout")
	flag.Parse()
	cmdArgs := flag.Args()
	if len(cmdArgs) == 0 {
		fail("usage: mcpsmoke [flags] -- <command> [args...]")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stderr = os.Stderr
	client := mcpgo.NewClient(&mcpgo.Implementation{Name: "gkg-acceptance", Version: "v0.1.0"}, nil)
	cs, err := client.Connect(ctx, &mcpgo.CommandTransport{Command: cmd}, nil)
	if err != nil {
		fail("connect %v: %v", cmdArgs, err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		fail("tools/list: %v", err)
	}
	got := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)
	want := append([]string(nil), wantTools...)
	sort.Strings(want)
	if len(got) != len(want) {
		fail("tools/list = %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			fail("tools/list = %v, want %v", got, want)
		}
	}
	fmt.Printf("mcp smoke: tools/list = %v\n", got)

	if *query != "" {
		call, err := cs.CallTool(ctx, &mcpgo.CallToolParams{
			Name:      "search",
			Arguments: map[string]any{"query": *query, "kind": "auto", "limit": 10},
		})
		if err != nil {
			fail("tools/call search: %v", err)
		}
		if call.IsError {
			fail("tools/call search returned an error: %v", call.Content)
		}
		raw, err := json.Marshal(call.StructuredContent)
		if err != nil {
			fail("search content: %v", err)
		}
		var out struct {
			Hits []struct {
				ID string `json:"id"`
			} `json:"hits"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			fail("search content %s: %v", raw, err)
		}
		if len(out.Hits) == 0 {
			fail("search %q returned no hits", *query)
		}
		fmt.Printf("mcp smoke: search %q = %d hits\n", *query, len(out.Hits))
	}
	fmt.Println("mcp smoke: OK")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mcpsmoke: "+format+"\n", args...)
	os.Exit(1)
}
