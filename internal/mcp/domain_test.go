package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestMCPDomains(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	word := fmt.Sprintf("quillon%d", time.Now().UnixNano())
	ids := map[string]string{}
	for _, slug := range []string{"a", "b"} {
		doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: word + slug, Hash: word, Domain: slug})
		if err != nil {
			t.Fatal(err)
		}
		ids[slug] = doc.ID
		t.Cleanup(func() { _ = st.DeleteDocument(ctx, doc.ID) })
		if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: word}}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertExtractionOpts(ctx, doc.ID, doc.ID+"-c0", "mock", []graph.EntityUpsert{{Key: graph.DomainKey(slug, word), Name: word}}, nil, graph.WriteOptions{Domain: slug}); err != nil {
			t.Fatal(err)
		}
	}
	cs := connectInMemory(t, config.Config{}, st, nil)
	out := structured[searchOut](t, callTool(t, cs, "search", map[string]any{"query": word, "domain": "a", "kind": "chunk"}))
	if len(out.Hits) != 1 || out.Hits[0].Domain != "a" {
		t.Fatalf("search=%+v", out)
	}
	chunks := structured[chunkread.Result](t, callTool(t, cs, "get_chunks", map[string]any{"ids": []string{ids["b"] + "-c0"}, "domain": "a"}))
	if len(chunks.Chunks) != 0 || len(chunks.Meta.Missing) != 1 {
		t.Fatalf("chunks=%+v", chunks)
	}
	candidates := structured[json.RawMessage](t, callTool(t, cs, "get_entity", map[string]any{"name": word}))
	// The store orders equal mention counts by entity id. Pin the complete
	// response: ambiguity must not include any empty single-entity fields.
	slugs := []string{"a", "b"}
	slices.SortFunc(slugs, func(a, b string) int {
		return strings.Compare(graph.EntityID(graph.DomainKey(a, word)), graph.EntityID(graph.DomainKey(b, word)))
	})
	wantCandidates := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		key := graph.DomainKey(slug, word)
		wantCandidates = append(wantCandidates, fmt.Sprintf(`{"domain":%q,"id":%q,"key":%q,"name":%q,"type":""}`, slug, graph.EntityID(key), key, word))
	}
	if want := `{"candidates":[` + strings.Join(wantCandidates, ",") + `]}`; string(candidates) != want {
		t.Fatalf("candidates JSON=%s, want %s", candidates, want)
	}
	entity := structured[entityOut](t, callTool(t, cs, "get_entity", map[string]any{"name": word, "domain": "a"}))
	if entity.Key != graph.DomainKey("a", word) {
		t.Fatalf("entity=%+v", entity)
	}
	for _, tool := range []string{"get_entity", "traverse"} {
		args := map[string]any{"domain": "a", "id": graph.EntityID(graph.DomainKey("b", word)), "entity": graph.DomainKey("b", word)}
		if out := callTool(t, cs, tool, args); !out.IsError {
			t.Errorf("%s accepted out-of-scope start", tool)
		}
	}
	docs := structured[listDocumentsOut](t, callTool(t, cs, "list_documents", map[string]any{"domain": "a", "limit": 1000}))
	for _, doc := range docs.Documents {
		if doc.Domain != "a" {
			t.Errorf("document=%+v", doc)
		}
	}
	for _, tool := range []string{"search", "get_entity", "get_chunks", "traverse", "list_documents", "status"} {
		args := map[string]any{"domain": "BAD", "query": word, "id": graph.EntityID(graph.DomainKey("a", word)), "entity": graph.DomainKey("a", word), "ids": []string{ids["a"] + "-c0"}}
		if out := callTool(t, cs, tool, args); !out.IsError {
			t.Errorf("%s accepted invalid domain", tool)
		}
	}
}
