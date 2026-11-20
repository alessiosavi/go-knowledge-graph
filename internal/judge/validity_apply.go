package judge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
)

// ErrValidityStopped identifies an apply that stopped after taking the lease.
var ErrValidityStopped = errors.New("validity apply stopped")

func readFinalValidity(dir, approved string, draft validityDraft) (sealedValidityRun, []validity.FinalRow, validity.PlanSummary, error) {
	rewrite := sealedValidityRun{seal: noValidityRewrites}
	read := func(name string) ([]byte, error) { return readValidityFile(filepath.Join(dir, name)) }
	if len(draft.rewrites) != 0 {
		verdicts, err := read("rewrite-verdicts.jsonl")
		if err != nil {
			return rewrite, nil, validity.PlanSummary{}, err
		}
		raw, err := read("rewrite-attempts.json")
		if err != nil {
			return rewrite, nil, validity.PlanSummary{}, err
		}
		ledger, err := decodeRejudgeAttempts("rewrite-attempts.json", raw)
		if err != nil {
			return rewrite, nil, validity.PlanSummary{}, err
		}
		rewrite, err = validateSealedValidityRun(verdicts, raw, ledger, draft.rewriteDigest, draft.rewrites)
		if err != nil {
			return rewrite, nil, validity.PlanSummary{}, err
		}
	}
	raw, err := read("final-plan.json")
	if err != nil {
		return rewrite, nil, validity.PlanSummary{}, err
	}
	summary, err := validity.DecodePlanSummary(raw)
	if err != nil {
		return rewrite, nil, validity.PlanSummary{}, err
	}
	if summary.Deferral != "" && summary.Deferral != unconfirmedDeferral {
		return rewrite, nil, validity.PlanSummary{}, fmt.Errorf("unknown final-plan.json deferral %q", summary.Deferral)
	}
	projection, err := projectFinalValidity(draft, rewrite, summary.Deferral == unconfirmedDeferral)
	expected := projection.summary
	if err != nil {
		return rewrite, nil, expected, err
	}
	final, err := read("final.jsonl")
	if err != nil {
		return rewrite, nil, expected, err
	}
	if !bytes.Equal(projection.encoded, final) {
		return rewrite, nil, expected, errors.New("final.jsonl differs from the finalized outcomes")
	}
	if !reflect.DeepEqual(expected, summary) {
		return rewrite, nil, expected, errors.New("final-plan.json differs from the recomputed plan")
	}
	if !strings.EqualFold(expected.FinalDigest, approved) {
		return rewrite, nil, expected, errors.New("approved digest does not match the recomputed final digest")
	}
	return rewrite, projection.rows, expected, nil
}

func checkValidityCatalog(catalog validityCatalog, draft validityDraft, rules *validity.Rules) error {
	type inventoryKey struct{ id, bucket, prior string }
	delta := make(map[inventoryKey]int)
	for _, row := range catalog.inventory {
		delta[inventoryKey{row.ID, row.Bucket, row.PriorJudge}]++
	}
	for _, row := range draft.inventory {
		delta[inventoryKey{row.ID, row.Bucket, row.PriorJudge}]--
	}
	var firstID string
	mismatch := false
	for key, n := range delta {
		if n != 0 && (!mismatch || key.id < firstID) {
			firstID, mismatch = key.id, true
		}
	}
	if mismatch {
		return fmt.Errorf("live classifier inventory differs from the sealed inventory at id=%q", firstID)
	}
	rederived := make([]validity.InputsRow, 0, len(draft.inputs))
	for _, input := range draft.inputs {
		// The per-row transaction checks live parity and target drift. Compute
		// the collision preconditions at the sealed destinations using the
		// live occupants and confidence of each source.
		row := validity.InputsRow{ID: input.ID, Input: input.Input, Target: input.Target}
		fact := catalog.planned[row.ID].fact
		projectValiditySource(&row, fact, rules, draft.bulk[row.ID].verdict.Verdict, catalog.occupants)
		if row.Decision != input.Decision {
			return fmt.Errorf("%s: sealed validity decision differs from Decide", row.ID)
		}
		rederived = append(rederived, row)
	}
	rankValidityCandidates(rederived, catalog)
	for i, row := range rederived {
		sealed := draft.inputs[i]
		if !reflect.DeepEqual(row.Destination, sealed.Destination) || !reflect.DeepEqual(row.Occupants, sealed.Occupants) {
			return fmt.Errorf("%s: destination or live occupants differ from sealed inputs", row.ID)
		}
		if !reflect.DeepEqual(row.CandidateGroup, sealed.CandidateGroup) {
			return fmt.Errorf("%s: candidate ranking differs from sealed inputs", row.ID)
		}
		if row.Outcome != sealed.Outcome || row.Reason != sealed.Reason {
			return fmt.Errorf("%s: sealed validity outcome differs from the live projection", row.ID)
		}
	}
	return nil
}

