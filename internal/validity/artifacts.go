package validity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Outcome identifies a projected or final fact disposition.
type Outcome string

const (
	OutcomeReject           Outcome = "reject"
	OutcomeDemote           Outcome = "demote"
	OutcomeValidityDrop     Outcome = "validity-drop"
	OutcomeStamp            Outcome = "stamp"
	OutcomeRewriteCandidate Outcome = "rewrite-candidate"
	OutcomeRewriteConfirmed Outcome = "rewrite-confirmed"
	OutcomeRewriteUnclear   Outcome = "rewrite-unclear"
)

// InputsRow is one inputs.jsonl object. The parity inputs and decision fields
// are flat. An empty action and rule mean the source verdict skipped validity.
// Outcome is reject, demote, validity-drop, stamp or rewrite-candidate.
// Reason carries projection details such as no-target or collision-merged:<id>.
// Destination is present for a projected destination, including collisions.
// Occupants and CandidateGroup record the inputs to collision decisions at K.
// Empty lists encode as [] and decode as nil slices from either [] or null.
type InputsRow struct {
	ID         string `json:"id"`
	Domain     string `json:"domain"`
	Preset     string `json:"preset"`
	Bucket     string `json:"bucket"`
	PriorJudge string `json:"prior_judge"`
	Input
	Decision
	Target         TitleTarget          `json:"target"`
	Outcome        Outcome              `json:"outcome"`
	Reason         string               `json:"reason"`
	Destination    *DestinationKey      `json:"destination,omitempty"`
	Occupants      []Occupant           `json:"occupants"`
	CandidateGroup []CandidateRankInput `json:"candidate_group"`
}

type inputsRowJSON InputsRow

// MarshalJSON encodes empty lists as JSON arrays.
func (r InputsRow) MarshalJSON() ([]byte, error) {
	if r.Target.TitleMatchKeys == nil {
		r.Target.TitleMatchKeys = []string{}
	}
	if r.Occupants == nil {
		r.Occupants = []Occupant{}
	}
	if r.CandidateGroup == nil {
		r.CandidateGroup = []CandidateRankInput{}
	}
	return json.Marshal(inputsRowJSON(r))
}

// UnmarshalJSON decodes a strict inputs.jsonl object and normalizes empty lists.
func (r *InputsRow) UnmarshalJSON(data []byte) error {
	row, err := decodeArtifact[inputsRowJSON](data)
	if err != nil {
		return err
	}
	if len(row.Target.TitleMatchKeys) == 0 {
		row.Target.TitleMatchKeys = nil
	}
	if len(row.Occupants) == 0 {
		row.Occupants = nil
	}
	if len(row.CandidateGroup) == 0 {
		row.CandidateGroup = nil
	}
	*r = InputsRow(row)
	return nil
}

// TitleTarget describes the selected title entity and the complete match set.
// Key, Name and Type are empty when no unique target exists. TitleMatchKeys
// must be in lexicographic order, including when there are multiple matches.
type TitleTarget struct {
	Key            string   `json:"key"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	TitleMatchKeys []string `json:"title_match_keys"`
}

// DestinationKey is the logical key K of the proposed reattached edge.
type DestinationKey struct {
	HeadKey string `json:"head_key"`
	Rel     string `json:"rel"`
	TailKey string `json:"tail_key"`
	ChunkID string `json:"chunk_id"`
}

// Occupant records a live edge at K and its judge state, which may be empty.
type Occupant struct {
	ID    string `json:"id"`
	Judge string `json:"judge"`
}

// CandidateRankInput records the confidence and source id used to rank a
// candidate for K.
type CandidateRankInput struct {
	SourceID   string  `json:"source_id"`
	Confidence float64 `json:"confidence"`
}

// FinalRow is one final.jsonl object. PlannedOutcome uses the InputsRow outcome
// set. FinalOutcome is reject, demote, validity-drop, stamp, rewrite-confirmed
// or rewrite-unclear. NewID is present for rewrites. RewriteVerdict is present
// when the row's candidate was judged, including a refuted candidate.
type FinalRow struct {
	ID             string  `json:"id"`
	PlannedOutcome Outcome `json:"planned_outcome"`
	FinalOutcome   Outcome `json:"final_outcome"`
	Reason         string  `json:"reason"`
	Rule           string  `json:"rule"`
	NewID          string  `json:"new_id,omitempty"`
	SourceVerdict  string  `json:"source_verdict"`
	RewriteVerdict string  `json:"rewrite_verdict,omitempty"`
}

// PlanSummary is the plan.json object. Counts are keyed by outcome, then stored
// relation, then rule. BucketCounts are keyed by inventory bucket. The optional
// final fields record the rewrite verdict seal (or no-rewrites) and final digest.
type PlanSummary struct {
	RulesID            string                                `json:"rules_id"`
	LexiconDigest      string                                `json:"lexicon_digest"`
	ManifestDigest     string                                `json:"manifest_digest"`
	VerdictSeal        string                                `json:"verdict_seal"`
	Counts             map[Outcome]map[string]map[string]int `json:"counts"`
	BucketCounts       map[string]int                        `json:"bucket_counts"`
	DraftDigest        string                                `json:"draft_digest"`
	RewriteVerdictSeal string                                `json:"rewrite_verdict_seal,omitempty"`
	FinalDigest        string                                `json:"final_digest,omitempty"`
}

// DecodeInputsRow decodes one inputs.jsonl row and rejects unknown fields.
func DecodeInputsRow(data []byte) (InputsRow, error) {
	var row InputsRow
	err := row.UnmarshalJSON(data)
	return row, err
}

// DecodeFinalRow decodes one final.jsonl row and rejects unknown fields.
func DecodeFinalRow(data []byte) (FinalRow, error) {
	return decodeArtifact[FinalRow](data)
}

// DecodePlanSummary decodes plan.json and rejects unknown fields.
func DecodePlanSummary(data []byte) (PlanSummary, error) {
	return decodeArtifact[PlanSummary](data)
}

func decodeArtifact[T any](data []byte) (T, error) {
	var artifact, zero T
	data = bytes.Trim(data, " \t\r\n")
	if len(data) == 0 || data[0] != '{' {
		return zero, errors.New("expected a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&artifact); err != nil {
		return zero, err
	}
	var extra json.RawMessage
	switch err := dec.Decode(&extra); err {
	case io.EOF:
		return artifact, nil
	case nil:
		return zero, errors.New("expected a single JSON object")
	default:
		return zero, fmt.Errorf("trailing JSON data: %w", err)
	}
}
