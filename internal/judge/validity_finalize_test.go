package judge

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"reflect"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
)

func projectionDeferralDraft() validityDraft {
	drop := func(id, reason string) validity.InputsRow {
		return validity.InputsRow{
			ID: id, Input: validity.Input{Head: id, Relation: "uses", Tail: "tool", Title: "Catalog", Evidence: "Uses a tool."},
			Decision: validity.Decision{Action: validity.Reattach, Rule: "reattach-rule"},
			Outcome:  validity.OutcomeValidityDrop, Reason: reason,
		}
	}
	noTarget := drop("no-target", "no-target")
	losesDestination := drop("loses-destination", "collision-occupied")
	losesDestination.Destination = &validity.DestinationKey{HeadKey: noTarget.ID, Rel: "uses", TailKey: "tool", ChunkID: "chunk"}
	losesDestination.Occupants = []validity.Occupant{{ID: noTarget.ID, Judge: "confirmed"}}
	keepsDestination := drop("keeps-destination", "collision-occupied")
	keepsDestination.Destination = &validity.DestinationKey{HeadKey: "stamp", Rel: "uses", TailKey: "tool", ChunkID: "chunk"}
	keepsDestination.Occupants = []validity.Occupant{{ID: "stamp", Judge: "confirmed"}}
	stamp := drop("stamp", "")
	stamp.Decision = validity.Decision{Action: validity.Keep, Rule: "keep-rule"}
	stamp.Outcome = validity.OutcomeStamp

	draft := validityDraft{
		summary: validity.PlanSummary{DraftDigest: "fixture-draft"},
		inputs:  []validity.InputsRow{noTarget, losesDestination, keepsDestination, stamp},
		bulk:    make(map[string]validityBulkSource),
	}
	for _, input := range draft.inputs {
		source := validityBulkSource{verdict: rejudgeVerdict{Verdict: "confirmed"}}
		source.manifest.Request.DocID, source.manifest.ChunkID = "doc", "chunk"
		draft.bulk[input.ID] = source
	}
	return draft
}

func projectionDeferralJSON[T any](t *testing.T, rows []T) []byte {
	t.Helper()
	raw, err := validityJSONLines(rows)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProjectFinalValidityDeferral(t *testing.T) {
	draft := projectionDeferralDraft()
	rewrite := sealedValidityRun{seal: noValidityRewrites}
	legacy, err := projectFinalValidity(draft, rewrite, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("without flag", func(t *testing.T) {
		// Hashes of the exact artifact bytes from the pre-deferral implementation.
		for _, artifact := range []struct {
			name string
			raw  []byte
			hash string
		}{
			{"final.jsonl", legacy.encoded, "f952d1002472087f39936e5f87c4e8f9562f3439fbe11af9d9755f4c99f13d34"},
			{"final-plan.json", projectionDeferralJSON(t, []validity.PlanSummary{legacy.summary}), "0c1f1f5e3e5aca7732fa4c7fe7677abc1611be00586e35a14b220c2cf0ab40d0"},
			{"affected.jsonl", projectionDeferralJSON(t, legacy.affected), "0a3e59de849a45d5c0c3fd6bb12bf43bf73aef32d63c9c36a51865d7043b26e0"},
			{"affected-summary.json", projectionDeferralJSON(t, []map[string]affectedValidityCount{legacy.affectedSummary}), "48d34ddd677eda6db5b7c112bb43851126a0af0d642b8539e52b9e715b1cdf87"},
		} {
			if hash := fmt.Sprintf("%x", sha256.Sum256(artifact.raw)); hash != artifact.hash {
				t.Errorf("%s SHA-256 = %s, want %s; bytes = %q", artifact.name, hash, artifact.hash, artifact.raw)
			}
			if bytes.Contains(artifact.raw, []byte(`"deferral":`)) || bytes.Contains(artifact.raw, []byte(`"deferred":`)) {
				t.Errorf("%s includes deferral fields: %s", artifact.name, artifact.raw)
			}
		}
	})
	t.Run("with flag", func(t *testing.T) {
		if len(legacy.rows) != 4 || len(legacy.affected) != 3 {
			t.Fatalf("legacy fixture has %d final and %d affected rows, want 4 and 3", len(legacy.rows), len(legacy.affected))
		}
		got, err := projectFinalValidity(draft, rewrite, true)
		if err != nil {
			t.Fatal(err)
		}
		plan := projectionDeferralJSON(t, []validity.PlanSummary{got.summary})
		if !bytes.Contains(plan, []byte(`"deferral":"unconfirmed-destination"`)) {
			t.Errorf("final-plan.json is missing deferral mode: %s", plan)
		}
		wantRows := append([]validity.FinalRow(nil), legacy.rows...)
		wantRows[0].FinalOutcome = validity.OutcomeDeferred
		wantRows[1].FinalOutcome = validity.OutcomeDeferred
		if !reflect.DeepEqual(got.rows, wantRows) {
			t.Errorf("final rows = %+v, want %+v", got.rows, wantRows)
		}
		wantCounts := map[validity.Outcome]map[string]map[string]int{
			validity.OutcomeDeferred:     {"uses": {"reattach-rule": 2}},
			validity.OutcomeValidityDrop: {"uses": {"reattach-rule": 1}},
			validity.OutcomeStamp:        {"uses": {"keep-rule": 1}},
		}
		if !reflect.DeepEqual(got.summary.Counts, wantCounts) {
			t.Errorf("final counts = %v, want %v", got.summary.Counts, wantCounts)
		}
		wantAffected := append([]affectedValidityRow(nil), legacy.affected...)
		for i, deferred := range []bool{true, true, false} {
			wantAffected[i].Deferred = new(deferred)
		}
		affectedBytes := projectionDeferralJSON(t, got.affected)
		if want := projectionDeferralJSON(t, wantAffected); !bytes.Equal(affectedBytes, want) {
			t.Errorf("affected.jsonl = %s, want %s", affectedBytes, want)
		}
		wantSummary := map[string]affectedValidityCount{
			"no-target":          {Sources: 1, WithoutConfirmedDestination: 1, Deferred: new(1)},
			"collision-occupied": {Sources: 2, WithoutConfirmedDestination: 1, Deferred: new(1)},
		}
		if !reflect.DeepEqual(got.affectedSummary, wantSummary) {
			t.Errorf("affected-summary.json = %s, want %s",
				projectionDeferralJSON(t, []map[string]affectedValidityCount{got.affectedSummary}),
				projectionDeferralJSON(t, []map[string]affectedValidityCount{wantSummary}))
		}
	})
	t.Run("group with zero deferrals", func(t *testing.T) {
		draft.inputs = draft.inputs[2:]
		got, err := projectFinalValidity(draft, rewrite, true)
		if err != nil {
			t.Fatal(err)
		}
		results := projectionDeferralJSON(t, []map[string]affectedValidityCount{got.affectedSummary})
		const want = "{\"collision-occupied\":{\"sources\":1,\"without_confirmed_destination\":0,\"deferred\":0}}\n"
		if string(results) != want {
			t.Errorf("affected-summary.json = %s, want %s", results, want)
		}
	})
}
