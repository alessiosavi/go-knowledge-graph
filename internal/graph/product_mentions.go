package graph

import (
	"context"
	"errors"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

type ProductMentionSelection struct {
	DocumentIDs  []string
	Domain       string
	SourcePrefix string
}

type ProductMentionStore struct {
	Store              string  `json:"store"`
	SeenSeq            int64   `json:"seen_seq"`
	SnapshotSeq        int64   `json:"snapshot_seq"`
	WipedSeq           int64   `json:"wiped_seq"`
	ReleaseID          *string `json:"release_id"`
	ProductCount       int64   `json:"product_count"`
	IdentifierProducts int     `json:"identifier_products"`
	EligibleProducts   int     `json:"eligible_products"`
	Domain             string  `json:"domain"`
}

type ProductIdentifierObservation struct {
	Product   Product
	Feature   string
	ValueFold *string
	ItemsFold []string
}

type ProductMentionInput struct {
	Release       string
	ActivationSeq int64
	Stores        []ProductMentionStore
	Documents     []Document
	Identifiers   []ProductIdentifierObservation
}

// ReadProductMentions delivers catalogue and document metadata before visiting
// chunks. All reads share one snapshot, and visits are ordered by source and seq.
func (s *Store) ReadProductMentions(ctx context.Context, selection ProductMentionSelection,
	begin func(ProductMentionInput) error, visitChunk func(Chunk) error) error {
	return s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		release, activation, err := txMentionRelease(ctx, tx)
		if err != nil {
			return err
		}
		if release == "" {
			return fmt.Errorf("no active product release")
		}
		input := ProductMentionInput{Release: release, ActivationSeq: activation}
		input.Documents, err = txMentionDocuments(ctx, tx, selection)
		if err != nil {
			return err
		}
		input.Stores, err = txMentionStores(ctx, tx)
		if err != nil {
			return err
		}
		for _, store := range input.Stores {
			input.Identifiers, err = txMentionIdentifiers(ctx, tx, store.Store, input.Release, input.Identifiers)
			if err != nil {
				return err
			}
		}
		if err := begin(input); err != nil {
			return err
		}
		return txVisitMentionChunks(ctx, tx, input.Documents, visitChunk)
	})
}

const maxProductMentionValues = 200_000

var ErrProductMentionIndexTooLarge = errors.New("product mention index exceeds its raw identifier limit")

// LoadProductMentionIndex reads the active release, bound stores and
// identifiers in one snapshot. A knowledge-only graph has an empty index.
func (s *Store) LoadProductMentionIndex(ctx context.Context) (*ProductMentionIndex, error) {
	return s.loadProductMentionIndex(ctx, maxProductMentionValues)
}

func (s *Store) loadProductMentionIndex(ctx context.Context, limit int) (*ProductMentionIndex, error) {
	var observations []ProductIdentifierObservation
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		release, _, err := txMentionRelease(ctx, tx)
		if err != nil || release == "" {
			return err
		}
		stores, err := txMentionStores(ctx, tx)
		if err != nil || len(stores) == 0 {
			return err
		}
		names := make([]string, 0, len(stores))
		for _, store := range stores {
			names = append(names, store.Store)
		}
		params := map[string]any{"stores": names, "release": release}
		rows, err := txRows(ctx, tx, boundMentionIdentifiers+`
RETURN sum(CASE WHEN e.value_fold IS NULL THEN 0 ELSE 1 END + coalesce(size(e.items_fold), 0)) AS raw_values`, params)
		if err != nil {
			return err
		}
		rawValues, _, err := neo4j.GetRecordValue[int64](rows[0], "raw_values")
		if err != nil {
			return err
		}
		if rawValues > int64(limit) {
			return ErrProductMentionIndexTooLarge
		}
		observations, err = txLoadMentionIdentifiers(ctx, tx, params)
		return err
	})
	if err != nil {
		return nil, err
	}
	return buildProductMentionIndex(ctx, observations)
}

