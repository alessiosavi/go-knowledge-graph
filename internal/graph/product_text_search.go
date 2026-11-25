package graph

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	firstProductTextCandidates = 256
	maxProductTextCandidates   = 16_384
	productTextExcerptRunes    = 200
	productTextCoverageTTL     = 30 * time.Second
)

type ProductText struct {
	Key           string `json:"key"`
	Text          string `json:"text"`
	TextTruncated bool   `json:"text_truncated"`
	Checked       bool   `json:"checked"`
}

type MatchedProductText struct {
	ProductText
	ExcerptCut bool    `json:"excerpt_cut"`
	Similarity float64 `json:"similarity"`
	TextID     string  `json:"text_id"`
}

type ProductTextHit struct {
	ID        string               `json:"id"`
	Store     string               `json:"store"`
	ProductID string               `json:"product_id"`
	Domain    string               `json:"domain"`
	Title     string               `json:"title"`
	Brand     string               `json:"brand"`
	Score     float64              `json:"score"`
	Matched   []MatchedProductText `json:"matched"`
}

type ProductTextSearchResult struct {
	Hits                  []ProductTextHit `json:"hits"`
	Exhausted             bool             `json:"exhausted"`
	CandidatesTruncated   bool             `json:"candidates_truncated"`
	StoresNotMaterialized []string         `json:"stores_not_materialized"`
	Partial               bool             `json:"-"`
}

const productTextStoresQuery = `
UNWIND $bound AS store
MATCH (s:ProductStore {store:store})
WHERE s.domain IS NOT NULL AND coalesce(s.product_count, 0) > 0 AND ($domain = '' OR s.domain = $domain)
RETURN s.store AS store, s.text_version AS version, s.text_count AS texts, s.text_edge_count AS edges
ORDER BY s.store`

const searchProductTextsQuery = `
CALL vector_search.search('text_vec', $k, $vec) YIELD node, distance
WITH node, 1.0 - distance AS score
WHERE node.space = $space AND score >= $floor
MATCH (n:ProductText {vec_id:node.vec_id})
WHERE n.store IN $stores
MATCH (p:Product)-[e:HAS_TEXT]->(n)
WHERE p.store = n.store AND ($domain = '' OR p.domain = $domain) AND NOT p.id IN $excluded
WITH p, e, n, score ORDER BY score DESC, e.key, n.vec_id
WITH p, max(score) AS score,
     collect({edge:e, node:n, similarity:score})[0..3] AS matched
ORDER BY score DESC, p.id LIMIT $limit
RETURN properties(p) AS product, score,
       [m IN matched | {key:m.edge.key, text:m.node.text, truncated:m.edge.truncated,
                        similarity:m.similarity, text_id:m.node.vec_id}] AS matched
ORDER BY score DESC, p.id`

// Bind vec_id as a symbol for the indexed lookup, then walk edges from
// the missing candidates. LIMIT stops the pipeline at the first reference.
const missingProductTextVectorsQuery = `
USING INDEX :TextVec(vec_id)
UNWIND $stores AS store
MATCH (n:ProductText {store:store})
WITH n, n.vec_id AS vecID
OPTIONAL MATCH (v:TextVec {vec_id:vecID})
WHERE v.space = $space AND v.embedding IS NOT NULL
WITH n, v WHERE v IS NULL
MATCH (n)<-[:HAS_TEXT]-(p:Product)
WHERE p.store = n.store AND ($domain = '' OR p.domain = $domain)
RETURN 1 AS missing LIMIT 1`

type productTextCoverageKey struct {
	domain string
	stores string // ordered stores and their text version, node count and edge count
}

type productTextCoverageEntry struct {
	missing   bool
	expiresAt time.Time
}

type productTextCoverageCache struct {
	mu      sync.Mutex
	entries map[productTextCoverageKey]productTextCoverageEntry
}

