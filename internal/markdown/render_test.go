package markdown

import (
	"strings"
	"testing"
)

func TestAnswerRendersGFM(t *testing.T) {
	out, err := RenderAnswer("# Title\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n- item\n- [x] done\n\n```go\nfmt.Println(\"hi\")\n```\n\n~~gone~~\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h1>Title</h1>", "<table>", "<ul>", "<li>item</li>", `<code class="language-go">`, "<del>gone</del>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
}

func TestAnswerSanitizesLinksAndOmitsHTMLAndImages(t *testing.T) {
	for _, dest := range []string{"javascript:alert(1)", "JaVaScRiPt:alert(1)", "data:text/html,bad", "//evil.example/x", `\/\/evil.example/x`, "jav&#x61;script&#58;alert(1)", "data&#58;text/html,bad", "&#47;&#47;evil.example/x", `javascript\:alert(1)`, `\\\\evil.example/x`} {
		t.Run(dest, func(t *testing.T) {
			out, err := RenderAnswer("[link](" + dest + ")")
			if err != nil {
				t.Fatal(err)
			}
			if out != "<p><a href=\"#\">link</a></p>\n" {
				t.Errorf("unsafe link: %s", out)
			}
		})
	}
	for _, dest := range []string{"https://example.com/x", "http://example.com/x", "mailto:a@example.com", "#sec", "/relative/x", "docs/x"} {
		out, err := RenderAnswer("[link](" + dest + ")")
		if err != nil || !strings.Contains(out, `href="`+dest+`"`) {
			t.Errorf("allowed link %q: %s (%v)", dest, out, err)
		}
	}
	out, err := RenderAnswer("<script>alert(1)</script>\n\n<img src=x onerror=alert(2)>\n\n![remote](https://evil.example/x) ![relative](/v1/ingest) ![nested *alt*](local.png)\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<script", "alert(", "<img", "onerror", "src=", "evil.example"} {
		if strings.Contains(out, bad) {
			t.Errorf("unsafe answer contains %q: %s", bad, out)
		}
	}
}
