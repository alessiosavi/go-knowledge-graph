package ingest

import (
	"strings"
	"testing"
)

func TestDeriveTitle(t *testing.T) {
	nineteenLines := strings.Repeat("Fixture prose.\n\n", 19)
	longFrontMatter := "---\nnotes: |\n" + strings.Repeat("  Fixture metadata.\n", 20) + "---\n"
	tests := []struct {
		name, text, source, title, origin string
	}{
		{"front matter first", "\uFEFF---\r\ntitle: \"Élan:   Phase A – Phase B\"\r\n---\r\n# Later\n", "other.md", "Élan: Phase A – Phase B", "frontmatter"},
		{"folded YAML", "---\ntitle: >-\n  Fixture:\n  Revised Edition\n...\n", "", "Fixture: Revised Edition", "frontmatter"},
		{"front matter requires top", "\n---\ntitle: Hidden Fixture\n---\n", "fixture-slug.md", "fixture slug", "slug"},
		{"blank metadata falls through", "---\ntitle: \" \u200b \"\n---\n# Fixture Heading\n", "", "Fixture Heading", "h1"},
		{"malformed metadata falls through", "---\ntitle: [invalid\n---\n# Fixture Heading\n", "", "Fixture Heading", "h1"},
		{"leading thematic break", "---\n\n# Real Title\n\n---\n", "fixture-slug.md", "Real Title", "h1"},
		{"empty mapping leaves body", "---\n{}\n\n# Fixture Heading\n\n---\n", "", "Fixture Heading", "h1"},
		{"sequence leaves body", "---\n- Fixture metadata\n\n# Fixture Heading\n\n---\n", "", "Fixture Heading", "h1"},
		{"mapping without title hides metadata comment", "---\nkind: fixture\n# Hidden Fixture\n---\n# Fixture Heading\n", "", "Fixture Heading", "h1"},
		{"unusable title keeps front matter separate", "---\ntitle: [Fixture metadata]\n# Hidden Fixture\n---\n# Fixture Heading\n", "", "Fixture Heading", "h1"},
		{"line twenty after front matter", longFrontMatter + nineteenLines + "# Fixture Heading\n", "", "Fixture Heading", "h1"},
		{"line twenty-one after front matter", longFrontMatter + nineteenLines + "More prose.\n# Too Late\n", "fixture-slug.md", "fixture slug", "slug"},
		{"ATX first H1", "## Section\n\n# Fixture Topic ###\n\n# Later\n", "other.md", "Fixture Topic", "h1"},
		{"setext before ATX", "Fixture **Heading**\n===\n\n# Later\n", "", "Fixture Heading", "h1"},
		{"setext distinguishes H2", "Fixture Section\n---\n\n# Fixture Heading\n", "", "Fixture Heading", "h1"},
		{"multiline setext", "Fixture\nHeading\n===\n", "", "Fixture Heading", "h1"},
		{"fenced ATX ignored", "```md\n# Code Fixture\n```\n\n# Fixture Heading\n", "", "Fixture Heading", "h1"},
		{"empty H1 ignored", "# ![Fixture image](fixture.png)\n\n# Fixture Heading\n", "", "Fixture Heading", "h1"},
		{"line twenty", nineteenLines + "# Fixture Heading\n", "", "Fixture Heading", "h1"},
		{"line twenty-one", nineteenLines + "More prose.\n# Too Late\n", "fixture-slug.md", "fixture slug", "slug"},
		{"setext text on line twenty", nineteenLines + "Fixture Heading\n===\n", "", "Fixture Heading", "h1"},
		{"setext link on line twenty", nineteenLines + "[Fixture Heading][ref]\n===\n\n[ref]: https://example.invalid/\n", "", "Fixture Heading", "h1"},
		{"setext text on line twenty-one", nineteenLines + "More prose.\n\nToo Late\n===\n", "fixture-slug.md", "fixture slug", "slug"},
		{"markup and punctuation", "# **Élan**: _Phase A_ – `v2.1` &amp; [Example](https://example.invalid/a_(b)) ![Drop me](fixture.png) ###", "", "Élan: Phase A – v2.1 & Example", "h1"},
		{"strikethrough markup", "# ~~Fixture~~ Heading", "", "Fixture Heading", "h1"},
		{"reference link after scan window", "# [Fixture][ref]\n\n" + nineteenLines + "[ref]: https://example.invalid/", "", "Fixture", "h1"},
		{"no prose titles", "**First fixture line**\n\nFixture body.", "index.md", "", ""},
		{"slug query fragment extension", "", "https://example.invalid/test/Élan-est_ici.md?q=other.md#part", "Élan est ici", "slug"},
		{"escaped slug", "", "https://example.invalid/test/Fixture%20%26%20%C3%89lan.md#part", "Fixture & Élan", "slug"},
		{"path fragment", "", "fixtures/Fixture-Élan.md#part?param", "Fixture Élan", "slug"},
		{"windows path", "", `C:\fixtures\Fixture_Name.md`, "Fixture Name", "slug"},
		{"host without a path", "", "https://fixtures.example.invalid", "", ""},
		{"numeric", "", "fixtures/12345.md", "", ""},
		{"unicode numeric", "", "fixtures/١٢٣.md", "", ""},
		{"hex", "", "fixtures/AA01234b56789cDEf.md", "", ""},
		{"stored hash source", "", "ab12cd34", "", ""},
		{"hex path segment", "", "fixtures/aB12cD34.md", "", ""},
		{"short hex", "", "fixtures/aB12cD3.md", "aB12cD3", "slug"},
		{"rune limit", "# " + strings.Repeat("è", 201), "", strings.Repeat("è", 200), "h1"},
		{"empty", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, origin := DeriveTitle(tt.text, tt.source)
			if title != tt.title || origin != tt.origin {
				t.Fatalf("DeriveTitle = (%q, %q), want (%q, %q)", title, origin, tt.title, tt.origin)
			}
		})
	}
	for _, generic := range []string{"Index", "Readme", "Default", "Home", "Page", "Untitled", "Content", "Document"} {
		t.Run("generic "+generic, func(t *testing.T) {
			if title, origin := DeriveTitle("", "fixtures/"+generic+".md#section"); title != "" || origin != "" {
				t.Fatalf("generic slug yielded (%q, %q)", title, origin)
			}
		})
	}
}

