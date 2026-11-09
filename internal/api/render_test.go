package api

import (
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunk"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// foldFrom splits text with the real chunker and folds the spans back the
// way the content endpoint does.
func foldFrom(t *testing.T, text string, opts chunk.Options) (string, []chunk.Span) {
	t.Helper()
	spans := chunk.Split(text, opts)
	if len(spans) == 0 {
		t.Fatalf("chunk.Split(%q) returned no spans", text)
	}
	gchunks := make([]graph.Chunk, len(spans))
	for i, s := range spans {
		if s.Text != text[s.Start:s.End] {
			t.Fatalf("span %d: Text != text[Start:End] (%q vs %q)", i, s.Text, text[s.Start:s.End])
		}
		gchunks[i] = graph.Chunk{Seq: s.Seq, Text: s.Text, Start: s.Start, End: s.End}
	}
	return foldChunks(gchunks), spans
}

// TestFoldChunksReconstructsOverlapAndHeadingGaps pins the reconstruction
// against real chunker output: trailing-sentence overlap is skipped by byte
// offset and a heading-forced boundary normalizes to "\n\n" without losing
// or duplicating content.
func TestFoldChunksReconstructsOverlapAndHeadingGaps(t *testing.T) {
	text := "Alpha sentence one. Beta sentence two. Gamma sentence three. Delta sentence four.\n\n## Section\n\nEpsilon sentence five. Zeta sentence six."
	got, spans := foldFrom(t, text, chunk.Options{TargetWords: 7, OverlapSentences: 1})
	if len(spans) != 5 {
		t.Fatalf("test setup: got %d spans, want 5 (the fixture pins overlaps and a heading break)", len(spans))
	}
	if spans[1].Start >= spans[0].End {
		t.Fatalf("test setup: spans 0 and 1 do not overlap")
	}
	if spans[3].Start <= spans[2].End {
		t.Fatalf("test setup: span 3 does not start after a heading gap")
	}
	if got != text {
		t.Errorf("fold = %q, want the input text back", got)
	}
	if n := strings.Count(got, "Beta sentence two."); n != 1 {
		t.Errorf("overlapped sentence appears %d times, want once", n)
	}
}

// TestFoldChunksNormalizesInterChunkGaps pins the whitespace-only loss: a
// sentence boundary between chunks (no overlap) comes back as a paragraph
// break.
func TestFoldChunksNormalizesInterChunkGaps(t *testing.T) {
	text := "One two three. Four five six."
	got, spans := foldFrom(t, text, chunk.Options{TargetWords: 3})
	if len(spans) != 2 {
		t.Fatalf("test setup: got %d spans, want 2", len(spans))
	}
	want := "One two three.\n\nFour five six."
	if got != want {
		t.Errorf("fold = %q, want %q", got, want)
	}
}

// TestFoldChunksSkipsFullyCoveredChunk pins the defensive branch: a chunk
// whose bytes are entirely inside already-consumed text contributes nothing
// (skip equal to its length is the boundary).
func TestFoldChunksSkipsFullyCoveredChunk(t *testing.T) {
	chunks := []graph.Chunk{
		{Seq: 0, Text: "Alpha one. Beta two.", Start: 0, End: 20},
		{Seq: 1, Text: "Beta two.", Start: 11, End: 20},
		{Seq: 2, Text: "Gamma three.", Start: 22, End: 34},
	}
	got := foldChunks(chunks)
	want := "Alpha one. Beta two.\n\nGamma three."
	if got != want {
		t.Errorf("fold = %q, want %q", got, want)
	}
}

// TestFoldChunksSingleAndEmpty pins the identity and empty cases.
func TestFoldChunksSingleAndEmpty(t *testing.T) {
	text := "Only one sentence here."
	got, spans := foldFrom(t, text, chunk.Options{})
	if len(spans) != 1 || got != text {
		t.Errorf("single chunk: spans=%d fold=%q, want 1 span and the input back", len(spans), got)
	}
	if got := foldChunks(nil); got != "" {
		t.Errorf("foldChunks(nil) = %q, want empty", got)
	}
}

// TestMarkdownHTMLRendersGFM pins the extensions: headings, tables, fenced
// code, task lists and strikethrough.
func TestMarkdownHTMLRendersGFM(t *testing.T) {
	md := "# Title\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```go\nfmt.Println(\"hi\")\n```\n\n- [x] done\n\n~~gone~~\n"
	out, err := markdownHTML(md)
	if err != nil {
		t.Fatalf("markdownHTML: %v", err)
	}
	for _, want := range []string{"<h1>Title</h1>", "<table>", "<pre>", "<del>gone</del>"} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q\n%s", want, out)
		}
	}
}

