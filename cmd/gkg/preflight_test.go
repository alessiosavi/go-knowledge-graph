package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestUsageListsCommands(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	usage(&out)
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range c.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok {
					continue
				}
				name, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.String(), name) {
					t.Errorf("command %q missing from usage", name)
				}
			}
			return true
		})
		return false
	})
	if !strings.Contains(out.String(), "migrate --schema-only") {
		t.Error("schema-only option missing from usage")
	}
	const productHelp = `  products emit -registry R.json [-release FILE] [-out PATH] FILE.jsonl
      emit canonical product observations
  products release create|activate|list
      manage product releases in Memgraph
`
	if !strings.Contains(out.String(), productHelp) {
		t.Error("usage must place the emit and release descriptions under their respective commands")
	}
	// Preflight exits 3 for a bad seed or an unreadable stoplist overlay;
	// the help line must name both, as deploy.sh and the docs do.
	if !strings.Contains(out.String(), "3=seed or stoplist overlay invalid") {
		t.Error("preflight exit 3 in usage must name the seed and the stoplist overlay")
	}
}

func TestMigrateRejectsArguments(t *testing.T) {
	t.Setenv("GKG_MEMGRAPH_URI", "://invalid")
	for _, arg := range []string{"--bogus", "unexpected", "--schema-only=wrong"} {
		var out, errb bytes.Buffer
		if code := run([]string{"migrate", arg}, &out, &errb); code != 2 {
			t.Errorf("%s: exit %d, stderr %q; want 2", arg, code, &errb)
		}
	}
}

func TestPreflightRejectsArguments(t *testing.T) {
	t.Setenv("GKG_MEMGRAPH_URI", "bolt://127.0.0.1:1")
	for _, arg := range []string{"--bogus", "unexpected", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run([]string{"preflight", arg}, &out, &errb); code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, &errb)
			}
		})
	}
	var out, errb bytes.Buffer
	if code := run([]string{"unknown-acme-command"}, &out, &errb); code != 2 {
		t.Fatalf("unknown command exit %d; want 2", code)
	}
}

// Record Bolt requests while forwarding only to the assigned throwaway database.
// This checks the real command, including store creation, without production hooks.
func TestPreflightDoesNotReadMentions(t *testing.T) {
	st := preflightCommandStore(t)
	migrationCommandDocument(t, st, graph.StatusDone)
	migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema',value:3})`, nil)
	t.Setenv("GKG_DOMAIN_SEED_FILE", "detect-all")
	target, err := url.Parse(os.Getenv("GKG_TEST_MEMGRAPH_URI"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("GKG_MEMGRAPH_URI", "bolt://"+listener.Addr().String())
	var connections sync.WaitGroup
	var mu sync.Mutex
	var requests bytes.Buffer
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer client.Close()
				upstream, err := net.DialTimeout("tcp", target.Host, time.Second)
				if err != nil {
					t.Errorf("connect to test database: %v", err)
					return
				}
				defer upstream.Close()
				var traffic bytes.Buffer
				copied := make(chan struct{})
				go func() {
					_, _ = io.Copy(upstream, io.TeeReader(client, &traffic))
					_ = upstream.Close()
					close(copied)
				}()
				_, _ = io.Copy(client, upstream)
				_ = client.Close()
				<-copied
				mu.Lock()
				requests.Write(traffic.Bytes())
				mu.Unlock()
			}()
		}
	}()
	var out, errb bytes.Buffer
	code := run([]string{"preflight"}, &out, &errb)
	_ = listener.Close()
	<-accepted
	connections.Wait()
	if code != 10 {
		t.Fatalf("exit %d, stderr %q; want 10", code, &errb)
	}
	if !strings.Contains(requests.String(), "MATCH (d:Document)") {
		t.Fatal("no document read captured")
	}
	for _, forbidden := range []string{"MENTIONS", ":Chunk", "name_profile"} {
		if strings.Contains(requests.String(), forbidden) {
			t.Errorf("preflight read unnecessary %s data", forbidden)
		}
	}
}

func TestPreflightReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name, seed   string
		marker, code int
	}{
		{"current", "", 4, 0}, {"missing", "", 3, 3}, {"valid", "valid", 3, 10},
		{"invalid", "invalid", 3, 3}, {"missing-file", "absent.json", 3, 3},
		{"unparseable", "unparseable", 3, 3}, {"unreadable", "directory", 3, 3}, {"invalid-slug", "invalid-slug", 3, 3},
		{"unmatched", "unmatched", 3, 3}, {"detect", "detect-all", 3, 10}, {"newer", "", 5, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := preflightCommandStore(t)
			migrationCommandDocument(t, st, graph.StatusDone)
			migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema',value:$v})`, map[string]any{"v": tc.marker})
			seed := tc.seed
			if seed == "directory" {
				seed = t.TempDir()
			}
			if seed == "valid" || seed == "invalid" || seed == "unmatched" || seed == "unparseable" || seed == "invalid-slug" {
				path := filepath.Join(t.TempDir(), "seed.json")
				data := `{"schema":1,"names":{"acme":"Acme-Max"},"rules":[{"match":"CliMigration","domain":"acme"}]}`
				if seed == "invalid" {
					data = `{"schema":2}`
				}
				if seed == "unparseable" {
					data = `{`
				}
				if seed == "invalid-slug" {
					data = `{"schema":1,"rules":[{"match":"CliMigration","domain":"Invalid!"}]}`
				}
				if seed == "unmatched" {
					data = `{"schema":1,"rules":[{"match":"https://quillon.invalid/","domain":"acme"}]}`
				}
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				seed = path
			}
			t.Setenv("GKG_DOMAIN_SEED_FILE", seed)
			// Neither the spool nor DDL may be needed by preflight.
			t.Setenv("GKG_INGEST_SPOOL_DIR", "/dev/null/unwritable")
			migrationQuery(t, `DROP INDEX ON :Entity(id)`, nil)
			t.Cleanup(func() { migrationQuery(t, `CREATE INDEX ON :Entity(id)`, nil) })
			before := graphtest.Snapshot(t)
			var out, errb bytes.Buffer
			if code := run([]string{"preflight"}, &out, &errb); code != tc.code {
				t.Errorf("exit %d, stdout %q, stderr %q; want %d", code, &out, &errb, tc.code)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want empty", &out)
			}
			if (tc.name == "unmatched" || tc.name == "missing") && (!strings.Contains(errb.String(), "1 unmatched") || !strings.Contains(errb.String(), "CliMigration")) {
				t.Errorf("unmatched diagnostics: %s", &errb)
			}
			fileErrors := map[string]string{
				"invalid": "unsupported seed schema", "missing-file": "no such file or directory",
				"unparseable": "decode seed", "unreadable": "is a directory", "invalid-slug": "invalid seed domain slug",
			}
			if want := fileErrors[tc.name]; want != "" {
				if !strings.Contains(errb.String(), want) || strings.Contains(errb.String(), "unmatched sources") || strings.Contains(errb.String(), "CliMigration") {
					t.Errorf("seed file diagnostic should report %q without coverage: %s", want, &errb)
				}
			}
			before.AssertUnchanged(t, "preflight")
		})
	}
}

func TestMigrateSchemaOnly(t *testing.T) {
	st, id, _ := migrationCommandFixture(t)
	t.Setenv("GKG_EXTRACTOR_MODE", "invalid-provider")
	t.Setenv("GKG_INGEST_SPOOL_DIR", "/dev/null/unwritable")
	var out, errb bytes.Buffer
	if code := run([]string{"migrate", "--schema-only"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, &out, &errb)
	}
	if out.String() != "schema ok\n" {
		t.Errorf("stdout: %q", &out)
	}
	doc, err := st.Document(context.Background(), id)
	if err != nil || doc.Status != graph.StatusChunked {
		t.Fatalf("document=%+v, %v", doc, err)
	}
	if rows := migrationQuery(t, `MATCH (e:Entity) RETURN e`, nil); len(rows) != 0 {
		t.Error("schema-only extracted entities")
	}
}

