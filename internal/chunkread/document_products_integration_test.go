package chunkread_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

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

func TestReadDocumentProductsOverCodeCapMemgraph(t *testing.T) {
	st, query := mentionDB(t, "chunk-cap-")
	query(`
CREATE (:GkgMeta {id:'product_release', value:'chunk-cap-release'}),
       (:ProductStore {store:'chunk-cap.test', domain:'chunk-cap'}),
       (p:Product {id:'chunk-cap-product', store:'chunk-cap.test', product_id:'chunk-cap.test:1',
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
	for _, count := range []int{50, 51} {
		link := fmt.Sprintf("https://chunk-url.test/shared-%d", count)
		docID := fmt.Sprintf("chunk-url-doc-%d", count)
		chunkID := docID + "-c0"
		query(`
MATCH (s:ProductStore {store:'chunk-url-owners.test'}) SET s.seen_seq = $count
CREATE (d:Document {id:$doc, domain:s.domain, status:'done', source_kind:'url', source:$link}),
       (d)-[:HAS_CHUNK]->(:Chunk {id:$chunk, doc_id:$doc, domain:d.domain, seq:0, text:'passage'})
WITH s UNWIND range(1,$count) AS i
CREATE (:Product {id:$doc + '-' + toString(i), store:s.store, product_id:'chunk-url-owners.test:' + toString(i), link:$link})`,
			map[string]any{"count": count, "doc": docID, "chunk": chunkID, "link": link})
		out, err := chunkread.Read(t.Context(), st, chunkread.Request{
			IDs: []string{chunkID}, WithProducts: true, ProductsLimit: new(50),
		})
		if err != nil || out.ProductsError != "" || out.LinksPartial != "" || len(out.Chunks) != 1 {
			t.Fatalf("read with %d URL owners: %+v, %v", count, out, err)
		}
		item := out.Chunks[0]
		if item.ProductsTotal == nil || *item.ProductsTotal != count || item.Products == nil || len(*item.Products) != 50 {
			t.Fatalf("%d URL owners: %+v, wanted 50 of %d", count, item, count)
		}
		products := *item.Products
		for _, p := range products {
			if !slices.Equal(p.Via, []string{"url"}) {
				t.Errorf("URL evidence = %v for %s", p.Via, p.ID)
			}
		}
		if !slices.IsSortedFunc(products, func(a, b chunkread.ChunkProduct) int {
			return strings.Compare(a.ProductID, b.ProductID)
		}) {
			t.Error("URL products are out of order")
		}
	}
}
