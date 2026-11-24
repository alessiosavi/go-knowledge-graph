package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mentionSnapshotStore struct {
	*graph.Store
	beforeLoad func()
	afterLoad  func()
	chunkRead  bool
}

func (s *mentionSnapshotStore) ChunkNeighbourhood(ctx context.Context, id string, neighbours int) (graph.Chunk, []graph.Chunk, error) {
	center, around, err := s.Store.ChunkNeighbourhood(ctx, id, neighbours)
	s.chunkRead = err == nil
	return center, around, err
}

func (s *mentionSnapshotStore) LoadProductLinkIndex(ctx context.Context) (*graph.ProductLinkIndex, error) {
	s.beforeLoad()
	index, err := s.Store.LoadProductLinkIndex(ctx)
	s.afterLoad()
	return index, err
}

func TestChunkProductsLifecycle(t *testing.T) {
	st, _, query := productTestDB(t)
	ctx := t.Context()
	const registry = `{"schema":"gkg-features/v1","features":[{"id":"catalogue_code","name":"Catalogue code",
		"definition":"Manufacturer catalogue identifier","kind":"code","cardinality":"multi",
		"checkability":"page_checkable","measure":null}],"stores":{"mention-life.test":{"keys":{
		"sku":{"feature":"catalogue_code","separator":null,"source_labels":["SKU"],"dev_evidence":5,"numeric_search":false}
		},"ignore":{},"deferred":[]}}}`
	digest := productFixtureDigest(registry)
	rev := digest[:16]
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n.store='mention-life.test' OR n.id STARTS WITH 'mention-life-'
			OR ((n:FeatureRegistry OR n:FeatureRev) AND n.rev=$rev)
			OR (n:GkgMeta AND n.id IN ['product_release','feature_registry_import']) DETACH DELETE n`, map[string]any{"rev": rev})
	})
	query(`CREATE (:Domain {id:'mention-life-products'}), (:Domain {id:'mention-life-rebound'}), (:Domain {id:'mention-life-catalogue'})`, nil)
	dir := t.TempDir()
	productCLI(t, 0, rev, "features", "import", writeProductFixture(t, dir, "R.json", registry))
	candidates := fmt.Sprintf(`{"registry_sha256":%q,"keys":[{"store":"mention-life.test","feature":"catalogue_code","source_key":"sku"}]}`, digest)
	candidatesPath := writeProductFixture(t, dir, "candidates.json", candidates)
	activate := func(release, domain string) {
		t.Helper()
		productCLI(t, 0, release, productReleaseTestArgs(t, release, rev, candidatesPath,
			productFixturePins(registry, candidates), "mention-life.test="+domain)...)
		if err := st.ActivateProductRelease(ctx, release); err != nil {
			t.Fatal(err)
		}
	}
	record := func(id, title, code string) string {
		return fmt.Sprintf(`{"store":"mention-life.test","product_id":"mention-life.test:%s","title":%q,"brand":"","category":"","subcategory":"","link":"","product_type":null,"attributes":{"sku":%q},"page_specs":null}`, id, title, code)
	}
	snapshot := func(release string, seq int64, records ...string) {
		t.Helper()
		data := strings.Join(records, "\n") + "\n"
		report := graph.ProductImportReport{SnapshotDigest: productFixtureDigest(data)}
		err := st.ImportProductSnapshot(ctx, graph.ProductSnapshotInput{Store: "mention-life.test", Release: release, Seq: seq, AllowShrink: true},
			strings.NewReader(data), &report, nil)
		if err != nil || report.Status != "imported" {
			t.Fatalf("snapshot: %+v, %v", report, err)
		}
	}
	require := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	doc := func(hash, text string) string {
		t.Helper()
		out, err := st.UpsertDocument(ctx, graph.DocumentInput{
			Kind: "text", Domain: "mention-life-catalogue", Source: "file:mention-life", Hash: hash,
		})
		require(err)
		t.Cleanup(func() { _ = st.DeleteDocument(context.Background(), out.ID) })
		require(st.PutChunks(ctx, out.ID, []graph.Chunk{{Seq: 0, Text: text}}))
		return out.ID
	}
	oldDoc := doc("mention-life-old", "AA12345, BB12345")
	req := chunkread.Request{IDs: []string{oldDoc + "-c0"}, Domain: "mention-life-catalogue", WithProducts: true}
	read := func(store chunkread.Store) chunkread.Result {
		t.Helper()
		got, err := chunkread.Read(ctx, store, req)
		if err != nil || got.ProductsError != "" || got.LinksPartial != "" {
			t.Fatalf("read: %+v %v", got, err)
		}
		return got
	}
	titles := func(got chunkread.Result, want ...string) {
		t.Helper()
		if len(got.Chunks) != 1 || got.Chunks[0].Products == nil || len(*got.Chunks[0].Products) != len(want) {
			t.Fatalf("read: %+v, expected titles %v", got, want)
		}
		for i, p := range *got.Chunks[0].Products {
			if p.Title != want[i] {
				t.Fatalf("title = %q, want %q", p.Title, want[i])
			}
		}
	}

	const release, rebound = "mention-life-release", "mention-life-release-rebound"
	activate(release, "mention-life-products")
	snapshot(release, 1, record("a", "Initial", "AA12345"))
	titles(read(st), "Initial")

	// Commit a push between the chunk read and T1, then a delete immediately
	// after T1. The response sees the push and retains that snapshot's title.
	store := mentionSnapshotStore{Store: st}
	store.beforeLoad = func() {
		if !store.chunkRead {
			t.Fatal("loaded the index before the chunks")
		}
		got, err := st.PushProduct(ctx, 2, []byte(record("a", "Pushed", "BB12345")))
		require(err)
		if got.Status != "updated" {
			t.Fatalf("push = %+v", got)
		}
	}
	store.afterLoad = func() {
		_, err := st.RemoveProduct(ctx, "mention-life.test:a", 3)
		require(err)
	}
	current := read(&store)
	titles(current, "Pushed")
	if got := (*current.Chunks[0].Products)[0].Matched; len(got) != 1 || got[0].Value != "BB12345" {
		t.Fatalf("T1 matched evidence = %+v", got)
	}
	titles(read(st)) // product deletion
	snapshot(release, 4, record("a", "Imported", "AA12345"))
	titles(read(st), "Imported")
	snapshot(release, 5, record("b", "Replacement", "BB12345"))
	titles(read(st), "Replacement")
	require(st.DeleteProductStore(ctx, "mention-life.test"))
	titles(read(st)) // wipe
	require(st.UnbindProductStore(ctx, "mention-life.test"))
	unbound := read(st)
	titles(unbound)
	if item := unbound.Chunks[0]; *item.Products == nil || item.ProductsTotal == nil || *item.ProductsTotal != 0 {
		t.Fatalf("read after unbinding the last store: %+v, want empty products and zero total", item)
	}
	activate(rebound, "mention-life-rebound")
	titles(read(st))
	snapshot(rebound, 6, record("b", "Rebound", "BB12345"))
	titles(read(st), "Rebound")
	_, err := st.PushProduct(ctx, 7, []byte(record("b", "Updated after rebind", "BB12345")))
	require(err)
	titles(read(st), "Updated after rebind")

	newDoc := doc("mention-life-new", "BB-12345")
	require(st.Supersede(ctx, oldDoc, newDoc))
	if got := read(st); len(got.Chunks) != 0 || len(got.Meta.Missing) != 1 {
		t.Fatalf("superseded chunk is visible: %+v", got)
	}
	req.IDs = []string{newDoc + "-c0"}
	titles(read(st), "Updated after rebind")
	t.Run("mcp", func(t *testing.T) {
		server := httptest.NewServer(newAPIServer(config.Config{}, st, nil, nil, nil).Handler())
		defer server.Close()
		client := mcpgo.NewClient(&mcpgo.Implementation{Name: "gkg-test"}, nil)
		session, err := client.Connect(ctx, &mcpgo.StreamableClientTransport{
			Endpoint: server.URL + "/mcp", HTTPClient: server.Client(),
		}, nil)
		require(err)
		defer session.Close()
		res, err := session.CallTool(ctx, &mcpgo.CallToolParams{Name: "get_chunks", Arguments: map[string]any{
			"ids": req.IDs, "domain": req.Domain, "with_products": true, "products_limit": 1,
		}})
		require(err)
		if res.IsError {
			t.Fatal(res.GetError())
		}
		wire, err := json.Marshal(res.StructuredContent)
		require(err)
		var out chunkread.Result
		require(json.Unmarshal(wire, &out))
		titles(out, "Updated after rebind")
		product := (*out.Chunks[0].Products)[0]
		if got := product.Matched; len(got) != 1 || got[0].Value != "BB12345" || !slices.Equal(product.Via, []string{"code"}) {
			t.Fatalf("MCP evidence = %+v, via = %v", got, product.Via)
		}
	})
	require(st.DeleteDocument(ctx, newDoc))
	if got := read(st); len(got.Chunks) != 0 || len(got.Meta.Missing) != 1 {
		t.Fatalf("deleted chunk is visible: %+v", got)
	}
}
