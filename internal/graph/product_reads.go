package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

var (
	ErrProductReadInput  = errors.New("invalid product read request")
	ErrProductNotNumeric = errors.New("not_numeric: no released key supports numeric search for this feature in scope")
	ErrProductCursor     = errors.New("stale_cursor: search filters, product snapshots, or the active release changed")
)

type ProductLookup struct {
	ID        string `json:"id,omitempty" jsonschema:"Graph product ID"`
	Store     string `json:"store,omitempty" jsonschema:"Store for an exact lookup by product_id"`
	ProductID string `json:"product_id,omitempty" jsonschema:"Full source product ID including its store prefix"`
}

const ProductLookupDescription = "Get a product with its active feature observations and up to three associated pages (pages_missing when none). " +
	"Provide id, or both store and the full source product_id."

type Product struct {
	ID          string  `json:"id"`
	Store       string  `json:"store"`
	ProductID   string  `json:"product_id"`
	Domain      string  `json:"domain"`
	Title       string  `json:"title"`
	Brand       string  `json:"brand"`
	Category    string  `json:"category"`
	Subcategory string  `json:"subcategory"`
	ProductType *string `json:"product_type,omitempty"`
	Link        string  `json:"link"`
}

type ProductFeature struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	Cardinality   string  `json:"cardinality"`
	CanonicalUnit *string `json:"canonical_unit"`
}

type ProductFeatureRev struct {
	ProductFeature
	Key              string  `json:"key"`
	Rev              string  `json:"rev"`
	Definition       string  `json:"definition"`
	DefinitionDigest string  `json:"definition_digest"`
	Replaces         *string `json:"replaces,omitempty"`
}

type ProductEvidence struct {
	Feature     ProductFeatureRev      `json:"feature"`
	SourceKey   string                 `json:"source_key"`
	Raw         string                 `json:"raw"`
	Items       []string               `json:"items,omitempty"`
	SplitStatus string                 `json:"split_status"`
	Typed       *productobs.TypedValue `json:"typed,omitempty"`
	Conflict    bool                   `json:"conflict"`
	Numeric     bool                   `json:"numeric"`
	ReleaseID   string                 `json:"release_id"`
}

type ProductDetail struct {
	Product
	Features     []ProductEvidence `json:"features"`
	Pages        []ProductPage     `json:"pages"`
	PagesMissing bool              `json:"pages_missing"`
}

// Keep the binding check in the projection so the optional match can use
// the ProductStore(store) index.
const getProductQuery = `
MATCH (p:Product {id:$id})
WHERE $store = '' OR (p.store = $store AND p.product_id = $product)
OPTIONAL MATCH (s:ProductStore {store:p.store})
OPTIONAL MATCH (m:GkgMeta {id:'product_release'})
OPTIONAL MATCH (p)-[e:HAS_FEATURE]->(f:FeatureRev)
WHERE e.release_id = m.value
RETURN properties(p) AS product, CASE WHEN s.domain IS NOT NULL THEN s.domain ELSE null END AS page_domain,
       properties(f) AS feature, properties(e) AS edge
ORDER BY f.id, e.source_key`

// GetProduct reads the product, active observations, and pages in one snapshot.
// The page statement uses URL candidates derived from the product link in Go.
func (s *Store) GetProduct(ctx context.Context, lookup ProductLookup) (ProductDetail, error) {
	id := lookup.ID
	switch {
	case id != "" && lookup.Store == "" && lookup.ProductID == "":
	case id == "" && lookup.Store != "" && lookup.ProductID != "":
		id = productobs.ProductID(lookup.Store, lookup.ProductID)
	default:
		return ProductDetail{}, fmt.Errorf("%w: provide id or both store and product_id", ErrProductReadInput)
	}
	var out ProductDetail
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := txRows(ctx, tx, getProductQuery, map[string]any{"id": id, "store": lookup.Store, "product": lookup.ProductID})
		if err != nil {
			return err
		}
		out, err = readProductDetail(rows)
		if err != nil {
			return err
		}
		out.Pages, err = txProductPages(ctx, tx, getString(rows[0], "page_domain"), out.Link)
		out.PagesMissing = len(out.Pages) == 0
		return err
	})
	if err != nil {
		return ProductDetail{}, err
	}
	return out, nil
}

