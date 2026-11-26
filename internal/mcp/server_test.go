package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

// mcpToken is a single alphanumeric token unique per run, so text-search
// assertions never depend on the shared accumulated corpus.
func mcpToken(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("mcpquark%d", time.Now().UnixNano())
}

// mcpSource is a per-run unique document identity for the shared database.
func mcpSource(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("file:mcp-%s-%d", strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()), time.Now().UnixNano())
}

// fakeSubmitter records the last submission instead of reaching Memgraph.
type fakeSubmitter struct {
	in  ingest.Input
	res ingest.Result
	err error
}

func (f *fakeSubmitter) Submit(_ context.Context, in ingest.Input) (ingest.Result, error) {
	f.in = in
	return f.res, f.err
}

// connectInMemory wires a client session to the server over in-memory
// transports; tests close both ends via cleanup.
func connectInMemory(t *testing.T, cfg config.Config, st *graph.Store, sub api.Submitter) *mcpgo.ClientSession {
	t.Helper()
	return connectServer(t, NewServer(cfg, st, sub))
}

// connectServer wires a client session to a prepared server.
func connectServer(t *testing.T, srv *Server) *mcpgo.ClientSession {
	t.Helper()
	return connectServerWithOptions(t, srv, nil)
}

func connectServerWithOptions(t *testing.T, srv *Server, options *mcpgo.ClientOptions) *mcpgo.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcpgo.NewInMemoryTransports()
	ss, err := srv.MCP().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpgo.NewClient(&mcpgo.Implementation{Name: "gkg-test", Version: "v0"}, options)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
	})
	return cs
}

func callTool(t *testing.T, cs *mcpgo.ClientSession, name string, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpgo.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s, %v): %v", name, args, err)
	}
	return res
}

// structured decodes a successful tool result's structured content.
func structured[T any](t *testing.T, res *mcpgo.CallToolResult) T {
	t.Helper()
	if res.IsError {
		t.Fatalf("result is an error: %s", toolErrorText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode structured content %s: %v", raw, err)
	}
	return out
}

func toolErrorText(res *mcpgo.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpgo.TextContent); ok {
			b.WriteString(tc.Text)
			b.WriteByte(' ')
		}
	}
	return strings.TrimSpace(b.String())
}

// stubEmbedder returns a fixed vector for every query (query-leg stub).
type stubEmbedder struct{ vec []float64 }

func (s stubEmbedder) EmbedQuery(context.Context, string) ([]float64, error) {
	return s.vec, nil
}

func TestToolsListExactNames(t *testing.T) {
	cs := connectInMemory(t, config.Config{}, nil, nil)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	assertGraphToolNames(t, res.Tools)
}

func assertGraphToolNames(t *testing.T, tools []*mcpgo.Tool) {
	t.Helper()
	got := make(map[string]bool, len(tools))
	for _, tool := range tools {
		got[tool.Name] = true
	}
	want := []string{"ingest", "status", "search", "get_entity", "get_chunks", "traverse", "list_documents", "cypher", "get_product", "find_products", "list_features"}
	if len(got) != len(want) {
		t.Fatalf("tools = %v, want exactly %v", got, want)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q missing", name)
		}
	}
}

func TestGetChunksTool(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: mcpSource(t), Hash: "zorbo-chunks"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DeleteDocument(ctx, doc.ID) })
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: "Zorbo begins."}, {Seq: 1, Text: "Quillon Ltd follows."}}); err != nil {
		t.Fatal(err)
	}
	cs := connectInMemory(t, config.Config{}, st, nil)
	res := callTool(t, cs, "get_chunks", map[string]any{"ids": []string{doc.ID + "-c0", "absent"}, "neighbours": 1})
	out := structured[struct {
		Chunks []struct {
			ID, Text    string
			NeighbourOf string `json:"neighbour_of"`
		}
		Meta map[string]json.RawMessage
	}](t, res)
	if len(out.Chunks) != 2 || out.Chunks[0].ID != doc.ID+"-c0" || out.Chunks[0].Text != "Zorbo begins." || out.Chunks[1].NeighbourOf != doc.ID+"-c0" {
		t.Fatalf("chunks=%+v", out.Chunks)
	}
	if len(out.Meta) != 4 {
		t.Fatalf("meta=%v", out.Meta)
	}
	for _, key := range []string{"returned", "missing", "truncated", "omitted_for_budget"} {
		if _, ok := out.Meta[key]; !ok {
			t.Errorf("meta lacks %s", key)
		}
	}
	if string(out.Meta["returned"]) != "2" || string(out.Meta["missing"]) != `["absent"]` {
		t.Fatalf("meta=%v", out.Meta)
	}
	for _, args := range []map[string]any{{"ids": []string{}}, {"ids": []string{"c1"}, "neighbours": -1}, {"ids": []string{"c1"}, "neighbours": 3}} {
		if res := callTool(t, cs, "get_chunks", args); !res.IsError {
			t.Errorf("accepted invalid args=%v", args)
		}
	}
	args := map[string]any{"ids": []string{doc.ID + "-c0"}, "with_products": true}
	enriched := structured[struct {
		Chunks        []map[string]json.RawMessage
		ProductsError json.RawMessage `json:"products_error"`
		LinksPartial  json.RawMessage `json:"links_partial"`
	}](t, callTool(t, cs, "get_chunks", args))
	if enriched.ProductsError != nil || enriched.LinksPartial != nil || len(enriched.Chunks) != 1 ||
		string(enriched.Chunks[0]["products"]) != "[]" || string(enriched.Chunks[0]["products_total"]) != "0" {
		t.Errorf("get_chunks without a bound product store = %+v, want empty products and zero total without error fields", enriched)
	}
}

