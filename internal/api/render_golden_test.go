package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These outputs were captured on goldmark v1.8.7 before the v2 migration.
// Keep the fixtures byte-for-byte: they pin both the sanitizer and rendering.
func adversarialMarkdown() map[string]string {
	return map[string]string{
		"schemes":    "[js](javascript:alert(1)) [vb](vbscript:msgbox(1)) [data](data:text/html,hello) [encoded](javascript&#58;alert(1)) [escaped](javascript\\:alert(1)) [double](javascript&amp;#58;alert(1))\n",
		"images":     "![js](javascript:alert(1)) ![vb](vbscript:msgbox(1)) ![data](data:image/png;base64,AAAA) ![remote](https://example.com/image.png) ![protocol](//example.com/a.png) ![relative](images/local.png)\n",
		"html":       "<script>alert(1)</script>\n\n<div><img src=x onerror=alert(1)></div>\n\ninline <b>bold</b> and <a href=\"javascript:alert(1)\">link</a> <!-- comment -->\n",
		"autolinks":  "<https://example.com/a?x=1&y=2> <ada@example.com> <javascript:alert(1)> <vbscript:msgbox(1)> <data:text/html,hello>\n",
		"references": "[safe][ok] [bad][js] [vb][vb] [data][data] ![remote][image] ![local][local]\n\n[ok]: https://example.com \"title\"\n[js]: javascript:alert(1)\n[vb]: vbscript:msgbox(1)\n[data]: data:text/html,hello\n[image]: https://example.com/a.png\n[local]: ./local.png\n",
		"emphasis":   "***deep **nested** emphasis*** and **outer *inner* text** and ~~gone *inside*~~\n\n- [x] **done**\n\n| a | b |\n|---|---|\n| *one* | **two** |\n",
		"long-line":  strings.Repeat("x", 128<<10) + " [bad](javascript:alert(1)) ![remote](https://example.com/a.png)\n",
	}
}

func TestMarkdownHTMLAdversarialGolden(t *testing.T) {
	for name, md := range adversarialMarkdown() {
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("testdata", "render", name+".golden"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := markdownHTML(md)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("renderer output differs from the v1 golden (%d bytes, want %d)", len(got), len(want))
			}
		})
	}
}
