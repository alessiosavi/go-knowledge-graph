package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
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

func newAgentFixture(t *testing.T) agentFixture {
	t.Helper()
	st := graphtest.Store(t)
	ctx := context.Background()
	token := fmt.Sprintf("agq%d", time.Now().UnixNano())
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{
		Kind: "file", Source: "file:agent-" + token, Hash: "hash-agent-" + token,
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
	fx := newAgentFixture(t)
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
		}
		if h.Kind == "chunk" && h.ID == fx.docID+"-c0" {
			foundChunk = true
			if h.DocumentID != fx.docID {
				t.Errorf("chunk hit document_id = %q, want %s", h.DocumentID, fx.docID)
			}
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
	fx := newAgentFixture(t)

	s := NewServer(config.Config{}, fx.st, nil)
	res, err := s.execTool(context.Background(), "get_entity", json.RawMessage(fmt.Sprintf(`{"id":%q,"include_low_confidence":true}`, fx.entityID)))
	if err != nil {
		t.Fatalf("get_entity by id: %v", err)
	}
	out := unmarshalResult[entityOut](t, res)
	if out.Name != "Ada "+fx.token || out.Type != "person" {
		t.Errorf("entity = %+v, want the seeded Ada", out)
	}
	found := false
	for _, f := range out.Facts {
		if f.Rel == "uses" && f.Other.Key == fx.otherKey {
			found = true
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
	fx := newAgentFixture(t)
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
		}
	}
	if !foundEdge {
		t.Errorf("edges = %+v, want the uses fact to the seeded engine", out.Edges)
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
