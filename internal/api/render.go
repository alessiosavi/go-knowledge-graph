package api

import (
	"bytes"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
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
	source := []byte(md)
	p := parser.New(
		parser.WithExtensions(extension.NewTableParser(), extension.NewStrikethroughParser(), extension.NewTaskListItemParser()),
		parser.WithASTTransformers(util.Prioritized[parser.ASTTransformer](destinationSanitizer{}, 100)),
	)
	r := html.New(html.WithExtensions(extension.NewTableHTMLRenderer(), extension.NewStrikethroughHTMLRenderer(), extension.NewTaskListItemHTMLRenderer()))
	doc := p.Parse(source)
	var buf bytes.Buffer
	if err := r.Render(&buf, source, doc); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// destinationSanitizer rewrites unsafe link destinations to "#" and strips
// image destinations that are not relative. The policy checks run on the
// resolved destination (backslash escapes and HTML entities applied), which
// is the form the renderer emits.
type destinationSanitizer struct{}

// Transform implements parser.ASTTransformer.
func (destinationSanitizer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Link:
			if !linkAllowed(v.Destination.Bytes(reader.Source())) {
				v.Destination = text.NewSingleLineValueFromString("#", nil)
			}
		case *ast.Image:
			if !imageAllowed(v.Destination.Bytes(reader.Source())) {
				v.Destination = text.NewSingleLineValueFromString("", nil)
			}
		}
		return ast.WalkContinue, nil
	})
}

// resolveDestination decodes backslash escapes and HTML references once,
// matching the v2 destination decoder used by the HTML renderer. Checking
// the raw bytes would let escaped schemes or slashes bypass the policy.
func resolveDestination(dest []byte) []byte {
	return text.NewDecoder().Decode(dest)
}

// destScheme returns the lowercased URL scheme of dest, or "" when dest is
// a relative path or a fragment. A protocol-relative destination ("//host",
// in any escaped spelling) reports "//" so the allowlists reject it — it
// points at a remote host despite looking slash-prefixed.
func destScheme(dest []byte) string {
	s := strings.TrimFunc(string(resolveDestination(dest)), func(r rune) bool {
		return r <= ' ' || r == 0x7f
	})
	// Browsers treat backslashes in URLs like slashes; normalize them so a
	// backslash form cannot masquerade as a plain relative path.
	s = strings.ReplaceAll(s, "\\", "/")
	if strings.HasPrefix(s, "//") {
		return "//"
	}
	for i, r := range s {
		switch {
		case r == ':':
			return strings.ToLower(s[:i])
		case r == '/' || r == '#' || r == '?':
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
