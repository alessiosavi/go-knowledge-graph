package graph_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

// These tests own the disposable database; no parallel tests or other DB commands.
func v4Fixture(t *testing.T) *graph.Store {
	t.Helper()
	st := graphtest.MigrationStore(t)
	uri := os.Getenv(graphtest.URIEnv)
	wipeGraph(t, uri)
	t.Cleanup(func() { wipeGraph(t, uri) })
	rawRun(t, uri, `CREATE (:GkgMeta {id:'extraction_schema',value:3})`)
	for i := 0; i < 5; i++ {
		rawRun(t, uri, fmt.Sprintf(`CREATE (d:Document {id:'acme-%d',source:'https://acme.invalid/%d',status:'done'}),
   (c:Chunk {id:'acme-%d-c0',doc_id:d.id,text:'Acme-Max and Zorbo'}),
   (c2:Chunk {id:'acme-%d-c1',doc_id:d.id,text:'Acme Max'}),
   (e:Entity {key:'acme-%d',name:'Acme-Max'}), (e2:Entity {key:'zorbo-%d',name:'Zórbo'}),
   (alias:Entity {key:'alias-%d',name:'Acme Max'}),
   (d)-[:HAS_CHUNK]->(c),(d)-[:HAS_CHUNK]->(c2),
   (c)-[:MENTIONS]->(e),(c)-[:MENTIONS]->(alias),(c)-[:MENTIONS]->(e2),(c2)-[:MENTIONS]->(e),
   (e)-[:uses {doc_id:d.id}]->(e2),(:FactCandidate {id:d.id,doc_id:d.id}),(:RejectedFact {doc_id:d.id})`, i, i, i, i, i, i, i))
	}
	rawRun(t, uri, `MATCH (c:Chunk {id:'acme-1-c0'}) SET c.duplicate_of='acme-0-c0'`)
	rawRun(t, uri, `CREATE (:Chunk {id:'orphan-duplicate',duplicate_of:'acme-0-c0',text:'Zorbo'})`)
	rawRun(t, uri, `CREATE (:JudgeVerdict {key:'acme-cache',status:'confirmed'}),(:Document {id:'old',source:'unmatched-old',status:'superseded'})`)
	return st
}

