package ingest_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func chosenDomainEnv(t *testing.T) pipelineEnv {
	t.Helper()
	env := newPipelineEnv(t)
	write := productRefStores(t)
	uri := os.Getenv(graphtest.URIEnv)
	original := rawRecords(t, uri, `MATCH (m:GkgMeta {id:'extraction_schema'}) RETURN m.value AS version`)
	t.Cleanup(func() {
		if len(original) == 0 {
			write(`MATCH (m:GkgMeta {id:'extraction_schema'}) DELETE m`)
		} else {
			version, _ := original[0].Get("version")
			write(fmt.Sprintf(`MATCH (m:GkgMeta {id:'extraction_schema'}) SET m.value=%d`, version))
		}
		write(`MATCH (r:DomainDecision {by:'caller'}) WHERE r.domain IN ['refs-da','refs-db','refs-deleted'] DELETE r`)
	})
	write(`MERGE (m:GkgMeta {id:'extraction_schema'}) SET m.value=4`)
	write(`MATCH (d:Domain {id:'refs-da'}) SET d.name=' Archive / Notes '`)
	write(`MATCH (d:Domain {id:'refs-db'}) SET d.name='Clinical Research'`)
	env.p.SetWriteMode(ingest.WriteModeForSchema(graph.SchemaVersionV4, false))
	return env
}

func expectIngestDomainError(t *testing.T, err error, code, detail string) {
	t.Helper()
	var rejection *graph.IngestDomainError
	if !errors.As(err, &rejection) || !rejection.Conflict || rejection.Code != code || !strings.Contains(rejection.Detail, detail) {
		t.Fatalf("domain rejection = %v, want conflict %s with %q", err, code, detail)
	}
}

func TestChosenDomainVersionsAndDecisions(t *testing.T) {
	env := chosenDomainEnv(t)
	ctx := t.Context()
	old := ingest.Input{Source: "partc-refs-chosen-versions", Text: "Archive contains earlier notes.", Domain: " Archive / Notes "}
	v1 := mustProcess(t, env, old).DocumentID
	newer := ingest.Input{Source: old.Source, Text: "Clinical research contains different notes.", Domain: "clinical research"}
	v2, err := env.p.Submit(ctx, newer)
	if err != nil || v2.Noop || v1 == v2.DocumentID {
		t.Fatalf("new version = %+v, %v", v2, err)
	}
	requireDoc := func(id, domain string, status graph.Status) {
		t.Helper()
		doc, err := env.st.Document(ctx, id)
		if err != nil || doc.Domain != domain || doc.Status != status {
			t.Fatalf("document %q = %+v, %v, want %s %s", id, doc, err, domain, status)
		}
	}
	requireDoc(v1, "refs-da", graph.StatusDone)
	requireDoc(v2.DocumentID, "refs-db", graph.StatusReceived)
	if err := env.p.Process(ctx, v2.DocumentID); err != nil {
		t.Fatal(err)
	}
	requireDoc(v1, "refs-da", graph.StatusSuperseded)
	requireDoc(v2.DocumentID, "refs-db", graph.StatusDone)

	if res, err := env.p.Submit(ctx, newer); err != nil || !res.Noop || res.DocumentID != v2.DocumentID {
		t.Fatalf("same-domain duplicate = %+v, %v", res, err)
	}
	for _, status := range []graph.Status{graph.StatusDone, graph.StatusFailed, graph.StatusSuperseded} {
		if err := env.st.SetStatus(ctx, v2.DocumentID, status, ""); err != nil {
			t.Fatal(err)
		}
		for _, force := range []bool{false, true} {
			conflict := newer
			conflict.Domain, conflict.Force = "refs-da", force
			_, err := env.p.Submit(ctx, conflict)
			expectIngestDomainError(t, err, "domain_conflict", "Clinical Research (refs-db), document "+v2.DocumentID)
		}
		if status != graph.StatusDone {
			if res, err := env.p.Submit(ctx, newer); err != nil || res.Noop || res.DocumentID != v2.DocumentID {
				t.Fatalf("same-domain retry in %s = %+v, %v", status, res, err)
			}
		}
	}
	newer.Force = true
	if res, err := env.p.Submit(ctx, newer); err != nil || res.Noop || res.DocumentID != v2.DocumentID {
		t.Fatalf("same-domain force = %+v, %v", res, err)
	}
	records, err := env.st.DomainDecisions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, record := range records {
		switch record.DocID {
		case v1:
			if record.Domain != "refs-da" || record.PreviousDomain != "" {
				t.Errorf("first version decision = %+v", record)
			}
		case v2.DocumentID:
			if record.Domain != "refs-db" || record.PreviousDomain != "refs-da" {
				t.Errorf("new version decision = %+v", record)
			}
		default:
			continue
		}
		if record.Action != "caller" || record.By != "caller" {
			t.Errorf("decision = %+v, want caller assignment", record)
		}
		counts[record.DocID]++
	}
	if counts[v1] != 1 || counts[v2.DocumentID] != 1 {
		t.Errorf("decision counts = %v, want one per new version", counts)
	}
}

