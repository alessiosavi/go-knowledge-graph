package judge

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunk"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const maxEvidenceBytes = 800

// EvidenceMention gives the byte offsets and type of one mention in ChunkText.
type EvidenceMention struct {
	Start, End   int
	MentionType  string
	TypeFallback bool
}

// V2Input carries the source inputs for a v2 judge request. PreviousChunkText,
// when present, must come from the immediately preceding chunk of the same
// document. The caller owns document and chunk identity checks.
type V2Input struct {
	ChunkText         string
	PreviousChunkText string
	Head, Tail        EvidenceMention
	Rel               string
	Definition        string
	DocTitle          string
}

type v2Fact struct {
	Head string `json:"head"`
	Rel  string `json:"rel"`
	Tail string `json:"tail"`
}

type v2Payload struct {
	Fact               v2Fact  `json:"fact"`
	RelationDefinition string  `json:"relation_definition"`
	HeadMentionType    string  `json:"head_mention_type"`
	TailMentionType    string  `json:"tail_mention_type"`
	HeadTypeFallback   bool    `json:"head_type_fallback"`
	TailTypeFallback   bool    `json:"tail_type_fallback"`
	Evidence           string  `json:"evidence"`
	EvidenceTruncated  bool    `json:"evidence_truncated,omitempty"`
	TableHeader        *string `json:"table_header"`
	DocTitle           string  `json:"doc_title"`
}

// BuildV2Request builds the exact user message from source text. It returns a
// reason and no bytes when the evidence cannot be reconstructed.
func BuildV2Request(in V2Input) (requestBytes []byte, unverifiable string) {
	requestBytes, _, unverifiable = buildV2Request(in)
	return requestBytes, unverifiable
}

func buildV2Request(in V2Input) (requestBytes []byte, tableRow bool, unverifiable string) {
	text := in.ChunkText
	if strings.TrimSpace(text) == "" {
		return nil, false, "missing chunk text"
	}
	if !utf8.ValidString(text) {
		return nil, false, "invalid UTF-8 in chunk text"
	}
	head, headName := mentionSurface(text, in.Head)
	if headName == "" {
		return nil, false, "invalid or empty head span"
	}
	tail, tailName := mentionSurface(text, in.Tail)
	if tailName == "" {
		return nil, false, "invalid or empty tail span"
	}

	var evidence string
	var evidenceTruncated bool
	var tableHeader *string
	headLine := physicalLine(text, head.start)
	row := headLine.text(text)
	cells, pipes := tableCells(row)
	tableRow = head.end <= headLine.end && tail.start >= headLine.start && tail.end <= headLine.end && isTableRow(row, cells, pipes)
	if tableRow {
		evidence = row
		if len(row) > maxEvidenceBytes {
			evidence = trimTableRow(text, headLine, head, tail)
			evidenceTruncated = true
		}
		tableHeader = tableHeaderAbove(text, in.PreviousChunkText, headLine.start)
	} else {
		evidence = proseEvidence(text, head, tail)
	}
	if evidence == "" {
		return nil, tableRow, "mentions do not fit in the evidence limit"
	}

	// This schema contains only strings, booleans and a nullable string.
	payload, _ := json.Marshal(v2Payload{
		Fact:               v2Fact{Head: headName, Rel: in.Rel, Tail: tailName},
		RelationDefinition: in.Definition,
		HeadMentionType:    in.Head.MentionType,
		TailMentionType:    in.Tail.MentionType,
		HeadTypeFallback:   in.Head.TypeFallback,
		TailTypeFallback:   in.Tail.TypeFallback,
		Evidence:           evidence,
		EvidenceTruncated:  evidenceTruncated,
		TableHeader:        tableHeader,
		DocTitle:           in.DocTitle,
	})
	return payload, tableRow, ""
}

type byteSpan struct{ start, end int }

func (s byteSpan) size() int { return s.end - s.start }

func (s byteSpan) text(text string) string { return text[s.start:s.end] }

func (s byteSpan) overlaps(other byteSpan) bool {
	return s.start < other.end && other.start < s.end
}

func (s byteSpan) union(other byteSpan) byteSpan {
	return byteSpan{min(s.start, other.start), max(s.end, other.end)}
}

func mentionSurface(text string, m EvidenceMention) (byteSpan, string) {
	if m.Start < 0 || m.End <= m.Start || m.End > len(text) ||
		!utf8.RuneStart(text[m.Start]) || (m.End < len(text) && !utf8.RuneStart(text[m.End])) {
		return byteSpan{}, ""
	}
	raw := text[m.Start:m.End]
	leftTrimmed := strings.TrimLeftFunc(raw, unicode.IsSpace)
	surface := strings.TrimRightFunc(leftTrimmed, unicode.IsSpace)
	start := m.Start + len(raw) - len(leftTrimmed)
	return byteSpan{start, start + len(surface)}, graph.DisplayName(surface)
}