func (c *productTextCoverageCache) lookup(key productTextCoverageKey, now time.Time) (missing, cached bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	return entry.missing, now.Before(entry.expiresAt)
}

func (c *productTextCoverageCache) remember(key productTextCoverageKey, entry productTextCoverageEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !now.Before(entry.expiresAt) || c.entries[key].expiresAt.After(entry.expiresAt) {
		return
	}
	if c.entries == nil {
		c.entries = make(map[productTextCoverageKey]productTextCoverageEntry)
	}
	for key, old := range c.entries {
		if !now.Before(old.expiresAt) {
			delete(c.entries, key)
		}
	}
	c.entries[key] = entry
}

// SearchProductTexts ranks products by their best matching stored text.
// Joins and release membership share a snapshot. Coverage can be reused for
// up to 30 seconds while the scoped store counters match.
func (s *Store) SearchProductTexts(ctx context.Context, vec []float64, domain string, limit int, excludeIDs []string) (ProductTextSearchResult, error) {
	out := ProductTextSearchResult{Hits: []ProductTextHit{}, StoresNotMaterialized: []string{}}
	size, err := s.textVecIndexSize(ctx)
	if err != nil {
		return out, err
	}
	if size < 0 {
		return out, ErrProductTextIndexUnavailable
	}
	var coverageKey productTextCoverageKey
	var checked productTextCoverageEntry
	err = s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		manifest, err := txActiveProductManifest(ctx, tx)
		if err != nil {
			return err
		}
		bound := make([]string, 0, len(manifest.StoreDomains))
		for store, boundDomain := range manifest.StoreDomains {
			if domain == "" || boundDomain == domain {
				bound = append(bound, store)
			}
		}
		if len(bound) == 0 {
			out.Exhausted = true
			return nil
		}
		stores, err := txRows(ctx, tx, productTextStoresQuery, map[string]any{"domain": domain, "bound": bound})
		if err != nil {
			return err
		}
		materialized := make([]string, 0, len(stores))
		var fingerprint strings.Builder
		for _, row := range stores {
			store := getString(row, "store")
			version := getInt(row, "version")
			if version == productTextVersion {
				materialized = append(materialized, store)
				fmt.Fprintf(&fingerprint, "%q:%d:%d:%d;", store, version, getInt(row, "texts"), getInt(row, "edges"))
			} else {
				out.StoresNotMaterialized = append(out.StoresNotMaterialized, store)
			}
		}
		out.Partial = len(out.StoresNotMaterialized) != 0
		if len(materialized) == 0 {
			out.Exhausted = true
			return nil
		}
		params := map[string]any{
			"domain": domain, "stores": materialized, "space": ProductTextSpace,
			"vec": vec, "floor": s.productTextFloor, "limit": limit, "excluded": excludeIDs,
		}
		// The global index includes other stores, other spaces and unreferenced
		// vectors. Widen based on the distinct products surviving the joins.
		var rows []*neo4j.Record
		for k := firstProductTextCandidates; ; k = min(k*4, maxProductTextCandidates) {
			if size > 0 {
				params["k"] = k
				rows, err = txRows(ctx, tx, searchProductTextsQuery, params)
				if err != nil {
					return fmt.Errorf("search product text vectors: %w", err)
				}
			}
			out.Exhausted = k >= size
			out.CandidatesTruncated = k == maxProductTextCandidates && !out.Exhausted
			if len(rows) >= limit || out.Exhausted || out.CandidatesTruncated {
				break
			}
		}
		if !out.Partial {
			coverageKey = productTextCoverageKey{domain: domain, stores: fingerprint.String()}
			now := time.Now()
			missing, cached := s.productTextCoverage.lookup(coverageKey, now)
			if !cached {
				uncovered, err := txRows(ctx, tx, missingProductTextVectorsQuery, params)
				if err != nil {
					return fmt.Errorf("check product text coverage: %w", err)
				}
				missing = len(uncovered) != 0
				checked = productTextCoverageEntry{missing: missing, expiresAt: now.Add(productTextCoverageTTL)}
			}
			out.Partial = missing
		}
		if len(rows) == 0 {
			return nil
		}
		keys := releasedProductTextKeys(manifest)
		for _, row := range rows {
			m := row.AsMap()
			p := readProduct(productProp[map[string]any](m, "product"))
			hit := ProductTextHit{
				ID: p.ID, Store: p.Store, ProductID: p.ProductID, Domain: p.Domain, Title: p.Title, Brand: p.Brand,
				Score: productProp[float64](m, "score"), Matched: []MatchedProductText{},
			}
			for _, raw := range productProp[[]any](m, "matched") {
				props, _ := raw.(map[string]any)
				text := readProductText(props, keys[p.Store])
				runes := []rune(text.Text)
				cut := len(runes) > productTextExcerptRunes
				if cut {
					text.Text = string(runes[:productTextExcerptRunes])
				}
				hit.Matched = append(hit.Matched, MatchedProductText{
					ProductText: text, ExcerptCut: cut, TextID: productProp[string](props, "text_id"),
					Similarity: productProp[float64](props, "similarity"),
				})
			}
			out.Hits = append(out.Hits, hit)
		}
		return nil
	})
	if err == nil && !checked.expiresAt.IsZero() {
		s.productTextCoverage.remember(coverageKey, checked)
	}
	return out, err
}

