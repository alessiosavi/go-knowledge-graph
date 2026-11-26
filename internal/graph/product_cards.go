package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

type ProductCards struct {
	Cards map[string]ProductCard
	AsOf  CardsAsOf
}

type ProductCard struct {
	Product
	Features        []ProductEvidence
	FeaturesOmitted int
}

type CardsAsOf struct {
	ReleaseID     *string
	ActivationSeq *int64
	Stores        []StoreWatermark
}

type StoreWatermark struct {
	Store                     string
	MaterializedActivationSeq *int64
}

const maxCardFeatures = 40

// Both subqueries aggregate without grouping at their boundaries. The release
// row survives even if there are no bound stores or requested products.
// Check the seed domain and source identity after matching the indexed graph ID.
const productCardsQuery = `
OPTIONAL MATCH (m:GkgMeta {id:'product_release'})
WITH m.value AS release_id, m.activation_seq AS activation_seq
CALL {
    MATCH (s:ProductStore {domain:$domain})
    WITH s ORDER BY s.store
    RETURN collect({store:s.store, materialized_activation_seq:s.materialized_activation_seq}) AS stores
}
CALL {
    WITH release_id
    UNWIND $products AS wanted
    WITH release_id, wanted.id AS id, wanted.store AS store, wanted.product_id AS product_id
    MATCH (p:Product {id:id})
    WITH p, release_id, store, product_id
    WHERE p.store = store AND p.product_id = product_id AND p.domain = $domain
    OPTIONAL MATCH (p)-[e:HAS_FEATURE]->(f:FeatureRev)
    WHERE e.release_id = release_id
    WITH p, f, e ORDER BY f.id, e.source_key
    WITH p, collect(CASE WHEN e IS NOT NULL THEN {feature:f, edge:e} END) AS evidence
    WITH p, size(evidence) AS total, evidence[..$max_features] AS selected
    RETURN collect({product:properties(p), features_omitted:total-size(selected),
        features:[row IN selected | {feature:properties(row.feature), edge:properties(row.edge)}]}) AS cards
}
RETURN release_id, activation_seq, stores, cards`

// ProductCards reads active evidence and domain store watermarks in one snapshot.
// Cards are keyed by full source product ID. Missing and out-of-domain IDs are omitted.
func (s *Store) ProductCards(ctx context.Context, domain string, productIDs []string, maxFeatures int) (ProductCards, error) {
	products := make([]map[string]any, 0, len(productIDs))
	seen := make(map[string]bool, len(productIDs))
	for _, productID := range productIDs {
		store, _, ok := strings.Cut(productID, ":")
		if !ok || store == "" || seen[productID] {
			continue
		}
		seen[productID] = true
		products = append(products, map[string]any{
			"id": productobs.ProductID(store, productID), "store": store, "product_id": productID,
		})
	}
	rows, err := s.runRead(ctx, productCardsQuery, map[string]any{
		"domain": domain, "products": products, "max_features": min(maxCardFeatures, max(0, maxFeatures)),
	})
	if err != nil {
		return ProductCards{}, fmt.Errorf("graph: product cards: %w", err)
	}
	return readProductCards(rows[0].AsMap())
}

func readProductCards(row map[string]any) (ProductCards, error) {
	stores := productProp[[]any](row, "stores")
	cards := productProp[[]any](row, "cards")
	out := ProductCards{
		Cards: make(map[string]ProductCard, len(cards)),
		AsOf: CardsAsOf{
			ReleaseID: productOptional[string](row, "release_id"), ActivationSeq: productOptional[int64](row, "activation_seq"),
			Stores: make([]StoreWatermark, 0, len(stores)),
		},
	}
	for _, store := range stores {
		props, ok := store.(map[string]any)
		if !ok {
			return ProductCards{}, fmt.Errorf("unexpected product store properties %T", store)
		}
		out.AsOf.Stores = append(out.AsOf.Stores, StoreWatermark{
			Store: productProp[string](props, "store"), MaterializedActivationSeq: productOptional[int64](props, "materialized_activation_seq"),
		})
	}
	for _, card := range cards {
		props, ok := card.(map[string]any)
		if !ok {
			return ProductCards{}, fmt.Errorf("unexpected product card properties %T", card)
		}
		rows := productProp[[]any](props, "features")
		item := ProductCard{
			Product:         readProduct(productProp[map[string]any](props, "product")),
			Features:        make([]ProductEvidence, 0, len(rows)),
			FeaturesOmitted: int(productProp[int64](props, "features_omitted")),
		}
		for _, row := range rows {
			evidence, ok := row.(map[string]any)
			if !ok {
				return ProductCards{}, fmt.Errorf("unexpected product evidence properties %T", row)
			}
			feature := readProductFeatureRev(productProp[map[string]any](evidence, "feature"))
			value, err := readProductEvidence(feature, productProp[map[string]any](evidence, "edge"))
			if err != nil {
				return ProductCards{}, err
			}
			item.Features = append(item.Features, value)
		}
		out.Cards[item.ProductID] = item
	}
	return out, nil
}
