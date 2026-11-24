package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/mcp"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

type agentProductCall struct {
	name string
	args productReadJSON
}

// askProductCalls scripts the local provider, including passing a preceding
// search's cursor to the next search when its cursor argument is null.
func askProductCalls(t *testing.T, st *graph.Store, domain string, calls ...agentProductCall) []productReadJSON {
	t.Helper()
	var modelResults []json.RawMessage
	turn := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		modelResults = modelResults[:0]
		for _, message := range req.Messages {
			if message.Role == "tool" {
				modelResults = append(modelResults, json.RawMessage(message.Content))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if turn == len(calls) {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"Product results."}}]}`)
			return
		}
		call := calls[turn]
		if cursor, ok := call.args["cursor"]; ok && cursor == nil {
			var page graph.ProductSearchResult
			if len(modelResults) == 0 || json.Unmarshal(modelResults[len(modelResults)-1], &page) != nil || page.Cursor == "" {
				t.Error("model did not receive a valid JSON continuation cursor")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			call.args["cursor"] = page.Cursor
		}
		args, err := json.Marshal(call.args)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		turn++
		fmt.Fprintf(w, `{"choices":[{"message":{"tool_calls":[{"id":"call-%d","type":"function","function":{"name":%q,"arguments":%q}}]}}]}`, turn, call.name, args)
	}))
	cfg := config.Config{}
	cfg.Agent.Enabled, cfg.Agent.BaseURL, cfg.Agent.Model, cfg.Agent.APIKey = true, stub.URL, "stub", "synthetic-key"
	response, err := agent.NewServer(cfg, st, nil).Ask(t.Context(), agent.AskRequest{Question: "Find catalog information", Domain: domain})
	stub.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.Answer != "Product results." || len(response.Evidence) != len(calls) || len(modelResults) != len(calls) {
		t.Fatalf("answer=%q, evidence=%d, model results=%d, calls=%d", response.Answer, len(response.Evidence), len(modelResults), len(calls))
	}
	results := make([]productReadJSON, 0, len(calls))
	for i, ev := range response.Evidence {
		if ev.Tool != calls[i].name || !json.Valid(modelResults[i]) || !bytes.Equal(ev.Result, modelResults[i]) {
			t.Fatalf("%s evidence and model copy differ or model JSON is invalid (%d/%d bytes)", ev.Tool, len(ev.Result), len(modelResults[i]))
		}
		t.Logf("%s model result: %d bytes", ev.Tool, len(modelResults[i]))
		results = append(results, readProductJSON(t, ev.Result))
	}
	return results
}

func TestAgentProductReadPaths(t *testing.T) {
	st, _, query := productTestDB(t)
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n:FeatureRegistry OR n:FeatureRev OR n:ProductRelease OR n:ProductStore
			OR n:Product OR (n:GkgMeta AND n.id IN ['product_release','feature_registry_import'])
			OR (n:Domain AND n.id IN ['agent-products-a','agent-products-b'])
			OR (n:Document AND n.id = 'agent-product-page') DETACH DELETE n`, nil)
	})
	query(`CREATE (:Domain {id:'agent-products-a'}), (:Domain {id:'agent-products-b'}),
		(:Document {id:'agent-product-page', domain:'agent-products-a', source:'https://example.test/item',
		 source_kind:'url', status:'done', title:'Product page', updated_at:'2026-11-21T00:00:00Z'})`, nil)
	const release, scope, otherScope = "agent-products", "agent-products-a", "agent-products-b"
	const catalogFeatures = 220
	var features, localKeys, otherKeys, triples strings.Builder
	for i := range catalogFeatures {
		id := fmt.Sprintf("zz-%03d", i)
		fmt.Fprintf(&features, `{"id":%q,"name":%q,"definition":"Fixture","kind":"code","cardinality":"multi","checkability":"page_checkable","measure":null},`, id, "Catalog entry "+strings.Repeat("<&>", 20))
		for _, store := range []struct {
			name string
			keys *strings.Builder
		}{{"example.test", &localKeys}, {"z.example.test", &otherKeys}} {
			fmt.Fprintf(store.keys, `%q:{"feature":%q,"separator":null,"source_labels":[%q],"dev_evidence":5,"numeric_search":false},`, id, id, id)
			if i != 1 || store.name != "example.test" {
				fmt.Fprintf(&triples, `{"store":%q,"feature":%q,"source_key":%q},`, store.name, id, id)
			}
		}
	}
	maxValue := strings.Repeat("\x01", 1<<10)
	specs := productReadJSON{}
	for i := range 5 {
		key := fmt.Sprintf("code-%d", i)
		fmt.Fprintf(&localKeys, `%q:{"feature":"code","separator":"/","source_labels":[%q],"dev_evidence":5,"numeric_search":false},`, key, key)
		fmt.Fprintf(&triples, `{"store":"example.test","feature":"code","source_key":%q},`, key)
		specs[key] = maxValue
	}
	fmt.Fprint(&features, `{"id":"outside","name":"Outside only","definition":"Fixture","kind":"code","cardinality":"single","checkability":"page_checkable","measure":null},`)
	fmt.Fprint(&otherKeys, `"outside":{"feature":"outside","separator":null,"source_labels":["Outside"],"dev_evidence":5,"numeric_search":false},`)
	fmt.Fprint(&triples, `{"store":"z.example.test","feature":"outside","source_key":"outside"},
		{"store":"example.test","feature":"length","source_key":"length"},
		{"store":"z.example.test","feature":"length","source_key":"length"}`)
	registry := strings.Replace(snapshotRegistry, `"features":[`, `"features":[`+features.String(), 1)
	registry = strings.Replace(registry, `"example.test":{"keys":{`, `"example.test":{"keys":{`+localKeys.String(), 1)
	registry = strings.Replace(registry, `"z.example.test":{"keys":{`, `"z.example.test":{"keys":{`+otherKeys.String(), 1)
	// The other store has a released length key but no numeric-search support.
	otherStart := strings.Index(registry, `"z.example.test"`)
	registry = registry[:otherStart] + strings.ReplaceAll(registry[otherStart:], `"numeric_search":true`, `"numeric_search":false`)
	dir := t.TempDir()
	digest := productFixtureDigest(registry)
	productCLI(t, 0, digest[:16], "features", "import", writeProductFixture(t, dir, "R.json", registry))
	candidates := fmt.Sprintf(`{"registry_sha256":%q,"keys":[%s]}`, digest, triples.String())
	candidatePath := writeProductFixture(t, dir, "candidates.json", candidates)
	productCLI(t, 0, release, productReleaseTestArgs(t, release, digest[:16], candidatePath,
		productFixturePins(registry, candidates), "example.test="+scope, "z.example.test="+otherScope)...)
	productCLI(t, 0, "activated", "products", "release", "activate", release)
	importSnapshot := func(store, data string) {
		file := writeProductFixture(t, dir, store+".jsonl", data)
		productCLI(t, 0, "imported", "products", "import", "-store", store, "-release", release, "-snapshot-seq", "1",
			"-report", filepath.Join(dir, store+"-report.json"), file)
	}
	var rows strings.Builder
	largeTitle := strings.Repeat("<", 2<<10) + strings.Repeat("X", 2<<10)
	const largeSourceID = "example.test:big-0"
	for i := range 4 {
		rows.WriteString(snapshotRecord(fmt.Sprintf("example.test:big-%d", i), largeTitle, string(mustProductJSON(t, specs))))
	}
	for i := range 22 {
		rows.WriteString(snapshotRecord(fmt.Sprintf("example.test:small-%d", i), "Small", `{"length":"20 mm"}`))
	}
	const overflowSourceID = "example.test:overflow"
	overflow := strings.Repeat("<", 4<<10)
	rows.WriteString(strings.Replace(snapshotRecord(overflowSourceID, overflow, `{"length":"30 mm"}`), `"brand":"Maker"`, fmt.Sprintf(`"brand":%q`, overflow), 1))
	importSnapshot("example.test", rows.String())
	const otherSourceID = "z.example.test:1"
	importSnapshot("z.example.test", snapshotRecord(otherSourceID, "Other", `{"length":"20 mm"}`))
	otherID := productobs.ProductID("z.example.test", otherSourceID)
	largeID := productobs.ProductID("example.test", largeSourceID)

	serverTransport, clientTransport := mcpgo.NewInMemoryTransports()
	ms, err := mcp.NewServer(config.Config{}, st, nil).MCP().Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	cs, err := mcpgo.NewClient(&mcpgo.Implementation{Name: "agent-product-test", Version: "v1"}, nil).Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	mcpTool := func(t *testing.T, name string, args productReadJSON) productReadJSON {
		t.Helper()
		result, err := cs.CallTool(t.Context(), &mcpgo.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("MCP %s = %v, %v", name, result, err)
		}
		return result.StructuredContent.(productReadJSON)
	}

	t.Run("byte_budget_cursor_and_detail", func(t *testing.T) {
		first := productReadJSON{"feature": "code", "value": maxValue, "limit": 100}
		second := productReadJSON{"feature": "code", "value": maxValue, "limit": 20, "cursor": nil}
		detail := productReadJSON{"id": largeID}
		results := askProductCalls(t, st, scope, agentProductCall{"find_products", first}, agentProductCall{"find_products", second}, agentProductCall{"get_product", detail})
		baseline := mcpTool(t, "find_products", productReadJSON{"feature": "code", "value": maxValue, "limit": 20, "domain": scope})
		var ids, wantIDs []string
		for _, hit := range baseline["hits"].([]any) {
			h := hit.(productReadJSON)
			wantIDs = append(wantIDs, h["product"].(productReadJSON)["id"].(string))
			if len(h["evidence"].([]any)) != 5 || len(h["evidence"].([]any)[0].(productReadJSON)["raw"].(string)) != 1<<10 {
				t.Fatal("expected five maximum-size evidence values per hit")
			}
		}
		for i, page := range results[:2] {
			hits := page["hits"].([]any)
			if len(hits) != 2 || (page["cursor"] != nil) != (i == 0) {
				t.Fatalf("page %d: hits=%d cursor present=%t", i+1, len(hits), page["cursor"] != nil)
			}
			for _, hit := range hits {
				h := hit.(productReadJSON)
				if h["evidence_truncated"] != true {
					t.Fatal("expected search to omit evidence within its byte budget")
				}
				ids = append(ids, h["product"].(productReadJSON)["id"].(string))
			}
		}
		if len(wantIDs) != 4 || !slices.Equal(ids, wantIDs) {
			t.Fatalf("page IDs=%v, want %v", ids, wantIDs)
		}
		full := mcpTool(t, "get_product", productReadJSON{"id": largeID, "with_mentions": true})
		got := results[2]
		if got["mentions_total"] != float64(0) {
			t.Fatalf("agent detail did not include the default mention fields: mentions_total=%v", got["mentions_total"])
		}
		n := len(got["features"].([]any))
		if n == 0 || n >= 5 || got["features_omitted"] != float64(5-n) || full["pages_missing"] != false || len(full["pages"].([]any)) != 1 {
			t.Fatalf("detail features=%d omitted=%v pages=%v", n, got["features_omitted"], got["pages"])
		}
		delete(got, "features_omitted")
		full["features"] = full["features"].([]any)[:n]
		if !reflect.DeepEqual(got, full) {
			t.Fatal("agent detail changed metadata, pages, or retained evidence")
		}
	})

	t.Run("scope_row_limits_and_unscoped_mcp_parity", func(t *testing.T) {
		res := askProductCalls(t, st, scope,
			agentProductCall{"find_products", productReadJSON{"feature": "length", "value": "20 mm"}},
			agentProductCall{"find_products", productReadJSON{"feature": "length", "value": "20 mm", "limit": 100}},
			agentProductCall{"find_products", productReadJSON{"feature": "length", "value": "20 mm", "domain": otherScope}},
			agentProductCall{"get_product", productReadJSON{"id": otherID}},
			agentProductCall{"get_product", productReadJSON{"id": "absent"}},
			agentProductCall{"get_product", productReadJSON{"store": "example.test", "product_id": largeSourceID}})
		for i, count := range []int{10, 20} {
			if len(res[i]["hits"].([]any)) != count || res[i]["cursor"] == nil {
				t.Fatalf("page %d: expected %d hits with a cursor", i, count)
			}
			for _, hit := range res[i]["hits"].([]any) {
				if hit.(productReadJSON)["product"].(productReadJSON)["domain"] != scope {
					t.Fatal("scoped search returned a product from another domain")
				}
			}
		}
		if res[2]["error"] != "domain outside the request scope" || res[3]["error"] != graph.ErrNotFound.Error() || !reflect.DeepEqual(res[3], res[4]) || res[5]["id"] != largeID {
			t.Fatal("scope mismatch, unknown product, or in-scope lookup behaved incorrectly")
		}
		searchArgs := productReadJSON{"feature": "length", "value": "20 mm", "limit": 4}
		getArgs := productReadJSON{"id": otherID}
		unscoped := askProductCalls(t, st, "", agentProductCall{"find_products", searchArgs},
			agentProductCall{"get_product", productReadJSON{"id": otherID, "with_mentions": false}})
		if !reflect.DeepEqual(unscoped[0], mcpTool(t, "find_products", searchArgs)) || !reflect.DeepEqual(unscoped[1], mcpTool(t, "get_product", getArgs)) {
			t.Fatal("small unscoped search or detail differs from MCP")
		}
		searchArgs["domain"] = otherScope
		withDomain := askProductCalls(t, st, "", agentProductCall{"find_products", searchArgs})[0]
		if !reflect.DeepEqual(withDomain, mcpTool(t, "find_products", searchArgs)) || len(withDomain["hits"].([]any)) != 1 {
			t.Fatal("unscoped ask did not pass through the model's domain filter")
		}
	})

	t.Run("metadata_alone_exceeds_budget", func(t *testing.T) {
		search := productReadJSON{"feature": "length", "value": "30 mm"}
		detail := productReadJSON{"id": productobs.ProductID("example.test", overflowSourceID)}
		full := mcpTool(t, "get_product", detail)
		delete(full, "features")
		if n := len(mustProductJSON(t, full)); n <= 32<<10 {
			t.Fatalf("metadata fixture is only %d bytes", n)
		}
		res := askProductCalls(t, st, scope, agentProductCall{"find_products", search}, agentProductCall{"get_product", detail})
		if res[0]["error"] != "product metadata exceeds the search response budget" || res[1]["error"] != "product tool metadata exceeds the agent result budget" {
			t.Fatalf("oversized metadata errors = %v", res)
		}
	})

	t.Run("feature_projection_and_query", func(t *testing.T) {
		const tool = "list_features"
		full := mcpTool(t, tool, productReadJSON{})
		listed := askProductCalls(t, st, "", agentProductCall{tool, productReadJSON{}})[0]
		all, got := full["features"].([]any), listed["features"].([]any)
		if listed["active_release_id"] != full["active_release_id"] || listed["truncated"] != true || len(got) == 0 || listed["omitted"] != float64(len(all)-len(got)) {
			t.Fatalf("listing: features=%d/%d truncated=%v omitted=%v", len(got), len(all), listed["truncated"], listed["omitted"])
		}
		for i, f := range got {
			item, original := f.(productReadJSON), all[i].(productReadJSON)
			if len(item) != 8 || item["id"] != original["id"] || item["name"] != original["name"] || item["kind"] != original["kind"] || item["cardinality"] != original["cardinality"] || item["canonical_unit"] != original["canonical_unit"] || item["released"] != original["released"] {
				t.Fatalf("bad ordered compact projection at feature %d: %v", i, item)
			}
		}
		local := askProductCalls(t, st, scope,
			agentProductCall{tool, productReadJSON{"query": "zZ-000"}},
			agentProductCall{tool, productReadJSON{"query": "oUtSiDe OnLy"}},
			agentProductCall{tool, productReadJSON{"query": "zz-001"}},
			agentProductCall{tool, productReadJSON{"query": "code"}})
		remote := askProductCalls(t, st, otherScope, agentProductCall{tool, productReadJSON{"query": "eNgTh"}})[0]
		global := askProductCalls(t, st, "", agentProductCall{tool, productReadJSON{"query": "eNgTh"}})[0]
		wantStores := []string{"example.test", "z.example.test"}
		for i, res := range []productReadJSON{local[0], remote, global} {
			features := res["features"].([]any)
			if len(features) != 1 || res["truncated"] != nil || res["omitted"] != nil {
				t.Fatalf("query %d got %d features, truncated=%v omitted=%v", i, len(features), res["truncated"], res["omitted"])
			}
			f := features[0].(productReadJSON)
			want := [][]any{{wantStores[0]}, {wantStores[1]}, {wantStores[0], wantStores[1]}}
			if !reflect.DeepEqual(f["stores"], want[i]) || f["numeric_search"] != (i == 2) || f["released"] != true {
				t.Fatalf("query %d projection = %v", i, f)
			}
		}
		if len(local[1]["features"].([]any)) != 0 {
			t.Fatal("local listing exposed an out-of-scope feature")
		}
		unreleased := local[2]["features"].([]any)[0].(productReadJSON)
		if unreleased["released"] != false || unreleased["numeric_search"] != false || len(unreleased["stores"].([]any)) != 0 {
			t.Fatalf("local listing counted a release in another scope: %v", unreleased)
		}
		code := local[3]["features"].([]any)[0].(productReadJSON)
		if !reflect.DeepEqual(code["stores"], []any{"example.test"}) {
			t.Fatalf("code store list contains duplicates: %v", code)
		}
	})
}
