package graphtest

// TestVectorFixtureGeometry pins the properties every vector-consuming test
// relies on (the 2026-11-10 CI-flake investigation): determinism; the self-match
// as the unique global nearest neighbour; and — the point of the dense
// component — no exact duplicate pairwise distances, because an exact-tie
// distance plateau makes Memgraph's approximate vector-search walk
// navigation-degenerate and lets it deterministically miss specific freshly
// inserted nodes at small k (internal/ingest TestEmbeddingSetsEntityVectors).
//
// The cosine formula is pinned here on purpose: the natural
// dot/(sqrt(na)*sqrt(nb)) formulation gives self-similarity values of 1±2e-16
// for part of the seed space, so an exact == 1 assertion would be fragile.

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

// cosine returns the cosine similarity of a and b; the formula is part of the
// pin (see the package comment above).
func cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func TestVectorFixtureGeometry(t *testing.T) {
	const dim = 768
	const seeds = 40

	vecs := make([][]float64, seeds)
	for i := range vecs {
		vecs[i] = Vector(fmt.Sprintf("seed-%d", i), dim)
		if len(vecs[i]) != dim {
			t.Fatalf("Vector length = %d, want %d", len(vecs[i]), dim)
		}
	}

	// (d) Determinism: same seed, same vector.
	if !reflect.DeepEqual(Vector("seed-0", dim), Vector("seed-0", dim)) {
		t.Error("Vector is not deterministic for the same seed")
	}

	// (a) No exact duplicate pairwise distances (the tie-plateau pathology).
	type pair struct{ i, j int }
	seen := make(map[float64]pair, seeds*(seeds-1)/2)
	dups := 0
	var firstDup string
	for i := 0; i < seeds; i++ {
		for j := i + 1; j < seeds; j++ {
			d := 1 - cosine(vecs[i], vecs[j])
			if prev, ok := seen[d]; ok {
				dups++
				if firstDup == "" {
					firstDup = fmt.Sprintf("distance %.17g shared by seed-%d/seed-%d and seed-%d/seed-%d",
						d, prev.i, prev.j, i, j)
				}
				continue
			}
			seen[d] = pair{i, j}
		}
	}
	if dups > 0 {
		t.Errorf("exact duplicate pairwise distances: %d duplicates among %d pairs (e.g. %s)",
			dups, seeds*(seeds-1)/2, firstDup)
	}

	// (b) The self-match is the unique global nearest: self-similarity within
	// 1e-12 of 1 and strictly greater than every non-self similarity.
	for i := 0; i < seeds; i++ {
		self := cosine(vecs[i], vecs[i])
		if math.Abs(self-1) > 1e-12 {
			t.Errorf("self-similarity for seed-%d = %.17g, want 1±1e-12", i, self)
		}
		maxOther := math.Inf(-1)
		for j := 0; j < seeds; j++ {
			if j == i {
				continue
			}
			if s := cosine(vecs[i], vecs[j]); s > maxOther {
				maxOther = s
			}
		}
		if self <= maxOther {
			t.Errorf("seed-%d: self-similarity %.17g not strictly greater than best non-self %.17g", i, self, maxOther)
		}
	}

	// (c) Non-self similarities stay well below the self-match.
	maxPair := math.Inf(-1)
	for i := 0; i < seeds; i++ {
		for j := i + 1; j < seeds; j++ {
			if s := cosine(vecs[i], vecs[j]); s > maxPair {
				maxPair = s
			}
		}
	}
	if maxPair >= 0.99 {
		t.Errorf("max non-self similarity = %.17g, want < 0.99", maxPair)
	}
}
