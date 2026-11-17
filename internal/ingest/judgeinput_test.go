package ingest

import (
	"encoding/json"
	"testing"
)

func TestCanonicalJudgeInputRoundTrip(t *testing.T) {
	for _, in := range []FactInput{
		{Head: "Acme-Max", Rel: "uses", Tail: "Zorbo", Evidence: "Acme-Max uses Zorbo.", DocContext: "Quillon Ltd", Confidence: 0.75},
		{Head: "Zorbó \"星\"", Rel: "uses", Tail: "Quillon Ltd", DocContext: "Unicode\n<>&", Confidence: 0},
		{},
	} {
		b := CanonicalJudgeInput(in)
		out, err := DecodeJudgeInput(b)
		if err != nil || out != in {
			t.Fatalf("round trip: in=%+v out=%+v err=%v", in, out, err)
		}
	}
	in := FactInput{Head: "Acme-Max", Rel: "uses", Tail: "Zorbo", Confidence: 0.75, Evidence: "e", DocContext: "d"}
	want := `{"doc_context":"d","evidence":"e","fact":{"confidence":0.75,"head":"Acme-Max","rel":"uses","tail":"Zorbo"}}`
	if got := string(CanonicalJudgeInput(in)); got != want {
		t.Fatalf("canonical payload = %s, want %s", got, want)
	}
}

func TestDecodeJudgeInputMalformed(t *testing.T) {
	for _, b := range []string{`{`, `{"fact":{"confidence":"high"}}`} {
		if _, err := DecodeJudgeInput([]byte(b)); err == nil {
			t.Errorf("DecodeJudgeInput(%q): want error", b)
		}
	}
}

// TestCanonicalJudgeInputMatchesLegacyPayload pins the bytes: the judge must
// receive exactly the message it received when the payload was a map.
func TestCanonicalJudgeInputMatchesLegacyPayload(t *testing.T) {
	for _, in := range []FactInput{
		{Head: "Zorbo", Rel: "located_in", Tail: "Quillon <Ltd> & Co", Evidence: "Zorbo \"is\" in Quillon.", DocContext: "Città", Confidence: 0.42},
		{},
	} {
		legacy, err := json.Marshal(map[string]any{
			"fact": map[string]any{
				"head":       in.Head,
				"rel":        in.Rel,
				"tail":       in.Tail,
				"confidence": in.Confidence,
			},
			"evidence":    in.Evidence,
			"doc_context": in.DocContext,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := CanonicalJudgeInput(in); string(got) != string(legacy) {
			t.Errorf("CanonicalJudgeInput = %s; want legacy %s", got, legacy)
		}
	}
}
