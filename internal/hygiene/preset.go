package hygiene

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
	"go.yaml.in/yaml/v4"
)

// Preset is the name policy alongside a label preset. Allow entries are exact;
// Stop entries use case/diacritic folding. Code relaxes path and code rules;
// Values exempts document-scoped labels from the five shape rules.
// SectionLabelKeepTypes exempts lowercased entity types from HeadingLead.
type Preset struct {
	Name                  string            `json:"-"`
	Allow                 map[string]bool   `json:"allowlist"`
	Stop                  map[string]bool   `json:"stoplist_exact"`
	StopPatterns          []*regexp.Regexp  `json:"stoplist_patterns"`
	Values                map[string]bool   `json:"document_scoped_labels"`
	Code                  bool              `json:"code_tokens"`
	SectionLabelKeepTypes map[string]bool   `json:"section_label_keep_types"`
	Validity              *validity.Lexicon `json:"validity,omitempty"`
}

var presetName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// PresetSettings is the hygiene portion of a label preset file.
type PresetSettings struct {
	Code                  bool     `yaml:"code_tokens"`
	Allow                 []string `yaml:"allowlist"`
	SectionLabelKeepTypes []string `yaml:"section_label_keep_types,omitempty"`
	Stop                  struct {
		Exact    []string `yaml:"exact"`
		Patterns []string `yaml:"patterns"`
	} `yaml:"stoplist"`
	// Keep the node to distinguish an omitted block from an explicit null
	// and apply strict validity decoding even in the hygiene-only loader.
	Validity yaml.Node `yaml:"validity,omitempty"`
}

// LoadPreset reads hygiene settings from the label YAML. An empty name returns
// built-in defaults without reading YAML. overlayPath adds literal stop words (blank
// lines and lines beginning with # are ignored).
func LoadPreset(dir, name, overlayPath string) (Preset, error) {
	var raw struct {
		Entities       []extract.Label `yaml:"entities"`
		PresetSettings `yaml:",inline"`
	}
	if name != "" {
		if !presetName.MatchString(name) {
			return Preset{}, fmt.Errorf("hygiene preset %q: invalid name", name)
		}
		data, err := os.ReadFile(filepath.Join(dir, name+".yaml"))
		if err != nil {
			return Preset{}, fmt.Errorf("hygiene preset %q: %w", name, err)
		}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return Preset{}, fmt.Errorf("hygiene preset %q: %w", name, err)
		}
	}
	return BuildPreset(name, raw.Entities, raw.PresetSettings, overlayPath)
}

// BuildPreset applies the hygiene settings from a decoded label preset.
// An empty name selects the built-in policy.
func BuildPreset(name string, entities []extract.Label, settings PresetSettings, overlayPath string) (Preset, error) {
	return buildPreset(name, entities, settings, overlayPath, nil)
}

// BuildPresetWithStoplistHash reports the SHA-256 of the exact overlay bytes
// used by the policy, or the empty byte stream when no overlay is configured.
func BuildPresetWithStoplistHash(name string, entities []extract.Label, settings PresetSettings, overlayPath string) (Preset, string, error) {
	digest := sha256.New()
	p, err := buildPreset(name, entities, settings, overlayPath, digest)
	return p, fmt.Sprintf("%x", digest.Sum(nil)), err
}

func buildPreset(name string, entities []extract.Label, settings PresetSettings, overlayPath string, overlayDigest io.Writer) (Preset, error) {
	p := Preset{Name: name, Allow: make(map[string]bool), Stop: make(map[string]bool), Values: make(map[string]bool)}
	keepTypes := settings.SectionLabelKeepTypes
	if keepTypes == nil {
		keepTypes = []string{"location"}
	}
	p.SectionLabelKeepTypes = make(map[string]bool, len(keepTypes))
	for _, kind := range keepTypes {
		p.SectionLabelKeepTypes[strings.ToLower(kind)] = true
	}
	if name == "" {
		for _, word := range []string{"AI", "EU", "UK", "US", "UE", "IT"} {
			p.Allow[word] = true
		}
		for _, word := range []string{"Descrizione", "Informazioni", "Indice", "Introduzione", "Description", "Information", "Index", "Introduction"} {
			p.Stop[fold(word)] = true
		}
		p.StopPatterns = []*regexp.Regexp{regexp.MustCompile(`(?i)^Sezione \d+$`), regexp.MustCompile(`(?i)^Section \d+$`)}
		// The built-in entity labels all have global scope, so Values is empty.
	} else {
		lexicon, err := validity.DecodeLexiconYAML(&settings.Validity)
		if err != nil {
			return Preset{}, fmt.Errorf("hygiene preset %q: %w", name, err)
		}
		p.Validity = lexicon
		p.Code = settings.Code
		for _, label := range entities {
			if label.Scope == "document" {
				p.Values[label.Name] = true
			}
		}
		for _, word := range settings.Allow {
			p.Allow[strings.TrimSpace(word)] = true
		}
		for _, word := range settings.Stop.Exact {
			p.Stop[fold(word)] = true
		}
		for _, pattern := range settings.Stop.Patterns {
			re, err := regexp.Compile("(?i)" + pattern)
			if err != nil {
				return Preset{}, fmt.Errorf("hygiene preset %q pattern %q: %w", name, pattern, err)
			}
			p.StopPatterns = append(p.StopPatterns, re)
		}
	}
	if overlayPath != "" {
		f, err := os.Open(overlayPath)
		if err != nil {
			return Preset{}, fmt.Errorf("hygiene overlay: %w", err)
		}
		defer f.Close()
		var overlay io.Reader = f
		if overlayDigest != nil {
			overlay = io.TeeReader(f, overlayDigest)
		}
		scanner := bufio.NewScanner(overlay)
		for scanner.Scan() {
			word := strings.TrimSpace(scanner.Text())
			if word != "" && !strings.HasPrefix(word, "#") {
				p.Stop[fold(word)] = true
			}
		}
		if err := scanner.Err(); err != nil {
			return Preset{}, fmt.Errorf("hygiene overlay: %w", err)
		}
	}
	return p, nil
}