const (
	evidenceDate          = "2024-03-01T00:00:00Z"
	evidenceVersion       = "2025-03-01T00:00:00Z"
	storedEvidenceDate    = "2024-03-01T00:00:00.000000000Z"
	storedEvidenceVersion = "2025-03-01T00:00:00.000000000Z"
)

func assertEvidenceDate(t *testing.T, row map[string]any, date, source string) {
	t.Helper()
	if date == "" {
		_, hasDate := row["document_date"]
		_, hasSource := row["document_date_source"]
		if hasDate || hasSource {
			t.Errorf("expected date fields to be omitted: %v", row)
		}
	} else if row["document_date"] != date || row["document_date_source"] != source {
		t.Errorf("document_date=%v document_date_source=%v, want %s (%s)", row["document_date"], row["document_date_source"], date, source)
	}
}

func writeEvidenceFixture(t *testing.T, query string, params map[string]any) {
	t.Helper()
	ctx := context.Background()
	drv, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(ctx)
	if _, err := neo4j.ExecuteQuery(ctx, drv, query, params, neo4j.EagerResultTransformer); err != nil {
		t.Fatal(err)
	}
}

func deleteEvidenceDocument(t *testing.T, id string) {
	t.Helper()
	params := map[string]any{"id": id}
	writeEvidenceFixture(t, "MATCH (d:Document {id:$id}) DETACH DELETE d", params)
	t.Cleanup(func() {
		// Reattach the chunks so the fixture's normal cleanup can remove its evidence.
		writeEvidenceFixture(t, `CREATE (d:Document {id:$id})
WITH d MATCH (c:Chunk {doc_id:$id}) CREATE (d)-[:HAS_CHUNK]->(c)`, params)
	})
}

func TestGetChunksDocumentDateMCP(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	cs := connectInMemory(t, config.Config{}, st, nil)
	for _, tc := range []struct {
		name                          string
		chunks, neighbours, wantCount int
	}{
		{"neighbours_zero", 2, 0, 1},
		{"with_neighbour", 2, 1, 2},
		{"single_chunk", 1, 2, 1},
		{"orphan_chunk", 1, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := mcpToken(t)
			doc, err := st.UpsertDocument(ctx, graph.DocumentInput{
				Kind: "text", Source: mcpSource(t), Hash: tok, DocumentDate: storedEvidenceDate,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := st.DeleteDocument(ctx, doc.ID); err != nil {
					t.Error(err)
				}
			})
			chunks := make([]graph.Chunk, tc.chunks)
			for i := range chunks {
				chunks[i] = graph.Chunk{Seq: i, Text: tok}
			}
			if err := st.PutChunks(ctx, doc.ID, chunks); err != nil {
				t.Fatal(err)
			}
			date, source := evidenceDate, "caller"
			if tc.name == "orphan_chunk" {
				deleteEvidenceDocument(t, doc.ID)
				date, source = "", ""
				search := structured[struct{ Hits []map[string]any }](t,
					callTool(t, cs, "search", map[string]any{"query": tok, "kind": "chunk"}))
				if len(search.Hits) != 1 || search.Hits[0]["id"] != doc.ID+"-c0" {
					t.Fatalf("orphan search hits = %v, want chunk %s-c0", search.Hits, doc.ID)
				}
				assertEvidenceDate(t, search.Hits[0], date, source)
			}
			out := structured[struct{ Chunks []map[string]any }](t, callTool(t, cs, "get_chunks",
				map[string]any{"ids": []string{doc.ID + "-c0"}, "neighbours": tc.neighbours}))
			if len(out.Chunks) != tc.wantCount || out.Chunks[0]["id"] != doc.ID+"-c0" {
				t.Fatalf("chunks = %v, want %d including center %s-c0", out.Chunks, tc.wantCount, doc.ID)
			}
			for _, row := range out.Chunks {
				assertEvidenceDate(t, row, date, source)
			}
		})
	}
}

