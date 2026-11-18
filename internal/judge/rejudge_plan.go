package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"golang.org/x/text/cases"
)

// ErrRejudgeAuditInput identifies audit input that cannot produce a valid plan.
var ErrRejudgeAuditInput = errors.New("invalid rejudge audit input")

type RejudgeSelection struct {
	Domain string
	// Nil selects all ids. An empty non-nil slice selects none.
	Labels []AuditRow
	Limit  int
}

type RejudgePlanResult struct {
	Planned, Reconciled, Mismatched, CallsNeeded int
	Unverifiable                                 map[string]int
	Headers                                      map[string]RejudgeHeaders
	Mismatches                                   []RejudgeMismatch
}

type RejudgeHeaders struct {
	Present, Null int
}

type RejudgeMismatch struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
	Reason string `json:"reason"`
}

type rejudgeManifestHeader struct {
	Version    int               `json:"rejudge_manifest"`
	Mismatches []RejudgeMismatch `json:"mismatches,omitempty"`
}

type rejudgeManifestRow struct {
	graph.RejudgeIdentity
	Before  map[string]any `json:"before"`
	Preset  string         `json:"preset"`
	Prior   string         `json:"prior"`
	Request Request        `json:"request"`
	Label   string         `json:"label,omitempty"`
}

type rejudgeManifest struct {
	header rejudgeManifestHeader
	rows   []rejudgeManifestRow
}

const rejudgeManifestVersion = 1

type rejudgeDefinitions map[string]map[string]string

func (d rejudgeDefinitions) forPreset(preset string) map[string]map[string]string {
	return map[string]map[string]string{preset: d[preset]}
}

