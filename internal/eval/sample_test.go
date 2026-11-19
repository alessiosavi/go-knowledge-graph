package eval

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
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

func TestReadSampleFromFile(t *testing.T) {
	const first = "  Café has blue lamps. Marble has green veins. Zorvax has small mugs."
	const second = "# Fixture heading\n\nNimbrix sells lamps."
	const input = `{"id":"record:7","text":"` + first + `","product_id":7,"kind":"record"}` + "\n" +
		`{"id":"page:7","text":"# Fixture heading\n\nNimbrix sells lamps.","product_id":7,"kind":"page"}` + "\n"
	dir := t.TempDir()
	source := filepath.Join(dir, "input.jsonl")
	output := filepath.Join(dir, "sample.jsonl")
	if err := os.WriteFile(source, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.IngestConfig{ChunkWords: 8, OverlapSentences: 1}
	counts, err := WriteSampleFromFile(t.Context(), cfg, "fixture", source, output)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readSample(output)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (SampleCounts{Documents: 2, Chunks: 3}) || got.Header.Counts != counts {
		t.Fatalf("counts = %+v, header counts = %+v", counts, got.Header.Counts)
	}
	record, page := got.Documents[0], got.Documents[1]
	if record.DocID != "record:7" || record.Source != fmt.Sprintf("%x", sha256.Sum256([]byte(first)))[:8] ||
		record.Title != "" || record.Labels != nil || page.DocID != "page:7" || page.Title != "Fixture heading" ||
		page.TitleSource != "h1" || page.Labels != nil {
		t.Fatalf("sample documents = %+v", got.Documents)
	}

	const firstChunk = "Café has blue lamps. Marble has green veins."
	const secondChunk = "Marble has green veins. Zorvax has small mugs."
	want := []sampleChunk{
		{Type: "chunk", DocID: "record:7", ChunkID: "record:7-c0", Seq: 0, Start: 2, End: 2 + len(firstChunk), Text: firstChunk},
		{Type: "chunk", DocID: "record:7", ChunkID: "record:7-c1", Seq: 1, Start: len(first) - len(secondChunk), End: len(first), Text: secondChunk},
		{Type: "chunk", DocID: "page:7", ChunkID: "page:7-c0", Seq: 0, Start: 0, End: len(second), Text: second},
	}
	chunks := append(record.Chunks, page.Chunks...)
	if !slices.Equal(chunks, want) {
		t.Errorf("replay reader chunks = %+v, want %+v", chunks, want)
	}
}