func v4Seed(t *testing.T) *domain.Seed {
	t.Helper()
	f, err := os.Open("../../testdata/domain-seed.synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seed, err := domain.LoadSeed(f)
	if err != nil {
		t.Fatal(err)
	}
	return &seed
}

func v4Snapshot(t *testing.T) any {
	uri := os.Getenv(graphtest.URIEnv)
	return []any{
		rawRecords(t, uri, `MATCH (n) RETURN labels(n),properties(n) ORDER BY id(n)`),
		rawRecords(t, uri, `MATCH (a)-[r]->(b) RETURN id(a),type(r),properties(r),id(b) ORDER BY id(r)`),
	}
}

func TestMigrationV4SeedValidation(t *testing.T) {
	for _, which := range []string{"missing", "unmatched"} {
		t.Run(which, func(t *testing.T) {
			st := v4Fixture(t)
			before := v4Snapshot(t)
			opt := graph.MigrationOptions{}
			if which == "unmatched" {
				opt.Seed = &domain.Seed{Schema: 1}
				for i := 0; i < 4; i++ {
					opt.Seed.Rules = append(opt.Seed.Rules, domain.SeedRule{Match: fmt.Sprintf("https://acme.invalid/%d", i), Domain: "acme"})
				}
			}
			var err error
			queries := graph.CaptureReadQueries(st, func() {
				_, err = st.MigrateExtractionSchemaOpts(context.Background(), opt)
			})
			if !errors.Is(err, graph.ErrSeedRequired) {
				t.Fatalf("error=%v; want ErrSeedRequired", err)
			}
			if which == "unmatched" && (!strings.Contains(err.Error(), "1 unmatched") || !strings.Contains(err.Error(), "https://acme.invalid/4")) {
				t.Errorf("missing unmatched sources: %v", err)
			}
			if which == "missing" {
				for _, q := range queries {
					if strings.Contains(q.Query, "MENTIONS") || strings.Contains(q.Query, "name_profile") {
						t.Errorf("seedless migration read profiles before rejecting the missing seed: %s", q.Query)
					}
				}
			}
			if !reflect.DeepEqual(before, v4Snapshot(t)) {
				t.Error("validation changed the graph")
			}
		})
	}
}

func TestProfilesFromMentions(t *testing.T) {
	st := v4Fixture(t)
	uri := os.Getenv(graphtest.URIEnv)
	rawRun(t, uri, `MATCH (d:Document {id:'acme-1'}) SET d.name_profile={stored:7}`)
	rawRun(t, uri, `MATCH (c:Chunk {id:'acme-0-c0'})
CREATE (e:Entity {key:'#acme-0:2042-03-04',name:'2042-03-04'}),(c)-[:MENTIONS]->(e)`)
	before := v4Snapshot(t)
	var profiles []domain.DocProfile
	queries := graph.CaptureReadQueries(st, func() {
		var err error
		profiles, err = st.ProfilesFromMentions(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	})
	mentionReads := 0
	seen := make(map[string]bool)
	for _, q := range queries {
		if !strings.Contains(q.Query, "MENTIONS") {
			continue
		}
		mentionReads++
		id, _ := q.Params["id"].(string)
		if id == "" || id == "acme-1" || id == "old" || seen[id] {
			t.Errorf("mention read must be scoped to one missing live profile: query=%s params=%v", q.Query, q.Params)
		}
		seen[id] = true
	}
	if mentionReads != 4 {
		t.Errorf("mention reads=%d, want 4 document batches", mentionReads)
	}
	if len(profiles) != 5 {
		t.Fatalf("profiles=%+v", profiles)
	}
	for i, p := range profiles {
		want := domain.Profile{"acmemax": 2, "zorbo": 1}
		if i == 1 {
			want = domain.Profile{"stored": 7}
		}
		if p.DocID != fmt.Sprintf("acme-%d", i) || !reflect.DeepEqual(p.Profile, want) {
			t.Errorf("profile=%+v want=%+v", p, want)
		}
	}
	if !reflect.DeepEqual(before, v4Snapshot(t)) {
		t.Error("profile read wrote graph")
	}
}

func TestMigrationInterruptedSeedChanged(t *testing.T) {
	st := v4Fixture(t)
	opt := graph.MigrationOptions{Seed: v4Seed(t)}
	err := graph.InterruptMigrationForTest(st, "assign", func() error {
		_, err := st.MigrateExtractionSchemaOpts(context.Background(), opt)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "synthetic migration interruption") {
		t.Fatalf("interruption=%v", err)
	}
	// An unassigned document sorts before the conflicting recorded decision.
	// The restart must validate every decision before assigning anything else.
	uri := os.Getenv(graphtest.URIEnv)
	rawRun(t, uri, `CREATE (:Document {id:'acme-00',source:'https://acme.invalid/new',status:'done'})`)
	opt.Seed.Rules = append(opt.Seed.Rules, domain.SeedRule{Match: "https://acme.invalid/2", Domain: "zorbo"})
	before := v4Snapshot(t)
	_, err = st.MigrateExtractionSchemaOpts(context.Background(), opt)
	if err == nil || !strings.Contains(err.Error(), "seed changed after the interrupted assign phase") {
		t.Errorf("restart error=%v; want seed-changed diagnostic", err)
	}
	if !reflect.DeepEqual(before, v4Snapshot(t)) {
		t.Error("changed-seed restart wrote to the graph")
	}
}

func assertV4(t *testing.T, st *graph.Store) any {
	t.Helper()
	uri := os.Getenv(graphtest.URIEnv)
	if graph.ExtractionSchemaVersion != 4 {
		t.Fatalf("schema=%d", graph.ExtractionSchemaVersion)
	}
	if v, err := st.SchemaVersion(context.Background()); err != nil || v != 4 {
		t.Fatalf("marker=%d err=%v", v, err)
	}
	for _, label := range []string{"Entity", "FactCandidate", "RejectedFact"} {
		if n := rawCount(t, uri, `MATCH (n:`+label+`) RETURN count(*) AS n`); n != 0 {
			t.Errorf("%s count=%d", label, n)
		}
	}
	if n := rawCount(t, uri, `MATCH ()-[m:MENTIONS]->() RETURN count(*) AS n`); n != 0 {
		t.Errorf("mentions=%d", n)
	}
	if n := rawCount(t, uri, `MATCH (c:Chunk) RETURN count(*) AS n`); n != 11 {
		t.Errorf("retained chunks=%d, want 11", n)
	}
	if n := rawCount(t, uri, `MATCH (c:Chunk) WHERE coalesce(c.duplicate_of,'')<>'' RETURN count(*) AS n`); n != 0 {
		t.Errorf("stale chunk duplicate links=%d, want 0", n)
	}
	for _, id := range []string{"acme-1-c0", "orphan-duplicate"} {
		rows := rawRecords(t, uri, fmt.Sprintf(`MATCH (c:Chunk {id:'%s'}) RETURN c.duplicate_of AS duplicate`, id))
		if len(rows) != 1 {
			t.Fatalf("chunk %s rows=%d, want 1", id, len(rows))
		}
		if duplicate, _ := rows[0].Get("duplicate"); duplicate != "" {
			t.Errorf("chunk %s duplicate_of=%v, want empty string", id, duplicate)
		}
	}
	if n := rawCount(t, uri, `MATCH (v:JudgeVerdict {key:'acme-cache'}) RETURN count(*) AS n`); n != 1 {
		t.Errorf("verdicts=%d", n)
	}
	if n := rawCount(t, uri, `MATCH (m:GkgMeta) WHERE m.v4_phase IS NOT NULL RETURN count(*) AS n`); n != 0 {
		t.Error("phase remains")
	}
	if n := rawCount(t, uri, `MATCH (d:Document) WHERE d.status='chunked' AND d.domain IS NOT NULL AND d.name_profile IS NOT NULL RETURN count(*) AS n`); n != 5 {
		t.Errorf("assigned/requeued=%d", n)
	}
	if n := rawCount(t, uri, `MATCH (d:Document)-[:HAS_CHUNK]->(c:Chunk) WHERE c.domain=d.domain AND c.domain_tok=d.domain_tok RETURN count(*) AS n`); n != 10 {
		t.Errorf("chunk domains=%d", n)
	}
	if n := rawCount(t, uri, `MATCH (d:DomainDecision {op_id:'migration-v4',by:'admin'}) RETURN count(*) AS n`); n != 5 {
		t.Errorf("ledger entries=%d, want exactly 5", n)
	}
	return []any{
		rawRecords(t, uri, `MATCH (d:Document) RETURN d.id,d.domain,d.name_profile,d.status ORDER BY d.id`),
		rawRecords(t, uri, `MATCH (d:DomainDecision) RETURN d.doc_id,d.domain,d.action,d.op_id,d.by,d.top,d.params ORDER BY d.doc_id`),
		rawRecords(t, uri, `MATCH (d:Domain) RETURN d.id,d.name,d.created_by ORDER BY d.id`),
	}
}

func TestMigrationV4Seeded(t *testing.T) {
	st := v4Fixture(t)
	result, err := st.MigrateExtractionSchemaOpts(context.Background(), graph.MigrationOptions{Seed: v4Seed(t)})
	if err != nil {
		t.Fatal(err)
	}
	if result.From != 3 || result.To != 4 || result.DocumentsRequeued != 5 || result.EntitiesRemoved != 15 || !result.Migrated {
		t.Errorf("result=%+v", result)
	}
	assertV4(t, st)
	profiles, err := st.DomainProfiles(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		if p.Domain != "acme" || !reflect.DeepEqual(p.Profile, domain.Profile{"acmemax": 2, "zorbo": 1}) {
			t.Errorf("profile=%+v", p)
		}
	}
	before := v4Snapshot(t)
	if _, err := st.MigrateExtractionSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, v4Snapshot(t)) {
		t.Error("repeat migration wrote graph")
	}
	t.Logf("seeded migration: %d documents, %d entities removed, 11 chunks retained (including one orphan)", result.DocumentsRequeued, result.EntitiesRemoved)
}

func TestMigrationInterruptedRestart(t *testing.T) {
	for _, detect := range []bool{false, true} {
		t.Run(fmt.Sprintf("detect=%t", detect), func(t *testing.T) {
			var want any
			for _, mode := range []string{"uninterrupted", "assign", "clear"} {
				t.Run(mode, func(t *testing.T) {
					st := v4Fixture(t)
					opt := graph.MigrationOptions{DetectAll: detect}
					if !detect {
						opt.Seed = v4Seed(t)
					}
					if mode != "uninterrupted" {
						err := graph.InterruptMigrationForTest(st, mode, func() error { _, err := st.MigrateExtractionSchemaOpts(context.Background(), opt); return err })
						if err == nil || !strings.Contains(err.Error(), "synthetic migration interruption") {
							t.Fatalf("interruption=%v", err)
						}
						uri := os.Getenv(graphtest.URIEnv)
						if mode == "assign" {
							if n := rawCount(t, uri, `MATCH (d:DomainDecision {op_id:'migration-v4'}) RETURN count(*) AS n`); n != 3 {
								t.Fatalf("interrupted after %d assignments, want 3", n)
							}
						} else {
							if n := rawCount(t, uri, `MATCH (m:GkgMeta {v4_phase:'assigned'}) RETURN count(*) AS n`); n != 1 {
								t.Fatal("missing assigned phase")
							}
							if n := rawCount(t, uri, `MATCH ()-[m:MENTIONS]->() RETURN count(*) AS n`); n != 0 {
								t.Fatal("clear batch did not commit")
							}
						}
					}
					if _, err := st.MigrateExtractionSchemaOpts(context.Background(), opt); err != nil {
						t.Fatal(err)
					}
					got := assertV4(t, st)
					if mode == "uninterrupted" {
						want = got
					} else if !reflect.DeepEqual(want, got) {
						t.Errorf("restart differs from uninterrupted: want=%+v got=%+v", want, got)
					}
				})
			}
		})
	}
}

func TestMigrationV4Empty(t *testing.T) {
	st := v4Fixture(t)
	uri := os.Getenv(graphtest.URIEnv)
	wipeGraph(t, uri)
	r, err := st.MigrateExtractionSchema(context.Background())
	if err != nil || !r.Migrated || r.From != 1 || r.To != 4 {
		t.Fatalf("empty migration=%+v err=%v", r, err)
	}
}

func TestBootstrapDocumentOnlyNeedsMigration(t *testing.T) {
	st := v4Fixture(t)
	uri := os.Getenv(graphtest.URIEnv)
	wipeGraph(t, uri)
	rawRun(t, uri, `CREATE (:Document {id:'quillon',source:'https://quillon.invalid/',status:'done'})`)
	if err := st.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, err := st.SchemaVersion(context.Background()); err != nil || v != 1 {
		t.Fatalf("bootstrap bypassed migration: version=%d err=%v", v, err)
	}
	_, err := st.MigrateExtractionSchema(context.Background())
	if !errors.Is(err, graph.ErrSeedRequired) {
		t.Fatalf("migration=%v", err)
	}
}

// wipeGraph empties the shared test graph. Embeddings are nulled
// first: Memgraph's vector index otherwise keeps deleted nodes and later
// vector searches fail with "Trying to get a property from a deleted object"
// (the product's delete paths do the same).
func wipeGraph(t *testing.T, uri string) {
	t.Helper()
	rawRun(t, uri, `MATCH (n) WHERE n.embedding IS NOT NULL SET n.embedding = null`)
	rawRun(t, uri, `MATCH (n) DETACH DELETE n`)
}
