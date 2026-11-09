package dedup

import (
	"errors"
	"log/slog"

	"github.com/alessiosavi/GoGPUtils/minhash"
)

// Index is the in-memory LSH candidate index over chunk fingerprints. It is
// seeded from persisted fingerprints at serve start and updated on every
// chunk insert.
type Index struct {
	idx *minhash.Index
}

// New builds an index with the same band layout as Fingerprint.
func New() *Index {
	idx, err := minhash.NewIndex(bands, rows)
	if err != nil {
		// Fixed parameters; construction cannot fail.
		panic("dedup: NewIndex: " + err.Error())
	}
	return &Index{idx: idx}
}

// Add indexes one fingerprint under id. Duplicate ids are swallowed; a
// fingerprint without a signature is ignored.
func (ix *Index) Add(id string, fp FP) error {
	if len(fp.Sig) == 0 {
		return nil
	}
	err := ix.idx.Add(id, minhash.Signature(toUint64(fp.Sig)))
	if errors.Is(err, minhash.ErrDuplicateID) {
		slog.Debug("dedup: id already indexed", "id", id)
		return nil
	}
	return err
}

// Lookup returns candidate chunk ids whose band keys collide with the
// fingerprint's. Candidates must be verified with VerifySimilar. A
// malformed signature (wrong length — format drift) is logged and yields no
// candidates rather than a silent error.
func (ix *Index) Lookup(fp FP) []string {
	if len(fp.Sig) == 0 {
		return nil
	}
	if len(fp.Sig) != numPerm {
		slog.Warn("dedup: malformed signature length in lookup", "len", len(fp.Sig), "want", numPerm)
		return nil
	}
	cands, err := ix.idx.Candidates(minhash.Signature(toUint64(fp.Sig)))
	if err != nil {
		slog.Debug("dedup: candidate lookup failed", "err", err)
		return nil
	}
	return cands
}

// VerifySimilar returns the exact Jaccard similarity of two token lists
// over their 5-word shingle sets; 0 when either side cannot be shingled
// (including empty input — empties are never duplicates).
func VerifySimilar(aTokens, bTokens []string) float64 {
	a, err := minhash.Shingles(aTokens, shingleK)
	if err != nil {
		return 0
	}
	b, err := minhash.Shingles(bTokens, shingleK)
	if err != nil {
		return 0
	}
	return minhash.Jaccard(a, b)
}
