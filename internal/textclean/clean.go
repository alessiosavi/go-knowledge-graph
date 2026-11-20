// Package textclean removes common extraction artifacts from document text.
package textclean

import (
	"iter"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"golang.org/x/text/unicode/bidi"
)

// Version identifies the cleaning rules used to produce a document's text.
const Version = 1

// Stats counts changes and remaining replacement characters in one document.
type Stats struct {
	InvalidUTF8   int // One replacement per contiguous run of invalid bytes.
	Controls      int
	BidiControls  int
	GlyphLines    int
	LeaderRuns    int
	FFFDRemoved   int
	FFFDRemaining int
}

const replacement = "\ufffd"

// Clean applies the extraction cleanup rules in order and returns the counts.
func Clean(s string) (string, Stats) {
	var stats Stats
	if !utf8.ValidString(s) {
		valid := strings.ToValidUTF8(s, replacement)
		stats.InvalidUTF8 = strings.Count(valid, replacement) - strings.Count(s, replacement)
		s = valid
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r' {
			stats.Controls++
			return -1
		}
		return r
	}, s)
	s, bidiLines := normalizeBidiLines(s, &stats)
	s = cleanArtifactLines(s, bidiLines, &stats)
	stats.FFFDRemaining = strings.Count(s, replacement)
	return s, stats
}

func normalizeBidiLines(s string, stats *Stats) (string, map[int]string) {
	if !strings.ContainsFunc(s, isBidiControl) {
		return s, nil
	}
	// Keep the original text by output offset. A line emptied by normalization
	// can leave a bare CR adjacent to an LF, merging two line boundaries.
	bidiLines := make(map[int]string)
	s = rewriteLines(s, nil, func(line string, _ bool, _, newStart int) string {
		cleaned := stripBidiControls(line, stats)
		if cleaned != line {
			bidiLines[newStart] = line
		}
		return cleaned
	})
	return s, bidiLines
}

func stripBidiControls(s string, stats *Stats) string {
	var out strings.Builder
	runStart, copied := -1, 0
	var previous, previousBase rune
	for i, r := range s {
		if isBidiControl(r) {
			if runStart < 0 {
				runStart = i
			}
			stats.BidiControls++
			continue
		}
		mark := unicode.IsMark(r)
		if runStart >= 0 {
			out.WriteString(s[copied:runStart])
			if runStart > 0 && !mark && !unicode.IsSpace(previous) && !unicode.IsSpace(r) &&
				isRTLLetter(previousBase) != isRTLLetter(r) {
				out.WriteByte(' ')
			}
			copied, runStart = i, -1
		}
		previous = r
		if !mark {
			previousBase = r
		}
	}
	if runStart >= 0 {
		out.WriteString(s[copied:runStart])
	} else if copied > 0 {
		out.WriteString(s[copied:])
	} else {
		return s
	}
	return out.String()
}

