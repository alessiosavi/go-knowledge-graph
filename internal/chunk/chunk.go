// Package chunk splits cleaned document text into sentence-aware chunks
// with absolute byte offsets into the original text, so spans can always be
// sliced back out byte-exact (Unicode included).
package chunk

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Options control splitting.
type Options struct {
	// TargetWords is the approximate word budget per chunk. <= 0 uses 200.
	TargetWords int
	// OverlapSentences is how many trailing sentences of one chunk are
	// repeated at the start of the next. Negative values mean 0.
	OverlapSentences int
}

// Span is one chunk: Text is exactly text[Start:End] of the input.
type Span struct {
	Seq   int
	Text  string
	Start int
	End   int
}

const (
	defaultTargetWords = 200
	maxHeadingBytes    = 60
	maxHeadingWords    = 8
)

// abbreviations that may end with a period mid-sentence.
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "st": true,
	"vs": true, "eg": true, "ie": true, "etc": true, "vol": true,
	"fig": true, "sig": true, "dott": true, "ing": true, "avv": true, "ecc": true,
}

// weakAbbreviations only abbreviate before a number ("no. 5", "art. 7");
// elsewhere they end a sentence ("No. Andiamo via.").
var weakAbbreviations = map[string]bool{"no": true, "ca": true, "art": true, "es": true}

type sentence struct {
	start, end int
	heading    bool
}

// Split cuts text into sentence-aware chunks. Headings (Markdown "#…" lines
// or short all-caps lines) force a chunk boundary and start their own chunk;
// no overlap is carried across a heading.
func Split(text string, opts Options) []Span {
	if opts.TargetWords <= 0 {
		opts.TargetWords = defaultTargetWords
	}
	if opts.OverlapSentences < 0 {
		opts.OverlapSentences = 0
	}
	sents := splitSentences(text)
	if len(sents) == 0 {
		return nil
	}
	wc := make([]int, len(sents))
	for i, s := range sents {
		wc[i] = len(strings.Fields(text[s.start:s.end]))
	}

	var spans []Span
	prevEnd := 0 // index one past the previous chunk's last sentence
	for i := 0; i < len(sents); {
		j := i
		words := 0
		for j < len(sents) {
			if j > i && (sents[j].heading || words+wc[j] > opts.TargetWords) {
				// Never emit a chunk that is pure overlap: extend through
				// the first sentence beyond the previous chunk.
				if !sents[j].heading && j <= prevEnd {
					words += wc[j]
					j++
					continue
				}
				break
			}
			words += wc[j]
			j++
		}
		start, end := sents[i].start, sents[j-1].end
		spans = append(spans, Span{Seq: len(spans), Text: text[start:end], Start: start, End: end})
		prevEnd = j
		if j == len(sents) {
			break
		}
		if sents[j].heading {
			i = j
			continue
		}
		next := j - opts.OverlapSentences
		if next <= i {
			next = i + 1
		}
		i = next
	}
	return spans
}

// splitSentences returns sentence spans (trimmed) in order, marking heading
// lines. Boundaries: .!?… followed by whitespace/EOF (with abbreviation,
// single-letter and decimal guards), blank lines, and heading lines.
func splitSentences(text string) []sentence {
	var out []sentence

	i := 0
	sentStart := -1

	flush := func(end int) {
		if sentStart < 0 {
			return
		}
		for end > sentStart {
			r, size := utf8.DecodeLastRuneInString(text[sentStart:end])
			if !unicode.IsSpace(r) {
				break
			}
			end -= size
		}
		if end > sentStart {
			out = append(out, sentence{start: sentStart, end: end})
		}
		sentStart = -1
	}

	for i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])

		if sentStart < 0 {
			if unicode.IsSpace(r) {
				i += size
				continue
			}
			if start, end, ok := headingAt(text, i); ok {
				out = append(out, sentence{start: start, end: end, heading: true})
				i = end
				continue
			}
			sentStart = i
		}

		switch {
		case r == '\n':
			if blank, next := blankLineBreak(text, i); blank {
				flush(i)
				i = next
				continue
			}
			// A heading on the next line interrupts even when the current
			// sentence has no terminator (CommonMark-style).
			if start, end, ok := headingAfterNewline(text, i+size); ok {
				flush(i)
				out = append(out, sentence{start: start, end: end, heading: true})
				i = end
				continue
			}
			i += size
		case r == '.' || r == '!' || r == '?' || r == '…':
			if isSentenceEnd(text, i, size) {
				flush(i + size)
			}
			i += size
		default:
			i += size
		}
	}
	flush(len(text))
	return out
}

