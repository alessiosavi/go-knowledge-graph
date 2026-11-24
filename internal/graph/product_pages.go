package graph

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	productPageLimit        = 3
	productPageMissingLimit = 20
)

type ProductPage struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Source     string   `json:"source"`
	UpdatedAt  string   `json:"updated_at"`
	Via        []string `json:"via"`
	ChunkIDs   []string `json:"chunk_ids"`
	ChunkCount int      `json:"chunk_count"`
}

// canonicalProductPageURL accepts absolute URLs with a host. It lowercases
// scheme and host, drops the fragment, and strips trailing path slashes.
// It preserves explicit ports, user info, path bytes and the raw query.
func canonicalProductPageURL(raw string) string {
	link, _, _ := strings.Cut(raw, "#")
	u, err := url.Parse(link)
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return ""
	}
	scheme, rest, _ := strings.Cut(link, "://")
	end := strings.IndexAny(rest, "/?")
	if end < 0 {
		end = len(rest)
	}
	authority, tail := rest[:end], rest[end:]
	start := strings.LastIndexByte(authority, '@') + 1
	path, _, _ := strings.Cut(tail, "?")
	return strings.ToLower(scheme) + "://" + authority[:start] + strings.ToLower(authority[start:]) +
		strings.TrimRight(path, "/") + tail[len(path):]
}

// Use the longer unchanged component as an anchor because extra slashes can
// separate the path from the query. With neither, prefilter by the authority.
func productPageAnchor(link string) (anchor string, authorityOnly bool) {
	_, rest, _ := strings.Cut(link, "://")
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		tail := rest[i:]
		path, _, _ := strings.Cut(tail, "?")
		if query := tail[len(path):]; len(query) > len(path) {
			return query, false
		}
		return path, false
	}
	return "://" + strings.ToLower(rest), true
}

// Use the existing Document(domain) index to bound work to the store's
// domain. Neither raw URL property is a canonical key on existing nodes.
const productPageDocuments = `
MATCH (d:Document {domain:$domain})
WHERE d.status = 'done'`

const productPageRefDocuments = `
WITH a MATCH (a)<-[:REFS]-(d:Document)
WHERE d.status = 'done'`

const productPageFields = `d.id AS id, d.title AS title, d.source AS source, d.source_kind AS source_kind,
       d.final_url AS final_url, d.updated_at AS updated_at`

const productRefPagesQuery = `
MATCH (a:ProductRef {id:$id})` + productPageRefDocuments + `
  AND ($scope = '' OR d.domain = $scope)
RETURN ` + productPageFields + `
ORDER BY d.updated_at DESC, d.id`

// Memgraph's toLower only folds ASCII. Keep non-ASCII authorities as
// candidates as well, then apply the same exact canonical match in Go.
const nonASCIIProductPageAuthority = `[^:/?#]+://[^/?#]*[^ -~][\s\S]*`

// The candidate scan costs O(done documents in the bound domain) per product
// read. A canonical URL property set at ingest is the post-v1 path.
const productPagesQuery = productPageDocuments + `
  AND CASE WHEN $authority_only THEN
    toLower(d.source) CONTAINS $anchor OR toLower(d.final_url) CONTAINS $anchor
      OR d.source =~ $non_ascii_authority OR d.final_url =~ $non_ascii_authority
  ELSE d.source CONTAINS $anchor OR d.final_url CONTAINS $anchor END
RETURN ` + productPageFields + `
ORDER BY d.updated_at DESC, d.id`

const productPagesReportDocumentsQuery = productPageDocuments + `
RETURN d.source AS source, d.source_kind AS source_kind, d.final_url AS final_url`

type productPageSource struct {
	kind   string
	source string
}

func productPageSourceKey(r *neo4j.Record, source, canonicalSource string) productPageSource {
	if canonicalSource == "" {
		// A redirect may match even when its stored source is an opaque ID.
		canonicalSource = source
	}
	return productPageSource{kind: getString(r, "source_kind"), source: canonicalSource}
}

type productPageSelection struct {
	limit int // zero means uncapped
	pages []ProductPage
	seen  map[productPageSource]int
}

func newProductPageSelection(limit int) productPageSelection {
	return productPageSelection{
		limit: limit, pages: make([]ProductPage, 0, limit), seen: make(map[productPageSource]int, limit),
	}
}

func (s *productPageSelection) full() bool {
	return s.limit > 0 && len(s.pages) == s.limit
}

