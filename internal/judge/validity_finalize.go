package judge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
)

const noValidityRewrites = "no-rewrites"

type validityDraft struct {
	summary         validity.PlanSummary
	inputs          []validity.InputsRow
	bulk            map[string]validityBulkSource
	rewriteBySource map[string]rejudgeManifestRow
	rewrites        map[string]rejudgeManifestRow
	rewriteDigest   string
}

type validityBulkSource struct {
	verdict        rejudgeVerdict
	docID, chunkID string
}

// FinalizeValidity validates all sealed inputs offline and publishes final
// outcomes and the affected-sources report. An empty verdict path explicitly
// selects the no-rewrites path.
func FinalizeValidity(ctx context.Context, dir, verdictPath string) (string, error) {
	draft, err := readValidityDraft(dir)
	if err != nil {
		return "", validityInputError(err)
	}
	var rewrite sealedValidityRun
	switch {
	case verdictPath == "" && len(draft.rewrites) != 0:
		return "", validityInputError(errors.New("-no-rewrites requires zero candidates"))
	case verdictPath != "" && len(draft.rewrites) == 0:
		return "", validityInputError(errors.New("zero candidates require -no-rewrites"))
	case verdictPath == "":
		rewrite.seal = noValidityRewrites
	default:
		rewrite, err = readSealedValidityRun(verdictPath, draft.rewriteDigest, draft.rewrites)
		if err != nil {
			return "", validityInputError(err)
		}
	}

	rows := make([]validity.FinalRow, 0, len(draft.inputs))
	finalBySource := make(map[string]validity.FinalRow, len(draft.inputs))
	finalCounts := make(validityCounts)
	for _, input := range draft.inputs {
		row := finalizeValidityRow(input, draft.bulk[input.ID].verdict, draft.rewriteBySource[input.ID], rewrite.verdicts)
		finalBySource[row.ID] = row
		rows = append(rows, row)
		finalCounts.add(row.FinalOutcome, input.Relation, row.Rule)
	}
	finalBytes, err := validityJSONLines(rows)
	if err != nil {
		return "", err
	}
	summary := draft.summary
	summary.Counts = finalCounts
	summary.RewriteVerdictSeal = rewrite.seal
	summary.FinalDigest = validityDigest([]byte(summary.DraftDigest), []byte(rewrite.seal), finalBytes)
	summaryBytes, err := validityJSONLines([]validity.PlanSummary{summary})
	if err != nil {
		return "", err
	}
	affected, affectedSummary := affectedValiditySources(draft.inputs, draft.bulk, finalBySource)
	affectedBytes, err := validityJSONLines(affected)
	if err != nil {
		return "", err
	}
	reportBytes, err := validityJSONLines([]map[string]affectedValidityCount{affectedSummary})
	if err != nil {
		return "", err
	}
	files := []validityFile{
		{"final.jsonl", finalBytes},
		{"affected.jsonl", affectedBytes},
		{"affected-summary.json", reportBytes},
	}
	if verdictPath != "" {
		files = append(files, validityFile{"rewrite-verdicts.jsonl", rewrite.bytes}, validityFile{"rewrite-attempts.json", rewrite.ledger})
	}
	files = append(files, validityFile{"final-plan.json", summaryBytes})
	return summary.FinalDigest, writeValidityFiles(ctx, dir, files)
}