// isSentenceEnd reports whether the terminator at position i ends a
// sentence: it must be followed by whitespace or EOF (which already
// excludes decimals like "3.14"), and the period must not be an
// abbreviation or a single-letter initial.
func isSentenceEnd(text string, i, size int) bool {
	var next rune
	if i+size < len(text) {
		next, _ = utf8.DecodeRuneInString(text[i+size:])
		if !unicode.IsSpace(next) {
			return false
		}
	}
	// nextNonSpace is the first non-space rune after the terminator (0 at EOF).
	var nextNonSpace rune
	for k := i + size; k < len(text); {
		r, s := utf8.DecodeRuneInString(text[k:])
		if !unicode.IsSpace(r) {
			nextNonSpace = r
			break
		}
		k += s
	}
	if text[i] != '.' {
		return true
	}
	word := wordBefore(text, i)
	segments := strings.Split(word, ".")
	allInitials := len(word) > 0
	letters := 0
	for _, seg := range segments {
		if seg == "" {
			allInitials = false
			continue
		}
		if utf8.RuneCountInString(seg) != 1 {
			allInitials = false
			continue
		}
		r, _ := utf8.DecodeRuneInString(seg)
		if !unicode.IsLetter(r) {
			allInitials = false
		}
		letters++
	}
	if allInitials && letters > 0 {
		return false
	}
	stripped := strings.ToLower(strings.ReplaceAll(word, ".", ""))
	if abbreviations[stripped] {
		return false
	}
	if weakAbbreviations[stripped] {
		return !unicode.IsDigit(nextNonSpace)
	}
	return true
}

// wordBefore returns the letter/digit/dot run immediately before i.
func wordBefore(text string, i int) string {
	j := i
	for j > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:j])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' {
			j -= size
			continue
		}
		break
	}
	return text[j:i]
}

// blankLineBreak reports whether the newline at i starts a blank line, and
// the position just past it (or EOF for trailing newlines).
func blankLineBreak(text string, i int) (bool, int) {
	j := i + 1
	for j < len(text) {
		switch text[j] {
		case ' ', '\t', '\r':
			j++
			continue
		}
		break
	}
	if j >= len(text) {
		return true, j
	}
	if text[j] == '\n' {
		return true, j + 1
	}
	return false, 0
}

// headingAt reports whether a heading line starts at i (which must be the
// first non-space character of its line), returning its trimmed span.
func headingAt(text string, i int) (int, int, bool) {
	if !atLineStart(text, i) {
		return 0, 0, false
	}
	lineEnd := len(text)
	if j := strings.IndexByte(text[i:], '\n'); j >= 0 {
		lineEnd = i + j
	}
	line := text[i:lineEnd]
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return 0, 0, false
	}
	start := i + strings.Index(line, trimmed)
	end := start + len(trimmed)
	if isHeadingLine(trimmed) {
		return start, end, true
	}
	return 0, 0, false
}

// headingAfterNewline reports whether a heading line starts at j (called
// right after a newline), skipping the line's leading whitespace.
func headingAfterNewline(text string, j int) (int, int, bool) {
	for j < len(text) {
		switch text[j] {
		case ' ', '\t', '\r':
			j++
			continue
		}
		break
	}
	if j >= len(text) {
		return 0, 0, false
	}
	return headingAt(text, j)
}

// atLineStart reports whether everything before i on its line is whitespace.
func atLineStart(text string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch text[j] {
		case '\n':
			return true
		case ' ', '\t', '\r':
			continue
		default:
			return false
		}
	}
	return true
}

// isHeadingLine reports whether s is a Markdown heading or a short all-caps
// line.
func isHeadingLine(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxHeadingBytes {
		return false
	}
	if strings.HasPrefix(s, "#") {
		n := 0
		for n < len(s) && s[n] == '#' {
			n++
		}
		return n <= 6 && n < len(s) && (s[n] == ' ' || s[n] == '\t')
	}
	if len(strings.Fields(s)) > maxHeadingWords {
		return false
	}
	hasLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			hasLetter = true
			if !unicode.IsUpper(r) {
				return false
			}
		}
	}
	return hasLetter
}
