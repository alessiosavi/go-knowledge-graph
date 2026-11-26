package chunkread_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

type cappedProductDisplayStore struct {
	*graph.Store
	missingID string
}

func (s cappedProductDisplayStore) LoadProductLinkIndex(ctx context.Context) (*graph.ProductLinkIndex, error) {
	index, err := s.Store.LoadProductLinkIndex(ctx)
	if err != nil {
		return nil, err
	}
	return index.WithProductDisplaysCappedForTest(), nil
}

func (s cappedProductDisplayStore) ProductLinkDisplays(ctx context.Context, ids []string) (map[string]graph.ProductLinkDisplay, error) {
	rows, err := s.Store.ProductLinkDisplays(ctx, ids)
	delete(rows, s.missingID)
	return rows, err
}

func TestReadProductStoresDropsOtherDomainProduct(t *testing.T) {
	st, query := mentionDB(t, "chunk-scope-")
	t.Cleanup(func() { query(`MATCH (s:ChunkScopeFixture) DETACH DELETE s`, nil) })
	query(`
CREATE (:GkgMeta {id:'product_release', value:'chunk-scope-release'}),
       (:ProductStore:ChunkScopeFixture {store:'shop.example', domain:'catalogue'}),
       (:Product {id:'chunk-scope-ref', store:'shop.example', product_id:'shop.example:1', domain:'other-domain'}),
       (:Product {id:'chunk-scope-url', store:'shop.example', product_id:'shop.example:2', domain:'other-domain',
                  link:'https://shop.example/guide'}),
       (:Product {id:'chunk-scope-page', store:'shop.example', product_id:'shop.example:3', domain:'other-domain',
                  link:'https://shop.example/catalogue/implant-kit.html'}),
       (foreign:Product {id:'chunk-scope-code', store:'shop.example', product_id:'shop.example:4', domain:'other-domain'}),
       (local:Product {id:'chunk-scope-local', store:'shop.example', product_id:'shop.example:5', domain:'catalogue',
                      link:'https://shop.example/guide'}),
       (d:Document {id:'chunk-scope-doc', domain:'catalogue', status:'done', source:'catalogue/implant-kit',
                    final_url:'https://shop.example/guide'}),
       (d)-[:HAS_CHUNK]->(:Chunk {id:'chunk-scope-c0', doc_id:d.id, domain:d.domain, seq:0, text:'AB12345 CD12345'}),
       (d)-[:REFS]->(:ProductRef {id:'chunk-scope-ref'}),
       (d)-[:REFS]->(:ProductRef {id:local.id}),
       (f:FeatureRev {id:'catalogue_code', key:'chunk-scope-code'}),
       (foreign)-[:HAS_FEATURE {release_id:'chunk-scope-release', value_fold:'AB12345'}]->(f),
       (local)-[:HAS_FEATURE {release_id:'chunk-scope-release', value_fold:'CD12345'}]->(f)`, nil)

	for _, tc := range []struct {
		name  string
		store chunkread.Store
	}{
		{"cached displays", st},
		{"fallback displays", cappedProductDisplayStore{Store: st}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := chunkread.Request{IDs: []string{"chunk-scope-c0"}, Domain: "catalogue", WithProducts: true}
			read := func(want ...string) {
				t.Helper()
				out, err := chunkread.Read(t.Context(), tc.store, req)
				if err != nil || len(out.Chunks) != 1 || out.ProductsError != "" || out.LinksPartial != "" {
					t.Fatalf("Read = %+v, %v", out, err)
				}
				item := out.Chunks[0]
				if item.Products == nil || item.ProductsTotal == nil {
					t.Fatalf("missing products or total: %+v", item)
				}
				var links []string
				for _, p := range *item.Products {
					links = append(links, p.ID+"/"+strings.Join(p.Via, "+"))
				}
				if !slices.Equal(links, want) || *item.ProductsTotal != len(want) {
					t.Errorf("links = %v, total = %d, want %v and total %d", links, *item.ProductsTotal, want, len(want))
				}
			}
			read("chunk-scope-ref/ref", "chunk-scope-local/ref+url+code", "chunk-scope-url/url", "chunk-scope-page/page", "chunk-scope-code/code")
			req.ProductStores = []string{"shop.example"}
			req.ProductsLimit = new(1)
			read("chunk-scope-local/ref+url+code")
		})
	}
}

