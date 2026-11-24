package productobs

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

var opaquePageSource = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[^/\s:?#]+)+$`)

// ProductPageKey returns the normalized last path segment of an HTTP(S) link.
func ProductPageKey(link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", false
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	return pageKey(path[strings.LastIndexByte(path, '/')+1:])
}

// SourcePageKey accepts opaque, namespaced sources. URL sources are matched
// separately by exact URL.
func SourcePageKey(source string) (string, bool) {
	if !opaquePageSource.MatchString(source) {
		return "", false
	}
	return pageKey(source[strings.LastIndexByte(source, '/')+1:])
}

func pageKey(raw string) (string, bool) {
	segment, err := url.PathUnescape(raw)
	if err == nil {
		if strings.ContainsAny(segment, "/%") || strings.ContainsFunc(segment, unicode.IsControl) {
			return "", false
		}
		if utf8.ValidString(segment) {
			key := cases.Lower(language.Und).String(norm.NFKC.String(segment))
			key = strings.TrimSpace(stripPageExtension(key))
			// Separators, percent escapes and combining marks are ineligible
			// even if they could be represented by an ASCII escape.
			if strings.ContainsAny(key, "/%") || strings.ContainsFunc(key, func(r rune) bool {
				return unicode.IsControl(r) || unicode.IsMark(r)
			}) {
				return "", false
			}
			if !strings.ContainsFunc(key, func(r rune) bool {
				return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.'
			}) {
				return eligiblePageKey(key)
			}
		}
	}

	// Preserve ASCII slugs with malformed escapes or non-UTF-8 escapes.
	key := stripPageExtension(strings.ToLower(raw))
	if strings.ContainsFunc(key, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '%' || r == '-')
	}) {
		return "", false
	}
	return eligiblePageKey(key)
}

func stripPageExtension(key string) string {
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		switch key[i:] {
		case ".html", ".htm", ".php", ".asp", ".aspx", ".jsp":
			return key[:i]
		}
	}
	return key
}

func eligiblePageKey(key string) (string, bool) {
	if utf8.RuneCountInString(key) < 8 || !strings.ContainsAny(key, "-_") || !strings.ContainsFunc(key, unicode.IsLetter) {
		return "", false
	}
	return key, true
}