func TestGetChunksToolProductsLimit(t *testing.T) {
	st := graphtest.Store(t)
	drv, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close(context.Background()) })
	const prefix = "mcp-chunk-products-limit-"
	params := map[string]any{"prefix": prefix}
	t.Cleanup(func() {
		_, err := neo4j.ExecuteQuery(context.Background(), drv, `
			MATCH (n) WHERE n.id STARTS WITH $prefix OR n.store STARTS WITH $prefix
				OR n.key STARTS WITH $prefix OR (n:GkgMeta AND n.id='product_release' AND n.value=$prefix)
			DETACH DELETE n`, params, neo4j.EagerResultTransformer)
		if err != nil {
			t.Errorf("delete products limit fixture: %v", err)
		}
	})
	_, err = neo4j.ExecuteQuery(t.Context(), drv, `
		CREATE (:GkgMeta {id:'product_release', value:$prefix}),
			(:ProductStore {store:$prefix + 'store.test', domain:'catalogue'}),
			(:Chunk {id:$prefix + 'chunk', seq:0, text:'AB12345 and CD67890'}),
			(f:FeatureRev {id:'catalogue_code', key:$prefix + 'code'}),
			(a:Product {id:$prefix + 'a', store:$prefix + 'store.test', product_id:'a'}),
			(b:Product {id:$prefix + 'b', store:$prefix + 'store.test', product_id:'b'}),
			(a)-[:HAS_FEATURE {release_id:$prefix, value_fold:'AB12345'}]->(f),
			(b)-[:HAS_FEATURE {release_id:$prefix, value_fold:'CD67890'}]->(f)`, params, neo4j.EagerResultTransformer)
	if err != nil {
		t.Fatalf("seed products limit fixture: %v", err)
	}
	cs := connectInMemory(t, config.Config{}, st, nil)
	call := func(t *testing.T, limit string) *mcpgo.CallToolResult {
		t.Helper()
		return callTool(t, cs, "get_chunks", map[string]any{
			"ids": []string{prefix + "chunk"}, "with_products": true, "products_limit": json.RawMessage(limit),
		})
	}
	for _, limit := range []string{"0", "51", "-1", "3.0", "1e1", "1.5", `"2"`, "null"} {
		t.Run("reject="+limit, func(t *testing.T) {
			if res := call(t, limit); !res.IsError || !strings.Contains(toolErrorText(res), "products_limit") {
				t.Errorf("products_limit=%s: got %q, want a products_limit argument error", limit, toolErrorText(res))
			}
		})
	}
	t.Run("limit=1", func(t *testing.T) {
		out := structured[struct {
			Chunks []struct {
				Products []chunkread.ChunkProduct
				Total    int             `json:"products_total"`
				Omitted  json.RawMessage `json:"products_omitted_for_budget"`
			}
		}](t, call(t, "1"))
		if len(out.Chunks) != 1 || out.Chunks[0].Total != 2 || len(out.Chunks[0].Products) != 1 || out.Chunks[0].Omitted != nil {
			t.Errorf("products_limit=1: got %+v, want one product of two with no budget omissions", out)
		}
	})
}

