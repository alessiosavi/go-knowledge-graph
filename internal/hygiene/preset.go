package hygiene

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"go.yaml.in/yaml/v4"
)

// Preset is the name policy alongside a label preset. Allow entries are exact;
// Stop entries use case/diacritic folding. Code relaxes path and code rules;
// Values exempts document-scoped labels from the five shape rules.
type Preset struct {
	Name         string           `json:"-"`
	Allow        map[string]bool  `json:"allowlist"`
	Stop         map[string]bool  `json:"stoplist_exact"`
	StopPatterns []*regexp.Regexp `json:"stoplist_patterns"`
	Values       map[string]bool  `json:"document_scoped_labels"`
	Code         bool             `json:"code_tokens"`
}

var presetName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// PresetSettings is the hygiene portion of a label preset file.
type PresetSettings struct {
	Code  bool     `yaml:"code_tokens"`
	Allow []string `yaml:"allowlist"`
	Stop  struct {
		Exact    []string `yaml:"exact"`
		Patterns []string `yaml:"patterns"`
	} `yaml:"stoplist"`
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
	p := Preset{Name: name, Allow: make(map[string]bool), Stop: make(map[string]bool), Values: make(map[string]bool)}
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
		scanner := bufio.NewScanner(f)
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
