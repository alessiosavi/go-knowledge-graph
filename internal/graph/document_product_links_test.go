package graph

import (
	"reflect"
	"slices"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

func TestDocumentProductLinksReadsLiveRefsInOneQuery(t *testing.T) {
	f := newPushFixture(t)
	const link = "https://push.test/implants/implant-kit.html"
	f.snapshot(pushA, 1, "imported", pageLinkRecord(pushOne, link))
	done := addProductLinkDocument(t, f.store, DocumentInput{
		Kind: "file", Source: "bc-it/implant-kit", FinalURL: link, Domain: pushDomain,
		ProductRefs: []string{pushOne, pushTwo},
	}, StatusDone)
	pending := addProductLinkDocument(t, f.store, DocumentInput{
		Kind: "file", Source: "bc-it/pending", Domain: pushDomain, ProductRefs: []string{pushOne},
	}, StatusReceived)
	id := productobs.ProductID(pushStore, pushOne)
	ctx := t.Context()
	count := countProductReadStatements(t, f.store)
	read := func(wantRefs []string) {
		t.Helper()
		start := len(count.queries)
		docs, err := f.store.DocumentProductLinks(ctx, []string{done, pending, done, "missing-doc"})
		if err != nil || len(docs) != 2 || !slices.Equal(count.queries[start:], []string{documentProductLinksQuery}) {
			t.Fatalf("documents = %+v, err = %v, statements = %v", docs, err, count.queries[start:])
		}
		doc := docs[done]
		var ids []string
		for _, ref := range doc.Refs {
			ids = append(ids, ref.ID)
			if ref.Brand != "Fixture brand" {
				t.Errorf("ref display = %+v", ref)
			}
		}
		doc.Refs = nil
		want := DocumentProductLinks{Status: StatusDone, Domain: pushDomain, Source: "bc-it/implant-kit", SourceKind: "file", FinalURL: link}
		if !reflect.DeepEqual(doc, want) || !slices.Equal(ids, wantRefs) {
			t.Fatalf("document = %+v, refs = %v, want %+v and %v", doc, ids, want, wantRefs)
		}
		if doc := docs[pending]; doc.Status != StatusReceived || len(doc.Refs) != 0 {
			t.Errorf("pending document = %+v", doc)
		}
	}
	read([]string{id})
	f.remove(2, pushOne, "deleted")
	read(nil)
	f.put(3, pageLinkRecord(pushOne, link), "created")
	read([]string{id})
}

func TestResolveDocumentProductsLegStatus(t *testing.T) {
	const link = "https://push.test/implant-kit"
	doc := DocumentProductLinks{Status: StatusDone, Domain: pushDomain, Source: "bc-it/implant-kit", FinalURL: link}
	index := &ProductLinkIndex{
		urlOwners:  map[string]map[string][]string{pushDomain: {link: {"url-owner"}}},
		pageOwners: map[string]map[string]string{pushDomain: {"implant-kit": "page-owner"}},
	}
	for _, tc := range []struct {
		name    string
		idx     *ProductLinkIndex
		want    InferredDocumentProducts
		partial string
	}{
		{"both", index, InferredDocumentProducts{URL: [2][]string{nil, {"url-owner"}}, Page: "page-owner"}, ""},
		{"url capped", &ProductLinkIndex{urlOff: true, pageOwners: index.pageOwners}, InferredDocumentProducts{Page: "page-owner"}, "index_too_large"},
		{"page capped", &ProductLinkIndex{pageOff: true, urlOwners: index.urlOwners},
			InferredDocumentProducts{URL: [2][]string{nil, {"url-owner"}}}, "index_too_large"},
		{"index unavailable", nil, InferredDocumentProducts{}, "index_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, partial := tc.idx.ResolveDocumentProducts(doc); !reflect.DeepEqual(got, tc.want) || partial != tc.partial {
				t.Fatalf("owners = %+v, partial = %q, want %+v, %q", got, partial, tc.want, tc.partial)
			}
		})
	}
}
