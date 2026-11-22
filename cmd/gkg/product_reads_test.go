package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/mcp"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

type productReadJSON = map[string]any

func readProductJSON(t *testing.T, data []byte) productReadJSON {
	t.Helper()
	var out productReadJSON
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode product response: %v, body prefix %q", err, data[:min(200, len(data))])
	}
	return out
}

func TestProductReadPaths(t *testing.T) {
	st, _, query := productTestDB(t)
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n:FeatureRegistry OR n:FeatureRev OR n:ProductRelease OR n:ProductStore
			OR n:Product OR (n:GkgMeta AND n.id IN ['product_release','feature_registry_import'])
			OR (n:ProductRef AND n.store IN ['example.test','z.example.test'])
			OR (n:Domain AND n.id IN ['product-reads','other-product-reads'])
			OR (n:Document AND n.domain IN ['product-reads','other-product-reads']) DETACH DELETE n`, nil)
	})
	query(`CREATE (:Domain {id:'product-reads'}), (:Domain {id:'other-product-reads'})`, nil)

	dir := t.TempDir()
	registry := snapshotRegistry
	const extraKeys = 6
	var keys strings.Builder
	for i := range extraKeys {
		fmt.Fprintf(&keys, `"codes%d":{"feature":"code","separator":"/","source_labels":["Codes %d"],"dev_evidence":5,"numeric_search":false},`, i, i)
	}
	registry = strings.Replace(registry, `"example.test":{"keys":{`, `"example.test":{"keys":{`+keys.String(), 1)
	otherStore := strings.Index(registry, `"z.example.test"`)
	registry = registry[:otherStore] + strings.Replace(registry[otherStore:], `"numeric_search":true`, `"numeric_search":false`, 1)
	digest := productFixtureDigest(registry)
	registryFile := writeProductFixture(t, dir, "R.json", registry)
	productCLI(t, 0, digest[:16], "features", "import", registryFile)

	const wide, narrow = "product-reads-a", "product-reads-b"
	const length = `{"store":"example.test","feature":"length","source_key":"length"}`
	const size = `{"store":"example.test","feature":"length","source_key":"size"}`
	const codes = `{"store":"example.test","feature":"code","source_key":"codes"}`
	const otherLength = `{"store":"z.example.test","feature":"length","source_key":"length"}`
	keys.Reset()
	fmt.Fprintf(&keys, "%s,%s,%s,%s", length, size, codes, otherLength)
	for i := range extraKeys {
		fmt.Fprintf(&keys, `,{"store":"example.test","feature":"code","source_key":"codes%d"}`, i)
	}
	create := func(id, keys string) {
		t.Helper()
		body := fmt.Sprintf(`{"registry_sha256":%q,"keys":[%s]}`, digest, keys)
		path := writeProductFixture(t, dir, id+".json", body)
		productCLI(t, 0, id, productReleaseTestArgs(t, id, digest[:16], path, productFixturePins(registry, body),
			"example.test=product-reads", "z.example.test=other-product-reads")...)
	}
	create(wide, keys.String())
	create(narrow, length+","+otherLength)

	const originalLink = "https://example.test/item"
	const pageLink = "HTTPs://eXample.TEST/Item/?sku=A%2F1&z=2#frag"
	fixture := strings.Replace(snapshotRecord("example.test:1", "conflict", `{"length":"2 cm","size":"3 cm","codes":" c 1 / B2"}`), originalLink, pageLink, 1) +
		strings.Replace(snapshotRecord("example.test:2", "unsplit", `{"length":"20 mm","codes":"A/(B"}`), originalLink, "https://example.test/single", 1) +
		strings.Replace(snapshotRecord("example.test:3", "range", `{"length":"10-20 mm"}`), originalLink, "/item", 1) +
		strings.Replace(snapshotRecord("example.test:4", "no features", "{}"), originalLink, "", 1)
	importSnapshot := func(t *testing.T, store, release, seq, data, want string) {
		t.Helper()
		file := writeProductFixture(t, dir, store+"-"+seq+".jsonl", data)
		productCLI(t, 0, want, "products", "import", "-store", store, "-release", release, "-snapshot-seq", seq,
			"-report", filepath.Join(dir, store+"-"+seq+".json"), file)
	}

	handler := api.NewServer(config.Config{}, st, nil).Handler()
	httpCall := func(t *testing.T, method, path, body string, status int) (*httptest.ResponseRecorder, bool) {
		t.Helper()
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(method, path, strings.NewReader(body)))
		if r.Code != status {
			t.Errorf("%s %s: got %d, want %d, body prefix %q", method, path, r.Code, status, r.Body.Bytes()[:min(200, r.Body.Len())])
			return r, false
		}
		return r, true
	}
	rest := func(t *testing.T, method, path, body string, status int) productReadJSON {
		t.Helper()
		r, ok := httpCall(t, method, path, body, status)
		if !ok {
			t.FailNow()
		}
		return readProductJSON(t, r.Body.Bytes())
	}
	assertProductNotFound := func(t *testing.T, path string) {
		t.Helper()
		out := rest(t, http.MethodGet, path, "", http.StatusNotFound)
		detail, _ := out["detail"].(string)
		if out["error"] != "store_error" || detail == "" {
			t.Fatalf("GET %s: got %v, want store_error and a nonempty detail", path, out)
		}
	}
	search := func(t *testing.T, body string) productReadJSON {
		t.Helper()
		return rest(t, http.MethodPost, "/v1/products/search", body, 200)
	}
	code := func(t *testing.T, body, want string) {
		t.Helper()
		if out := rest(t, http.MethodPost, "/v1/products/search", body, 400); out["error"] != want {
			t.Fatalf("got %v, want %s", out, want)
		}
	}
	searchIDs := func(t *testing.T, out productReadJSON, want ...string) {
		t.Helper()
		var got []string
		for _, h := range out["hits"].([]any) {
			got = append(got, h.(productReadJSON)["product"].(productReadJSON)["product_id"].(string))
		}
		if !slices.Equal(got, want) {
			t.Fatalf("search products = %v, want %v", got, want)
		}
	}
	ctx := context.Background()
	serverTransport, clientTransport := mcpgo.NewInMemoryTransports()
	ms, err := mcp.NewServer(config.Config{}, st, nil).MCP().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	cs, err := mcpgo.NewClient(&mcpgo.Implementation{Name: "product-read-test", Version: "v1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	callTool := func(t *testing.T, name string, args productReadJSON) *mcpgo.CallToolResult {
		t.Helper()
		out, err := cs.CallTool(ctx, &mcpgo.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	tool := func(t *testing.T, name string, args productReadJSON, wantError string) productReadJSON {
		t.Helper()
		out := callTool(t, name, args)
		if wantError != "" {
			if !out.IsError || !strings.Contains(out.Content[0].(*mcpgo.TextContent).Text, wantError) {
				t.Fatalf("%s: expected %s, got %+v", name, wantError, out)
			}
			return nil
		}
		if out.IsError {
			t.Fatalf("%s: %s", name, out.Content[0].(*mcpgo.TextContent).Text)
		}
		return out.StructuredContent.(productReadJSON)
	}

	id := productobs.ProductID("example.test", "example.test:1")
	detailPath := "/v1/products/" + id
	exactPath := "/v1/products?store=example.test&product_id=" + url.QueryEscape("example.test:1")
	assertDetail := func(t *testing.T, out productReadJSON, release string, count int) {
		t.Helper()
		if out["id"] != id || out["domain"] != "product-reads" || out["title"] != "conflict" || out["product_id"] != "example.test:1" {
			t.Fatalf("unexpected product: %v", out)
		}
		features := out["features"].([]any)
		if len(features) != count {
			t.Fatalf("detail features = %v, want %d", features, count)
		}
		for _, value := range features {
			edge := value.(productReadJSON)
			feature := edge["feature"].(productReadJSON)
			if edge["release_id"] != release || feature["rev"] != digest[:16] || feature["name"] == "" || feature["cardinality"] == "" {
				t.Fatalf("unexpected edge: %v", edge)
			}
			if count == 1 && edge["source_key"] != "length" {
				t.Fatalf("unexpected source key: %v", edge)
			}
		}
	}
	checkKeys := func(t *testing.T, out productReadJSON, release string, codesReleased bool) {
		t.Helper()
		if out["active_release_id"] != release || len(out["features"].([]any)) != 2 {
			t.Fatalf("unexpected feature registry: %v", out)
		}
		for _, f := range out["features"].([]any) {
			feature := f.(productReadJSON)
			if feature["id"] == "code" {
				if feature["released"] != codesReleased || feature["kind"] != "code" || feature["cardinality"] != "multi" {
					t.Fatalf("unexpected code feature: %v", feature)
				}
				continue
			}
			if feature["canonical_unit"] != "mm" || feature["released"] != true {
				t.Fatalf("unexpected length: %v", feature)
			}
			configured := make(map[string]bool)
			for _, v := range feature["keys"].([]any) {
				key := v.(productReadJSON)
				configured[key["store"].(string)+"/"+key["source_key"].(string)] = key["numeric_search"].(bool)
			}
			if len(configured) != 3 || !configured["example.test/length"] || configured["z.example.test/length"] || configured["example.test/size"] {
				t.Fatalf("unexpected per-key numeric policy: %v", configured)
			}
		}
	}

	t.Run("unmaterialized", func(t *testing.T) {
		list := rest(t, "GET", "/v1/products/stores", "", 200)
		store := list["stores"].([]any)[0].(productReadJSON)
		if list["active_release_id"] != nil || store["wiped"] != false || store["release_id"] != nil {
			t.Fatalf("unexpected bound store: %v", list)
		}
		out := productCLI(t, 0, "example.test", "products", "pages-report", "-store", "example.test", "-json")
		const want = `[{"store":"example.test","domain":"product-reads","zero":0,"one":0,"multiple":0,"missing_product_ids":[],"total":0,"refs":0,"url":0}]`
		if strings.TrimSpace(out) != want {
			t.Fatalf("pages-report for an empty store = %s, want %s", out, want)
		}
		productCLI(t, 0, "deleted", "products", "delete-store", "example.test")
		list = rest(t, "GET", "/v1/products/stores", "", 200)
		store = list["stores"].([]any)[0].(productReadJSON)
		if store["wiped"] != true || store["wiped_seq"] != float64(0) {
			t.Fatalf("unexpected wipe before first import: %v", list)
		}
		features := rest(t, "GET", "/v1/features", "", 200)
		mcpFeatures := tool(t, "list_features", productReadJSON{}, "")
		for _, out := range []productReadJSON{features, mcpFeatures} {
			if out["active_release_id"] != nil || len(out["features"].([]any)) != 0 {
				t.Fatalf("unexpected features without an active release: %v", out)
			}
		}
		searchIDs(t, search(t, `{"feature":"length","value":"10 mm"}`))
	})
	productCLI(t, 0, "activated", "products", "release", "activate", wide)
	importSnapshot(t, "example.test", wide, "1", fixture, "imported")
	importSnapshot(t, "z.example.test", wide, "1", snapshotRecord("z.example.test:5", "other store", `{"length":"20 mm"}`), "imported")

	t.Run("endpoints_and_tools", func(t *testing.T) {
		assertDetail(t, rest(t, "GET", detailPath, "", 200), wide, 3)
		assertDetail(t, rest(t, "GET", exactPath, "", 200), wide, 3)
		assertDetail(t, tool(t, "get_product", productReadJSON{"id": id}, ""), wide, 3)
		assertDetail(t, tool(t, "get_product", productReadJSON{"store": "example.test", "product_id": "example.test:1"}, ""), wide, 3)
		rest(t, "GET", "/v1/products?store=example.test", "", 400)
		assertProductNotFound(t, "/v1/products?store=example.test&product_id=1")
		assertProductNotFound(t, "/v1/products/missing")
		checkKeys(t, rest(t, "GET", "/v1/features", "", 200), wide, true)
		checkKeys(t, tool(t, "list_features", productReadJSON{}, ""), wide, true)
		list := rest(t, "GET", "/v1/products/stores", "", 200)
		store := list["stores"].([]any)[0].(productReadJSON)
		if list["active_release_id"] != wide || store["store"] != "example.test" || store["domain"] != "product-reads" ||
			store["release_id"] != wide || store["imported_at"] == nil || store["snapshot_seq"] != float64(1) ||
			store["seen_seq"] != float64(1) || store["wiped_seq"] != float64(0) ||
			store["product_count"] != float64(4) || store["edge_count"] != float64(6) || store["wiped"] != false {
			t.Fatalf("unexpected watermark: %v", list)
		}
	})

	t.Run("invalid_domain", func(t *testing.T) {
		out := rest(t, http.MethodGet, detailPath+"?domain=Bad!", "", http.StatusBadRequest)
		if out["error"] != "invalid_request" {
			t.Errorf("invalid domain response = %v, want invalid_request", out)
		}
	})

	t.Run("pages", func(t *testing.T) {
		assertPages := func(t *testing.T, out productReadJSON, want ...string) {
			t.Helper()
			pages, ok := out["pages"].([]any)
			if !ok || len(pages) != len(want) || out["pages_missing"] != (len(want) == 0) {
				t.Fatalf("pages = %v, pages_missing = %v, want %v", out["pages"], out["pages_missing"], want)
			}
			for i, id := range want {
				page := pages[i].(productReadJSON)
				if page["id"] != id || page["title"] == "" || page["source"] == "" {
					t.Fatalf("page %d = %v, want %s with title and source", i, page, id)
				}
				if _, err := time.Parse(time.RFC3339Nano, page["updated_at"].(string)); err != nil {
					t.Fatalf("page %d updated_at: %v", i, err)
				}
			}
		}
		get := func() productReadJSON { return rest(t, "GET", detailPath, "", 200) }
		add := func(kind, source, final, domain, hash string, status graph.Status, refs ...string) graph.UpsertResult {
			t.Helper()
			doc, err := st.UpsertDocument(ctx, graph.DocumentInput{
				Kind: kind, Domain: domain, Source: source, FinalURL: final, Title: hash, Hash: hash, ProductRefs: refs,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetStatus(ctx, doc.ID, status, ""); err != nil {
				t.Fatal(err)
			}
			return doc
		}
		setUpdatedAt := func(id string, second int) {
			t.Helper()
			query(`MATCH (d:Document {id:$id}) SET d.updated_at=$at`, map[string]any{
				"id": id, "at": fmt.Sprintf("2026-01-01T00:00:0%d.000000000Z", second),
			})
		}
		const domain, other = "product-reads", "other-product-reads"
		const matching = "hTTps://EXample.tesT/Item///?sku=A%2F1&z=2#old"
		assertPages(t, get())
		assertPages(t, tool(t, "get_product", productReadJSON{"store": "example.test", "product_id": "example.test:1"}, ""))
		add("url", matching, "", other, "elsewhere", graph.StatusDone)
		add("text", pageLink, "", domain, "pending", graph.StatusReceived)
		add("url", "https://example.test/item?sku=A%2F1&z=2", "", domain, "different path case", graph.StatusDone)
		add("url", "https://example.test/Item?z=2&sku=A%2F1", "", domain, "different query order", graph.StatusDone)
		assertPages(t, get())
		for i, link := range []string{"/item", ""} {
			add("text", link, "", domain, fmt.Sprintf("invalid link %d", i), graph.StatusDone)
			productID := productobs.ProductID("example.test", fmt.Sprintf("example.test:%d", i+3))
			out := rest(t, "GET", "/v1/products/"+productID, "", 200)
			if out["link"] != link {
				t.Fatalf("product link = %q, want %q", out["link"], link)
			}
			assertPages(t, out)
		}

		first := add("file", matching, pageLink, domain, "first", graph.StatusDone)
		assertPages(t, get(), first.ID)
		exact := rest(t, "GET", exactPath, "", 200)
		assertPages(t, exact, first.ID)
		if page := exact["pages"].([]any)[0].(productReadJSON); page["source"] != matching || page["title"] != "first" {
			t.Fatalf("page metadata = %v", page)
		}
		assertPages(t, tool(t, "get_product", productReadJSON{"id": id}, ""), first.ID)

		newer := add("file", matching, "", domain, "newer", graph.StatusDone)
		if newer.PreviousID != first.ID {
			t.Fatalf("previous id = %s, want %s", newer.PreviousID, first.ID)
		}
		variant := add("file", pageLink, "", domain, "canonical variant", graph.StatusDone)
		setUpdatedAt(first.ID, 1)
		setUpdatedAt(variant.ID, 2)
		setUpdatedAt(newer.ID, 3)
		assertPages(t, get(), newer.ID)
		productCLI(t, 0, "zero=3 one=1 multiple=0", "products", "pages-report", "-store", "example.test")

		otherKind := add("url", pageLink, "", domain, "different kind", graph.StatusDone)
		setUpdatedAt(otherKind.ID, 1)
		assertPages(t, get(), newer.ID, otherKind.ID)
		for _, doc := range []graph.UpsertResult{variant, otherKind} {
			if err := st.DeleteDocument(ctx, doc.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.Supersede(ctx, first.ID, newer.ID); err != nil {
			t.Fatal(err)
		}
		assertPages(t, get(), newer.ID)
		if err := st.DeleteDocument(ctx, newer.ID); err != nil {
			t.Fatal(err)
		}
		assertPages(t, get())

		var competing []string
		for i, second := range []int{2, 4, 1, 4} {
			source := fmt.Sprintf("https://redirect.example.test/%d", i)
			d := add("url", source, matching, domain, source, graph.StatusDone)
			setUpdatedAt(d.ID, second)
			competing = append(competing, d.ID)
		}
		assertPages(t, get(), min(competing[1], competing[3]), max(competing[1], competing[3]), competing[0])
		add("url", "https://EXAMPLE.test/single/#page", "https://example.test/single", domain, "single", graph.StatusDone)

		refsOnly := add("text", "refs-only", "", domain, "refs only", graph.StatusDone, "example.test:99", "example.test:4")
		for _, status := range []graph.Status{graph.StatusSuperseded, graph.StatusFailed, graph.StatusReceived} {
			add("text", "ref-"+string(status), "", domain, string(status), status, "example.test:4")
		}
		noLink := productobs.ProductID("example.test", "example.test:4")
		// The cross-domain ref wins over a different document with the same
		// canonical source. The other ref document also matches the product URL.
		cross := add("url", "https://REDIRECT.example.test/1/#ref", "", other, "cross-domain", graph.StatusDone,
			"example.test:1", "z.example.test:99")
		same := add("url", "HTTPS://EXAMPLE.TEST/Item/?sku=A%2F1&z=2#refs", "", domain, "both branches", graph.StatusDone, "example.test:1")
		setUpdatedAt(cross.ID, 4)
		setUpdatedAt(same.ID, 1)
		t.Run("refs_and_scope", func(t *testing.T) {
			assertPages(t, rest(t, "GET", "/v1/products/"+noLink, "", 200), refsOnly.ID)
			assertPages(t, rest(t, "GET", detailPath, "", 200), cross.ID, same.ID, competing[3])
			assertPages(t, rest(t, "GET", detailPath+"?domain="+domain, "", 200), same.ID, min(competing[1], competing[3]), max(competing[1], competing[3]))
			scoped := rest(t, "GET", exactPath+"&domain="+other, "", 200)
			assertDetail(t, scoped, wide, 3)
			assertPages(t, scoped, cross.ID)
			assertPages(t, rest(t, "GET", "/v1/products/"+noLink+"?domain="+other, "", 200))
			assertPages(t, tool(t, "get_product", productReadJSON{"id": id, "domain": other}, ""), cross.ID)
		})
		setUpdatedAt(same.ID, 4)
		t.Run("refs_tie_break", func(t *testing.T) {
			// cross was inserted first, but same has the lower graph ID.
			if cross.ID <= same.ID {
				t.Fatalf("expected refs inserted in descending ID order, got %s then %s", cross.ID, same.ID)
			}
			assertPages(t, rest(t, "GET", detailPath, "", 200), same.ID, cross.ID, competing[3])
		})
		t.Run("document_products_dtos", func(t *testing.T) {
			assertProducts := func(doc productReadJSON) {
				t.Helper()
				got, ok := doc["products"].([]any)
				if !ok || !slices.Equal(got, []any{"example.test:4", "example.test:99"}) {
					t.Errorf("document %s products = %v, want canonical refs", doc["id"], doc["products"])
				}
			}
			assertListed := func(out productReadJSON) {
				t.Helper()
				for _, raw := range out["documents"].([]any) {
					if doc := raw.(productReadJSON); doc["id"] == refsOnly.ID {
						assertProducts(doc)
						return
					}
				}
				t.Errorf("document %s missing from list", refsOnly.ID)
			}
			docPath := "/v1/documents/" + refsOnly.ID
			assertProducts(rest(t, "GET", docPath, "", 200))
			assertProducts(rest(t, "GET", docPath+"/status", "", 200))
			assertListed(rest(t, "GET", "/v1/documents?domain="+domain, "", 200))
			assertListed(tool(t, "list_documents", productReadJSON{"domain": domain}, ""))
			assertListed(tool(t, "status", productReadJSON{"document_id": refsOnly.ID}, ""))
			if doc := rest(t, "GET", "/v1/documents/"+competing[0], "", 200); doc["products"] != nil {
				t.Errorf("URL-only document products = %v, want absent", doc["products"])
			}
		})
		missing := productobs.ProductID("example.test", "example.test:3")
		out := productCLI(t, 0, "example.test", "products", "pages-report", "-json")
		want := fmt.Sprintf(`[{"store":"example.test","domain":"product-reads","zero":1,"one":2,"multiple":1,"missing_product_ids":[%q],"total":7,"refs":3,"url":6},`+
			`{"store":"z.example.test","domain":"other-product-reads","zero":1,"one":0,"multiple":0,"missing_product_ids":[%q],"total":0,"refs":0,"url":0}]`,
			missing, productobs.ProductID("z.example.test", "z.example.test:5"))
		if strings.TrimSpace(out) != want {
			t.Fatalf("pages-report = %s, want %s", out, want)
		}
		out = productCLI(t, 0, "total=7 refs=3 url=6", "products", "pages-report", "-store", "example.test")
		if strings.Contains(out, "z.example.test") {
			t.Fatalf("pages-report included another store: %s", out)
		}
		productCLI(t, 1, "missing.test", "products", "pages-report", "-store", "missing.test")
		for _, doc := range []graph.UpsertResult{refsOnly, cross, same} {
			if err := st.DeleteDocument(ctx, doc.ID); err != nil {
				t.Fatal(err)
			}
		}

		t.Run("bare_host", func(t *testing.T) {
			setLink := func(link string) {
				query(`MATCH (p:Product {id:$id}) SET p.link=$link`, map[string]any{"id": id, "link": link})
			}
			t.Cleanup(func() { setLink(pageLink) })
			for _, tc := range []struct{ link, source string }{
				{"https://User:Pass@bare.example.test", "HTTPS://User:Pass@BARE.EXAMPLE.TEST///#root"},
				{"https://bücher.test", "https://BÜCHER.TEST/"},
				{"https://kelvin.test", "https://KELVIN.TEST/"},
			} {
				setLink(tc.link)
				doc := add("url", tc.source, "", domain, tc.source, graph.StatusDone)
				assertPages(t, get(), doc.ID)
			}
		})
	})

	t.Run("semantics", func(t *testing.T) {
		for _, tc := range []struct {
			name, body, errorCode string
			ids                   []string
		}{
			{"single", `{"feature":"length","value":" ２  cm "}`, "", []string{"example.test:1"}},
			{"multi", `{"feature":"code","value":" c   1 "}`, "", []string{"example.test:1"}},
			{"unsplit_whole", `{"feature":"code","value":"a/(b"}`, "", []string{"example.test:2"}},
			{"unsplit_fragment", `{"feature":"code","value":"a"}`, "", nil},
			{"conflicting_alias", `{"feature":"length","value":"3 cm"}`, "", []string{"example.test:1"}},
			{"inclusive_exact_only", `{"domain":"product-reads","feature":"length","min":"20.000000","max":"20"}`, "", []string{"example.test:1", "example.test:2"}},
			{"numeric_max", `{"feature":"length","max":"19.999999"}`, "", nil},
			{"not_numeric", `{"feature":"code","min":"1"}`, "not_numeric", nil},
			{"numeric_scope", `{"domain":"other-product-reads","feature":"length","min":"1"}`, "not_numeric", nil},
			{"domain_filter", `{"domain":"other-product-reads","feature":"length","value":"20 mm"}`, "", []string{"z.example.test:5"}},
			{"empty_domain", `{"domain":"missing-product-domain","feature":"length","value":"20 mm"}`, "", nil},
			{"unknown_feature_value", `{"feature":"missing-feature","value":"20 mm"}`, "", nil},
			{"unknown_feature_numeric", `{"feature":"missing-feature","min":"20"}`, "", nil},
			{"bad_precision", `{"feature":"length","min":"0.0000001"}`, "invalid_request", nil},
			{"reversed_range", `{"feature":"length","min":"21","max":"20"}`, "invalid_request", nil},
			{"ambiguous_match", `{"feature":"length","min":"1","value":"2 cm"}`, "invalid_request", nil},
			{"over_limit", `{"feature":"length","min":"1","limit":51}`, "invalid_request", nil},
			{"bad_cursor", `{"feature":"length","min":"1","cursor":"!!!"}`, "invalid_request", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if tc.errorCode != "" {
					code(t, tc.body, tc.errorCode)
					return
				}
				slices.SortFunc(tc.ids, func(a, b string) int {
					storeA, _, _ := strings.Cut(a, ":")
					storeB, _, _ := strings.Cut(b, ":")
					return strings.Compare(productobs.ProductID(storeA, a), productobs.ProductID(storeB, b))
				})
				searchIDs(t, search(t, tc.body), tc.ids...)
			})
		}
		out := tool(t, "find_products", productReadJSON{"feature": "length", "value": "3 cm"}, "")
		searchIDs(t, out, "example.test:1")
		evidence := out["hits"].([]any)[0].(productReadJSON)["evidence"].([]any)
		if len(evidence) != 1 || evidence[0].(productReadJSON)["source_key"] != "size" || evidence[0].(productReadJSON)["conflict"] != true {
			t.Fatalf("conflicting evidence: %v", evidence)
		}
		tool(t, "find_products", productReadJSON{"feature": "code", "min": "1"}, "not_numeric")
		searchIDs(t, tool(t, "find_products", productReadJSON{"feature": "missing-feature", "value": "20 mm"}, ""))
		searchIDs(t, tool(t, "find_products", productReadJSON{"feature": "missing-feature", "min": "20"}, ""))
	})

	t.Run("mixed_numeric_stores", func(t *testing.T) {
		args := productReadJSON{"feature": "length", "min": "20", "max": "20"}
		for _, out := range []productReadJSON{
			search(t, string(mustProductJSON(t, args))),
			tool(t, "find_products", args, ""),
		} {
			searchIDs(t, out, "example.test:1", "example.test:2")
			if stores, ok := out["numeric_excluded_stores"].([]any); !ok || !slices.Equal(stores, []any{"z.example.test"}) {
				t.Fatalf("numeric_excluded_stores = %v, want [z.example.test]", out["numeric_excluded_stores"])
			}
		}
		out := search(t, `{"feature":"length","max":"19.999999"}`)
		searchIDs(t, out)
		if stores, ok := out["numeric_excluded_stores"].([]any); !ok || !slices.Equal(stores, []any{"z.example.test"}) {
			t.Fatalf("numeric_excluded_stores on an empty page = %v, want [z.example.test]", out["numeric_excluded_stores"])
		}
	})

	const page = `{"feature":"length","min":"20","limit":1%s}`
	var cursor string
	t.Run("paging", func(t *testing.T) {
		first := search(t, fmt.Sprintf(page, ""))
		cursor = first["cursor"].(string)
		second := search(t, fmt.Sprintf(page, fmt.Sprintf(`,"cursor":%q`, cursor)))
		a, b := "example.test:1", "example.test:2"
		if productobs.ProductID("example.test", a) > productobs.ProductID("example.test", b) {
			a, b = b, a
		}
		searchIDs(t, first, a)
		searchIDs(t, second, b)
		if second["cursor"] != nil {
			t.Fatalf("unexpected extra page: %v", second)
		}
		mcpFirst := tool(t, "find_products", productReadJSON{"feature": "length", "min": "20", "limit": 1}, "")
		mcpCursor := mcpFirst["cursor"].(string)
		mcpSecond := tool(t, "find_products", productReadJSON{"feature": "length", "min": "20", "limit": 1, "cursor": mcpCursor}, "")
		searchIDs(t, mcpFirst, a)
		searchIDs(t, mcpSecond, b)
		if mcpSecond["cursor"] != nil {
			t.Fatalf("unexpected extra MCP page: %v", mcpSecond)
		}
		for _, tc := range []struct {
			name string
			args productReadJSON
		}{
			{"domain", productReadJSON{"domain": "product-reads", "feature": "length", "min": "20", "limit": 1}},
			{"feature", productReadJSON{"feature": "missing-feature", "min": "20", "limit": 1}},
			{"min", productReadJSON{"feature": "length", "min": "19", "limit": 1}},
			{"max", productReadJSON{"feature": "length", "min": "20", "max": "20", "limit": 1}},
			{"limit", productReadJSON{"feature": "length", "min": "20", "limit": 2}},
			{"mode", productReadJSON{"feature": "length", "value": "20 mm", "limit": 1}},
		} {
			t.Run("changed_"+tc.name, func(t *testing.T) {
				tc.args["cursor"] = cursor
				code(t, string(mustProductJSON(t, tc.args)), "stale_cursor")
			})
		}
		t.Run("changed_value", func(t *testing.T) {
			first := search(t, `{"feature":"length","value":"20 mm","limit":1}`)
			code(t, fmt.Sprintf(`{"feature":"length","value":"2 cm","limit":1,"cursor":%q}`, first["cursor"]), "stale_cursor")
		})
		t.Run("changed_mcp_filter", func(t *testing.T) {
			tool(t, "find_products", productReadJSON{"feature": "length", "min": "19", "limit": 1, "cursor": mcpCursor}, "stale_cursor")
		})
		importSnapshot(t, "example.test", wide, "2", strings.Replace(fixture, "no features", "updated", 1), "imported")
		code(t, fmt.Sprintf(page, fmt.Sprintf(`,"cursor":%q`, cursor)), "stale_cursor")
		cursor = search(t, fmt.Sprintf(page, ""))["cursor"].(string)
		importSnapshot(t, "example.test", wide, "3", strings.Replace(fixture, "no features", "updated", 1), "unchanged")
		code(t, fmt.Sprintf(page, fmt.Sprintf(`,"cursor":%q`, cursor)), "stale_cursor")
		cursor = search(t, fmt.Sprintf(page, ""))["cursor"].(string)
	})

	t.Run("release_visibility", func(t *testing.T) {
		productCLI(t, 0, "activated", "products", "release", "activate", narrow)
		if cursor != "" {
			code(t, fmt.Sprintf(page, fmt.Sprintf(`,"cursor":%q`, cursor)), "stale_cursor")
		}
		assertDetail(t, rest(t, "GET", detailPath, "", 200), narrow, 0)
		assertDetail(t, tool(t, "get_product", productReadJSON{"id": id}, ""), narrow, 0)
		searchIDs(t, search(t, `{"feature":"length","value":"2 cm"}`))
		checkKeys(t, rest(t, "GET", "/v1/features", "", 200), narrow, false)
		checkKeys(t, tool(t, "list_features", productReadJSON{}, ""), narrow, false)
		gone := query(`MATCH ()-[e:HAS_FEATURE]->() WHERE e.release_id=$release AND e.source_key <> 'length' RETURN count(e)`, map[string]any{"release": wide})
		if gone[0].Values[0] != int64(0) {
			t.Fatalf("A-only edges survived activation: %v", gone[0].Values)
		}
		importSnapshot(t, "example.test", narrow, "4", fixture, "imported")
		importSnapshot(t, "z.example.test", narrow, "2", snapshotRecord("z.example.test:5", "other store", `{"length":"20 mm"}`), "imported")
		assertDetail(t, rest(t, "GET", detailPath, "", 200), narrow, 1)
		assertDetail(t, rest(t, "GET", exactPath, "", 200), narrow, 1)
		assertDetail(t, tool(t, "get_product", productReadJSON{"id": id}, ""), narrow, 1)
		searchIDs(t, search(t, `{"feature":"length","value":"3 cm"}`))
		searchIDs(t, search(t, `{"feature":"code","value":"B2"}`))
		searchIDs(t, tool(t, "find_products", productReadJSON{"feature": "length", "value": "3 cm"}, ""))
		searchIDs(t, tool(t, "find_products", productReadJSON{"feature": "code", "value": "B2"}, ""))
		gone = query(`MATCH ()-[e:HAS_FEATURE {release_id:$release}]->() RETURN count(e)`, map[string]any{"release": wide})
		if gone[0].Values[0] != int64(0) {
			t.Fatalf("A edges survived B imports: %v", gone[0].Values)
		}
	})

	t.Run("wipe", func(t *testing.T) {
		first := search(t, fmt.Sprintf(page, ""))["cursor"].(string)
		productCLI(t, 0, "deleted", "products", "delete-store", "example.test")
		code(t, fmt.Sprintf(page, fmt.Sprintf(`,"cursor":%q`, first)), "stale_cursor")
		list := rest(t, "GET", "/v1/products/stores", "", 200)
		store := list["stores"].([]any)[0].(productReadJSON)
		if list["active_release_id"] != narrow || store["release_id"] != nil || store["imported_at"] != nil ||
			store["wiped_seq"] != store["seen_seq"] || store["snapshot_seq"] != float64(4) ||
			store["product_count"] != float64(0) || store["edge_count"] != float64(0) || store["wiped"] != true {
			t.Fatalf("unexpected wiped watermark: %v", store)
		}
		assertProductNotFound(t, detailPath)
	})

	t.Run("response_budget", func(t *testing.T) {
		productCLI(t, 0, "activated", "products", "release", "activate", wide)
		raw := strings.Repeat("\x01", 1000)
		attrs := make(map[string]string)
		for i := range extraKeys {
			attrs[fmt.Sprintf("codes%d", i)] = raw
		}
		encoded, err := json.Marshal(attrs)
		if err != nil {
			t.Fatal(err)
		}
		var rows strings.Builder
		for i := range 50 {
			rows.WriteString(snapshotRecord(fmt.Sprintf("example.test:large-%d", i), "large", string(encoded)))
		}
		importSnapshot(t, "example.test", wide, "5", rows.String(), "imported")
		args := productReadJSON{"feature": "code", "value": raw, "limit": 50}
		body := mustProductJSON(t, args)
		r, ok := httpCall(t, "POST", "/v1/products/search", string(body), 200)
		if !ok {
			t.FailNow()
		}
		if r.Body.Len() > 1<<20 {
			t.Fatalf("search response exceeded 1 MiB: %d", r.Body.Len())
		}
		out := readProductJSON(t, r.Body.Bytes())
		hits := out["hits"].([]any)
		if len(hits) != 50 {
			t.Fatalf("budget must preserve hit metadata when it fits; got %d hits", len(hits))
		}
		var belowEdgeLimit bool
		for _, item := range hits {
			h := item.(productReadJSON)
			edges := h["evidence"].([]any)
			count := len(edges)
			if count > 5 || h["evidence_truncated"] != true {
				t.Fatalf("capped evidence count=%d, flag=%v", count, h["evidence_truncated"])
			}
			for i, edge := range edges {
				if want := fmt.Sprintf("codes%d", i); edge.(productReadJSON)["source_key"] != want {
					t.Fatalf("evidence edge %d = %v, want source key %s", i, edge.(productReadJSON)["source_key"], want)
				}
			}
			belowEdgeLimit = belowEdgeLimit || count < 5
		}
		mcpPage := callTool(t, "find_products", args)
		if mcpPage.IsError || len(mcpPage.StructuredContent.(productReadJSON)["hits"].([]any)) != 50 {
			t.Fatal("MCP did not return the same 50-hit page")
		}
		if data := mustProductJSON(t, mcpPage); len(data) > 1<<20 {
			t.Fatalf("MCP search result exceeded 1 MiB: %d", len(data))
		}
		if !belowEdgeLimit {
			t.Fatal("expected evidence truncation for the byte budget")
		}
		if n := len(search(t, fmt.Sprintf(`{"feature":"code","value":%s}`, string(mustProductJSON(t, raw))))["hits"].([]any)); n != 20 {
			t.Fatalf("default page length = %d", n)
		}
	})

	t.Run("metadata_budget_and_reactivation", func(t *testing.T) {
		var rows strings.Builder
		for i := range 51 {
			rows.WriteString(snapshotRecord(fmt.Sprintf("example.test:metadata-%d", i), strings.Repeat("<", 1<<12), `{"length":"20 mm"}`))
		}
		importSnapshot(t, "example.test", wide, "6", rows.String(), "imported")
		const bounds = `{"feature":"length","min":"20","limit":50%s}`
		first := search(t, fmt.Sprintf(bounds, ""))
		pageSize := len(first["hits"].([]any))
		if pageSize == 0 || pageSize >= 50 || len(mustProductJSON(t, first)) >= 1<<20 {
			t.Fatalf("unexpected metadata page size: %d hits, %d bytes", pageSize, len(mustProductJSON(t, first)))
		}
		continuation := fmt.Sprintf(bounds, fmt.Sprintf(`,"cursor":%q`, first["cursor"]))
		second := search(t, continuation)
		seen := make(map[string]bool)
		for _, out := range []productReadJSON{first, second} {
			for _, item := range out["hits"].([]any) {
				id := item.(productReadJSON)["product"].(productReadJSON)["id"].(string)
				if seen[id] {
					t.Fatalf("repeated product across byte-bounded pages: %s", id)
				}
				seen[id] = true
			}
		}
		if len(seen) != 51 || second["cursor"] != nil {
			t.Fatalf("expected all 51 products across two pages, got %d", len(seen))
		}
		productCLI(t, 0, "activated", "products", "release", "activate", narrow)
		productCLI(t, 0, "activated", "products", "release", "activate", wide)
		code(t, continuation, "stale_cursor")
	})
}

func mustProductJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
