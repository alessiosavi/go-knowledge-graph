package graph

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const (
	maxProductLinkRows  = 200_000
	maxProductLinkBytes = 64 << 20

	productPageIndexTooLarge = "index_too_large"
	productPageUnavailable   = "index_unavailable"

	productViaRef  = "ref"
	productViaURL  = "url"
	productViaPage = "page"
)

// ProductLinkIndex is the immutable catalogue snapshot shared by product reads.
type ProductLinkIndex struct {
	code    *ProductMentionIndex
	codeErr error

	urlOwners     map[string]map[string][]string // domain -> canonical URL -> product IDs
	pageOwners    map[string]map[string]string   // domain -> unambiguous page key -> product ID
	pageAmbiguous int
	urlOff        bool
	pageOff       bool

	products    map[string]Product // product metadata for subsequent index legs
	productsOff bool
}

type productLinkBudget struct {
	maxRows int
	rows    int
	bytes   int
}

func (b *productLinkBudget) add(bytes int) bool {
	if b.rows == b.maxRows || bytes > maxProductLinkBytes-b.bytes {
		return false
	}
	b.rows++
	b.bytes += bytes
	return true
}

const allProductLinksQuery = `
UNWIND $bindings AS binding
MATCH (p:Product {store:binding.store})
WHERE p.product_id IS NOT NULL
RETURN p.id AS id, p.store AS store, p.product_id AS product_id, binding.domain AS domain,
       p.title AS title, p.brand AS brand, p.link AS link`

func readProductLinkRow(row *neo4j.Record) Product {
	return Product{
		ID: getString(row, "id"), Store: getString(row, "store"), ProductID: getString(row, "product_id"),
		Domain: getString(row, "domain"), Title: getString(row, "title"), Brand: getString(row, "brand"),
		Link: getString(row, "link"),
	}
}

func (p Product) linkRowBytes() int {
	return len(p.ID) + len(p.Store) + len(p.ProductID) + len(p.Domain) + len(p.Title) + len(p.Brand) + len(p.Link)
}

type productURLOwnerOrder struct {
	store, productID string
}

