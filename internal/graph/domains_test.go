package graph_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func domainAssignmentFixture(t *testing.T) (*graph.Store, graph.Assignment) {
	t.Helper()
	st := graphtest.Store(t)
	ctx := context.Background()
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: t.Name(), Hash: "quillon"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutChunks(ctx, res.ID, []graph.Chunk{{Text: "Quillon visits Acme-Max.", DuplicateOf: "missing"}}); err != nil {
		t.Fatal(err)
	}
	slug := domain.NewDomainID(res.ID)
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, res.ID); err != nil {
			t.Error(err)
		}
		rawRun(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (n:Domain {id:'%s'}) DETACH DELETE n`, slug))
		rawRun(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (r:DomainDecision {doc_id:'%s'}) DETACH DELETE r`, res.ID))
	})
	return st, graph.Assignment{DocID: res.ID, Domain: slug, DomainName: "Quillon", CreatedBy: "auto", NewDomain: true, Profile: domain.Profile{"quillon": 2, "acmemax": 1}, Decision: graph.DecisionRecord{OpID: t.Name(), DocID: res.ID, Domain: slug, Action: "new", By: "auto", Params: domain.DefaultParams(), Top: []domain.DomainScore{{Domain: "a", Score: .01}}, At: time.Now().UTC()}}
}

