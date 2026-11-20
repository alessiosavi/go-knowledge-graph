package productobs

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
)

type releaseKey struct {
	store, feature, sourceKey string
}

// Release is the set of store, feature and source-key triples to emit.
// A nil release selects every mapped source key.
type Release struct {
	keys map[releaseKey]struct{}
}

// ReleaseTriple is a store-specific feature and source key admitted for release.
type ReleaseTriple struct {
	Feature   string `json:"feature"`
	SourceKey string `json:"source_key"`
	Store     string `json:"store"`
}

// Triples returns the release keys sorted by store, feature and source key.
func (r *Release) Triples() []ReleaseTriple {
	triples := make([]ReleaseTriple, 0, len(r.keys))
	for k := range r.keys {
		triples = append(triples, ReleaseTriple{Store: k.store, Feature: k.feature, SourceKey: k.sourceKey})
	}
	slices.SortFunc(triples, func(a, b ReleaseTriple) int {
		return cmp.Or(cmp.Compare(a.Store, b.Store), cmp.Compare(a.Feature, b.Feature), cmp.Compare(a.SourceKey, b.SourceKey))
	})
	return triples
}

// ParseRelease validates a frozen candidates document against this registry.
func (r *Registry) ParseRelease(data []byte) (*Release, error) {
	obj, err := readObject(data, "registry_sha256 keys")
	if err != nil {
		return nil, fmt.Errorf("release: %w", err)
	}
	if digest, _ := readString(obj["registry_sha256"]); digest != r.Digest() {
		return nil, errors.New("release: registry_sha256 does not match the registry bytes")
	}
	keys, err := readArray(obj["keys"])
	if err != nil {
		return nil, fmt.Errorf("release keys: %w", err)
	}
	release := &Release{keys: make(map[releaseKey]struct{}, len(keys))}
	for i, raw := range keys {
		obj, err := readObject(raw, "store feature source_key")
		if err != nil {
			return nil, fmt.Errorf("release key %d: %w", i+1, err)
		}
		store, _ := readString(obj["store"])
		feature, _ := readString(obj["feature"])
		source, _ := readString(obj["source_key"])
		config, mapped := r.stores[store][source]
		if !mapped || config.feature != feature {
			return nil, fmt.Errorf("release key %d: triple is not mapped by the registry", i+1)
		}
		key := releaseKey{store: store, feature: feature, sourceKey: source}
		if _, exists := release.keys[key]; exists {
			return nil, fmt.Errorf("release key %d: duplicate triple", i+1)
		}
		release.keys[key] = struct{}{}
	}
	return release, nil
}

// Observation is an emitted observation of a mapped raw key. This struct
// and productLine keep their fields in lexicographic JSON key order.
type Observation struct {
	Conflict    bool        `json:"conflict"`
	Feature     string      `json:"feature"`
	Items       []string    `json:"items"`
	ProductID   string      `json:"product_id"`
	Raw         string      `json:"raw"`
	SourceKey   string      `json:"source_key"`
	SplitStatus string      `json:"split_status"`
	Type        string      `json:"type"`
	Typed       *TypedValue `json:"typed"`
	kind        string
}

// ValueFold returns the fold of the whole raw value.
func (o Observation) ValueFold() string {
	return Fold(o.kind, o.Raw)
}

// ItemsFolds returns the folded multi-value items in their original order.
// A single-valued observation returns nil.
func (o Observation) ItemsFolds() []string {
	if o.Items == nil {
		return nil
	}
	folds := make([]string, len(o.Items))
	for i, item := range o.Items {
		folds[i] = Fold(o.kind, item)
	}
	return folds
}

type productLine struct {
	Brand       string  `json:"brand"`
	Category    string  `json:"category"`
	Link        string  `json:"link"`
	ProductID   string  `json:"product_id"`
	ProductType *string `json:"product_type"`
	Store       string  `json:"store"`
	Subcategory string  `json:"subcategory"`
	Title       string  `json:"title"`
	Type        string  `json:"type"`
}

