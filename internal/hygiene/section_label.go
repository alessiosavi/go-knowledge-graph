package hygiene

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	HeadingLead = "heading_lead"
	LeadIn      = "lead_in"
)

var (
	atxHeadingStart = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+`)
	sectionNumber   = regexp.MustCompile(`^\d+(\.\d+)*\.?[ \t]+`)
	listStart       = regexp.MustCompile(`^[ \t]*(?:(?:[-*+•]|\d+[.)]) +)?`)
	codeFenceStart  = regexp.MustCompile(`^ {0,3}(?:` + "```+" + `|~~~+)`)
)

// SectionLabel classifies a single byte-exact mention as a heading lead or
// emphasized lead-in. An empty result means this rule keeps the mention.
func SectionLabel(chunkText string, start, end int, preset Preset) string {
	if start < 0 || end <= start || end > len(chunkText) {
		return ""
	}
	name := chunkText[start:end]
	if strings.ContainsAny(name, "\r\n") || !utf8.ValidString(name) || preset.Allow[name] {
		return ""
	}
	lineStart := strings.LastIndexByte(chunkText[:start], '\n') + 1
	line, _, _ := strings.Cut(chunkText[lineStart:], "\n")
	line = strings.TrimSuffix(line, "\r")
	start, end = start-lineStart, end-lineStart

	rule := classifySectionLine(line, start, end)
	if rule != "" && insideCodeFence(chunkText[:lineStart]) {
		return ""
	}
	return rule
}

func classifySectionLine(line string, start, end int) string {
	if prefix := atxHeadingStart.FindString(line); prefix != "" {
		textStart := len(prefix)
		if emphasis := openingEmphasis(line[textStart:]); len(emphasis) <= 3 {
			textStart += len(emphasis)
		}
		if headingLead(line, textStart, start, end) {
			return HeadingLead
		}
		return ""
	}

	contentStart := len(listStart.FindString(line))
	content := line[contentStart:]
	emphasis := openingEmphasis(content)
	if len(emphasis) == 0 || len(emphasis) > 2 {
		return ""
	}
	textStart := contentStart + len(emphasis)
	rest := line[textStart:]
	closer := strings.Index(rest, emphasis)
	if closer <= 0 {
		return ""
	}
	textEnd := textStart + closer
	after := line[textEnd+len(emphasis):]

	if len(emphasis) == 2 && strings.TrimSpace(after) == "" &&
		end <= textEnd && headingLead(line[:textEnd], textStart, start, end) {
		return HeadingLead
	}

	text := line[textStart:textEnd]
	hasPunctuation := strings.HasSuffix(text, ":") || strings.HasSuffix(text, ".")
	if start == textStart && (end == textEnd || (hasPunctuation && end == textEnd-1)) &&
		(hasPunctuation || hasAnyPrefix(after, ":", ".", " —", " –", "—")) {
		return LeadIn
	}
	return ""
}

func headingLead(line string, textStart, start, end int) bool {
	textStart += len(sectionNumber.FindString(line[textStart:]))
	if start != textStart {
		return false
	}
	rest := strings.TrimLeft(line[end:], "*_")
	if hasAnyPrefix(rest, ":", " (", " —", " –", " - ", "—", "–") {
		return true
	}
	if strings.HasPrefix(rest, ".") {
		next, _ := utf8.DecodeRuneInString(rest[1:])
		return len(rest) == 1 || unicode.IsSpace(next)
	}
	return false
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func openingEmphasis(s string) string {
	if s == "" || (s[0] != '*' && s[0] != '_') {
		return ""
	}
	n := 1
	for n < len(s) && s[n] == s[0] {
		n++
	}
	return s[:n]
}

func insideCodeFence(prefix string) bool {
	var fence string
	for line := range strings.SplitSeq(prefix, "\n") {
		marker := codeFenceStart.FindString(line)
		if marker == "" {
			continue
		}
		rest := line[len(marker):]
		marker = strings.TrimLeft(marker, " ")
		if fence == "" {
			if marker[0] != '`' || !strings.Contains(rest, "`") {
				fence = marker
			}
		} else if strings.HasPrefix(marker, fence) && strings.TrimSpace(rest) == "" {
			fence = ""
		}
	}
	return fence != ""
}
