package api

import (
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/markdown"
)

// foldChunks reconstructs a document's cleaned text from its stored chunks
// in seq order. Chunks are contiguous slices of the source that may repeat
// trailing sentences at the start of the next chunk (sentence overlap), and
// the whitespace between chunks (trimmed spaces, blank lines, heading
// boundaries) is not stored in any chunk. Overlapped bytes are skipped by
// offset; gaps normalize to a paragraph break. The result is lossy in
// whitespace only, never in content.
func foldChunks(chunks []graph.Chunk) string {
	var b strings.Builder
	pos := 0
	for _, c := range chunks {
		if c.Start > pos {
			b.WriteString("\n\n")
		}
		if c.Start < pos {
			skip := pos - c.Start
			if skip >= len(c.Text) {
				continue // the whole chunk is inside already-consumed text
			}
			b.WriteString(c.Text[skip:])
		} else {
			b.WriteString(c.Text)
		}
		pos = c.Start + len(c.Text)
	}
	return b.String()
}

// markdownHTML uses the shared sanitized reader pipeline.
func markdownHTML(md string) (string, error) { return markdown.Render(md) }