func proseEvidence(text string, head, tail byteSpan) string {
	headSentence, tailSentence := head, tail
	for start, end := range chunk.SentenceBounds(text) {
		sentence := byteSpan{start, end}
		if sentence.overlaps(head) {
			headSentence = headSentence.union(sentence)
		}
		if sentence.overlaps(tail) {
			tailSentence = tailSentence.union(sentence)
		}
	}
	if headSentence.overlaps(tailSentence) {
		return windowAround(text, headSentence.union(tailSentence), head.union(tail), maxEvidenceBytes)
	}
	if headSentence.start > tailSentence.start {
		headSentence, tailSentence = tailSentence, headSentence
		head, tail = tail, head
	}
	return joinedEvidence(text, headSentence, tailSentence, head, tail)
}

func trimTableRow(text string, row, head, tail byteSpan) string {
	mentions := head.union(tail)
	if mentions.size() <= maxEvidenceBytes {
		return windowAround(text, row, mentions, maxEvidenceBytes)
	}
	if head.overlaps(tail) {
		return ""
	}
	if head.start > tail.start {
		head, tail = tail, head
	}
	// Split the context between the mentions so the windows cannot overlap.
	split := head.end + (tail.start-head.end)/2
	for !utf8.RuneStart(text[split]) {
		split--
	}
	return joinedEvidence(text, byteSpan{row.start, split}, byteSpan{split, row.end}, head, tail)
}

func joinedEvidence(text string, firstContext, secondContext, firstMention, secondMention byteSpan) string {
	const joiner = " … "
	const budget = maxEvidenceBytes - len(joiner)
	if firstMention.size()+secondMention.size() > budget {
		return ""
	}
	// Start from an even split. Give unused room to the longer context
	// and reserve enough room for each complete mention.
	firstBudget := max(min(budget/2, firstContext.size()), budget-secondContext.size(), firstMention.size())
	firstBudget = min(firstBudget, budget-secondMention.size())
	return windowAround(text, firstContext, firstMention, firstBudget) + joiner +
		windowAround(text, secondContext, secondMention, budget-firstBudget)
}

func windowAround(text string, sentence, mention byteSpan, budget int) string {
	if mention.size() > budget {
		return ""
	}
	if sentence.size() <= budget {
		return strings.TrimSpace(sentence.text(text))
	}
	start := mention.start - (budget-mention.size())/2
	start = max(sentence.start, min(start, sentence.end-budget))
	for !utf8.RuneStart(text[start]) {
		start++
	}
	end := min(sentence.end, start+budget)
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	return strings.TrimSpace(text[start:end])
}

func physicalLine(text string, offset int) byteSpan {
	start := strings.LastIndexByte(text[:offset], '\n') + 1
	end := len(text)
	if n := strings.IndexByte(text[offset:], '\n'); n >= 0 {
		end = offset + n
	}
	if end > start && text[end-1] == '\r' {
		end--
	}
	return byteSpan{start, end}
}

// tableHeaderAbove walks only contiguous table lines directly above the row.
// The nearest separator belongs to that block. A missing or incompatible
// header at that separator leaves the row's header unknown.
func tableHeaderAbove(text, previous string, rowStart int) *string {
	var lines []string
	if previous != "" && utf8.ValidString(previous) {
		// A terminal line ending closes the last line. An extra one is a
		// blank line and must break the table block.
		lines = strings.Split(strings.TrimSuffix(previous, "\n"), "\n")
	}
	if rowStart > 0 {
		lines = append(lines, strings.Split(text[:rowStart-1], "\n")...)
	}
	for i := len(lines) - 1; i > 0; i-- {
		line := strings.TrimSuffix(lines[i], "\r")
		cells, pipes := tableCells(line)
		if !isTableSeparator(cells) {
			if !isTableRow(line, cells, pipes) {
				break
			}
			continue
		}
		header := strings.TrimSuffix(lines[i-1], "\r")
		headerCells, _ := tableCells(header)
		if len(headerCells) != len(cells) || isTableSeparator(headerCells) {
			break
		}
		value := header + "\n" + line
		return &value
	}
	return nil
}

func isTableRow(line string, cells []string, pipes int) bool {
	if strings.HasPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), "|") {
		return true
	}
	if pipes < 2 {
		return false
	}
	nonEmpty := 0
	for _, cell := range cells {
		if strings.TrimSpace(cell) != "" {
			nonEmpty++
		}
	}
	return nonEmpty >= 2
}

// tableCells splits at unescaped pipes. Leading and trailing pipes count
// toward the delimiter total but do not create cells.
func tableCells(line string) ([]string, int) {
	var cells []string
	start := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case '|':
			cells = append(cells, line[start:i])
			start = i + 1
		}
	}
	pipes := len(cells)
	if pipes == 0 {
		return nil, 0
	}
	cells = append(cells, line[start:])
	if strings.TrimSpace(cells[0]) == "" {
		cells = cells[1:]
	}
	if strings.TrimSpace(cells[len(cells)-1]) == "" {
		cells = cells[:len(cells)-1]
	}
	return cells, pipes
}

func isTableSeparator(cells []string) bool {
	for _, cell := range cells {
		dashes := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(cell), ":"), ":")
		if dashes == "" || strings.Trim(dashes, "-") != "" {
			return false
		}
	}
	return len(cells) > 0
}