func TestChosenDomainUnassignedVersions(t *testing.T) {
	env := chosenDomainEnv(t)
	ctx := t.Context()
	in := ingest.Input{Source: "partc-refs-unassigned", Text: "Exact content with no domain."}
	res, err := env.p.Submit(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Domain = "refs-da"
	_, err = env.p.Submit(ctx, in)
	expectIngestDomainError(t, err, "document_busy", "retry after the domain is assigned")
	for _, status := range []graph.Status{graph.StatusDone, graph.StatusFailed, graph.StatusSuperseded} {
		if err := env.st.SetStatus(ctx, res.DocumentID, status, ""); err != nil {
			t.Fatal(err)
		}
		_, err := env.p.Submit(ctx, in)
		expectIngestDomainError(t, err, "domain_conflict", "delete it and post again")
	}
}

func TestChosenDomainEffectiveProductRefs(t *testing.T) {
	env := chosenDomainEnv(t)
	source := "partc-refs-chosen-pins"
	mustProcess(t, env, ingest.Input{Source: source, Text: "Original product page.", Domain: "refs-da", Products: []string{refA}})
	for _, tc := range []struct {
		name    string
		refs    []string
		errCode string
	}{
		{name: "inherited pin conflicts"},
		{name: "repeated pin conflicts", refs: []string{refA}},
		{name: "explicit pin conflicts", refs: []string{refA2}},
		{name: "unbound store", refs: []string{"refs-unbound.test:1"}, errCode: "products_unknown_store"},
		{name: "unknown store", refs: []string{"refs-unknown.test:1"}, errCode: "products_unknown_store"},
		{name: "pin matches choice", refs: []string{refB}},
		{name: "multiple domains", refs: []string{refA, refB}},
		{name: "clear inherited refs", refs: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := ingest.Input{Source: source, Text: "New version for " + tc.name, Domain: "refs-db", Products: tc.refs}
			res, err := env.p.Submit(t.Context(), in)
			switch {
			case tc.errCode != "":
				requireRefError(t, err, tc.errCode)
			case strings.HasSuffix(tc.name, "conflicts"):
				expectIngestDomainError(t, err, "domain_conflict", "product refs pin domain refs-da")
			default:
				if err != nil || res.Noop {
					t.Fatalf("Submit = %+v, %v", res, err)
				}
				if doc := expectStoredProductRefs(t, env.st, res.DocumentID, tc.refs); doc.Domain != "refs-db" {
					t.Errorf("domain = %q, want refs-db", doc.Domain)
				}
			}
		})
	}
}