func TestToolResultSizeGuidance(t *testing.T) {
	cs := connectInMemory(t, config.Config{}, nil, nil)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"search": "Start with limit 10.", "traverse": "Start with limit 10.", "list_documents": "Start with limit 10.",
		"get_chunks":    "Read chunk text, up to 4 KiB per chunk; `truncated` marks a cut, by id (ids from search hits), optionally with up to 2 neighbouring chunks of the same document. Use this instead of cypher to read chunk text. omitted_for_budget may also list neighbour ids.",
		"find_products": "Pages over 256 KiB are returned only in structuredContent, with empty text content.",
	}
	for _, tool := range res.Tools {
		if desc, ok := want[tool.Name]; ok {
			if tool.Name == "get_chunks" {
				if !strings.Contains(tool.Description, "document_date and document_date_source") {
					t.Error("get_chunks description does not explain the document date fields")
				}
				if !strings.Contains(tool.Description, "Set with_products to list document-linked products first, then exact identifier matches from the full chunk text.") || strings.Contains(tool.Description, "with_products defaults to true") {
					t.Errorf("get_chunks description does not explain how to opt in to product results: %q", tool.Description)
				}
			}
			if !strings.Contains(tool.Description, desc) {
				t.Errorf("%s description=%q, want %q", tool.Name, tool.Description, desc)
			}
			delete(want, tool.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing tools=%v", want)
	}
}

func TestIngestToolReturnsDocID(t *testing.T) {
	sub := &fakeSubmitter{res: ingest.Result{DocumentID: "doc-mcp-1"}}
	cs := connectInMemory(t, config.Config{}, nil, sub)
	res := callTool(t, cs, "ingest", map[string]any{"text": "hello world", "source": "greeting"})
	out := structured[ingestOut](t, res)
	if out.DocumentID != "doc-mcp-1" {
		t.Errorf("document_id = %q, want doc-mcp-1", out.DocumentID)
	}
	if sub.in.Kind != "text" || sub.in.Text != "hello world" || sub.in.Source != "greeting" {
		t.Errorf("submitted %+v, want the text input mapped", sub.in)
	}

	// The url branch maps kind=url without fetching (the submitter is fake).
	res = callTool(t, cs, "ingest", map[string]any{"url": "https://example.com/article"})
	out = structured[ingestOut](t, res)
	if out.DocumentID != "doc-mcp-1" {
		t.Errorf("url ingest document_id = %q", out.DocumentID)
	}
	if sub.in.Kind != "url" || sub.in.URL != "https://example.com/article" {
		t.Errorf("submitted %+v, want the url input mapped", sub.in)
	}

	// Neither url nor text, or both, are actionable validation errors.
	res = callTool(t, cs, "ingest", map[string]any{})
	if !res.IsError || !strings.Contains(toolErrorText(res), "url or text") {
		t.Errorf("empty ingest = %+v (%q), want a url-or-text error", res, toolErrorText(res))
	}
	res = callTool(t, cs, "ingest", map[string]any{"url": "https://example.com/x", "text": "hello"})
	if !res.IsError || !strings.Contains(toolErrorText(res), "not both") {
		t.Errorf("url+text ingest = %+v (%q), want a not-both error", res, toolErrorText(res))
	}
}

func TestStatusToolLists(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: mcpSource(t), Hash: "hash-mcp-status"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	cs := connectInMemory(t, config.Config{}, st, nil)

	res := callTool(t, cs, "status", map[string]any{"document_id": doc.ID})
	out := structured[statusOut](t, res)
	if len(out.Documents) != 1 || out.Documents[0].ID != doc.ID || out.Documents[0].Status != "received" {
		t.Errorf("status = %+v, want the seeded document received", out)
	}

	wire := structured[struct {
		Documents []map[string]any `json:"documents"`
	}](t, res)
	if len(wire.Documents) != 1 || wire.Documents[0]["document_date"] == nil || wire.Documents[0]["document_date_source"] != "ingest" {
		t.Errorf("status wire = %+v, want the ingest date and its origin", wire)
	}

	res = callTool(t, cs, "status", map[string]any{})
	out = structured[statusOut](t, res)
	found := false
	for _, d := range out.Documents {
		if d.ID == doc.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("status list = %d documents, want the seeded %s", len(out.Documents), doc.ID)
	}

	// list_documents filters by status in the query.
	res = callTool(t, cs, "list_documents", map[string]any{"status": "received", "limit": 10})
	list := structured[listDocumentsOut](t, res)
	found = false
	for _, d := range list.Documents {
		if d.ID == doc.ID {
			found = true
		}
		if d.Status != "received" {
			t.Errorf("list_documents leaked status %q", d.Status)
		}
	}
	if !found {
		t.Errorf("list_documents = %d documents, want the seeded %s", len(list.Documents), doc.ID)
	}

	res = callTool(t, cs, "list_documents", map[string]any{"limit": -1})
	if !res.IsError || !strings.Contains(toolErrorText(res), "limit") {
		t.Errorf("negative limit = %+v (%q), want a limit error", res, toolErrorText(res))
	}
}

func TestSearchShapedHits(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{
		Kind: "file", Source: mcpSource(t), Hash: "hash-mcp-search", DocumentDate: storedEvidenceDate,
	})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteDocument(ctx, doc.ID) })
	tok := mcpToken(t)
	text := "The " + tok + " guidance explains the procedure step by step."
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	key := graph.EntityKey(tok)
	if err := st.UpsertExtraction(ctx, doc.ID, doc.ID+"-c0", "mock:1",
		[]graph.EntityUpsert{{Key: key, Name: tok, Type: "technology"}}, nil); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}

	cs := connectInMemory(t, config.Config{}, st, nil)
	res := callTool(t, cs, "search", map[string]any{"query": tok, "kind": "auto"})
	out := structured[struct{ Hits []map[string]any }](t, res)
	var foundEntity, foundChunk bool
	for _, hit := range out.Hits {
		if hit["kind"] == "entity" && hit["id"] == graph.EntityID(key) {
			foundEntity = true
			if _, present := hit["document_id"]; present {
				t.Errorf("entity hit carries document_id = %v, want omitted", hit["document_id"])
			}
			assertEvidenceDate(t, hit, "", "")
		}
		if hit["kind"] == "chunk" && hit["id"] == doc.ID+"-c0" {
			foundChunk = true
			if hit["document_id"] != doc.ID {
				t.Errorf("chunk hit document_id = %v, want %s", hit["document_id"], doc.ID)
			}
			if snippet, _ := hit["snippet"].(string); !strings.Contains(snippet, tok) {
				t.Errorf("chunk snippet = %q, want the token inside", snippet)
			}
			assertEvidenceDate(t, hit, evidenceDate, "caller")
		}
	}
	if !foundEntity || !foundChunk {
		t.Errorf("hits = %+v, want entity %s and chunk %s", out.Hits, graph.EntityID(key), doc.ID+"-c0")
	}
}