type productLinkSnapshotStore struct {
	*graph.Store
	index *graph.ProductLinkIndex
}

func (s productLinkSnapshotStore) LoadProductLinkIndex(context.Context) (*graph.ProductLinkIndex, error) {
	return s.index, nil
}

func TestReadCappedCodeProductBrandScopeMemgraph(t *testing.T) {
	st, query := mentionDB(t, "chunk-brand-")
	t.Cleanup(func() { query(`MATCH (s:ChunkBrandFixture) DETACH DELETE s`, nil) })
	query(`
CREATE (:GkgMeta {id:'product_release', value:'chunk-brand-release'}),
       (:ProductStore:ChunkBrandFixture {store:'shop.example', domain:'chunk-brand-original'}),
       (p:Product {id:'chunk-brand-product', store:'shop.example', product_id:'shop.example:1',
                   domain:'chunk-brand-original', brand:''}),
       (d:Document {id:'chunk-brand-doc', domain:'chunk-brand-original', status:'done'}),
       (d)-[:HAS_CHUNK]->(:Chunk {id:'chunk-brand-c0', doc_id:d.id, domain:d.domain, seq:0, text:'BX12345'}),
       (f:FeatureRev {id:'catalogue_code', key:'chunk-brand-code'}),
       (p)-[:HAS_FEATURE {release_id:'chunk-brand-release', value_fold:'BX12345'}]->(f)`, nil)
	index, err := st.LoadProductLinkIndex(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Keep the original index while the live display read sees the rebind.
	store := productLinkSnapshotStore{Store: st, index: index.WithProductDisplaysCappedForTest()}
	query(`MATCH (p:Product {id:'chunk-brand-product'}) SET p.domain='chunk-brand-rebound', p.brand='Live Brand'`, nil)

	for _, tc := range []struct {
		name   string
		stores []string
		brand  string
	}{
		{"scoped", []string{"shop.example"}, ""},
		{"unscoped", nil, "Live Brand"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := chunkread.Read(t.Context(), store, chunkread.Request{
				IDs: []string{"chunk-brand-c0"}, Domain: "chunk-brand-original", WithProducts: true, ProductStores: tc.stores,
			})
			if err != nil || len(out.Chunks) != 1 || out.ProductsError != "" || out.LinksPartial != "" {
				t.Fatalf("Read = %+v, %v", out, err)
			}
			item := out.Chunks[0]
			if item.Products == nil || len(*item.Products) != 1 || item.ProductsTotal == nil || *item.ProductsTotal != 1 {
				t.Fatalf("Read = %+v, want one code link", item)
			}
			p := (*item.Products)[0]
			if p.ID != "chunk-brand-product" || !slices.Equal(p.Via, []string{"code"}) || p.Brand != tc.brand {
				t.Errorf("product = %+v, want chunk-brand-product via code with brand %q", p, tc.brand)
			}
		})
	}
}

