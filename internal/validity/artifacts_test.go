package validity

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestInputsRowJSON(t *testing.T) {
	checkArtifactJSON(t, DecodeInputsRow, `{
		"id":"source-1","domain":"test","preset":"invented","bucket":"planned","prior_judge":"",
		"head":"zorb","relation":"has_feature","tail":"Widget","title":"Zorb","evidence":"A zorb has a widget.",
		"action":"reattach","rule":"ra-title-product",
		"target":{"key":"zorb-title","name":"Zorb","type":"zorb","title_match_keys":["zorb-title"]},
		"outcome":"validity-drop","reason":"collision-occupied",
		"destination":{"head_key":"zorb-title","rel":"has_feature","tail_key":"widget","chunk_id":"chunk-1"},
		"occupants":[{"id":"occupant-1","judge":"unclear"}],
		"candidate_group":[{"source_id":"source-1","confidence":0.9}]
	}`)
	checkArtifactJSON(t, DecodeInputsRow, `{
		"id":"source-2","domain":"test","preset":"invented","bucket":"planned","prior_judge":"confirmed",
		"head":"zorb","relation":"has_feature","tail":"Widget","title":"Zorb","evidence":"A zorb has a widget.",
		"action":"reattach","rule":"ra-title-product",
		"target":{"key":"","name":"","type":"","title_match_keys":["zorb-1","zorb-2"]},
		"outcome":"validity-drop","reason":"ambiguous-target","occupants":[],"candidate_group":[]
	}`)
	for _, nested := range []string{"target", "destination", "occupants", "candidate_group"} {
		wrapper := `{"typo":true}`
		if nested == "occupants" || nested == "candidate_group" {
			wrapper = "[" + wrapper + "]"
		}
		_, err := DecodeInputsRow([]byte(`{"` + nested + `":` + wrapper + `}`))
		if err == nil || !strings.Contains(err.Error(), `unknown field "typo"`) {
			t.Errorf("DecodeInputsRow accepts unknown %s field: %v", nested, err)
		}
	}
}

func TestInputsRowZeroValueJSON(t *testing.T) {
	const wire = `{"id":"","domain":"","preset":"","bucket":"","prior_judge":"",` +
		`"head":"","relation":"","tail":"","title":"","evidence":"","action":"","rule":"",` +
		`"target":{"key":"","name":"","type":"","title_match_keys":[]},"outcome":"","reason":"",` +
		`"occupants":[],"candidate_group":[]}`
	encoded, err := json.Marshal(InputsRow{})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != wire {
		t.Errorf("zero value encodes as %s, want %s", encoded, wire)
	}

	for _, tc := range []struct{ name, wire string }{
		{"empty arrays", wire},
		{"null lists", strings.ReplaceAll(wire, `:[]`, `:null`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, err := DecodeInputsRow([]byte(tc.wire))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(row, InputsRow{}) {
				t.Errorf("decoded row = %#v, want the zero value", row)
			}
			var direct InputsRow
			if err := json.Unmarshal([]byte(tc.wire), &direct); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(direct, InputsRow{}) {
				t.Errorf("unmarshaled row = %#v, want the zero value", direct)
			}
		})
	}
}

func TestFinalRowJSON(t *testing.T) {
	checkArtifactJSON(t, DecodeFinalRow, `{
		"id":"source-1","planned_outcome":"rewrite-candidate","final_outcome":"rewrite-unclear",
		"reason":"","rule":"made-of-reattach","new_id":"rewritten-1","source_verdict":"confirmed","rewrite_verdict":"unclear"
	}`)
	checkArtifactJSON(t, DecodeFinalRow, `{
		"id":"source-2","planned_outcome":"reject","final_outcome":"reject","reason":"","rule":"","source_verdict":"refuted"
	}`)
}

func TestPlanSummaryJSON(t *testing.T) {
	const draft = `{
		"rules_id":"catalog-v3","lexicon_digest":"a1","manifest_digest":"a2","verdict_seal":"a3",
		"counts":{"stamp":{"has_feature":{"default-keep":2}}},"bucket_counts":{"planned":2},"draft_digest":"a4"`
	checkArtifactJSON(t, DecodePlanSummary, draft+`}`)
	checkArtifactJSON(t, DecodePlanSummary, draft+`,"rewrite_verdict_seal":"no-rewrites","final_digest":"a5"}`)
}

func checkArtifactJSON[T any](t *testing.T, decode func([]byte) (T, error), wire string) {
	t.Helper()
	got, err := decode([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := json.Compact(&want, []byte(wire)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, want.Bytes()) {
		t.Errorf("round trip = %s, want %s", encoded, &want)
	}
	if _, err := decode([]byte(`{"typo":true}`)); err == nil || !strings.Contains(err.Error(), `unknown field "typo"`) {
		t.Errorf("unknown-field error = %v", err)
	}
	for _, invalid := range []string{`null`, `{}` + "\n" + `{}`, `{}{}bad`} {
		if _, err := decode([]byte(invalid)); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}
