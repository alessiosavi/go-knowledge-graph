package graph_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/GoGPUtils/textnorm"

	"github.com/alessiosavi/go-knowledge-graph/internal/dedup"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

const fpBoilerplate = "The quarterly operations report describes routine maintenance procedures for the warehouse team. " +
	"Every operator must log inbound shipments before noon, verify pallet labels against the manifest, " +
	"and file damaged goods in the quarantine area for afternoon inspection. " +
	"Safety drills run weekly and attendance is recorded by the shift supervisor in the central operations log."

// TestIndexReseedFromStore pins the serve-start path: fingerprints persisted
// on chunks are re-loaded and re-seeded into the LSH index, and a near
// duplicate of a stored chunk finds it as a candidate.
func TestIndexReseedFromStore(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	docID := testUpsert(t, st, "file", uniqueSource(t), "hash-a").ID

	variant := strings.Replace(fpBoilerplate, "quarterly", "annual", 1)
	unrelated := "Quantum chromodynamics couples gluons to colour charge through the strong interaction Lagrangian density."
	texts := []string{fpBoilerplate, variant, unrelated, unrelated}
	chunks := make([]graph.Chunk, 0, len(texts))
	for i, text := range texts {
		chunks = append(chunks, graph.Chunk{Seq: i, Text: text, Start: 0, End: len(text)})
	}
	if err := st.PutChunks(ctx, docID, chunks); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}

	fps := make([]dedup.FP, len(chunks))
	for i, c := range chunks {
		fp, err := dedup.Fingerprint(c.Text)
		if err != nil {
			t.Fatalf("Fingerprint chunk %d: %v", i, err)
		}
		fps[i] = fp
		if err := st.PutFingerprint(ctx, fmt.Sprintf("%s-c%d", docID, c.Seq), fp); err != nil {
			t.Fatalf("PutFingerprint chunk %d: %v", i, err)
		}
	}

	// A synthetic fingerprint with extreme int64 values must round-trip
	// through Memgraph bit-exactly.
	syn := dedup.FP{
		ExactKey: "synthetic",
		Sig:      []int64{-1, math.MinInt64, 5},
		Bands:    []int64{-9, 7},
		Version:  1,
	}
	synChunk := graph.Chunk{Seq: len(chunks), Text: "synthetic placeholder text for storage", Start: 0, End: 39}
	if err := st.PutChunks(ctx, docID, []graph.Chunk{synChunk}); err != nil {
		t.Fatalf("PutChunks synthetic: %v", err)
	}
	if err := st.PutFingerprint(ctx, fmt.Sprintf("%s-c%d", docID, synChunk.Seq), syn); err != nil {
		t.Fatalf("PutFingerprint synthetic: %v", err)
	}

	all, err := st.AllFingerprints(ctx)
	if err != nil {
		t.Fatalf("AllFingerprints: %v", err)
	}
	byID := make(map[string]dedup.FP, len(all))
	for _, cfp := range all {
		byID[cfp.ChunkID] = cfp.FP
	}
	got, ok := byID[fmt.Sprintf("%s-c%d", docID, synChunk.Seq)]
	if !ok {
		t.Fatalf("synthetic fingerprint not loaded back from the store")
	}
	if !slices.Equal(got.Sig, syn.Sig) || !slices.Equal(got.Bands, syn.Bands) || got.ExactKey != syn.ExactKey {
		t.Errorf("synthetic roundtrip = %+v, want %+v", got, syn)
	}

	// Re-seed the index from the store (serve-start path). Some older test
	// rows carry malformed signature lengths; production fingerprints are
	// always 64 long (Fingerprint).
	ix := dedup.New()
	for _, cfp := range all {
		if len(cfp.FP.Sig) != 64 {
			continue
		}
		if err := ix.Add(cfp.ChunkID, cfp.FP); err != nil {
			t.Fatalf("reseed Add(%s): %v", cfp.ChunkID, err)
		}
	}
	cands := ix.Lookup(fps[0])
	for _, want := range []string{docID + "-c0", docID + "-c1"} {
		if !slices.Contains(cands, want) {
			t.Errorf("near-duplicate lookup = %v, want it to contain %q", cands, want)
		}
	}
	if slices.Contains(cands, docID+"-c2") {
		t.Errorf("unrelated chunk surfaced as a candidate: %v", cands)
	}

	// Sanity: the near-duplicate texts are within the near threshold.
	if j := dedup.VerifySimilar(testTokens(t, fpBoilerplate), testTokens(t, variant)); j < 0.85 {
		t.Errorf("Jaccard = %v, want >= 0.85", j)
	}

	// Same candidates after a full reload: fetch again and re-seed.
	all2, err := st.AllFingerprints(ctx)
	if err != nil {
		t.Fatalf("AllFingerprints (reload): %v", err)
	}
	ix2 := dedup.New()
	for _, cfp := range all2 {
		if len(cfp.FP.Sig) != 64 {
			continue
		}
		if err := ix2.Add(cfp.ChunkID, cfp.FP); err != nil {
			t.Fatalf("reseed2 Add(%s): %v", cfp.ChunkID, err)
		}
	}
	if !slices.Contains(ix2.Lookup(fps[0]), docID+"-c1") {
		t.Error("reloaded index does not find the near duplicate")
	}
}

func TestPutAndAllFingerprintsMissingChunk(t *testing.T) {
	st := graphtest.Store(t)
	err := st.PutFingerprint(context.Background(), "no-such-chunk", dedup.FP{ExactKey: "x"})
	if !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("PutFingerprint(missing chunk) error = %v, want ErrNotFound", err)
	}
}

func testTokens(t *testing.T, text string) []string {
	t.Helper()
	tokens, err := textnorm.MeaningPreset().SplitTokens().Run(text)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	return tokens
}
