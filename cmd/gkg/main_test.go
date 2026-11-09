package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBinaryDoesNotDependOnTesting pins that production packages never pull
// the Go test framework into the gkg binary: test support for a package must
// live in its own package, imported only from _test.go files.
func TestBinaryDoesNotDependOnTesting(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./cmd/gkg")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/gkg: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "testing" {
			t.Fatal("the gkg binary transitively imports the testing package; keep test support out of production packages")
		}
	}
}

func TestRunDispatch(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		env      map[string]string
		wantExit int
		wantOut  string
		wantErr  string
	}{
		{"version", []string{"version"}, nil, 0, "gkg v", ""},
		{"help", []string{"help"}, nil, 0, "usage: gkg", ""},
		{"help flag", []string{"-h"}, nil, 0, "usage: gkg", ""},
		{"no args", nil, nil, 2, "", "usage: gkg"},
		{"unknown", []string{"bogus"}, nil, 2, "", "unknown command"},
		{"serve bad uri", []string{"serve"}, map[string]string{"GKG_MEMGRAPH_URI": "://bad"}, 1, "", "gkg serve:"},
		{"ingest no args", []string{"ingest"}, nil, 2, "", "gkg ingest:"},
		{"ingest bad uri", []string{"ingest", "sample.md"}, map[string]string{"GKG_MEMGRAPH_URI": "://bad"}, 1, "", "gkg ingest:"},
		{"mcp bad uri", []string{"mcp"}, map[string]string{"GKG_MEMGRAPH_URI": "://bad"}, 1, "", "gkg mcp:"},
		{"eval missing golden", []string{"eval", "--golden", "/nonexistent-golden.jsonl"}, map[string]string{"GKG_EXTRACTOR_MODE": "mock"}, 1, "", "gkg eval:"},
		{"migrate bad uri", []string{"migrate"}, map[string]string{"GKG_MEMGRAPH_URI": "://bad"}, 1, "", "gkg migrate:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var out, errb bytes.Buffer
			code := run(tc.args, &out, &errb)
			if code != tc.wantExit {
				t.Errorf("exit = %d, want %d", code, tc.wantExit)
			}
			if tc.wantOut == "" {
				if out.Len() != 0 {
					t.Errorf("stdout = %q, want empty", out.String())
				}
			} else if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", out.String(), tc.wantOut)
			}
			if tc.wantErr == "" {
				if errb.Len() != 0 {
					t.Errorf("stderr = %q, want empty", errb.String())
				}
			} else if !strings.Contains(errb.String(), tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", errb.String(), tc.wantErr)
			}
		})
	}
}
