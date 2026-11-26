package graph

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const (
	cardTestDomain  = "scoped-catalogue"
	cardTestProduct = "shop.example:1"
	cardTestRelease = "scoped-release"
	cardTestOther   = "other.example:2"
)

func scopedReadTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("GKG_TEST_MEMGRAPH_URI")
	if uri == "" {
		t.Skip("requires a disposable Memgraph integration database")
	}
	st, err := Open(t.Context(), config.Config{Memgraph: config.MemgraphConfig{URI: uri}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if err := st.runWrite(ctx, `MATCH (n:ScopedReadFixture) DETACH DELETE n`, nil); err != nil {
			t.Error(err)
		}
		if err := st.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return st
}

func seedCardProduct(t *testing.T, st *Store, keyStore, keyID, store, productID, domain string) {
	t.Helper()
	foldTestWrite(t, st, `
CREATE (:Product:ScopedReadFixture {id:$id, store:$store, product_id:$product_id, domain:$domain,
    title:'Fixture kit', brand:'Fixture', category:'Kits', subcategory:'Starter', product_type:'Kit',
    link:'https://shop.example/kit'})`, map[string]any{
		"id": productobs.ProductID(keyStore, keyID), "store": store, "product_id": productID, "domain": domain,
	})
}

func TestProductCardsMatchesGetProduct(t *testing.T) {
	st := scopedReadTestStore(t)
	ctx := t.Context()
	seedCardProduct(t, st, "shop.example", cardTestProduct, "shop.example", cardTestProduct, cardTestDomain)
	seedCardProduct(t, st, "other.example", cardTestOther, "other.example", cardTestOther, cardTestDomain)
	foldTestWrite(t, st, `
CREATE (:GkgMeta:ScopedReadFixture {id:'product_release', value:$release, activation_seq:7})
WITH 1 AS ignored
MATCH (p:Product {id:$id})
UNWIND range(41, 0, -1) AS i
CREATE (p)-[:HAS_FEATURE {source_key:'source', release_id:$release, raw:'12 mm', items_json:'["12 mm"]',
    split_status:'single', status:'exact', lo:'12', lo_canonical:'12', unit_in:'mm', canonical_unit:'mm',
    conflict:true, numeric:true}]->(:FeatureRev:ScopedReadFixture {id:'f'+toString(i), key:'r1/f'+toString(i),
    rev:'r1', definition:'Fixture measurement', definition_digest:'digest', name:'Fixture measurement',
    kind:'measurement', cardinality:'single', canonical_unit:'mm'})`, map[string]any{
		"id": productobs.ProductID("shop.example", cardTestProduct), "release": cardTestRelease,
	})
	foldTestWrite(t, st, `
MATCH (p:Product {id:$id})
CREATE (p)-[:HAS_FEATURE {source_key:'stale', release_id:'old-release', raw:'stale'}]->
    (:FeatureRev:ScopedReadFixture {id:'a-stale', key:'old/a-stale'})`,
		map[string]any{"id": productobs.ProductID("shop.example", cardTestProduct)})

	want, err := st.GetProduct(ctx, ProductLookup{Store: "shop.example", ProductID: cardTestProduct, SeedDomain: cardTestDomain})
	if err != nil || len(want.Features) != 42 {
		t.Fatalf("GetProduct features = %d, err = %v, want 42 active evidence rows", len(want.Features), err)
	}
	got, err := st.ProductCards(ctx, cardTestDomain, []string{cardTestOther, cardTestProduct, cardTestProduct, "shop.example:missing"}, 40)
	if err != nil {
		t.Fatal(err)
	}
	card, ok := got.Cards[cardTestProduct]
	if !ok || !reflect.DeepEqual(card.Product, want.Product) ||
		!reflect.DeepEqual(card.Features, want.Features[:40]) || card.FeaturesOmitted != 2 {
		t.Fatalf("card mismatch: found=%t product=%+v features=%d omitted=%d", ok, card.Product, len(card.Features), card.FeaturesOmitted)
	}
	second, ok := got.Cards[cardTestOther]
	if len(got.Cards) != 2 || !ok || second.Features == nil || len(second.Features) != 0 || second.FeaturesOmitted != 0 {
		t.Fatalf("unexpected batch membership or second card: size=%d second=%+v found=%t", len(got.Cards), second, ok)
	}
}

func TestProductCardsSeedDomain(t *testing.T) {
	st := scopedReadTestStore(t)
	seedCardProduct(t, st, "shop.example", cardTestProduct, "shop.example", cardTestProduct, cardTestDomain)
	seedCardProduct(t, st, "other.example", cardTestOther, "other.example", cardTestOther, "scoped-other")
	seedCardProduct(t, st, "shop.example", "shop.example:wrong-store", "other.example", "shop.example:wrong-store", cardTestDomain)
	seedCardProduct(t, st, "shop.example", "shop.example:wrong-id", "shop.example", "shop.example:different", cardTestDomain)
	got, err := st.ProductCards(t.Context(), cardTestDomain, []string{
		cardTestProduct, cardTestOther, "shop.example:wrong-store", "shop.example:wrong-id",
	}, 40)
	if _, found := got.Cards[cardTestProduct]; err != nil || len(got.Cards) != 1 || !found {
		t.Fatalf("ProductCards = %+v, %v, want only %s", got, err, cardTestProduct)
	}
}

func TestProductCardsNoActiveRelease(t *testing.T) {
	st := scopedReadTestStore(t)
	seedCardProduct(t, st, "shop.example", cardTestProduct, "shop.example", cardTestProduct, cardTestDomain)
	foldTestWrite(t, st, `
MATCH (p:Product {id:$id})
CREATE (p)-[:HAS_FEATURE {source_key:'stale', release_id:'old-release', raw:'stale'}]->
    (:FeatureRev:ScopedReadFixture {id:'a-stale', key:'old/a-stale'})`,
		map[string]any{"id": productobs.ProductID("shop.example", cardTestProduct)})
	got, err := st.ProductCards(t.Context(), cardTestDomain, []string{cardTestProduct}, 40)
	card, ok := got.Cards[cardTestProduct]
	if err != nil || !ok || card.Features == nil || len(card.Features) != 0 || card.FeaturesOmitted != 0 ||
		got.AsOf.ReleaseID != nil || got.AsOf.ActivationSeq != nil || got.AsOf.Stores == nil || len(got.AsOf.Stores) != 0 {
		t.Fatalf("ProductCards = %+v, %v, want the card with empty features and no active release", got, err)
	}
}

func TestProductCardsWatermarks(t *testing.T) {
	st := scopedReadTestStore(t)
	foldTestWrite(t, st, `
CREATE (:GkgMeta:ScopedReadFixture {id:'product_release', value:$release, activation_seq:7}),
       (:ProductStore:ScopedReadFixture {store:'shop.example', domain:$domain, materialized_activation_seq:4}),
       (:ProductStore:ScopedReadFixture {store:'other.example', domain:$domain})`,
		map[string]any{"domain": cardTestDomain, "release": cardTestRelease})
	got, err := st.ProductCards(t.Context(), cardTestDomain, nil, 40)
	want := CardsAsOf{
		ReleaseID: new(cardTestRelease), ActivationSeq: new(int64(7)),
		Stores: []StoreWatermark{{Store: "other.example"}, {Store: "shop.example", MaterializedActivationSeq: new(int64(4))}},
	}
	if err != nil || got.Cards == nil || len(got.Cards) != 0 || !reflect.DeepEqual(got.AsOf, want) {
		t.Fatalf("ProductCards = %+v, %v, want no cards and watermarks %+v", got, err, want)
	}
}
