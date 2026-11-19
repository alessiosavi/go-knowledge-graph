package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// writeArtifact hashes the exact JSONL bytes before the trailer. A hard
// failure removes the incomplete artifact so the same path can be retried.
func writeArtifact(path string, rows func(*json.Encoder) (any, error)) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	digest := sha256.New()
	counts, err := rows(json.NewEncoder(io.MultiWriter(f, digest)))
	if err != nil {
		return err
	}
	return json.NewEncoder(f).Encode(artifactTrailer{
		Type: "trailer", SHA256: hex.EncodeToString(digest.Sum(nil)), Counts: counts,
	})
}

type artifactTrailer struct {
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
	Counts any    `json:"counts"`
}

func sha256Hex(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