func TestAssignAtomic(t *testing.T) {
	st, a := domainAssignmentFixture(t)
	synthetic := errors.New("synthetic failure inside assignment transaction")
	if err := graph.AssignDomainFailForTest(context.Background(), st, a, synthetic); !errors.Is(err, synthetic) {
		t.Fatalf("error=%v", err)
	}
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (d:Document {id:'%s'})-[:HAS_CHUNK]->(c:Chunk) RETURN d.domain AS domain,d.name_profile AS profile,c.domain AS chunk_domain`, a.DocID))
	for _, key := range []string{"domain", "profile", "chunk_domain"} {
		if v, _ := rows[0].Get(key); v != nil && v != "" {
			t.Errorf("partial write %s=%#v", key, v)
		}
	}
	for _, q := range []string{fmt.Sprintf(`MATCH (n:Domain {id:'%s'}) RETURN count(n) AS n`, a.Domain), fmt.Sprintf(`MATCH (n:DomainDecision {doc_id:'%s'}) RETURN count(n) AS n`, a.DocID)} {
		if n := rawCount(t, os.Getenv(graphtest.URIEnv), q); n != 0 {
			t.Errorf("partial domain/ledger: %d", n)
		}
	}
}

func TestDomainProfilesAndAssignment(t *testing.T) {
	st, a := domainAssignmentFixture(t)
	a.Decision.Top = []domain.DomainScore{{Domain: "a", Score: .04}, {Domain: "b", Score: .03}, {Domain: "c", Score: .02}, {Domain: "d", Score: .01}}
	ctx := context.Background()
	if err := st.AssignDomain(ctx, a); err != nil {
		t.Fatal(err)
	}
	profiles, err := st.DomainProfiles(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range profiles {
		if p.DocID == a.DocID {
			found = true
			if !reflect.DeepEqual(p.Profile, a.Profile) || p.Domain != a.Domain || p.Source != t.Name() {
				t.Errorf("profile=%+v", p)
			}
		}
	}
	if !found {
		t.Fatal("assigned profile missing")
	}
	ledger := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (r:DomainDecision {doc_id:'%s'}) RETURN r.top AS top`, a.DocID))
	top, _ := ledger[0].Get("top")
	if !reflect.DeepEqual(top, []any{[]any{"a", .04}, []any{"b", .03}, []any{"c", .02}}) {
		t.Errorf("ledger top=%#v", top)
	}
	profiles, err = st.DomainProfiles(ctx, a.DocID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		if p.DocID == a.DocID {
			t.Fatal("target included")
		}
	}
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (d:Document {id:'%s'})-[:HAS_CHUNK]->(c:Chunk) MATCH (n:Domain {id:d.domain}) RETURN d.domain_tok AS dt,c.domain AS cd,c.domain_tok AS ct,n.tok AS nt,n.name AS name,n.created_by AS by`, a.DocID))
	if len(rows) != 1 {
		t.Fatalf("assignment rows=%d", len(rows))
	}
	for _, key := range []string{"dt", "ct", "nt"} {
		if got, _ := rows[0].Get(key); got != domain.DomainToken(a.Domain) {
			t.Errorf("%s=%v", key, got)
		}
	}
	if got, _ := rows[0].Get("cd"); got != a.Domain {
		t.Errorf("chunk domain=%v", got)
	}
	if err := st.UpdateNameProfile(ctx, a.DocID, domain.Profile{}); err != nil {
		t.Fatal(err)
	}
	profiles, err = st.DomainProfiles(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		if p.DocID == a.DocID && len(p.Profile) != 0 {
			t.Errorf("profile not replaced: %v", p.Profile)
		}
	}
	if err := st.SetStatus(ctx, a.DocID, graph.StatusSuperseded, ""); err != nil {
		t.Fatal(err)
	}
	profiles, err = st.DomainProfiles(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		if p.DocID == a.DocID {
			t.Fatal("superseded profile included")
		}
	}
	if err := st.UpdateNameProfile(ctx, "missing-doc", domain.Profile{}); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("missing update=%v", err)
	}
}

func TestAssignDomainTokenCollision(t *testing.T) {
	st, a := domainAssignmentFixture(t)
	uri := os.Getenv(graphtest.URIEnv)
	const spoof = "quillon-collision"
	rawRun(t, uri, fmt.Sprintf(`CREATE (:Domain {id:'%s',tok:'%s'})`, spoof, domain.DomainToken(a.Domain)))
	t.Cleanup(func() { rawRun(t, uri, `MATCH (n:Domain {id:'quillon-collision'}) DELETE n`) })
	if err := st.AssignDomain(context.Background(), a); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("collision error=%v", err)
	}
	d, err := st.Document(context.Background(), a.DocID)
	if err != nil || d.Domain != "" {
		t.Errorf("collision wrote domain: %+v err=%v", d, err)
	}
	a.Domain = "Bad:Slug"
	if err := st.AssignDomain(context.Background(), a); err == nil {
		t.Fatal("invalid slug accepted")
	}
}

func TestDomainProfilesSynthetic600(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	t.Cleanup(func() { rawRun(t, uri, `MATCH (d:Document {source_kind:'domain-bench'}) DETACH DELETE d`) })
	rawRun(t, uri, `UNWIND range(0,599) AS i CREATE (:Document {id:'domain-bench-'+toString(i),source_kind:'domain-bench',source:'quillon-'+toString(i),status:'done',domain:'quillon-'+toString(i%6),name_profile:{acmemax:1,quillon:2,zorbo:1,wexora:1,flimble:1,trindle:1}})`)
	start := time.Now()
	profiles, err := st.DomainProfiles(context.Background(), "")
	read := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range profiles {
		if strings.HasPrefix(p.DocID, "domain-bench-") {
			n++
		}
	}
	if n != 600 {
		t.Fatalf("synthetic profiles=%d", n)
	}
	start = time.Now()
	decision := domain.Decide(domain.DocProfile{DocID: "target", Source: "target", Profile: domain.Profile{"acmemax": 1, "quillon": 2, "zorbo": 1}}, profiles, domain.DefaultParams())
	t.Logf("synthetic_documents=600 corpus_profiles=%d DomainProfiles=%s detection=%s action=%s", len(profiles), read, time.Since(start), decision.Action)
}

func TestFactAuditIDSanitizesRelation(t *testing.T) {
	if graph.FactAuditID("acme", "Works At!", "zorbo", "quillon") != graph.FactAuditID("acme", "works_at_", "zorbo", "quillon") {
		t.Fatal("raw ingest and sanitized backfill relation produce different audit IDs")
	}
}

func TestMoveDocumentGuardsAndLedger(t *testing.T) {
	st, a := domainAssignmentFixture(t)
	ctx := t.Context()
	uri := os.Getenv(graphtest.URIEnv)
	if err := st.AssignDomain(ctx, a); err != nil {
		t.Fatal(err)
	}
	rawRun(t, uri, `MERGE (m:GkgMeta {id:'extraction_schema'}) SET m.value=3`)
	if err := st.MoveDocument(ctx, a.DocID, "zorbo-admin", "move-acme", "move", "admin"); !errors.Is(err, graph.ErrSchemaNotV4) {
		t.Fatalf("v3 move=%v", err)
	}
	rawRun(t, uri, `MATCH (m:GkgMeta {id:'extraction_schema'}) SET m.value=4`)
	t.Cleanup(func() {
		rawRun(t, uri, fmt.Sprintf(`MATCH (m:GkgMeta {id:'extraction_schema'}) SET m.value=%d`, graph.ExtractionSchemaVersion))
		rawRun(t, uri, `MATCH (n:Domain {id:'zorbo-admin'}) DELETE n`)
	})
	if err := st.MoveDocument(ctx, a.DocID, "zorbo-admin", "move-acme", "move", "admin"); !errors.Is(err, graph.ErrNotDone) {
		t.Fatalf("unfinished move=%v", err)
	}
	d, _ := st.Document(ctx, a.DocID)
	if d.Domain != a.Domain {
		t.Fatal("unfinished move changed domain")
	}
	if err := st.SetStatus(ctx, a.DocID, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.MoveDocument(ctx, a.DocID, "zorbo-admin", "move-acme", "move", "admin"); err != nil {
		t.Fatal(err)
	}
	records, err := st.DomainDecisions(ctx, "move-acme")
	if err != nil || len(records) != 1 || records[0].PreviousDomain != a.Domain {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	// A committed document is skipped even while re-extraction is pending.
	if err := st.MoveDocument(ctx, a.DocID, "zorbo-admin", "move-acme", "move", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RenameDomain(ctx, "zorbo-admin", "Quillon Ltd"); err != nil {
		t.Fatal(err)
	}
	domains, err := st.ListDomains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range domains {
		if d.ID == "zorbo-admin" {
			found = true
			if d.Name != "Quillon Ltd" || d.Documents != 0 {
				t.Fatalf("domain=%+v", d)
			}
		}
	}
	if !found {
		t.Fatal("new domain missing")
	}
}

func TestRefutationAuditUsesOneSanitizedID(t *testing.T) {
	st, a := domainAssignmentFixture(t)
	audit := graph.FactAudit{HeadKey: "acme", Rel: "Works At!", TailKey: "zorbo", DocID: a.DocID, ChunkID: a.DocID + "-c0", JudgeVerdict: "refuted"}
	if err := st.WriteFactAudit(t.Context(), audit); err != nil {
		t.Fatal(err)
	}
	audit.Rel = graph.SanitizeRel(audit.Rel)
	if err := st.WriteFactAudit(t.Context(), audit); err != nil {
		t.Fatal(err)
	}
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (r:RejectedFact {doc_id:'%s'}) RETURN r.rel AS rel`, a.DocID))
	if len(rows) != 1 {
		t.Fatalf("same fact produced %d audit rows", len(rows))
	}
	if rel, _ := rows[0].Get("rel"); rel != audit.Rel {
		t.Fatalf("audit relation=%v", rel)
	}
}
