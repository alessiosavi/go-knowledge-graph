package eval

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSample(t *testing.T) {
	const header = `{"type":"header","domain":"zorbax","counts":{"documents":1,"chunks":1},"seed":42}` + "\n"
	const document = `{"type":"document","doc_id":"doc-a","title":"Zorvax Note"}` + "\n"
	const chunk = `{"type":"chunk","doc_id":"doc-a","chunk_id":"chunk-a","text":"Zorvax uses Nimbrix."}` + "\n"
	const body = header + document + chunk
	const counts = `{"documents":1,"chunks":1}`
	seal := func(body, counts string) string {
		return body + fmt.Sprintf("{\"type\":\"trailer\",\"counts\":%s,\"sha256\":\"%x\"}\n", counts, sha256.Sum256([]byte(body)))
	}
	valid := seal(body, counts)
	for _, tc := range []struct {
		name, data, want string
	}{
		{"valid-legacy-labels", valid, ""},
		{"invalid-json", seal(header+"{\n", counts), "sample line 2:"},
		{"missing-header", seal(document+chunk, counts), "sample must start with a header"},
		{"invalid-domain", seal(strings.Replace(body, "zorbax", "INVALID", 1), counts), "invalid sample header"},
		{"repeated-header", seal(header+body, counts), "invalid sample header"},
		{"repeated-document", seal(body+document, counts), "invalid or repeated document"},
		{"repeated-chunk", seal(body+chunk, counts), "invalid, repeated or orphaned chunk"},
		{"chunk-before-document", seal(header+chunk+document, counts), "invalid, repeated or orphaned chunk"},
		{"wrong-chunk-owner", seal(header+document+strings.Replace(chunk, "doc-a", "doc-b", 1), counts), "invalid, repeated or orphaned chunk"},
		{"unknown-row", seal(body+`{"type":"unknown"}`+"\n", counts), `unknown row type "unknown"`},
		{"tampered-body", strings.Replace(valid, "Zorvax uses", "Vexnal uses", 1), "sample trailer, counts or SHA-256 do not match"},
		{"header-counts", seal(strings.Replace(body, `"chunks":1`, `"chunks":2`, 1), counts), "sample trailer, counts or SHA-256 do not match"},
		{"trailer-counts", seal(body, `{"documents":1,"chunks":2}`), "sample trailer, counts or SHA-256 do not match"},
		{"trailer-digest", strings.Replace(valid, fmt.Sprintf("%x", sha256.Sum256([]byte(body))), strings.Repeat("0", 64), 1), "sample trailer, counts or SHA-256 do not match"},
		{"rows-after-trailer", valid + chunk, "sample trailer, counts or SHA-256 do not match"},
		{"missing-trailer", body, "sample is missing its trailer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sample.jsonl")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readSample(path)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("readSample error = %v, want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Header.Domain != "zorbax" || got.Header.Seed != 42 || got.Header.Counts != (SampleCounts{Documents: 1, Chunks: 1}) ||
				got.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(tc.data))) ||
				len(got.Documents) != 1 || got.Documents[0].Labels != nil || len(got.Documents[0].Chunks) != 1 ||
				got.Documents[0].Chunks[0].Text != "Zorvax uses Nimbrix." {
				t.Fatalf("unexpected sample: %+v", got)
			}
		})
	}
}
