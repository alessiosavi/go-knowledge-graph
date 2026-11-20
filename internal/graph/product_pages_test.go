package graph

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

func TestCanonicalProductPageURL(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"HTTPS://ExAMPle.Test/Path///?SKU=01&color=BLUE#frag", "https://example.test/Path?SKU=01&color=BLUE"},
		{"HtTp://EXAMPLE.TEST:80/#frag", "http://example.test:80"},
		{"https://EXAMPLE.TEST:443/", "https://example.test:443"},
		{"https://EXAMPLE.TEST/Path%2f/?a=%2F+q&b=/", "https://example.test/Path%2f?a=%2F+q&b=/"},
		{"https://example.test/Path%2F/?a=%2F+q&b=/", "https://example.test/Path%2F?a=%2F+q&b=/"},
		{"http://Mixed:Case@[FE80::A]:8080///?", "http://Mixed:Case@[fe80::a]:8080?"},
		{"https://EXÄMPLE.test/Español/#%", "https://exämple.test/Español"},
		{"https://BÜCHER.test/", "https://bücher.test"},
		{"https://XN--BCHER-KVA.test/", "https://xn--bcher-kva.test"},
		{"https://EXAMPLE.TEST/a//b/", "https://example.test/a//b"},
		{"https://EXAMPLE.TEST/", "https://example.test"},
		{"https://EXAMPLE.TEST///?p=1", "https://example.test?p=1"},
		{"", ""},
		{"/product", ""},
		{"//example.test/item", ""},
		{"https:///item", ""},
		{"https://example.test/%ZZ", ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got := canonicalProductPageURL(tc.raw)
			if got != tc.want {
				t.Fatalf("canonicalProductPageURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if got == "" {
				return
			}
			anchor, authorityOnly := productPageAnchor(got)
			candidate := tc.raw
			if authorityOnly {
				candidate = strings.ToLower(candidate)
			}
			if !strings.Contains(candidate, anchor) {
				t.Fatalf("URL %q is missing candidate anchor %q", candidate, anchor)
			}
		})
	}
}

func TestProductPageBareHostAnchor(t *testing.T) {
	anchor, authorityOnly := productPageAnchor("https://example.test")
	if anchor != "://example.test" || !authorityOnly {
		t.Fatalf("bare host anchor = (%q, %t), want (\"://example.test\", true)", anchor, authorityOnly)
	}
}

func TestProductPageQueryPlans(t *testing.T) {
	uri := os.Getenv("GKG_TEST_MEMGRAPH_URI")
	if uri == "" {
		t.Skip("requires a disposable Memgraph integration database")
	}
	st, err := Open(t.Context(), config.Config{Memgraph: config.MemgraphConfig{URI: uri}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	if err := st.Bootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query string
		params      map[string]any
		indexes     []string
	}{
		{"detail", getProductQuery, map[string]any{"id": "product", "store": "", "product": ""},
			[]string{":Product {id}", ":ProductStore {store}"}},
		{"pages", productPagesQuery, map[string]any{
			"domain": "catalog", "anchor": "/item", "authority_only": false, "non_ascii_authority": nonASCIIProductPageAuthority,
		}, []string{":Document {domain}"}},
		{"pages_bare_host", productPagesQuery, map[string]any{
			"domain": "catalog", "anchor": "://example.test", "authority_only": true, "non_ascii_authority": nonASCIIProductPageAuthority,
		},
			[]string{":Document {domain}"}},
		{"report_products", productsForPagesReportQuery, map[string]any{"store": "example.test"},
			[]string{":Product {store, product_id}"}},
		{"report_pages", productPageURLsQuery, map[string]any{"domain": "catalog"},
			[]string{":Document {domain}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := st.runRead(t.Context(), "EXPLAIN "+tc.query, tc.params)
			if err != nil {
				t.Fatal(err)
			}
			var plan []string
			for _, row := range rows {
				plan = append(plan, getString(row, "QUERY PLAN"))
			}
			for _, index := range tc.indexes {
				indexed := slices.ContainsFunc(plan, func(line string) bool {
					return strings.Contains(line, "ScanAllByLabelProperties") && strings.Contains(line, index)
				})
				if !indexed {
					t.Errorf("missing indexed scan on %s in plan:\n%s", index, strings.Join(plan, "\n"))
				}
			}
		})
	}
}
