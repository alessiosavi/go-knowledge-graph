package graph

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
)

// FoldExceptions is a set of exact entity keys excluded from fold matching.
// Its zero value is an empty set.
type FoldExceptions struct {
	keys map[string]struct{}
}

// ParseFoldExceptions reads one fully qualified entity key per line. It strips
// an initial UTF-8 BOM and trims each line before skipping blanks and // comments.
// A scoped surface must be nonempty and free of surrounding whitespace.
func ParseFoldExceptions(r io.Reader) (FoldExceptions, error) {
	reader := bufio.NewReader(r)
	keys := make(map[string]struct{})
	for line := 1; ; line++ {
		raw, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return FoldExceptions{}, fmt.Errorf("fold exceptions line %d: read: %w", line, err)
		}
		if line == 1 {
			raw = strings.TrimPrefix(raw, "\ufeff")
		}
		key := strings.TrimSpace(raw)
		if key != "" && !strings.HasPrefix(key, "//") {
			if err := validateFoldException(key); err != nil {
				return FoldExceptions{}, fmt.Errorf("fold exceptions line %d: %w", line, err)
			}
			if _, exists := keys[key]; exists {
				return FoldExceptions{}, fmt.Errorf("fold exceptions line %d: duplicate key %q", line, key)
			}
			keys[key] = struct{}{}
		}
		if err == io.EOF {
			return FoldExceptions{keys: keys}, nil
		}
	}
}

func validateFoldException(key string) error {
	if key[0] != '@' && key[0] != '#' {
		return nil
	}
	scope, surface, ok := strings.Cut(key[1:], ":")
	if !ok {
		return fmt.Errorf("missing ':' after scope")
	}
	trimmedSurface := strings.TrimSpace(surface)
	if trimmedSurface == "" {
		return fmt.Errorf("empty surface")
	}
	if trimmedSurface != surface {
		return fmt.Errorf("surface has surrounding whitespace")
	}
	if key[0] == '@' && !domain.ValidDomain(scope) {
		return fmt.Errorf("invalid domain %q", scope)
	}
	if key[0] == '#' && strings.TrimSpace(scope) == "" {
		return fmt.Errorf("empty document ID")
	}
	return nil
}

// Contains reports whether key occurs verbatim in the exception set.
func (e FoldExceptions) Contains(key string) bool {
	_, ok := e.keys[key]
	return ok
}

// Keys returns the keys in ascending lexicographic order.
func (e FoldExceptions) Keys() []string {
	return slices.Sorted(maps.Keys(e.keys))
}

// Digest returns the hex-encoded SHA-256 of the sorted keys joined by newlines.
func (e FoldExceptions) Digest() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(e.Keys(), "\n"))))
}
