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
	Name         string
	Allow        map[string]bool
	Stop         map[string]bool
	StopPatterns []*regexp.Regexp
	Values       map[string]bool
	Code         bool
}

var presetName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// LoadPreset reads hygiene settings from the label YAML. An empty name returns
// built-in defaults without reading YAML. overlayPath adds literal stop words (blank
// lines and lines beginning with # are ignored).
func LoadPreset(dir, name, overlayPath string) (Preset, error) {
	p := Preset{Name: name, Allow: make(map[string]bool), Stop: make(map[string]bool), Values: make(map[string]bool)}
	if name == "" {
		for _, word := range []string{"AI", "EU", "UK", "US", "UE", "IT"} {
			p.Allow[word] = true
		}
		for _, word := range []string{"Descrizione", "Informazioni", "Indice", "Introduzione"} {
			p.Stop[fold(word)] = true
		}
		p.StopPatterns = []*regexp.Regexp{regexp.MustCompile(`(?i)^Sezione \d+$`), regexp.MustCompile(`(?i)^Section \d+$`)}
		// The built-in entity labels all have global scope, so Values is empty.
	} else {
		if !presetName.MatchString(name) {
			return Preset{}, fmt.Errorf("hygiene preset %q: invalid name", name)
		}
		data, err := os.ReadFile(filepath.Join(dir, name+".yaml"))
		if err != nil {
			return Preset{}, fmt.Errorf("hygiene preset %q: %w", name, err)
		}
		var raw struct {
			Entities []extract.Label `yaml:"entities"`
			Code     bool            `yaml:"code_tokens"`
			Allow    []string        `yaml:"allowlist"`
			Stop     struct {
				Exact    []string `yaml:"exact"`
				Patterns []string `yaml:"patterns"`
			} `yaml:"stoplist"`
		}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return Preset{}, fmt.Errorf("hygiene preset %q: %w", name, err)
		}
		p.Code = raw.Code
		for _, label := range raw.Entities {
			if label.Scope == "document" {
				p.Values[label.Name] = true
			}
		}
		for _, word := range raw.Allow {
			p.Allow[strings.TrimSpace(word)] = true
		}
		for _, word := range raw.Stop.Exact {
			p.Stop[fold(word)] = true
		}
		for _, pattern := range raw.Stop.Patterns {
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