// Observations returns one observation per mapped attribute in feature and
// source-key order, with conflicts resolved before any release projection.
func (r *Registry) Observations(record Record, release *Release) []Observation {
	keys := r.stores[record.Store]
	var rows []Observation
	for key, raw := range record.Attributes {
		config, mapped := keys[key]
		if !mapped {
			continue
		}
		f := r.features[config.feature]
		row := Observation{
			Feature: config.feature, ProductID: record.ProductID, SourceKey: key,
			Raw: raw, Type: "obs", SplitStatus: "single", kind: f.kind,
		}
		if f.cardinality == "multi" {
			row.SplitStatus, row.Items = splitMulti(raw, config.separator)
		}
		if f.measure != nil {
			row.Typed = f.measure.parse(raw)
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b Observation) int {
		return cmp.Or(cmp.Compare(a.Feature, b.Feature), cmp.Compare(a.SourceKey, b.SourceKey))
	})
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && rows[end].Feature == rows[start].Feature {
			end++
		}
		group := rows[start:end]
		f := r.features[group[0].Feature]
		if len(group) > 1 && groupConflicts(group, f) {
			for i := range group {
				group[i].Conflict = true
			}
		}
		start = end
	}
	if release != nil {
		rows = slices.DeleteFunc(rows, func(row Observation) bool {
			_, selected := release.keys[releaseKey{record.Store, row.Feature, row.SourceKey}]
			return !selected
		})
	}
	return rows
}

func groupConflicts(group []Observation, f feature) bool {
	if f.cardinality == "single" {
		for _, row := range group[1:] {
			if !equalSingle(group[0], row) {
				return true
			}
		}
		return false
	}
	sets := make([]map[string]struct{}, len(group))
	folds := make([]string, len(group))
	for i, row := range group {
		folds[i] = row.ValueFold()
		sets[i] = make(map[string]struct{}, len(row.Items))
		for _, item := range row.Items {
			sets[i][Fold(row.kind, item)] = struct{}{}
		}
	}
	for i, left := range group {
		for j := i + 1; j < len(group); j++ {
			if left.SplitStatus == "unsplit" || group[j].SplitStatus == "unsplit" {
				if folds[i] != folds[j] {
					return true
				}
			} else if !subset(sets[i], sets[j]) && !subset(sets[j], sets[i]) {
				return true
			}
		}
	}
	return false
}

func subset(a, b map[string]struct{}) bool {
	for item := range a {
		if _, exists := b[item]; !exists {
			return false
		}
	}
	return true
}

func equalSingle(a, b Observation) bool {
	left, right := a.Typed, b.Typed
	leftParsed := left != nil && left.Status == "ok"
	rightParsed := right != nil && right.Status == "ok"
	if leftParsed != rightParsed {
		return false
	}
	if !leftParsed {
		return a.ValueFold() == b.ValueFold()
	}
	return *left.Qualifier == *right.Qualifier &&
		*left.CanonicalUnit == *right.CanonicalUnit &&
		*left.Inclusive == *right.Inclusive &&
		textOrEmpty(left.LoCanonical) == textOrEmpty(right.LoCanonical) &&
		textOrEmpty(left.HiCanonical) == textOrEmpty(right.HiCanonical)
}

func textOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// Emit writes canonical gkg-record-obs/v1 JSONL. All product records survive
// projection. Within a product, obs lines sort before the product line.
func (r *Registry) Emit(w io.Writer, records []Record, release *Release) error {
	sorted := slices.Clone(records)
	slices.SortFunc(sorted, func(a, b Record) int { return cmp.Compare(a.ProductID, b.ProductID) })
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1].ProductID == sorted[i].ProductID {
			return errors.New("records: duplicate product_id")
		}
	}
	encoder := jsontext.NewEncoder(w)
	for _, record := range sorted {
		for _, row := range r.Observations(record, release) {
			if err := json.MarshalEncode(encoder, row, json.FormatNilSliceAsNull(true)); err != nil {
				return fmt.Errorf("write observation: %w", err)
			}
		}
		product := productLine{
			Brand: record.Brand, Category: record.Category, Link: record.Link, ProductID: record.ProductID,
			ProductType: record.ProductType, Store: record.Store, Subcategory: record.Subcategory,
			Title: record.Title, Type: "product",
		}
		if err := json.MarshalEncode(encoder, product); err != nil {
			return fmt.Errorf("write product: %w", err)
		}
	}
	return nil
}