func TestDecodeHTMLTitle(t *testing.T) {
	for _, tt := range []struct {
		name, html, title, text string
	}{
		{
			name:  "readability",
			html:  `<html><head><title>Fallback fixture</title><meta property="og:title" content="Élan – Phase A - Phase B"></head><body><article><p>` + strings.Repeat("Synthetic fixture paragraph with enough words for readability. ", 12) + `</p></article></body></html>`,
			title: "Élan – Phase A - Phase B", text: "Synthetic fixture paragraph",
		},
		{
			name:  "title element fallback",
			html:  `<html><head><title>Élan &amp; Fixture - Phase B</title><meta property="og:title" content="Unused fixture metadata"></head><body><script>ignored</script></body></html>`,
			title: "Élan & Fixture - Phase B", text: "Élan & Fixture - Phase B",
		},
		{
			name: "no HTML title",
			html: `<html><body><p>Fixture text.</p></body></html>`,
			text: "Fixture text.",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			loaded, err := Decode([]byte(tt.html), "text/html", nil)
			if err != nil {
				t.Fatal(err)
			}
			wantOrigin := titleSourceHTML
			if tt.title == "" {
				wantOrigin = ""
			}
			if loaded.Title != tt.title || loaded.TitleSource != wantOrigin || !strings.Contains(loaded.Text, tt.text) {
				t.Fatalf("Decode title=%q origin=%q text=%q", loaded.Title, loaded.TitleSource, loaded.Text)
			}
		})
	}
}
