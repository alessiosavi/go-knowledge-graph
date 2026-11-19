package eval

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteArtifactsRollsBackWhenSecondTrailerFails(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "predictions.jsonl"), filepath.Join(dir, "mentions.jsonl")}
	err := writeArtifacts(paths, func(encoders []*json.Encoder) ([]any, error) {
		for _, enc := range encoders {
			if err := enc.Encode(map[string]string{"type": "header"}); err != nil {
				return nil, err
			}
		}
		// The first trailer can be written, while encoding the second fails.
		return []any{0, make(chan int)}, nil
	})
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Fatalf("writeArtifacts error = %v, want unsupported JSON type", err)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("artifact %s was not removed, stat error = %v", p, err)
		}
	}
}