func readValidityDraft(dir string) (validityDraft, error) {
	read := func(name string) ([]byte, error) { return readValidityFile(filepath.Join(dir, name)) }
	var draft validityDraft
	raw, err := read("plan.json")
	if err != nil {
		return draft, err
	}
	summary, err := validity.DecodePlanSummary(raw)
	if err != nil {
		return draft, err
	}
	draft.summary = summary
	lexicon, err := read("lexicon.json")
	if err != nil {
		return draft, err
	}
	var policy validity.Lexicon
	if err := json.Unmarshal(lexicon, &policy); err != nil {
		return draft, err
	}
	if policy.Rules != summary.RulesID || fmt.Sprintf("%x", sha256.Sum256(lexicon)) != summary.LexiconDigest {
		return draft, errors.New("lexicon digest or rules id mismatch")
	}
	inputs, err := read("inputs.jsonl")
	if err != nil {
		return draft, err
	}
	rewrites, err := read("rewrites.jsonl")
	if err != nil {
		return draft, err
	}
	if validityDraftDigest(summary, inputs, rewrites) != summary.DraftDigest || summary.FinalDigest != "" || summary.RewriteVerdictSeal != "" {
		return draft, errors.New("draft digest mismatch or unexpected final seal in plan.json")
	}
	inputByID := make(map[string]validity.InputsRow)
	if err := scanRejudgeLines(bytes.NewReader(inputs), func(_ int, raw []byte) error {
		row, err := validity.DecodeInputsRow(raw)
		if err != nil {
			return err
		}
		if row.ID == "" || inputByID[row.ID].ID != "" || row.Bucket != catalogPlanned {
			return fmt.Errorf("invalid or duplicate inputs row %q", row.ID)
		}
		inputByID[row.ID] = row
		draft.inputs = append(draft.inputs, row)
		return nil
	}); err != nil {
		return draft, err
	}
	if len(draft.inputs) == 0 {
		return draft, errors.New("draft has no planned inputs")
	}
	if err := checkDraftInventory(read, draft, inputByID); err != nil {
		return draft, err
	}
	draft.bulk, err = readDraftBulk(read, summary, inputByID)
	if err != nil {
		return draft, err
	}
	draft.rewrites, draft.rewriteBySource, err = decodeValidityRewrites(rewrites, inputByID, summary.Counts)
	if err != nil {
		return draft, err
	}
	draft.rewriteDigest = fmt.Sprintf("%x", sha256.Sum256(rewrites))
	return draft, nil
}

func checkDraftInventory(read func(string) ([]byte, error), draft validityDraft, inputs map[string]validity.InputsRow) error {
	raw, err := read("inventory.jsonl")
	if err != nil {
		return err
	}
	preset := draft.inputs[0].Preset
	buckets := make(map[string]int)
	var inventory []validityInventoryRow
	planned := make(map[string]bool)
	if err := scanRejudgeLines(bytes.NewReader(raw), func(_ int, raw []byte) error {
		var row validityInventoryRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if row.Bucket == catalogPlanned {
			if input, ok := inputs[row.ID]; !ok || input.Preset != preset || input.PriorJudge != row.PriorJudge || planned[row.ID] {
				return fmt.Errorf("inventory does not match input %s", row.ID)
			}
			planned[row.ID] = true
		}
		buckets[row.Bucket]++
		inventory = append(inventory, row)
		return nil
	}); err != nil {
		return err
	}
	files, digest, err := inventoryArtifacts(preset, inventory)
	if err != nil {
		return err
	}
	if digest != draft.inputs[0].InventoryDigest || len(planned) != len(inputs) || !maps.Equal(buckets, draft.summary.BucketCounts) {
		return errors.New("inventory digest, counts or planned population mismatch")
	}
	for _, file := range files {
		actual, err := read(file.name)
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, file.data) {
			return fmt.Errorf("%s differs from the sealed inventory", file.name)
		}
	}
	return nil
}

