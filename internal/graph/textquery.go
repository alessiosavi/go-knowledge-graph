package graph

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// TextQueryWords splits plain text into the lowercase words a text-index
// query searches for: the runs of Unicode letters and digits, after NFC
// normalization so a decomposed "e" + U+0301 matches an indexed "é".
func TextQueryWords(q string) []string {
	words := strings.FieldsFunc(norm.NFC.String(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for i, w := range words {
		words[i] = strings.ToLower(w)
	}
	return words
}

// TextQueryTerms converts plain text to space-separated, quoted search
// terms. Punctuation separates Unicode letter/digit runs; quoting keeps
// words such as and, or and not from becoming Tantivy operators (Memgraph
// upper-cases them in any case before parsing).
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
