package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/mcp"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestProductMentionsWires(t *testing.T) {
	st, driver, query := productTestDB(t)
	ctx := t.Context()
	const store, sourceID, scope = "mention-wire.test", "mention-wire.test:42", "mention-wire-main"
	id := productobs.ProductID(store, sourceID)
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n.id=$id OR n.id='mention-wire-extra' OR n.store=$store OR n.key STARTS WITH 'mention-wire-'
		    OR (n:GkgMeta AND n.id='product_release' AND n.value='mention-wire-release') DETACH DELETE n`,
			map[string]any{"id": id, "store": store})
	})
	query(`CREATE (p:Product {id:$id,store:$store,product_id:$pid,title:'Wire Product'}),
		(:ProductStore {store:$store,domain:$scope}), (:Product {id:'mention-wire-extra',store:$store}),
		(:FeatureRev {key:'mention-wire-code',id:'catalogue_code',kind:'code'})`, map[string]any{"id": id, "store": store, "pid": sourceID, "scope": scope})
	seed := func(source, domain, text string, n int) string {
		t.Helper()
		r, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Domain: domain, Source: source, Title: "Title " + source, Hash: "v1"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := st.DeleteDocument(context.Background(), r.ID); err != nil {
				t.Error(err)
			}
		})
		cs := make([]graph.Chunk, n)
		for i := range cs {
			cs[i] = graph.Chunk{Seq: i, Text: text}
		}
		if err := st.PutChunks(ctx, r.ID, cs); err != nil {
			t.Fatal(err)
		}
		if err := st.SetStatus(ctx, r.ID, graph.StatusDone, ""); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	text := strings.Repeat("界", 100) + "; CODE00001; CODE00002; CODE00003; CODE00004; CODE00005; CODE00006; CODE00007; CODE00008; CODE00009"
	seed("z-mention-wire", scope, text, 1)
	first := seed("a-mention-wire", scope, text, 5)
	seed("0-mention-wire", "mention-wire-other", text, 1)
	var router http.Handler
	var client *mcpgo.ClientSession
	useStore := func(store *graph.Store) {
		t.Helper()
		router = api.NewServer(config.Config{}, store, nil).Handler()
		serverTransport, clientTransport := mcpgo.NewInMemoryTransports()
		server, err := mcp.NewServer(config.Config{}, store, nil).MCP().Connect(ctx, serverTransport, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		nextClient, err := mcpgo.NewClient(&mcpgo.Implementation{Name: "mention-wire-tests"}, nil).Connect(ctx, clientTransport, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = nextClient.Close() })
		client = nextClient
	}
	useStore(st)
	rest := func(path string, status int) []byte {
		t.Helper()
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != status {
			t.Fatalf("%s: status %d body %s, want %d", path, r.Code, r.Body.Bytes(), status)
		}
		return r.Body.Bytes()
	}
	paths := []string{"/v1/products/" + id + "?domain=" + scope, "/v1/products?store=" + store + "&product_id=" + sourceID + "&domain=" + scope}
	tool := func(args string, failure string) []byte {
		t.Helper()
		got, err := client.CallTool(ctx, &mcpgo.CallToolParams{Name: "get_product", Arguments: json.RawMessage(args)})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Content) != 1 {
			t.Fatalf("tool: %+v", got)
		}
		body := []byte(got.Content[0].(*mcpgo.TextContent).Text)
		if got.IsError != (failure != "") || !bytes.Contains(body, []byte(failure)) {
			t.Fatalf("tool error=%t body=%s, expected error %q", got.IsError, body, failure)
		}
		if failure == "" {
			structured, err := json.Marshal(got.StructuredContent)
			if err != nil || !bytes.Equal(structured, body) {
				t.Fatalf("structured %s, content %s, error %v", structured, body, err)
			}
		}
		return body
	}
	base := `{"id":"` + id + `","domain":"` + scope + `"`
	current, err := st.GetProduct(ctx, graph.ProductLookup{ID: id, Domain: scope})
	if err != nil {
		t.Fatal(err)
	}
	original, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		old := rest(path, 200)
		if !bytes.Equal(old, append(slices.Clone(original), '\n')) {
			t.Fatalf("legacy REST bytes: %s", old)
		}
		if !bytes.Equal(old, rest(path+"&with_mentions=false&mentions_limit=1", 200)) {
			t.Fatal("REST off changed bytes")
		}
		for _, q := range []string{"&mentions_limit=0", "&mentions_limit=21", "&mentions_limit=1.0", "&mentions_limit=", "&with_mentions=maybe"} {
			rest(path+q, 400)
		}
	}
	// The MCP SDK serializes the original struct via an object to apply schema defaults.
	oldMCP, err := json.Marshal(readProductJSON(t, original))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldMCP, tool(base+"}", "")) || !bytes.Equal(oldMCP, tool(base+`,"with_mentions":false,"mentions_limit":1}`, "")) {
		t.Fatal("MCP off changed bytes")
	}
	for _, arg := range []string{`"mentions_limit":0`, `"mentions_limit":21`, `"mentions_limit":1.0`, `"mentions_limit":1e1`, `"mentions_limit":null`, `"with_mentions":"true"`} {
		name, _, _ := strings.Cut(arg, ":")
		tool(base+","+arg+"}", strings.Trim(name, `"`))
	}
	readBoth := func(suffix string, wantTotal, wantShown, wantKeys int, wantError string, wantPartial bool) {
		t.Helper()
		var bodies [][]byte
		for _, path := range paths {
			url := path + "&with_mentions=true"
			if suffix != "" {
				url += "&mentions_limit=" + suffix
			}
			bodies = append(bodies, rest(url, 200))
		}
		args := base + `,"with_mentions":true`
		if suffix != "" {
			args += `,"mentions_limit":` + suffix
		}
		bodies = append(bodies, tool(args+"}", ""))
		for _, body := range bodies {
			fields := readProductJSON(t, body)
			if fields["title"] != "Wire Product" || fields["features"] == nil || fields["pages"] == nil {
				t.Fatalf("lost base product: %s", body)
			}
			mentions, ok := fields["mentions"].([]any)
			if wantError != "" {
				if _, exists := fields["mentions"]; exists || fields["mentions_error"] != wantError || fields["mentions_total"] != nil ||
					fields["mentions_keys"] != nil || fields["mentions_partial"] != nil {
					t.Fatalf("error shape: %s", body)
				}
				continue
			}
			_, hasError := fields["mentions_error"]
			if !ok || len(mentions) != wantShown || fields["mentions_total"] != float64(wantTotal) ||
				fields["mentions_keys"] != float64(wantKeys) || fields["mentions_partial"] != wantPartial || hasError {
				t.Fatalf("success shape: %s", body)
			}
			for i := range mentions {
				m := mentions[i].(map[string]any)
				code := m["matched"].([]any)
				if m["doc_id"] != first || m["seq"] != float64(i) || m["domain"] != scope || m["doc_title"] != "Title a-mention-wire" ||
					m["doc_source"] != "a-mention-wire" || m["chunk_id"] != graph.ChunkID(first, i) || len(code) != 8 ||
					!strings.Contains(m["excerpt"].(string), "CODE00001") || len(m["excerpt"].(string)) > 300 {
					t.Fatalf("order/evidence: %v", m)
				}
				for j, item := range code {
					if got := item.(map[string]any); got["feature"] != "catalogue_code" || got["value"] != "CODE0000"+string(rune('1'+j)) {
						t.Fatalf("evidence %v", code)
					}
				}
			}
		}
	}
	readBoth("", 0, 0, 0, "", false)
	query(`MATCH (p:Product {id:$id}),(f:FeatureRev {key:'mention-wire-code'})
	   CREATE (:GkgMeta {id:'product_release',value:'mention-wire-release'}),
	   (p)-[:HAS_FEATURE {release_id:'mention-wire-release',items_fold:[i IN range(1,9) | 'CODE0000'+toString(i)]}]->(f)`, map[string]any{"id": id})
	readBoth("", 6, 5, 9, "", false)
	readBoth("1", 6, 1, 9, "", false)
	const setStoreDomain = `MATCH (s:ProductStore {store:$store}) SET s.domain=$domain`
	query(setStoreDomain, map[string]any{"store": store, "domain": nil})
	readBoth("", 0, 0, 0, "", false)
	query(setStoreDomain, map[string]any{"store": store, "domain": scope})
	query(`MATCH (p:Product {id:$id})-[e:HAS_FEATURE]->()
		SET e.items_fold=['ABCDEé12345']`, map[string]any{"id": id})
	// Direct DB edits are outside the token contract. Use a fresh Store.
	useStore(graphtest.Store(t))
	readBoth("", 0, 0, 0, "", true)
	query(`MATCH (p:Product {id:$id})-[e:HAS_FEATURE]->()
		SET e.items_fold=[i IN range(1,9) | 'CODE0000'+toString(i)]`, map[string]any{"id": id})
	query(`MATCH (p:Product {id:'mention-wire-extra'}),(f:FeatureRev {key:'mention-wire-code'})
		CREATE (p)-[:HAS_FEATURE {release_id:'mention-wire-release',value_fold:12345}]->(f)`, nil)
	// Use a fresh Store for each raw edit so failure backoff cannot mask the next wire shape.
	useStore(graphtest.Store(t))
	readBoth("", 0, 0, 0, "index_unavailable", false)
	rest("/v1/products/mention-wire-missing?with_mentions=true", 404)
	rest("/v1/products?store="+store+"&product_id=missing&with_mentions=true", 404)
	tool(`{"id":"mention-wire-missing","with_mentions":true}`, "not found")
	query(`MATCH (:Product {id:'mention-wire-extra'})-[e:HAS_FEATURE]->() SET e.value_fold=null, e.items_fold=[i IN range(1,200001) | 'OVERRIDE12345']
		WITH e MATCH (s:ProductStore {store:$store}) SET s.domain=null
		WITH s CREATE (s2:ProductStore {store:'mention-wire-bound.test',domain:'mention-wire-other'}), (b:Product {id:'mention-wire-gate',store:s2.store})
		WITH b MATCH (p:Product {id:'mention-wire-extra'}) SET p.store=b.store`, map[string]any{"store": store})
	t.Cleanup(func() { query(`MATCH (n) WHERE n.store='mention-wire-bound.test' DETACH DELETE n`, nil) })
	useStore(graphtest.Store(t))
	readBoth("", 0, 0, 0, "index_too_large", false)
	query(`MATCH (:Product {id:'mention-wire-extra'})-[e:HAS_FEATURE]->() DELETE e WITH count(*) AS ignored
		MATCH (s:ProductStore {store:$store}) SET s.domain=$domain`, map[string]any{"store": store, "domain": scope})
	useStore(graphtest.Store(t))
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)
	result, err := session.Run(ctx, "DROP TEXT INDEX chunk_text_v2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.Consume(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Bootstrap(context.Background()); err != nil {
			t.Error(err)
		}
	})
	readBoth("", 0, 0, 0, "search_unavailable", false)
}