func TestReadDocumentProductsMemgraph(t *testing.T) {
	const prefix = "chunk-links-"
	st, query := mentionDB(t, prefix)
	query(`
CREATE (:GkgMeta {id:'product_release', value:'chunk-links-release'}),
       (:ProductStore {store:'chunk-links-a.test', domain:'chunk-links-domain'}),
       (:ProductStore {store:'chunk-links-b.test', domain:'chunk-links-domain'}),
       (:ProductStore {store:'chunk-links-z.test', domain:'chunk-links-other'}),
       (c:Product {id:'chunk-links-code', store:'chunk-links-a.test', product_id:'chunk-links-a.test:1', title:'Code', brand:'Code Brand'}),
       (u:Product {id:'chunk-links-url', store:'chunk-links-a.test', product_id:'chunk-links-a.test:2', title:'URL', brand:'URL Brand',
                   link:'https://chunk-links.test/guide'}),
       (m:Product {id:'chunk-links-mixed', store:'chunk-links-b.test', product_id:'chunk-links-b.test:1', title:'Mixed', brand:'Mixed Brand',
                   link:'https://chunk-links.test/catalogue/implant-kit.html'}),
       (:Product {id:'chunk-links-cross-ref', store:'chunk-links-z.test', product_id:'chunk-links-z.test:1', title:'Ref', brand:'Ref Brand'}),
       (f:FeatureRev {id:'catalogue_code', key:'chunk-links-code'}),
       (c)-[:HAS_FEATURE {release_id:'chunk-links-release', value_fold:'BC12345'}]->(f),
       (m)-[:HAS_FEATURE {release_id:'chunk-links-release', value_fold:'AB12345'}]->(f)`, nil)

	add := func(id, domain, status, source, final, body string, refs ...string) {
		t.Helper()
		query(`CREATE (d:Document {id:$id, domain:$domain, status:$status, source_kind:'file',
		                           source:$source, final_url:$final, updated_at:'2026-11-24'}),
		             (d)-[:HAS_CHUNK]->(:Chunk {id:$id + '-c0', doc_id:$id, domain:$domain, seq:0, text:$body})
		WITH d UNWIND $refs AS id MERGE (a:ProductRef {id:id}) MERGE (d)-[:REFS]->(a)`, map[string]any{
			"id": prefix + id, "domain": prefix + domain, "status": status, "source": source, "final": final, "body": body, "refs": refs,
		})
	}
	add("all", "domain", "done", "bc-it/implant-kit", "HTTPS://CHUNK-LINKS.TEST/guide/#section", "AB12345, BC12345",
		prefix+"mixed", prefix+"cross-ref")
	add("page", "domain", "done", "bc-it/IMPLANT-KIT", "", "Plain catalogue description.")
	add("orphan", "domain", "done", "bc-it/unknown", "", "", prefix+"missing")
	add("retired", "domain", "superseded", "bc-it/implant-kit", "https://chunk-links.test/guide", "", prefix+"mixed")
	add("foreign", "outside", "done", "bc-it/implant-kit", "https://chunk-links.test/guide", "", prefix+"cross-ref")
	add("foreign-inferred", "outside", "done", "bc-it/implant-kit", "https://chunk-links.test/guide", "")

	read := func(doc, domain string, limit *int, total int, wantIDs ...string) []chunkread.ChunkProduct {
		t.Helper()
		out, err := chunkread.Read(t.Context(), st, chunkread.Request{
			IDs: []string{prefix + doc + "-c0"}, Domain: prefix + domain, WithProducts: true, ProductsLimit: limit,
		})
		if err != nil || out.ProductsError != "" || out.LinksPartial != "" || len(out.Chunks) != 1 {
			t.Fatalf("read %s: %+v, %v", doc, out, err)
		}
		item := out.Chunks[0]
		if item.ProductsTotal == nil || *item.ProductsTotal != total || item.Products == nil || *item.Products == nil {
			t.Fatalf("read %s products and total = %+v, want %v and %d", doc, item, wantIDs, total)
		}
		var ids []string
		for _, p := range *item.Products {
			ids = append(ids, p.ID)
		}
		if !slices.Equal(ids, wantIDs) {
			t.Fatalf("read %s product IDs = %v, want %v", doc, ids, wantIDs)
		}
		return *item.Products
	}
	all := read("all", "domain", nil, 4, prefix+"mixed", prefix+"cross-ref", prefix+"url", prefix+"code")
	for i, tc := range []struct {
		brand string
		via   []string
		code  string
	}{
		{"Mixed Brand", []string{"ref", "page", "code"}, "AB12345"},
		{"Ref Brand", []string{"ref"}, ""},
		{"URL Brand", []string{"url"}, ""},
		{"Code Brand", []string{"code"}, "BC12345"},
	} {
		p := all[i]
		wantMatches := []chunkread.MatchedIdentifier{}
		if tc.code != "" {
			wantMatches = append(wantMatches, chunkread.MatchedIdentifier{Feature: "catalogue_code", Value: tc.code})
		}
		if p.Brand != tc.brand || !slices.Equal(p.Via, tc.via) || p.Matched == nil ||
			!slices.Equal(p.Matched, wantMatches) || p.OutsideReturnedText {
			t.Errorf("product evidence = %+v, want %+v", p, tc)
		}
	}
	read("all", "domain", new(1), 4, prefix+"mixed")
	page := read("page", "domain", nil, 1, prefix+"mixed")
	if !slices.Equal(page[0].Via, []string{"page"}) {
		t.Errorf("page-only evidence = %v", page[0].Via)
	}
	read("retired", "domain", nil, 0)
	foreign := read("foreign", "outside", nil, 1, prefix+"cross-ref")
	if !slices.Equal(foreign[0].Via, []string{"ref"}) {
		t.Errorf("cross-domain evidence = %v", foreign[0].Via)
	}
	read("foreign-inferred", "outside", nil, 0)
	for _, id := range []string{prefix + "url", prefix + "mixed"} {
		detail, err := st.GetProduct(t.Context(), graph.ProductLookup{ID: id})
		if err != nil || slices.ContainsFunc(detail.Pages, func(p graph.ProductPage) bool {
			return p.ID == prefix+"foreign-inferred"
		}) {
			t.Errorf("get_product %s returned cross-domain inferred pages: %v, %v", id, detail.Pages, err)
		}
	}

	for _, tc := range []struct {
		name, missingID, partial string
		want                     []chunkread.ChunkProduct
	}{
		{"complete displays", "", "", all},
		{"one missing display", prefix + "url", "index_unavailable", slices.Delete(slices.Clone(all), 2, 3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := chunkread.Read(t.Context(), cappedProductDisplayStore{st, tc.missingID}, chunkread.Request{
				IDs: []string{prefix + "all-c0", prefix + "page-c0"}, WithProducts: true,
			})
			if err != nil || len(out.Chunks) != 2 || out.ProductsError != "" || out.LinksPartial != tc.partial {
				t.Fatalf("capped displays: %+v, %v", out, err)
			}
			for i, want := range [][]chunkread.ChunkProduct{tc.want, page} {
				item := out.Chunks[i]
				if item.ProductsTotal == nil || *item.ProductsTotal != len(want) || item.Products == nil || !reflect.DeepEqual(*item.Products, want) {
					t.Fatalf("chunk %s products and total = %+v, want %v", item.ID, item, want)
				}
			}
		})
	}

	for _, tc := range []struct{ product, doc, domain, via string }{
		{"cross-ref", "all", "domain", "ref"},
		{"url", "all", "domain", "url"},
		{"mixed", "page", "domain", "page"},
		{"cross-ref", "foreign", "outside", "ref"},
	} {
		detail, err := st.GetProduct(t.Context(), graph.ProductLookup{ID: prefix + tc.product, Domain: prefix + tc.domain})
		if err != nil || !slices.ContainsFunc(detail.Pages, func(page graph.ProductPage) bool {
			return page.ID == prefix+tc.doc && slices.Contains(page.Via, tc.via)
		}) {
			t.Errorf("get_product %s pages = %+v, err = %v, want %s via %s", tc.product, detail.Pages, err, tc.doc, tc.via)
		}
	}
	detail, _, err := st.GetProductWithMentions(t.Context(), graph.ProductLookup{ID: prefix + "code"})
	if err != nil || detail.ProductMentionFields == nil || !slices.ContainsFunc(detail.Mentions, func(mention graph.ProductChunkMention) bool {
		return mention.ChunkID == prefix+"all-c0"
	}) {
		t.Errorf("get_product code mentions = %+v, err = %v, want %sall-c0", detail.ProductMentionFields, err, prefix)
	}

	read("orphan", "domain", nil, 0)
	create := func() {
		query(`CREATE (:Product {id:'chunk-links-missing', store:'chunk-links-b.test', product_id:'chunk-links-b.test:2'})`, nil)
	}
	create()
	read("orphan", "domain", nil, 1, prefix+"missing")
	query(`MATCH (p:Product {id:'chunk-links-missing'}) DETACH DELETE p`, nil)
	read("orphan", "domain", nil, 0)
	create()
	read("orphan", "domain", nil, 1, prefix+"missing")
}