func (s *productPageSelection) add(row *neo4j.Record, via, canonicalLink string) {
	source := getString(row, "source")
	canonicalSource := canonicalProductPageURL(source)
	if canonicalLink != "" && canonicalSource != canonicalLink && canonicalProductPageURL(getString(row, "final_url")) != canonicalLink {
		return
	}
	key := productPageSourceKey(row, source, canonicalSource)
	if i, ok := s.seen[key]; ok {
		evidence := s.pages[i].Via
		if evidence[len(evidence)-1] != via {
			s.pages[i].Via = append(slices.Clip(evidence), via)
		}
		return
	}
	if s.full() {
		return
	}
	s.seen[key] = len(s.pages)
	s.pages = append(s.pages, ProductPage{
		ID: getString(row, "id"), Title: getString(row, "title"), Source: source,
		UpdatedAt: getString(row, "updated_at"), Via: []string{via}, ChunkIDs: []string{},
	})
}

func txAppendProductPages(ctx context.Context, tx neo4j.ManagedTransaction, query string, params map[string]any,
	via, canonicalLink string, selection *productPageSelection,
) error {
	rows, err := tx.Run(ctx, query, params)
	if err != nil {
		return err
	}
	// Only identities selected by a higher tier can gain evidence. Once
	// full, finish walking this tier only if such identities exist.
	checkOverlap := len(selection.pages) > 0
	for rows.Next(ctx) {
		selection.add(rows.Record(), via, canonicalLink)
		if selection.full() && !checkOverlap {
			break
		}
	}
	_, err = rows.Consume(ctx)
	return err
}

const productPageChunksQuery = `
UNWIND $ids AS id
MATCH (d:Document {id:id})
OPTIONAL MATCH (d)-[:HAS_CHUNK]->(c:Chunk)
WITH DISTINCT id, c
ORDER BY c.seq, c.id
RETURN id, count(c) AS chunk_count, collect(c.id)[..3] AS chunk_ids`

func (s *Store) loadProductPageChunks(ctx context.Context, pages []ProductPage) error {
	if len(pages) == 0 {
		return nil
	}
	ids := make([]string, len(pages))
	for i, page := range pages {
		ids[i] = page.ID
	}
	return s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := txRows(ctx, tx, productPageChunksQuery, map[string]any{"ids": ids})
		if err != nil {
			return err
		}
		byID := make(map[string]*neo4j.Record, len(rows))
		for _, row := range rows {
			byID[getString(row, "id")] = row
		}
		for i := range pages {
			if row := byID[pages[i].ID]; row != nil {
				pages[i].ChunkCount = getInt(row, "chunk_count")
				pages[i].ChunkIDs = append(pages[i].ChunkIDs, getStringSlice(row, "chunk_ids")...)
			}
		}
		return nil
	})
}

type ProductPagesReport struct {
	Store             string   `json:"store"`
	Domain            string   `json:"domain"`
	Zero              int      `json:"zero"`
	One               int      `json:"one"`
	Multiple          int      `json:"multiple"`
	MissingProductIDs []string `json:"missing_product_ids"`
	Total             int      `json:"total"`
	Refs              int      `json:"refs"`
	URL               int      `json:"url"`
	Page              int      `json:"page"`
}