func readProductDetail(rows []*neo4j.Record) (ProductDetail, error) {
	if len(rows) == 0 {
		return ProductDetail{}, ErrNotFound
	}
	product, _, err := neo4j.GetRecordValue[map[string]any](rows[0], "product")
	if err != nil {
		return ProductDetail{}, err
	}
	out := ProductDetail{Product: readProduct(product), Features: make([]ProductEvidence, 0, len(rows))}
	for _, row := range rows {
		edge, null, err := neo4j.GetRecordValue[map[string]any](row, "edge")
		if err != nil {
			return ProductDetail{}, err
		}
		if null {
			continue
		}
		feature, _, err := neo4j.GetRecordValue[map[string]any](row, "feature")
		if err != nil {
			return ProductDetail{}, err
		}
		evidence, err := readProductEvidence(readProductFeatureRev(feature), edge)
		if err != nil {
			return ProductDetail{}, err
		}
		out.Features = append(out.Features, evidence)
	}
	return out, nil
}

type ProductStoreWatermark struct {
	Store                     string  `json:"store"`
	Domain                    string  `json:"domain"`
	SnapshotSeq               int64   `json:"snapshot_seq"`
	SeenSeq                   int64   `json:"seen_seq"`
	WipedSeq                  int64   `json:"wiped_seq"`
	Wiped                     bool    `json:"wiped"`
	ReleaseID                 *string `json:"release_id"`
	ProductCount              int64   `json:"product_count"`
	EdgeCount                 int64   `json:"edge_count"`
	ImportedAt                *string `json:"imported_at"`
	MaterializedActivationSeq *int64  `json:"materialized_activation_seq"`
	PushedAt                  *string `json:"pushed_at"`
}

type ProductStores struct {
	ActiveReleaseID *string                 `json:"active_release_id"`
	Stores          []ProductStoreWatermark `json:"stores"`
}

const productStoresMatch = `
OPTIONAL MATCH (m:GkgMeta {id:'product_release'})
OPTIONAL MATCH (s:ProductStore)
WHERE s.domain IS NOT NULL`

const productStoresReturn = `
RETURN m.value AS active_release_id, coalesce(m.activation_seq, 0) AS activation_seq,
       properties(s) AS store
ORDER BY s.store`

const productStoresQuery = productStoresMatch + productStoresReturn

func (s *Store) ListProductStores(ctx context.Context) (ProductStores, error) {
	rows, err := s.runRead(ctx, productStoresQuery, nil)
	if err != nil {
		return ProductStores{}, err
	}
	out, _, err := readProductStores(rows)
	return out, err
}

func readProductStores(rows []*neo4j.Record) (ProductStores, int64, error) {
	out := ProductStores{Stores: make([]ProductStoreWatermark, 0, len(rows))}
	if len(rows) == 0 {
		return out, 0, nil
	}
	pointer := rows[0].AsMap()
	out.ActiveReleaseID = productOptional[string](pointer, "active_release_id")
	for _, row := range rows {
		p, null, err := neo4j.GetRecordValue[map[string]any](row, "store")
		if err != nil {
			return ProductStores{}, 0, err
		}
		if null {
			continue
		}
		seq := productProp[int64](p, "snapshot_seq")
		seen, ok := p["seen_seq"].(int64)
		if !ok {
			seen = seq
		}
		wiped, hasWipe := p["wiped_seq"].(int64)
		release := productOptional[string](p, "release_id")
		out.Stores = append(out.Stores, ProductStoreWatermark{
			Store: productProp[string](p, "store"), Domain: productProp[string](p, "domain"),
			SnapshotSeq: seq, SeenSeq: seen, WipedSeq: wiped,
			Wiped: hasWipe && release == nil, ReleaseID: release, ImportedAt: productOptional[string](p, "imported_at"),
			ProductCount: productProp[int64](p, "product_count"), EdgeCount: productProp[int64](p, "edge_count"),
			MaterializedActivationSeq: productOptional[int64](p, "materialized_activation_seq"), PushedAt: productOptional[string](p, "pushed_at"),
		})
	}
	return out, productProp[int64](pointer, "activation_seq"), nil
}

type ProductFeatureKey struct {
	Store         string `json:"store"`
	SourceKey     string `json:"source_key"`
	NumericSearch bool   `json:"numeric_search"`
	Released      bool   `json:"released"`
}

type ProductFeatureListing struct {
	ProductFeature
	Released bool                `json:"released"`
	Keys     []ProductFeatureKey `json:"keys"`
}

type ProductFeatures struct {
	ActiveReleaseID *string                 `json:"active_release_id"`
	Features        []ProductFeatureListing `json:"features"`
}

const ProductFeaturesDescription = "List features in the active release's registry"

