package api

import (
	"bytes"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
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

// markdownHTML renders markdown to HTML with goldmark (GFM tables, task
// lists, strikethrough). Raw HTML in the source is never passed through —
// the default renderer omits it — and an AST transformer hardens link and
// image destinations so an ingested document cannot smuggle unsafe schemes
// or remote fetches into a reader's browser.
func markdownHTML(md string) (string, error) {
	converter := goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList),
		goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(destinationSanitizer{}, 100))),
	)
	var buf bytes.Buffer
	if err := converter.Convert([]byte(md), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// destinationSanitizer rewrites unsafe link destinations to "#" and strips
// image destinations that are not relative.
type destinationSanitizer struct{}

// Transform implements parser.ASTTransformer.
func (destinationSanitizer) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Link:
			if !linkAllowed(v.Destination) {
				v.Destination = []byte("#")
			}
		case *ast.Image:
			if !imageAllowed(v.Destination) {
				v.Destination = nil
			}
		}
		return ast.WalkContinue, nil
	})
}

// destScheme returns the lowercased URL scheme of dest, or "" when dest is
// a relative path or a fragment.
func destScheme(dest []byte) string {
	for i, b := range dest {
		switch {
		case b == ':':
			return strings.ToLower(string(dest[:i]))
		case b == '/' || b == '#' || b == '?':
			return ""
		}
	}
	return ""
}

// linkAllowed reports whether a link destination may be kept: http(s),
// mailto, fragments and relative paths.
func linkAllowed(dest []byte) bool {
	switch destScheme(dest) {
	case "", "http", "https", "mailto":
		return true
	default:
		return false
	}
}

// imageAllowed reports whether an image destination may be kept: relative
// paths only, never anything that triggers a remote fetch.
func imageAllowed(dest []byte) bool { return destScheme(dest) == "" }
