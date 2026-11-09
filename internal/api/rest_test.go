package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

// apiToken is a single alphanumeric token unique per run: text-search
// assertions never depend on the shared accumulated corpus.
func apiToken(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("apiquark%d", time.Now().UnixNano())
}

// apiSource is a per-run unique document identity for the shared database.
func apiSource(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("file:api-%s-%d", strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()), time.Now().UnixNano())
}

// fakeSubmitter records the last submission instead of reaching Memgraph.
type fakeSubmitter struct {
	called bool
	in     ingest.Input
	res    ingest.Result
	err    error
}

func (f *fakeSubmitter) Submit(_ context.Context, in ingest.Input) (ingest.Result, error) {
	f.called = true
	f.in = in
	return f.res, f.err
}

func apiHandler(t *testing.T, cfg config.Config, st *graph.Store, sub Submitter) http.Handler {
	t.Helper()
	return NewServer(cfg, st, sub).Handler()
}

func do(t *testing.T, h http.Handler, method, target string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func doJSON(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, method, target, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
}

func decode[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return v
}

// TestAuthAppliesToMountedHandlers pins that extra handlers mounted through
// Mount (e.g. the MCP streamable endpoint) sit behind the same bearer
// policy as the built-in routes.
func TestAuthAppliesToMountedHandlers(t *testing.T) {
	noContent := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cfg := config.Config{}
	cfg.Server.APIKey = "sekret"
	secured := NewServer(cfg, nil, nil)
	secured.Mount("/mcp", noContent)
	h := secured.Handler()

	rr := do(t, h, http.MethodPost, "/mcp", strings.NewReader("{}"), map[string]string{"Content-Type": "application/json"})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("mounted handler without token: code = %d, want 401", rr.Code)
	}
	rr = do(t, h, http.MethodPost, "/mcp", strings.NewReader("{}"), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer sekret",
	})
	if rr.Code != http.StatusNoContent {
		t.Errorf("mounted handler with token: code = %d, want the handler's 204", rr.Code)
	}

	// Without a key the mounted handler is reachable directly.
	open := NewServer(config.Config{}, nil, nil)
	open.Mount("/mcp", noContent)
	rr = do(t, open.Handler(), http.MethodPost, "/mcp", strings.NewReader("{}"), map[string]string{"Content-Type": "application/json"})
	if rr.Code != http.StatusNoContent {
		t.Errorf("mounted handler without a configured key: code = %d, want 204", rr.Code)
	}
}

func TestAuthRequiredOnlyWhenKeySet(t *testing.T) {
	// A write query keeps the request store-free: the guard answers 400
	// before any database access, so a wrong 401 is distinguishable.
	writeQuery := `{"query":"MATCH (n) DELETE n"}`

	open := apiHandler(t, config.Config{}, nil, nil)
	rr := doJSON(t, open, http.MethodPost, "/v1/cypher", writeQuery)
	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("no key configured: got 401, want the request to be served")
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no key configured: code = %d, want the guard's 400", rr.Code)
	}

	cfg := config.Config{}
	cfg.Server.APIKey = "sekret"
	secured := apiHandler(t, cfg, nil, nil)

	rr = doJSON(t, secured, http.MethodPost, "/v1/cypher", writeQuery)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("missing bearer token: code = %d, want 401", rr.Code)
	}
	rr = do(t, secured, http.MethodPost, "/v1/cypher", strings.NewReader(writeQuery), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer wrong",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer token: code = %d, want 401", rr.Code)
	}
	rr = do(t, secured, http.MethodPost, "/v1/cypher", strings.NewReader(writeQuery), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer sekret",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("valid bearer token: code = %d, want the guard's 400 (auth passed)", rr.Code)
	}
	// The bearer scheme is case-insensitive (RFC 7235).
	rr = do(t, secured, http.MethodPost, "/v1/cypher", strings.NewReader(writeQuery), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "bearer sekret",
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("lowercase bearer scheme: code = %d, want the guard's 400 (auth passed)", rr.Code)
	}
}

