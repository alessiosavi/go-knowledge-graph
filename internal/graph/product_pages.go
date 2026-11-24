package graph

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	productPageLimit        = 3
	productPageChunkLimit   = 3
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
MATCH (d)-[:HAS_CHUNK]->(c:Chunk)
WITH DISTINCT id, c
RETURN id, c.id AS chunk_id, c.seq AS seq`

func (s *Store) loadProductPageChunks(ctx context.Context, pages []ProductPage) error {
	if len(pages) == 0 {
		return nil
	}
	ids := make([]string, len(pages))
	for i, page := range pages {
		ids[i] = page.ID
	}
	return s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := tx.Run(ctx, productPageChunksQuery, map[string]any{"ids": ids})
		if err != nil {
			return err
		}
		type chunkOrder struct {
			id  string
			seq int
		}
		type pageChunks struct {
			count int
			first []chunkOrder
		}
		byID := make(map[string]*pageChunks, len(pages))
		for _, id := range ids {
			byID[id] = &pageChunks{}
		}
		for rows.Next(ctx) {
			row := rows.Record()
			chunks := byID[getString(row, "id")]
			chunks.count++
			if id := getString(row, "chunk_id"); id != "" {
				chunks.first = append(chunks.first, chunkOrder{id: id, seq: getInt(row, "seq")})
				slices.SortFunc(chunks.first, func(a, b chunkOrder) int {
					return cmp.Or(cmp.Compare(a.seq, b.seq), cmp.Compare(a.id, b.id))
				})
				chunks.first = chunks.first[:min(len(chunks.first), productPageChunkLimit)]
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range pages {
			chunks := byID[pages[i].ID]
			pages[i].ChunkCount = chunks.count
			pages[i].ChunkIDs = pages[i].ChunkIDs[:0]
			for _, chunk := range chunks.first {
				pages[i].ChunkIDs = append(pages[i].ChunkIDs, chunk.id)
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
	Page              *int     `json:"page"`
	PageStatus        string   `json:"page_status,omitempty"`
}

// ReportProductPages counts page associations per product, without the detail
// read's three-page limit. The report and its inferred owners share a snapshot.
func (s *Store) ReportProductPages(ctx context.Context, store string) ([]ProductPagesReport, error) {
	return s.reportProductPages(ctx, store, maxProductLinkRows)
}

func (s *Store) reportProductPages(ctx context.Context, store string, linkLimit int) ([]ProductPagesReport, error) {
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
		if err := txLoadProductLinkOwners(ctx, tx, stores, index, linkLimit); err != nil {
			return err
		}
		var pageStatus string
		if index.urlOff || index.pageOff {
			pageStatus = productPageIndexTooLarge
			index.pageOwners = nil
		}
		// Collect exact URL keys directly from the selected products so that
		// even an over-cap inferred URL index leaves exact URL counts intact.
		urlsByDomain := make(map[string]map[string]productPageSources)
		productsByStore := make(map[string][]productForPagesReport, len(selected))
		for _, bound := range selected {
			urls := urlsByDomain[bound.Domain]
			if urls == nil {
				urls = make(map[string]productPageSources)
				urlsByDomain[bound.Domain] = urls
			}
			rows, err := tx.Run(ctx, productsForPagesReportQuery, map[string]any{"store": bound.Store})
			if err != nil {
				return err
			}
			var products []productForPagesReport
			for rows.Next(ctx) {
				row := rows.Record()
				link := canonicalProductPageURL(getString(row, "link"))
				products = append(products, productForPagesReport{id: getString(row, "id"), link: link})
				if link != "" {
					urls[link] = nil
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			productsByStore[bound.Store] = products
		}
		pages := make(map[string]productPageSources)
		for domain, urls := range urlsByDomain {
			if domain != "" && (len(urls) != 0 || len(index.pageOwners[domain]) != 0) {
				if err := txCountProductPages(ctx, tx, domain, index, urls, pages); err != nil {
					return err
				}
			}
		}
		for _, bound := range selected {
			report, err := txProductPagesReport(ctx, tx, bound, productsByStore[bound.Store], urlsByDomain[bound.Domain], pages, pageStatus)
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

type productForPagesReport struct {
	id, link string
}

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

func txProductPagesReport(ctx context.Context, tx neo4j.ManagedTransaction, bound ProductMentionStore,
	products []productForPagesReport, urls, pages map[string]productPageSources, pageStatus string,
) (ProductPagesReport, error) {
	report := ProductPagesReport{Store: bound.Store, Domain: bound.Domain, MissingProductIDs: []string{}, PageStatus: pageStatus}
	if pageStatus == "" {
		report.Page = new(int)
	}
	ids := make([]string, len(products))
	for i, product := range products {
		ids[i] = product.id
	}
	refs, err := txProductPageRefs(ctx, tx, ids)
	if err != nil {
		return report, err
	}
	for _, product := range products {
		refPages, urlPages, keyPages := refs[product.id], urls[product.link], pages[product.id]
		report.Refs += len(refPages)
		report.URL += len(urlPages)
		if report.Page != nil {
			*report.Page += len(keyPages)
		}
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
				report.MissingProductIDs = append(report.MissingProductIDs, product.id)
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
	hasPageOwners := len(index.pageOwners[domain]) != 0
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
			if _, wanted := urls[link]; wanted {
				// Shared URL owners use the same source set without
				// materializing a copy for every product.
				addProductPageSource(urls, link, key)
			}
		}
		if hasPageOwners {
			if id := index.pageProductOwner(domain, source); id != "" {
				addProductPageSource(pages, id, key)
			}
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
