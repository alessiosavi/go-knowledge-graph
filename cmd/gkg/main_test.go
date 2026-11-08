package main

import (
	"bytes"
	"strings"
	"testing"
)

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
		{"serve unimplemented", []string{"serve"}, nil, 2, "not implemented", ""},
		{"ingest unimplemented", []string{"ingest"}, nil, 2, "not implemented", ""},
		{"eval unimplemented", []string{"eval"}, nil, 2, "not implemented", ""},
		{"mcp unimplemented", []string{"mcp"}, nil, 2, "not implemented", ""},
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
