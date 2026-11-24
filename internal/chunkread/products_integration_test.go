package chunkread_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/mcp"
	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func mentionDB(t *testing.T, prefix string) (*graph.Store, func(string, map[string]any)) {
	t.Helper()
	st := graphtest.Store(t)
	driver, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })
	query := func(cypher string, params map[string]any) {
		t.Helper()
		if _, err := neo4j.ExecuteQuery(context.Background(), driver, cypher, params, neo4j.EagerResultTransformer); err != nil {
			t.Fatalf("fixture: %v\n%s", err, cypher)
		}
	}
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n.id STARTS WITH $prefix OR n.store STARTS WITH $prefix OR
			n.key STARTS WITH $prefix OR (n:GkgMeta AND n.id='product_release' AND n.value STARTS WITH $prefix)
			DETACH DELETE n`, map[string]any{"prefix": prefix})
	})
	return st, query
}

func TestReadProductsMemgraph(t *testing.T) {
	st, query := mentionDB(t, "chunk-mentions-")
	query(`CREATE (d:Document {id:'chunk-mentions-doc', domain:'catalogue', status:'fetched'}),
		(d)-[:HAS_CHUNK]->(:Chunk {id:'chunk-mentions-c0', doc_id:d.id, domain:'catalogue', seq:0,
			text:'AB-12345, ZX 98765, AM12345, UN12345, OLD12345'}),
		(:Chunk {id:'chunk-mentions-other', domain:'other-domain', seq:0, text:'AB12345'})`, nil)
	req := chunkread.Request{IDs: []string{"chunk-mentions-c0", "chunk-mentions-other"}, Domain: "catalogue", WithProducts: true}
	read := func(want ...string) chunkread.Item {
		t.Helper()
		got, err := chunkread.Read(t.Context(), st, req)
		if err != nil || len(got.Chunks) != 1 || !slices.Equal(got.Meta.Missing, req.IDs[1:]) || got.ProductsError != "" {
			t.Fatalf("Read: %+v, %v", got, err)
		}
		item := got.Chunks[0]
		if item.ProductsTotal == nil || *item.ProductsTotal != len(want) || item.Products == nil || *item.Products == nil {
			t.Fatalf("product totals: %+v, expected %v", item, want)
		}
		var ids []string
		for _, p := range *item.Products {
			ids = append(ids, p.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("product IDs = %v, want %v", ids, want)
		}
		return item
	}
	read() // knowledge-only graph, no active release
	query(`CREATE (:GkgMeta {id:'product_release', value:'chunk-mentions-release'})`, nil)
	read() // release pointer with no bound stores
	checkEmpty := func(fixture string) {
		t.Helper()
		index, err := st.LoadProductMentionIndex(t.Context())
		if err != nil || index == nil {
			t.Fatalf("%s: load index = %v, %v", fixture, index, err)
		}
		if index.Stats.IdentifierValues != 0 || index.Stats.EligibleKeys != 0 || len(index.Match("AB12345, UN12345")) != 0 {
			t.Fatalf("%s: expected an empty index, stats = %+v", fixture, index.Stats)
		}
		read()
	}
	query(`CREATE (:ProductStore {store:'chunk-mentions-a.test', domain:'product-a'})`, nil)
	checkEmpty("release and bound store with no identifiers in the graph")
	query(`CREATE (a:Product {id:'chunk-mentions-a', store:'chunk-mentions-a.test', product_id:'chunk-mentions-a.test:1', domain:'product-a', title:'A'}),
		(:ProductStore {store:'chunk-mentions-unbound.test'}),
		(u:Product {id:'chunk-mentions-u', store:'chunk-mentions-unbound.test', product_id:'chunk-mentions-unbound.test:1'}),
		(f:FeatureRev {id:'catalogue_code', key:'chunk-mentions-code'}),
		(u)-[:HAS_FEATURE {release_id:'chunk-mentions-release',value_fold:'UN12345'}]->(f)`, nil)
	checkEmpty("bound product with zero identifier edges while an unbound identifier exists")
	query(`MATCH (a:Product {id:'chunk-mentions-a'}), (f:FeatureRev {key:'chunk-mentions-code'})
		CREATE (:ProductStore {store:'chunk-mentions-b.test', domain:'product-b'}),
		(b:Product {id:'chunk-mentions-b', store:'chunk-mentions-b.test', product_id:'chunk-mentions-b.test:1', domain:'product-b', title:'B'}),
		(r:FeatureRev {id:'product_reference', key:'chunk-mentions-ref'}),
		(a)-[:HAS_FEATURE {release_id:'chunk-mentions-release',value_fold:'AB12345'}]->(f),
		(a)-[:HAS_FEATURE {release_id:'chunk-mentions-release',value_fold:'AM12345'}]->(f),
		(a)-[:HAS_FEATURE {release_id:'old-release',value_fold:'OLD12345'}]->(f),
		(b)-[:HAS_FEATURE {release_id:'chunk-mentions-release',items_fold:['ZX_98765','ZX_98765','AM-12345']}]->(r)`, nil)
	// Direct DB edits are outside the token contract. Use a fresh Store.
	st = graphtest.Store(t)
	item := read("chunk-mentions-a", "chunk-mentions-b")
	for i, want := range []chunkread.MatchedIdentifier{{Feature: "catalogue_code", Value: "AB12345"}, {Feature: "product_reference", Value: "ZX_98765"}} {
		if !slices.Equal((*item.Products)[i].Matched, []chunkread.MatchedIdentifier{want}) {
			t.Errorf("matched = %+v, want %+v", (*item.Products)[i].Matched, want)
		}
	}
	query(`MATCH (s:ProductStore {store:'chunk-mentions-unbound.test'}) SET s.domain='product-u'`, nil)
	read("chunk-mentions-a", "chunk-mentions-b", "chunk-mentions-u")

	query(`MATCH (:Product {id:'chunk-mentions-u'})-[e:HAS_FEATURE]->() SET e.value_fold=12345`, nil)
	// Use a fresh Store after the direct DB edit.
	out, err := chunkread.Read(t.Context(), graphtest.Store(t), req)
	if err != nil || len(out.Chunks) != 1 || out.ProductsError != "index_unavailable" ||
		out.Chunks[0].Products != nil || out.Chunks[0].ProductsTotal != nil {
		t.Fatalf("invalid index data: %+v, %v", out, err)
	}
}

func TestReadProductsScale(t *testing.T) {
	st, query := mentionDB(t, "chunk-scale-")
	query(`CREATE (:GkgMeta {id:'product_release', value:'chunk-scale-release'}),
		(:FeatureRev {id:'catalogue_code', key:'chunk-scale-code'})`, nil)
	for storeIdx := range 2 {
		store := fmt.Sprintf("chunk-scale-%d.test", storeIdx)
		query(`CREATE (:ProductStore {store:$store, domain:$domain, product_count:10000})`,
			map[string]any{"store": store, "domain": fmt.Sprintf("scale-%d", storeIdx)})
		for start := 0; start < 10000; start += 1000 {
			query(`MATCH (f:FeatureRev {key:'chunk-scale-code'})
				UNWIND range($start, $end) AS i
				WITH f, i, $store + ':' + toString(i) AS id, 'AB' + toString($storeIdx) + 'X' + toString(i) + 'Y' AS code
				CREATE (p:Product {id:id, store:$store, product_id:id, title:'Scale fixture product'})
				CREATE (p)-[:HAS_FEATURE {release_id:'chunk-scale-release',
					items_fold:[j IN range(0, CASE WHEN i = 0 THEN 63 ELSE 0 END) | code + 'L' + toString(j)]}]->(f)
				WITH p, f, code UNWIND range(1, 5) AS j
				CREATE (p)-[:HAS_FEATURE {release_id:'chunk-scale-release', value_fold:code + toString(j)}]->(f)`,
				map[string]any{"start": start, "end": start + 999, "store": store, "storeIdx": storeIdx})
		}
	}
	query(`UNWIND range(0,19) AS i
		CREATE (:Chunk {id:'chunk-scale-c' + toString(i), seq:i, text:'AB0X' + toString(i) + 'Y1 in this passage.'})`, nil)
	var ids []string
	for i := range 20 {
		ids = append(ids, fmt.Sprintf("chunk-scale-c%d", i))
	}
	server, client := mcpgo.NewInMemoryTransports()
	serverSession, err := mcp.NewServer(config.Config{}, st, nil).MCP().Connect(t.Context(), server, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := mcpgo.NewClient(&mcpgo.Implementation{Name: "chunk-scale-test"}, nil).Connect(t.Context(), client, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	req := &mcpgo.CallToolParams{Name: "get_chunks", Arguments: map[string]any{"ids": ids, "with_products": true}}

	const size = 120126
	checkIndex := func(index *graph.ProductMentionIndex, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("load at scale: %v", err)
		}
		if index.Stats.IdentifierValues != size || index.Stats.EligibleKeys != size || len(index.Stats.Ambiguous) != 0 {
			t.Fatalf("index size: %+v, want %d distinct eligible values", index.Stats, size)
		}
		for i := range 2 {
			store := fmt.Sprintf("chunk-scale-%d.test", i)
			stats := index.Stats.ByStore[store]
			if stats.IdentifierProducts != 10_000 || stats.EligibleProducts != 10_000 {
				t.Fatalf("%s has %+v, want 10,000 eligible products", store, stats)
			}
		}
	}
	checkIndex(st.LoadProductMentionIndex(t.Context())) // warm up
	var loads, reads []time.Duration
	for range 20 {
		start := time.Now()
		index, err := st.LoadProductMentionIndex(t.Context())
		loads = append(loads, time.Since(start))
		checkIndex(index, err)

		start = time.Now()
		res, err := clientSession.CallTool(t.Context(), req)
		reads = append(reads, time.Since(start))
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatal(res.GetError())
		}
		wire, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var out chunkread.Result
		if err := json.Unmarshal(wire, &out); err != nil {
			t.Fatal(err)
		}
		if out.ProductsError != "" || len(out.Chunks) != 20 {
			t.Fatalf("20-id read: %+v %q", out.Meta, out.ProductsError)
		}
		for _, item := range out.Chunks {
			if item.ProductsTotal == nil || *item.ProductsTotal != 1 {
				t.Fatalf("unexpected scale fixture product count: %+v", item)
			}
		}
	}
	slices.Sort(loads)
	slices.Sort(reads)
	t.Logf("20 samples, %d identifiers, index load p95=%s, 20-id MCP get_chunks p95=%s", size, loads[18], reads[18])
}