// ReportProductPages counts the uncapped union of page associations per
// product. The report and its inferred owners use the same read snapshot.
func (s *Store) ReportProductPages(ctx context.Context, store string) ([]ProductPagesReport, error) {
	reports := []ProductPagesReport{}
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		stores, err := txMentionStores(ctx, tx)
		if err != nil {
			return err
		}
		selected := stores
		if store != "" {
			selected = nil
			for _, bound := range stores {
				if bound.Store == store {
					selected = []ProductMentionStore{bound}
					break
				}
			}
			if len(selected) == 0 {
				return fmt.Errorf("product store %q: %w", store, ErrNotFound)
			}
		}
		if len(selected) == 0 {
			return nil
		}
		// Include all bound owners when testing uniqueness, even for a
		// single-store report.
		index := &ProductLinkIndex{}
		if err := txLoadProductLinkOwners(ctx, tx, stores, index, maxProductLinkRows); err != nil {
			return err
		}
		if index.urlOff || index.pageOff {
			return fmt.Errorf("product pages report: %s (url_off=%t, page_off=%t)", productPageIndexTooLarge, index.urlOff, index.pageOff)
		}
		urlsByDomain := make(map[string]map[string]productPageSources)
		pages := make(map[string]productPageSources)
		for _, bound := range selected {
			domain := bound.Domain
			urls := urlsByDomain[domain]
			if urls == nil && (len(index.urlOwners[domain]) != 0 || len(index.pageOwners[domain]) != 0) {
				urls = make(map[string]productPageSources)
				if err := txCountProductPages(ctx, tx, domain, index, urls, pages); err != nil {
					return err
				}
				urlsByDomain[domain] = urls
			}
			report, err := txProductPagesReport(ctx, tx, bound.Store, domain, urls, pages)
			if err != nil {
				return err
			}
			reports = append(reports, report)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reports, nil
}

const productsForPagesReportQuery = `
MATCH (p:Product {store:$store})
WHERE p.product_id IS NOT NULL
RETURN p.id AS id, p.link AS link ORDER BY p.id`

const productPageRefsReportQuery = `
UNWIND $ids AS id
MATCH (a:ProductRef {id:id})` + productPageRefDocuments + `
RETURN a.id AS id, d.source AS source, d.source_kind AS source_kind`

type productPageSources map[productPageSource]struct{}

func addProductPageSource(sets map[string]productPageSources, id string, key productPageSource) {
	if sets[id] == nil {
		sets[id] = make(productPageSources)
	}
	sets[id][key] = struct{}{}
}

func txProductPagesReport(ctx context.Context, tx neo4j.ManagedTransaction, store, domain string, urls, pages map[string]productPageSources) (ProductPagesReport, error) {
	report := ProductPagesReport{Store: store, Domain: domain, MissingProductIDs: []string{}}
	products, err := txRows(ctx, tx, productsForPagesReportQuery, map[string]any{"store": store})
	if err != nil {
		return report, err
	}
	ids := make([]string, len(products))
	for i, product := range products {
		ids[i] = getString(product, "id")
	}
	refs, err := txProductPageRefs(ctx, tx, ids)
	if err != nil {
		return report, err
	}
	for i, id := range ids {
		link := canonicalProductPageURL(getString(products[i], "link"))
		refPages, urlPages, keyPages := refs[id], urls[link], pages[id]
		report.Refs += len(refPages)
		report.URL += len(urlPages)
		report.Page += len(keyPages)
		count := len(refPages)
		for key := range urlPages {
			if _, ok := refPages[key]; !ok {
				count++
			}
		}
		for key := range keyPages {
			_, inRefs := refPages[key]
			_, inURLs := urlPages[key]
			if !inRefs && !inURLs {
				count++
			}
		}
		report.Total += count
		switch count {
		case 0:
			report.Zero++
			if len(report.MissingProductIDs) < productPageMissingLimit {
				report.MissingProductIDs = append(report.MissingProductIDs, id)
			}
		case 1:
			report.One++
		default:
			report.Multiple++
		}
	}
	return report, nil
}

func txCountProductPages(ctx context.Context, tx neo4j.ManagedTransaction, domain string, index *ProductLinkIndex, urls, pages map[string]productPageSources) error {
	rows, err := tx.Run(ctx, productPagesReportDocumentsQuery, map[string]any{"domain": domain})
	if err != nil {
		return err
	}
	for rows.Next(ctx) {
		r := rows.Record()
		source := getString(r, "source")
		canonicalSource := canonicalProductPageURL(source)
		final := canonicalProductPageURL(getString(r, "final_url"))
		key := productPageSourceKey(r, source, canonicalSource)
		for _, link := range [...]string{canonicalSource, final} {
			if len(index.urlProductOwners(domain, link)) > 0 {
				// Shared URL owners use the same source set without
				// materializing a copy for every product.
				addProductPageSource(urls, link, key)
			}
		}
		if id := index.pageProductOwner(domain, source); id != "" {
			addProductPageSource(pages, id, key)
		}
	}
	return rows.Err()
}

func txProductPageRefs(ctx context.Context, tx neo4j.ManagedTransaction, ids []string) (map[string]productPageSources, error) {
	refs := make(map[string]productPageSources)
	if len(ids) == 0 {
		return refs, nil
	}
	rows, err := tx.Run(ctx, productPageRefsReportQuery, map[string]any{"ids": ids})
	if err != nil {
		return nil, err
	}
	for rows.Next(ctx) {
		r := rows.Record()
		source := getString(r, "source")
		addProductPageSource(refs, getString(r, "id"), productPageSourceKey(r, source, canonicalProductPageURL(source)))
	}
	return refs, rows.Err()
}
