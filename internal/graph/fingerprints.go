package graph

import (
	"context"
	"fmt"

	"github.com/alessiosavi/go-knowledge-graph/internal/dedup"
)

// ChunkFP is one persisted chunk fingerprint.
type ChunkFP struct {
	ChunkID string
	FP      dedup.FP
}

// PutFingerprint stores the chunk's fingerprint fields (exact key,
// signature, band keys, signature version). Returns ErrNotFound when the
// chunk does not exist.
func (s *Store) PutFingerprint(ctx context.Context, chunkID string, fp dedup.FP) error {
	recs, err := s.runRead(ctx, "MATCH (c:Chunk {id: $id}) RETURN 1", map[string]any{"id": chunkID})
	if err != nil {
		return fmt.Errorf("graph: put fingerprint: %w", err)
	}
	if len(recs) == 0 {
		return fmt.Errorf("graph: chunk %s: %w", chunkID, ErrNotFound)
	}
	err = s.runWrite(ctx, `
MATCH (c:Chunk {id: $id})
SET c.exact_key = $exact, c.minhash_sig = $sig, c.band_keys = $bands, c.signature_version = $version`,
		map[string]any{
			"id": chunkID, "exact": fp.ExactKey, "sig": fp.Sig,
			"bands": fp.Bands, "version": fp.Version,
		})
	if err != nil {
		return fmt.Errorf("graph: put fingerprint: %w", err)
	}
	return nil
}

// AllFingerprints returns every chunk carrying a fingerprint; the serve
// start path uses it to re-seed the in-memory LSH index.
func (s *Store) AllFingerprints(ctx context.Context) ([]ChunkFP, error) {
	recs, err := s.runRead(ctx, `
MATCH (c:Chunk)
WHERE c.minhash_sig IS NOT NULL
RETURN c.id AS id, c.exact_key AS exact_key, c.minhash_sig AS sig,
       c.band_keys AS bands, c.signature_version AS version`, nil)
	if err != nil {
		return nil, fmt.Errorf("graph: all fingerprints: %w", err)
	}
	out := make([]ChunkFP, 0, len(recs))
	for _, r := range recs {
		out = append(out, ChunkFP{
			ChunkID: getString(r, "id"),
			FP: dedup.FP{
				ExactKey: getString(r, "exact_key"),
				Sig:      getInt64Slice(r, "sig"),
				Bands:    getInt64Slice(r, "bands"),
				Version:  getInt(r, "version"),
			},
		})
	}
	return out, nil
}
