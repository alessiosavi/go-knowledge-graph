package graph

import (
	"context"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// DocumentProductLinks contains the inputs for resolving a document's product links.
type DocumentProductLinks struct {
	Status     Status
	Domain     string
	Source     string
	SourceKind string
	FinalURL   string
	Refs       []Product
}

const documentProductLinksQuery = `
UNWIND $ids AS id
WITH DISTINCT id
MATCH (d:Document {id:id})
OPTIONAL MATCH (d)-[:REFS]->(a:ProductRef) WHERE d.status = 'done'
OPTIONAL MATCH (p:Product {id:a.id})
RETURN id, d.status AS status, d.domain AS domain, d.source AS source,
       d.source_kind AS source_kind, d.final_url AS final_url,
       collect(DISTINCT CASE WHEN p IS NULL THEN null ELSE
         {id:p.id, store:p.store, product_id:p.product_id, domain:p.domain, title:p.title, brand:p.brand}
       END) AS refs`

// DocumentProductLinks reads existing documents and resolves live REFS in one
// query. Missing documents have no entry. Only done documents supply refs.
func (s *Store) DocumentProductLinks(ctx context.Context, docIDs []string) (map[string]DocumentProductLinks, error) {
	if len(docIDs) == 0 {
		return nil, nil
	}
	documents := make(map[string]DocumentProductLinks, len(docIDs))
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := tx.Run(ctx, documentProductLinksQuery, map[string]any{"ids": docIDs})
		if err != nil {
			return err
		}
		for rows.Next(ctx) {
			row := rows.Record()
			doc := DocumentProductLinks{
				Status: Status(getString(row, "status")), Domain: getString(row, "domain"),
				Source: getString(row, "source"), SourceKind: getString(row, "source_kind"),
				FinalURL: getString(row, "final_url"),
			}
			refs, _, err := neo4j.GetRecordValue[[]any](row, "refs")
			if err != nil {
				return err
			}
			for _, ref := range refs {
				if props, ok := ref.(map[string]any); ok {
					doc.Refs = append(doc.Refs, Product{
						ID: productProp[string](props, "id"), Store: productProp[string](props, "store"),
						ProductID: productProp[string](props, "product_id"), Domain: productProp[string](props, "domain"),
						Title: productProp[string](props, "title"), Brand: productProp[string](props, "brand"),
					})
				}
			}
			documents[getString(row, "id")] = doc
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return documents, nil
}

const productLinkDisplaysQuery = `
UNWIND $ids AS id
MATCH (p:Product {id:id})
MATCH (s:ProductStore {store:p.store})
WHERE s.domain IS NOT NULL
RETURN p.id AS id, p.store AS store, p.product_id AS product_id, p.domain AS domain, s.domain AS store_domain,
       p.title AS title, p.brand AS brand, p.link AS link`

// ProductLinkDisplay carries the product and the store's current domain binding.
type ProductLinkDisplay struct {
	Product
	StoreDomain string
}

// ProductLinkDisplays fills in display rows for inferred owners when the
// display leg hit its cap. Only current products in bound stores are returned.
func (s *Store) ProductLinkDisplays(ctx context.Context, ids []string) (map[string]ProductLinkDisplay, error) {
	rows, err := s.runRead(ctx, productLinkDisplaysQuery, map[string]any{"ids": ids})
	if err != nil {
		return nil, err
	}
	products := make(map[string]ProductLinkDisplay, len(rows))
	for _, row := range rows {
		p := readProductLinkRow(row)
		products[p.ID] = ProductLinkDisplay{Product: p, StoreDomain: getString(row, "store_domain")}
	}
	return products, nil
}
