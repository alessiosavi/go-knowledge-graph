package dedup

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/alessiosavi/GoGPUtils/minhash"
	"github.com/alessiosavi/GoGPUtils/textnorm"
)

// boilerplate is long enough (73 words) that a one-word change keeps the
// exact 5-shingle Jaccard at/above 0.85.
const boilerplate = "The quarterly operations report describes routine maintenance procedures for the warehouse team. " +
	"Every operator must log inbound shipments before noon, verify pallet labels against the manifest, " +
	"and file damaged goods in the quarantine area for afternoon inspection. " +
	"Safety drills run weekly and attendance is recorded by the shift supervisor in the central operations log. " +
	"All personnel review the updated evacuation routes and confirm their emergency contact details during the monthly onboarding session."

const boilerplateVariant = "The annual operations report describes routine maintenance procedures for the warehouse team. " +
	"Every operator must log inbound shipments before noon, verify pallet labels against the manifest, " +
	"and file damaged goods in the quarantine area for afternoon inspection. " +
	"Safety drills run weekly and attendance is recorded by the shift supervisor in the central operations log. " +
	"All personnel review the updated evacuation routes and confirm their emergency contact details during the monthly onboarding session."

const unrelated = "Quantum chromodynamics couples gluons to colour charge through the strong interaction Lagrangian density."

func tokensOf(t *testing.T, text string) []string {
	t.Helper()
	tokens, err := textnorm.MeaningPreset().SplitTokens().Run(text)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	return tokens
}

func TestFingerprintDeterministic(t *testing.T) {
	a, err := Fingerprint(boilerplate)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	b, err := Fingerprint(boilerplate)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if a.ExactKey != b.ExactKey || !slices.Equal(a.Sig, b.Sig) || !slices.Equal(a.Bands, b.Bands) {
		t.Errorf("fingerprint not deterministic:\n%+v\n%+v", a, b)
	}
	if len(a.Sig) != 64 || len(a.Bands) != 16 {
		t.Errorf("sig len %d, bands len %d; want 64 and 16", len(a.Sig), len(a.Bands))
	}
	if a.Version != minhash.SignatureVersion {
		t.Errorf("Version = %d, want library constant %d", a.Version, minhash.SignatureVersion)
	}
}

func TestNearDuplicatesShareBands(t *testing.T) {
	a, err := Fingerprint(boilerplate)
	if err != nil {
		t.Fatalf("Fingerprint(a): %v", err)
	}
	b, err := Fingerprint(boilerplateVariant)
	if err != nil {
		t.Fatalf("Fingerprint(b): %v", err)
	}
	shared := false
	set := make(map[int64]bool, len(a.Bands))
	for _, k := range a.Bands {
		set[k] = true
	}
	for _, k := range b.Bands {
		if set[k] {
			shared = true
			break
		}
	}
	if !shared {
		t.Error("near duplicates share no band key")
	}
	j := VerifySimilar(tokensOf(t, boilerplate), tokensOf(t, boilerplateVariant))
	if j < 0.85 {
		t.Errorf("exact Jaccard = %v, want >= 0.85", j)
	}
}

func TestUnrelatedTextNoCandidates(t *testing.T) {
	a, err := Fingerprint(boilerplate)
	if err != nil {
		t.Fatalf("Fingerprint(a): %v", err)
	}
	b, err := Fingerprint(unrelated)
	if err != nil {
		t.Fatalf("Fingerprint(b): %v", err)
	}
	ix := New()
	if err := ix.Add("a", a); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if cands := ix.Lookup(a); !slices.Contains(cands, "a") {
		t.Errorf("lookup of the stored fingerprint = %v, want it to contain %q", cands, "a")
	}
	if cands := ix.Lookup(b); slices.Contains(cands, "a") {
		t.Errorf("unrelated text lookup returned a candidate: %v", cands)
	}
}

func TestIndexAddDuplicateSwallowed(t *testing.T) {
	a, err := Fingerprint(boilerplate)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	ix := New()
	if err := ix.Add("a", a); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := ix.Add("a", a); err != nil {
		t.Errorf("duplicate Add = %v, want it swallowed", err)
	}
}

func TestEmptyTextNoFingerprint(t *testing.T) {
	for _, text := range []string{"", "   ", "\n\t "} {
		if _, err := Fingerprint(text); !errors.Is(err, minhash.ErrEmptySet) {
			t.Errorf("Fingerprint(%q) error = %v, want ErrEmptySet", text, err)
		}
	}
	// Short text (fewer tokens than the shingle window) also gets no
	// fingerprint: it can never be a duplicate candidate.
	for _, text := range []string{"one", "two words", "three whole words", "four short words here"} {
		if fp, err := Fingerprint(text); !errors.Is(err, minhash.ErrEmptySet) {
			t.Errorf("Fingerprint(%q) = %+v, %v; want ErrEmptySet", text, fp, err)
		}
	}
	// Exactly a window of tokens is fingerprintable.
	if _, err := Fingerprint("one two three four five"); err != nil {
		t.Errorf("Fingerprint(5 tokens) error = %v, want nil", err)
	}
}

func TestLookupIgnoresMalformedSignature(t *testing.T) {
	ix := New()
	malformed := FP{Sig: []int64{1, 2}, Bands: []int64{3}}
	if cands := ix.Lookup(malformed); cands != nil {
		t.Errorf("Lookup(malformed) = %v, want nil", cands)
	}
	if err := ix.Add("bad", malformed); err == nil {
		t.Error("Add(malformed) = nil; want a length error so callers see format drift")
	}
}

func TestInt64Roundtrip(t *testing.T) {
	in := []uint64{0, 1, math.MaxUint64, 1 << 63, 42}
	if got := toUint64(toInt64(in)); !slices.Equal(got, in) {
		t.Errorf("unsigned roundtrip = %v, want %v", got, in)
	}
	neg := []int64{-1, math.MinInt64, 7}
	if got := toInt64(toUint64(neg)); !slices.Equal(got, neg) {
		t.Errorf("signed roundtrip = %v, want %v", got, neg)
	}
}
