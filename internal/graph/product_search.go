package graph

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const (
	// MaxProductSearchLimit is the largest accepted find_products page size.
	MaxProductSearchLimit = 50
	maxProductEvidence    = 5
	// Leave room for the REST newline and the MCP result envelope.
	maxProductSearchBytes = (1 << 20) - 512
)

var errProductSearchEnvelopeBudget = errors.New("product search metadata exceeds the search response budget")

type ProductSearchRequest struct {
	Domain  string  `json:"domain,omitempty" jsonschema:"Restrict matches to this domain ID"`
	Feature string  `json:"feature" jsonschema:"Feature ID from list_features"`
	Value   *string `json:"value,omitempty" jsonschema:"Exact folded value or item. Use either value or numeric bounds"`
	Min     *string `json:"min,omitempty" jsonschema:"Inclusive lower bound as a decimal string in the canonical unit"`
	Max     *string `json:"max,omitempty" jsonschema:"Inclusive upper bound as a decimal string in the canonical unit"`
	Limit   *int    `json:"limit,omitempty" jsonschema:"Page size from 1 to 50 (default 20)"`
	Cursor  string  `json:"cursor,omitempty" jsonschema:"Opaque continuation cursor from the previous page with the same filters and limit"`
}

const ProductSearchDescription = "Find products by a feature ID and either an exact folded value or inclusive numeric bounds in the canonical unit. " +
	"Pass the same filters with the next cursor."

type ProductHit struct {
	Product           Product           `json:"product"`
	Evidence          []ProductEvidence `json:"evidence"`
	EvidenceTruncated bool              `json:"evidence_truncated"`
}

type ProductSearchResult struct {
	Hits                  []ProductHit `json:"hits"`
	Cursor                string       `json:"cursor,omitempty"`
	NumericExcludedStores []string     `json:"numeric_excluded_stores,omitempty" jsonschema:"Stores with released keys for this feature but no numeric-enabled key in a global numeric search"`
}

type productCursor struct {
	Release  string `json:"release"`
	Snapshot string `json:"snapshot"`
	Filters  string `json:"filters"`
	LastID   string `json:"last_id"`
}

func decodeProductCursor(encoded string) (productCursor, error) {
	var c productCursor
	if encoded == "" {
		return c, nil
	}
	if len(encoded) <= 1024 {
		data, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
		if err == nil && json.Unmarshal(data, &c) == nil && validProductCursorHex(c.Release, 64) &&
			validProductCursorHex(c.Snapshot, 64) && validProductCursorHex(c.Filters, 64) && validProductCursorHex(c.LastID, 32) {
			return c, nil
		}
	}
	return productCursor{}, fmt.Errorf("%w: malformed cursor", ErrProductReadInput)
}

func validProductCursorHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (c productCursor) encode() string {
	data, _ := json.Marshal(c) // Only strings.
	return base64.RawURLEncoding.EncodeToString(data)
}

