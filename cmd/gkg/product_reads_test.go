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

	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
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
			OR (n:Domain AND n.id IN ['product-reads','other-product-reads']) DETACH DELETE n`, nil)
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

	fixture := snapshotRecord("example.test:1", "conflict", `{"length":"2 cm","size":"3 cm","codes":" c 1 / B2"}`) +
		snapshotRecord("example.test:2", "unsplit", `{"length":"20 mm","codes":"A/(B"}`) +
		snapshotRecord("example.test:3", "range", `{"length":"10-20 mm"}`) +
		snapshotRecord("example.test:4", "no features", "{}")
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
		rest(t, "GET", "/v1/products?store=example.test&product_id=1", "", 404)
		rest(t, "GET", "/v1/products/missing", "", 404)
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
		rest(t, "GET", detailPath, "", 404)
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
