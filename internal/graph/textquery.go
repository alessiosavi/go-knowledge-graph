package graph

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// TextQueryWords splits plain text into the words a text-index query
// searches for: runs of Unicode letters, numbers and combining marks, after
// NFC normalization so a decomposed "e" + U+0301 matches an indexed "é".
// Case is kept: the index tokenizer lowercases query terms the same way it
// lowercased the indexed text (full Unicode rules: "İstanbul" only matches
// when Tantivy, not Go, lowercases it).
func TextQueryWords(q string) []string {
	return strings.FieldsFunc(norm.NFC.String(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	})
}

// TextQueryTerms converts plain text to space-separated, quoted search
// terms. Punctuation separates the words; quoting keeps words such as and,
// or and not from becoming Tantivy operators (Memgraph upper-cases bare ones
// in any case before parsing; a quoted "AND" stays a plain term).
func TextQueryTerms(q string) string {
	words := TextQueryWords(q)
	for i, w := range words {
		words[i] = `"` + w + `"`
	}
	return strings.Join(words, " ")
}

// textQuery returns the sanitized query, or false when no searchable word
// is left; callers then return no rows without querying (an empty Tantivy
// query is a syntax error).
func textQuery(q string) (string, bool) {
	terms := TextQueryTerms(q)
	return terms, terms != ""
}