func TestReadDocumentProductsSurviveCodeMatchBudgetMemgraph(t *testing.T) {
	st, query := mentionDB(t, "chunk-budget-")
	query(`
CREATE (:GkgMeta {id:'product_release', value:'chunk-budget-release'}),
       (:ProductStore {store:'chunk-budget-a.test', domain:'chunk-budget'}),
       (p:Product {id:'chunk-budget-product', store:'chunk-budget-a.test', product_id:'chunk-budget-a.test:1'}),
       (a:ProductRef {id:p.id}),
       (d:Document {id:'chunk-budget-doc', domain:'chunk-budget', status:'done', source:'bc-it/unknown'}),
       (d)-[:REFS]->(a),
       (d)-[:HAS_CHUNK]->(:Chunk {id:'chunk-budget-c0', doc_id:d.id, text:$text, seq:0, domain:d.domain}),
       (f:FeatureRev {id:'catalogue_code', key:'chunk-budget-code'}),
       (p)-[:HAS_FEATURE {release_id:'chunk-budget-release', value_fold:'AB12345'}]->(f)`,
		map[string]any{"text": "AB12345 " + strings.Repeat(" ", 2<<20)})

	out, err := chunkread.Read(t.Context(), st, chunkread.Request{IDs: []string{"chunk-budget-c0"}, WithProducts: true})
	if err != nil || len(out.Chunks) != 1 || out.ProductsError != "match_budget" || out.LinksPartial != "" {
		t.Fatalf("over-budget code read = %+v, %v", out, err)
	}
	item := out.Chunks[0]
	if item.ProductsTotal == nil || *item.ProductsTotal != 1 || item.Products == nil || len(*item.Products) != 1 {
		t.Fatalf("over-budget code read lost document product: %+v", item)
	}
	if p := (*item.Products)[0]; p.ID != "chunk-budget-product" || !slices.Equal(p.Via, []string{"ref"}) || len(p.Matched) != 0 {
		t.Errorf("product = %+v, want a ref link", p)
	}
}

