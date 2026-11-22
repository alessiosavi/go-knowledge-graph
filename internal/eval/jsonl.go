package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
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
	files := make([]*os.File, 0, len(paths))
	defer func() {
		for _, f := range files {
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			for _, f := range files {
				_ = os.Remove(f.Name())
			}
		}
	}()
	for _, path := range paths {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		files = append(files, f)
	}
	return writeArtifactRows(files, rows)
}

// writeStagedArtifacts writes all rows and closes all temporary files before
// publishing. Exclusive reservations preserve the no-overwrite contract even
// if another invocation creates a destination while extraction is running.
func writeStagedArtifacts(paths []string, rows func([]*json.Encoder) ([]any, error)) (err error) {
	files := make([]*os.File, 0, len(paths))
	reserved := make([]string, 0, len(paths))
	defer func() {
		for _, f := range files {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
		if err != nil {
			for _, path := range reserved {
				_ = os.Remove(path)
			}
		}
	}()
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if seen[absolute] {
			return argerr.Errorf("replay output paths must differ: %s", path)
		}
		seen[absolute] = true
		if _, err := os.Lstat(path); err == nil {
			return &os.PathError{Op: "create", Path: path, Err: os.ErrExist}
		} else if !os.IsNotExist(err) {
			return err
		}
		f, err := os.CreateTemp(filepath.Dir(path), ".gkg-eval-*")
		if err != nil {
			return err
		}
		files = append(files, f)
	}
	if err := writeArtifactRows(files, rows); err != nil {
		return err
	}
	for _, f := range files {
		if err := f.Close(); err != nil {
			return err
		}
	}
	for _, path := range paths {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		reserved = append(reserved, path)
		if err := f.Close(); err != nil {
			return err
		}
	}
	for i, f := range files {
		if err := os.Rename(f.Name(), paths[i]); err != nil {
			return err
		}
	}
	return nil
}

func writeArtifactRows(files []*os.File, rows func([]*json.Encoder) ([]any, error)) error {
	digests := make([]hash.Hash, len(files))
	encoders := make([]*json.Encoder, len(files))
	for i, f := range files {
		digests[i] = sha256.New()
		encoders[i] = json.NewEncoder(io.MultiWriter(f, digests[i]))
	}
	counts, err := rows(encoders)
	if err != nil {
		return err
	}
	for i, f := range files {
		if counts[i] == nil {
			continue
		}
		if err := json.NewEncoder(f).Encode(artifactTrailer{
			Type: "trailer", SHA256: hex.EncodeToString(digests[i].Sum(nil)), Counts: counts[i],
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