func txMentionRelease(ctx context.Context, tx neo4j.ManagedTransaction) (string, int64, error) {
	rows, err := txRows(ctx, tx, `
MATCH (m:GkgMeta {id:'product_release'})
RETURN m.value AS release, coalesce(m.activation_seq, 0) AS activation_seq`, nil)
	if err != nil || len(rows) == 0 {
		return "", 0, err
	}
	return getString(rows[0], "release"), productProp[int64](rows[0].AsMap(), "activation_seq"), nil
}

func txMentionStores(ctx context.Context, tx neo4j.ManagedTransaction) ([]ProductMentionStore, error) {
	rows, err := txRows(ctx, tx, `
MATCH (s:ProductStore) WHERE s.domain IS NOT NULL
RETURN s.store AS store, s.domain AS domain, coalesce(s.seen_seq, s.snapshot_seq, 0) AS seen_seq,
       coalesce(s.snapshot_seq, 0) AS snapshot_seq, coalesce(s.product_count, 0) AS product_count,
       coalesce(s.wiped_seq, 0) AS wiped_seq, s.release_id AS release_id
ORDER BY s.store`, nil)
	if err != nil {
		return nil, err
	}
	stores := make([]ProductMentionStore, 0, len(rows))
	for _, row := range rows {
		props := row.AsMap()
		stores = append(stores, ProductMentionStore{
			Store: getString(row, "store"), Domain: getString(row, "domain"),
			SeenSeq: productProp[int64](props, "seen_seq"), SnapshotSeq: productProp[int64](props, "snapshot_seq"),
			WipedSeq: productProp[int64](props, "wiped_seq"), ReleaseID: productOptional[string](props, "release_id"),
			ProductCount: productProp[int64](props, "product_count"),
		})
	}
	return stores, nil
}

const mentionFeatureIDs = `['catalogue_code', 'product_reference']`

const boundMentionIdentifiers = `
UNWIND ` + mentionFeatureIDs + ` AS feature
MATCH (f:FeatureRev {id:feature})<-[e:HAS_FEATURE]-(p:Product)
WHERE e.release_id=$release AND p.store IN $stores`

func txLoadMentionIdentifiers(ctx context.Context, tx neo4j.ManagedTransaction, params map[string]any) ([]ProductIdentifierObservation, error) {
	// Group by product and feature to send each title, feature name and
	// product identity once. The builder treats scalars and list items alike.
	result, err := tx.Run(ctx, boundMentionIdentifiers+`
WITH p, feature, collect(e.value_fold) AS scalars, collect(e.items_fold) AS lists
RETURN p.id AS id, p.store AS store, p.product_id AS product_id, p.domain AS domain, p.title AS title,
       feature, scalars, lists`, params)
	if err != nil {
		return nil, err
	}
	var observations []ProductIdentifierObservation
	for result.Next(ctx) {
		row := result.Record()
		product := Product{ID: getString(row, "id"), Store: getString(row, "store"),
			ProductID: getString(row, "product_id"), Domain: getString(row, "domain"), Title: getString(row, "title")}
		scalars, lists := row.Values[6].([]any), row.Values[7].([]any)
		values := make([]string, 0, len(scalars)+len(lists))
		for _, raw := range scalars {
			value, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("invalid identifier value_fold for product %q", product.ID)
			}
			values = append(values, value)
		}
		for _, raw := range lists {
			items, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("invalid identifier items_fold for product %q", product.ID)
			}
			for _, item := range items {
				if value, ok := item.(string); ok {
					values = append(values, value)
				}
			}
		}
		observations = append(observations, ProductIdentifierObservation{
			Product: product, Feature: getString(row, "feature"), ItemsFold: values,
		})
	}
	return observations, result.Err()
}

