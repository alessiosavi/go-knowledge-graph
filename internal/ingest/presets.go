package ingest

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/hygiene"
	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
	"go.yaml.in/yaml/v4"
)

// presetNameRe restricts label preset names (they reach the filesystem).
var presetNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// labelNameRe requires alphanumeric content in every label name.
var labelNameRe = regexp.MustCompile(`[a-zA-Z0-9]`)

type presetLabels struct {
	Entities  []extract.Label `yaml:"entities"`
	Relations []extract.Label `yaml:"relations"`
}

type presetFile struct {
	presetLabels           `yaml:",inline"`
	hygiene.PresetSettings `yaml:",inline"`
}

// LoadPreset reads a named YAML preset from dir, validating label names, scopes
// and unambiguous sidecar answers within each kind. An empty name returns copies
// of the built-in defaults without reading the filesystem.
func LoadPreset(dir, name string) (entities, relations []extract.Label, err error) {
	if name == "" {
		entities, relations = DefaultLabels()
		return entities, relations, nil
	}
	if !presetNameRe.MatchString(name) {
		return nil, nil, argerr.Errorf("label preset %q: invalid name", name)
	}
	path := filepath.Join(dir, name+".yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil, argerr.Errorf("label preset %q: %w", name, err)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("label preset %q: %w", name, err)
	}
	var preset struct {
		presetLabels `yaml:",inline"`
		Validity     yaml.Node `yaml:"validity,omitempty"`
	}
	if err := yaml.Unmarshal(data, &preset); err != nil {
		return nil, nil, fmt.Errorf("label preset %q: %w", name, err)
	}
	if _, err := validity.DecodeLexiconYAML(&preset.Validity); err != nil {
		return nil, nil, fmt.Errorf("label preset %q: %w", name, err)
	}
	// The validity block is validated strictly and errors are fatal. Other
	// unknown preset keys are logged as warnings.
	var fieldErrors *yaml.LoadErrors
	if errors.As(decodePresetFile(data, &presetFile{}), &fieldErrors) {
		for _, field := range fieldErrors.Errors {
			if strings.HasPrefix(field.Message, "field ") && strings.Contains(field.Message, " not found in type ") {
				slog.Warn("unknown preset key", "preset", name, "detail", field.Error())
			}
		}
	}
	if err := preset.validate(name); err != nil {
		return nil, nil, err
	}
	return preset.Entities, preset.Relations, nil
}

func (preset presetLabels) validate(name string) error {
	for _, l := range append(append([]extract.Label{}, preset.Entities...), preset.Relations...) {
		if !labelNameRe.MatchString(l.Name) {
			return fmt.Errorf("label preset %q: label %q has no alphanumeric content", name, l.Name)
		}
		if l.Scope != "" && l.Scope != "document" {
			return fmt.Errorf("label preset %q: label %q has unknown scope %q", name, l.Name, l.Scope)
		}
	}
	// Scope decides entity identity, so it is meaningful on entity labels
	// only; on a relation it would silently scope nothing.
	for _, l := range preset.Relations {
		if l.Scope != "" {
			return fmt.Errorf("label preset %q: relation label %q cannot have a scope (scope applies to entity labels)", name, l.Name)
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
					return fmt.Errorf("label preset %q: labels %q and %q have ambiguous mapping key %q", name, other, l.Name, key)
				}
			}
			for _, key := range labelKeys {
				keys[key] = l.Name
			}
		}
	}
	return nil
}

func decodePresetFile(data []byte, preset *presetFile) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(preset); err != nil {
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("expected a single YAML document")
	}
	return nil
}

// CheckedPreset holds the same label and hygiene settings used by ingestion,
// with a fingerprint of the YAML from which they were validated.
type CheckedPreset struct {
	Entities  []extract.Label
	Relations []extract.Label
	Hygiene   hygiene.Preset
	SHA256    string
}

// LoadCheckedPreset validates a named preset, or the built-in preset if name
// is empty. The stoplist overlay is applied to the effective hygiene policy.
func LoadCheckedPreset(dir, name, overlayPath string) (CheckedPreset, error) {
	if name != "" {
		if !presetNameRe.MatchString(name) {
			return CheckedPreset{}, argerr.Errorf("label preset %q: invalid name", name)
		}
		return checkPresetFile(filepath.Join(dir, name+".yaml"), name, overlayPath)
	}
	entities, relations := DefaultLabels()
	raw := presetFile{presetLabels: presetLabels{Entities: entities, Relations: relations}}
	data, err := yaml.Marshal(raw)
	if err != nil {
		return CheckedPreset{}, err
	}
	return raw.checked(name, overlayPath, data)
}

// CheckPresetFile validates a label and hygiene preset with an optional
// stoplist overlay.
func CheckPresetFile(path, overlayPath string) error {
	name := strings.TrimSuffix(filepath.Base(path), ".yaml")
	_, err := checkPresetFile(path, name, overlayPath)
	return err
}

func checkPresetFile(path, name, overlayPath string) (CheckedPreset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CheckedPreset{}, err
	}
	var raw presetFile
	if err := decodePresetFile(data, &raw); err != nil {
		return CheckedPreset{}, fmt.Errorf("label preset %q: %w", name, err)
	}
	return raw.checked(name, overlayPath, data)
}

func (raw presetFile) checked(name, overlayPath string, data []byte) (CheckedPreset, error) {
	if err := raw.validate(name); err != nil {
		return CheckedPreset{}, err
	}
	for _, kind := range []struct {
		name   string
		labels []extract.Label
		limit  int
	}{
		{"entity", raw.Entities, 30},
		{"relation", raw.Relations, 15},
	} {
		if len(kind.labels) > kind.limit {
			return CheckedPreset{}, fmt.Errorf("label preset %q: at most %d %s labels allowed", name, kind.limit, kind.name)
		}
		for _, l := range kind.labels {
			if l.Threshold <= 0 || l.Threshold > 1 || math.IsNaN(l.Threshold) {
				return CheckedPreset{}, fmt.Errorf("label preset %q: label %q threshold must be finite and in (0, 1]", name, l.Name)
			}
			if utf8.RuneCountInString(l.SidecarText()) > 60 {
				return CheckedPreset{}, fmt.Errorf("label preset %q: label %q model text must be at most 60 characters", name, l.Name)
			}
		}
	}
	sanitized := make(map[string]string, len(raw.Relations))
	for _, l := range raw.Relations {
		key := graph.SanitizeRel(l.Name)
		if other, ok := sanitized[key]; ok {
			return CheckedPreset{}, fmt.Errorf("label preset %q: relations %q and %q both sanitize to %q", name, other, l.Name, key)
		}
		sanitized[key] = l.Name
	}
	entityNames := make(map[string]bool, len(raw.Entities))
	for _, l := range raw.Entities {
		entityNames[strings.ToLower(l.Name)] = true
	}
	for _, keepType := range raw.SectionLabelKeepTypes {
		if !entityNames[strings.ToLower(keepType)] {
			return CheckedPreset{}, fmt.Errorf("label preset %q: unknown entity label name %q in section_label_keep_types", name, keepType)
		}
	}
	policy, err := hygiene.BuildPreset(name, raw.Entities, raw.PresetSettings, overlayPath)
	if err != nil {
		return CheckedPreset{}, err
	}
	return CheckedPreset{
		Entities: raw.Entities, Relations: raw.Relations, Hygiene: policy,
		SHA256: fmt.Sprintf("%x", sha256.Sum256(data)),
	}, nil
}
