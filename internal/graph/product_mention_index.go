package graph

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

type ProductMentionIdentity struct {
	ID    string `json:"id"`
	Store string `json:"store"`
}

type ProductMentionAmbiguity struct {
	Key      string                   `json:"key"`
	Products []ProductMentionIdentity `json:"products"`
}

type ProductMentionDivergenceExample struct {
	ProductMentionIdentity
	Key       string `json:"key"`
	Feature   string `json:"feature"`
	ValueFold string `json:"value_fold"`
}

type ProductMentionDivergence struct {
	Count    int                               `json:"count"`
	Examples []ProductMentionDivergenceExample `json:"examples"`
}

type ProductMentionStoreStats struct {
	IdentifierProducts int
	EligibleProducts   int
}

type ProductMentionStats struct {
	IdentifierValues int
	EligibleKeys     int
	IneligibleValues int
	Ambiguous        []ProductMentionAmbiguity
	Divergence       ProductMentionDivergence
	ByStore          map[string]ProductMentionStoreStats
}

type ProductMentionEvidence struct {
	Feature   string
	ValueFold string
}

type ProductMentionMatch struct {
	productobs.CodeMatch
	Product  Product
	Evidence []ProductMentionEvidence
}

type productMentionKey struct {
	product  *Product
	evidence []ProductMentionEvidence
}

// ProductMentionIndex matches eligible identifiers unique across all its stores.
type ProductMentionIndex struct {
	keys  map[string]productMentionKey
	Stats ProductMentionStats
}

// BuildProductMentionIndex accepts the observations from every bound store.
// The input order determines which normalization examples appear in the stats.
func BuildProductMentionIndex(observations []ProductIdentifierObservation) *ProductMentionIndex {
	index, _ := buildProductMentionIndex(context.Background(), observations)
	return index
}

func buildProductMentionIndex(ctx context.Context, observations []ProductIdentifierObservation) (*ProductMentionIndex, error) {
	var numValues int
	for _, observation := range observations {
		numValues += len(observation.ItemsFold)
		if observation.ValueFold != nil {
			numValues++
		}
	}
	index := &ProductMentionIndex{
		keys: make(map[string]productMentionKey, numValues),
		Stats: ProductMentionStats{
			Ambiguous:  []ProductMentionAmbiguity{},
			Divergence: ProductMentionDivergence{Examples: []ProductMentionDivergenceExample{}},
			ByStore:    make(map[string]ProductMentionStoreStats),
		},
	}
	type productState struct {
		product  Product
		eligible bool
	}
	products := make(map[string]*productState)
	ambiguous := make(map[string]map[string]ProductMentionIdentity)
	type identifierValue struct {
		productID string
		ProductMentionEvidence
	}
	seenValues := make(map[identifierValue]struct{}, numValues)
	// Single-value keys share backing storage. Restrict each slice's
	// capacity so extra evidence for one key cannot overwrite another.
	evidenceBuffer := make([]ProductMentionEvidence, 0, numValues)
	addValue := func(state *productState, feature, value string) {
		product := &state.product
		evidence := ProductMentionEvidence{Feature: feature, ValueFold: value}
		triple := identifierValue{productID: product.ID, ProductMentionEvidence: evidence}
		if _, seen := seenValues[triple]; seen {
			return
		}
		seenValues[triple] = struct{}{}
		index.Stats.IdentifierValues++
		key := productobs.CodeKey(value)
		if key != value {
			index.Stats.Divergence.Count++
			if len(index.Stats.Divergence.Examples) < 20 {
				index.Stats.Divergence.Examples = append(index.Stats.Divergence.Examples, ProductMentionDivergenceExample{
					ProductMentionIdentity: ProductMentionIdentity{ID: product.ID, Store: product.Store},
					Key:                    key, Feature: feature, ValueFold: value,
				})
			}
		}
		if !productobs.EligibleCodeKey(key) {
			index.Stats.IneligibleValues++
			return
		}
		if !state.eligible {
			state.eligible = true
			stats := index.Stats.ByStore[product.Store]
			stats.EligibleProducts++
			index.Stats.ByStore[product.Store] = stats
		}
		if identities := ambiguous[key]; identities != nil {
			identities[product.ID] = ProductMentionIdentity{ID: product.ID, Store: product.Store}
			return
		}
		entry := index.keys[key]
		switch {
		case entry.product == nil:
			start := len(evidenceBuffer)
			evidenceBuffer = append(evidenceBuffer, evidence)
			index.keys[key] = productMentionKey{product: product, evidence: evidenceBuffer[start : start+1 : start+1]}
		case entry.product == product:
			entry.evidence = append(entry.evidence, evidence)
			index.keys[key] = entry
		default:
			ambiguous[key] = map[string]ProductMentionIdentity{
				entry.product.ID: {ID: entry.product.ID, Store: entry.product.Store},
				product.ID:       {ID: product.ID, Store: product.Store},
			}
			delete(index.keys, key)
		}
	}
	for _, observation := range observations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		product := observation.Product
		state := products[product.ID]
		if state == nil {
			state = &productState{product: product}
			products[product.ID] = state
			stats := index.Stats.ByStore[product.Store]
			stats.IdentifierProducts++
			index.Stats.ByStore[product.Store] = stats
		}
		if observation.ValueFold != nil {
			addValue(state, observation.Feature, *observation.ValueFold)
		}
		for _, item := range observation.ItemsFold {
			addValue(state, observation.Feature, item)
		}
	}
	index.Stats.EligibleKeys = len(index.keys) + len(ambiguous)
	for _, entry := range index.keys {
		slices.SortFunc(entry.evidence, func(a, b ProductMentionEvidence) int {
			return cmp.Or(cmp.Compare(a.Feature, b.Feature), cmp.Compare(a.ValueFold, b.ValueFold))
		})
	}
	for _, key := range slices.Sorted(maps.Keys(ambiguous)) {
		identities := ambiguous[key]
		ambiguity := ProductMentionAmbiguity{Key: key, Products: make([]ProductMentionIdentity, 0, len(identities))}
		for _, id := range slices.Sorted(maps.Keys(identities)) {
			ambiguity.Products = append(ambiguity.Products, identities[id])
		}
		index.Stats.Ambiguous = append(index.Stats.Ambiguous, ambiguity)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return index, nil
}

// Match returns exact matches in original byte-span order. The returned
// evidence is shared with the index and must be treated as read-only.
func (index *ProductMentionIndex) Match(text string) []ProductMentionMatch {
	matches := productobs.MatchCodes(text, index.keys)
	if len(matches) == 0 {
		return nil
	}
	out := make([]ProductMentionMatch, 0, len(matches))
	for _, match := range matches {
		entry := index.keys[match.Key]
		out = append(out, ProductMentionMatch{CodeMatch: match, Product: *entry.product, Evidence: entry.evidence})
	}
	return out
}