func TestIngestURLReturnsDocID(t *testing.T) {
	sub := &fakeSubmitter{res: ingest.Result{DocumentID: "doc-url-1"}}
	h := apiHandler(t, config.Config{}, nil, sub)

	rr := doJSON(t, h, http.MethodPost, "/v1/ingest",
		`{"url":"https://example.com/article","source":"src-1","labels":"news","force":true}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202; body %s", rr.Code, rr.Body.String())
	}
	got := decode[ingestResponse](t, rr)
	if got.DocumentID != "doc-url-1" {
		t.Errorf("document_id = %q, want doc-url-1", got.DocumentID)
	}
	if !sub.called {
		t.Fatal("submitter not called")
	}
	if sub.in.Kind != "url" || sub.in.URL != "https://example.com/article" {
		t.Errorf("submitted %+v, want kind=url url=https://example.com/article", sub.in)
	}
	if sub.in.Source != "src-1" || sub.in.Labels != "news" || !sub.in.Force {
		t.Errorf("submitted %+v, want source/labels/force mapped", sub.in)
	}
}

func TestIngestMultipartFile(t *testing.T) {
	content := "# Note\n\nBody text."
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	sub := &fakeSubmitter{res: ingest.Result{DocumentID: "doc-file-1"}}
	h := apiHandler(t, config.Config{}, nil, sub)
	rr := do(t, h, http.MethodPost, "/v1/ingest", &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202; body %s", rr.Code, rr.Body.String())
	}
	if got := decode[ingestResponse](t, rr); got.DocumentID != "doc-file-1" {
		t.Errorf("document_id = %q, want doc-file-1", got.DocumentID)
	}
	if sub.in.Kind != "file" || string(sub.in.Bytes) != content {
		t.Errorf("submitted kind=%q bytes=%q, want the uploaded file", sub.in.Kind, sub.in.Bytes)
	}
	if sub.in.Source != "note.md" {
		t.Errorf("source = %q, want the filename as the default", sub.in.Source)
	}

	// An explicit source field overrides the filename.
	var buf2 bytes.Buffer
	mw2 := multipart.NewWriter(&buf2)
	fw2, err := mw2.CreateFormFile("file", "note.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw2.Write([]byte(content))
	if err := mw2.WriteField("source", "explicit.md"); err != nil {
		t.Fatal(err)
	}
	if err := mw2.Close(); err != nil {
		t.Fatal(err)
	}
	sub2 := &fakeSubmitter{res: ingest.Result{DocumentID: "doc-file-2"}}
	rr = do(t, apiHandler(t, config.Config{}, nil, sub2), http.MethodPost, "/v1/ingest", &buf2, map[string]string{"Content-Type": mw2.FormDataContentType()})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("explicit source: code = %d, body %s", rr.Code, rr.Body.String())
	}
	if sub2.in.Source != "explicit.md" {
		t.Errorf("source = %q, want the explicit form field", sub2.in.Source)
	}
}

func TestStatusEndpoint(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: apiSource(t), Hash: "hash-api-status", Title: "Status doc"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	h := apiHandler(t, config.Config{}, st, nil)

	rr := do(t, h, http.MethodGet, "/v1/documents/"+res.ID+"/status", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body %s", rr.Code, rr.Body.String())
	}
	got := decode[statusResponse](t, rr)
	if got.ID != res.ID || got.Status != "received" {
		t.Errorf("status = %+v, want id=%s status=received", got, res.ID)
	}

	rr = do(t, h, http.MethodGet, "/v1/documents/00000000/status", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown id: code = %d, want 404", rr.Code)
	}

	// The status filter must run before the row limit: a newer failed
	// document must not hide older received ones behind an empty page.
	res2, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: apiSource(t) + "-failed", Hash: "hash-api-status-2"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if err := st.SetStatus(ctx, res2.ID, graph.StatusFailed, "seeded failure"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	rr = do(t, h, http.MethodGet, "/v1/documents?status=received&limit=1", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("filtered page: code = %d, body %s", rr.Code, rr.Body.String())
	}
	page := decode[documentsResponse](t, rr)
	// The newest received document is ours under the suite's serialized
	// topology; under a parallel test run another writer's fresher
	// document is acceptable — the regression this pins is the empty
	// page the filter-after-limit bug produced.
	if len(page.Documents) != 1 || page.Documents[0].Status != "received" {
		t.Errorf("filtered page = %+v, want one received document, not an empty page", page.Documents)
	}

	// The unfiltered list carries a server-side default cap.
	rr = do(t, h, http.MethodGet, "/v1/documents", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("default list: code = %d, body %s", rr.Code, rr.Body.String())
	}
	all := decode[documentsResponse](t, rr)
	if len(all.Documents) > 100 {
		t.Errorf("default list returned %d documents, want <= 100", len(all.Documents))
	}
}

func TestSearchEndpoint(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: apiSource(t), Hash: "hash-api-search"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	tok := apiToken(t)
	text := "The " + tok + " guidance explains the procedure step by step."
	if err := st.PutChunks(ctx, res.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	key := graph.EntityKey("technology", tok)
	if err := st.UpsertExtraction(ctx, res.ID, res.ID+"-c0", "mock:1",
		[]graph.EntityUpsert{{Key: key, Name: tok, Type: "technology"}}, nil); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}

	h := apiHandler(t, config.Config{}, st, nil)
	rr := doJSON(t, h, http.MethodPost, "/v1/search", fmt.Sprintf(`{"query":%q,"kind":"auto"}`, tok))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body %s", rr.Code, rr.Body.String())
	}
	got := decode[searchResponse](t, rr)
	var foundEntity, foundChunk bool
	for _, hit := range got.Hits {
		if hit.Kind == "entity" && hit.ID == graph.EntityID(key) {
			foundEntity = true
		}
		if hit.Kind == "chunk" && hit.ID == res.ID+"-c0" {
			foundChunk = true
		}
	}
	if !foundEntity || !foundChunk {
		t.Errorf("hits = %+v, want entity %s and chunk %s", got.Hits, graph.EntityID(key), res.ID+"-c0")
	}
}

func TestEntityEndpointExcludesLowConfidenceByDefault(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: apiSource(t), Hash: "hash-api-entity"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	tok := apiToken(t)
	a := graph.EntityKey("person", "Ada "+tok)
	b := graph.EntityKey("organization", "Org "+tok)
	c := graph.EntityKey("technology", "Tech "+tok)
	text := "Ada " + tok + " works at Org " + tok + " and uses Tech " + tok + "."
	if err := st.PutChunks(ctx, res.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
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
	ents := []graph.EntityUpsert{
		{Key: a, Name: "Ada " + tok, Type: "person"},
		{Key: b, Name: "Org " + tok, Type: "organization"},
		{Key: c, Name: "Tech " + tok, Type: "technology"},
	}
	if err := st.UpsertExtraction(ctx, res.ID, res.ID+"-c0", "mock:1", ents, facts); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	h := apiHandler(t, config.Config{}, st, nil)

	rr := do(t, h, http.MethodGet, "/v1/entities/"+graph.EntityID(a), nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body %s", rr.Code, rr.Body.String())
	}
	view := decode[entityDTO](t, rr)
	if !hasRel(view, "works_at") {
		t.Errorf("facts = %+v, want works_at", view.Facts)
	}
	if hasRel(view, "uses") {
		t.Errorf("facts = %+v, want the low-confidence uses fact excluded by default", view.Facts)
	}

	rr = do(t, h, http.MethodGet, "/v1/entities/"+url.PathEscape(a)+"?include_low_confidence=true", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("include_low_confidence: code = %d, body %s", rr.Code, rr.Body.String())
	}
	view = decode[entityDTO](t, rr)
	if !hasRel(view, "uses") {
		t.Errorf("facts = %+v, want uses when opted in", view.Facts)
	}

	rr = do(t, h, http.MethodGet, "/v1/entities/no-such-entity", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown entity: code = %d, want 404", rr.Code)
	}
}

func hasRel(view entityDTO, rel string) bool {
	for _, f := range view.Facts {
		if f.Rel == rel {
			return true
		}
	}
	return false
}

func TestTraverseEndpointReturnsSubgraph(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: apiSource(t), Hash: "hash-api-traverse"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	tok := apiToken(t)
	a := graph.EntityKey("person", "Ada "+tok)
	b := graph.EntityKey("organization", "Org "+tok)
	text := "Ada " + tok + " wrote at Org " + tok + "."
	if err := st.PutChunks(ctx, res.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	ents := []graph.EntityUpsert{
		{Key: a, Name: "Ada " + tok, Type: "person"},
		{Key: b, Name: "Org " + tok, Type: "organization"},
	}
	facts := []graph.FactUpsert{{
		HeadKey: a, TailKey: b, Rel: "works_at", Confidence: 0.9,
		HeadMention: graph.Span{Start: 0, End: 4, Text: "Ada"},
		TailMention: graph.Span{Start: 13, End: 16, Text: "Org"},
	}}
	if err := st.UpsertExtraction(ctx, res.ID, res.ID+"-c0", "mock:1", ents, facts); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	h := apiHandler(t, config.Config{}, st, nil)

	rr := doJSON(t, h, http.MethodPost, "/v1/traverse", fmt.Sprintf(`{"entity":%q,"depth":2}`, a))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body %s", rr.Code, rr.Body.String())
	}
	sub := decode[traverseResponse](t, rr)
	foundEdge := false
	for _, e := range sub.Edges {
		if e.From == a && e.To == b && e.Rel == "works_at" {
			foundEdge = true
		}
	}
	if !foundEdge {
		t.Errorf("edges = %+v, want %s -works_at-> %s", sub.Edges, a, b)
	}
	if len(sub.Nodes) < 2 {
		t.Errorf("nodes = %+v, want both endpoints", sub.Nodes)
	}

	// depth is capped at the agent-tool ceiling.
	rr = doJSON(t, h, http.MethodPost, "/v1/traverse", fmt.Sprintf(`{"entity":%q,"depth":3}`, a))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("depth 3: code = %d, want 400", rr.Code)
	}
}

func TestCypherEndpointDeniesWrite(t *testing.T) {
	h := apiHandler(t, config.Config{}, nil, nil)
	for _, q := range []string{
		`{"query":"MATCH (n) DELETE n"}`,
		`{"query":"MATCH (n) RETURN n; DROP CONSTRAINT ON (n:X) ASSERT n.y IS UNIQUE"}`,
		`{"query":""}`,
		`{"query":"MATCH (d:Document) RETURN d UNION MATCH (e:Entity) RETURN e"}`,
		`{"query":"MATCH (n) RETURN n LIMIT 5 LIMIT 1000"}`,
	} {
		rr := doJSON(t, h, http.MethodPost, "/v1/cypher", q)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("query %s: code = %d, want 400", q, rr.Code)
			continue
		}
		body := decode[errorBody](t, rr)
		if body.Error == "" || body.Detail == "" {
			t.Errorf("query %s: error body = %+v, want error and detail", q, body)
		}
	}
}

// TestIngestErrorClassification pins that store failures surface as 5xx
// (via ingest.ErrStore) while input failures stay 400.
func TestIngestErrorClassification(t *testing.T) {
	storeDown := &fakeSubmitter{err: fmt.Errorf("%w: graph down", ingest.ErrStore)}
	h := apiHandler(t, config.Config{}, nil, storeDown)
	rr := doJSON(t, h, http.MethodPost, "/v1/ingest", `{"text":"hello"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("store failure: code = %d, want 500; body %s", rr.Code, rr.Body.String())
	}
	if body := decode[errorBody](t, rr); body.Error != "store_error" {
		t.Errorf("store failure: error = %q, want store_error", body.Error)
	}

	badInput := &fakeSubmitter{err: errors.New("ingest: load: undecodable binary content")}
	rr = doJSON(t, apiHandler(t, config.Config{}, nil, badInput), http.MethodPost, "/v1/ingest", `{"text":"hello"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("input failure: code = %d, want 400; body %s", rr.Code, rr.Body.String())
	}
}

// TestCypherEndpointRejectsExplicitLimitAboveCap pins that the configured
// row cap wins over a query-authored LIMIT — otherwise a caller could
// bypass Limits.CypherRows with "LIMIT 1000000000".
func TestCypherEndpointRejectsExplicitLimitAboveCap(t *testing.T) {
	cfg := config.Config{}
	cfg.Limits.CypherRows = 100
	h := apiHandler(t, cfg, nil, nil) // rejection precedes the store

	rr := doJSON(t, h, http.MethodPost, "/v1/cypher", `{"query":"MATCH (n) RETURN n LIMIT 101"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body %s", rr.Code, rr.Body.String())
	}
	if body := decode[errorBody](t, rr); !strings.Contains(body.Detail, "cypher_rows") {
		t.Errorf("detail = %q, want it to name the cypher_rows cap", body.Detail)
	}

	// LIMIT arithmetic would defeat the cap regardless of the literal
	// value; the guard rejects the query.
	rr = doJSON(t, h, http.MethodPost, "/v1/cypher", `{"query":"MATCH (n) RETURN n LIMIT 2 * 1000"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("arithmetic LIMIT: code = %d, want 400; body %s", rr.Code, rr.Body.String())
	}
	if body := decode[errorBody](t, rr); !strings.Contains(body.Detail, "LIMIT must be") {
		t.Errorf("detail = %q, want the LIMIT-expression rejection", body.Detail)
	}

	// A LIMIT inside the cap passes the guard and reaches the store step.
	rr = doJSON(t, h, http.MethodPost, "/v1/cypher", `{"query":"MATCH (n) RETURN n LIMIT 5"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("small LIMIT: code = %d, want 503 from the nil store (proof the query passed the guard)", rr.Code)
	}

	// A property named "limit" must not suppress the injected cap (it
	// would otherwise uncap the query); the rewritten query reaches the
	// store step like any other.
	rr = doJSON(t, h, http.MethodPost, "/v1/cypher", `{"query":"MATCH (n) WHERE n.limit IS NULL RETURN n"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("n.limit query: code = %d, want 503 (passed the guard with an injected cap)", rr.Code)
	}

	// A query LIMIT within the configured cap is judged against the
	// configured cap, not the request's smaller limit: the request limit
	// only tightens the injected parameter for queries without their own
	// LIMIT.
	rr = doJSON(t, h, http.MethodPost, "/v1/cypher", `{"query":"MATCH (n) RETURN n LIMIT 5","limit":3}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("request limit below query LIMIT: code = %d, want 503 (within the configured cap)", rr.Code)
	}
}

// TestCypherEndpointAllowsRead exercises the allowed path end to end: the
// guard rewrites the query with the injected LIMIT, the store runs it and
// the Bolt node becomes a JSON object.
func TestCypherEndpointAllowsRead(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: apiSource(t), Hash: "hash-api-cypher", Title: "Cypher doc"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	h := apiHandler(t, config.Config{}, st, nil)

	q := fmt.Sprintf(`MATCH (d:Document) WHERE d.id = '%s' RETURN d.id AS id, d AS node, date('2026-01-02') AS day, duration('P1DT2H') AS dur`, res.ID)
	rr := doJSON(t, h, http.MethodPost, "/v1/cypher", fmt.Sprintf(`{"query":%q,"limit":5}`, q))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body %s", rr.Code, rr.Body.String())
	}
	got := decode[cypherResponse](t, rr)
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %+v, want the seeded document", got.Rows)
	}
	if got.Rows[0]["id"] != res.ID {
		t.Errorf("row id = %v, want %s", got.Rows[0]["id"], res.ID)
	}
	node, ok := got.Rows[0]["node"].(map[string]any)
	if !ok {
		t.Fatalf("node = %v, want a JSON object", got.Rows[0]["node"])
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

	// Temporal values must not silently blank out: a Date becomes an
	// ISO date string, a Duration a structured amount.
	if day := got.Rows[0]["day"]; day != "2026-01-02" {
		t.Errorf("day = %v (%T), want the ISO date string", day, day)
	}
	dur, ok := got.Rows[0]["dur"].(map[string]any)
	if !ok {
		t.Fatalf("dur = %v (%T), want a structured duration", got.Rows[0]["dur"], got.Rows[0]["dur"])
	}
	if dur["days"] != float64(1) {
		t.Errorf("dur.days = %v, want 1 (P1DT2H)", dur["days"])
	}
}

// TestCypherEndpointSyntaxErrorMapsTo400 pins the client-error
// classification: Memgraph reports syntax errors as Neo.ClientError, which
// must surface as 400 query_failed, not 500.
func TestCypherEndpointSyntaxErrorMapsTo400(t *testing.T) {
	st := graphtest.Store(t)
	h := apiHandler(t, config.Config{}, st, nil)
	rr := doJSON(t, h, http.MethodPost, "/v1/cypher", `{"query":"MATCH (n RETURN n"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body %s", rr.Code, rr.Body.String())
	}
	if body := decode[errorBody](t, rr); body.Error != "query_failed" {
		t.Errorf("error = %q, want query_failed", body.Error)
	}
}

// TestCypherEndpointTimeoutMapsTo504 pins the timeout classification: the
// driver reports a connectivity error that does not wrap
// context.DeadlineExceeded, so the handler must consult the request context.
func TestCypherEndpointTimeoutMapsTo504(t *testing.T) {
	st := graphtest.Store(t)
	cfg := config.Config{}
	cfg.Limits.CypherRows = 10
	cfg.Limits.CypherTimeoutMS = 2
	h := apiHandler(t, cfg, st, nil)

	// The aggregation cannot be short-circuited by the injected row cap
	// (LIMIT applies after aggregation), and 2M elements keep the server
	// memory bounded (~140 MB) while taking far longer than the 2 ms
	// deadline to build/count.
	q := `UNWIND range(1, 2000000) AS i RETURN count(i) AS n`
	rr := doJSON(t, h, http.MethodPost, "/v1/cypher", fmt.Sprintf(`{"query":%q}`, q))
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("code = %d, want 504; body %s", rr.Code, rr.Body.String())
	}
	if body := decode[errorBody](t, rr); body.Error != "query_timeout" {
		t.Errorf("error = %q, want query_timeout", body.Error)
	}
}

// stubEmbedder returns a fixed vector for every input (vector-leg stub).
type stubEmbedder struct{ vec []float64 }

func (s stubEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i := range texts {
		out[i] = s.vec
	}
	return out, nil
}

// TestSearchEndpointUsesEmbedderWhenEnabled pins the api wiring: with an
// embedder set and a query that matches no name, a vector-seeded entity
// still surfaces through the endpoint.
func TestSearchEndpointUsesEmbedderWhenEnabled(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	const dim = 768
	if err := st.EnsureVectorIndex(ctx, dim); err != nil {
		t.Fatalf("EnsureVectorIndex: %v", err)
	}
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: apiSource(t), Hash: "hash-api-embed"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	tok := apiToken(t)
	text := "The " + tok + " guidance."
	if err := st.PutChunks(ctx, res.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	key := graph.EntityKey("technology", tok)
	if err := st.UpsertExtraction(ctx, res.ID, res.ID+"-c0", "mock:1",
		[]graph.EntityUpsert{{Key: key, Name: tok, Type: "technology"}}, nil); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	vec := graphtest.Vector(tok, dim)
	if err := st.SetEntityEmbedding(ctx, graph.EntityID(key), vec); err != nil {
		t.Fatalf("SetEntityEmbedding: %v", err)
	}

	srv := NewServer(config.Config{}, st, nil)
	srv.SetEmbedder(stubEmbedder{vec})
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/v1/search", fmt.Sprintf(`{"query":%q,"kind":"entity"}`, apiToken(t)))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body %s", rr.Code, rr.Body.String())
	}
	got := decode[searchResponse](t, rr)
	for _, h := range got.Hits {
		if h.ID == graph.EntityID(key) {
			return
		}
	}
	t.Errorf("hits = %+v, want the vector-seeded entity %s", got.Hits, graph.EntityID(key))
}

func TestBodyCaps(t *testing.T) {
	cfg := config.Config{}
	cfg.Ingest.MaxUploadMB = 1
	sub := &fakeSubmitter{res: ingest.Result{DocumentID: "doc-cap"}}
	h := apiHandler(t, cfg, nil, sub)

	big := strings.Repeat("a", 2<<20)
	rr := doJSON(t, h, http.MethodPost, "/v1/ingest", `{"text":"`+big+`"}`)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized JSON: code = %d, want 413; body %s", rr.Code, rr.Body.String())
	}
	if sub.called {
		t.Fatal("submitter called for an oversized JSON body")
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "big.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	rr = do(t, h, http.MethodPost, "/v1/ingest", &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: code = %d, want 413; body %s", rr.Code, rr.Body.String())
	}
	if sub.called {
		t.Fatal("submitter called for an oversized upload")
	}
}