func txMentionIdentifiers(ctx context.Context, tx neo4j.ManagedTransaction, store, release string,
	observations []ProductIdentifierObservation) ([]ProductIdentifierObservation, error) {
	result, err := tx.Run(ctx, `
MATCH (p:Product {store:$store})-[e:HAS_FEATURE]->(f:FeatureRev)
WHERE e.release_id=$release AND f.id IN `+mentionFeatureIDs+`
RETURN p.id AS id, p.product_id AS product_id, p.domain AS domain, p.title AS title,
       f.id AS feature, e.value_fold AS value_fold, e.items_fold AS items_fold
ORDER BY p.id, f.id, e.value_fold, e.items_fold`, map[string]any{"store": store, "release": release})
	if err != nil {
		return nil, err
	}
	for result.Next(ctx) {
		row := result.Record()
		observation := ProductIdentifierObservation{
			Product: Product{ID: getString(row, "id"), ProductID: getString(row, "product_id"),
				Store: store, Domain: getString(row, "domain"), Title: getString(row, "title")},
			Feature: getString(row, "feature"), ItemsFold: getStringSlice(row, "items_fold"),
		}
		value, null, err := neo4j.GetRecordValue[string](row, "value_fold")
		if err != nil {
			return nil, err
		}
		if !null {
			observation.ValueFold = &value
		}
		observations = append(observations, observation)
	}
	return observations, result.Err()
}

func txMentionDocuments(ctx context.Context, tx neo4j.ManagedTransaction, selection ProductMentionSelection) ([]Document, error) {
	const columns = `d.id AS id, d.source AS source, d.title AS title, d.hash AS hash,
       d.domain AS domain, d.status AS status ORDER BY d.source, d.id`
	var query string
	params := map[string]any{}
	switch {
	case len(selection.DocumentIDs) > 0 && selection.Domain == "" && selection.SourcePrefix == "":
		query = `UNWIND $ids AS id WITH DISTINCT id
OPTIONAL MATCH (d:Document {id:id})
RETURN id AS requested, ` + columns
		params["ids"] = selection.DocumentIDs
	case len(selection.DocumentIDs) == 0 && selection.Domain != "":
		query = `MATCH (d:Document {domain:$domain, status:$done})
WHERE $prefix = '' OR d.source STARTS WITH $prefix
RETURN ` + columns
		params["domain"], params["done"], params["prefix"] = selection.Domain, string(StatusDone), selection.SourcePrefix
	default:
		return nil, fmt.Errorf("%w: select documents by ID or domain", ErrProductReadInput)
	}
	rows, err := txRows(ctx, tx, query, params)
	if err != nil {
		return nil, err
	}
	docs := make([]Document, 0, len(rows))
	for _, row := range rows {
		id, status := getString(row, "id"), Status(getString(row, "status"))
		if id == "" {
			return nil, fmt.Errorf("document %q: %w", getString(row, "requested"), ErrNotFound)
		}
		if status != StatusDone {
			return nil, fmt.Errorf("document %q (status %q): %w", id, status, ErrNotDone)
		}
		docs = append(docs, Document{
			ID: id, Status: status, Source: getString(row, "source"), Title: getString(row, "title"),
			Hash: getString(row, "hash"), Domain: getString(row, "domain"),
		})
	}
	return docs, nil
}

func txVisitMentionChunks(ctx context.Context, tx neo4j.ManagedTransaction, docs []Document, visit func(Chunk) error) error {
	// Versions with the same source share sequence order in the output.
	for len(docs) > 0 {
		ids := []string{docs[0].ID}
		for i := 1; i < len(docs) && docs[i].Source == docs[0].Source; i++ {
			ids = append(ids, docs[i].ID)
		}
		docs = docs[len(ids):]
		result, err := tx.Run(ctx, `
UNWIND $ids AS docID
MATCH (:Document {id:docID})-[:HAS_CHUNK]->(ch:Chunk)
WITH DISTINCT docID, ch
RETURN docID AS doc_id, ch.id AS id, ch.seq AS seq, ch.text AS text, ch.hash AS hash
ORDER BY ch.seq, ch.id, docID`, map[string]any{"ids": ids})
		if err != nil {
			return err
		}
		for result.Next(ctx) {
			row := result.Record()
			if err := visit(Chunk{DocID: getString(row, "doc_id"), ID: getString(row, "id"),
				Seq: getInt(row, "seq"), Text: getString(row, "text"), Hash: getString(row, "hash")}); err != nil {
				return err
			}
		}
		if err := result.Err(); err != nil {
			return err
		}
	}
	return nil
}
