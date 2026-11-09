// Package dedup computes deterministic chunk fingerprints (normalized
// tokens → 5-word shingles → MinHash signature → LSH band keys, via
// GoGPUtils) and keeps the in-memory candidate index. The package is pure:
// persistence lives in internal/graph.
package dedup

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/alessiosavi/GoGPUtils/minhash"
	"github.com/alessiosavi/GoGPUtils/textnorm"
)

// Fingerprint configuration; the band layout must match New.
const (
	shingleK = 5
	numPerm  = 64
	seed     = 42
	bands    = 16
	rows     = numPerm / bands
)

// SignatureLength is the MinHash signature length in entries (numPerm);
// persisted signatures of another length are stale format and must be
// skipped when re-seeding the index.
const SignatureLength = numPerm

// FP is a chunk fingerprint ready for storage: signature and band keys as
// signed int64 (bit-preserving reinterpretation of the library's uint64,
// per its persistence notes), plus an exact key over the normalized tokens.
type FP struct {
	ExactKey string
	Sig      []int64
	Bands    []int64
	Version  int
}

// Fingerprint computes the fingerprint of one chunk text. Text with fewer
// than shingleK tokens (including empty text) returns minhash.ErrEmptySet —
// such a chunk gets no fingerprint and can never be a duplicate candidate.
func Fingerprint(text string) (FP, error) {
	tokens, err := textnorm.MeaningPreset().SplitTokens().Run(text)
	if err != nil {
		return FP{}, err
	}
	if len(tokens) < shingleK {
		return FP{}, minhash.ErrEmptySet
	}
	shingles, err := minhash.Shingles(tokens, shingleK)
	if err != nil {
		return FP{}, err
	}
	h, err := minhash.New(numPerm, seed)
	if err != nil {
		return FP{}, err
	}
	sig, err := h.Sign(shingles)
	if err != nil {
		return FP{}, err // minhash.ErrEmptySet on empty token sets
	}
	keys, err := minhash.BandKeys(sig, bands)
	if err != nil {
		return FP{}, err
	}
	return FP{
		ExactKey: exactKey(tokens),
		Sig:      toInt64(sig),
		Bands:    toInt64(keys),
		Version:  minhash.SignatureVersion,
	}, nil
}

// exactKey identifies the normalized token sequence exactly.
func exactKey(tokens []string) string {
	sum := sha256.Sum256([]byte(strings.Join(tokens, "\x00")))
	return hex.EncodeToString(sum[:])[:32]
}

// Tokens normalizes text with the same pipeline Fingerprint uses, so exact
// Jaccard verification compares like with like.
func Tokens(text string) ([]string, error) {
	return textnorm.MeaningPreset().SplitTokens().Run(text)
}

// Shingles hashes consecutive token windows with Fingerprint's parameters.
func Shingles(tokens []string) ([]uint64, error) {
	return minhash.Shingles(tokens, shingleK)
}

// SimilarShingles returns the exact Jaccard similarity of two shingle sets
// (as returned by Shingles).
func SimilarShingles(a, b []uint64) float64 {
	return minhash.Jaccard(a, b)
}

// toInt64 reinterprets unsigned signature values as signed int64 without
// changing bits; toUint64 restores them.
func toInt64(in []uint64) []int64 {
	out := make([]int64, len(in))
	for i, v := range in {
		out[i] = int64(v)
	}
	return out
}

func toUint64(in []int64) []uint64 {
	out := make([]uint64, len(in))
	for i, v := range in {
		out[i] = uint64(v)
	}
	return out
}
