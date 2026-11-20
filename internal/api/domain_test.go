package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestDomainsREST(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	slug := apiToken(t)
	drv, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(ctx)
	session := drv.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)
	params := map[string]any{"id": slug, "empty": slug + "-empty", "unbound": slug + "-unbound"}
	result, err := session.Run(ctx, `
CREATE (:Domain {id:$id,name:'Quillon'}),
       (:Domain {id:$empty,name:'Zorbo'}),
       (:Domain {id:$unbound}),
       (:ProductStore {store:$id,domain:$id}),
       (:ProductStore {store:$empty,domain:$empty})
WITH 1 AS created
UNWIND [1, 2, 3] AS i
CREATE (:Product {id:$id + '-' + toString(i),store:$id,domain:$id})
`, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = result.Consume(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		r, e := session.Run(ctx, `
MATCH (n)
WHERE (n:Product AND n.store=$id)
   OR (n:ProductStore AND n.store IN [$id,$empty])
   OR (n:Domain AND n.id IN [$id,$empty,$unbound])
DELETE n`, params)
		if e == nil {
			_, _ = r.Consume(ctx)
		}
	}()
	for i, status := range []graph.Status{graph.StatusDone, graph.StatusDone, graph.StatusFailed} {
		source := fmt.Sprintf("%s-%d", slug, i)
		doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: source, Hash: source, Domain: slug})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.DeleteDocument(ctx, doc.ID) })
		if err := st.SetStatus(ctx, doc.ID, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	h := apiHandler(t, config.Config{}, st, nil)
	rr := doJSON(t, h, "GET", "/v1/domains", "")
	if rr.Code != 200 {
		t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var rows []graph.DomainCount
	if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, row := range rows {
		switch row.ID {
		case slug + "-empty":
			if row.Documents != 0 || row.Products != 0 || !row.ProductsBound {
				t.Fatalf("empty domain=%+v", row)
			}
		case slug + "-unbound":
			if row.Products != 0 || row.ProductsBound {
				t.Fatalf("unbound domain=%+v", row)
			}
		case slug:
			if row.Name != "Quillon" || row.Documents != 2 || row.Products != 3 || !row.ProductsBound {
				t.Errorf("domain=%+v", row)
			}
		default:
			continue
		}
		found[row.ID] = true
	}
	if len(found) != 3 {
		t.Fatalf("domains found=%v", found)
	}
	rr = doJSON(t, h, "GET", "/v1/documents?domain="+slug, "")
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	var docs struct{ Documents []struct{ Domain string } }
	_ = json.Unmarshal(rr.Body.Bytes(), &docs)
	if len(docs.Documents) != 3 {
		t.Fatalf("docs=%s", rr.Body.String())
	}
	for _, d := range docs.Documents {
		if d.Domain != slug {
			t.Fatal(d)
		}
	}
	rr = doJSON(t, h, "POST", "/v1/search", `{"query":"Quillon","domain":"BAD"}`)
	if rr.Code != 400 {
		t.Fatalf("invalid domain HTTP %d", rr.Code)
	}
	rr = doJSON(t, h, "GET", "/v1/documents?domain=BAD", "")
	if rr.Code != 400 {
		t.Fatalf("invalid documents domain HTTP %d: %s", rr.Code, rr.Body.String())
	}
}
