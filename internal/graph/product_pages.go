package graph

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	productPageLimit        = 3
	productPageMissingLimit = 20
)

type ProductPage struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Source    string `json:"source"`
	UpdatedAt string `json:"updated_at"`
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

const productPageURLsQuery = productPageDocuments + `
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

func txProductPages(ctx context.Context, tx neo4j.ManagedTransaction, id, domain, link, scope string) ([]ProductPage, error) {
	// Refs have priority. Stream both ordered branches through the same set
	// so only distinct page identities take a slot in the result.
	seen := make(map[productPageSource]struct{}, productPageLimit)
	pages, err := txAppendProductPages(ctx, tx, productRefPagesQuery, map[string]any{"id": id, "scope": scope},
		"", make([]ProductPage, 0, productPageLimit), seen)
	if err != nil || len(pages) == productPageLimit {
		return pages, err
	}
	canonical := canonicalProductPageURL(link)
	if domain == "" || canonical == "" || (scope != "" && scope != domain) {
		return pages, nil
	}
	anchor, authorityOnly := productPageAnchor(canonical)
	return txAppendProductPages(ctx, tx, productPagesQuery, map[string]any{
		"domain": domain, "anchor": anchor, "authority_only": authorityOnly, "non_ascii_authority": nonASCIIProductPageAuthority,
	}, canonical, pages, seen)
}

func txAppendProductPages(ctx context.Context, tx neo4j.ManagedTransaction, query string, params map[string]any,
	canonicalLink string, pages []ProductPage, seen map[productPageSource]struct{},
) ([]ProductPage, error) {
	rows, err := tx.Run(ctx, query, params)
	if err != nil {
		return nil, err
	}
	for rows.Next(ctx) {
		r := rows.Record()
		source := getString(r, "source")
		canonicalSource := canonicalProductPageURL(source)
		if canonicalLink != "" && canonicalSource != canonicalLink && canonicalProductPageURL(getString(r, "final_url")) != canonicalLink {
			continue
		}
		key := productPageSourceKey(r, source, canonicalSource)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		pages = append(pages, ProductPage{
			ID: getString(r, "id"), Title: getString(r, "title"), Source: source, UpdatedAt: getString(r, "updated_at"),
		})
		if len(pages) == productPageLimit {
			break
		}
	}
	_, err = rows.Consume(ctx)
	return pages, err
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
}

// ReportProductPages counts associations over the materialized products of
// each selected store. It reads bound-domain URL candidates once per store
// and looks up explicit refs by anchor ID.
func (s *Store) ReportProductPages(ctx context.Context, store string) ([]ProductPagesReport, error) {
	query := `MATCH (s:ProductStore) WHERE s.domain IS NOT NULL RETURN s.store AS store, s.domain AS domain ORDER BY s.store`
	if store != "" {
		query = `MATCH (s:ProductStore {store:$store}) WHERE s.domain IS NOT NULL RETURN s.store AS store, s.domain AS domain`
	}
	reports := []ProductPagesReport{}
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		stores, err := txRows(ctx, tx, query, map[string]any{"store": store})
		if err != nil {
			return err
		}
		if store != "" && len(stores) == 0 {
			return fmt.Errorf("product store %q: %w", store, ErrNotFound)
		}
		for _, row := range stores {
			report, err := txProductPagesReport(ctx, tx, getString(row, "store"), getString(row, "domain"))
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

func txProductPagesReport(ctx context.Context, tx neo4j.ManagedTransaction, store, domain string) (ProductPagesReport, error) {
	report := ProductPagesReport{Store: store, Domain: domain, MissingProductIDs: []string{}}
	products, err := txRows(ctx, tx, productsForPagesReportQuery, map[string]any{"store": store})
	if err != nil {
		return report, err
	}
	links := make([]string, len(products))
	ids := make([]string, len(products))
	urls := make(map[string]productPageSources, len(products))
	for i, product := range products {
		ids[i] = getString(product, "id")
		links[i] = canonicalProductPageURL(getString(product, "link"))
		if links[i] != "" {
			urls[links[i]] = nil
		}
	}
	if domain != "" && len(urls) > 0 {
		if err := txCountProductPages(ctx, tx, domain, urls); err != nil {
			return report, err
		}
	}
	refs, err := txProductPageRefs(ctx, tx, ids)
	if err != nil {
		return report, err
	}
	for i, id := range ids {
		refPages, urlPages := refs[id], urls[links[i]]
		report.Refs += len(refPages)
		report.URL += len(urlPages)
		count := len(refPages) + len(urlPages)
		for key := range refPages {
			if _, ok := urlPages[key]; ok {
				count--
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

func txCountProductPages(ctx context.Context, tx neo4j.ManagedTransaction, domain string, urls map[string]productPageSources) error {
	rows, err := tx.Run(ctx, productPageURLsQuery, map[string]any{"domain": domain})
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
			if _, ok := urls[link]; ok {
				addProductPageSource(urls, link, key)
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
