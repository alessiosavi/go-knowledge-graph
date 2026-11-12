package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"go.yaml.in/yaml/v4"
)

// presetNameRe restricts label preset names (they reach the filesystem).
var presetNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// labelNameRe requires alphanumeric content in every label name.
var labelNameRe = regexp.MustCompile(`[a-zA-Z0-9]`)

// LoadPreset reads a named YAML preset from dir, validating label names, scopes
// and unambiguous sidecar answers within each kind. An empty name returns copies
// of the built-in defaults without reading the filesystem.
func LoadPreset(dir, name string) (entities, relations []extract.Label, err error) {
	if name == "" {
		entities, relations = DefaultLabels()
		return entities, relations, nil
	}
	if !presetNameRe.MatchString(name) {
		return nil, nil, fmt.Errorf("label preset %q: invalid name", name)
	}
	path := filepath.Join(dir, name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("label preset %q: %w", name, err)
	}
	var preset struct {
		Entities  []extract.Label `yaml:"entities"`
		Relations []extract.Label `yaml:"relations"`
	}
	if err := yaml.Unmarshal(data, &preset); err != nil {
		return nil, nil, fmt.Errorf("label preset %q: %w", name, err)
	}
	for _, l := range append(append([]extract.Label{}, preset.Entities...), preset.Relations...) {
		if !labelNameRe.MatchString(l.Name) {
			return nil, nil, fmt.Errorf("label preset %q: label %q has no alphanumeric content", name, l.Name)
		}
		if l.Scope != "" && l.Scope != "document" {
			return nil, nil, fmt.Errorf("label preset %q: label %q has unknown scope %q", name, l.Name, l.Scope)
		}
	}
	// Scope decides entity identity, so it is meaningful on entity labels
	// only; on a relation it would silently scope nothing.
	for _, l := range preset.Relations {
		if l.Scope != "" {
			return nil, nil, fmt.Errorf("label preset %q: relation label %q cannot have a scope (scope applies to entity labels)", name, l.Name)
		}
	}
	for _, labels := range [][]extract.Label{preset.Entities, preset.Relations} {
		keys := make(map[string]string, len(labels)*2)
		for _, l := range labels {
			labelKeys := []string{strings.ToLower(strings.TrimSpace(l.Name))}
			if l.Text != "" {
				labelKeys = append(labelKeys, strings.ToLower(strings.TrimSpace(l.Text)))
			}
			// Check both aliases before adding either, so a label may use its
			// own name as text without appearing ambiguous with itself.
			for _, key := range labelKeys {
				if other, ok := keys[key]; ok {
					return nil, nil, fmt.Errorf("label preset %q: labels %q and %q have ambiguous mapping key %q", name, other, l.Name, key)
				}
			}
			for _, key := range labelKeys {
				keys[key] = l.Name
			}
		}
	}
	return preset.Entities, preset.Relations, nil
}
