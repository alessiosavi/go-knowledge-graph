package ingest

import (
	"bytes"
	"net/url"
	"strings"

	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html"
)

// HTMLToText converts an HTML body to readable text: go-readability first,
// with a tag-stripping fallback when no article can be detected.
func HTMLToText(body []byte, pageURL *url.URL) (string, error) {
	if pageURL == nil {
		pageURL = &url.URL{Scheme: "https", Host: "example.invalid"}
	}
	if article, err := readability.FromReader(bytes.NewReader(body), pageURL); err == nil {
		if text := strings.TrimSpace(article.TextContent); text != "" {
			return text, nil
		}
	}
	return stripTags(string(body)), nil
}

// stripTags removes markup with the stdlib tokenizer, skipping script and
// style bodies, and collapses whitespace runs.
func stripTags(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	skip := ""
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.Join(strings.Fields(b.String()), " ")
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			if n := string(name); n == "script" || n == "style" {
				skip = n
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if string(name) == skip {
				skip = ""
			}
		case html.TextToken:
			if skip == "" {
				b.Write(z.Text())
				b.WriteByte(' ')
			}
		}
	}
}
