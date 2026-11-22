package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// unmarshalResult round-trips a tool result through its JSON encoding.
func unmarshalResult[T any](t *testing.T, v any) T {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode result %s: %v", raw, err)
	}
	return out
}

// agentFixture seeds two entities, one fact and one chunk with a unique
// per-run token.
type agentFixture struct {
	st        *graph.Store
	token     string
	docID     string
	entityID  string
	entityKey string
	otherKey  string
}

const agentEvidenceDate = "2024-03-01T00:00:00Z"
const storedAgentEvidenceDate = "2024-03-01T00:00:00.000000000Z"

func assertAgentEvidenceDate(t *testing.T, value any, present bool) {
	t.Helper()
	row := unmarshalResult[map[string]any](t, value)
	if !present {
		_, hasDate := row["document_date"]
		_, hasSource := row["document_date_source"]
		if hasDate || hasSource {
			t.Errorf("date fields should be omitted: %v", row)
		}
	} else if row["document_date"] != agentEvidenceDate || row["document_date_source"] != "caller" {
		t.Errorf("document_date=%v document_date_source=%v, want %s (caller)", row["document_date"], row["document_date_source"], agentEvidenceDate)
	}
}

func newAgentFixture(t *testing.T) agentFixture {
	t.Helper()
	return seedAgentFixture(t, "")
}

func seedAgentFixture(t *testing.T, documentDate string) agentFixture {
	t.Helper()
	st := graphtest.Store(t)
	ctx := context.Background()
	token := fmt.Sprintf("agq%d", time.Now().UnixNano())
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{
		Kind: "file", Source: "file:agent-" + token, Hash: "hash-agent-" + token, DocumentDate: documentDate,
	})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	key := graph.EntityKey("Ada " + token)
	other := graph.EntityKey("Engine " + token)
	text := "Ada " + token + " uses Engine " + token + " daily."
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	engineStart := 4 + len(token) + 6
	ents := []graph.EntityUpsert{
		{Key: key, Name: "Ada " + token, Type: "person"},
		{Key: other, Name: "Engine " + token, Type: "technology"},
	}
	facts := []graph.FactUpsert{{
		HeadKey: key, TailKey: other, Rel: "uses", Confidence: 0.9,
		HeadMention: graph.Span{Start: 0, End: 3, Text: "Ada"},
		TailMention: graph.Span{Start: engineStart, End: engineStart + 6, Text: "Engine"},
	}}
	if err := st.UpsertExtraction(ctx, doc.ID, doc.ID+"-c0", "mock:1", ents, facts); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	return agentFixture{
		st: st, token: token, docID: doc.ID,
		entityID: graph.EntityID(key), entityKey: key, otherKey: other,
	}
}

func TestToolSearch(t *testing.T) {
	fx := seedAgentFixture(t, storedAgentEvidenceDate)
	s := NewServer(config.Config{}, fx.st, nil)

	res, err := s.execTool(context.Background(), "search", json.RawMessage(fmt.Sprintf(`{"query":%q}`, fx.token)))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	out := unmarshalResult[searchOut](t, res)
	found, foundChunk := false, false
	for _, h := range out.Hits {
		if h.Kind == "entity" && h.ID == fx.entityID {
			found = true
			if h.DocumentID != "" {
				t.Errorf("entity hit carries document_id = %q, want empty", h.DocumentID)
			}
			assertAgentEvidenceDate(t, h, false)
		}
		if h.Kind == "chunk" && h.ID == fx.docID+"-c0" {
			foundChunk = true
			if h.DocumentID != fx.docID {
				t.Errorf("chunk hit document_id = %q, want %s", h.DocumentID, fx.docID)
			}
			assertAgentEvidenceDate(t, h, true)
		}
	}
	if !found || !foundChunk {
		t.Errorf("hits = %+v, want the seeded entity %s and chunk %s-c0", out.Hits, fx.entityID, fx.docID)
	}

	if _, err := s.execTool(context.Background(), "search", json.RawMessage(`{"query":"  "}`)); err == nil {
		t.Error("empty query: want an error")
	}
}

