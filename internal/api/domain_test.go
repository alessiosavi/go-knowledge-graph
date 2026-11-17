package api

import (
	"context"
	"encoding/json"
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
	result, err := session.Run(ctx, `CREATE (:Domain {id:$id,name:'Quillon'}), (:Domain {id:$empty,name:'Zorbo'})`, map[string]any{"id": slug, "empty": slug + "-empty"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = result.Consume(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		r, e := session.Run(ctx, `MATCH (d:Domain) WHERE d.id IN $ids DELETE d`, map[string]any{"ids": []string{slug, slug + "-empty"}})
		if e == nil {
			_, _ = r.Consume(ctx)
		}
	}()
	for _, status := range []graph.Status{graph.StatusDone, graph.StatusFailed} {
		doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: slug + string(status), Hash: slug, Domain: slug})
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
	emptyFound := false
	found := false
	for _, row := range rows {
		if row.ID == slug+"-empty" {
			emptyFound = true
			if row.Documents != 0 {
				t.Fatalf("empty domain=%+v", row)
			}
		}
		if row.ID == slug {
			found = true
			if row.Name != "Quillon" || row.Documents != 1 {
				t.Errorf("domain=%+v", row)
			}
		}
	}
	if !found || !emptyFound {
		t.Fatal("domain missing")
	}
	rr = doJSON(t, h, "GET", "/v1/documents?domain="+slug, "")
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	var docs struct{ Documents []struct{ Domain string } }
	_ = json.Unmarshal(rr.Body.Bytes(), &docs)
	if len(docs.Documents) != 2 {
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