func TestGetEntityUnknownNameErrors(t *testing.T) {
	st := graphtest.Store(t)
	cs := connectInMemory(t, config.Config{}, st, nil)
	missing := "no-such-" + mcpToken(t)
	res := callTool(t, cs, "get_entity", map[string]any{"name": missing})
	if !res.IsError {
		t.Fatalf("result = %+v, want an error for an unknown name", res)
	}
	if got := toolErrorText(res); !strings.Contains(got, "no entity matching") || !strings.Contains(got, missing) {
		t.Errorf("error = %q, want an actionable no-entity message naming %q", got, missing)
	}

	// Neither id nor name is an actionable validation error.
	res = callTool(t, cs, "get_entity", map[string]any{})
	if !res.IsError || !strings.Contains(toolErrorText(res), "id or name") {
		t.Errorf("empty get_entity = %+v (%q), want an id-or-name error", res, toolErrorText(res))
	}
}

func TestGetEntityLowConfidenceGating(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: mcpSource(t), Hash: "hash-mcp-entity"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	tok := mcpToken(t)
	a := graph.EntityKey("Ada " + tok)
	b := graph.EntityKey("Org " + tok)
	c := graph.EntityKey("Tech " + tok)
	text := "Ada " + tok + " works at Org " + tok + " and uses Tech " + tok + "."
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	ents := []graph.EntityUpsert{
		{Key: a, Name: "Ada " + tok, Type: "person"},
		{Key: b, Name: "Org " + tok, Type: "organization"},
		{Key: c, Name: "Tech " + tok, Type: "technology"},
	}
	facts := []graph.FactUpsert{
		{
			HeadKey: a, TailKey: b, Rel: "works_at", Confidence: 0.9,
			HeadMention: graph.Span{Start: 0, End: 4, Text: "Ada"},
			TailMention: graph.Span{Start: 14, End: 17, Text: "Org"},
		},
		{
			HeadKey: a, TailKey: c, Rel: "uses", Confidence: 0.3, LowConfidence: true,
			HeadMention: graph.Span{Start: 0, End: 4, Text: "Ada"},
			TailMention: graph.Span{Start: 36, End: 40, Text: "Tech"},
		},
	}
	if err := st.UpsertExtraction(ctx, doc.ID, doc.ID+"-c0", "mock:1", ents, facts); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}

	cs := connectInMemory(t, config.Config{}, st, nil)
	res := callTool(t, cs, "get_entity", map[string]any{"id": graph.EntityID(a)})
	out := structured[entityOut](t, res)
	if !hasRel(out, "works_at") {
		t.Errorf("facts = %+v, want works_at", out.Facts)
	}
	if hasRel(out, "uses") {
		t.Errorf("facts = %+v, want the low-confidence uses fact excluded by default", out.Facts)
	}
	if len(out.Together) != 2 {
		t.Fatalf("together = %+v, want Org and Tech regardless of fact confidence", out.Together)
	}
	wantTogether := []graph.EntityRef{
		{ID: graph.EntityID(b), Key: b, Name: "Org " + tok, Type: "organization"},
		{ID: graph.EntityID(c), Key: c, Name: "Tech " + tok, Type: "technology"},
	}
	for i, want := range wantTogether {
		got := out.Together[i]
		if got.ID != want.ID || got.Key != want.Key || got.Name != want.Name || got.Type != want.Type || got.SharedChunks != 1 {
			t.Errorf("together[%d] = %+v, want %+v in one shared chunk", i, got, want)
		}
	}

	res = callTool(t, cs, "get_entity", map[string]any{"id": graph.EntityID(a), "include_low_confidence": true})
	out = structured[entityOut](t, res)
	if !hasRel(out, "uses") {
		t.Errorf("facts = %+v, want uses when opted in", out.Facts)
	}

	// Name lookup resolves to the same entity.
	res = callTool(t, cs, "get_entity", map[string]any{"name": "Ada " + tok})
	out = structured[entityOut](t, res)
	if out.ID != graph.EntityID(a) {
		t.Errorf("name lookup id = %q, want %s", out.ID, graph.EntityID(a))
	}
}