func TestCommandsNeedMigrationSeed(t *testing.T) {
	for _, cmd := range []string{"serve", "mcp", "migrate"} {
		t.Run(cmd, func(t *testing.T) {
			migrationCommandFixture(t)
			t.Setenv("GKG_DOMAIN_SEED_FILE", "")
			var out, errb bytes.Buffer
			if code := run([]string{cmd}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "v4 migration needs GKG_DOMAIN_SEED_FILE") {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, &out, &errb)
			}
		})
	}
}

func TestMigrationStartupHelper(t *testing.T) {
	command := os.Getenv("GKG_TEST_MIGRATION_COMMAND")
	if command == "" {
		return
	}
	os.Exit(run([]string{command}, os.Stdout, os.Stderr))
}

func TestMigrationSeededStartup(t *testing.T) {
	for _, command := range []string{"serve", "mcp", "migrate"} {
		t.Run(command, func(t *testing.T) {
			st, id, _ := migrationCommandFixture(t)
			migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema',value:3})`, nil)
			seed := domain.Seed{Schema: 1, Names: map[string]string{"acme": "Acme-Max"}}
			for _, r := range migrationQuery(t, `MATCH (d:Document) WHERE d.status <> 'superseded' RETURN DISTINCT d.source AS source`, nil) {
				source, _ := r.Get("source")
				seed.Rules = append(seed.Rules, domain.SeedRule{Match: source.(string), Domain: "acme"})
			}
			data, err := json.Marshal(seed)
			if err != nil {
				t.Fatal(err)
			}
			seedPath := filepath.Join(t.TempDir(), "seed.json")
			if err := os.WriteFile(seedPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GKG_DOMAIN_SEED_FILE", seedPath)
			t.Setenv("GKG_API_KEY", "")
			if command == "migrate" {
				var out, errb bytes.Buffer
				if code := run([]string{command}, &out, &errb); code != 0 {
					t.Fatalf("exit %d: stdout=%q stderr=%q", code, &out, &errb)
				}
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMigrationStartupHelper$")
				cmd.Env = append(os.Environ(), "GKG_TEST_MIGRATION_COMMAND="+command)
				var logs bytes.Buffer
				cmd.Stderr = &logs
				if command == "serve" {
					ln, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					addr := ln.Addr().String()
					ln.Close()
					cmd.Env = append(cmd.Env, "GKG_SERVER_ADDR="+addr)
					cmd.Stdout = &logs
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 1)
					go func() { done <- cmd.Wait() }()
					ready := false
					client := &http.Client{Timeout: 100 * time.Millisecond}
					deadline := time.Now().Add(10 * time.Second)
					for !ready && time.Now().Before(deadline) {
						select {
						case err := <-done:
							t.Fatalf("serve exited before ready: %v: %s", err, &logs)
						default:
						}
						resp, err := client.Get("http://" + addr + "/readyz")
						if err == nil {
							ready = resp.StatusCode == 200
							resp.Body.Close()
						}
						if !ready {
							time.Sleep(20 * time.Millisecond)
						}
					}
					if !ready {
						cancel()
						<-done
						t.Fatalf("serve never ready: %s", &logs)
					}
					if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
					if err := <-done; err != nil {
						t.Fatalf("serve shutdown: %v: %s", err, &logs)
					}
				} else {
					stdin, err := cmd.StdinPipe()
					if err != nil {
						t.Fatal(err)
					}
					stdout, err := cmd.StdoutPipe()
					if err != nil {
						t.Fatal(err)
					}
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					_, err = fmt.Fprintln(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"acme","version":"1"}}}`)
					if err != nil {
						t.Fatal(err)
					}
					line, err := bufio.NewReader(stdout).ReadString('\n')
					if err != nil || !strings.Contains(line, `"result"`) {
						cancel()
						_ = cmd.Wait()
						t.Fatalf("MCP initialization: %q, %v: %s", line, err, &logs)
					}
					_ = stdin.Close()
					if err := cmd.Wait(); err != nil {
						t.Fatalf("MCP shutdown: %v: %s", err, &logs)
					}
				}
			}
			if v, err := st.SchemaVersion(context.Background()); err != nil || v != 4 {
				t.Fatalf("startup marker=%d err=%v", v, err)
			}
			doc, err := st.Document(context.Background(), id)
			if err != nil || doc.Domain != "acme" {
				t.Fatalf("startup domain=%+v, %v", doc, err)
			}
		})
	}
}

func TestPreflightLegacyWithoutLiveDocuments(t *testing.T) {
	for _, tc := range []struct {
		name, query, seed string
		code              int
	}{
		{"empty", "", "", 0},
		{"orphan-entity", `CREATE (:Entity {key:'acme'})`, "", 10},
		{"superseded", `CREATE (:Document {id:'old',source:'https://acme.invalid/old',status:'superseded'})`, "", 3},
		{"superseded-detect", `CREATE (:Document {id:'old',source:'https://acme.invalid/old',status:'superseded'})`, "detect-all", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := preflightCommandStore(t)
			migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema',value:3})`, nil)
			if tc.query != "" {
				migrationQuery(t, tc.query, nil)
			}
			t.Cleanup(func() { wipeCommandGraph(t) })
			t.Setenv("GKG_DOMAIN_SEED_FILE", tc.seed)
			var out, errb bytes.Buffer
			if code := run([]string{"preflight"}, &out, &errb); code != tc.code {
				t.Fatalf("exit=%d want=%d stderr=%s", code, tc.code, &errb)
			}
			if tc.name == "superseded" && (!strings.Contains(errb.String(), "no live documents but legacy rows remain") || strings.Contains(errb.String(), "unmatched")) {
				t.Errorf("superseded-only diagnostic: %s", &errb)
			}
			if v, err := st.SchemaVersion(context.Background()); err != nil || v != 3 {
				t.Fatalf("preflight changed marker: %d, %v", v, err)
			}
		})
	}
}

