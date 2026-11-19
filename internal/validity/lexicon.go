// Package validity evaluates the catalog validity rules and defines the
// artifacts shared by validity planning and apply.
// Character classes follow Go's Unicode tables, which can differ from the
// frozen Python reference for code points added in later Unicode versions.
// The parity corpus is unaffected by these differences.
package validity

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"

	"go.yaml.in/yaml/v4"
)

// RulesID identifies the frozen catalog rule set.
const RulesID = "catalog-v3"

// Lexicon is the contents of a preset's validity block.
type Lexicon struct {
	Rules           string            `json:"rules" yaml:"rules"`
	ProductTypes    map[string]string `json:"product_types" yaml:"product_types"`
	StopTokens      []string          `json:"stop_tokens" yaml:"stop_tokens"`
	QualityNouns    []string          `json:"quality_nouns" yaml:"quality_nouns"`
	ClassNouns      []string          `json:"class_nouns" yaml:"class_nouns"`
	EvaluativeStems []string          `json:"evaluative_stems" yaml:"evaluative_stems"`
	EvaluativeWords []string          `json:"evaluative_words" yaml:"evaluative_words"`
	SensoryStems    []string          `json:"sensory_stems" yaml:"sensory_stems"`
	SensoryWords    []string          `json:"sensory_words" yaml:"sensory_words"`
	FunctionWords   []string          `json:"function_words" yaml:"function_words"`
}

var (
	wordPattern = regexp.MustCompile(`^[a-z0-9]+$`)
	stemPattern = regexp.MustCompile(`^[a-z]+$`)
)

// DecodeLexiconYAML decodes a validity block, rejecting unknown fields. A zero
// node denotes an omitted block. An explicit null is invalid.
func DecodeLexiconYAML(node *yaml.Node) (*Lexicon, error) {
	if node.IsZero() {
		return nil, nil
	}
	var lexicon Lexicon
	if err := node.Load(&lexicon, yaml.WithKnownFields(), yaml.WithUniqueKeys(true)); err != nil {
		return nil, fmt.Errorf("validity: %w", err)
	}
	// YAML decoding into []string skips null elements. Check the nodes so a
	// list with both a token and a null entry still fails validation.
	if err := rejectNull(node, make(map[*yaml.Node]bool)); err != nil {
		return nil, err
	}
	if err := lexicon.Validate(); err != nil {
		return nil, err
	}
	return &lexicon, nil
}

func rejectNull(node *yaml.Node, seen map[*yaml.Node]bool) error {
	if seen[node] {
		return nil
	}
	seen[node] = true
	if node.ShortTag() == "!!null" {
		return fmt.Errorf("validity: null value at line %d", node.Line)
	}
	if node.Alias != nil {
		return rejectNull(node.Alias, seen)
	}
	for _, child := range node.Content {
		if err := rejectNull(child, seen); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks the rule id and the required lowercase token vocabularies.
func (l Lexicon) Validate() error {
	if l.Rules != RulesID {
		return fmt.Errorf("validity.rules: expected %q, got %q", RulesID, l.Rules)
	}
	if len(l.ProductTypes) == 0 {
		return fmt.Errorf("validity.product_types: must contain at least one mapping")
	}
	for _, key := range slices.Sorted(maps.Keys(l.ProductTypes)) {
		value := l.ProductTypes[key]
		if !wordPattern.MatchString(key) || !wordPattern.MatchString(value) {
			return fmt.Errorf("validity.product_types: %q to %q must use lowercase ASCII tokens", key, value)
		}
	}
	for _, list := range []struct {
		name    string
		entries []string
		pattern *regexp.Regexp
	}{
		{"stop_tokens", l.StopTokens, wordPattern},
		{"quality_nouns", l.QualityNouns, wordPattern},
		{"class_nouns", l.ClassNouns, wordPattern},
		{"evaluative_stems", l.EvaluativeStems, stemPattern},
		{"evaluative_words", l.EvaluativeWords, wordPattern},
		{"sensory_stems", l.SensoryStems, stemPattern},
		{"sensory_words", l.SensoryWords, wordPattern},
		{"function_words", l.FunctionWords, wordPattern},
	} {
		if len(list.entries) == 0 {
			return fmt.Errorf("validity.%s: must contain at least one token", list.name)
		}
		for _, entry := range list.entries {
			if !list.pattern.MatchString(entry) {
				return fmt.Errorf("validity.%s: token %q must match %s", list.name, entry, list.pattern)
			}
		}
	}
	return nil
}

// LexiconDigest returns the lowercase hex SHA-256 of the validated block's
// compact JSON object (without a validity wrapper or trailing newline). Keys
// are emitted in Lexicon field order: rules, product_types, stop_tokens,
// quality_nouns, class_nouns, evaluative_stems, evaluative_words, sensory_stems,
// sensory_words, function_words. Product_types keys are sorted lexicographically.
// Lists retain their file order, including any repeated tokens.
func LexiconDigest(lexicon Lexicon) (string, error) {
	if err := lexicon.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(lexicon)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}