func hasRel(view entityOut, rel string) bool {
	for _, f := range view.Facts {
		if f.Rel == rel {
			return true
		}
	}
	return false
}

func TestTraverseToolDepthCap(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: mcpSource(t), Hash: "hash-mcp-traverse"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	tok := mcpToken(t)
	a := graph.EntityKey("Ada " + tok)
	b := graph.EntityKey("Org " + tok)
	c := graph.EntityKey("Tech " + tok)
	text := "Ada " + tok + " wrote at Org " + tok + " and uses Tech " + tok + "."
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	ents := []graph.EntityUpsert{
		{Key: a, Name: "Ada " + tok, Type: "person"},
		{Key: b, Name: "Org " + tok, Type: "organization"},
		{Key: c, Name: "Tech " + tok, Type: "technology"},
	}
	facts := []graph.FactUpsert{
		{
			HeadKey: a, TailKey: b, Rel: "works_at", Confidence: 0.9,
			HeadMention: graph.Span{Start: 0, End: 4, Text: "Ada"},
			TailMention: graph.Span{Start: 13, End: 16, Text: "Org"},
		},
		{
			HeadKey: a, TailKey: c, Rel: "uses", Confidence: 0.3, LowConfidence: true,
			HeadMention: graph.Span{Start: 0, End: 4, Text: "Ada"},
			TailMention: graph.Span{Start: 40, End: 44, Text: "Tech"},
		},
	}
	if err := st.UpsertExtraction(ctx, doc.ID, doc.ID+"-c0", "mock:1", ents, facts); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}

	cs := connectInMemory(t, config.Config{}, st, nil)
	res := callTool(t, cs, "traverse", map[string]any{"entity": a, "depth": 2})
	out := structured[traverseOut](t, res)
	foundEdge := false
	for _, e := range out.Edges {
		if e.From == a && e.To == b && e.Rel == "works_at" {
			foundEdge = true
		}
		if e.Rel == "uses" {
			t.Errorf("low-confidence uses fact leaked into the default traversal: %+v", e)
		}
	}
	if !foundEdge {
		t.Errorf("edges = %+v, want %s -works_at-> %s", out.Edges, a, b)
	}

	// The low-confidence fact is reachable when opted in.
	res = callTool(t, cs, "traverse", map[string]any{"entity": a, "include_low_confidence": true})
	out = structured[traverseOut](t, res)
	foundLow := false
	for _, e := range out.Edges {
		if e.Rel == "uses" && e.LowConfidence {
			foundLow = true
		}
	}
	if !foundLow {
		t.Errorf("edges = %+v, want the low-confidence uses fact when opted in", out.Edges)
	}

	res = callTool(t, cs, "traverse", map[string]any{"entity": a, "depth": 3})
	if !res.IsError || !strings.Contains(toolErrorText(res), "depth") {
		t.Errorf("depth 3 = %+v (%q), want a depth error", res, toolErrorText(res))
	}
}

func TestCypherToolDeniesWrite(t *testing.T) {
	cs := connectInMemory(t, config.Config{}, nil, nil)
	for _, q := range []string{
		"MATCH (n) DELETE n",
		"MATCH (d:Document) RETURN d UNION MATCH (e:Entity) RETURN e",
		"MATCH (n) RETURN n LIMIT 2 * 1000",
	} {
		res := callTool(t, cs, "cypher", map[string]any{"query": q})
		if !res.IsError {
			t.Errorf("query %q: result = %+v, want a tool error", q, res)
			continue
		}
		if got := toolErrorText(res); got == "" {
			t.Errorf("query %q: empty error text", q)
		}
	}
}

// TestCypherToolAllowsRead runs the allowed path through the tool: the
// guard injects the row cap, the query executes and nodes convert.
func TestCypherToolAllowsRead(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: mcpSource(t), Hash: "hash-mcp-cypher", Title: "Cypher doc"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	cs := connectInMemory(t, config.Config{}, st, nil)

	q := fmt.Sprintf("MATCH (d:Document) WHERE d.id = '%s' RETURN d.id AS id, d AS node", doc.ID)
	res := callTool(t, cs, "cypher", map[string]any{"query": q, "limit": 5})
	out := structured[cypherOut](t, res)
	if len(out.Rows) != 1 {
		t.Fatalf("rows = %+v, want the seeded document", out.Rows)
	}
	if out.Rows[0]["id"] != doc.ID {
		t.Errorf("row id = %v, want %s", out.Rows[0]["id"], doc.ID)
	}
	node, ok := out.Rows[0]["node"].(map[string]any)
	if !ok {
		t.Fatalf("node = %v, want a JSON object", out.Rows[0]["node"])
	}
	labels, _ := node["labels"].([]any)
	found := false
	for _, l := range labels {
		if l == "Document" {
			found = true
		}
	}
	if !found {
		t.Errorf("node labels = %v, want Document", labels)
	}

	// A query LIMIT above the configured cap is rejected through the tool
	// exactly like over REST.
	res = callTool(t, cs, "cypher", map[string]any{"query": "MATCH (n) RETURN n LIMIT 1000000000"})
	if !res.IsError || !strings.Contains(toolErrorText(res), "cypher_rows") {
		t.Errorf("oversized LIMIT = %+v (%q), want the cap rejection", res, toolErrorText(res))
	}
}