func checkValidityRequestBytes(catalog validityCatalog, draft validityDraft, reader graph.CatalogReader) error {
	docs := make(map[string]struct{})
	for id := range draft.rewriteBySource {
		docs[catalog.planned[id].fact.DocID] = struct{}{}
	}
	matches, err := reader.TitleMatches(slices.Sorted(maps.Keys(docs)))
	if err != nil {
		return err
	}
	for _, input := range draft.inputs {
		e := catalog.planned[input.ID]
		source := draft.bulk[input.ID].manifest
		if !bytes.Equal(e.bytes, source.Request.RequestBytes) {
			return validityInputError(fmt.Errorf("bulk manifest request bytes differ at id=%q", input.ID))
		}
		candidate := draft.rewriteBySource[input.ID]
		if candidate.ID == "" {
			continue
		}
		input.Target = validityTitleTarget(matches[e.fact.DocID])
		rewritten, err := makeValidityRewrite(input, e, source)
		if err != nil {
			return fmt.Errorf("rewrites manifest id=%q (source id=%q): %w", candidate.ID, input.ID, err)
		}
		if !bytes.Equal(rewritten.Request.RequestBytes, candidate.Request.RequestBytes) {
			return validityInputError(fmt.Errorf("rewrites manifest request bytes differ at id=%q (source id=%q)", candidate.ID, input.ID))
		}
	}
	return nil
}

func checkValidityEvidence(fact graph.RejudgeFact, reader graph.CatalogReader, input validity.InputsRow,
	source, candidate rejudgeManifestRow, definition string) error {
	if !fact.ChunkOwned || fact.Preset != input.Preset || validityParityInput(fact) != input.Input {
		return errors.New("live parity inputs or chunk ownership differ from the sealed inputs")
	}
	matches, err := reader.TitleMatches([]string{fact.DocID})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(validityTitleTarget(matches[fact.DocID]), input.Target) {
		return errors.New("live title matches or resolved target differ from the sealed target")
	}
	e, reason, err := buildRejudgeEvidence(fact, definition)
	if err != nil {
		return err
	}
	if reason != "" {
		return fmt.Errorf("cannot rebuild source request: %s", reason)
	}
	batch := []rejudgeEvidence{e}
	if err := completeRejudgeEvidence(batch, func(chunks map[string]string) (map[string]string, error) {
		previous, duplicates, err := reader.PreviousChunks(chunks)
		if err == nil && len(duplicates) != 0 {
			err = fmt.Errorf("multiple preceding chunks for %s", fact.ChunkID)
		}
		return previous, err
	}); err != nil {
		return err
	}
	e = batch[0]
	if !bytes.Equal(e.bytes, source.Request.RequestBytes) {
		return errors.New("source request bytes differ from the sealed manifest")
	}
	if candidate.ID != "" {
		// Defence in depth: checkValidityRequestBytes compares every rewritten
		// request in preflight, and this row has already re-checked the target and
		// source bytes, so a mismatch here means the builder itself changed.
		rewritten, err := makeValidityRewrite(input, e, source)
		if err != nil {
			return err
		}
		if rewritten.ID != candidate.ID || candidate.Request.DocID != fact.DocID ||
			!bytes.Equal(rewritten.Request.RequestBytes, candidate.Request.RequestBytes) {
			return errors.New("rewritten request bytes differ from the sealed manifest")
		}
	}
	return nil
}

type validityLineage struct {
	ID             string            `json:"id"`
	PriorJudge     string            `json:"prior_judge"`
	PlannedOutcome validity.Outcome  `json:"planned_outcome"`
	ActualOutcome  *validity.Outcome `json:"actual_outcome"`
	Rule           string            `json:"rule"`
	Reason         string            `json:"reason"`
	NewID          string            `json:"new_id,omitempty"`
	SourceVerdict  string            `json:"source_verdict"`
	Evidence       string            `json:"evidence"`
}

type validityApplyTotals struct {
	Counts     validityCounts            `json:"counts"`
	TypedEdges *graph.ValidityEdgeCounts `json:"typed_edges"`
}

