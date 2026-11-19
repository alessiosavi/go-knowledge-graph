// Package hygiene normalizes extracted names without changing evidence text or spans.
package hygiene

import (
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// Rule identifies the first rule that dropped a mention.
type Rule string

// Mention retains the provider's evidence and adds its normalized entity name.
type Mention struct {
	extract.Mention
	Name    string
	Aliases []string
}

// Result is a cleaned chunk extraction. Candidate indices refer to Mentions.
type Result struct {
	ChunkID    string
	Mentions   []Mention
	Candidates []extract.Candidate
	Dropped    map[Rule]int
	Renamed    int
}

// Trace links a cleaned extraction to the original provider occurrences.
type Trace struct {
	MentionIndices   []int // original mention -> cleaned index, or -1
	MentionDrops     []Rule
	CandidateSources []int // cleaned candidate -> original index
}

// NameOnly applies the rules that do not require chunk text.
func NameOnly(name, label string, p Preset) (normalized string, aliases []string, dropped Rule) {
	return cleanName(name, label, false, p)
}

func cleanName(name, label string, wholeHeading bool, p Preset) (string, []string, Rule) {
	if rule := rawDrop(name, wholeHeading, p.Values[label], p); rule != "" {
		return "", nil, rule
	}
	normalized, aliases := normalize(name)
	if rule := normalizedDrop(normalized, p.Values[label], p); rule != "" {
		return "", nil, rule
	}
	if graph.EntityKey(normalized) == "" {
		return "", nil, "empty_key"
	}
	return normalized, aliases, ""
}

// Apply cleans every mention, then drops facts with missing endpoints and
// reindexes the surviving candidates. It never mutates the provider's result.
func Apply(r extract.ChunkResult, chunkText string, p Preset) Result {
	return apply(r, chunkText, p, nil)
}

// ApplyWithTrace also records the source indices and the rules that dropped
// mentions, so callers can account for relations with dropped endpoints.
func ApplyWithTrace(r extract.ChunkResult, chunkText string, p Preset) (Result, Trace) {
	trace := Trace{MentionDrops: make([]Rule, len(r.Mentions))}
	out := apply(r, chunkText, p, &trace)
	return out, trace
}

func apply(r extract.ChunkResult, chunkText string, p Preset, trace *Trace) Result {
	out := Result{ChunkID: r.ChunkID, Dropped: make(map[Rule]int)}
	indices := make([]int, len(r.Mentions))
	if trace != nil {
		trace.MentionIndices = indices
	}
	for i, m := range r.Mentions {
		indices[i] = -1
		name, aliases, rule := cleanName(m.Text, m.Label, isHeadingLine(m.Text, chunkText, m.Start, m.End), p)
		if rule == "" && m.Start >= 0 && m.End > m.Start && m.End <= len(chunkText) &&
			chunkText[m.Start:m.End] == m.Text && SectionLabel(chunkText, m.Start, m.End, m.Label, p) != "" {
			rule = "section_label"
		}
		if rule != "" {
			out.Dropped[rule]++
			if trace != nil {
				trace.MentionDrops[i] = rule
			}
			continue
		}
		indices[i] = len(out.Mentions)
		out.Mentions = append(out.Mentions, Mention{Mention: m, Name: name, Aliases: aliases})
		if name != m.Text {
			out.Renamed++
		}
	}
	for i, c := range r.Candidates {
		if c.Head < 0 || c.Head >= len(indices) || c.Tail < 0 || c.Tail >= len(indices) {
			continue
		}
		if indices[c.Head] < 0 || indices[c.Tail] < 0 {
			continue
		}
		c.Head, c.Tail = indices[c.Head], indices[c.Tail]
		out.Candidates = append(out.Candidates, c)
		if trace != nil {
			trace.CandidateSources = append(trace.CandidateSources, i)
		}
	}
	return out
}
