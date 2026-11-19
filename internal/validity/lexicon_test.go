package validity

import (
	"slices"
	"testing"
)

func TestLexiconDigest(t *testing.T) {
	lexicon := readTestLexicon(t, "testdata/invented.yaml")
	const want = "bdef5d4d09752aba374c43f0a84800206ac9f252cdd65727d4696c538d3f4ed1"
	got, err := LexiconDigest(lexicon)
	if err != nil || got != want {
		t.Fatalf("LexiconDigest = %q, %v, want %q", got, err, want)
	}
	slices.Reverse(lexicon.StopTokens)
	reversed, err := LexiconDigest(lexicon)
	if err != nil || reversed == got {
		t.Fatalf("LexiconDigest with reversed stop list = %q, %v", reversed, err)
	}
}