// TestSearchToolUsesEmbedderWhenEnabled pins the mcp wiring: with an
// embedder set and a query that matches no name, a vector-seeded entity
// surfaces through the tool.
func TestSearchToolUsesEmbedderWhenEnabled(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	const dim = 768
	if err := st.EnsureVectorIndex(ctx, dim); err != nil {
		t.Fatalf("EnsureVectorIndex: %v", err)
	}
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: mcpSource(t), Hash: "hash-mcp-embed"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	tok := mcpToken(t)
	text := "The " + tok + " guidance."
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	key := graph.EntityKey(tok)
	if err := st.UpsertExtraction(ctx, doc.ID, doc.ID+"-c0", "mock:1",
		[]graph.EntityUpsert{{Key: key, Name: tok, Type: "technology"}}, nil); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	vec := graphtest.Vector(tok, dim)
	if err := st.SetEntityEmbedding(ctx, graph.EntityID(key), vec); err != nil {
		t.Fatalf("SetEntityEmbedding: %v", err)
	}

	srv := NewServer(config.Config{}, st, nil)
	srv.SetEmbedder(stubEmbedder{vec})
	cs := connectServer(t, srv)
	res := callTool(t, cs, "search", map[string]any{"query": mcpToken(t), "kind": "entity"})
	out := structured[searchOut](t, res)
	for _, hit := range out.Hits {
		if hit.ID == graph.EntityID(key) {
			return
		}
	}
	t.Errorf("hits = %+v, want the vector-seeded entity %s", out.Hits, graph.EntityID(key))
}

// TestHTTPDefaultClientThroughAPIServer pins that the api surface's
// middleware keeps streaming intact: a default go-sdk client (standalone
// SSE enabled) connects through api.Server.Handler and lists the tools.
func TestHTTPDefaultClientThroughAPIServer(t *testing.T) {
	rest := api.NewServer(config.Config{}, nil, nil)
	rest.Mount("/mcp", NewServer(config.Config{}, nil, nil).HTTPHandler())
	ts := httptest.NewServer(rest.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := mcpgo.NewClient(&mcpgo.Implementation{Name: "gkg-sse-test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, &mcpgo.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: ts.Client()}, nil)
	if err != nil {
		t.Fatalf("connect (default SSE) through the api surface: %v", err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools through the api surface: %v", err)
	}
	assertGraphToolNames(t, res.Tools)
}

// TestHTTPHandlerServesTools pins the streamable-HTTP path used by the
// serve command: a real MCP client over HTTP lists the graph tools.
func TestHTTPHandlerServesTools(t *testing.T) {
	srv := NewServer(config.Config{}, nil, nil)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	ctx := context.Background()
	client := mcpgo.NewClient(&mcpgo.Implementation{Name: "gkg-http-test", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, &mcpgo.StreamableClientTransport{
		Endpoint:             ts.URL,
		HTTPClient:           ts.Client(),
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect over http: %v", err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools over http: %v", err)
	}
	assertGraphToolNames(t, res.Tools)
}

func TestCandidateNotVisible(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: mcpSource(t), Hash: "candidate"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DeleteDocument(ctx, doc.ID) })
	token := mcpToken(t)
	ents := []graph.EntityUpsert{{Key: "zorbo-" + token, Name: "Zorbo", Type: "person"}, {Key: "quillon-" + token, Name: "Quillon Ltd", Type: "organization"}}
	text := "Zorbo joined Quillon Ltd."
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text, End: len(text)}}); err != nil {
		t.Fatal(err)
	}
	facts := []graph.FactUpsert{{HeadKey: ents[0].Key, Rel: "joined", TailKey: ents[1].Key, Confidence: 0.9, HeadMention: graph.Span{Start: 0, End: 5}, TailMention: graph.Span{Start: 13, End: 24}}}
	facts[0].JudgeInput = ingest.CanonicalJudgeInput(ingest.FactInput{
		Head: "Zorbo", Rel: "joined", Tail: "Quillon Ltd", Confidence: 0.9, Evidence: text, DocID: doc.ID,
	})
	if err := st.UpsertExtractionOpts(ctx, doc.ID, doc.ID+"-c0", "mock:1", ents, facts, graph.WriteOptions{FactsAsCandidates: true}); err != nil {
		t.Fatal(err)
	}
	cs := connectInMemory(t, config.Config{}, st, nil)
	res := callTool(t, cs, "get_entity", map[string]any{"id": graph.EntityID(ents[0].Key), "include_low_confidence": true})
	view := structured[entityOut](t, res)
	if len(view.Facts) != 0 || len(view.Together) != 1 || view.Together[0].Key != ents[1].Key {
		t.Fatalf("candidate view = %+v", view)
	}
}

