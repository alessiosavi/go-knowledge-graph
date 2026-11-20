package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const (
	maxProductImportEdges      = 60000
	maxProductImportValueBytes = 1 << 10
	maxProductImportPropBytes  = 64 << 20
	maxProductReportDetails    = 1000
)

type ProductImportReport struct {
	Store                string                    `json:"store"`
	Release              string                    `json:"release_id"`
	SnapshotSeq          int64                     `json:"snapshot_seq"`
	SnapshotDigest       string                    `json:"snapshot_digest,omitempty"`
	Status               string                    `json:"status"`
	Error                string                    `json:"error,omitempty"`
	ProductCount         int                       `json:"product_count"`
	EdgeCount            int                       `json:"edge_count"`
	PropertyBytes        int                       `json:"property_bytes"`
	PreviousProductCount int                       `json:"previous_product_count"`
	AllowShrink          bool                      `json:"allow_shrink"`
	ShrinkAllowed        bool                      `json:"shrink_allowed"`
	Rejections           []ProductImportRejection  `json:"rejections"`
	RejectionCount       int                       `json:"rejection_count"`
	UnmappedKeys         map[string]int            `json:"unmapped_keys"`
	UnmappedCount        int                       `json:"unmapped_count"`
	Conflicts            int                       `json:"conflicts"`
	ParseStatus          map[string]map[string]int `json:"parse_status_by_feature"`
}

