package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
)

// writeArtifact hashes the exact JSONL bytes before the trailer. A hard
// failure removes the incomplete artifact so the same path can be retried.
func writeArtifact(path string, rows func(*json.Encoder) (any, error)) error {
	return writeArtifacts([]string{path}, func(encoders []*json.Encoder) ([]any, error) {
		counts, err := rows(encoders[0])
		return []any{counts}, err
	})
}

// writeArtifacts reserves every path before writing any rows and removes all
// files it created if any write or close fails.
func writeArtifacts(paths []string, rows func([]*json.Encoder) ([]any, error)) (err error) {
	type artifact struct {
		file   *os.File
		digest hash.Hash
	}
	files := make([]artifact, 0, len(paths))
	defer func() {
		for _, a := range files {
			if closeErr := a.file.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			for _, a := range files {
				_ = os.Remove(a.file.Name())
			}
		}
	}()
	encoders := make([]*json.Encoder, 0, len(paths))
	for _, path := range paths {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		digest := sha256.New()
		files = append(files, artifact{file: f, digest: digest})
		encoders = append(encoders, json.NewEncoder(io.MultiWriter(f, digest)))
	}
	counts, err := rows(encoders)
	if err != nil {
		return err
	}
	for i, a := range files {
		if err := json.NewEncoder(a.file).Encode(artifactTrailer{
			Type: "trailer", SHA256: hex.EncodeToString(a.digest.Sum(nil)), Counts: counts[i],
		}); err != nil {
			return err
		}
	}
	return nil
}

type artifactTrailer struct {
	Type   string `json:"type"`
	SHA256 string `json:"sha256"`
	Counts any    `json:"counts"`
}

func sha256Hex(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
