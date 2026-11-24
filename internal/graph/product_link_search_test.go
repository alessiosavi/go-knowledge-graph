package graph

import (
	"errors"
	"slices"
	"testing"
)

func TestProductLinkSearchRanking(t *testing.T) {
	products := []Product{
		{ID: "one", Title: "Implant", Brand: "Acme Labs"},
		{ID: "empty", Title: "--", Brand: "Implant Kit"},
		{ID: "brand-only", Title: "Polishing Disc", Brand: "Implant Kit"},
		{ID: "reversed", Title: "Kit Implant", Brand: "Acme Labs"},
		{ID: "partial", Title: "Implant Drill", Brand: "Acme Labs"},
		{ID: "lower-share", Title: "Implant Drill Kit"},
		{ID: "no-repeat", Title: "Implant Implant Drill Tray"},
		{ID: "second-store", Store: "b", ProductID: "b:1", Title: "Implant Kit"},
		{ID: "second-id", Store: "a", ProductID: "a:2", Title: "Implant Kit"},
		{ID: "exact", Store: "a", ProductID: "a:1", Title: "Ｉmplànt Kít"},
	}
	var index ProductLinkIndex
	budget := productLinkBudget{maxRows: maxProductLinkRows}
	for _, p := range products {
		index.addProductSearchRow(p, &budget)
	}
	for _, tc := range []struct {
		name, query string
		limit       int
		want        []string
	}{
		{"normalization and cutoff", "IMPLA\u0301NT—KIT", 3, []string{"exact", "second-id", "second-store"}},
		{"contiguous beats branded reordered", "Ácme Labs implant kit", 4, []string{"exact", "second-id", "second-store", "reversed"}},
		{"brand before overlap", "Acme Labs kit / versus implant", 8, []string{"reversed", "partial", "exact", "second-id", "second-store", "lower-share"}},
		{"share before length", "kit / versus implant", 8, []string{"reversed", "exact", "second-id", "second-store", "lower-share", "partial"}},
		{"brand alone", "Acme Labs", 8, nil},
		{"single-token title on exact query only", "implant", 8, []string{"one", "reversed", "exact", "second-id", "second-store", "partial"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			words := productSearchTokens(foldProductSearchText(tc.query))
			found, err := index.searchProducts(t.Context(), words, "", tc.limit)
			var ids []string
			for _, p := range found {
				ids = append(ids, p.ID)
			}
			if err != nil || !slices.Equal(ids, tc.want) {
				t.Fatalf("ids=%v err=%v, want %v", ids, err, tc.want)
			}
		})
	}
}

func TestFoldProductSearchTextKeepsNonLatinMarks(t *testing.T) {
	if got := foldProductSearchText("Caffè Crème"); got != "caffe creme" {
		t.Fatalf("Latin fold = %q, want %q", got, "caffe creme")
	}
	if a, b := foldProductSearchText("किट"), foldProductSearchText("कट"); a == b {
		t.Fatalf("Devanagari marks dropped: %q == %q", a, b)
	}
	if got := productSearchTokens(foldProductSearchText("किट kit")); !slices.Equal(got, []string{"किट", "kit"}) {
		t.Fatalf("tokens = %q, want marks kept inside words", got)
	}
}

func TestProductLinkSearchUsesPrecomputedFields(t *testing.T) {
	var index ProductLinkIndex
	budget := productLinkBudget{maxRows: 2}
	index.addProductSearchRow(Product{ID: "winner", ProductID: "2", Title: "Implant Kit", Brand: "Ácme"}, &budget)
	index.addProductSearchRow(Product{ID: "other", ProductID: "1", Title: "Implant Kit"}, &budget)

	// Change the display fields of this test-owned index. The ranking must
	// still use the title tokens, brand tokens and length captured at build.
	index.searchRows[0].product.Title = "An unrelated and much longer display title"
	index.searchRows[0].product.Brand = ""
	words := []string{"acme", "implant", "kit"}
	for range 2 {
		found, err := index.searchProducts(t.Context(), words, "", 1)
		if err != nil || len(found) != 1 || found[0].ID != "winner" {
			t.Fatalf("hits=%+v err=%v, want the precomputed brand and title match", found, err)
		}
	}

	index.addProductSearchRow(Product{ID: "overflow", Title: "Another Kit"}, &budget)
	found, err := index.searchProducts(t.Context(), words, "", 1)
	if len(found) != 0 || !errors.Is(err, ErrProductLinkSearchTooLarge) {
		t.Fatalf("hits=%+v err=%v, want the capped leg to report an error", found, err)
	}
}

func TestSearchProductLinksReadsBoundDomainAndRefreshes(t *testing.T) {
	f := newPushFixture(t)
	f.snapshot(pushA, 1, "imported", pushRecord(pushOne, "Implànt Kít", "null", "{}"))
	f.query(`MATCH (p:Product {product_id:$id}) SET p.domain='push-rebound'`, map[string]any{"id": pushOne})

	assertSearch := func(query, domain string, want bool) {
		t.Helper()
		hits, err := f.store.SearchProductLinks(t.Context(), query, domain, 3)
		if err != nil {
			t.Fatal(err)
		}
		if want {
			if len(hits) != 1 || hits[0].ProductID != pushOne || hits[0].Domain != pushDomain {
				t.Fatalf("hits=%+v, want %s in bound domain %s", hits, pushOne, pushDomain)
			}
		} else if len(hits) != 0 {
			t.Fatalf("unexpected hits for %q in %q: %+v", query, domain, hits)
		}
	}
	assertSearch("implant kit", pushDomain, true)
	assertSearch("implant kit", "push-rebound", false)
	assertSearch("implant kit", "", true)

	f.put(2, pushRecord(pushOne, "Dental Tray", "null", "{}"), "updated")
	assertSearch("implant kit", pushDomain, false)
	assertSearch("dental tray", pushDomain, true)
}
