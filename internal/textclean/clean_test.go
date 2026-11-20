package textclean

import (
	"math/rand"
	"strings"
	"testing"
)

const (
	extraGlyphLines   = "\n- ظ Second item\n- ظ Third item"
	cleanedExtraLines = "\n- Second item\n- Third item"
)

func TestCleanBidiControls(t *testing.T) {
	const bullet = "\u202b ظ\u202cUna sola sequenza operativa, solo 3 strumenti"
	const cleaned = "Una sola sequenza operativa, solo 3 strumenti"
	for _, tt := range []struct {
		name, raw, want string
		stats           Stats
	}{
		{"leading wrapped glyph", bullet + extraGlyphLines, cleaned + cleanedExtraLines, Stats{BidiControls: 2, GlyphLines: 3}},
		{"indented wrapped glyph", "  \u202b ظ\u202cUna" + extraGlyphLines, "  Una" + cleanedExtraLines, Stats{BidiControls: 2, GlyphLines: 3}},
		{"markdown wrapped glyph", "- \u202b ظ\u202cUna" + extraGlyphLines, "- Una" + cleanedExtraLines, Stats{BidiControls: 2, GlyphLines: 3}},
		{"indented wrapped glyph mid-document", "Title\r\n\t  " + bullet + "\rEnd" + extraGlyphLines, "Title\r\n\t  " + cleaned + "\rEnd" + cleanedExtraLines, Stats{BidiControls: 2, GlyphLines: 3}},
		{"bidi before indentation and trailing hard break", "\u202b\t  ظ\u202c" + cleaned + "  " + extraGlyphLines, cleaned + "  " + cleanedExtraLines, Stats{BidiControls: 2, GlyphLines: 3}},
		{"bidi before list marker", "  \u202b- ظ\u202c" + cleaned + extraGlyphLines, "  - " + cleaned + cleanedExtraLines, Stats{BidiControls: 2, GlyphLines: 3}},
		{"bidi before indentation and list marker", "\u202b\t  2) ظ\u202cUna" + extraGlyphLines, "2) Una" + cleanedExtraLines, Stats{BidiControls: 2, GlyphLines: 3}},
		{"wrapped glyph below threshold", "\u202b ظ\u202cUna sola", " ظ Una sola", Stats{BidiControls: 2}},
		{"real RTL document", bullet + extraGlyphLines + "\nBefore\u202bمرحبا بالعالم\u202cafter", " ظ " + cleaned + extraGlyphLines + "\nBefore مرحبا بالعالم after", Stats{BidiControls: 4}},
		{"embedding override and isolate controls", "A\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069B", "AB", Stats{BidiControls: 9}},
		{"bidi inside Latin word", "abc\u202bdef", "abcdef", Stats{BidiControls: 1}},
		{"bidi between spaces", "x \u202b y", "x  y", Stats{BidiControls: 1}},
		{"RTL boundary with existing whitespace", "ظ\u202b\t\u202cLatin ظ\u2069\r\nLatin ظ\u2067\u00a0Latin", "ظ\tLatin ظ\r\nLatin ظ\u00a0Latin", Stats{BidiControls: 4}},
		{"combining mark after RTL letter", "ظ\u202bّ", "ظّ", Stats{BidiControls: 1}},
		{"RTL boundary after combining mark", "ظ\u202bّ\u202cX", "ظّ X", Stats{BidiControls: 2}},
		{"spacing and enclosing combining marks", "ظ\u202b\u0903\u202c\u20dd\u2069X", "ظ\u0903\u20dd X", Stats{BidiControls: 3}},
		{"one space per script boundary", "\u202bA\u202a\u2067ظ\u2069\u202cB\u202c", "A ظ B", Stats{BidiControls: 6}},
		{"bidi controls only", "\u2067\u2069", "", Stats{BidiControls: 2}},
		{"keep directional marks", "A\u200eB\u200fC", "A\u200eB\u200fC", Stats{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checkClean(t, tt.raw, tt.want, tt.stats)
		})
	}
}

func TestBidiIdempotenceBoundary(t *testing.T) {
	const meem = "\u0645"
	const lines = "\n- " + meem + " First\n- " + meem + " Second"
	checkClean(t, meem+"\u202b"+meem+"\u202c"+lines, meem+meem+lines, Stats{BidiControls: 2})
}

