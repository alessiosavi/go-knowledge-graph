package eval

import (
	"encoding/json"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/hygiene"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

const mentionsFormat = "gkg-mentions/v1"

type replayMentions struct {
	enc    *json.Encoder
	counts mentionsCounts
}

type mentionsHeader struct {
	Type               string          `json:"type"`
	Format             string          `json:"format"`
	AttemptID          string          `json:"attempt_id"`
	InputSHA256        string          `json:"input_sha256"`
	Preset             string          `json:"preset"`
	PresetSHA256       string          `json:"preset_sha256"`
	NoBorrow           bool            `json:"no_borrow"`
	Offsets            string          `json:"offsets"`
	Labels             json.RawMessage `json:"labels"`
	LabelsSHA256       string          `json:"labels_sha256"`
	Hygiene            hygiene.Preset  `json:"hygiene"`
	WriteMode          replayWriteMode `json:"write_mode"`
	StoreFloor         float64         `json:"store_floor"`
	AssertFloor        float64         `json:"assert_floor"`
	SidecarModel       string          `json:"sidecar_model,omitempty"`
	SidecarImage       string          `json:"sidecar_image"`
	SidecarImageDigest string          `json:"sidecar_image_digest"`
	ExtractorVersion   string          `json:"extractor_version"`
	GKGVersion         string          `json:"gkg_version"`
}

type mentionsLabels struct {
	Entities  []mentionsEntityLabel   `json:"entities"`
	Relations []mentionsRelationLabel `json:"relations"`
}

type mentionsRelationLabel struct {
	Name      string  `json:"name"`
	Text      string  `json:"text"`
	Threshold float64 `json:"threshold"`
}

type mentionsEntityLabel struct {
	mentionsRelationLabel
	Scope string `json:"scope"`
}

func (m *replayMentions) writeHeader(h replayHeader, preset ingest.CheckedPreset) error {
	labels := mentionsLabels{
		Entities:  make([]mentionsEntityLabel, 0, len(preset.Entities)),
		Relations: make([]mentionsRelationLabel, 0, len(preset.Relations)),
	}
	asLabel := func(l extract.Label) mentionsRelationLabel {
		return mentionsRelationLabel{Name: l.Name, Text: l.SidecarText(), Threshold: l.Threshold}
	}
	for _, l := range preset.Entities {
		labels.Entities = append(labels.Entities, mentionsEntityLabel{mentionsRelationLabel: asLabel(l), Scope: l.Scope})
	}
	for _, l := range preset.Relations {
		labels.Relations = append(labels.Relations, asLabel(l))
	}
	data, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	return m.enc.Encode(mentionsHeader{
		Type: "header", Format: mentionsFormat, AttemptID: h.AttemptID, InputSHA256: h.InputSHA256,
		Preset: h.Preset, PresetSHA256: h.PresetSHA256, NoBorrow: h.NoBorrow, Offsets: "utf8_bytes_chunk",
		Labels: data, LabelsSHA256: sha256Hex(data), Hygiene: h.Hygiene, WriteMode: h.WriteMode,
		StoreFloor: h.StoreFloor, AssertFloor: h.AssertFloor, SidecarModel: h.SidecarModel, SidecarImage: h.SidecarImage,
		SidecarImageDigest: h.SidecarImageDigest, ExtractorVersion: h.ExtractorVersion, GKGVersion: h.GKGVersion,
	})
}

type mentionsCounts struct {
	ChunksRequested    int  `json:"chunks_requested"`
	ChunksOK           int  `json:"chunks_ok"`
	ChunksEmpty        int  `json:"chunks_empty"`
	ChunksBorrowed     int  `json:"chunks_borrowed"`
	ChunksError        int  `json:"chunks_error"`
	Entities           int  `json:"entities"`
	Relations          int  `json:"relations"`
	AmbiguousRelations int  `json:"ambiguous_relations"`
	Complete           bool `json:"complete"`
}

func (m *replayMentions) trailerCounts() mentionsCounts {
	counts := m.counts
	counts.Complete = counts.ChunksError == 0 && counts.ChunksOK+counts.ChunksEmpty+counts.ChunksBorrowed == counts.ChunksRequested
	return counts
}

type mentionsChunk struct {
	Type               string          `json:"type"`
	ChunkID            string          `json:"chunk_id"`
	DocID              string          `json:"doc_id"`
	ProductID          json.RawMessage `json:"product_id,omitempty"`
	Kind               json.RawMessage `json:"kind,omitempty"`
	Status             string          `json:"status"`
	SourceChunkID      string          `json:"source_chunk_id,omitempty"`
	Error              string          `json:"error"`
	Entities           int             `json:"entities"`
	Relations          int             `json:"relations"`
	AmbiguousRelations int             `json:"ambiguous_relations"`
}

type mentionEndpoint struct {
	Index int    `json:"index"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Label string `json:"label"`
}

type mentionsEntity struct {
	Type    string `json:"type"`
	ChunkID string `json:"chunk_id"`
	mentionEndpoint
	Text    string         `json:"text"`
	Score   float64        `json:"score"`
	Hygiene mentionHygiene `json:"hygiene"`
}

type mentionHygiene struct {
	Outcome string `json:"outcome"`
	*keptMention
	Rule hygiene.Rule `json:"rule,omitempty"`
}

type keptMention struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
	Key     string   `json:"key"`
}

type mentionsRelation struct {
	Type          string          `json:"type"`
	ChunkID       string          `json:"chunk_id"`
	Index         int             `json:"index"`
	Label         string          `json:"label"`
	Score         float64         `json:"score"`
	Head          mentionEndpoint `json:"head"`
	Tail          mentionEndpoint `json:"tail"`
	Disposition   string          `json:"disposition"`
	LowConfidence bool            `json:"low_confidence"`
	StoredWinner  bool            `json:"stored_winner"`
}

type mentionsAmbiguity struct {
	Type           string            `json:"type"`
	ChunkID        string            `json:"chunk_id"`
	Index          int               `json:"index"`
	Label          string            `json:"label"`
	Score          float64           `json:"score"`
	HeadCandidates []mentionEndpoint `json:"head_candidates"`
	TailCandidates []mentionEndpoint `json:"tail_candidates"`
}

func (m *replayMentions) writeChunk(d sampleDocument, c sampleChunk, a replayAssessment, sourceChunkID string) error {
	raw := a.raw
	if sourceChunkID != "" {
		raw = extract.ChunkResult{}
	}
	chunk := mentionsChunk{
		Type: "chunk", ChunkID: c.ChunkID, DocID: d.DocID, ProductID: d.ProductID, Kind: d.Kind,
		Entities: len(raw.Mentions), Relations: len(raw.Candidates),
	}
	for _, diagnostic := range raw.Diagnostics {
		if diagnostic.Type == "ambiguous_relation" {
			chunk.AmbiguousRelations++
		}
	}
	switch {
	case a.err != nil:
		chunk.Status, chunk.Error = "error", a.err.Error()
		m.counts.ChunksError++
	case sourceChunkID != "":
		chunk.Status, chunk.SourceChunkID = "borrowed", sourceChunkID
		m.counts.ChunksBorrowed++
	case chunk.Entities+chunk.Relations+chunk.AmbiguousRelations == 0:
		chunk.Status = "empty"
		m.counts.ChunksEmpty++
	default:
		chunk.Status = "ok"
		m.counts.ChunksOK++
	}
	if err := m.enc.Encode(chunk); err != nil {
		return err
	}
	m.counts.Entities += chunk.Entities
	m.counts.Relations += chunk.Relations
	m.counts.AmbiguousRelations += chunk.AmbiguousRelations

	endpoint := func(i int) mentionEndpoint {
		e := raw.Mentions[i]
		return mentionEndpoint{Index: i, Start: e.Start, End: e.End, Label: e.Label}
	}
	for i, entity := range raw.Mentions {
		decision := a.Mentions[i]
		h := mentionHygiene{Outcome: "dropped", Rule: decision.DropRule}
		if decision.DropRule == "" {
			aliases := decision.Aliases
			if aliases == nil {
				aliases = []string{}
			}
			h = mentionHygiene{Outcome: "kept", keptMention: &keptMention{Name: decision.Name, Aliases: aliases, Key: decision.Key}}
		}
		if err := m.enc.Encode(mentionsEntity{
			Type: "entity", ChunkID: c.ChunkID, mentionEndpoint: endpoint(i), Text: entity.Text, Score: entity.Score, Hygiene: h,
		}); err != nil {
			return err
		}
	}
	for i, relation := range raw.Candidates {
		decision := a.Occurrences[i]
		if err := m.enc.Encode(mentionsRelation{
			Type: "relation", ChunkID: c.ChunkID, Index: i, Label: relation.Label, Score: relation.Score,
			Head: endpoint(relation.Head), Tail: endpoint(relation.Tail), Disposition: decision.Disposition,
			LowConfidence: decision.LowConfidence, StoredWinner: decision.StoredWinner,
		}); err != nil {
			return err
		}
	}
	endpoints := func(indices []int) []mentionEndpoint {
		out := make([]mentionEndpoint, len(indices))
		for i, index := range indices {
			out[i] = endpoint(index)
		}
		return out
	}
	for index, d := range raw.Diagnostics {
		if d.Type != "ambiguous_relation" {
			continue
		}
		if err := m.enc.Encode(mentionsAmbiguity{
			Type: "ambiguous_relation", ChunkID: c.ChunkID, Index: index, Label: d.Label, Score: d.Score,
			HeadCandidates: endpoints(d.HeadCandidates), TailCandidates: endpoints(d.TailCandidates),
		}); err != nil {
			return err
		}
	}
	return nil
}
