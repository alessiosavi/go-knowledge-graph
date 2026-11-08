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
		wantExit int
		wantOut  string
		wantErr  string
	}{
		{"version", []string{"version"}, 0, "gkg v", ""},
		{"help", []string{"help"}, 0, "usage: gkg", ""},
		{"help flag", []string{"-h"}, 0, "usage: gkg", ""},
		{"no args", nil, 2, "", "usage: gkg"},
		{"unknown", []string{"bogus"}, 2, "", "unknown command"},
		{"serve unimplemented", []string{"serve"}, 2, "not implemented", ""},
		{"ingest unimplemented", []string{"ingest"}, 2, "not implemented", ""},
		{"eval unimplemented", []string{"eval"}, 2, "not implemented", ""},
		{"migrate unimplemented", []string{"migrate"}, 2, "not implemented", ""},
		{"mcp unimplemented", []string{"mcp"}, 2, "not implemented", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