func txReleasedProductTextKeys(ctx context.Context, tx neo4j.ManagedTransaction) (map[string]map[string]bool, error) {
	manifest, err := txActiveProductManifest(ctx, tx)
	if err != nil {
		return nil, err
	}
	return releasedProductTextKeys(manifest), nil
}

func txActiveProductManifest(ctx context.Context, tx neo4j.ManagedTransaction) (productManifest, error) {
	rows, err := txRows(ctx, tx, `MATCH (m:GkgMeta {id:'product_release'}) WHERE m.value IS NOT NULL RETURN m.value AS id`, nil)
	if err != nil || len(rows) == 0 {
		return productManifest{}, err
	}
	manifest, _, err := txProductManifest(ctx, tx, getString(rows[0], "id"))
	return manifest, err
}

func releasedProductTextKeys(manifest productManifest) map[string]map[string]bool {
	keys := make(map[string]map[string]bool)
	for _, triple := range manifest.Triples {
		if keys[triple.Store] == nil {
			keys[triple.Store] = make(map[string]bool)
		}
		keys[triple.Store][triple.SourceKey] = true
	}
	return keys
}

func readProductText(props map[string]any, released map[string]bool) ProductText {
	key := productProp[string](props, "key")
	return ProductText{
		Key: key, Text: productProp[string](props, "text"),
		TextTruncated: productProp[bool](props, "truncated"), Checked: released[key],
	}
}

// ValidateProductTextQuery validates the query mode before any upstream work.
func (r ProductSearchRequest) ValidateProductTextQuery(query string) (int, error) {
	if utf8.RuneCountInString(query) > 500 {
		return 0, fmt.Errorf("%w: query must be at most 500 runes after trimming", ErrProductReadInput)
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"feature", &r.Feature}, {"value", r.Value}, {"min", r.Min}, {"max", r.Max}, {"cursor", &r.Cursor}} {
		if field.value != nil && strings.TrimSpace(*field.value) != "" {
			return 0, fmt.Errorf("%w: %s cannot be combined with query", ErrProductReadInput, field.name)
		}
	}
	if r.Domain != "" && !ValidDomain(r.Domain) {
		return 0, fmt.Errorf("%w: invalid domain slug", ErrProductReadInput)
	}
	limit := 10
	if r.Limit != nil {
		limit = *r.Limit
		if limit < 1 || limit > 20 {
			return 0, fmt.Errorf("%w: query limit must be between 1 and 20", ErrProductReadInput)
		}
	}
	return limit, nil
}
