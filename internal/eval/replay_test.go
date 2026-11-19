package eval

import (
	"strings"
	"testing"
)

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