func TestIngestToolProductRefsValidation(t *testing.T) {
	pipe := ingest.NewPipeline(config.Config{}, nil, nil, nil, nil)
	cs := connectInMemory(t, config.Config{}, nil, pipe)
	res := callTool(t, cs, "ingest", map[string]any{"text": "page", "products": []string{"store.test:"}})
	if !res.IsError || !strings.Contains(toolErrorText(res), "invalid_products") {
		t.Errorf("result=%+v error=%q, want invalid_products", res, toolErrorText(res))
	}
}

func TestIngestToolAcceptsDomain(t *testing.T) {
	sub := &fakeSubmitter{res: ingest.Result{DocumentID: "doc"}}
	cs := connectInMemory(t, config.Config{}, nil, sub)
	for _, value := range []any{"", "Research / Medicine"} {
		res := callTool(t, cs, "ingest", map[string]any{"text": "page", "domain": value})
		if res.IsError || structured[ingestOut](t, res).DocumentID != "doc" || sub.in.Domain != value {
			t.Errorf("domain %q: %s", value, toolErrorText(res))
		}
	}
	sub.in = ingest.Input{}
	res := callTool(t, cs, "ingest", map[string]any{"text": "page", "domain": 5})
	if !res.IsError || !strings.Contains(toolErrorText(res), "domain") || sub.in.Text != "" {
		t.Errorf("numeric domain: result=%+v, submitted=%+v", res, sub.in)
	}
	for _, tc := range []struct {
		err     error
		message string
	}{
		{&graph.IngestDomainError{Code: "unknown_domain"}, "unknown_domain"},
		{&graph.IngestDomainError{Code: "ambiguous_domain"}, "ambiguous_domain"},
		{&graph.IngestDomainError{Code: "unknown_domain", Conflict: true}, "unknown_domain"},
		{&graph.IngestDomainError{Code: "domain_conflict", Conflict: true}, "domain_conflict"},
		{&graph.IngestDomainError{Code: "document_busy", Conflict: true}, "document_busy"},
		{graph.ErrSchemaNotV4, "schema_not_v4"},
		{fmt.Errorf("%w: could not connect", ingest.ErrDomainLookup), "domain lookup failed"},
	} {
		sub.err = tc.err
		res := callTool(t, cs, "ingest", map[string]any{"text": "page", "domain": "d-research"})
		if !res.IsError || !strings.Contains(toolErrorText(res), tc.message) {
			t.Errorf("domain error %v: result=%+v, want %q", tc.err, res, tc.message)
		}
	}
}

func TestIngestToolProductRefsPresence(t *testing.T) {
	sub := &fakeSubmitter{res: ingest.Result{DocumentID: "doc"}}
	cs := connectInMemory(t, config.Config{}, nil, sub)
	for _, products := range [][]string{nil, {}, {"a:1", "b:2"}} {
		args := map[string]any{"text": "page"}
		if products != nil {
			args["products"] = products
		}
		res := callTool(t, cs, "ingest", args)
		if res.IsError || !reflect.DeepEqual(sub.in.Products, products) {
			t.Errorf("products input=%#v, forwarded=%#v, error=%q", products, sub.in.Products, toolErrorText(res))
		}
	}
}

func TestIngestDocumentDateMCP(t *testing.T) {
	sub := &fakeSubmitter{res: ingest.Result{DocumentID: "doc"}}
	cs := connectInMemory(t, config.Config{}, nil, sub)
	res := callTool(t, cs, "ingest", map[string]any{"text": "page", "document_date": "2024-03-01T00:30:00+01:00"})
	if res.IsError || sub.in.DocumentDate != "2024-02-29T23:30:00.000000000Z" {
		t.Errorf("valid date: forwarded=%q, error=%q", sub.in.DocumentDate, toolErrorText(res))
	}
	sub.in = ingest.Input{}
	res = callTool(t, cs, "ingest", map[string]any{"text": "page", "document_date": "bad-date"})
	if !res.IsError || !strings.Contains(toolErrorText(res), "document_date") || sub.in.Text != "" {
		t.Errorf("invalid date: forwarded=%+v, error=%q", sub.in, toolErrorText(res))
	}
}