type validityApplySummary struct {
	Status       string                   `json:"status"`
	FinalDigest  string                   `json:"final_digest"`
	ValidityRun  string                   `json:"validity_run"`
	Before       graph.ValidityEdgeCounts `json:"before"`
	BucketCounts map[string]int           `json:"bucket_counts"`
	Projected    validityApplyTotals      `json:"projected"`
	Actual       validityApplyTotals      `json:"actual"`
	StopRow      string                   `json:"stop_row,omitempty"`
	StopReason   string                   `json:"stop_reason,omitempty"`
}

func projectedValidityEdges(before graph.ValidityEdgeCounts, rows []validity.FinalRow, draft validityDraft) graph.ValidityEdgeCounts {
	for _, row := range rows {
		if row.FinalOutcome == validity.OutcomeDeferred {
			continue
		}
		source := draft.bulk[row.ID].manifest
		if source.Judge == "confirmed" {
			before.ConfirmedOnly--
		}
		if source.Judge == "confirmed" || !source.LowConfidence {
			before.Default--
		}
		switch row.FinalOutcome {
		case validity.OutcomeStamp, validity.OutcomeRewriteConfirmed:
			before.ConfirmedOnly++
			before.Default++
		case validity.OutcomeDemote, validity.OutcomeRewriteUnclear:
			if !source.LowConfidence {
				before.Default++
			}
		}
	}
	return before
}

func validityWriteFor(input validity.InputsRow, row validity.FinalRow, draft validityDraft, rewrite sealedValidityRun, validityRun string) graph.ValidityWrite {
	source := draft.bulk[input.ID]
	w := graph.ValidityWrite{
		Source: source.manifest.RejudgeIdentity, DocID: source.manifest.Request.DocID,
		Outcome: row.FinalOutcome, ValidityRun: validityRun, Rule: row.Rule,
		Judge: source.verdict.Verdict, JudgeRun: draft.summary.ManifestDigest[:16], JudgePrompt: source.verdict.Fingerprint,
		Model: source.manifest.Request.Profile.Model,
	}
	candidate := draft.rewriteBySource[input.ID]
	if candidate.ID != "" {
		verdict := rewrite.verdicts[candidate.ID]
		w.Judge, w.JudgeRun, w.JudgePrompt = verdict.Verdict, draft.rewriteDigest[:16], verdict.Fingerprint
		w.Model = candidate.Request.Profile.Model
		w.Destination, w.HeadSurface = input.Destination, strings.TrimSpace(input.Target.Name)
	}
	switch row.FinalOutcome {
	case validity.OutcomeReject:
		w.Reason = "v3: " + source.verdict.Reason
	case validity.OutcomeValidityDrop:
		rule := row.Reason
		if rule == "" {
			rule = row.Rule
		}
		w.Judge, w.Reason = "validity", "validity: "+rule
	}
	return w
}