func TestToolGetEntityByIDAndName(t *testing.T) {
	fx := seedAgentFixture(t, storedAgentEvidenceDate)

	s := NewServer(config.Config{}, fx.st, nil)
	res, err := s.execTool(context.Background(), "get_entity", json.RawMessage(fmt.Sprintf(`{"id":%q,"include_low_confidence":true}`, fx.entityID)))
	if err != nil {
		t.Fatalf("get_entity by id: %v", err)
	}
	out := unmarshalResult[entityOut](t, res)
	if out.Name != "Ada "+fx.token || out.Type != "person" {
		t.Errorf("entity = %+v, want the seeded Ada", out)
	}
	if len(out.Together) != 1 {
		t.Fatalf("together = %+v, want the seeded Engine", out.Together)
	}
	if got := out.Together[0]; got.ID != graph.EntityID(fx.otherKey) || got.Key != fx.otherKey || got.Name != "Engine "+fx.token || got.Type != "technology" || got.SharedChunks != 1 {
		t.Errorf("together[0] = %+v, want Engine in one shared chunk", got)
	}
	found := false
	for _, f := range out.Facts {
		if f.Rel == "uses" && f.Other.Key == fx.otherKey {
			found = true
			assertAgentEvidenceDate(t, f, true)
		}
	}
	if !found {
		t.Errorf("facts = %+v, want the uses fact", out.Facts)
	}

	res, err = s.execTool(context.Background(), "get_entity", json.RawMessage(fmt.Sprintf(`{"name":%q}`, "Ada "+fx.token)))
	if err != nil {
		t.Fatalf("get_entity by name: %v", err)
	}
	if byName := unmarshalResult[entityOut](t, res); byName.ID != fx.entityID {
		t.Errorf("name lookup id = %q, want %s", byName.ID, fx.entityID)
	}

	if _, err := s.execTool(context.Background(), "get_entity", json.RawMessage(`{}`)); err == nil {
		t.Error("empty args: want an id-or-name error")
	}
}

func TestToolTraverse(t *testing.T) {
	fx := seedAgentFixture(t, storedAgentEvidenceDate)
	s := NewServer(config.Config{}, fx.st, nil)

	res, err := s.execTool(context.Background(), "traverse", json.RawMessage(fmt.Sprintf(
		`{"entity":%q,"direction":"both","include_low_confidence":true}`, fx.entityKey)))
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	out := unmarshalResult[traverseOut](t, res)
	foundEdge := false
	for _, e := range out.Edges {
		if e.Rel == "uses" && e.To == fx.otherKey {
			foundEdge = true
			assertAgentEvidenceDate(t, e, true)
		}
	}
	if !foundEdge {
		t.Errorf("edges = %+v, want the uses fact to the seeded engine", out.Edges)
	}
}

