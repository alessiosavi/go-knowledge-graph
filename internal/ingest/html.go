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
	text, _ := htmlToTextAndTitle(body, pageURL)
	return text, nil
}

func htmlToTextAndTitle(body []byte, pageURL *url.URL) (text, title string) {
	if pageURL == nil {
		pageURL = &url.URL{Scheme: "https", Host: "example.invalid"}
	}
	if article, err := readability.FromReader(bytes.NewReader(body), pageURL); err == nil {
		if text := strings.TrimSpace(article.TextContent); text != "" {
			title := article.Title
			if strings.TrimSpace(title) == "" {
				_, title = htmlFallback(body)
			}
			return text, title
		}
	}
	return htmlFallback(body)
}

// htmlFallback strips markup while collecting the first title element.
func htmlFallback(body []byte) (text, title string) {
	z := html.NewTokenizer(bytes.NewReader(body))
	var b, titleText strings.Builder
	inTitle, seenTitle := false, false
	skip := ""
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.Join(strings.Fields(b.String()), " "), strings.TrimSpace(titleText.String())
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style":
				skip = string(name)
			case "title":
				if !seenTitle && skip == "" {
					inTitle, seenTitle = true, true
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if string(name) == "title" {
				inTitle = false
			}
			if string(name) == skip {
				skip = ""
			}
		case html.TextToken:
			if skip == "" {
				value := z.Text()
				b.Write(value)
				b.WriteByte(' ')
				if inTitle {
					titleText.Write(value)
				}
			}
		}
	}
}
