package graph

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const (
	maxProductLinkRows   = 200_000
	maxProductLinkBytes  = 64 << 20
	maxDocumentURLOwners = 50

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

	urlOwners     map[string]map[string][]productURLOwner // domain -> canonical URL -> ordered owners
	pageOwners    map[string]map[string]string            // domain -> unambiguous page key -> product ID
	pageAmbiguous int
	urlOff        bool
	pageOff       bool

	products    map[string]Product // display metadata shared by the index legs
	productsOff bool

	searchRows []searchableProduct
	searchOff  bool
}

// MentionIndex returns the code leg independently of the document legs.
func (index *ProductLinkIndex) MentionIndex() (*ProductMentionIndex, error) {
	return index.code, index.codeErr
}

// ProductDisplay returns the snapshot's display row for a product.
func (index *ProductLinkIndex) ProductDisplay(id string) (Product, bool) {
	if index == nil {
		return Product{}, false
	}
	p, ok := index.products[id]
	return p, ok
}

// ProductDisplaysCapped reports whether callers need a read for display rows.
func (index *ProductLinkIndex) ProductDisplaysCapped() bool {
	return index != nil && index.productsOff
}

// WithProductDisplaysCappedForTest simulates a display cap without changing the
// cached snapshot or the availability of its other legs.
func (index *ProductLinkIndex) WithProductDisplaysCappedForTest() *ProductLinkIndex {
	capped := *index
	capped.products, capped.productsOff = nil, true
	return &capped
}

// InferredDocumentProducts contains owner IDs for the document's source and
// redirect URL, and its page key.
type InferredDocumentProducts struct {
	URL          []string
	URLTruncated bool
	Page         string
}

// ResolveDocumentProducts applies the store binding and source rules to a done
// document. The status describes an unavailable inferred leg.
func (index *ProductLinkIndex) ResolveDocumentProducts(doc DocumentProductLinks) (InferredDocumentProducts, string) {
	var owners InferredDocumentProducts
	if doc.Status != StatusDone || !allowsInferredProductPages(doc.Domain, "") {
		return owners, ""
	}
	if index == nil {
		return owners, productPageUnavailable
	}
	if !index.urlOff {
		source, final := canonicalProductPageURL(doc.Source), canonicalProductPageURL(doc.FinalURL)
		first := index.urlProductOwners(doc.Domain, source)
		var second []productURLOwner
		if final != source {
			second = index.urlProductOwners(doc.Domain, final)
		}
		owners.URL, owners.URLTruncated = firstDocumentURLOwners(first, second)
	}
	if !index.pageOff {
		owners.Page = index.pageProductOwner(doc.Domain, doc.Source)
	}
	if index.urlOff || index.pageOff {
		return owners, productPageIndexTooLarge
	}
	return owners, ""
}

// Both URL buckets are ordered by product identity. Each product has one
// canonical link, so the buckets for distinct URLs have disjoint owners.
func firstDocumentURLOwners(first, second []productURLOwner) ([]string, bool) {
	total := len(first) + len(second)
	if total == 0 {
		return nil, false
	}
	ids := make([]string, min(total, maxDocumentURLOwners))
	for i := range ids {
		if len(second) == 0 || (len(first) != 0 && compareProductURLOwners(first[0], second[0]) < 0) {
			ids[i] = first[0].id
			first = first[1:]
		} else {
			ids[i] = second[0].id
			second = second[1:]
		}
	}
	return ids, total > maxDocumentURLOwners
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

type productURLOwner struct {
	id, store, productID string
}

func compareProductURLOwners(a, b productURLOwner) int {
	return cmp.Or(cmp.Compare(a.store, b.store), cmp.Compare(a.productID, b.productID), cmp.Compare(a.id, b.id))
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
	index.urlOwners = make(map[string]map[string][]productURLOwner)
	index.pageOwners = make(map[string]map[string]string)
	index.products = make(map[string]Product)
	urlBudget, pageBudget, productsBudget := productLinkBudget{maxRows: maxRows}, productLinkBudget{maxRows: maxRows}, productLinkBudget{maxRows: maxRows}
	searchBudget := productLinkBudget{maxRows: maxRows}
	for rows.Next(ctx) {
		p := readProductLinkRow(rows.Record())
		if !index.productsOff {
			if productsBudget.add(p.linkRowBytes()) {
				index.products[p.ID] = p
			} else {
				index.products, index.productsOff = nil, true
			}
		}
		if !index.searchOff {
			index.addProductSearchRow(p, &searchBudget)
		}
		if !index.urlOff {
			if link := canonicalProductPageURL(p.Link); link != "" {
				bytes := len(p.Domain) + len(p.ID) + len(p.Store) + len(p.ProductID) + max(len(p.Link), len(link))
				if urlBudget.add(bytes) {
					if index.urlOwners[p.Domain] == nil {
						index.urlOwners[p.Domain] = make(map[string][]productURLOwner)
					}
					owner := productURLOwner{id: p.ID, store: p.Store, productID: p.ProductID}
					index.urlOwners[p.Domain][link] = append(index.urlOwners[p.Domain][link], owner)
				} else {
					index.urlOwners, index.urlOff = nil, true
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
		if index.urlOff && index.pageOff && index.productsOff && index.searchOff {
			_, err := rows.Consume(ctx)
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, links := range index.urlOwners {
		for _, owners := range links {
			slices.SortFunc(owners, compareProductURLOwners)
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
		owners[key] = "" // retain the marker so each ambiguous key is counted once
		index.pageAmbiguous++
	}
}

// The domain is the document's domain and the owner maps use the store
// binding. Callers supply only done documents.
func (index *ProductLinkIndex) urlProductOwners(domain, canonical string) []productURLOwner {
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
	if err != nil || selection.full() || !allowsInferredProductPages(domain, scope) {
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
	index, err := s.LoadProductLinkIndex(ctx)
	if err != nil {
		slog.WarnContext(ctx, "resolve product page tier", "product", id, "error", err)
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
		slog.WarnContext(ctx, "resolve product page tier", "product", id, "error", err)
		return selection, productPageUnavailable
	}
	return matched, ""
}
