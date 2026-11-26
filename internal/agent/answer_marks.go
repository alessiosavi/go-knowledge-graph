package agent

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxAnswerCitations = 10

type markResult struct {
	Products, Chunks []string
	NotReturned      int
	Spans            [][2]int
}

// resolveMarks resolves only exact IDs from tool results. An unterminated group
// stops at the first token that cannot be resolved, preserving ensuing prose.
func resolveMarks(text string, seen *seenIndex) markResult {
	var result markResult
	if seen == nil {
		seen = newSeenIndex()
	}
	cite := func(id string) bool {
		switch product, ok := seen.Product(id); {
		case ok:
			result.Products = appendCitation(result.Products, product)
		case seen.Chunk(id):
			result.Chunks = appendCitation(result.Chunks, id)
		default:
			return false
		}
		return true
	}

	var candidates []string

	offset := 0
	for line := range strings.SplitSeq(text, "\n") {
		// Treat the carriage return in CRLF as part of the line ending.
		content := strings.TrimSuffix(line, "\r")
		closingPossible := true
		for pos := 0; pos < len(content); {
			open := strings.Index(content[pos:], "[[")
			if open < 0 {
				break
			}
			start := pos + open
			pos = start + 2

			if closingPossible {
				if closeAt := strings.Index(content[pos:], "]]"); closeAt >= 0 {
					end := pos + closeAt
					for token := range strings.FieldsFuncSeq(content[pos:end], markSeparator) {
						if !cite(token) {
							result.NotReturned++
						}
					}
					pos = end + 2
					result.Spans = append(result.Spans, [2]int{offset + start, offset + pos})
					continue
				}
				closingPossible = false
			}

			if candidates == nil {
				candidates = slices.DeleteFunc(seen.KnownIDs(), func(id string) bool {
					return id == "" || strings.ContainsFunc(id, markSeparator)
				})
				slices.SortFunc(candidates, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
			}
			for pos < len(content) {
				next, size := utf8.DecodeRuneInString(content[pos:])
				if markSeparator(next) {
					pos += size
					continue
				}
				id := leadingSeenID(content[pos:], candidates)
				if id == "" {
					break
				}
				cite(id)
				pos += len(id)
			}
			result.Spans = append(result.Spans, [2]int{offset + start, offset + pos})
		}
		offset += len(line) + 1
	}
	return result
}

func markSeparator(r rune) bool {
	return r == ',' || r == '[' || r == ']' || unicode.IsSpace(r)
}

func leadingSeenID(text string, candidates []string) string {
	for _, id := range candidates {
		if !strings.HasPrefix(text, id) {
			continue
		}
		if len(text) == len(id) {
			return id
		}
		next, _ := utf8.DecodeRuneInString(text[len(id):])
		if markSeparator(next) || strings.ContainsRune(".;:!?)", next) {
			return id
		}
	}
	return ""
}

func appendCitation(ids []string, id string) []string {
	if len(ids) < maxAnswerCitations && !slices.Contains(ids, id) {
		return append(ids, id)
	}
	return ids
}

var standaloneAnswerID = regexp.MustCompile(`[[:xdigit:]]{32}(?:-c[0-9]+)?`)

func cleanSummary(text string, spans [][2]int) string {
	text, gaps := cutSummarySpans(text, spans)
	var ids [][2]int
	for _, match := range standaloneAnswerID.FindAllStringIndex(text, -1) {
		before, _ := utf8.DecodeLastRuneInString(text[:match[0]])
		after, _ := utf8.DecodeRuneInString(text[match[1]:])
		if !summaryWordRune(before) && !summaryWordRune(after) {
			ids = append(ids, [2]int{match[0], match[1]})
		}
	}
	if len(ids) > 0 {
		// Carry the mark boundaries through the second removal so spacing
		// elsewhere in the model's Markdown stays intact.
		for _, gap := range gaps {
			ids = append(ids, [2]int{gap, gap})
		}
		slices.SortFunc(ids, func(a, b [2]int) int { return cmp.Compare(a[0], b[0]) })
		text, gaps = cutSummarySpans(text, ids)
	}

	var out strings.Builder
	last := 0
	for _, gap := range gaps {
		if gap < last {
			continue
		}
		start, end := gap, gap
		for start > last && text[start-1] == ' ' {
			start--
		}
		for end < len(text) && text[end] == ' ' {
			end++
		}
		if start < end && end < len(text) && strings.ContainsRune(".,;:!?)]", rune(text[end])) {
			out.WriteString(text[last:start])
			last = end
		}
	}
	out.WriteString(text[last:])
	return out.String()
}

func summaryWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r)
}

// cutSummarySpans removes sorted byte ranges and records where they were removed.
func cutSummarySpans(text string, spans [][2]int) (string, []int) {
	if len(spans) == 0 {
		return text, nil
	}
	var out strings.Builder
	gaps := make([]int, 0, len(spans))
	last := 0
	for _, span := range spans {
		if span[0] >= last {
			out.WriteString(text[last:span[0]])
			gaps = append(gaps, out.Len())
		}
		last = max(last, span[1])
	}
	out.WriteString(text[last:])
	return out.String(), gaps
}