func TestToolRelatedEntityDomainsJSON(t *testing.T) {
	fx := newAgentFixture(t)
	ctx := withDomain(context.Background(), "zorbo")
	t.Cleanup(func() {
		if err := fx.st.DeleteDocument(ctx, fx.docID); err != nil {
			t.Error(err)
		}
	})
	drv, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(ctx)
	session := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)
	rows, err := session.Run(ctx, `MATCH (a:Entity {key:$head}), (b:Entity {key:$tail}) SET a.domain='zorbo', b.domain='quillon'`, map[string]any{"head": fx.entityKey, "tail": fx.otherKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rows.Consume(ctx); err != nil {
		t.Fatal(err)
	}
	s := NewServer(config.Config{}, fx.st, nil)
	res, err := s.execTool(context.Background(), "get_entity", json.RawMessage(fmt.Sprintf(`{"id":%q}`, fx.entityID)))
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalResult[map[string]any](t, res)["domain"]; got != "zorbo" {
		t.Errorf("viewed entity domain = %v, want zorbo", got)
	}
	view := unmarshalResult[entityOut](t, res)
	if len(view.Facts) != 1 || len(view.Together) != 1 {
		t.Fatalf("entity JSON=%+v, want one fact and one co-mention", view)
	}
	if ref := view.Facts[0].Other; ref.Key != fx.otherKey || ref.Domain != "quillon" {
		t.Errorf("fact other JSON=%+v, want %s in quillon", ref, fx.otherKey)
	}
	if ref := view.Together[0]; ref.Key != fx.otherKey || ref.Domain != "quillon" {
		t.Errorf("co-mention JSON=%+v, want %s in quillon", ref, fx.otherKey)
	}
	res, err = s.execTool(ctx, "traverse", json.RawMessage(fmt.Sprintf(`{"entity":%q,"direction":"out","depth":1}`, fx.entityID)))
	if err != nil {
		t.Fatal(err)
	}
	sub := unmarshalResult[traverseOut](t, res)
	if len(sub.Nodes) != 2 {
		t.Fatalf("traversal JSON=%+v, want two nodes", sub)
	}
	domains := map[string]string{fx.entityKey: "zorbo", fx.otherKey: "quillon"}
	for _, node := range sub.Nodes {
		if want, ok := domains[node.Key]; !ok || node.Domain != want {
			t.Errorf("traversal node JSON=%+v, want domain %q", node, want)
		}
	}
}

func TestToolListDocumentsAndStatus(t *testing.T) {
	fx := newAgentFixture(t)
	s := NewServer(config.Config{}, fx.st, nil)

	res, err := s.execTool(context.Background(), "list_documents", json.RawMessage(`{"limit":50}`))
	if err != nil {
		t.Fatalf("list_documents: %v", err)
	}
	docs := unmarshalResult[documentsOut](t, res)
	found := false
	for _, d := range docs.Documents {
		if d.ID == fx.docID {
			found = true
		}
	}
	if !found {
		t.Errorf("documents do not contain the seeded %s", fx.docID)
	}

	res, err = s.execTool(context.Background(), "status", json.RawMessage(fmt.Sprintf(`{"document_id":%q}`, fx.docID)))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	one := unmarshalResult[documentsOut](t, res)
	if len(one.Documents) != 1 || one.Documents[0].ID != fx.docID {
		t.Errorf("status = %+v, want the one seeded document", one.Documents)
	}
	wire := unmarshalResult[struct {
		Documents []map[string]any `json:"documents"`
	}](t, res)
	if len(wire.Documents) != 1 || wire.Documents[0]["document_date"] == nil || wire.Documents[0]["document_date_source"] != "ingest" {
		t.Errorf("status wire = %+v, want the ingest date and its origin", wire)
	}
}

func TestToolCypherReadsAndRejectsWrites(t *testing.T) {
	fx := newAgentFixture(t)
	s := NewServer(config.Config{}, fx.st, nil)

	query := fmt.Sprintf(`{"query":"MATCH (e:Entity {id: '%s'}) RETURN e.name AS name"}`, fx.entityID)
	res, err := s.execTool(context.Background(), "cypher", json.RawMessage(query))
	if err != nil {
		t.Fatalf("cypher read: %v", err)
	}
	out := unmarshalResult[cypherOut](t, res)
	if len(out.Rows) != 1 || out.Rows[0]["name"] != "Ada "+fx.token {
		t.Errorf("cypher rows = %+v, want the seeded entity name", out.Rows)
	}

	_, err = s.execTool(context.Background(), "cypher", json.RawMessage(`{"query":"MATCH (n) DELETE n"}`))
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("cypher write err = %v, want a read-only rejection", err)
	}
}

func TestToolEmptyArgumentsUseDefaults(t *testing.T) {
	fx := newAgentFixture(t)
	s := NewServer(config.Config{}, fx.st, nil)

	// Absent/empty/whitespace arguments mean the documented defaults: a
	// no-argument call must not fail with a JSON decode error.
	for _, raw := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("  \n")} {
		res, err := s.execTool(context.Background(), "list_documents", raw)
		if err != nil {
			t.Fatalf("list_documents with %q args: %v", raw, err)
		}
		if docs := unmarshalResult[documentsOut](t, res); len(docs.Documents) == 0 {
			t.Errorf("list_documents with %q args returned no documents", raw)
		}
	}
	if _, err := s.execTool(context.Background(), "status", nil); err != nil {
		t.Errorf("status with nil args: %v", err)
	}
}

func TestToolUnknownName(t *testing.T) {
	fx := newAgentFixture(t)
	s := NewServer(config.Config{}, fx.st, nil)
	_, err := s.execTool(context.Background(), "bogus", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("err = %v, want an unknown-tool error", err)
	}
}

