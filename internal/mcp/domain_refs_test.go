package mcp

import (
	"context"
	"os"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestRelatedEntityDomainsJSON(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: mcpSource(t), Hash: "domain-refs", Domain: "zorbo"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, doc.ID); err != nil {
			t.Error(err)
		}
	})
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Text: "Acme-Max uses Quillon."}}); err != nil {
		t.Fatal(err)
	}
	head, tail := graph.DomainKey("zorbo", "acme-"+doc.ID), graph.DomainKey("zorbo", "quillon-"+doc.ID)
	ents := []graph.EntityUpsert{{Key: head, Name: "Acme-Max", Type: "organization"}, {Key: tail, Name: "Quillon", Type: "product"}}
	facts := []graph.FactUpsert{{HeadKey: head, TailKey: tail, Rel: "uses", Confidence: .9,
		HeadMention: graph.Span{Start: 0, End: 8}, TailMention: graph.Span{Start: 14, End: 21}}}
	if err := st.UpsertExtractionOpts(ctx, doc.ID, doc.ID+"-c0", "mock:1", ents, facts, graph.WriteOptions{Domain: "zorbo"}); err != nil {
		t.Fatal(err)
	}
	drv, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(ctx)
	session := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)
	rows, err := session.Run(ctx, `MATCH (e:Entity {key:$key}) SET e.domain='quillon'`, map[string]any{"key": tail})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rows.Consume(ctx); err != nil {
		t.Fatal(err)
	}
	cs := connectInMemory(t, config.Config{}, st, nil)
	view := structured[map[string]any](t, callTool(t, cs, "get_entity", map[string]any{"id": graph.EntityID(head)}))
	if got := view["domain"]; got != "zorbo" {
		t.Errorf("viewed entity domain = %v, want zorbo", got)
	}
	factRows, together := view["facts"].([]any), view["together"].([]any)
	if len(factRows) != 1 || len(together) != 1 {
		t.Fatalf("entity JSON=%v, want one fact and one co-mention", view)
	}
	for _, ref := range []map[string]any{factRows[0].(map[string]any)["other"].(map[string]any), together[0].(map[string]any)} {
		if ref["key"] != tail || ref["domain"] != "quillon" {
			t.Errorf("related entity JSON=%v, want %s in quillon", ref, tail)
		}
	}
	sub := structured[map[string]any](t, callTool(t, cs, "traverse", map[string]any{"entity": graph.EntityID(head), "direction": "out", "depth": 1, "domain": "zorbo"}))
	nodes := sub["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("traversal JSON=%v, want two nodes", sub)
	}
	domains := map[string]string{head: "zorbo", tail: "quillon"}
	for _, node := range nodes {
		ref := node.(map[string]any)
		key, _ := ref["key"].(string)
		if want, ok := domains[key]; !ok || ref["domain"] != want {
			t.Errorf("traversal node JSON=%v, want domain %q", ref, want)
		}
	}
}