func txLoadProductLinkOwners(ctx context.Context, tx neo4j.ManagedTransaction, stores []ProductMentionStore, index *ProductLinkIndex, maxRows int) error {
	// Use the bindings already read in this transaction.
	bindings := make([]map[string]any, len(stores))
	for i, store := range stores {
		bindings[i] = map[string]any{"store": store.Store, "domain": store.Domain}
	}
	rows, err := tx.Run(ctx, allProductLinksQuery, map[string]any{"bindings": bindings})
	if err != nil {
		return err
	}
	index.urlOwners = make(map[string]map[string][]string)
	index.pageOwners = make(map[string]map[string]string)
	index.products = make(map[string]Product)
	urlOrder := make(map[string]productURLOwnerOrder)
	urlBudget, pageBudget, productsBudget := productLinkBudget{maxRows: maxRows}, productLinkBudget{maxRows: maxRows}, productLinkBudget{maxRows: maxRows}
	for rows.Next(ctx) {
		p := readProductLinkRow(rows.Record())
		if !index.productsOff {
			if productsBudget.add(p.linkRowBytes()) {
				index.products[p.ID] = p
			} else {
				index.products, index.productsOff = nil, true
			}
		}
		if !index.urlOff {
			if link := canonicalProductPageURL(p.Link); link != "" {
				bytes := len(p.Domain) + len(p.ID) + len(p.Store) + len(p.ProductID) + max(len(p.Link), len(link))
				if urlBudget.add(bytes) {
					if index.urlOwners[p.Domain] == nil {
						index.urlOwners[p.Domain] = make(map[string][]string)
					}
					index.urlOwners[p.Domain][link] = append(index.urlOwners[p.Domain][link], p.ID)
					urlOrder[p.ID] = productURLOwnerOrder{p.Store, p.ProductID}
				} else {
					index.urlOwners, index.urlOff, urlOrder = nil, true, nil
				}
			}
		}
		if !index.pageOff {
			if key, ok := productobs.ProductPageKey(p.Link); ok {
				bytes := len(p.Domain) + len(p.ID) + max(len(p.Link), len(key))
				if pageBudget.add(bytes) {
					index.addPageOwner(p.Domain, key, p.ID)
				} else {
					index.pageOwners, index.pageOff, index.pageAmbiguous = nil, true, 0
				}
			}
		}
		if index.urlOff && index.pageOff && index.productsOff {
			_, err := rows.Consume(ctx)
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, links := range index.urlOwners {
		for _, owners := range links {
			slices.SortFunc(owners, func(a, b string) int {
				x, y := urlOrder[a], urlOrder[b]
				return cmp.Or(cmp.Compare(x.store, y.store), cmp.Compare(x.productID, y.productID), cmp.Compare(a, b))
			})
		}
	}
	for _, keys := range index.pageOwners {
		for key, owner := range keys {
			if owner == "" {
				delete(keys, key)
			}
		}
	}
	return ctx.Err()
}

func (index *ProductLinkIndex) addPageOwner(domain, key, id string) {
	owners := index.pageOwners[domain]
	if owners == nil {
		owners = make(map[string]string)
		index.pageOwners[domain] = owners
	}
	switch owner, exists := owners[key]; {
	case !exists:
		owners[key] = id
	case owner != "" && owner != id:
		owners[key] = "" // keep an ambiguity marker until the build ends
		index.pageAmbiguous++
	}
}

// The domain is the document's domain and the owner maps use the store
// binding. Callers supply only done documents.
func (index *ProductLinkIndex) urlProductOwners(domain, canonical string) []string {
	return index.urlOwners[domain][canonical]
}

func (index *ProductLinkIndex) pageProductOwner(domain, source string) string {
	key, ok := productobs.SourcePageKey(source)
	if !ok {
		return ""
	}
	return index.pageOwners[domain][key]
}

func allowsInferredProductPages(domain, scope string) bool {
	return domain != "" && (scope == "" || scope == domain)
}

func txResolveDirectProductPages(ctx context.Context, tx neo4j.ManagedTransaction, id, domain, link, scope string, selection *productPageSelection) error {
	err := txAppendProductPages(ctx, tx, productRefPagesQuery, map[string]any{"id": id, "scope": scope},
		productViaRef, "", selection)
	if err != nil || !allowsInferredProductPages(domain, scope) {
		return err
	}
	canonical := canonicalProductPageURL(link)
	if canonical == "" {
		return nil
	}
	// An exact match can also supply URL evidence for an identity already
	// selected by the refs tier.
	anchor, authorityOnly := productPageAnchor(canonical)
	return txAppendProductPages(ctx, tx, productPagesQuery, map[string]any{
		"domain": domain, "anchor": anchor, "authority_only": authorityOnly, "non_ascii_authority": nonASCIIProductPageAuthority,
	}, productViaURL, canonical, selection)
}

const productPageCandidatesQuery = productPageDocuments + `
  AND d.source CONTAINS '/' AND NOT d.source STARTS WITH '/' AND NOT d.source CONTAINS ':'
RETURN d.id AS id, d.source AS source`

const productPageDetailsQuery = `
UNWIND $ids AS id
MATCH (d:Document {id:id})
RETURN ` + productPageFields + `
ORDER BY d.updated_at DESC, d.id`

func (s *Store) resolveProductPageTier(ctx context.Context, selection productPageSelection, id, domain, link, scope string) (productPageSelection, string) {
	if selection.full() || !allowsInferredProductPages(domain, scope) {
		return selection, ""
	}
	key, ok := productobs.ProductPageKey(link)
	if !ok {
		return selection, ""
	}
	// Bound the optional tier independently of the direct read and the
	// detached index build. A subsequent request can reuse the finished index.
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	index, err := s.loadProductLinkIndex(ctx)
	if err != nil {
		return selection, productPageUnavailable
	}
	if index.pageOff {
		return selection, productPageIndexTooLarge
	}
	if index.pageOwners[domain][key] != id {
		return selection, ""
	}
	matched := productPageSelection{limit: selection.limit, pages: slices.Clone(selection.pages), seen: maps.Clone(selection.seen)}
	err = s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := tx.Run(ctx, productPageCandidatesQuery, map[string]any{"domain": domain})
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next(ctx) {
			row := rows.Record()
			if index.pageProductOwner(domain, getString(row, "source")) == id {
				ids = append(ids, getString(row, "id"))
			}
		}
		if err := rows.Err(); err != nil || len(ids) == 0 {
			return err
		}
		return txAppendProductPages(ctx, tx, productPageDetailsQuery, map[string]any{"ids": ids}, productViaPage, "", &matched)
	})
	if err != nil {
		return selection, productPageUnavailable
	}
	return matched, ""
}
