package graph

import (
	"html"
	"regexp"
	"strings"
	"unicode"

	"github.com/alessiosavi/GoGPUtils/textnorm"
	"golang.org/x/text/unicode/norm"
)

// FoldKeyVersion identifies the fold key format stored in Memgraph.
const FoldKeyVersion = 1

var (
	foldKeyReference        = regexp.MustCompile(`&(?:[A-Za-z][A-Za-z0-9]*|#[0-9]+|#[xX][0-9A-Fa-f]+);`)
	foldKeyTrademarkSymbols = strings.NewReplacer("™", "", "®", "", "©", "", "℠", "")
	foldKeyInput            = textnorm.New().SanitizeUTF8().Then(prepareFoldKey).FoldWidth().FoldCase()
)

// FoldKeyV2 computes a fold identity from a name. An empty result has no fold
// identity. Callers that fold entity display names supply DisplayName output.
func FoldKeyV2(name string) string {
	// The input stages sanitize UTF-8 before the width transform can see it.
	s, _ := foldKeyInput.Run(name)
	s = trimFoldKeyEdges(s)
	runes := stripFoldKeyLatinMarks([]rune(norm.NFD.String(s)))
	runes = mapFoldKeySymbols(runes)
	return norm.NFC.String(compactFoldKeySpaces(runes))
}

// prepareFoldKey decodes exact HTML references in one pass, removes trademark
// and copyright symbols, then applies compatibility normalization.
func prepareFoldKey(s string) (string, error) {
	s = foldKeyReference.ReplaceAllStringFunc(s, func(ref string) string {
		decoded := html.UnescapeString(ref)
		// A legacy prefix match leaves the literal semicolon behind. A lone
		// semicolon is itself the valid decoding of &semi; and its numeric forms.
		if strings.HasSuffix(decoded, ";") && decoded != ";" {
			return ref
		}
		return decoded
	})
	return norm.NFKC.String(foldKeyTrademarkSymbols.Replace(s)), nil
}

type foldKeyCluster struct {
	base  rune
	start int
}

// trimFoldKeyEdges uses base-plus-mark clusters so discarding an edge or
// possessive also discards any marks attached to it.
func trimFoldKeyEdges(s string) string {
	var clusters []foldKeyCluster
	for i, r := range s {
		if i == 0 || !unicode.IsMark(r) {
			clusters = append(clusters, foldKeyCluster{base: r, start: i})
		}
	}

	// Peel matching wrappers before dropping other edge punctuation.
	left, right := 0, len(clusters)
	for right-left >= 2 {
		close := foldKeyClosingWrapper(clusters[left].base)
		if close == 0 || close != clusters[right-1].base {
			break
		}
		left++
		right--
	}
	for left < right && !keepFoldKeyEdge(clusters[left].base) {
		left++
	}
	for left < right && !keepFoldKeyEdge(clusters[right-1].base) {
		right--
	}

	// Remove a single possessive as long as at least one cluster remains.
	if right-left >= 2 {
		penultimate, last := clusters[right-2].base, clusters[right-1].base
		switch {
		case penultimate == 's' && isFoldKeyApostrophe(last):
			right--
		case right-left > 2 && isFoldKeyApostrophe(penultimate) && last == 's':
			right -= 2
		}
	}
	for left < right && isFoldKeyApostrophe(clusters[left].base) {
		left++
	}
	for left < right && isFoldKeyApostrophe(clusters[right-1].base) {
		right--
	}

	if left == right {
		return ""
	}
	if right < len(clusters) {
		s = s[:clusters[right].start]
	}
	return s[clusters[left].start:]
}

func foldKeyClosingWrapper(r rune) rune {
	switch r {
	case '\'', '"':
		return r
	case '‘':
		return '’'
	case '“':
		return '”'
	case '(':
		return ')'
	case '[':
		return ']'
	case '{':
		return '}'
	default:
		return 0
	}
}

func isFoldKeyApostrophe(r rune) bool {
	return r == '\'' || r == '’'
}

func keepFoldKeyEdge(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || isFoldKeyApostrophe(r) || keepFoldKeySymbol(r)
}

func keepFoldKeySymbol(r rune) bool {
	switch r {
	case '+', '#', '@', '/', '&', '%':
		return true
	default:
		return false
	}
}

// stripFoldKeyLatinMarks removes Mn, Mc and Me marks from decomposed Latin
// bases and drops unattached marks. A mark never changes the current base.
func stripFoldKeyLatinMarks(runes []rune) []rune {
	out := runes[:0]
	keepMarks := false
	for _, r := range runes {
		if unicode.IsMark(r) {
			if !keepMarks {
				continue
			}
		} else {
			keepMarks = !unicode.In(r, unicode.Latin)
		}
		out = append(out, r)
	}
	return out
}

// mapFoldKeySymbols replaces disallowed bases with spaces and drops their marks.
// A period or comma is allowed only when its immediate neighbors are digits.
func mapFoldKeySymbols(runes []rune) []rune {
	out := runes[:0]
	keepMarks := false
	for i, r := range runes {
		if unicode.IsMark(r) {
			if keepMarks {
				out = append(out, r)
			}
			continue
		}

		keepMarks = true
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), keepFoldKeySymbol(r):
		case (r == '.' || r == ',') && i > 0 && i+1 < len(runes) && unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1]):
		default:
			r = ' '
			keepMarks = false
		}
		out = append(out, r)
	}
	return out
}

// compactFoldKeySpaces keeps one space per run directly between two digits.
func compactFoldKeySpaces(runes []rune) string {
	out := runes[:0]
	pendingSpace := false
	for _, r := range runes {
		if r == ' ' {
			pendingSpace = true
			continue
		}
		if pendingSpace && len(out) > 0 && unicode.IsDigit(out[len(out)-1]) && unicode.IsDigit(r) {
			out = append(out, ' ')
		}
		out = append(out, r)
		pendingSpace = false
	}
	return string(out)
}
