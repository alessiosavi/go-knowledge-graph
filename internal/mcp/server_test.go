package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
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
	ctx := context.Background()
	serverTransport, clientTransport := mcpgo.NewInMemoryTransports()
	ss, err := srv.MCP().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpgo.NewClient(&mcpgo.Implementation{Name: "gkg-test", Version: "v0"}, nil)
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
	got := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	want := []string{"ingest", "status", "search", "get_entity", "traverse", "list_documents", "cypher"}
	if len(got) != len(want) {
		t.Fatalf("tools = %v, want exactly %v", got, want)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q missing", name)
		}
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
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: mcpSource(t), Hash: "hash-mcp-search"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	tok := mcpToken(t)
	text := "The " + tok + " guidance explains the procedure step by step."
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	key := graph.EntityKey("technology", tok)
	if err := st.UpsertExtraction(ctx, doc.ID, doc.ID+"-c0", "mock:1",
		[]graph.EntityUpsert{{Key: key, Name: tok, Type: "technology"}}, nil); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}

	cs := connectInMemory(t, config.Config{}, st, nil)
	res := callTool(t, cs, "search", map[string]any{"query": tok, "kind": "auto"})
	out := structured[searchOut](t, res)
	var foundEntity, foundChunk bool
	for _, hit := range out.Hits {
		if hit.Kind == "entity" && hit.ID == graph.EntityID(key) {
			foundEntity = true
			if hit.DocumentID != "" {
				t.Errorf("entity hit carries document_id = %q, want empty", hit.DocumentID)
			}
		}
		if hit.Kind == "chunk" && hit.ID == doc.ID+"-c0" {
			foundChunk = true
			if hit.DocumentID != doc.ID {
				t.Errorf("chunk hit document_id = %q, want %s", hit.DocumentID, doc.ID)
			}
			if !strings.Contains(hit.Snippet, tok) {
				t.Errorf("chunk snippet = %q, want the token inside", hit.Snippet)
			}
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
	a := graph.EntityKey("person", "Ada "+tok)
	b := graph.EntityKey("organization", "Org "+tok)
	c := graph.EntityKey("technology", "Tech "+tok)
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
	a := graph.EntityKey("person", "Ada "+tok)
	b := graph.EntityKey("organization", "Org "+tok)
	c := graph.EntityKey("technology", "Tech "+tok)
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
	key := graph.EntityKey("technology", tok)
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
	if len(res.Tools) != 7 {
		t.Errorf("tools = %d, want 7", len(res.Tools))
	}
}

// TestHTTPHandlerServesTools pins the streamable-HTTP path used by the
// serve command: a real MCP client over HTTP lists the same seven tools.
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
	if len(res.Tools) != 7 {
		t.Errorf("tools over http = %d, want 7", len(res.Tools))
	}
}
