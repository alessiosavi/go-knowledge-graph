// Package markdown renders untrusted Markdown with a shared destination policy.
package markdown

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

// Render renders markdown to HTML with goldmark (GFM tables, task
// lists, strikethrough). Raw HTML in the source is never passed through —
// the default renderer omits it — and an AST transformer hardens link and
// image destinations so an ingested document cannot smuggle unsafe schemes
// or remote fetches into a reader's browser.
func Render(md string) (string, error) { return render(md, false) }

// RenderAnswer uses the reader's policy but omits every image, including
// relative images, so model output cannot trigger a browser fetch.
func RenderAnswer(md string) (string, error) { return render(md, true) }

func render(md string, noImages bool) (string, error) {
	source := []byte(md)
	p := parser.New(
		parser.WithExtensions(extension.NewTableParser(), extension.NewStrikethroughParser(), extension.NewTaskListItemParser()),
		parser.WithASTTransformers(util.Prioritized[parser.ASTTransformer](destinationSanitizer{noImages: noImages}, 100)),
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
type destinationSanitizer struct{ noImages bool }

// Transform implements parser.ASTTransformer.
func (s destinationSanitizer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	var images []*ast.Image
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
			if s.noImages {
				images = append(images, v)
				return ast.WalkSkipChildren, nil
			}
			if !imageAllowed(v.Destination.Bytes(reader.Source())) {
				v.Destination = text.NewSingleLineValueFromString("", nil)
			}
		}
		return ast.WalkContinue, nil
	})
	for _, img := range images {
		img.Parent().RemoveChild(img)
	}
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