func TestToolGetChunks(t *testing.T) {
	fx := seedAgentFixture(t, storedAgentEvidenceDate)
	s := NewServer(config.Config{}, fx.st, nil)
	res, err := s.execTool(context.Background(), "get_chunks", json.RawMessage(fmt.Sprintf(`{"ids":[%q,"absent"],"neighbours":1}`, fx.docID+"-c0")))
	if err != nil {
		t.Fatalf("get_chunks: %v", err)
	}
	out := unmarshalResult[struct {
		Chunks []struct{ ID, Text string }
		Meta   struct {
			Returned int
			Missing  []string
		}
	}](t, res)
	if len(out.Chunks) != 1 || out.Chunks[0].ID != fx.docID+"-c0" || !strings.Contains(out.Chunks[0].Text, fx.token) || out.Meta.Returned != 1 || len(out.Meta.Missing) != 1 || out.Meta.Missing[0] != "absent" {
		t.Fatalf("get_chunks = %+v", out)
	}
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`{"ids":[]}`), json.RawMessage(`{"ids":["c1"],"neighbours":3}`), json.RawMessage(`{"ids":"c1"}`)} {
		if _, err := s.execTool(context.Background(), "get_chunks", raw); err == nil {
			t.Errorf("get_chunks accepted %s", raw)
		}
	}
	raw := json.RawMessage(fmt.Sprintf(`{"ids":[%q],"with_products":true}`, fx.docID+"-c0"))
	res, err = s.execTool(t.Context(), "get_chunks", raw)
	if err != nil {
		t.Fatal(err)
	}
	enriched := unmarshalResult[struct {
		Chunks []map[string]json.RawMessage
	}](t, res)
	if len(enriched.Chunks) != 1 || string(enriched.Chunks[0]["products"]) != "[]" ||
		string(enriched.Chunks[0]["products_total"]) != "0" {
		t.Errorf("get_chunks products = %+v, want an empty array and zero total", enriched.Chunks)
	} else {
		assertAgentEvidenceDate(t, enriched.Chunks[0], true)
	}
}

type chunkProductsStore struct {
	graphStore
	index *graph.ProductMentionIndex
}

func (s chunkProductsStore) ChunkNeighbourhood(_ context.Context, id string, _ int) (graph.Chunk, []graph.Chunk, error) {
	return graph.Chunk{ID: id, Text: "AB12345 and CD67890"}, nil, nil
}

func (s chunkProductsStore) LoadProductMentionIndex(context.Context) (*graph.ProductMentionIndex, error) {
	return s.index, nil
}

func TestToolGetChunksProductsLimit(t *testing.T) {
	s := NewServer(config.Config{}, nil, nil)
	s.store = chunkProductsStore{index: graph.BuildProductMentionIndex([]graph.ProductIdentifierObservation{
		{Product: graph.Product{ID: "a", Store: "products.test", ProductID: "products.test:a"}, Feature: "catalogue_code", ValueFold: new("AB12345")},
		{Product: graph.Product{ID: "b", Store: "products.test", ProductID: "products.test:b"}, Feature: "catalogue_code", ValueFold: new("CD67890")},
	})}
	call := func(t *testing.T, limit string) (any, error) {
		t.Helper()
		raw := json.RawMessage(fmt.Sprintf(`{"ids":["c1"],"with_products":true,"products_limit":%s}`, limit))
		return s.execTool(t.Context(), "get_chunks", raw)
	}
	for _, limit := range []string{"0", "51", "-1", "3.0", "1e1", "1.5", `"2"`, "null"} {
		t.Run("reject="+limit, func(t *testing.T) {
			if _, err := call(t, limit); err == nil || err.Error() != chunkread.ProductsLimitError {
				t.Errorf("products_limit=%s: got %v, want a products_limit argument error", limit, err)
			}
		})
	}
	t.Run("limit=1", func(t *testing.T) {
		res, err := call(t, "1")
		if err != nil {
			t.Fatal(err)
		}
		out := unmarshalResult[struct {
			Chunks []struct {
				Products []chunkread.ChunkProduct
				Total    int             `json:"products_total"`
				Omitted  json.RawMessage `json:"products_omitted_for_budget"`
			}
		}](t, res)
		if len(out.Chunks) != 1 || out.Chunks[0].Total != 2 || len(out.Chunks[0].Products) != 1 || out.Chunks[0].Omitted != nil {
			t.Errorf("products_limit=1: got %+v, want one product of two with no budget omissions", out)
		}
	})
}

func TestToolResultSizeGuidance(t *testing.T) {
	want := map[string]string{
		"search": "Start with limit 10.", "traverse": "Start with limit 10.", "list_documents": "Start with limit 10.",
		"get_chunks": "Read chunk text, up to 4 KiB per chunk; `truncated` marks a cut, by id (ids from search hits), optionally with up to 2 neighbouring chunks of the same document. Use this instead of cypher to read chunk text. omitted_for_budget may also list neighbour ids.",
	}
	for _, def := range toolDefs() {
		if desc, ok := want[def.Function.Name]; ok {
			if def.Function.Name == "get_chunks" && !strings.Contains(def.Function.Description, "document_date and document_date_source") {
				t.Error("get_chunks description does not explain the document date fields")
			}
			if !strings.Contains(def.Function.Description, desc) {
				t.Errorf("%s description=%q, want %q", def.Function.Name, def.Function.Description, desc)
			}
			delete(want, def.Function.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing tools: %v", want)
	}
}