func TestClean(t *testing.T) {
	for _, tt := range []struct {
		name, raw, want string
		stats           Stats
	}{
		{"invalid byte runs", "A\xff\xfeB�\xed\xa0\x80C", "A�B��C", Stats{InvalidUTF8: 2, FFFDRemaining: 3}},
		{"controls", "a\x00b\x07c\x1bd\x7fe\u0085f\tg\nh\ri", "abcdef\tg\nh\ri", Stats{Controls: 5}},
		{"keep Unicode formatting", "e\u0301 ﬁ Ａ\u200c\u200d\u200e\u200f", "e\u0301 ﬁ Ａ\u200c\u200d\u200e\u200f", Stats{}},
		{"literal Hebrew letter", "The Hebrew letter א is the first letter of the alphabet.", "The Hebrew letter א is the first letter of the alphabet.", Stats{}},
		{"literal Arabic letter", "He wrote و in the margin.", "He wrote و in the margin.", Stats{}},
		{"mixed lone letters", "denti sui ظ La lacca è trasparente\ncite the letter א here", "denti sui ظ La lacca è trasparente\ncite the letter א here", Stats{}},
		{"lowercase before glyph", "- cite و here", "- cite و here", Stats{}},
		{"lowercase before glyph with gate open", "  - cite  ظ here  " + extraGlyphLines, "  - cite  ظ here  " + cleanedExtraLines, Stats{GlyphLines: 2}},
		{"non-list uppercase before glyph with gate open", "words These ظ here" + extraGlyphLines, "words These ظ here" + cleanedExtraLines, Stats{GlyphLines: 2}},
		{"Hebrew alphabet", "א Alef\nב Bet\nג Gimel\nא Alef\nא Alef", "א Alef\nב Bet\nג Gimel\nא Alef\nא Alef", Stats{}},
		{"Hebrew letter drill", "א aleph\nא aleph\nא aleph", "א aleph\nא aleph\nא aleph", Stats{}},
		{"prose after glyph list", "- ظ A\n- ظ B\n- ظ C\nظ is the letter dha in Arabic", "- A\n- B\n- C\nظ is the letter dha in Arabic", Stats{GlyphLines: 3}},
		{"bidi on another line does not qualify prose", "\u202b- ظ\u202c A\n- ظ B\n- ظ C\nظ is the letter dha in Arabic", "- A\n- B\n- C\nظ is the letter dha in Arabic", Stats{BidiControls: 2, GlyphLines: 3}},
		{"empty bidi line does not qualify following prose", "- ظ A\n- ظ B\n- ظ C\r\u202b\nظ is the letter dha in Arabic", "- A\n- B\n- C\r\nظ is the letter dha in Arabic", Stats{BidiControls: 1, GlyphLines: 3}},
		{"only two glyph lines", "- ظ Una voce\n- 3 ظ ظ strumenti", "- ظ Una voce\n- 3 ظ ظ strumenti", Stats{}},
		{"bidi inside RTL word", "م\u202bر\u202c", "مر", Stats{BidiControls: 2}},
		{"bidi inside RTL word with artifact lines", "م\u202bر\u202c\n- ظ First\n- ظ Second\n- ظ Third", "مر\n- ظ First\n- ظ Second\n- ظ Third", Stats{BidiControls: 2}},
		{"reported reversal", "- denti sui invisibile totalmente e trasparente è lacca La ظ" + extraGlyphLines, "- La lacca è trasparente e totalmente invisibile sui denti" + cleanedExtraLines, Stats{GlyphLines: 3}},
		{"leading glyph", "\u202bظ\u202c Una sola ظ sequenza" + extraGlyphLines, "Una sola sequenza" + cleanedExtraLines, Stats{BidiControls: 2, GlyphLines: 3}},
		{"middle glyph", "+ strumenti 3 Solo ظ Una sequenza" + extraGlyphLines, "+ Solo 3 strumenti Una sequenza" + cleanedExtraLines, Stats{GlyphLines: 3}},
		{"wrapped list continuation", "- parodontali, splintaggi di caso in adatto versatile: e all'uso Pronto ظ splintaggi ortodontici e rinforzi protesici" + extraGlyphLines, "- Pronto all'uso e versatile: adatto in caso di splintaggi parodontali, splintaggi ortodontici e rinforzi protesici" + cleanedExtraLines, Stats{GlyphLines: 3}},
		{"digit before glyph", "- polimerizzazione di modalità 3 ظ" + extraGlyphLines, "- 3 modalità di polimerizzazione" + cleanedExtraLines, Stats{GlyphLines: 3}},
		{"multiple glyphs", "1. operativa sequenza ظ sola La ظ Una" + extraGlyphLines, "1. La sola sequenza operativa Una" + cleanedExtraLines, Stats{GlyphLines: 3}},
		{"multiple glyphs with continuation", "2) iniziale. frase ظ La ظ Seconda frase." + extraGlyphLines, "2) La frase iniziale. Seconda frase." + cleanedExtraLines, Stats{GlyphLines: 3}},
		{"glyphs only", "  \u202bظ ظ\u202c  " + extraGlyphLines, "    " + cleanedExtraLines, Stats{BidiControls: 2, GlyphLines: 3}},
		{"markdown prefixes and line endings", "  - fini Altri ظ\r\n\t* ظ Prima voce\r## titolo Primo ظ\n", "  - Altri fini\r\n\t* Prima voce\r## titolo Primo ظ\n", Stats{GlyphLines: 2}},
		{"indentation and trailing whitespace", "\t  \u202bظ\u202c Prima voce\t  " + extraGlyphLines, "\t  Prima voce\t  " + cleanedExtraLines, Stats{BidiControls: 2, GlyphLines: 3}},
		{"reversal and trailing hard break", "* finale Frase ظ dopo  " + extraGlyphLines, "* Frase finale dopo  " + cleanedExtraLines, Stats{GlyphLines: 3}},
		{"real Arabic anywhere", "ظ Una voce" + extraGlyphLines + "\n\nمرحبا بالعالم", "ظ Una voce" + extraGlyphLines + "\n\nمرحبا بالعالم", Stats{}},
		{"RTL letters inside token", "ظ Una voce" + extraGlyphLines + "\naظbא", "ظ Una voce" + extraGlyphLines + "\naظbא", Stats{}},
		{"glued glyph and bidi mark", "denti sui .ظ\n\u200f Una voce", "denti sui .ظ\n\u200f Una voce", Stats{}},
		{"long index leader", "Entry " + strings.Repeat("�", 99) + " 26", "Entry … 26", Stats{LeaderRuns: 1, FFFDRemoved: 99}},
		{"dot threshold", "Short.......26\nLong........26", "Short.......26\nLong … 26", Stats{LeaderRuns: 1}},
		{"mixed leader groups", "Entry........\t���� ........26", "Entry … 26", Stats{LeaderRuns: 1, FFFDRemoved: 4}},
		{"short mixed groups", "Entry ....�� ..��", "Entry ....�� ..��", Stats{FFFDRemaining: 4}},
		{"ellipsis and middle dot", "Entry …………26\nOther ····", "Entry … 26\nOther … ", Stats{LeaderRuns: 2}},
		{"leaders without left context", " ........26\n\t���� 27\r\n........28\r····29", " ........26\n\t���� 27\r\n........28\r····29", Stats{FFFDRemaining: 4}},
		{"leading Unicode spacing", "\u00a0........26", "\u00a0........26", Stats{}},
		{"unattached decoding loss", "��� ........26\n�", "��� ........26\n�", Stats{FFFDRemaining: 4}},
		{"single replacement", "Entry � 26", "Entry � 26", Stats{FFFDRemaining: 1}},
		{"cleaning order", "- 26 ���� Ent\x00ry ظ" + extraGlyphLines, "- Entry … 26" + cleanedExtraLines, Stats{Controls: 1, GlyphLines: 3, LeaderRuns: 1, FFFDRemoved: 4}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checkClean(t, tt.raw, tt.want, tt.stats)
		})
	}
}

