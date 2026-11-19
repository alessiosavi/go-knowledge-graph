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

// AssessReplay uses ingest's extraction builder and the store's fact
// selection on a frozen root chunk. It needs no pipeline or graph connection.
func AssessReplay(cfg config.IngestConfig, preset CheckedPreset, doc graph.Document, c graph.Chunk, raw extract.ChunkResult, version string, mode WriteMode) []OccurrenceDecision {
	cleaned, trace := hygiene.ApplyWithTrace(raw, c.Text, preset.Hygiene)
	var built extractionTrace
	_, facts, _ := buildExtraction(&doc, c, cleaned, version, documentScopedLabels(preset.Entities), cfg, mode.Domains, &built)
	texts := make(map[string]string, len(preset.Relations))
	for _, label := range preset.Relations {
		texts[label.Name] = label.SidecarText()
	}

	endpoint := func(i int) OccurrenceEndpoint {
		m := raw.Mentions[i]
		e := OccurrenceEndpoint{Text: m.Text, Start: m.Start, End: m.End, Type: m.Label}
		if j := trace.MentionIndices[i]; j >= 0 {
			e.Name = cleaned.Mentions[j].Name
			e.Key = built.mentionKeys[j]
		}
		return e
	}
	out := make([]OccurrenceDecision, len(raw.Candidates))
	for i, cand := range raw.Candidates {
		// The sidecar has already validated the mention indices and spans.
		head, tail := endpoint(cand.Head), endpoint(cand.Tail)
		rule := trace.MentionDrops[cand.Head]
		if rule == "" {
			rule = trace.MentionDrops[cand.Tail]
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
	for _, i := range graph.FactWinnerIndices(facts) {
		out[trace.CandidateSources[built.factSources[i]]].StoredWinner = true
	}
	return out
}
