package hygiene

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var (
	markdownLink = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)|\(#[^)]*\)`)
	heading      = regexp.MustCompile(`^#{1,6}[\t ]+(.+?)\s*#*\s*$`)
	// Undotted SpA spellings are case-sensitive to preserve the English word
	// "Spa"; the dotted s.p.a. cannot be that word, so it folds case.
	// The capture identifies suffixes that can follow a compound Co. marker.
	companySuffix = regexp.MustCompile(`[,\s]+(?:(?i:s\.r\.l\.?|srl\.?|s\.p\.a\.?)|SpA\.?|SPA\.?|(?i:(ltd\.?|inc\.?|gmbh\.?|llc\.?)))$`)
	companyMarker = regexp.MustCompile(`(?i)\s+co\.,?$`)
	quotes        = strings.NewReplacer("’", "'", "‘", "'", "“", `"`, "”", `"`)
)

func rawDrop(name string, wholeHeading, value bool, p Preset) Rule {
	if strings.ContainsAny(name, "\r\n") {
		return "newline"
	}
	s := strings.TrimSpace(name)
	if !value && strings.HasPrefix(s, "--") {
		return "cli_flag"
	}
	if !value && strings.HasPrefix(s, "-") {
		r, _ := utf8.DecodeRuneInString(s[1:])
		if unicode.IsLetter(r) {
			return "cli_flag"
		}
	}
	if markdownLink.MatchString(s) {
		return "markdown_link"
	}
	if wholeHeading {
		return "heading_line"
	}
	if !value && !p.Code {
		if strings.ContainsAny(s, `/\`) && strings.IndexFunc(s, unicode.IsSpace) < 0 {
			return "path_like"
		}
		if strings.ContainsAny(s, "`{}=;_") || strings.Contains(s, "()") {
			return "code_like"
		}
	}
	return ""
}

// A repeated title in prose is a different mention. When the provider supplies
// valid offsets, inspect only that occurrence's line.
func isHeadingLine(name, chunkText string, start, end int) bool {
	lines := chunkText
	if start >= 0 && end > start && end <= len(chunkText) && chunkText[start:end] == name {
		lineStart := strings.LastIndexByte(chunkText[:start], '\n') + 1
		lineEnd := len(chunkText)
		if n := strings.IndexByte(chunkText[start:], '\n'); n >= 0 {
			lineEnd = start + n
		}
		lines = chunkText[lineStart:lineEnd]
	} else if !strings.HasPrefix(strings.TrimSpace(name), "#") {
		return false
	}
	name = strings.TrimSpace(name)
	for _, line := range strings.Split(lines, "\n") {
		line = strings.TrimSpace(line)
		if match := heading.FindStringSubmatch(line); match != nil && (name == line || name == strings.TrimSpace(match[1])) {
			return true
		}
	}
	return false
}

func normalize(name string) (string, []string) {
	name = strings.Join(strings.Fields(quotes.Replace(name)), " ")
	lower := strings.ToLower(name)
	for _, prefix := range []string{"l'", "un'", "d'", "dell'", "dall'", "all'", "nell'", "sull'", "dagl'", "degl'", "negl'", "sugl'", "quell'"} {
		if strings.HasPrefix(lower, prefix) {
			rest := strings.TrimSpace(name[len(prefix):])
			if utf8.RuneCountInString(rest) >= 3 {
				name = rest
			}
			break
		}
	}
	if base, stripped := StripCompanySuffix(name); stripped {
		return base, []string{name}
	}
	return name, nil
}

// StripCompanySuffix removes a trailing legal company suffix from a name.
// If no suffix is removed, it returns the original name and false.
func StripCompanySuffix(name string) (string, bool) {
	// A possessive company form ("Quillon Ltd's") matches its suffix on the
	// stem, so it keys like the plain form.
	stem := name
	trimEdge := func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) || r == '`' }
	stem = strings.TrimRightFunc(stem, trimEdge)
	if n := len(stem); n > 2 && strings.EqualFold(stem[n-2:], "'s") {
		stem = stem[:n-2]
	}
	stem = strings.TrimFunc(stem, trimEdge)
	at := companySuffix.FindStringSubmatchIndex(stem)
	if at == nil {
		return name, false
	}
	base := strings.TrimSpace(stem[:at[0]])
	if strings.HasSuffix(base, "&") {
		return name, false
	}
	if at[2] >= 0 { // Ltd, Inc, GmbH or LLC.
		withoutMarker := strings.TrimRight(companyMarker.ReplaceAllString(base, ""), ", ")
		if withoutMarker != "" && !strings.HasSuffix(withoutMarker, "&") {
			return withoutMarker, true
		}
		base = strings.TrimRight(base, ", ")
	}
	return base, true
}

func normalizedDrop(name string, value bool, p Preset) Rule {
	length := utf8.RuneCountInString(name)
	if length > 60 {
		return "too_long"
	}
	if !value && strings.IndexFunc(name, unicode.IsLetter) < 0 {
		return "no_letters"
	}
	if !value && length <= 2 && !p.Allow[name] {
		return "too_short"
	}
	key := fold(name)
	if p.Stop[key] {
		return "stoplist"
	}
	for _, pattern := range p.StopPatterns {
		if pattern.MatchString(key) {
			return "stoplist"
		}
	}
	return ""
}

// fold supplies case- and diacritic-insensitive stoplist matching.
func fold(s string) string {
	s = norm.NFD.String(cases.Fold().String(s))
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