func TestReadDocumentProductsOverCodeCapMemgraph(t *testing.T) {
	st, query := mentionDB(t, "chunk-cap-")
	query(`
CREATE (:GkgMeta {id:'product_release', value:'chunk-cap-release'}),
       (:ProductStore {store:'chunk-cap-a.test', domain:'chunk-cap'}),
       (p:Product {id:'chunk-cap-product', store:'chunk-cap-a.test', product_id:'chunk-cap-a.test:1',
                   link:'https://chunk-cap.test/catalogue/implant-kit.html'}),
       (a:ProductRef {id:p.id}),
       (d:Document {id:'chunk-cap-doc', domain:'chunk-cap', status:'done', source:'bc-it/implant-kit'}),
       (d)-[:REFS]->(a),
       (d)-[:HAS_CHUNK]->(:Chunk {id:'chunk-cap-c0', doc_id:d.id, text:'AB12345', seq:0, domain:d.domain}),
       (f:FeatureRev {id:'catalogue_code', key:'chunk-cap-code'})
WITH p, f UNWIND range(1, 3126) AS i
CREATE (p)-[:HAS_FEATURE {release_id:'chunk-cap-release', items_fold:[j IN range(1,64) | 'AB12345']}]->(f)`, nil)

	out, err := chunkread.Read(t.Context(), st, chunkread.Request{IDs: []string{"chunk-cap-c0"}, WithProducts: true})
	if err != nil || len(out.Chunks) != 1 || out.ProductsError != "index_too_large" || out.LinksPartial != "" {
		t.Fatalf("over-cap code index read = %+v, %v", out, err)
	}
	item := out.Chunks[0]
	if item.ProductsTotal == nil || *item.ProductsTotal != 1 || item.Products == nil || len(*item.Products) != 1 {
		t.Fatalf("over-cap code index lost document product: %+v", item)
	}
	if p := (*item.Products)[0]; p.ID != "chunk-cap-product" || !slices.Equal(p.Via, []string{"ref", "page"}) || len(p.Matched) != 0 {
		t.Errorf("product = %+v, want a ref and page link", p)
	}
}

