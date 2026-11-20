// Package productobs implements the graph-free product observation rules.
package productobs

import (
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

const RegistrySchema = "gkg-features/v1"

var (
	storePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
	integerPattern = regexp.MustCompile(`^[0-9]+$`)
	ignoreReasons  = []string{"marketing_text", "description", "logistics", "identifier_internal", "out_of_scope", "ambiguous_key"}
)

// Registry holds the validated feature rules and per-store route for each
// mapped raw key.
type Registry struct {
	digest   string
	features map[string]feature
	stores   map[string]map[string]mapping
}

type feature struct {
	name        string
	definition  string
	kind        string
	cardinality string
	measure     *measure
}

// FeatureDefinition is the metadata stored on each graph feature revision.
type FeatureDefinition struct {
	ID, Name, Definition, Kind, Cardinality, CanonicalUnit string
}

// FeatureDefinitions returns the metadata in feature ID order.
func (r *Registry) FeatureDefinitions() []FeatureDefinition {
	definitions := make([]FeatureDefinition, 0, len(r.features))
	for _, id := range slices.Sorted(maps.Keys(r.features)) {
		f := r.features[id]
		definition := FeatureDefinition{
			ID: id, Name: f.name, Definition: f.definition, Kind: f.kind, Cardinality: f.cardinality,
		}
		if f.measure != nil {
			definition.CanonicalUnit = f.measure.canonical
		}
		definitions = append(definitions, definition)
	}
	return definitions
}

// MappingSemanticChange reports alias or separator changes for a feature
// shared by two registry revisions. A key is compared when both revisions
// map that exact store and source key to the same feature.
func (r *Registry) MappingSemanticChange(previous *Registry, id string) string {
	current, old := r.features[id], previous.features[id]
	if current.measure != nil && old.measure != nil && !maps.Equal(current.measure.aliases, old.measure.aliases) {
		return "unit aliases"
	}
	for store, keys := range r.stores {
		for key, m := range keys {
			if prior, ok := previous.stores[store][key]; ok && m.feature == id && prior.feature == id && m.separator != prior.separator {
				return "separator"
			}
		}
	}
	return ""
}

type mapping struct {
	feature       string
	separator     string
	numericSearch bool
}

// Mapping describes the feature rules for a store's mapped source key.
// An empty Separator or CanonicalUnit represents a JSON null.
type Mapping struct {
	FeatureID     string
	Separator     string
	Kind          string
	Cardinality   string
	CanonicalUnit string
	NumericSearch bool
}

// Digest returns the hex SHA-256 of the R.json file bytes as shipped,
// including whitespace and key order, as pinned by release candidates.
func (r *Registry) Digest() string {
	return r.digest
}

// Counts returns the schema-mode counts for stores, features and mapped keys.
func (r *Registry) Counts() (stores, features, mapped int) {
	for _, keys := range r.stores {
		mapped += len(keys)
	}
	return len(r.stores), len(r.features), mapped
}

// MappedKeys returns the registry's mapped keys in store and source-key order.
func (r *Registry) MappedKeys() []ReleaseTriple {
	var result []ReleaseTriple
	for _, store := range slices.Sorted(maps.Keys(r.stores)) {
		keys := r.stores[store]
		for _, key := range slices.Sorted(maps.Keys(keys)) {
			result = append(result, ReleaseTriple{Store: store, Feature: keys[key].feature, SourceKey: key})
		}
	}
	return result
}

// Lookup returns the mapping for an exact store and source key, if present.
func (r *Registry) Lookup(store, key string) (Mapping, bool) {
	m, ok := r.stores[store][key]
	if !ok {
		return Mapping{}, false
	}
	f := r.features[m.feature]
	result := Mapping{
		FeatureID: m.feature, Separator: m.separator, NumericSearch: m.numericSearch,
		Kind: f.kind, Cardinality: f.cardinality,
	}
	if f.measure != nil {
		result.CanonicalUnit = f.measure.canonical
	}
	return result, true
}

// ParseRegistry validates a gkg-features/v1 JSON document in schema mode.
func ParseRegistry(data []byte) (*Registry, error) {
	root, err := readObject(data, "schema features stores")
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	if schema, _ := readString(root["schema"]); schema != RegistrySchema {
		return nil, fmt.Errorf("registry: expected schema %s", RegistrySchema)
	}

	features, err := readArray(root["features"])
	if err != nil {
		return nil, fmt.Errorf("registry features: %w", err)
	}
	stores, err := readObject(root["stores"], "")
	if err != nil {
		return nil, fmt.Errorf("registry stores: %w", err)
	}
	r := &Registry{
		digest:   fmt.Sprintf("%x", sha256.Sum256(data)),
		features: make(map[string]feature, len(features)),
		stores:   make(map[string]map[string]mapping, len(stores)),
	}
	for i, raw := range features {
		id, f, err := parseFeature(raw)
		if err != nil {
			return nil, fmt.Errorf("registry feature %d: %w", i+1, err)
		}
		if _, exists := r.features[id]; exists {
			return nil, fmt.Errorf("registry feature %d: duplicate id", i+1)
		}
		r.features[id] = f
	}
	for store, raw := range stores {
		if !storePattern.MatchString(store) {
			return nil, errors.New("registry: invalid store")
		}
		keys, err := parseStore(raw, r.features)
		if err != nil {
			return nil, fmt.Errorf("registry store: %w", err)
		}
		r.stores[store] = keys
	}
	return r, nil
}

func parseFeature(raw jsontext.Value) (string, feature, error) {
	var f feature
	obj, err := readObject(raw, "id name definition kind cardinality checkability measure")
	if err != nil {
		return "", f, err
	}
	for _, field := range []string{"id", "name", "definition"} {
		if s, _ := readString(obj[field]); !nonempty(s) {
			return "", f, fmt.Errorf("%s: expected non-empty string", field)
		}
	}
	id, _ := readString(obj["id"])
	f.name, _ = readString(obj["name"])
	f.definition, _ = readString(obj["definition"])
	f.kind, _ = readString(obj["kind"])
	switch f.kind {
	case "enum", "code", "text", "measure":
	default:
		return "", f, errors.New("invalid kind")
	}
	f.cardinality, _ = readString(obj["cardinality"])
	switch f.cardinality {
	case "single", "multi":
	default:
		return "", f, errors.New("invalid cardinality")
	}
	wantCheckability := "page_checkable"
	if f.kind == "text" {
		wantCheckability = "unverified_text"
	}
	if checkability, _ := readString(obj["checkability"]); checkability != wantCheckability {
		return "", f, errors.New("invalid checkability for kind")
	}
	if f.kind == "measure" {
		f.measure, err = parseMeasure(obj["measure"])
		if err != nil {
			return "", f, fmt.Errorf("measure: %w", err)
		}
	} else if obj["measure"].Kind() != jsontext.KindNull {
		return "", f, errors.New("non-measure kind requires null measure")
	}
	return id, f, nil
}

func parseMeasure(raw jsontext.Value) (*measure, error) {
	obj, err := readObject(raw, "canonical_unit aliases")
	if err != nil {
		return nil, err
	}
	canonical, _ := readString(obj["canonical_unit"])
	aliases, err := readObject(obj["aliases"], "")
	if err != nil {
		return nil, fmt.Errorf("aliases: %w", err)
	}
	factors := make(map[string]string, len(aliases))
	for alias, rawFactor := range aliases {
		factor, ok := readString(rawFactor)
		if !ok {
			return nil, errors.New("expected a positive decimal string factor")
		}
		factors[alias] = factor
	}
	return newMeasure(canonical, factors)
}

func parseStore(raw jsontext.Value, features map[string]feature) (map[string]mapping, error) {
	obj, err := readObject(raw, "keys ignore deferred")
	if err != nil {
		return nil, err
	}
	keys, err := readObject(obj["keys"], "")
	if err != nil {
		return nil, fmt.Errorf("keys: %w", err)
	}
	ignore, err := readObject(obj["ignore"], "")
	if err != nil {
		return nil, fmt.Errorf("ignore: %w", err)
	}
	dispositions, err := uniqueStrings(obj["deferred"])
	if err != nil {
		return nil, fmt.Errorf("deferred: %w", err)
	}
	for _, collection := range []object{keys, ignore} {
		for key := range collection {
			if !nonempty(key) {
				return nil, errors.New("empty raw key")
			}
			if _, exists := dispositions[key]; exists {
				return nil, errors.New("overlapping key dispositions")
			}
			dispositions[key] = struct{}{}
		}
	}

	mapped := make(map[string]mapping, len(keys))
	assignedLabels := make(map[string]struct{})
	for key, entry := range keys {
		m, labels, err := parseMapping(entry, features)
		if err != nil {
			return nil, fmt.Errorf("mapped key: %w", err)
		}
		for label := range labels {
			if _, exists := assignedLabels[label]; exists {
				return nil, errors.New("source label serves multiple keys")
			}
			assignedLabels[label] = struct{}{}
		}
		mapped[key] = m
	}
	for _, raw := range ignore {
		entry, err := readObject(raw, "reason dev_evidence")
		if err != nil {
			return nil, fmt.Errorf("ignored key: %w", err)
		}
		if reason, _ := readString(entry["reason"]); !slices.Contains(ignoreReasons, reason) {
			return nil, errors.New("ignored key: invalid reason")
		}
		if !enoughEvidence(entry["dev_evidence"], '1') {
			return nil, errors.New("ignored key: expected positive DEV evidence")
		}
	}
	return mapped, nil
}

func parseMapping(raw jsontext.Value, features map[string]feature) (mapping, map[string]struct{}, error) {
	var m mapping
	entry, err := readObject(raw, "feature separator source_labels dev_evidence numeric_search")
	if err != nil {
		return m, nil, err
	}
	m.feature, _ = readString(entry["feature"])
	f, exists := features[m.feature]
	if !exists {
		return m, nil, errors.New("unknown feature id")
	}
	if separator := entry["separator"]; separator.Kind() != jsontext.KindNull {
		m.separator, _ = readString(separator)
		if f.cardinality != "multi" || !nonempty(m.separator) || strings.ContainsAny(m.separator, "()") {
			return m, nil, errors.New("invalid separator for cardinality")
		}
		if m.separator != Fold("measure", m.separator) {
			return m, nil, errors.New("separator must be NFKC and whitespace-collapsed")
		}
	}
	labels, err := uniqueStrings(entry["source_labels"])
	if err != nil {
		return m, nil, fmt.Errorf("source labels: %w", err)
	}
	if !enoughEvidence(entry["dev_evidence"], '5') {
		return m, nil, errors.New("expected at least 5 DEV occurrences")
	}
	kind := entry["numeric_search"].Kind()
	if kind != jsontext.KindTrue && kind != jsontext.KindFalse {
		return m, nil, errors.New("invalid numeric_search")
	}
	m.numericSearch = kind == jsontext.KindTrue
	return m, labels, nil
}

// The schema requires positive JSON integers, without imposing a size limit.
// Strict JSON decoding already disallows leading zeroes.
func enoughEvidence(value jsontext.Value, minimum byte) bool {
	return integerPattern.Match(value) && (len(value) > 1 || value[0] >= minimum)
}
