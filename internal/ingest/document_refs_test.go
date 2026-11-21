package ingest_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	refA  = "refs-a.test:1"
	refA2 = "refs-a.test:2"
	refA3 = "refs-a.test:3"
	refB  = "refs-b.test:1"
)

func productRefStores(t *testing.T) func(string) {
	t.Helper()
	drv, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close(context.Background()) })
	write := func(query string) {
		t.Helper()
		ctx := context.Background()
		sess := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
		defer sess.Close(ctx)
		res, err := sess.Run(ctx, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := res.Consume(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		write(`MATCH (d:Document) WHERE d.source STARTS WITH 'partc-refs-' DETACH DELETE d`)
		write(`MATCH (n:ProductRef) WHERE n.store IN ['refs-a.test','refs-b.test'] DETACH DELETE n`)
		write(`MATCH (n:ProductStore) WHERE n.store IN ['refs-a.test','refs-b.test','refs-unbound.test','refs-missing.test'] DELETE n`)
		write(`MATCH (n:Domain) WHERE n.id IN ['refs-da','refs-db','refs-deleted'] DELETE n`)
	})
	write(`CREATE (:Domain {id:'refs-da'}), (:Domain {id:'refs-db'}),
		(:ProductStore {store:'refs-a.test',domain:'refs-da'}), (:ProductStore {store:'refs-b.test',domain:'refs-db'}),
		(:ProductStore {store:'refs-unbound.test'}), (:ProductStore {store:'refs-missing.test',domain:'refs-deleted'})`)
	return write
}

func requireRefError(t *testing.T, err error, code string) {
	t.Helper()
	var refsErr *graph.DocumentRefsError
	if !errors.As(err, &refsErr) || refsErr.Code != code {
		t.Fatalf("ref error = %v, want %s", err, code)
	}
}

