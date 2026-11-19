package ingest

import (
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/alessiosavi/GoGPUtils/textnorm"
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"go.yaml.in/yaml/v4"
)

const (
	titleSourceCaller = "caller"
	titleSourcePDF    = "pdf"
	titleSourceHTML   = "html"
	titleHeadingLines = 20
	maxTitleRunes     = 200
)

var (
	titleHygiene   = textnorm.HygienePreset()
	slugSeparators = strings.NewReplacer("-", " ", "_", " ")
)

// DeriveTitle picks the first usable title from leading YAML front matter,
// an early Markdown H1, or the source path.
func DeriveTitle(text, source string) (title, origin string) {
	text = strings.TrimPrefix(text, "\uFEFF")
	body, frontTitle := titleFrontMatter(text)
	if title := normalizeTitle(frontTitle); title != "" {
		return title, "frontmatter"
	}
	if title := markdownTitle(body); title != "" {
		return title, "h1"
	}
	if title := slugTitle(source); title != "" {
		return title, "slug"
	}
	return "", ""
}

func normalizeTitle(title string) string {
	// Convert whitespace controls such as CR to spaces before hygiene
	// removes control runes.
	title = strings.Join(strings.Fields(title), " ")
	// HygienePreset's stages do not return errors.
	title, _ = titleHygiene.Run(title)
	n := 0
	for i := range title {
		if n == maxTitleRunes {
			return strings.TrimSpace(title[:i])
		}
		n++
	}
	return title
}

func titleFrontMatter(text string) (body, title string) {
	first, rest, found := strings.Cut(text, "\n")
	if !found || strings.TrimRight(first, " \t\r") != "---" {
		return text, ""
	}
	offset := 0
	for line := range strings.Lines(rest) {
		if marker := strings.TrimRight(line, " \t\r\n"); marker == "---" || marker == "..." {
			var metadata map[string]yaml.Node
			if err := yaml.Load([]byte(rest[:offset]), &metadata, yaml.WithV3Defaults()); err != nil || len(metadata) == 0 {
				return text, ""
			}
			var title string
			if value, ok := metadata["title"]; ok {
				if err := value.Decode(&title); err != nil {
					title = ""
				}
			}
			return rest[offset+len(line):], title
		}
		offset += len(line)
	}
	return text, ""
}

func titleHeadingBounds(body string) (headingEnd, parseEnd int) {
	count, end := 0, 0
	for line := range strings.Lines(body) {
		if count == titleHeadingLines {
			// A setext underline can follow the last eligible text line.
			return end, end + len(line)
		}
		end += len(line)
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return end, end
}

func markdownTitle(body string) string {
	headingEnd, parseEnd := titleHeadingBounds(body)
	// Skip parsing documents with no possible ATX marker or setext underline
	// in the scan. Parse the full Markdown when reference links are possible
	// so the H1 can use definitions that appear later in the document.
	if !strings.ContainsAny(body[:parseEnd], "#=") {
		return ""
	}
	if !strings.Contains(body[:headingEnd], "[") {
		body = body[:parseEnd]
	}
	source := []byte(body)
	p := parser.New(parser.WithExtensions(extension.NewStrikethroughParser()))
	doc := p.Parse(source)
	for node := doc.FirstChild(); node != nil; node = node.NextSibling() {
		heading, ok := node.(*ast.Heading)
		if !ok || heading.Level != 1 || len(heading.Source()) == 0 {
			continue
		}
		lastLine := heading.Source()[len(heading.Source())-1].Start
		if lastLine >= headingEnd {
			break
		}
		if title := headingText(heading, source); title != "" {
			return title
		}
	}
	return ""
}

func headingText(heading *ast.Heading, source []byte) string {
	var b strings.Builder
	_ = ast.Walk(heading, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			switch n := node.(type) {
			case *ast.Image:
				return ast.WalkSkipChildren, nil
			case *ast.Text:
				b.WriteString(n.Value.Value(source))
				if n.SoftLineBreak() || n.HardLineBreak() {
					b.WriteByte(' ')
				}
			case *ast.CodeSpan:
				b.WriteString(n.Value.Value(source))
			case *ast.AutoLink:
				b.WriteString(n.Label.Value(source))
			}
		}
		return ast.WalkContinue, nil
	})
	return normalizeTitle(b.String())
}

func slugTitle(source string) string {
	if i := strings.IndexAny(source, "?#"); i >= 0 {
		source = source[:i]
	}
	source = strings.ReplaceAll(strings.TrimSpace(source), "\\", "/")
	if strings.Contains(source, "://") || strings.HasPrefix(source, "//") {
		u, err := url.Parse(source)
		if err != nil {
			return ""
		}
		source = u.EscapedPath()
	}
	segment := path.Base(source)
	if decoded, err := url.PathUnescape(segment); err == nil {
		segment = decoded
	}
	segment = strings.TrimSpace(strings.TrimSuffix(segment, path.Ext(segment)))
	if segment == "" || segment == "/" || strings.TrimFunc(segment, unicode.IsDigit) == "" ||
		(len(segment) >= 8 && strings.Trim(segment, "0123456789abcdefABCDEF") == "") {
		return ""
	}
	title := normalizeTitle(slugSeparators.Replace(segment))
	switch strings.ToLower(title) {
	case "index", "readme", "default", "home", "page", "untitled", "content", "document":
		return ""
	}
	return title
}
