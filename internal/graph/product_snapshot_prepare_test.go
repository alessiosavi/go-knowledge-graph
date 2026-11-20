package graph

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

func TestValidateProductImportTimeout(t *testing.T) {
	for _, tc := range []struct {
		seconds  string
		accepted bool
	}{
		{"0", true},
		{"0.001", false},
		{"299.999", false},
		{"300", true},
		{"600", true},
		{"-1", false},
		{"invalid", false},
	} {
		t.Run(tc.seconds, func(t *testing.T) {
			if err := validateProductImportTimeout(tc.seconds); (err == nil) != tc.accepted {
				t.Fatalf("validate timeout %q = %v, want accepted=%t", tc.seconds, err, tc.accepted)
			}
		})
	}
}

func TestScaleProductDecimal(t *testing.T) {
	for _, tc := range []struct {
		decimal string
		want    int64
		indexed bool
	}{
		{"-1.5", 0, false},
		{"+1.5", 0, false},
		{"0.000001", 1, true},
		{"9223372036855", 0, false},
		{"9223372036854.775807", math.MaxInt64, true},
	} {
		t.Run(tc.decimal, func(t *testing.T) {
			got, indexed := scaleProductDecimal(tc.decimal)
			if indexed != tc.indexed || (indexed && got != tc.want) {
				t.Fatalf("scaled %q = %d, %t; want %d, %t", tc.decimal, got, indexed, tc.want, tc.indexed)
			}
		})
	}
}

func TestProductSnapshotProjectionCaps(t *testing.T) {
	const store = "example.test"
	for _, tc := range []struct {
		name, reason       string
		attrs, rows, width int
	}{
		{"edges", "60,000 released edges", 80, 751, 1},
		{"bytes", "64 MiB", 40, 850, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys := make(map[string]any)
			attributes := make(map[string]string)
			var triples []productobs.ReleaseTriple
			for i := range tc.attrs {
				key := fmt.Sprintf("attr%d", i)
				keys[key] = map[string]any{"feature": "text", "separator": nil, "numeric_search": false,
					"source_labels": []string{key}, "dev_evidence": 5}
				attributes[key] = strings.Repeat("x", tc.width)
				triples = append(triples, productobs.ReleaseTriple{Store: store, Feature: "text", SourceKey: key})
			}
			body, err := json.Marshal(keys)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := productobs.ParseRegistry(fmt.Appendf(nil, `{
				"schema":"gkg-features/v1",
				"features":[{"id":"text","name":"Text","definition":"Text","kind":"text","cardinality":"single",
					"checkability":"unverified_text","measure":null}],
				"stores":{"example.test":{"keys":%s,"ignore":{},"deferred":[]}}
			}`, body))
			if err != nil {
				t.Fatal(err)
			}
			release, err := registry.ProjectRelease(triples)
			if err != nil {
				t.Fatal(err)
			}
			attrs, err := json.Marshal(attributes)
			if err != nil {
				t.Fatal(err)
			}
			var rows strings.Builder
			for i := range tc.rows {
				fmt.Fprintf(&rows, `{"store":"example.test","product_id":"example.test:%d","title":"", "brand":"","category":"","subcategory":"","product_type":null,"link":"","page_specs":null,"attributes":%s}`+"\n", i, attrs)
			}
			data := rows.String()
			report := ProductImportReport{SnapshotDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(data)))}
			_, err = prepareProductReplacement(t.Context(), ProductSnapshotInput{Store: store, Release: "id", Seq: 1},
				strings.NewReader(data), "domain", "rev", registry, release, &report)
			if !errors.Is(err, ErrProductData) || !strings.Contains(err.Error(), tc.reason) || report.RejectionCount != 1 {
				t.Fatalf("error = %v, rejections = %+v; want %q", err, report.Rejections, tc.reason)
			}
		})
	}
}