// TestMarkdownHTMLNeverEmitsRawHTML pins that embedded raw HTML (a
// prompt-injection vector in ingested documents) never becomes active
// markup.
func TestMarkdownHTMLNeverEmitsRawHTML(t *testing.T) {
	out, err := markdownHTML("<script>alert(1)</script>\n\ninline <b>bold</b> text\n")
	if err != nil {
		t.Fatalf("markdownHTML: %v", err)
	}
	for _, bad := range []string{"<script", "</script", "alert(1)", "<b>"} {
		if strings.Contains(out, bad) {
			t.Errorf("raw HTML leaked: output contains %q\n%s", bad, out)
		}
	}
}

// TestMarkdownHTMLLinkHardening pins the AST link sanitizer: only http(s),
// mailto, fragments and relative destinations survive; everything else
// becomes "#".
func TestMarkdownHTMLLinkHardening(t *testing.T) {
	md := "[ok](https://example.com/x) [mail](mailto:a@b.co) [frag](#sec) [rel](docs/x.md) [bad](javascript:alert(1)) [mixed](JaVaScRiPt:alert(2)) [ftp](ftp://x.example/y)\n"
	out, err := markdownHTML(md)
	if err != nil {
		t.Fatalf("markdownHTML: %v", err)
	}
	for _, want := range []string{`href="https://example.com/x"`, `href="mailto:a@b.co"`, `href="#sec"`, `href="docs/x.md"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q\n%s", want, out)
		}
	}
	for _, bad := range []string{"javascript", "JaVaScRiPt", "ftp://"} {
		if strings.Contains(out, bad) {
			t.Errorf("unsafe destination leaked: output contains %q\n%s", bad, out)
		}
	}
	if n := strings.Count(out, `href="#"`); n != 3 {
		t.Errorf(`href="#" count = %d, want 3 (javascript ×2 + ftp)`, n)
	}
}

// TestMarkdownHTMLImageHardening pins the image sanitizer: only relative
// destinations survive, so an ingested document cannot trigger remote
// fetches from a reader's browser.
func TestMarkdownHTMLImageHardening(t *testing.T) {
	md := "![evil](https://evil.example/x.png) ![ok](img/logo.png) ![d](data:image/png;base64,AAAA)\n"
	out, err := markdownHTML(md)
	if err != nil {
		t.Fatalf("markdownHTML: %v", err)
	}
	if strings.Contains(out, "evil.example") {
		t.Errorf("remote image destination leaked\n%s", out)
	}
	if strings.Contains(out, "data:image") {
		t.Errorf("data: image destination leaked\n%s", out)
	}
	if !strings.Contains(out, `src="img/logo.png"`) || !strings.Contains(out, `alt="ok"`) {
		t.Errorf("relative image was not kept\n%s", out)
	}
}

// TestMarkdownHTMLLinkHardeningEdgeSchemes pins the sanitizer against
// destinations that only look relative before resolution: protocol-relative
// forms (plain and backslash-escaped) and entity-encoded schemes the
// renderer resolves after the transformer runs.
func TestMarkdownHTMLLinkHardeningEdgeSchemes(t *testing.T) {
	md := "[a](//evil.example/x) [b](\\/\\/evil.example/y) [c](tel&#58;+39000000) [d](blob&#58;https://evil.example/z) [e](jav&#x61;script&#58;alert(1))\n"
	out, err := markdownHTML(md)
	if err != nil {
		t.Fatalf("markdownHTML: %v", err)
	}
	for _, bad := range []string{"evil.example", "tel:", "blob:", "alert(1)"} {
		if strings.Contains(out, bad) {
			t.Errorf("unsafe destination leaked: output contains %q\n%s", bad, out)
		}
	}
	if n := strings.Count(out, `href="#"`); n != 5 {
		t.Errorf("href=\"#\" count = %d, want 5 (every hostile form becomes #)\n%s", n, out)
	}
}

// TestMarkdownHTMLImageHardeningEdgeSchemes pins the same guard on images:
// a protocol-relative or entity-encoded destination must be stripped, not
// fetched from a reader's browser.
func TestMarkdownHTMLImageHardeningEdgeSchemes(t *testing.T) {
	md := "![a](//evil.example/x.png) ![b](\\/\\/evil.example/y.png) ![c](data&#58;image/png;base64,AAAA)\n"
	out, err := markdownHTML(md)
	if err != nil {
		t.Fatalf("markdownHTML: %v", err)
	}
	for _, bad := range []string{"evil.example", "base64", "data:"} {
		if strings.Contains(out, bad) {
			t.Errorf("unsafe image destination leaked: output contains %q\n%s", bad, out)
		}
	}
}
