package productobs

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

var caseFold = cases.Fold()

// Include the four information separators treated as whitespace by the
// frozen Unicode fold.
func isSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= '\x1c' && r <= '\x1f')
}

func nonempty(s string) bool {
	return strings.TrimFunc(s, isSpace) != ""
}

// Fold returns the normalized value used by the observation rules for kind.
func Fold(kind, raw string) string {
	normalized := strings.Join(strings.FieldsFunc(norm.NFKC.String(raw), isSpace), " ")
	switch kind {
	case "text", "enum":
		return strings.Trim(caseFold.String(normalized), " .,;:")
	case "code":
		return strings.ReplaceAll(cases.Upper(language.Und).String(normalized), " ", "")
	default:
		return normalized
	}
}

func splitMulti(raw, separator string) (string, []string) {
	value := Fold("measure", raw)
	items := []string{}
	start, depth := 0, 0
	var comma, semicolon bool
	for i, char := range value {
		switch char {
		case '(':
			depth++
			continue
		case ')':
			depth--
			if depth < 0 {
				return "unsplit", []string{raw}
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if char == ',' {
			before, _ := utf8.DecodeLastRuneInString(value[:i])
			after, _ := utf8.DecodeRuneInString(value[i+1:])
			if unicode.IsDigit(before) && unicode.IsDigit(after) {
				continue
			}
			comma = true
		}
		semicolon = semicolon || char == ';'
		if separator != "" && i >= start && strings.HasPrefix(value[i:], separator) {
			if item := strings.TrimSpace(value[start:i]); item != "" {
				items = append(items, item)
			}
			start = i + len(separator)
		}
	}
	if depth != 0 || (comma && semicolon && (separator == "," || separator == ";")) {
		return "unsplit", []string{raw}
	}
	if item := strings.TrimSpace(value[start:]); item != "" {
		items = append(items, item)
	}
	return "split", items
}