func TestReadDocumentURLWith50And51OwnersMemgraph(t *testing.T) {
	st, query := mentionDB(t, "chunk-url-")
	query(`CREATE (:ProductStore {store:'chunk-url-owners.test', domain:'chunk-url'})`, nil)
	for i, tc := range []struct {
		name  string
		count int
		split bool
	}{
		{"50", 50, false},
		{"51", 51, false},
		{"51-across-urls", 51, true},
	} {
		link := "https://chunk-url.test/shared-" + tc.name
		final := link + "#redirect"
		if tc.split {
			final = link + "-redirect"
		}
		docID := "chunk-url-doc-" + tc.name
		chunkID := docID + "-c0"
		query(`
MATCH (s:ProductStore {store:'chunk-url-owners.test'}) SET s.seen_seq = $seq
CREATE (d:Document {id:$doc, domain:s.domain, status:'done', source_kind:'url', source:$link, final_url:$final}),
       (d)-[:HAS_CHUNK]->(:Chunk {id:$chunk, doc_id:$doc, domain:d.domain, seq:0, text:'passage'})
WITH s UNWIND range(1,$count) AS i
CREATE (:Product {id:$doc + '-' + toString(i), store:s.store, product_id:'chunk-url-owners.test:' + toString(i),
                 link:CASE WHEN $split AND i % 2 = 0 THEN $final ELSE $link END})`,
			map[string]any{"count": tc.count, "seq": i + 1, "doc": docID, "chunk": chunkID, "link": link, "final": final, "split": tc.split})
		var wantPartial string
		if tc.count > 50 {
			wantPartial = "owners_truncated"
		}
		wantIDs := make([]string, 0, tc.count)
		for j := 1; j <= tc.count; j++ {
			wantIDs = append(wantIDs, fmt.Sprintf("%s-%d", docID, j))
		}
		slices.Sort(wantIDs)
		out, err := chunkread.Read(t.Context(), st, chunkread.Request{
			IDs: []string{chunkID}, WithProducts: true, ProductsLimit: new(50),
		})
		if err != nil || out.ProductsError != "" || out.LinksPartial != wantPartial || len(out.Chunks) != 1 {
			t.Fatalf("read %s URL owners: %+v, %v", tc.name, out, err)
		}
		item := out.Chunks[0]
		if item.ProductsTotal == nil || *item.ProductsTotal != 50 || item.Products == nil || len(*item.Products) != 50 {
			t.Fatalf("%s URL owners: %+v, wanted 50 of 50", tc.name, item)
		}
		for j, p := range *item.Products {
			if p.ID != wantIDs[j] || !slices.Equal(p.Via, []string{"url"}) {
				t.Errorf("%s URL owner %d = %+v, want %s via URL", tc.name, j, p, wantIDs[j])
			}
		}
	}
}