func isBidiControl(r rune) bool {
	return (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069')
}

func isRTLLetter(r rune) bool {
	if r < utf8.RuneSelf || !unicode.IsLetter(r) {
		return false
	}
	p, _ := bidi.LookupRune(r)
	return p.Class() == bidi.R || p.Class() == bidi.AL
}

func hasRTLWord(s string) bool {
	for word := range strings.FieldsSeq(s) {
		letters := 0
		for _, r := range word {
			if isRTLLetter(r) {
				letters++
				if letters == 2 {
					return true
				}
			}
		}
	}
	return false
}

// Count standalone glyphs after bidi removal so wrapped glyphs are
// visible, and require the same glyph on at least three prose lines.
func repeatedGlyph(s string, fences []lineSpan) string {
	glyph := ""
	lines := 0
	for span, fenced := range documentLines(s, fences) {
		if fenced {
			continue
		}
		found := false
		for word := range strings.FieldsSeq(s[span.start:span.end]) {
			r, n := utf8.DecodeRuneInString(word)
			if n != len(word) || !isRTLLetter(r) {
				continue
			}
			if glyph != "" && word != glyph {
				return ""
			}
			glyph = word
			found = true
		}
		if found {
			lines++
		}
	}
	if lines < 3 {
		return ""
	}
	return glyph
}

func cleanArtifactLines(s string, bidiLines map[int]string, stats *Stats) string {
	checkGlyphs := strings.ContainsFunc(s, isRTLLetter)
	checkLeaders := hasLeaderGroup(s)
	if !checkGlyphs && !checkLeaders {
		return s
	}
	fences := fencedCodeSpans(s)
	if checkGlyphs {
		for range 4 {
			glyph := repeatedGlyph(s, fences)
			if glyph == "" || hasRTLWord(s) {
				break
			}
			repaired, nextBidiLines := repairGlyphLines(s, bidiLines, fences, glyph, stats)
			if repaired == s {
				break
			}
			s, bidiLines = repaired, nextBidiLines
			// Repair can change list indentation and expose lines under a fence.
			fences = fencedCodeSpans(s)
		}
	}
	if !checkLeaders {
		return s
	}
	return rewriteLines(s, fences, func(line string, fenced bool, _, _ int) string {
		if !fenced && hasLeaderGroup(line) {
			return collapseLeaders(line, stats)
		}
		return line
	})
}

func repairGlyphLines(s string, bidiLines map[int]string, fences []lineSpan, glyph string, stats *Stats) (string, map[int]string) {
	var nextBidiLines map[int]string
	if len(bidiLines) > 0 {
		nextBidiLines = make(map[int]string)
	}
	s = rewriteLines(s, fences, func(line string, fenced bool, start, newStart int) string {
		beforeBidi := bidiLines[start]
		if !fenced && strings.Contains(line, glyph) {
			if repaired, changed := repairGlyphLine(line, beforeBidi, glyph); changed {
				stats.GlyphLines++
				return repaired
			}
		}
		if beforeBidi != "" {
			nextBidiLines[newStart] = beforeBidi
		}
		return line
	})
	return s, nextBidiLines
}

func rewriteLines(s string, fences []lineSpan, rewrite func(line string, fenced bool, start, newStart int) string) string {
	var out strings.Builder
	written := 0
	for span, fenced := range documentLines(s, fences) {
		line := s[span.start:span.end]
		cleaned := rewrite(line, fenced, span.start, out.Len()+span.start-written)
		if cleaned != line {
			out.WriteString(s[written:span.start])
			out.WriteString(cleaned)
			written = span.end
		}
	}
	if written == 0 {
		return s
	}
	out.WriteString(s[written:])
	return out.String()
}

type lineSpan struct{ start, end int }

func documentLines(s string, fences []lineSpan) iter.Seq2[lineSpan, bool] {
	return func(yield func(lineSpan, bool) bool) {
		for start := 0; start < len(s); {
			end := start + lineLength(s[start:])
			for len(fences) > 0 && start >= fences[0].end {
				fences = fences[1:]
			}
			fenced := len(fences) > 0 && start >= fences[0].start
			if !yield(lineSpan{start, end}, fenced) {
				return
			}
			start = nextLineStart(s, end)
		}
	}
}

var markdownParser = parser.New()

func fencedCodeSpans(s string) []lineSpan {
	if !strings.Contains(s, "```") && !strings.Contains(s, "~~~") {
		return nil
	}
	// Goldmark scans LF lines. Normalize bare CRs for parsing only, keeping
	// all source offsets and the document's original line endings intact.
	markdown := s
	if strings.Count(s, "\r") != strings.Count(s, "\r\n") {
		normalized := []byte(s)
		for i, b := range normalized {
			if b == '\r' && (i+1 == len(normalized) || normalized[i+1] != '\n') {
				normalized[i] = '\n'
			}
		}
		markdown = string(normalized)
	}
	var spans []lineSpan
	_ = ast.Walk(markdownParser.ParseStringSource(markdown), func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if block, ok := node.(*ast.CodeBlock); entering && ok && block.CodeBlockKind == ast.CodeBlockKindFenced {
			pos := block.Pos()
			start := strings.LastIndexAny(s[:pos], "\r\n") + 1
			end := pos + lineLength(s[pos:])
			if lines := block.Value.Segments(); len(lines) > 0 {
				end = lines[len(lines)-1].Stop
			}
			// A matching closer contains only fence characters and whitespace.
			spans = append(spans, lineSpan{start, end})
		}
		return ast.WalkContinue, nil
	})
	return spans
}

