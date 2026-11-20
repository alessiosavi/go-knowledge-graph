package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductObsCommands(t *testing.T) {
	const fixtures = "../../internal/productobs/testdata/"
	const wantEmitDigest = "d0bc60d6bb94a41026233213c5c50797d77cabdd0849a3b42be1745608c9daf9" // A6 transform of emit-expected.jsonl
	var fixture struct {
		Registry json.RawMessage
		Records  []json.RawMessage
	}
	input, err := os.ReadFile(fixtures + "emit-input.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(input, &fixture); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	registry := write("registry.json", fixture.Registry)
	wantCheck := fmt.Sprintf("sha256 %x registry\nstores=1 features=4 mapped=9\n", sha256.Sum256(fixture.Registry))
	var buf bytes.Buffer
	for _, record := range fixture.Records {
		if err := json.Compact(&buf, record); err != nil {
			t.Fatal(err)
		}
		buf.WriteByte('\n')
	}
	records := write("records.jsonl", buf.Bytes())
	bad := write("rejected.jsonl", []byte(`{"secret":"one","secret":"two"}`+"\n"))
	badRegistry := write("invalid.json", []byte(`{"schema":"gkg-features/v0","features":[],"stores":{}}`))
	out := filepath.Join(dir, "output.jsonl")
	const priorOutput = "prior output\n"
	guard := write("existing-output.jsonl", []byte(priorOutput))
	checkEmit := func(t *testing.T, data []byte) {
		t.Helper()
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != wantEmitDigest {
			t.Fatalf("emit SHA-256 = %s, want %s", got, wantEmitDigest)
		}
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"help", []string{"help"}, 0},
		{"emit help", []string{"products", "emit", "-h"}, 0},
		{"check", []string{"features", "check", registry}, 0},
		{"check usage", []string{"features", "check"}, 2},
		{"check invalid", []string{"features", "check", badRegistry}, 1},
		{"check read failure", []string{"features", "check", filepath.Join(dir, "missing.json")}, 1},
		{"emit usage", []string{"products", "emit", records}, 2},
		{"emit without sidecar", []string{"products", "emit", "-registry", registry, records}, 0},
		{"removed partition flag", []string{"products", "emit", "-registry", registry, "-partition", "unused.json", records}, 2},
		{"file", []string{"products", "emit", "-registry", registry, "-out", out, records}, 0},
		{"invalid row", []string{"products", "emit", "-registry", registry, "-out", guard, bad}, 1},
		{"input collision", []string{"products", "emit", "-registry", registry, "-out", records, records}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != tc.code {
				t.Fatalf("exit = %d, want %d; stderr: %s", code, tc.code, &stderr)
			}
			if tc.name == "help" || tc.name == "emit help" {
				help := stdout.String() + stderr.String()
				if !strings.Contains(help, "products emit -registry") || strings.Contains(help, "-partition") {
					t.Fatal("expected emit help without the partition flag")
				}
			}
			if tc.name == "check" && stdout.String() != wantCheck {
				t.Fatalf("stdout = %q, want %q", stdout.String(), wantCheck)
			}
			if tc.name == "emit without sidecar" {
				checkEmit(t, stdout.Bytes())
			}
			if tc.name == "file" {
				data, err := os.ReadFile(out)
				if err != nil || stdout.Len() != 0 {
					t.Fatalf("private file output check failed: %v", err)
				}
				checkEmit(t, data)
				info, err := os.Stat(out)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("expected output mode 0600: %v", err)
				}
			}
			if tc.name == "invalid row" {
				data, err := os.ReadFile(guard)
				if err != nil || string(data) != priorOutput {
					t.Fatalf("output changed after record rejection: %v", err)
				}
			}
			if tc.name == "input collision" {
				data, err := os.ReadFile(records)
				if err != nil || !bytes.Equal(data, buf.Bytes()) {
					t.Fatalf("input changed: %v", err)
				}
			}
			if tc.code != 0 && (stderr.Len() == 0 || stdout.Len() != 0 || strings.Contains(stderr.String(), "secret")) {
				t.Fatalf("unexpected error reporting: stdout %d bytes, stderr %q", stdout.Len(), &stderr)
			}
		})
	}
}