var productSearchDecimal = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)$`)

const scopedProductStoresQuery = productStoresMatch + `
  AND ($domain = '' OR s.domain = $domain)` + productStoresReturn

func parseProductBound(field string, raw *string) (*int64, error) {
	if raw == nil {
		return nil, nil
	}
	scaled, ok := scaleProductDecimal(*raw)
	if !ok || !productSearchDecimal.MatchString(*raw) {
		return nil, fmt.Errorf("%w: %s must be a non-negative decimal in range with at most six fractional digits", ErrProductReadInput, field)
	}
	return &scaled, nil
}

// FindProducts searches with the default product response budget.
func (s *Store) FindProducts(ctx context.Context, req ProductSearchRequest) (ProductSearchResult, error) {
	return s.FindProductsBudget(ctx, req, maxProductSearchBytes)
}

// FindProductsBudget caps the encoded result at maxBytes. It uses one read
// snapshot for the release, store watermarks and matching edges.
func (s *Store) FindProductsBudget(ctx context.Context, req ProductSearchRequest, maxBytes int) (ProductSearchResult, error) {
	var out ProductSearchResult
	numeric := req.Min != nil || req.Max != nil
	if req.Feature == "" || (req.Value != nil) == numeric {
		return out, fmt.Errorf("%w: provide feature and either value or at least one numeric bound", ErrProductReadInput)
	}
	limit := 20
	if req.Limit != nil {
		limit = *req.Limit
		if limit < 1 || limit > MaxProductSearchLimit {
			return out, fmt.Errorf("%w: limit must be between 1 and %d", ErrProductReadInput, MaxProductSearchLimit)
		}
	}
	lo, err := parseProductBound("min", req.Min)
	if err != nil {
		return out, err
	}
	hi, err := parseProductBound("max", req.Max)
	if err != nil {
		return out, err
	}
	if lo != nil && hi != nil && *lo > *hi {
		return out, fmt.Errorf("%w: min exceeds max", ErrProductReadInput)
	}
	cursor, err := decodeProductCursor(req.Cursor)
	if err != nil {
		return out, err
	}
	filters, _ := json.Marshal(struct {
		Domain, Feature string
		Value, Min, Max *string
		Limit           int
	}{req.Domain, req.Feature, req.Value, req.Min, req.Max, limit})
	filterKey := productDigest(filters)
	if req.Cursor != "" && cursor.Filters != filterKey {
		return out, ErrProductCursor
	}
	if maxBytes < len(`{"hits":[]}`) {
		return out, errProductSearchEnvelopeBudget
	}

	err = s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		out = ProductSearchResult{Hits: []ProductHit{}}
		rows, err := txRows(ctx, tx, scopedProductStoresQuery, map[string]any{"domain": req.Domain})
		if err != nil {
			return err
		}
		stores, activation, err := readProductStores(rows)
		if err != nil {
			return err
		}
		if stores.ActiveReleaseID == nil {
			if req.Cursor != "" {
				return ErrProductCursor
			}
			return nil
		}
		releaseID := *stores.ActiveReleaseID
		// Hash the ordered scoped watermark set. In addition to snapshot_seq,
		// include seen_seq (no-op imports) and wiped_seq (wipes keep snapshot_seq).
		data, _ := json.Marshal(struct {
			Domain     string
			Activation int64
			Stores     []ProductStoreWatermark
		}{req.Domain, activation, stores.Stores})
		snapshot := productDigest(data)
		// Use a fixed-width digest of the release ID in the opaque cursor.
		releaseKey := productDigest([]byte(releaseID))
		if req.Cursor != "" && (cursor.Release != releaseKey || cursor.Snapshot != snapshot) {
			return ErrProductCursor
		}

		manifest, registry, err := txProductReadRegistry(ctx, tx, releaseID)
		if err != nil {
			return err
		}
		kind, supportsNumeric, excludedStores := productSearchPolicy(req, manifest, registry)
		if kind == "" {
			return nil
		}
		if numeric && !supportsNumeric {
			return ErrProductNotNumeric
		}
		params := map[string]any{
			"feature": manifest.RegistryRev + "/" + req.Feature,
			"release": releaseID, "domain": req.Domain, "after": cursor.LastID,
			"limit": limit + 1, "edge_limit": maxProductEvidence + 1,
		}
		predicate := `(e.value_fold = $value OR $value IN e.items_fold)`
		if numeric {
			predicate = `e.numeric = true AND e.lo_scaled IS NOT NULL AND e.hi_scaled IS NOT NULL
				AND ($lo IS NULL OR e.lo_scaled >= $lo) AND ($hi IS NULL OR e.hi_scaled <= $hi)`
			// Bolt parameters must contain scalar values or nil.
			params["lo"], params["hi"] = nil, nil
			if lo != nil {
				params["lo"] = *lo
			}
			if hi != nil {
				params["hi"] = *hi
			}
		} else {
			params["value"] = productobs.Fold(kind, *req.Value)
		}
		hits, more, err := txProductSearchHits(ctx, tx, predicate, params, limit)
		if err != nil {
			return err
		}
		out, err = budgetProductSearch(hits, more, productCursor{Release: releaseKey, Snapshot: snapshot, Filters: filterKey}, excludedStores, maxBytes)
		return err
	})
	return out, err
}

func productSearchPolicy(req ProductSearchRequest, manifest productManifest, registry *productobs.Registry) (kind string, numeric bool, excludedStores []string) {
	var numericByStore map[string]bool
	if req.Domain == "" && req.Value == nil {
		numericByStore = make(map[string]bool)
	}
	for _, triple := range manifest.Triples {
		if triple.Feature != req.Feature || (req.Domain != "" && manifest.StoreDomains[triple.Store] != req.Domain) {
			continue
		}
		if mapping, ok := registry.Lookup(triple.Store, triple.SourceKey); ok {
			kind = mapping.Kind
			numeric = numeric || mapping.NumericSearch
			if numericByStore != nil {
				numericByStore[triple.Store] = numericByStore[triple.Store] || mapping.NumericSearch
			}
		}
	}
	for store, enabled := range numericByStore {
		if !enabled {
			excludedStores = append(excludedStores, store)
		}
	}
	slices.Sort(excludedStores)
	return kind, numeric, excludedStores
}

func txProductSearchHits(ctx context.Context, tx neo4j.ManagedTransaction, predicate string, params map[string]any, limit int) ([]ProductHit, bool, error) {
	predicate = "e.release_id = $release AND " + predicate
	rows, err := txRows(ctx, tx, `