func expectStoredProductRefs(t *testing.T, st *graph.Store, id string, want []string) *graph.Document {
	t.Helper()
	d, err := st.Document(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.ProductRefs == nil || !slices.Equal(d.ProductRefs, want) {
		t.Errorf("raw refs for %s = %#v, want a canonical list %v", id, d.ProductRefs, want)
	}
	rows, err := st.RunReadQuery(t.Context(), `
MATCH (:Document {id:$id})-[:REFS]->(r:ProductRef) RETURN r.product_id AS product ORDER BY product`,
		map[string]any{"id": id}, 51)
	if err != nil {
		t.Fatal(err)
	}
	var linked []string
	for _, row := range rows.Rows {
		linked = append(linked, row["product"].(string))
	}
	if !slices.Equal(linked, want) {
		t.Errorf("REFS for %s = %v, want %v", id, linked, want)
	}
	return d
}

func TestDocumentProductRefAdmissionAndLifecycle(t *testing.T) {
	env := newPipelineEnv(t)
	write := productRefStores(t)
	submit := func(in ingest.Input) ingest.Result {
		t.Helper()
		res, err := env.p.Submit(t.Context(), in)
		if err != nil || res.Noop {
			t.Fatalf("submit %v: %+v, %v", in, res, err)
		}
		return res
	}
	t.Run("new source", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			refs        []string
			want        []string
			domain, err string
		}{
			{name: "omitted"},
			{name: "clear", refs: []string{}},
			{name: "one domain", refs: []string{refA2, refA}, want: []string{refA, refA2}, domain: "refs-da"},
			{name: "several domains", refs: []string{refB, refA}, want: []string{refA, refB}},
			{name: "unknown", refs: []string{"refs-unknown.test:1"}, err: "products_unknown_store"},
			{name: "unbound", refs: []string{"refs-unbound.test:1"}, err: "products_unknown_store"},
			{name: "domain missing", refs: []string{"refs-missing.test:1"}, err: "domain_missing"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				in := ingest.Input{Source: "partc-refs-new-" + tc.name, Text: "A new product page.", Products: tc.refs}
				res, err := env.p.Submit(t.Context(), in)
				if tc.err != "" {
					requireRefError(t, err, tc.err)
					return
				}
				if err != nil || res.Noop {
					t.Fatalf("submit: %+v, %v", res, err)
				}
				if doc := expectStoredProductRefs(t, env.st, res.DocumentID, tc.want); doc.Domain != tc.domain {
					t.Errorf("domain = %q, want %q", doc.Domain, tc.domain)
				}
			})
		}
	})
	t.Run("unchanged force and failed", func(t *testing.T) {
		in := ingest.Input{Source: "partc-refs-unchanged", Text: "Original content.", Products: []string{refA, refA2}}
		id := submit(in).DocumentID
		write(`MATCH (s:ProductStore {store:'refs-a.test'}) REMOVE s.domain`)
		for _, tc := range []struct {
			refs []string
			err  string
		}{
			{refs: []string{refA2, refA}}, {},
			{refs: []string{refB}, err: "refs_changed_use_force"},
			{refs: []string{}, err: "refs_changed_use_force"},
		} {
			in.Products = tc.refs
			res, err := env.p.Submit(t.Context(), in)
			if tc.err != "" {
				requireRefError(t, err, tc.err)
			} else if err != nil || !res.Noop || res.DocumentID != id {
				t.Fatalf("no-op for %v: %+v, %v", in.Products, res, err)
			}
		}
		in.Force, in.Products = true, []string{refA2, refA}
		if got := submit(in); got.DocumentID != id {
			t.Fatal("equal force changed document identity")
		}
		in.Products = nil
		submit(in)
		expectStoredProductRefs(t, env.st, id, []string{refA, refA2})
		in.Products = []string{refB}
		_, err := env.p.Submit(t.Context(), in)
		requireRefError(t, err, "domain_pin_conflict")

		write(`MATCH (s:ProductStore {store:'refs-a.test'}) SET s.domain = 'refs-da'`)
		in.Products = []string{refA3}
		submit(in)
		expectStoredProductRefs(t, env.st, id, []string{refA3})
		in.Products = []string{}
		submit(in)
		expectStoredProductRefs(t, env.st, id, nil)

		in.Force = false
		for _, tc := range []struct {
			refs, want []string
			err        string
		}{
			{refs: []string{refA}, want: []string{refA}},
			{want: []string{refA}},
			{refs: []string{refB}, err: "domain_pin_conflict"},
			{refs: []string{}},
		} {
			if err := env.st.SetStatus(t.Context(), id, graph.StatusFailed, "failed"); err != nil {
				t.Fatal(err)
			}
			in.Products = tc.refs
			res, err := env.p.Submit(t.Context(), in)
			if tc.err != "" {
				requireRefError(t, err, tc.err)
			} else if err != nil || res.Noop || res.DocumentID != id {
				t.Fatalf("failed retry for %v: %+v, %v", in.Products, res, err)
			} else {
				expectStoredProductRefs(t, env.st, id, tc.want)
			}
		}
	})
	t.Run("first pin on an unassigned version", func(t *testing.T) {
		for _, status := range []graph.Status{graph.StatusReceived, graph.StatusFailed} {
			in := ingest.Input{Source: "partc-refs-first-pin-" + string(status), Text: "Pin this document."}
			id := submit(in).DocumentID
			if err := env.st.SetStatus(t.Context(), id, status, ""); err != nil {
				t.Fatal(err)
			}
			in.Products, in.Force = []string{refA}, status != graph.StatusFailed
			submit(in)
			if doc := expectStoredProductRefs(t, env.st, id, []string{refA}); doc.Domain != "refs-da" {
				t.Errorf("status %s first pin = %q", status, doc.Domain)
			}
		}
	})
}