// ApplyValidity applies one finalized and owner-approved plan. The caller
// asserts writer quiescence and supplies a fresh report path.
func ApplyValidity(ctx context.Context, st *graph.Store, presetsDir, dir, approved, reportPath string) (err error) {
	if _, err := os.Lstat(reportPath); err == nil {
		return validityInputError(fmt.Errorf("validity apply report %q already exists", reportPath))
	} else if !errors.Is(err, os.ErrNotExist) {
		return validityInputError(fmt.Errorf("check validity apply report path: %w", err))
	}
	draft, err := readValidityDraft(dir)
	if err != nil {
		return validityInputError(err)
	}
	rewrite, rows, plan, err := readFinalValidity(dir, approved, draft)
	if err != nil {
		return validityInputError(err)
	}
	preset := draft.inputs[0].Preset
	checked, err := ingest.LoadCheckedPreset(presetsDir, preset, "")
	if err != nil {
		return validityInputError(err)
	}
	lexicon := checked.Hygiene.Validity
	if lexicon == nil || lexicon.Rules != plan.RulesID {
		return validityInputError(errors.New("preset rules id differs from the sealed plan"))
	}
	digest, err := validity.LexiconDigest(*lexicon)
	if err != nil {
		return validityInputError(err)
	}
	if digest != plan.LexiconDigest {
		return validityInputError(errors.New("preset lexicon digest differs from the sealed plan"))
	}
	rules, err := validity.NewRules(*lexicon)
	if err != nil {
		return validityInputError(err)
	}
	definitions, err := rejudgeRelationDefinitions(preset, checked.Relations)
	if err != nil {
		return validityInputError(err)
	}
	blocker, err := st.CheckValidityApply(ctx)
	if err != nil {
		return err
	}
	if blocker != "" {
		return validityInputError(errors.New(blocker))
	}
	err = st.ReadValidityCatalog(ctx, preset, func(edges []graph.CatalogEdge, reader graph.CatalogReader) error {
		catalog, err := classifyValidityCatalog(edges, reader, definitions)
		if err != nil {
			return err
		}
		if err := checkValidityCatalog(catalog, draft, rules); err != nil {
			return validityInputError(err)
		}
		return checkValidityRequestBytes(catalog, draft, reader)
	})
	if err != nil {
		return err
	}
	before, err := st.CountValidityEdges(ctx)
	if err != nil {
		return err
	}
	owner := rand.Text()
	rollback, blocker, err := st.AcquireValidityApplyLease(ctx, owner)
	if err != nil {
		return err
	}
	if blocker != "" {
		return validityInputError(errors.New(blocker))
	}
	var report *os.File
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if report == nil {
			err = errors.Join(err, rollback(cleanup))
			return
		}
		err = errors.Join(err, st.ReleaseLease(cleanup, graph.ValidityApplyLease, owner))
	}()
	report, err = os.OpenFile(reportPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return validityInputError(fmt.Errorf("open validity apply report: %w", err))
	}
	defer func() { err = errors.Join(err, report.Close()) }()

	projected := projectedValidityEdges(before, rows, draft)
	summary := validityApplySummary{
		Status: "applied", FinalDigest: plan.FinalDigest, ValidityRun: plan.FinalDigest[:16],
		Before: before, BucketCounts: plan.BucketCounts,
		Projected: validityApplyTotals{Counts: plan.Counts, TypedEdges: &projected},
		Actual:    validityApplyTotals{Counts: make(validityCounts)},
	}
	encoder := json.NewEncoder(report)
	for i, row := range rows {
		input := draft.inputs[i]
		plannedOutcome := row.FinalOutcome
		if row.FinalOutcome == validity.OutcomeDeferred {
			plannedOutcome = row.PlannedOutcome
		}
		line := validityLineage{
			ID: row.ID, PriorJudge: input.PriorJudge, PlannedOutcome: plannedOutcome, Rule: row.Rule,
			Reason: row.Reason, SourceVerdict: row.SourceVerdict, Evidence: input.Evidence,
		}
		if summary.StopReason == "" {
			w := validityWriteFor(input, row, draft, rewrite, summary.ValidityRun)
			if err := st.ApplyValidityFact(ctx, owner, i == 0, w, func(fact graph.RejudgeFact, reader graph.CatalogReader) error {
				return checkValidityEvidence(fact, reader, input, draft.bulk[row.ID].manifest, draft.rewriteBySource[row.ID], definitions[input.Relation])
			}); err != nil {
				summary.StopRow, summary.StopReason = row.ID, err.Error()
			} else {
				line.ActualOutcome, line.NewID = &row.FinalOutcome, row.NewID
				summary.Actual.Counts.add(row.FinalOutcome, input.Relation, row.Rule)
			}
		}
		if err := encoder.Encode(line); err != nil {
			return fmt.Errorf("write validity apply report: %w", err)
		}
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	after, countErr := st.CountValidityEdges(finish)
	if countErr == nil {
		summary.Actual.TypedEdges = &after
	}
	if summary.StopReason == "" {
		switch {
		case countErr != nil:
			summary.StopReason = fmt.Sprintf("count typed edges after apply: %v", countErr)
		case !reflect.DeepEqual(summary.Actual, summary.Projected):
			summary.StopReason = "actual outcomes or visible typed edge counts differ from projection"
		}
	}
	if summary.StopReason != "" {
		summary.Status = "stopped"
	}
	if err := encoder.Encode(summary); err != nil {
		return fmt.Errorf("write validity apply summary: %w", err)
	}
	if err := report.Sync(); err != nil {
		return fmt.Errorf("sync validity apply report: %w", err)
	}
	if summary.Status == "stopped" {
		if summary.StopRow != "" {
			return fmt.Errorf("%w at %s: %s", ErrValidityStopped, summary.StopRow, summary.StopReason)
		}
		return fmt.Errorf("%w: %s", ErrValidityStopped, summary.StopReason)
	}
	return nil
}
