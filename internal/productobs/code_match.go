package productobs

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

const CodeMatcherRevision = "codekey-v1/window3"

func isCodePunctuation(r rune) bool {
	return strings.ContainsRune(".-_/+#", r)
}

func isCodeSeparator(r rune) bool {
	return unicode.IsSpace(r) || isCodePunctuation(r)
}

// CodeKey normalizes an identifier or an original text window for mention matching.
func CodeKey(s string) string {
	return codeKey(s, cases.Upper(language.Und))
}

func codeKey(s string, upper cases.Caser) string {
	return strings.Map(func(r rune) rune {
		if isCodeSeparator(r) {
			return -1
		}
		return r
	}, upper.String(norm.NFKC.String(s)))
}

// EligibleCodeKey reports whether a normalized identifier qualifies for matching.
func EligibleCodeKey(key string) bool {
	length := utf8.RuneCountInString(key)
	if length < 5 {
		return false
	}
	letter, digit, allDigits := false, false, true
	for _, r := range key {
		// A window key holds only token runes after separator removal.
		if !unicode.IsLetter(r) && !unicode.IsMark(r) && !unicode.IsDigit(r) {
			return false
		}
		isDigit := unicode.IsDigit(r)
		letter = letter || unicode.IsLetter(r)
		digit = digit || isDigit
		allDigits = allDigits && isDigit
	}
	return (letter && digit) || (allDigits && length >= 7)
}

type CodeMatch struct {
	Key    string
	Span   [2]int
	Text   string
	Tokens int
}

func (m CodeMatch) SeparatorRun() bool {
	return m.Tokens > 1 || strings.ContainsFunc(m.Text, isCodeSeparator)
}

// MatchCodes matches whole windows of one to three tokens against normalized keys.
// Matches are ordered by their original byte start and end offsets.
func MatchCodes[V any](text string, keys map[string]V) []CodeMatch {
	if len(keys) == 0 {
		return nil
	}
	upper := cases.Upper(language.Und)
	var matches []CodeMatch
	var starts [3]int
	n, start := 0, -1
	finishToken := func(end int) {
		starts[2], starts[1], starts[0] = starts[1], starts[0], start
		n = min(n+1, len(starts))
		for i, windowStart := range starts[:n] {
			window := text[windowStart:end]
			key := codeKey(window, upper)
			if key == "" {
				continue
			}
			if _, ok := keys[key]; ok {
				matches = append(matches, CodeMatch{Key: key, Span: [2]int{windowStart, end}, Text: window, Tokens: i + 1})
			}
		}
		start = -1
	}
	for i, r := range text {
		if unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r) || isCodePunctuation(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			finishToken(i)
		}
		if r != ' ' && r != '\t' {
			n = 0
		}
	}
	if start >= 0 {
		finishToken(len(text))
	}
	slices.SortFunc(matches, func(a, b CodeMatch) int {
		if order := cmp.Compare(a.Span[0], b.Span[0]); order != 0 {
			return order
		}
		return cmp.Compare(a.Span[1], b.Span[1])
	})
	return matches
}