func readDraftBulk(read func(string) ([]byte, error), summary validity.PlanSummary, inputs map[string]validity.InputsRow) (map[string]validityBulkSource, error) {
	manifestBytes, err := read("source-manifest.jsonl")
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(manifestBytes)) != summary.ManifestDigest {
		return nil, errors.New("source manifest digest mismatch")
	}
	manifest, err := decodeRejudgeManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		return nil, err
	}
	if len(manifest.rows) != len(inputs) || len(manifest.header.Mismatches) != 0 {
		return nil, errors.New("source manifest population mismatch")
	}
	rows := make(map[string]rejudgeManifestRow)
	for _, row := range manifest.rows {
		if input, ok := inputs[row.ID]; !ok || input.PriorJudge != row.Judge || input.Preset != row.Preset || rows[row.ID].ID != "" {
			return nil, fmt.Errorf("source manifest row %s differs from inputs", row.ID)
		}
		rows[row.ID] = row
	}
	ledgerBytes, err := read("source-attempts.json")
	if err != nil {
		return nil, err
	}
	ledger, err := decodeRejudgeAttempts("source-attempts.json", ledgerBytes)
	if err != nil {
		return nil, err
	}
	verdictBytes, err := read("source-verdicts.jsonl")
	if err != nil {
		return nil, err
	}
	bulk, err := validateSealedValidityRun(verdictBytes, ledgerBytes, ledger, summary.ManifestDigest, rows)
	if err != nil {
		return nil, err
	}
	if bulk.seal != summary.VerdictSeal {
		return nil, errors.New("source verdict seal mismatch")
	}
	sources := make(map[string]validityBulkSource, len(rows))
	for id, row := range rows {
		sources[id] = validityBulkSource{verdict: bulk.verdicts[id], docID: row.Request.DocID, chunkID: row.ChunkID}
	}
	return sources, nil
}