func TestCleanFencedCode(t *testing.T) {
	const (
		outside      = "- voce Prima ظ\nTitle........26"
		cleanOutside = "- Prima voce\nTitle … 26"
		code         = "- ظ Code........26\n- א More����26"
		backticks    = "````info........26 א\n" + code + "\n```\n~~~\n````` suffix\n" + code + "\n   `````\t"
		tildes       = " ~~~info\n" + code + "\n   ~~~~"
		nested       = "> ```go\n> Name........26\n> ```"
	)
	for _, tt := range []struct {
		name, raw, want string
		stats           Stats
	}{
		{"backtick length and character", outside + "\n" + backticks + extraGlyphLines, cleanOutside + "\n" + backticks + cleanedExtraLines, Stats{GlyphLines: 3, LeaderRuns: 1, FFFDRemaining: 8}},
		{"tilde with CRLF", strings.ReplaceAll(outside+"\n"+tildes+extraGlyphLines, "\n", "\r\n"), strings.ReplaceAll(cleanOutside+"\n"+tildes+cleanedExtraLines, "\n", "\r\n"), Stats{GlyphLines: 3, LeaderRuns: 1, FFFDRemaining: 4}},
		{"unclosed fence with bare CR", outside + extraGlyphLines + "\r~~~\r" + code, cleanOutside + cleanedExtraLines + "\r~~~\r" + code, Stats{GlyphLines: 3, LeaderRuns: 1, FFFDRemaining: 4}},
		{"fenced glyphs do not count toward threshold", outside + "\n```\n- ظ code\n- ظ more code\n```", "- voce Prima ظ\nTitle … 26\n```\n- ظ code\n- ظ more code\n```", Stats{LeaderRuns: 1}},
		{"nested fence", outside + "\n\n" + nested + "\n" + extraGlyphLines, cleanOutside + "\n\n" + nested + "\n" + cleanedExtraLines, Stats{GlyphLines: 3, LeaderRuns: 1}},
		{"repaired list item exposes a leader", "- ظ A\n- ظ B\n- \u202b ظ\u202cC\n  ```\nX........26", "- A\n- B\n- C\n  ```\nX … 26", Stats{BidiControls: 2, GlyphLines: 3, LeaderRuns: 1}},
		{"repaired list item exposes more glyphs", "- ظ A\n- ظ B\n- \u202b ظ\u202cC\n  ```\nظ\u202b\t\u202cLatin\n- A ظ B\n- ظ D", "- A\n- B\n- C\n  ```\nLatin\n- A B\n- D", Stats{BidiControls: 4, GlyphLines: 6}},
		{"erased glyph line preserves original bidi context", "Title\r\u202bظ\u202c\n- ظ A\n- ظ B\n- \u202b ظ\u202cC\n  ```\nظ\u202b\t\u202cLatin\n- A ظ B\n- ظ D", "Title\r\n- A\n- B\n- C\n  ```\nLatin\n- A B\n- D", Stats{BidiControls: 6, GlyphLines: 7}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checkClean(t, tt.raw, tt.want, tt.stats)
		})
	}
}

