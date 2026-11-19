package eval

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func TestReplayHygieneWithoutValidity(t *testing.T) {
	preset, err := ingest.LoadCheckedPreset(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	const wantSHA = "f5ee1242c4a8077b28d4b1e2ffa982eec6bec3fc69b80d67685ac65dcd999e20"
	if preset.SHA256 != wantSHA {
		t.Errorf("built-in preset SHA-256 = %q, want %q", preset.SHA256, wantSHA)
	}
	header, err := json.Marshal(replayHeader{Type: "header", Hygiene: preset.Hygiene})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(header, &fields); err != nil {
		t.Fatal(err)
	}
	const want = `{"allowlist":{"AI":true,"EU":true,"IT":true,"UE":true,"UK":true,"US":true},"stoplist_exact":{"description":true,"descrizione":true,"index":true,"indice":true,"information":true,"informazioni":true,"introduction":true,"introduzione":true},"stoplist_patterns":["(?i)^Sezione \\d+$","(?i)^Section \\d+$"],"document_scoped_labels":{},"code_tokens":false,"section_label_keep_types":{"location":true}}`
	if got := string(fields["hygiene"]); got != want {
		t.Errorf("replay hygiene JSON = %s, want %s", got, want)
	}
}

func TestReplayRoot(t *testing.T) {
	for _, tc := range []struct {
		name, start string
		duplicates  map[string]string
		root, issue string
	}{
		{"root", "c0", map[string]string{"c0": ""}, "c0", ""},
		{"chain", "c2", map[string]string{"c2": "c1", "c1": "c0", "c0": ""}, "c0", ""},
		{"cycle", "c2", map[string]string{"c2": "c1", "c1": "c0", "c0": "c1"}, "", "cycle"},
		{"self-cycle", "c0", map[string]string{"c0": "c0"}, "", "cycle"},
		{"missing", "c2", map[string]string{"c2": "c1", "c1": "c0"}, "", "outside_sample"},
		{"missing-start", "c0", nil, "", "outside_sample"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, issue := replayRoot(tc.start, tc.duplicates)
			if root != tc.root || issue != tc.issue {
				t.Errorf("replayRoot = (%q, %q), want (%q, %q)", root, issue, tc.root, tc.issue)
			}
		})
	}
}

func TestReplaySidecarImageDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name, image, want string
	}{
		{"bare", digest, digest},
		{"pinned", "synthetic-sidecar:tag@" + digest, digest},
		{"uppercase-hex", "synthetic-sidecar@sha256:" + strings.Repeat("AB", 32), digest},
		{"whitespace", "  " + digest + "\n", digest},
		{"empty", "", "unknown"},
		{"tag", "synthetic-sidecar:tag", "unknown"},
		{"short", "synthetic-sidecar@sha256:ab", "unknown"},
		{"invalid-hex", "sha256:" + strings.Repeat("xz", 32), "unknown"},
		{"other-algorithm", "synthetic-sidecar@sha512:" + strings.Repeat("ab", 64), "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replaySidecarImageDigest(tc.image); got != tc.want {
				t.Errorf("replaySidecarImageDigest = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplayHygieneWithValidity(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	dir := t.TempDir()
	input, output := filepath.Join(dir, "sample.jsonl"), filepath.Join(dir, "predictions.jsonl")
	// A zero-chunk sample exercises the header writer without calling a sidecar.
	if counts, err := WriteSample(ctx, st, "replay-validity", input, 0, 0); err != nil || counts != (SampleCounts{}) {
		t.Fatalf("empty sample = %+v, err = %v", counts, err)
	}
	var cfg config.Config
	cfg.Memgraph.URI = os.Getenv(graphtest.URIEnv)
	cfg.Labels.PresetsDir = "../validity/testdata"
	if _, err := Replay(ctx, cfg, input, output, "invented", "test", io.Discard); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var header struct {
		Type    string
		Preset  string
		Hygiene struct {
			Validity json.RawMessage
		}
	}
	if err := json.NewDecoder(f).Decode(&header); err != nil {
		t.Fatal(err)
	}
	if header.Type != "header" || header.Preset != "invented" {
		t.Fatalf("replay header = %+v", header)
	}
	var lexicon struct{ Rules string }
	if err := json.Unmarshal(header.Hygiene.Validity, &lexicon); err != nil {
		t.Fatal(err)
	}
	if lexicon.Rules != "catalog-v3" {
		t.Errorf("replay validity rules = %q, want catalog-v3", lexicon.Rules)
	}
	const wantDigest = "bdef5d4d09752aba374c43f0a84800206ac9f252cdd65727d4696c538d3f4ed1"
	if got := sha256Hex(header.Hygiene.Validity); got != wantDigest {
		t.Errorf("replay validity lexicon SHA-256 = %s, want %s", got, wantDigest)
	}
}
