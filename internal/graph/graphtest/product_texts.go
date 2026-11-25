package graphtest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const (
	TextReadDomain = "t3-text-reads"
	TextReadStore  = "t3-text-reads.test"
)

var (
	TextReadManyID  = productobs.ProductID(TextReadStore, TextReadStore+":many")
	TextReadLargeID = productobs.ProductID(TextReadStore, TextReadStore+":large")
)

// ProductTextReads seeds capped product lookups and one searchable vector.
func ProductTextReads(t testing.TB) *graph.Store {
	t.Helper()
	st := Store(t)
	run := lookupFixtureQuery(t)
	if err := st.EnsureTextVecIndex(t.Context()); err != nil {
		t.Fatal(err)
	}
	const release = "t3-text-reads-release"
	const manifest = `{"store_domains":{"t3-text-reads.test":"t3-text-reads"}}`
	pointer := run(`MATCH (m:GkgMeta {id:'product_release'}) RETURN properties(m)`, nil)
	run(`
CREATE (:ProductRelease {id:$release, manifest_json:$manifest, manifest_digest:$digest})
MERGE (m:GkgMeta {id:'product_release'}) SET m.value=$release`,
		map[string]any{"release": release, "manifest": manifest, "digest": fmt.Sprintf("%x", sha256.Sum256([]byte(manifest)))})
	t.Cleanup(func() { run("DROP VECTOR INDEX text_vec", nil) })
	t.Cleanup(func() {
		run(`MATCH (v:TextVec {id:'t3-text-reads-vector'}) SET v.embedding=null DELETE v`, nil)
		run(`MATCH (n) WHERE n.store=$store DETACH DELETE n`, map[string]any{"store": TextReadStore})
		run(`MATCH (r:ProductRelease {id:$release}) DELETE r`, map[string]any{"release": release})
		if len(pointer) == 0 {
			run(`MATCH (m:GkgMeta {id:'product_release'}) DELETE m`, nil)
		} else {
			run(`MATCH (m:GkgMeta {id:'product_release'}) SET m=$props`, map[string]any{"props": pointer[0].Values[0]})
		}
	})
	run(`
CREATE (:ProductStore {store:$store, domain:$domain, product_count:2, text_version:1, text_count:92, text_edge_count:92})
CREATE (:Product {id:$many, store:$store, product_id:$store+':many', domain:$domain, title:'Ceramic kit'})
CREATE (:Product {id:$large, store:$store, product_id:$store+':large', domain:$domain, title:'Large kit'})
CREATE (v:TextVec {id:'t3-text-reads-vector', vec_id:$many+'-1', space:$space})
SET v.embedding = [1.0] + [i IN range(1, 767) | 0.0]`,
		map[string]any{"store": TextReadStore, "domain": TextReadDomain, "many": TextReadManyID, "large": TextReadLargeID, "space": graph.ProductTextSpace})
	for _, item := range []struct {
		id, text string
		count    int
	}{{TextReadManyID, strings.Repeat("é", 201), 70}, {TextReadLargeID, strings.Repeat("<", 2048), 22}} {
		run(`
MATCH (p:Product {id:$product})
UNWIND range(1, $count) AS i
WITH p, i, p.id + '-' + toString(i) AS id
CREATE (p)-[:HAS_TEXT {key:'key-' + toString(i), truncated:i = 1}]->
       (:ProductText {id:id, vec_id:id, store:p.store, text:$text})`,
			map[string]any{"product": item.id, "count": item.count, "text": item.text})
	}
	return st
}

// ProductTextEmbedder provides an aligned query vector without calling a provider.
type ProductTextEmbedder struct{}

func (ProductTextEmbedder) Space() string { return graph.ProductTextSpace }

func (ProductTextEmbedder) EmbedQuery(_ context.Context, _ string) ([]float64, error) {
	vec := make([]float64, 768)
	vec[0] = 1
	return vec, nil
}