func (s *Store) ListProductFeatures(ctx context.Context) (ProductFeatures, error) {
	out := ProductFeatures{Features: []ProductFeatureListing{}}
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := txRows(ctx, tx, `MATCH (m:GkgMeta {id:'product_release'}) RETURN m.value AS id`, nil)
		if err != nil || len(rows) == 0 {
			return err
		}
		id := getString(rows[0], "id")
		manifest, registry, err := txProductReadRegistry(ctx, tx, id)
		if err != nil {
			return err
		}
		release, err := registry.ProjectRelease(manifest.Triples)
		if err != nil {
			return fmt.Errorf("read active release %q projection: %w", id, err)
		}
		keys := make(map[string][]ProductFeatureKey)
		released := make(map[string]bool)
		for _, triple := range registry.MappedKeys() {
			mapping, _ := registry.Lookup(triple.Store, triple.SourceKey)
			inRelease := release.Includes(triple.Store, triple.Feature, triple.SourceKey)
			keys[triple.Feature] = append(keys[triple.Feature], ProductFeatureKey{
				Store: triple.Store, SourceKey: triple.SourceKey, NumericSearch: mapping.NumericSearch, Released: inRelease,
			})
			released[triple.Feature] = released[triple.Feature] || inRelease
		}
		out.ActiveReleaseID = &id
		for _, f := range registry.FeatureDefinitions() {
			item := ProductFeatureListing{
				ProductFeature: ProductFeature{ID: f.ID, Name: f.Name, Kind: f.Kind, Cardinality: f.Cardinality},
				Released:       released[f.ID],
				Keys:           keys[f.ID],
			}
			if f.CanonicalUnit != "" {
				item.CanonicalUnit = &f.CanonicalUnit
			}
			if item.Keys == nil {
				item.Keys = []ProductFeatureKey{}
			}
			out.Features = append(out.Features, item)
		}
		return nil
	})
	return out, err
}

func txProductReadRegistry(ctx context.Context, tx neo4j.ManagedTransaction, id string) (productManifest, *productobs.Registry, error) {
	manifest, _, err := txProductManifest(ctx, tx, id)
	if err != nil {
		return productManifest{}, nil, err
	}
	registry, err := txProductRegistry(ctx, tx, manifest.RegistryRev)
	if err != nil {
		return productManifest{}, nil, err
	}
	if registry.Digest() != manifest.RegistrySHA256 {
		return productManifest{}, nil, fmt.Errorf("registry bytes do not match active release %q", id)
	}
	return manifest, registry, nil
}

func readProduct(p map[string]any) Product {
	return Product{
		ID: productProp[string](p, "id"), Store: productProp[string](p, "store"), ProductID: productProp[string](p, "product_id"),
		Domain: productProp[string](p, "domain"), Title: productProp[string](p, "title"), Brand: productProp[string](p, "brand"),
		Category: productProp[string](p, "category"), Subcategory: productProp[string](p, "subcategory"),
		ProductType: productOptional[string](p, "product_type"), Link: productProp[string](p, "link"),
	}
}

func readProductFeatureRev(f map[string]any) ProductFeatureRev {
	return ProductFeatureRev{
		ProductFeature: ProductFeature{
			ID: productProp[string](f, "id"), Name: productProp[string](f, "name"), Kind: productProp[string](f, "kind"),
			Cardinality: productProp[string](f, "cardinality"), CanonicalUnit: productOptional[string](f, "canonical_unit"),
		},
		Key: productProp[string](f, "key"), Rev: productProp[string](f, "rev"), Definition: productProp[string](f, "definition"),
		DefinitionDigest: productProp[string](f, "definition_digest"), Replaces: productOptional[string](f, "replaces"),
	}
}

func readProductEvidence(f ProductFeatureRev, e map[string]any) (ProductEvidence, error) {
	out := ProductEvidence{
		Feature: f, SourceKey: productProp[string](e, "source_key"), Raw: productProp[string](e, "raw"),
		SplitStatus: productProp[string](e, "split_status"), Conflict: productProp[bool](e, "conflict"),
		Numeric: productProp[bool](e, "numeric"), ReleaseID: productProp[string](e, "release_id"),
	}
	if items := productProp[string](e, "items_json"); items != "" {
		if err := json.Unmarshal([]byte(items), &out.Items); err != nil {
			return ProductEvidence{}, fmt.Errorf("decode stored items for %s: %w", out.SourceKey, err)
		}
	}
	if status := productProp[string](e, "status"); status != "" {
		out.Typed = &productobs.TypedValue{
			Status: status, Qualifier: productOptional[string](e, "qualifier"), Inclusive: productOptional[bool](e, "inclusive"),
			Lo: productOptional[string](e, "lo"), Hi: productOptional[string](e, "hi"), UnitIn: productOptional[string](e, "unit_in"),
			LoCanonical: productOptional[string](e, "lo_canonical"), HiCanonical: productOptional[string](e, "hi_canonical"),
			CanonicalUnit: productOptional[string](e, "canonical_unit"),
		}
	}
	return out, nil
}

func productProp[T any](p map[string]any, key string) T {
	value, _ := p[key].(T)
	return value
}

func productOptional[T any](p map[string]any, key string) *T {
	value, ok := p[key].(T)
	if !ok {
		return nil
	}
	return &value
}