func TestPreflightUnmatchedLimit(t *testing.T) {
	preflightCommandStore(t)
	t.Cleanup(func() { wipeCommandGraph(t) })
	migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema',value:3})`, nil)
	for i := 0; i < 25; i++ {
		migrationQuery(t, `CREATE (:Document {id:$id,source:$source,status:'done'})`, map[string]any{"id": fmt.Sprintf("acme-%02d", i), "source": fmt.Sprintf("https://acme.invalid/%02d", i)})
	}
	t.Setenv("GKG_DOMAIN_SEED_FILE", "")
	var out, errb bytes.Buffer
	if code := run([]string{"preflight"}, &out, &errb); code != 3 {
		t.Fatalf("exit=%d stderr=%s", code, &errb)
	}
	if !strings.Contains(errb.String(), "25 unmatched") || strings.Count(errb.String(), "https://acme.invalid/") != 20 || strings.Contains(errb.String(), "https://acme.invalid/20") {
		t.Fatalf("unmatched limit: %s", &errb)
	}
}

// Preflight starts and ends with an empty disposable integration graph.
func preflightCommandStore(t *testing.T) *graph.Store {
	t.Helper()
	st := migrationCommandStore(t)
	wipeCommandGraph(t)
	t.Cleanup(func() { wipeCommandGraph(t) })
	return st
}

func wipeCommandGraph(t *testing.T) {
	t.Helper()
	migrationQuery(t, `MATCH (n) WHERE n.embedding IS NOT NULL SET n.embedding = null`, nil)
	migrationQuery(t, `MATCH (n) DETACH DELETE n`, nil)
}
