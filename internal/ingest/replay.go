package ingest

import (
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/hygiene"
)

// OccurrenceEndpoint is the raw evidence and the resulting ingest identity.
// Start and end are byte offsets relative to the stored chunk.
type OccurrenceEndpoint struct {
	Text  string `json:"text"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Name  string `json:"name"`
	Key   string `json:"key"`
	Type  string `json:"type"`
}

// OccurrenceDecision records the pre-judge outcome of a model relation.
type OccurrenceDecision struct {
	Head          OccurrenceEndpoint `json:"head"`
	Tail          OccurrenceEndpoint `json:"tail"`
	Relation      string             `json:"relation"`
	RelationText  string             `json:"relation_label_text"`
	Score         float64            `json:"score"`
	Evidence      string             `json:"evidence_sentence"`
	Disposition   string             `json:"disposition"`
	LowConfidence bool               `json:"low_confidence"`
	StoredWinner  bool               `json:"stored_winner"`
}

// MentionDecision records a normalized mention or the rule that dropped it.
type MentionDecision struct {
	Name     string
	Aliases  []string
	Key      string
	DropRule hygiene.Rule
}

// ReplayAssessment follows the validated mention and candidate orders.
type ReplayAssessment struct {
	Mentions    []MentionDecision
	Occurrences []OccurrenceDecision
	Winners     []graph.FactUpsert
}

// AssessReplay uses ingest's extraction builder and the store's fact
// selection on a frozen chunk. It needs no pipeline or graph connection.
func AssessReplay(cfg config.IngestConfig, preset CheckedPreset, doc graph.Document, c graph.Chunk, raw extract.ChunkResult, version string, mode WriteMode, includeWinners bool) ReplayAssessment {
	var cleaned hygiene.Result
	var trace hygiene.Trace
	if mode.Hygiene {
		cleaned, trace = hygiene.ApplyWithTrace(raw, c.Text, preset.Hygiene)
	} else {
		cleaned = rawExtraction(raw)
		trace.MentionIndices = identityIndices(len(raw.Mentions))
		trace.CandidateSources = identityIndices(len(raw.Candidates))
	}
	var built extractionTrace
	_, facts, _ := buildExtraction(&doc, c, cleaned, version, documentScopedLabels(preset.Entities), cfg, mode.Domains, &built)
	mentions := make([]MentionDecision, len(raw.Mentions))
	for i, j := range trace.MentionIndices {
		if j < 0 {
			mentions[i].DropRule = trace.MentionDrops[i]
			continue
		}
		m := cleaned.Mentions[j]
		mentions[i] = MentionDecision{Name: m.Name, Aliases: m.Aliases, Key: built.mentionKeys[j]}
	}
	texts := make(map[string]string, len(preset.Relations))
	for _, label := range preset.Relations {
		texts[label.Name] = label.SidecarText()
	}

	endpoint := func(i int) OccurrenceEndpoint {
		m := raw.Mentions[i]
		return OccurrenceEndpoint{
			Text: m.Text, Start: m.Start, End: m.End, Type: m.Label, Name: mentions[i].Name, Key: mentions[i].Key,
		}
	}
	out := make([]OccurrenceDecision, len(raw.Candidates))
	for i, cand := range raw.Candidates {
		// The sidecar has already validated the mention indices and spans.
		head, tail := endpoint(cand.Head), endpoint(cand.Tail)
		rule := mentions[cand.Head].DropRule
		if rule == "" {
			rule = mentions[cand.Tail].DropRule
		}
		if rule == "" {
			rule = "empty_key" // the builder can also skip unusable endpoints
		}
		labelText := texts[cand.Label]
		if labelText == "" {
			labelText = cand.Label
		}
		out[i] = OccurrenceDecision{
			Head: head, Tail: tail, Relation: graph.SanitizeRel(cand.Label), RelationText: labelText, Score: cand.Score,
			Evidence: EvidenceSentence(c.Text, head.Start, head.End), Disposition: "hygiene:" + string(rule),
		}
	}
	for i, dropped := range built.storeFloorDrop {
		if dropped {
			out[trace.CandidateSources[i]].Disposition = "floor:store_floor"
		}
	}
	for i, source := range built.factSources {
		row := &out[trace.CandidateSources[source]]
		row.Disposition = "kept"
		row.LowConfidence = facts[i].LowConfidence
	}
	assessment := ReplayAssessment{Mentions: mentions, Occurrences: out}
	for _, i := range graph.FactWinnerIndices(facts) {
		out[trace.CandidateSources[built.factSources[i]]].StoredWinner = true
		if includeWinners {
			assessment.Winners = append(assessment.Winners, facts[i])
		}
	}
	return assessment
}

func identityIndices(n int) []int {
	indices := make([]int, n)
	for i := range indices {
		indices[i] = i
	}
	return indices
}

// ReplayJudgeInput renders a selected fact using the production judge payload.
func ReplayJudgeInput(doc *graph.Document, chunk graph.Chunk, fact graph.FactUpsert) []byte {
	return CanonicalJudgeInput(factInputFor(doc, chunk, fact))
}