func decodeValidityRewrites(raw []byte, inputs map[string]validity.InputsRow, counts validityCounts) (map[string]rejudgeManifestRow, map[string]rejudgeManifestRow, error) {
	rows, bySource := make(map[string]rejudgeManifestRow), make(map[string]rejudgeManifestRow)
	err := scanRejudgeLines(bytes.NewReader(raw), func(line int, raw []byte) error {
		if line == 1 {
			var header rejudgeManifestHeader
			if err := json.Unmarshal(raw, &header); err != nil {
				return err
			}
			if header.Version != rejudgeManifestVersion || len(header.Mismatches) != 0 {
				return errors.New("invalid rewrite manifest header")
			}
			return nil
		}
		var row validityRewriteRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		source := inputs[row.SourceID]
		if source.Outcome != validity.OutcomeRewriteCandidate || source.Destination == nil || row.ID == "" ||
			rows[row.ID].ID != "" || bySource[row.SourceID].ID != "" {
			return fmt.Errorf("invalid or duplicate rewrite for source %s", row.SourceID)
		}
		k := source.Destination
		if row.ID != graph.FactAuditID(k.HeadKey, k.Rel, k.TailKey, k.ChunkID) ||
			row.HeadKey != k.HeadKey || row.Relation != k.Rel || row.TailKey != k.TailKey || row.ChunkID != k.ChunkID {
			return fmt.Errorf("rewrite %s differs from its destination", row.ID)
		}
		rows[row.ID], bySource[row.SourceID] = row.rejudgeManifestRow, row.rejudgeManifestRow
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	actual := make(validityCounts)
	for _, input := range inputs {
		actual.add(input.Outcome, input.Relation, input.Rule)
		if source, merged := strings.CutPrefix(input.Reason, "collision-merged:"); merged {
			winner := inputs[source]
			if input.Destination == nil || winner.Destination == nil || *input.Destination != *winner.Destination || bySource[source].ID == "" {
				return nil, nil, fmt.Errorf("merged source %s has no candidate at its destination", input.ID)
			}
		}
		if input.Outcome == validity.OutcomeRewriteCandidate && bySource[input.ID].ID == "" {
			return nil, nil, fmt.Errorf("missing rewrite for candidate %s", input.ID)
		}
	}
	if len(raw) == 0 || !reflect.DeepEqual(actual, counts) {
		return nil, nil, errors.New("rewrite manifest header or plan counts mismatch")
	}
	return rows, bySource, nil
}

func finalizeValidityRow(input validity.InputsRow, source rejudgeVerdict, candidate rejudgeManifestRow,
	verdicts map[string]rejudgeVerdict) validity.FinalRow {
	row := validity.FinalRow{
		ID: input.ID, PlannedOutcome: input.Outcome, FinalOutcome: input.Outcome, Reason: input.Reason,
		Rule: input.Rule, SourceVerdict: source.Verdict,
	}
	if input.Outcome == validity.OutcomeRewriteCandidate {
		row.RewriteVerdict = verdicts[candidate.ID].Verdict
		switch row.RewriteVerdict {
		case "confirmed":
			row.FinalOutcome, row.NewID = validity.OutcomeRewriteConfirmed, candidate.ID
		case "unclear":
			row.FinalOutcome, row.NewID = validity.OutcomeRewriteUnclear, candidate.ID
		case "refuted":
			row.FinalOutcome, row.Reason = validity.OutcomeValidityDrop, "reattach-refuted"
		}
	}
	return row
}

type affectedValidityRow struct {
	SourceID string `json:"source_id"`
	DocID    string `json:"doc_id"`
	ChunkID  string `json:"chunk_id"`
	validity.Input
	Rule                    string                     `json:"rule"`
	Reason                  string                     `json:"reason"`
	Destination             *validity.DestinationKey   `json:"destination,omitempty"`
	States                  []affectedDestinationState `json:"destination_states"`
	HasConfirmedDestination bool                       `json:"has_confirmed_destination"`
}

type affectedDestinationState struct {
	ID       string `json:"id"`
	EdgeID   *int   `json:"edge_id,omitempty"`
	SourceID string `json:"source_id,omitempty"`
	Outcome  string `json:"outcome"`
	Live     bool   `json:"live"`
	Judge    string `json:"judge"`
}

type affectedValidityCount struct {
	Sources                     int `json:"sources"`
	WithoutConfirmedDestination int `json:"without_confirmed_destination"`
}

func affectedValiditySources(inputs []validity.InputsRow, bulk map[string]validityBulkSource, final map[string]validity.FinalRow) ([]affectedValidityRow, map[string]affectedValidityCount) {
	var affected []affectedValidityRow
	counts := make(map[string]affectedValidityCount)
	for _, input := range inputs {
		row := affectedValidityRow{
			SourceID: input.ID, Input: input.Input, Rule: input.Rule, Reason: input.Reason, Destination: input.Destination,
			States: []affectedDestinationState{},
		}
		group := input.Reason
		switch {
		case input.Reason == "no-target" || input.Reason == "ambiguous-target":
			group = "no-target"
		case input.Reason == "collision-occupied":
			for _, occupant := range input.Occupants {
				state := affectedDestinationState{ID: occupant.ID, EdgeID: occupant.EdgeID, Outcome: "untouched", Live: true, Judge: occupant.Judge}
				if row, ok := final[occupant.ID]; ok {
					state.Outcome = string(row.FinalOutcome)
					state.Live, state.Judge = priorDestinationState(row.FinalOutcome)
				}
				row.States = append(row.States, state)
			}
		case strings.HasPrefix(input.Reason, "collision-merged:"):
			group = "collision-merged"
			winner := final[strings.TrimPrefix(input.Reason, "collision-merged:")]
			k := input.Destination
			state := affectedDestinationState{
				ID: graph.FactAuditID(k.HeadKey, k.Rel, k.TailKey, k.ChunkID), SourceID: winner.ID, Outcome: string(winner.FinalOutcome),
			}
			switch winner.FinalOutcome {
			case validity.OutcomeRewriteConfirmed:
				state.Live, state.Judge = true, "confirmed"
			case validity.OutcomeRewriteUnclear:
				state.Live, state.Judge = true, "unclear"
			}
			row.States = append(row.States, state)
		default:
			continue
		}
		source := bulk[input.ID]
		row.DocID, row.ChunkID = source.docID, source.chunkID
		for _, state := range row.States {
			row.HasConfirmedDestination = row.HasConfirmedDestination || (state.Live && state.Judge == "confirmed")
		}
		count := counts[group]
		count.Sources++
		if !row.HasConfirmedDestination {
			count.WithoutConfirmedDestination++
		}
		counts[group] = count
		affected = append(affected, row)
	}
	return affected, counts
}

func priorDestinationState(outcome validity.Outcome) (bool, string) {
	switch outcome {
	case validity.OutcomeStamp:
		return true, "confirmed"
	case validity.OutcomeDemote:
		return true, "unclear"
	default:
		return false, ""
	}
}