func TestChosenDomainSupersededVersionRejectsConflictingPin(t *testing.T) {
	env := chosenDomainEnv(t)
	const source, original, chosen = "partc-refs-chosen-revival", "Original notes.", "refs-da"
	v1 := mustProcess(t, env, ingest.Input{Source: source, Text: original, Domain: chosen})
	mustProcess(t, env, ingest.Input{Source: source, Text: "Replacement notes.", Domain: chosen})
	doc, err := env.st.Document(t.Context(), v1.DocumentID)
	if err != nil || doc.Status != graph.StatusSuperseded || doc.Domain != chosen {
		t.Fatalf("original version before resubmission = %+v, %v, want superseded in %s", doc, err, chosen)
	}

	body := fmt.Sprintf(`{"source":%q,"text":%q,"domain":%q,"products":[%q]}`, source, original, chosen, refB)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/ingest", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	api.NewServer(env.cfg, env.st, env.p).Handler().ServeHTTP(response, req)
	var rejection struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &rejection); err != nil || response.Code != http.StatusConflict ||
		rejection.Error != "domain_conflict" || rejection.Detail != "product refs pin domain refs-db" {
		t.Errorf("POST /v1/ingest = %d %s, want 409 domain_conflict: product refs pin domain refs-db", response.Code, strings.TrimSpace(response.Body.String()))
	}
	doc, err = env.st.Document(t.Context(), v1.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != graph.StatusSuperseded || doc.Domain != chosen {
		t.Errorf("original version after resubmission = %s in %s, want superseded in %s", doc.Status, doc.Domain, chosen)
	}
}

func TestChosenDomainRefsFromUnboundStore(t *testing.T) {
	env := chosenDomainEnv(t)
	const source = "partc-refs-chosen-unbound"
	mustProcess(t, env, ingest.Input{Source: source, Text: "Original product page.", Domain: "refs-da", Products: []string{refA}})
	rawRecords(t, os.Getenv(graphtest.URIEnv), `MATCH (s:ProductStore {store:'refs-a.test'}) REMOVE s.domain RETURN 1`)

	for _, choice := range []struct{ name, id string }{{"auto", ""}, {"explicit", "refs-da"}} {
		t.Run(choice.name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				refs []string
			}{
				{name: "inherited"},
				{name: "unchanged", refs: []string{refA}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					res := mustProcess(t, env, ingest.Input{
						Source: source, Text: "New version with " + tc.name + " refs in " + choice.name,
						Domain: choice.id, Products: tc.refs,
					})
					doc := expectStoredProductRefs(t, env.st, res.DocumentID, []string{refA})
					if doc.Domain != "refs-da" {
						t.Errorf("domain = %q, want refs-da", doc.Domain)
					}
				})
			}
			_, err := env.p.Submit(t.Context(), ingest.Input{
				Source: source, Text: "New version with changed refs.", Domain: choice.id, Products: []string{refA2},
			})
			requireRefError(t, err, "products_unknown_store")
		})
	}
}

func TestChosenDomainDeletionBeforeAdmissionRestoresSpool(t *testing.T) {
	env := chosenDomainEnv(t)
	uri := os.Getenv(graphtest.URIEnv)
	t.Cleanup(func() { rawRecords(t, uri, `MATCH (d:Domain {id:'chosen-vanished'}) DELETE d RETURN 1`) })
	rawRecords(t, uri, `CREATE (:Domain {id:'chosen-vanished',name:'Vanishing domain'}) RETURN 1`)
	ingest.SetBeforeDocumentUpsert(env.p, func() {
		deleted, err := env.st.DeleteEmptyDomain(t.Context(), "chosen-vanished")
		if err != nil || !deleted {
			t.Fatalf("delete chosen domain = %t, %v", deleted, err)
		}
	})
	_, err := env.p.Submit(t.Context(), ingest.Input{Source: "partc-refs-vanished", Text: "This should not be stored.", Domain: "Vanishing domain"})
	expectIngestDomainError(t, err, "unknown_domain", "chosen-vanished")
	if _, err := env.st.DomainDocumentIDs(t.Context(), "chosen-vanished"); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("chosen domain was recreated: %v", err)
	}
	if rows := rawRecords(t, uri, `MATCH (d:Document {source:'partc-refs-vanished'}) RETURN d.id`); len(rows) != 0 {
		t.Fatalf("rejected document persisted: %v", rows)
	}
	if entries, err := os.ReadDir(env.cfg.Ingest.SpoolDir); err != nil || len(entries) != 0 {
		t.Errorf("spool after rejection = %v, %v", entries, err)
	}
}