func TestDocumentProductRefVersionInheritance(t *testing.T) {
	env := newPipelineEnv(t)
	write := productRefStores(t)
	source := "partc-refs-versions"
	original := ingest.Input{Source: source, Text: "Version one.", Products: []string{refA}}
	v1 := mustProcess(t, env, original)
	v2 := mustProcess(t, env, ingest.Input{Source: source, Text: "Version two is different.", Products: []string{refA2}})
	expectStoredProductRefs(t, env.st, v2.DocumentID, []string{refA2})
	old, err := env.st.Document(t.Context(), v1.DocumentID)
	if err != nil || old.Status != graph.StatusSuperseded || !slices.Equal(old.ProductRefs, []string{refA}) {
		t.Fatalf("superseded history: %+v, %v", old, err)
	}
	if n := rawCount(t, os.Getenv(graphtest.URIEnv), "MATCH (:Document {id:'"+v1.DocumentID+"'})-[r:REFS]->() RETURN count(r) AS n"); n != 0 {
		t.Fatalf("superseded ref edge count = %d", n)
	}
	pending, err := env.p.Submit(t.Context(), ingest.Input{Source: source, Text: "Pending version.", Products: []string{refA3}})
	if err != nil || pending.Noop {
		t.Fatalf("pending version: %+v, %v", pending, err)
	}
	write(`MATCH (s:ProductStore {store:'refs-a.test'}) REMOVE s.domain`)
	for _, in := range []ingest.Input{
		{Source: source, Text: "Changed content explicitly repeats stored refs.", Products: []string{refA2}},
		{Source: source, Text: "Changed content inherits stored refs."},
		{Source: source, Text: original.Text}, // A11 revives a superseded same-ID version.
	} {
		res := mustProcess(t, env, in)
		expectStoredProductRefs(t, env.st, res.DocumentID, []string{refA2})
		if in.Text == original.Text && res.DocumentID != v1.DocumentID {
			t.Fatalf("A11 revived ID = %s, want %s", res.DocumentID, v1.DocumentID)
		}
	}
	cleared := mustProcess(t, env, ingest.Input{Source: source, Text: "Version that clears refs.", Products: []string{}})
	expectStoredProductRefs(t, env.st, cleared.DocumentID, nil)

	// A superseded raw set is historical. With no live done donor it is
	// cleared on resubmission, even if the old REFS have yet to be retired.
	write(`MATCH (s:ProductStore {store:'refs-a.test'}) SET s.domain = 'refs-da'`)
	in := ingest.Input{Source: "partc-refs-no-donor", Text: "Version with refs.", Products: []string{refA}}
	obsolete := mustProcess(t, env, in)
	replacement, err := env.p.Submit(t.Context(), ingest.Input{Source: in.Source, Text: "Still pending."})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.st.Supersede(t.Context(), obsolete.DocumentID, replacement.DocumentID); err != nil {
		t.Fatal(err)
	}
	in.Products = nil
	revived := mustProcess(t, env, in)
	if revived.DocumentID != obsolete.DocumentID {
		t.Fatalf("revived ID = %s, want %s", revived.DocumentID, obsolete.DocumentID)
	}
	expectStoredProductRefs(t, env.st, revived.DocumentID, nil)

	// More than one live done version can exist during overlapping work.
	// Status updates must not change which completed version supplies refs.
	source = "partc-refs-two-done"
	for _, version := range []struct{ text, ref string }{{"Older done.", refA}, {"Newer done.", refA2}} {
		res, err := env.p.Submit(t.Context(), ingest.Input{Source: source, Text: version.text, Products: []string{version.ref}})
		if err != nil {
			t.Fatal(err)
		}
		if err := env.st.SetStatus(t.Context(), res.DocumentID, graph.StatusDone, ""); err != nil {
			t.Fatal(err)
		}
	}
	write(`MATCH (d:Document {source:'partc-refs-two-done'}) WHERE d.product_refs_raw = ['refs-a.test:1'] SET d.updated_at = '9999'`)
	latest := mustProcess(t, env, ingest.Input{Source: source, Text: "Use the newest live done refs."})
	expectStoredProductRefs(t, env.st, latest.DocumentID, []string{refA2})
}

func TestDocumentProductRefsRejectedUpsertRestoresSpool(t *testing.T) {
	env := newPipelineEnv(t)
	write := productRefStores(t)
	write(`CREATE (:Domain {id:'refs-deleted'})`)
	in := ingest.Input{Source: "partc-refs-spool", Text: "Original content.", Title: "Current"}
	res, err := env.p.Submit(t.Context(), in)
	if err != nil || res.Noop {
		t.Fatalf("initial submit: %+v, %v", res, err)
	}
	paths, err := filepath.Glob(filepath.Join(env.cfg.Ingest.SpoolDir, res.DocumentID+"-*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("payload paths = %v, %v", paths, err)
	}
	path := paths[0]
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ingest.SetBeforeDocumentUpsert(env.p, func() {
		backup, err := os.ReadFile(path + ".prev")
		if err != nil || !bytes.Equal(backup, before) {
			t.Fatalf("backup while the upsert is pending = %s, %v", backup, err)
		}
		write(`MATCH (s:ProductStore {store:'refs-missing.test'}) REMOVE s.domain`)
		deleted, err := env.st.DeleteEmptyDomain(t.Context(), "refs-deleted")
		if err != nil || !deleted {
			t.Fatalf("concurrent domain deletion = %t, %v", deleted, err)
		}
	})
	in.Force, in.Title, in.Products = true, "Rejected", []string{"refs-missing.test:1"}
	_, err = env.p.Submit(t.Context(), in)
	requireRefError(t, err, "domain_missing")
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Errorf("payload after rejection = %s, %v, want %s", after, err, before)
	}
	if _, err := os.Stat(path + ".prev"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backup stat after restoration = %v", err)
	}
	doc := expectStoredProductRefs(t, env.st, res.DocumentID, nil)
	if doc.Domain != "" || doc.Title != "Current" || doc.Status != graph.StatusReceived {
		t.Errorf("graph changed on rejected upsert: %+v", doc)
	}
}