func lineLength(s string) int {
	if n := strings.IndexAny(s, "\r\n"); n >= 0 {
		return n
	}
	return len(s)
}

func nextLineStart(s string, end int) int {
	if end+1 < len(s) && s[end] == '\r' && s[end+1] == '\n' {
		return end + 2
	}
	return min(end+1, len(s))
}

var listMarker = regexp.MustCompile(`^(?:[-*+]|[0-9]{1,9}[.)])[ \t]+`)

func glyphLinePrefix(line string) (indent, marker string) {
	body := strings.TrimLeftFunc(line, unicode.IsSpace)
	return line[:len(line)-len(body)], listMarker.FindString(body)
}

func repairGlyphLine(line, beforeBidi, glyph string) (string, bool) {
	original := line
	if beforeBidi != "" {
		original = beforeBidi
	}
	indent, marker := glyphLinePrefix(original)
	bodyStart := len(indent) + len(marker)
	if marker == "" && original != line {
		// Removing controls can expose a list marker. Keep the original indent.
		cleanIndent, cleanMarker := glyphLinePrefix(line)
		bodyStart = len(cleanIndent) + len(cleanMarker)
		marker = cleanMarker
	}
	body := line[bodyStart:]
	words := strings.Fields(body)
	last := len(words) - 1
	for last >= 0 && words[last] != glyph {
		last--
	}
	if last < 0 {
		return line, false
	}
	leading := words[0] == glyph
	if marker == "" && (!leading || beforeBidi == "") {
		return line, false
	}
	if !leading {
		previous, _ := utf8.DecodeRuneInString(words[last-1])
		if !unicode.IsUpper(previous) && !unicode.IsDigit(previous) {
			return line, false
		}
	}
	kept := words[:0]
	for i, word := range words {
		// Docling joins wrapped LTR continuations after the final glyph.
		if i == last && !leading {
			slices.Reverse(kept)
		}
		if word != glyph {
			kept = append(kept, word)
		}
	}
	trailing := body[len(strings.TrimRightFunc(body, unicode.IsSpace)):]
	return indent + marker + strings.Join(kept, " ") + trailing, true
}

// Bibcodes can contain up to seven padding dots. Each group must reach
// its own threshold before adjacent groups are coalesced into one leader.
const leaderGroup = `(?:\.{8,}|…{4,}|·{4,}|�{4,})`

var leaders = regexp.MustCompile(`[ \t]*` + leaderGroup + `(?:[ \t]*` + leaderGroup + `)*[ \t]*`)

func hasLeaderGroup(s string) bool {
	return strings.Contains(s, "........") || strings.Contains(s, "…………") ||
		strings.Contains(s, "····") || strings.Contains(s, "����")
}

func collapseLeaders(s string, stats *Stats) string {
	var out strings.Builder
	written := 0
	for _, match := range leaders.FindAllStringIndex(s, -1) {
		start, end := match[0], match[1]
		if !hasLeaderContext(s[:start]) {
			continue
		}
		out.WriteString(s[written:start])
		out.WriteString(" … ")
		written = end
		stats.LeaderRuns++
		stats.FFFDRemoved += strings.Count(s[start:end], replacement)
	}
	if written == 0 {
		return s
	}
	out.WriteString(s[written:])
	return out.String()
}

func hasLeaderContext(prefix string) bool {
	for len(prefix) > 0 {
		r, n := utf8.DecodeLastRuneInString(prefix)
		switch r {
		case '\r', '\n', utf8.RuneError:
			return false
		}
		if !unicode.IsSpace(r) {
			return true
		}
		prefix = prefix[:len(prefix)-n]
	}
	return false
}
