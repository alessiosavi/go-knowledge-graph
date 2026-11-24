package graph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

func pageLinkRecord(id, link string) string {
	record := pushRecord(id, "Implant kit", "null", `{}`)
	return strings.NewReplacer(`"https://push.test/p"`, fmt.Sprintf("%q", link), `"brand":""`, `"brand":"Fixture brand"`).Replace(record)
}

func addProductLinkDocument(t *testing.T, st *Store, in DocumentInput, status Status) string {
	t.Helper()
	in.Title, in.Hash = in.Source, t.Name()+"/"+in.Hash
	doc, err := st.UpsertDocument(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(context.Background(), doc.ID); err != nil {
			t.Error(err)
		}
	})
	if err := st.SetStatus(t.Context(), doc.ID, status, ""); err != nil {
		t.Fatal(err)
	}
	return doc.ID
}

func getLinkedProduct(t *testing.T, st *Store, sourceID, scope string) ProductDetail {
	t.Helper()
	out, err := st.GetProduct(t.Context(), ProductLookup{Store: pushStore, ProductID: sourceID, Domain: scope})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestProductPageArrivalOrderAndDomain(t *testing.T) {
	f := newPushFixture(t)
	const firstLink, secondLink = "https://push.test/implants/implant-kit.html", "https://push.test/implants/second-implant-kit.html"

	// Prime the index before import so the first read must see the new
	// product. Then insert the second document against an already warm index.
	if err := f.store.WarmProductLinks(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := addProductLinkDocument(t, f.store, DocumentInput{
		Kind: "file", Source: "bc-it/Implant-Kit", Domain: pushDomain,
	}, StatusDone)
	f.snapshot(pushA, 1, "imported", pageLinkRecord(pushOne, firstLink))
	assertPage := func(out ProductDetail, id string) {
		t.Helper()
		if out.PagesMissing || out.PagesPartial != "" || len(out.Pages) != 1 || out.Pages[0].ID != id ||
			!slices.Equal(out.Pages[0].Via, []string{"page"}) {
			t.Fatalf("pages=%+v missing=%t partial=%q, want page %s", out.Pages, out.PagesMissing, out.PagesPartial, id)
		}
	}
	assertPage(getLinkedProduct(t, f.store, pushOne, ""), first)

	f.put(2, pageLinkRecord(pushTwo, secondLink), "created")
	for _, in := range []DocumentInput{
		{Kind: "file", Source: "other/second-implant-kit", Domain: "push-rebound"},
		{Kind: "text", Source: "https://foreign.example/%ZZ/second-implant-kit.html", Domain: pushDomain},
		{Kind: "text", Source: "//foreign.example/second-implant-kit.html", Domain: pushDomain},
		{Kind: "text", Source: "file:///manuals/second-implant-kit.html", Domain: pushDomain},
		{Kind: "text", Source: "http:foo/bar", Domain: pushDomain},
		{Kind: "text", Source: "second-implant-kit", Domain: pushDomain},
		{Kind: "url", Source: "https://elsewhere.test/implants/second-implant-kit.html", Domain: pushDomain},
	} {
		addProductLinkDocument(t, f.store, in, StatusDone)
	}
	pending := addProductLinkDocument(t, f.store, DocumentInput{
		Kind: "file", Source: "bc-it/second-implant-kit", Domain: pushDomain,
	}, StatusReceived)
	if out := getLinkedProduct(t, f.store, pushTwo, ""); !out.PagesMissing || out.PagesPartial != "" || len(out.Pages) != 0 {
		t.Fatalf("linked pending or invalid candidates: %+v", out)
	}
	if err := f.store.SetStatus(t.Context(), pending, StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	count := countProductReadStatements(t, f.store)
	assertPage(getLinkedProduct(t, f.store, pushTwo, ""), pending)
	if !slices.Equal(count.statements, []int{3, 1, 2, 1}) {
		t.Fatalf("warm read statements = %v, want direct, token, page tier, chunk batch", count.statements)
	}
	count.queries = nil
	if out := getLinkedProduct(t, f.store, pushTwo, "push-rebound"); !out.PagesMissing || out.PagesPartial != "" || len(out.Pages) != 0 {
		t.Fatalf("out-of-domain scope linked a page: %+v", out)
	}
	if !slices.Equal(count.queries, []string{getProductQuery, productRefPagesQuery}) {
		t.Fatalf("scoped read issued inferred queries: %v", count.queries)
	}

	// The product can be changed into a competing owner, then removed
	// without re-ingesting the document or restarting the store.
	f.put(3, pageLinkRecord(pushTwo, "https://another.example/Something/IMPLANT-KIT"), "updated")
	for _, sourceID := range []string{pushOne, pushTwo} {
		if out := getLinkedProduct(t, f.store, sourceID, ""); !out.PagesMissing || out.PagesPartial != "" || len(out.Pages) != 0 {
			t.Fatalf("ambiguous key linked %s: %+v", sourceID, out)
		}
	}
	index, err := f.store.loadProductLinkIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if index.pageAmbiguous != 1 || len(index.pageOwners[pushDomain]) != 0 {
		t.Fatalf("ambiguous count=%d owners=%v", index.pageAmbiguous, index.pageOwners)
	}
	f.remove(4, pushTwo, "deleted")
	assertPage(getLinkedProduct(t, f.store, pushOne, ""), first)

	directRead := len(count.statements)
	count.afterTx = func(n int) {
		if n == directRead {
			if _, err := f.other.PushProduct(t.Context(), 5, []byte(pageLinkRecord(pushTwo, firstLink))); err != nil {
				t.Fatal(err)
			}
		}
	}
	out := getLinkedProduct(t, f.store, pushOne, "")
	count.afterTx = nil
	if !out.PagesMissing || out.PagesPartial != "" || len(out.Pages) != 0 {
		t.Fatalf("page resolver used the index from before the direct snapshot committed: %+v", out)
	}
}

func TestProductPagePrecedenceChunksAndReportUnion(t *testing.T) {
	f := newPushFixture(t)
	const link = "https://push.test/dentalica/implant-kit.html"
	f.snapshot(pushA, 1, "imported",
		pageLinkRecord(pushOne, link), pageLinkRecord(pushTwo, "https://push.test/missing-item.html"))
	add := func(kind, source, final, timestamp string, refs ...string) string {
		t.Helper()
		id := addProductLinkDocument(t, f.store, DocumentInput{
			Kind: kind, Source: source, FinalURL: final, Domain: pushDomain, ProductRefs: refs,
		}, StatusDone)
		f.query(`MATCH (d:Document {id:$id}) SET d.updated_at=$timestamp`, map[string]any{
			"id": id, "timestamp": "2026-01-01T00:00:0" + timestamp + ".000000000Z",
		})
		return id
	}
	ref := add("file", "refs/implant-kit", link, "1", pushOne)
	exact := add("text", link, "", "2") // Dentalica-shaped text source with no refs
	older := add("file", "old/implant-kit", "", "3")
	first := add("file", "newer-a/implant-kit", "", "4")
	second := add("file", "newer-b/implant-kit", "", "4")
	wantPage := min(first, second)

	// Ingest yields one chunk per sequence. Change one seq in the fixture
	// to exercise the read's secondary ID sort.
	chunks := []Chunk{{Seq: 9, Text: "last"}, {Seq: 4, Text: "third"}, {Seq: 1, Text: "first"}, {Seq: 2, Text: "second"}}
	if err := f.store.PutChunks(t.Context(), exact, chunks); err != nil {
		t.Fatal(err)
	}
	f.query(`MATCH (c:Chunk {id:$id}) SET c.seq=1`, map[string]any{"id": ChunkID(exact, 2)})
	if err := f.store.PutChunks(t.Context(), wantPage, []Chunk{{Seq: 0, Text: "page passage"}}); err != nil {
		t.Fatal(err)
	}

	count := countProductReadStatements(t, f.store)
	out := getLinkedProduct(t, f.store, pushOne, "")
	if !slices.Equal(count.statements, []int{3, 1, 3, 2, 1}) || count.queries[len(count.queries)-1] != productPageChunksQuery {
		t.Fatalf("cold read statements = %v, want direct, token, three index statements, two page statements, one chunk batch", count.statements)
	}
	if out.PagesMissing || out.PagesPartial != "" || len(out.Pages) != 3 {
		t.Fatalf("pages = %+v, missing = %t, partial = %q", out.Pages, out.PagesMissing, out.PagesPartial)
	}
	id1, id2 := ChunkID(exact, 1), ChunkID(exact, 2)
	for i, want := range []struct {
		id            string
		via, chunkIDs []string
		chunkCount    int
	}{
		{ref, []string{"ref", "url", "page"}, []string{}, 0},
		{exact, []string{"url"}, []string{min(id1, id2), max(id1, id2), ChunkID(exact, 4)}, 4},
		{wantPage, []string{"page"}, []string{ChunkID(wantPage, 0)}, 1},
	} {
		got := out.Pages[i]
		if got.ID != want.id || !slices.Equal(got.Via, want.via) || !reflect.DeepEqual(got.ChunkIDs, want.chunkIDs) || got.ChunkCount != want.chunkCount {
			t.Errorf("page %d = %+v, want %+v", i, got, want)
		}
	}

	// Include another store bound to the same domain to check that the
	// report scans the domain once and still emits a row per store.
	f.query(`CREATE (:ProductStore {store:'same-page-domain.test', domain:$domain})`, map[string]any{"domain": pushDomain})
	count.queries = nil
	reports, err := f.store.ReportProductPages(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 || reports[0].Store != pushStore || reports[1].Store != "same-page-domain.test" {
		t.Fatalf("reports=%+v", reports)
	}
	scans := 0
	for _, query := range count.queries {
		if query == productPagesReportDocumentsQuery {
			scans++
		}
	}
	if scans != 1 {
		t.Fatalf("reports for one domain used %d document scans", scans)
	}
	want := ProductPagesReport{Store: pushStore, Domain: pushDomain, MissingProductIDs: []string{},
		Refs: 1, URL: 2, Page: 4}
	for _, sourceID := range []string{pushOne, pushTwo} {
		detail := getLinkedProduct(t, f.store, sourceID, "")
		selected := newProductPageSelection(0)
		err := f.store.runReadTx(t.Context(), func(tx neo4j.ManagedTransaction) error {
			return txResolveDirectProductPages(t.Context(), tx, detail.ID, pushDomain, detail.Link, "", &selected)
		})
		if err != nil {
			t.Fatal(err)
		}
		selected, partial := f.store.resolveProductPageTier(t.Context(), selected, detail.ID, pushDomain, detail.Link, "")
		if partial != "" {
			t.Fatalf("uncapped resolver returned partial = %q", partial)
		}
		if sourceID == pushOne {
			got := make([]string, 0, len(selected.pages))
			for _, p := range selected.pages {
				got = append(got, p.ID)
			}
			if !slices.Equal(got, []string{ref, exact, min(first, second), max(first, second), older}) {
				t.Fatalf("uncapped union = %v", got)
			}
		}
		n := len(selected.pages)
		want.Total += n
		switch n {
		case 0:
			want.Zero++
			want.MissingProductIDs = append(want.MissingProductIDs, detail.ID)
		case 1:
			want.One++
		default:
			want.Multiple++
		}
	}
	if !reflect.DeepEqual(reports[0], want) {
		t.Errorf("report = %+v, resolver union = %+v", reports[0], want)
	}
}

func TestProductPagePartialAndIndependentLegs(t *testing.T) {
	f := newPushFixtureWithFeature(t, "catalogue_code")
	const link = "https://push.test/implant-kit.html"
	record := strings.Replace(pageLinkRecord(pushOne, link), `"attributes":{}`, `"attributes":{"scalar":"AB12345"}`, 1)
	f.snapshot(pushA, 1, "imported", record)
	ctx := t.Context()
	loadWithCaps := func(codeCap, linkCap int) *ProductLinkIndex {
		t.Helper()
		index, err := f.store.productLinks.load(ctx, f.store.readProductLinkToken, func(ctx context.Context) (productLinkSnapshot, error) {
			return f.store.buildProductLinkSnapshotWithLimit(ctx, codeCap, linkCap)
		})
		if err != nil {
			t.Fatal(err)
		}
		return index
	}
	index := loadWithCaps(0, 1)
	id := productobs.ProductID(pushStore, pushOne)
	if !errors.Is(index.codeErr, ErrProductMentionIndexTooLarge) ||
		index.pageProductOwner(pushDomain, "bc-it/implant-kit") != id ||
		!slices.Equal(index.urlProductOwners(pushDomain, link), []string{id}) ||
		index.products[id].Title != "Implant kit" || index.products[id].Brand != "Fixture brand" {
		t.Fatalf("code cap dropped unrelated data: %+v", index)
	}
	if _, err := f.store.LoadProductMentionIndex(ctx); !errors.Is(err, ErrProductMentionIndexTooLarge) {
		t.Fatalf("code-only read error = %v", err)
	}
	f.put(2, pageLinkRecord(pushTwo, "https://push.test/x"), "created")
	index = loadWithCaps(maxProductMentionValues, 1)
	if !index.urlOff || index.pageOff || index.pageProductOwner(pushDomain, "bc-it/implant-kit") != id ||
		index.codeErr != nil || len(index.code.Match("AB12345")) != 1 {
		t.Fatalf("URL cap dropped unrelated data: %+v", index)
	}

	f.put(3, pageLinkRecord(pushTwo, "https://push.test/other-implant-kit.html"), "updated")
	index = loadWithCaps(maxProductMentionValues, 1)
	if !index.pageOff || index.pageOwners != nil || index.codeErr != nil || len(index.code.Match("AB12345")) != 1 {
		t.Fatalf("page cap dropped unrelated code data: %+v", index)
	}
	addProductLinkDocument(t, f.store, DocumentInput{
		Kind: "file", Domain: pushDomain, Source: "bc-it/implant-kit",
	}, StatusDone)
	if out := getLinkedProduct(t, f.store, pushOne, ""); out.PagesMissing || out.PagesPartial != "index_too_large" || len(out.Pages) != 0 {
		t.Fatalf("empty partial pages = %+v", out)
	}
	exact := addProductLinkDocument(t, f.store, DocumentInput{
		Kind: "text", Domain: pushDomain, Source: link,
	}, StatusDone)
	if out := getLinkedProduct(t, f.store, pushOne, ""); out.PagesMissing || out.PagesPartial != "index_too_large" ||
		len(out.Pages) != 1 || out.Pages[0].ID != exact || !slices.Equal(out.Pages[0].Via, []string{"url"}) {
		t.Fatalf("partial pages lost exact URL: %+v", out)
	}
}

func TestProductURLIndexIncludesAllOrderedOwners(t *testing.T) {
	f := newPushFixture(t)
	ctx := t.Context()
	const store2 = "second-url-owner.test"
	f.query(`CREATE (:ProductStore {store:$store, domain:$domain})`, map[string]any{"store": store2, "domain": pushDomain})
	type ownersFixture struct {
		link string
		doc  string
		ids  []string
	}
	var fixtures []ownersFixture
	for _, n := range []int{50, 51} {
		link := fmt.Sprintf("https://example.test/implant-kit-%d", n)
		var want []string
		for _, store := range []string{pushStore, store2} {
			var products []map[string]any
			for i := range n {
				if (i < n/2) != (store == pushStore) {
					continue
				}
				sourceID := fmt.Sprintf("%s:%d-%02d", store, n, i)
				id := productobs.ProductID(store, sourceID)
				products = append(products, map[string]any{"id": id, "product_id": sourceID})
				want = append(want, id)
			}
			slices.Reverse(products)
			f.query(`UNWIND $products AS p CREATE (:Product {
				id:p.id, product_id:p.product_id, store:$store, link:$link })`, map[string]any{
				"products": products, "store": store, "link": link,
			})
		}
		doc := addProductLinkDocument(t, f.store, DocumentInput{Kind: "text", Source: link, Domain: pushDomain}, StatusDone)
		fixtures = append(fixtures, ownersFixture{link: link, doc: doc, ids: want})
	}
	fresh, err := f.other.buildProductLinkSnapshot(ctx, maxProductMentionValues)
	if err != nil {
		t.Fatal(err)
	}
	index, err := f.store.loadProductLinkIndex(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(index.urlOwners, fresh.index.urlOwners) || !reflect.DeepEqual(index.pageOwners, fresh.index.pageOwners) ||
		index.pageAmbiguous != 2 {
		t.Fatalf("cached and fresh link key sets differ or missing ambiguities: %v vs %v", index.pageOwners, fresh.index.pageOwners)
	}
	for _, fixture := range fixtures {
		if got := index.urlProductOwners(pushDomain, fixture.link); !slices.Equal(got, fixture.ids) {
			t.Fatalf("URL owners = %v, want all %d in product order", got, len(fixture.ids))
		}
		for _, id := range fixture.ids {
			out, err := f.store.GetProduct(ctx, ProductLookup{ID: id})
			if err != nil || len(out.Pages) != 1 || out.Pages[0].ID != fixture.doc || !slices.Equal(out.Pages[0].Via, []string{"url"}) {
				t.Fatalf("%s pages=%+v err=%v, want %s via URL", id, out.Pages, err, fixture.doc)
			}
		}
	}
	reports, err := f.store.ReportProductPages(ctx, "")
	if err != nil || len(reports) != 2 {
		t.Fatalf("reports = %+v, err = %v", reports, err)
	}
	for i, want := range []int{50, 51} {
		report := reports[i]
		if report.Total != want || report.URL != want || report.One != want || report.Page != 0 {
			t.Fatalf("report %+v, want %d URL associations", report, want)
		}
	}
}

func TestProductLinkBudgetBoundaries(t *testing.T) {
	rows := productLinkBudget{maxRows: maxProductLinkRows, rows: maxProductLinkRows - 1}
	if !rows.add(0) || rows.add(0) {
		t.Fatal("row cap must allow the limit and reject the next row")
	}
	bytes := productLinkBudget{maxRows: maxProductLinkRows, bytes: maxProductLinkBytes - 1}
	if !bytes.add(1) || bytes.add(1) {
		t.Fatal("byte cap must allow the limit and reject the next byte")
	}
}