type ProductImportRejection struct {
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

func (r *ProductImportReport) Reject(line int, err error) {
	r.RejectionCount++
	if len(r.Rejections) < maxProductReportDetails {
		r.Rejections = append(r.Rejections, ProductImportRejection{Line: line, Reason: err.Error()})
	}
}

func (r *ProductImportReport) RejectionError() error {
	if r.RejectionCount == 0 {
		return nil
	}
	first := r.Rejections[0]
	return fmt.Errorf("%w: %d records rejected (line %d: %s)", ErrProductData, r.RejectionCount, first.Line, first.Reason)
}

type productReplacement struct {
	products []map[string]any
	edges    []map[string]any
}

func prepareProductReplacement(ctx context.Context, input ProductSnapshotInput, source io.Reader, domain, rev string,
	registry *productobs.Registry, release *productobs.Release, report *ProductImportReport) (productReplacement, error) {
	var batch productReplacement
	report.UnmappedKeys = make(map[string]int)
	report.ParseStatus = make(map[string]map[string]int)
	var changedInput bool
	digest, err := productobs.ScanImportRecords(ctx, source, input.Store, func(_ int, _ error) {
		changedInput = true
	}, func(line int, record productobs.Record) error {
		// The emitter computes conflicts across mapped aliases before projecting the release.
		observations := registry.Observations(record, release)
		for _, obs := range observations {
			if len(obs.Raw) > maxProductImportValueBytes {
				report.Reject(line, errors.New("released feature value exceeds 1 KiB"))
				return nil
			}
			if len(obs.Items) > 64 {
				report.Reject(line, errors.New("feature value splits into more than 64 items"))
				return nil
			}
			for _, item := range obs.Items {
				if len(item) > maxProductImportValueBytes {
					report.Reject(line, errors.New("released feature item exceeds 1 KiB"))
					return nil
				}
			}
		}
		id := productobs.ProductID(record.Store, record.ProductID)
		props := map[string]any{"id": id, "store": record.Store, "product_id": record.ProductID, "domain": domain,
			"title": record.Title, "brand": record.Brand, "category": record.Category, "subcategory": record.Subcategory, "link": record.Link}
		if record.ProductType != nil {
			props["product_type"] = *record.ProductType
		}
		report.PropertyBytes += productPropertyBytes(props)
		for sourceKey := range record.Attributes {
			if _, mapped := registry.Lookup(record.Store, sourceKey); !mapped {
				report.UnmappedCount++
				if _, tracked := report.UnmappedKeys[sourceKey]; tracked || len(report.UnmappedKeys) < maxProductReportDetails {
					report.UnmappedKeys[sourceKey]++
				}
			}
		}
		for _, obs := range observations {
			if len(batch.edges) == maxProductImportEdges {
				report.Reject(line, errors.New("snapshot exceeds 60,000 released edges"))
				return report.RejectionError()
			}
			mapping, _ := registry.Lookup(record.Store, obs.SourceKey)
			edge := productEdgeProperties(obs, input.Release, mapping.NumericSearch)
			report.PropertyBytes += productPropertyBytes(edge)
			if report.PropertyBytes > maxProductImportPropBytes {
				report.Reject(line, errors.New("replacement properties exceed 64 MiB"))
				return report.RejectionError()
			}
			batch.edges = append(batch.edges, map[string]any{"product": id, "feature": rev + "/" + obs.Feature, "props": edge})
			report.EdgeCount++
			if obs.Conflict {
				report.Conflicts++
			}
			status := "not_applicable"
			if obs.Typed != nil {
				status = obs.Typed.Status
			}
			if report.ParseStatus[obs.Feature] == nil {
				report.ParseStatus[obs.Feature] = make(map[string]int)
			}
			report.ParseStatus[obs.Feature][status]++
		}
		if report.PropertyBytes > maxProductImportPropBytes {
			report.Reject(line, errors.New("replacement properties exceed 64 MiB"))
			return report.RejectionError()
		}
		batch.products = append(batch.products, props)
		return nil
	})
	if err != nil {
		return productReplacement{}, err
	}
	if changedInput || digest != report.SnapshotDigest {
		return productReplacement{}, errors.New("records changed after preflight")
	}
	return batch, report.RejectionError()
}

func productEdgeProperties(obs productobs.Observation, release string, numeric bool) map[string]any {
	props := map[string]any{"source_key": obs.SourceKey, "raw": obs.Raw, "split_status": obs.SplitStatus,
		"conflict": obs.Conflict, "release_id": release, "numeric": numeric}
	if obs.Items == nil {
		props["value_fold"] = obs.ValueFold()
	} else {
		// Items are validated strings, so this encoding cannot fail.
		items, _ := json.Marshal(obs.Items)
		props["items_json"] = string(items)
		props["items_fold"] = obs.ItemsFolds()
	}
	if obs.Typed == nil {
		return props
	}
	typed := obs.Typed
	props["status"] = typed.Status
	for key, value := range map[string]*string{
		"lo": typed.Lo, "hi": typed.Hi, "lo_canonical": typed.LoCanonical, "hi_canonical": typed.HiCanonical,
		"qualifier": typed.Qualifier, "unit_in": typed.UnitIn, "canonical_unit": typed.CanonicalUnit,
	} {
		if value != nil {
			props[key] = *value
		}
	}
	if typed.Inclusive != nil {
		props["inclusive"] = *typed.Inclusive
	}
	if numeric && typed.Status == "ok" && typed.Qualifier != nil && *typed.Qualifier == "exact" && typed.LoCanonical != nil {
		// For an exact observation the engine sets both bounds to the same value.
		if scaled, ok := scaleProductDecimal(*typed.LoCanonical); ok {
			props["lo_scaled"], props["hi_scaled"] = scaled, scaled
		}
	}
	return props
}

func scaleProductDecimal(decimal string) (int64, bool) {
	if strings.HasPrefix(decimal, "-") || strings.HasPrefix(decimal, "+") {
		return 0, false
	}
	whole, fraction, _ := strings.Cut(decimal, ".")
	if len(fraction) > 6 {
		return 0, false
	}
	n, err := strconv.ParseInt(whole+fraction+strings.Repeat("0", 6-len(fraction)), 10, 64)
	return n, err == nil
}

// Count property names plus their payloads, including both JSON and folded
// item values. Integers use eight bytes and booleans use one.
func productPropertyBytes(props map[string]any) int {
	var bytes int
	for key, value := range props {
		bytes += len(key)
		switch v := value.(type) {
		case string:
			bytes += len(v)
		case int64:
			bytes += 8
		case bool:
			bytes++
		case []string:
			for _, item := range v {
				bytes += len(item)
			}
		}
	}
	return bytes
}