func TestCleanDocumentIdempotence(t *testing.T) {
	// Use the review's line alphabet so short inputs still reach the glyph
	// threshold and exercise interactions with Markdown fences.
	lines := []string{
		"- ظ A", "- ظ B", "- ظ C", "- ظ", "ظ Una", "- ظ Una sola", "  ظ Uno",
		"- ظ A", "- ظ 3", "- ظ A ظ", "- A ظ B", "- ظ A ظ B ظ C",
		"- cite ظ here", "text ظ more", "ظ", "- ظ", "",
		"```", "~~~", "````", "```go", "    ```", "  ```",
		"- א A", "א Una", "א", "- א B",
		"م ر", "مم", "مرحبا", "Before\u202bمرحبا\u202cafter", "م\u202bم\u202c",
		"\u202b ظ\u202cUna", "- \u202b ظ\u202cUna", "  \u202b ظ\u202cUna",
		"\u202b- ظ\u202cUna", "A\u202bظ\u202cB", "ظ\u202b\t\u202cLatin",
		"Title........26", "Entry … 26", "Sh.......26", "Name....��26",
		"- 26 ��� Ent\x00ry ظ", "\u202b\t  2) ظ\u202cUna",
	}
	rng := rand.New(rand.NewSource(999))
	for i := range 5000 {
		var doc strings.Builder
		for range 1 + rng.Intn(9) {
			doc.WriteString(lines[rng.Intn(len(lines))])
			doc.WriteByte('\n')
		}
		raw := doc.String()
		once, _ := Clean(raw)
		if twice, _ := Clean(once); twice != once {
			t.Fatalf("document %d, input %q: first pass %q, second pass %q", i, raw, once, twice)
		}
	}
}

func checkClean(t *testing.T, raw, want string, wantStats Stats) {
	t.Helper()
	got, stats := Clean(raw)
	if got != want || stats != wantStats {
		t.Errorf("Clean() = (%q, %+v), want (%q, %+v)", got, stats, want, wantStats)
	}
	if twice, _ := Clean(got); twice != got {
		t.Errorf("Clean(Clean(x)) = %q, want %q", twice, got)
	}
}