MATCH (f:FeatureRev {key:$feature})<-[e:HAS_FEATURE]-(p:Product)
WHERE p.id > $after AND ($domain = '' OR p.domain = $domain) AND `+predicate+`
WITH DISTINCT p, f ORDER BY p.id LIMIT $limit
CALL {
    WITH p, f
    MATCH (p)-[fe:HAS_FEATURE]->(f)
    WHERE `+strings.ReplaceAll(predicate, "e.", "fe.")+`
    WITH fe ORDER BY fe.source_key, fe.raw LIMIT $edge_limit
    RETURN collect(properties(fe)) AS evidence
}
RETURN properties(p) AS product, properties(f) AS feature, evidence
ORDER BY p.id`, params)
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > limit
	rows = rows[:min(len(rows), limit)]
	hits := make([]ProductHit, 0, len(rows))
	for _, row := range rows {
		p, _, err := neo4j.GetRecordValue[map[string]any](row, "product")
		if err != nil {
			return nil, false, err
		}
		f, _, err := neo4j.GetRecordValue[map[string]any](row, "feature")
		if err != nil {
			return nil, false, err
		}
		edges, _, err := neo4j.GetRecordValue[[]any](row, "evidence")
		if err != nil {
			return nil, false, err
		}
		feature := readProductFeatureRev(f)
		hit := ProductHit{Product: readProduct(p), Evidence: make([]ProductEvidence, 0, len(edges)),
			EvidenceTruncated: len(edges) > maxProductEvidence}
		for _, edge := range edges {
			props, ok := edge.(map[string]any)
			if !ok {
				return nil, false, fmt.Errorf("unexpected product evidence properties %T", edge)
			}
			evidence, err := readProductEvidence(feature, props)
			if err != nil {
				return nil, false, err
			}
			hit.Evidence = append(hit.Evidence, evidence)
		}
		slices.SortFunc(hit.Evidence, func(a, b ProductEvidence) int {
			if order := strings.Compare(a.SourceKey, b.SourceKey); order != 0 {
				return order
			}
			return strings.Compare(a.Raw, b.Raw)
		})
		hit.Evidence = hit.Evidence[:min(maxProductEvidence, len(hit.Evidence))]
		hits = append(hits, hit)
	}
	return hits, more, nil
}

// budgetProductSearch reserves room for hit metadata first, then fills in
// evidence. A cursor points after the last emitted hit even if the byte
// budget shortened the page.
func budgetProductSearch(hits []ProductHit, more bool, cursor productCursor, excludedStores []string, maxBytes int) (ProductSearchResult, error) {
	out := ProductSearchResult{Hits: make([]ProductHit, 0, len(hits)), NumericExcludedStores: excludedStores}
	// Reserve a cursor even if this page ends up being the last. Product IDs
	// have fixed length, so the final ID does not change the encoded size.
	if len(hits) > 0 {
		cursor.LastID = hits[0].Product.ID
		out.Cursor = cursor.encode()
	}
	envelope, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	out.Cursor = ""
	if maxBytes < len(envelope) {
		return out, errProductSearchEnvelopeBudget
	}
	remaining := maxBytes - len(envelope)
	if len(hits) == 0 {
		return out, nil
	}
	for _, hit := range hits {
		base := ProductHit{Product: hit.Product, Evidence: []ProductEvidence{}}
		wire, err := json.Marshal(base)
		if err != nil {
			return out, err
		}
		// One byte for the separator, and false is longer than true.
		if len(wire)+1 > remaining {
			more = true
			break
		}
		remaining -= len(wire) + 1
		out.Hits = append(out.Hits, base)
	}
	if len(out.Hits) == 0 {
		return out, fmt.Errorf("product metadata exceeds the search response budget")
	}
	for i := range out.Hits {
		hit, source := &out.Hits[i], hits[i]
		for _, edge := range source.Evidence {
			wire, err := json.Marshal(edge)
			if err != nil {
				return out, err
			}
			if len(wire)+1 > remaining {
				break
			}
			remaining -= len(wire) + 1
			hit.Evidence = append(hit.Evidence, edge)
		}
		hit.EvidenceTruncated = source.EvidenceTruncated || len(hit.Evidence) < len(source.Evidence)
	}
	if more {
		cursor.LastID = out.Hits[len(out.Hits)-1].Product.ID
		out.Cursor = cursor.encode()
	}
	return out, nil
}