// PlanRejudge freezes validated requests and counts distinct cache misses.
// Its only output side effect is an atomically published private manifest.
func PlanRejudge(ctx context.Context, st *graph.Store, model, presetsDir, path string, opt RejudgeSelection) (RejudgePlanResult, error) {
	result := RejudgePlanResult{Unverifiable: map[string]int{}, Headers: map[string]RejudgeHeaders{}}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = os.ErrExist
		}
		return result, fmt.Errorf("manifest %s: %w", path, err)
	}
	labels := make(map[string]AuditRow, len(opt.Labels))
	var ids []string
	if opt.Labels != nil {
		ids = make([]string, 0, len(opt.Labels))
	}
	for _, row := range opt.Labels {
		if row.ID == "" {
			return result, fmt.Errorf("%w: empty labelled id", ErrRejudgeAuditInput)
		}
		if !validRejudgeLabel(row.Label) {
			return result, fmt.Errorf("%w: invalid label %q for id %s", ErrRejudgeAuditInput, row.Label, row.ID)
		}
		if _, exists := labels[row.ID]; exists {
			return result, fmt.Errorf("%w: duplicate labelled id %s", ErrRejudgeAuditInput, row.ID)
		}
		ids = append(ids, row.ID)
		labels[row.ID] = row
	}
	seen := make(map[string]bool)
	definitions := make(rejudgeDefinitions)
	profiles := make(map[string]Profile)
	presetErrors := make(map[string]error)
	var incomplete []error
	manifest := rejudgeManifest{header: rejudgeManifestHeader{Version: rejudgeManifestVersion}}

	unverifiable := func(id, reason string) {
		result.Unverifiable[reason]++
		if ids != nil {
			incomplete = append(incomplete, fmt.Errorf("%s: unverifiable (%s)", id, reason))
		}
	}
	err := st.WalkRejudgeFacts(ctx, opt.Domain, ids, opt.Limit, func(facts []graph.RejudgeFact) error {
		// Try the current chunk first. Only table rows with a missing header
		// can benefit from loading the previous chunk.
		type pendingRequest struct {
			fact          graph.RejudgeFact
			input         V2Input
			bytes         []byte
			headerPresent bool
			tryPrevious   bool
		}
		batch := make([]pendingRequest, 0, len(facts))
		previous := make(map[string]string)
		for _, fact := range facts {
			if seen[fact.ID] {
				return fmt.Errorf("multiple typed edges share audit id %s", fact.ID)
			}
			seen[fact.ID] = true
			if !fact.ChunkOwned {
				unverifiable(fact.ID, "document does not own chunk")
				continue
			}
			if _, loaded := definitions[fact.Preset]; !loaded {
				definitions[fact.Preset], presetErrors[fact.Preset] = loadRejudgeDefinitions(presetsDir, fact.Preset)
			}
			if err := presetErrors[fact.Preset]; err != nil {
				unverifiable(fact.ID, "unavailable label preset "+fmt.Sprintf("%q", fact.Preset))
				continue
			}
			input := V2Input{
				ChunkText: fact.Text, DocTitle: fact.Title, Rel: fact.Relation,
				Definition: definitions[fact.Preset][fact.Relation],
				Head:       EvidenceMention{fact.HeadStart, fact.HeadEnd, fact.HeadType, fact.HeadTypeFallback},
				Tail:       EvidenceMention{fact.TailStart, fact.TailEnd, fact.TailType, fact.TailTypeFallback},
			}
			payloadBytes, tableRow, reason := buildV2Request(input)
			if reason != "" {
				unverifiable(fact.ID, reason)
				continue
			}
			var payload v2Payload
			if err := json.Unmarshal(payloadBytes, &payload); err != nil {
				return err
			}
			if label := labels[fact.ID]; scoredRejudgeLabel(label.Label) {
				if reason := reconcileRejudgeLabel(label, fact, payload); reason != "" {
					result.Mismatches = append(result.Mismatches, RejudgeMismatch{fact.ID, fact.Domain, reason})
					continue
				}
				result.Reconciled++
			}
			tryPrevious := tableRow && payload.TableHeader == nil
			if tryPrevious {
				previous[fact.ChunkID] = fact.DocID
			}
			batch = append(batch, pendingRequest{fact, input, payloadBytes, payload.TableHeader != nil, tryPrevious})
		}
		previousText, err := st.RejudgePreviousChunks(ctx, previous)
		if err != nil {
			return err
		}
		for _, pending := range batch {
			fact, input, payloadBytes := pending.fact, pending.input, pending.bytes
			headerPresent := pending.headerPresent
			if input.PreviousChunkText = previousText[fact.ChunkID]; pending.tryPrevious && input.PreviousChunkText != "" {
				payloadBytes, _ = BuildV2Request(input)
				var payload v2Payload
				if err := json.Unmarshal(payloadBytes, &payload); err != nil {
					return err
				}
				headerPresent = payload.TableHeader != nil
			}
			headers := result.Headers[fact.Relation]
			if !headerPresent {
				headers.Null++
			} else {
				headers.Present++
			}
			result.Headers[fact.Relation] = headers

			prior := fact.Judge
			if prior == "" {
				prior = "unjudged"
			}
			profile, ok := profiles[fact.Preset]
			if !ok {
				profile = NewV2Profile(model, definitions.forPreset(fact.Preset))
				profiles[fact.Preset] = profile
			}
			manifest.rows = append(manifest.rows, rejudgeManifestRow{
				RejudgeIdentity: fact.RejudgeIdentity, Before: fact.Before, Preset: fact.Preset, Prior: prior,
				Request: Request{Profile: profile, RequestBytes: payloadBytes, DocID: fact.DocID}, Label: labels[fact.ID].Label,
			})
		}
		return nil
	})
	result.Planned, result.Mismatched = len(manifest.rows), len(result.Mismatches)
	if err != nil {
		return result, err
	}
	for _, id := range ids {
		if !seen[id] {
			incomplete = append(incomplete, fmt.Errorf("%s: missing eligible typed edge", id))
		}
	}
	if len(incomplete) > 0 {
		return result, fmt.Errorf("%w: %w", ErrRejudgeAuditInput, errors.Join(incomplete...))
	}
	if result.Planned == 0 {
		err := errors.New("judge: no rows to plan after selection, evidence checks and label reconciliation")
		if ids != nil {
			err = fmt.Errorf("%w: %w", ErrRejudgeAuditInput, err)
		}
		return result, err
	}
	manifest.header.Mismatches = result.Mismatches
	keys := make(map[string]struct{}, len(manifest.rows))
	for _, row := range manifest.rows {
		keys[RequestKeyV2(row.Request.RequestBytes, row.Request.Profile.Fingerprint)] = struct{}{}
	}
	hits, err := st.CountCachedJudgeRequests(ctx, slices.Collect(maps.Keys(keys)))
	if err != nil {
		return result, err
	}
	result.CallsNeeded = len(keys) - hits
	return result, writeRejudgeAtomic(path, false, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		if err := enc.Encode(manifest.header); err != nil {
			return err
		}
		for _, row := range manifest.rows {
			if err := enc.Encode(row); err != nil {
				return err
			}
		}
		return nil
	})
}

func loadRejudgeDefinitions(dir, name string) (map[string]string, error) {
	_, relations, err := ingest.LoadPreset(dir, name)
	if err != nil {
		return nil, err
	}
	definitions := make(map[string]string, len(relations))
	for _, relation := range relations {
		key := graph.SanitizeRel(relation.Name)
		if _, duplicate := definitions[key]; duplicate {
			return nil, fmt.Errorf("preset %q has multiple definitions for relation %q", name, key)
		}
		definitions[key] = relation.Definition
	}
	return definitions, nil
}

func foldRejudgeName(s string) string { return cases.Fold().String(graph.DisplayName(s)) }

func reconcileRejudgeLabel(label AuditRow, fact graph.RejudgeFact, payload v2Payload) string {
	if head := foldRejudgeName(label.Head); head != foldRejudgeName(fact.HeadName) && head != foldRejudgeName(payload.Fact.Head) {
		return "head differs"
	}
	if tail := foldRejudgeName(label.Tail); tail != foldRejudgeName(fact.TailName) && tail != foldRejudgeName(payload.Fact.Tail) {
		return "tail differs"
	}
	if label.Relation != payload.Fact.Rel {
		return "relation differs"
	}
	if label.Evidence != ingest.EvidenceSentence(fact.Text, fact.HeadStart, fact.HeadEnd) {
		return "audit head sentence differs"
	}
	return ""
}

func scoredRejudgeLabel(label string) bool {
	return label == "ok" || label == "wrong" || label == "noise"
}
