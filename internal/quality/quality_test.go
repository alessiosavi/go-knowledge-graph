package quality

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/dedup"
	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/hygiene"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func fixture() dataset {
	return dataset{
		version:   4,
		documents: []document{{id: "d1", source: "text/acme/one", domain: "a"}, {id: "d2", source: "text/zorbo/two", domain: "b"}},
		chunks:    []chunk{{"c1", "d1"}, {"c2", "d1"}, {"c3", "d1"}, {"c4", "d2"}, {"c5", "d2"}},
		entities:  []entity{{"a", "Acme-Max", "organization"}, {"b", "Acme Max", "product"}, {"c", "Zorbo", "location"}, {"d", "Quillon Ltd", "organization"}, {"e", "777", "number"}, {"f", "Quillon", "organization"}},
		mentions:  []mention{{"a", "c1"}, {"b", "c1"}, {"c", "c1"}, {"a", "c2"}, {"b", "c2"}, {"a", "c3"}, {"d", "c3"}, {"c", "c4"}, {"d", "c4"}, {"e", "c5"}, {"f", "c5"}, {"a", "c1"}}, // 12 spans, 11 distinct pairs.
		facts:     []fact{{"a", "b", "r", "c1", "d1", .3, true}, {"b", "a", "s", "c2", "d1", .8, false}, {"b", "a", "r", "c3", "d1", .2, false}, {"c", "c", "r", "c4", "d2", .9, true}},
		unjudged:  1, unclear: 2, rejected: 3,
	}
}

func policy(t *testing.T) map[string]hygiene.Preset {
	t.Helper()
	p, err := hygiene.LoadPreset("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]hygiene.Preset{"": p}
}

func number(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-10 {
		t.Errorf("%s = %v, want %.12g", name, got, want)
	}
}

func TestMetricsOnHandGraph(t *testing.T) {
	r := compute(fixture(), Options{}, policy(t))
	// chunks/entity = [3,2,2,2,1,1], 11 distinct mentions; 2 singletons,
	// 4 single-document entities. Sorted weighted sum = 45, so Gini = 13/66.
	if r.Metrics != 3 || r.Documents != 2 || r.Chunks != 5 || r.Entities != 6 || r.Mentions != 11 {
		t.Fatalf("population: %+v", r)
	}
	number(t, "isolated", r.IsolatedShare, .5)
	number(t, "singleton", r.SingletonShare, 2.0/6)
	number(t, "single document", r.SingleDocumentShare, 4.0/6)
	number(t, "gini", r.MentionGini, 13.0/66)
	number(t, "top one percent", r.Top1PctMentionShare, 3.0/11)
	if r.CrossDomainEntities == nil || *r.CrossDomainEntities != 2 {
		t.Errorf("cross domains: %v", r.CrossDomainEntities)
	}
	// Fact nodes {a,b,c}: components {a,b}, {c}; all other entities isolated.
	if r.FactComponents != 2 || r.Facts.Typed != 4 || r.Facts.CandidatesUnjudged == nil || *r.Facts.CandidatesUnjudged != 1 || r.Facts.CandidatesUnclear == nil || *r.Facts.CandidatesUnclear != 2 || r.Facts.Rejected != 3 {
		t.Errorf("facts: %+v", r)
	}
	number(t, "giant/fact nodes", r.GiantShareOfFactNodes, 2.0/3)
	number(t, "giant/entities", r.GiantShareOfEntities, 2.0/6)
	// low_confidence flag and confidence < .4 are independent (two each).
	number(t, "low flag", r.LowConfidenceShare, .5)
	number(t, "confidence < .4", r.ConfidenceLT04Share, .5)
	if r.Support.Triples != 4 {
		t.Errorf("support: %+v", r.Support)
	}
	number(t, "one chunk", r.Support.OneChunkShare, 1)
	number(t, "multi document", r.Support.MultiDocumentShare, 0)
	// Unordered {a,b} has r and s across both directions; r has one reverse
	// pair (a < b), and (c,r,c) is one distinct self-loop triple.
	if r.RelationTypes != 2 || r.MultiTypePairs != 1 || r.InversePairs != 1 || r.SelfLoops != 1 {
		t.Errorf("relations: %+v", r)
	}
	number(t, "entropy", r.RelationEntropyBits, -.75*math.Log2(.75)-.25*math.Log2(.25))
	number(t, "max entropy", r.MaxEntropyBits, 1)
	if r.DuplicateGroups != 2 || r.DuplicateEntities != 4 || r.CrossTypeGroups != 1 {
		t.Errorf("duplicates: %+v", r)
	}
	number(t, "noise", r.NoiseRuleShare, 1.0/6)
	// Co-mentions: ab(2), ac, bc, ad, cd, ef; giant {a,b,c,d}.
	if r.CoMention.Nodes != 6 || r.CoMention.Edges != 6 {
		t.Errorf("co-mention: %+v", r.CoMention)
	}
	number(t, "weight1", r.CoMention.Weight1Share, 5.0/6)
	number(t, "co giant", r.CoMention.GiantShare, 4.0/6)
}

func TestMetricBoundaries(t *testing.T) {
	number(t, "equal gini", gini([]int{1, 1, 1, 1}), 0)
	number(t, "concentrated gini", gini([]int{0, 0, 0, 4}), .75)
	number(t, "equal entropy", entropy([]int{2, 2}), 1)
	if gini(nil) != nil || gini([]int{0, 0}) != nil || entropy(nil) != nil {
		t.Fatal("empty metrics must be undefined")
	}
	r := compute(dataset{version: 3}, Options{}, policy(t))
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"isolated_share", "singleton_share", "single_document_share", "mention_gini", "top1pct_mention_share", "cross_domain_entities", "domain_coverage", "giant_share_of_fact_nodes", "giant_share_of_entities", "low_confidence_share", "confidence_lt_0_4_share", "relation_entropy_bits", "max_entropy_bits", "noise_rule_share"} {
		v, ok := m[key]
		if !ok || v != nil {
			t.Errorf("%s = %v (present %v), want null", key, v, ok)
		}
	}
	if r.Facts.CandidatesUnjudged != nil || r.Facts.CandidatesUnclear != nil {
		t.Fatal("pre-v4 candidates must be null")
	}
}

func TestSupportAndMentionDedup(t *testing.T) {
	d := fixture()
	d.facts = append(d.facts, fact{"a", "b", "r", "c4", "d2", .4, false})
	r := compute(d, Options{}, policy(t))
	// Five stored edges, four distinct triples; exactly one spans two documents.
	if r.Facts.Typed != 5 || r.Support.Triples != 4 {
		t.Fatalf("support: %+v", r)
	}
	number(t, "one chunk", r.Support.OneChunkShare, .75)
	number(t, "multi document", r.Support.MultiDocumentShare, .25)
	number(t, "strict confidence bound", r.ConfidenceLT04Share, 2.0/5)
}

func TestDomainAndDuplicateScopes(t *testing.T) {
	d := fixture()
	d.documents[0].domain = ""
	d.documents[1].domain = ""
	r := compute(d, Options{}, policy(t))
	if r.CrossDomainEntities != nil {
		t.Fatal("missing domains must be null")
	}
	seed := domain.Seed{Schema: 1, Rules: []domain.SeedRule{{Match: "text/", Domain: "other"}, {Match: "text/acme/", Domain: "a"}, {Match: "text/zorbo/", Domain: "b"}}}
	r = compute(d, Options{Seed: &seed, Domain: "b"}, policy(t))
	if r.Documents != 1 || r.Chunks != 2 || r.Entities != 4 || r.Mentions != 4 || r.Facts.Typed != 1 {
		t.Fatalf("full-source seed/domain scope: %+v", r)
	}
	if r.CrossDomainEntities != nil {
		t.Fatalf("scoped cross domains: %+v", r)
	}
	// Document and domain boundaries must survive duplicate folding.
	d = dataset{version: 4, documents: []document{{id: "d1", source: "one", domain: "a"}, {id: "d2", source: "two", domain: "b"}}, chunks: []chunk{{"c1", "d1"}, {"c2", "d2"}}, entities: []entity{{"a", "Àcme-Max Ltd.", "organization"}, {"b", "ＡＣＭＥ Max", "product"}, {"#d1:x", "Zorbo", "value"}, {"#d2:y", "Zorbo", "value"}}, mentions: []mention{{"a", "c1"}, {"b", "c2"}, {"#d1:x", "c1"}, {"#d2:y", "c2"}}}
	r = compute(d, Options{}, policy(t))
	if r.DuplicateGroups != 0 {
		t.Fatal("cross-domain or cross-document duplicate", r.DuplicateGroups)
	}
	d.documents[1].domain = "a"
	r = compute(d, Options{}, policy(t))
	if r.DuplicateGroups != 1 || r.DuplicateEntities != 2 || r.CrossTypeGroups != 1 {
		t.Fatalf("fold/scopes: %+v", r)
	}
}

func TestDuplicateGroupsCompanySuffix(t *testing.T) {
	for _, tt := range []struct {
		name, first, second string
		wantGroups          int
	}{
		{"resort spa", "Zorbo Resort & Spa", "Zorbo Resort", 0},
		{"compound suffix", "Quillon Packaging Co., Ltd", "Quillon Packaging", 1},
		{"Italian suffix", "Quillon SpA", "Quillon", 1},
		{"comma before suffix", "Quillon Packaging,Inc", "Quillon Packaging", 1},
		{"lowercase dotted SpA", "Acme s.p.a.", "Acme", 1},
		{"no-break space before suffix", "Acme\u00a0Ltd", "Acme", 1},
		{"full-width suffix", "ＡＣＭＥ ＬＴＤ", "Acme", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := dataset{
				version:   4,
				documents: []document{{id: "d1", source: "one", domain: "a"}},
				chunks:    []chunk{{"c1", "d1"}},
				entities:  []entity{{"a", tt.first, "organization"}, {"b", tt.second, "organization"}},
				mentions:  []mention{{"a", "c1"}, {"b", "c1"}},
			}
			r := compute(d, Options{}, policy(t))
			if r.DuplicateGroups != tt.wantGroups || r.DuplicateEntities != 2*tt.wantGroups {
				t.Errorf("duplicate groups/entities = %d/%d, want %d/%d", r.DuplicateGroups, r.DuplicateEntities, tt.wantGroups, 2*tt.wantGroups)
			}
		})
	}
}

func TestCrossDomainCoverage(t *testing.T) {
	partial := fixture()
	partial.documents = append(partial.documents, document{id: "d3", source: "text/quillon/three"})
	partial.chunks = append(partial.chunks, chunk{"c6", "d3"})
	partial.mentions = append(partial.mentions, mention{"a", "c6"})
	unresolved := fixture()
	for i := range unresolved.documents {
		unresolved.documents[i].domain = ""
	}
	seed := domain.Seed{Schema: 1, Rules: []domain.SeedRule{{Match: "text/acme/", Domain: "a"}}}
	for _, tc := range []struct {
		name         string
		data         dataset
		opt          Options
		wantCross    int // -1 means undefined.
		wantCoverage float64
	}{
		{"complete", fixture(), Options{}, 2, 1},
		{"partial stored domains", partial, Options{}, 2, 2.0 / 3},
		{"partial seed", unresolved, Options{Seed: &seed}, 0, .5},
		{"no domains or seed", unresolved, Options{}, -1, 0},
		{"seed without matches", unresolved, Options{Seed: &domain.Seed{Schema: 1}}, -1, 0},
		{"scoped", fixture(), Options{Domain: "a"}, -1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := compute(tc.data, tc.opt, policy(t))
			if tc.wantCross < 0 {
				if r.CrossDomainEntities != nil {
					t.Errorf("cross_domain_entities = %d, want null", *r.CrossDomainEntities)
				}
			} else if r.CrossDomainEntities == nil || *r.CrossDomainEntities != tc.wantCross {
				t.Errorf("cross_domain_entities = %v, want %d", r.CrossDomainEntities, tc.wantCross)
			}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var values struct {
				Coverage *float64 `json:"domain_coverage"`
			}
			if err := json.Unmarshal(data, &values); err != nil {
				t.Fatal(err)
			}
			number(t, "domain coverage", values.Coverage, tc.wantCoverage)
		})
	}
}

func TestPairMetricsDistinctTriples(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		facts                []fact
		multi, inverse, self int
	}{
		{"types across directions", []fact{{head: "a", tail: "b", rel: "r"}, {head: "b", tail: "a", rel: "s"}}, 1, 0, 0},
		{"two inverse types", []fact{{head: "a", tail: "b", rel: "r"}, {head: "b", tail: "a", rel: "r"}, {head: "a", tail: "b", rel: "s"}, {head: "b", tail: "a", rel: "s"}, {head: "a", tail: "b", rel: "r", chunk: "c2"}}, 1, 2, 0},
		{"repeated self loop", []fact{{head: "a", tail: "a", rel: "r", chunk: "c1"}, {head: "a", tail: "a", rel: "r", chunk: "c2"}}, 0, 0, 1},
		{"two self loop types", []fact{{head: "a", tail: "a", rel: "r"}, {head: "a", tail: "a", rel: "s"}, {head: "a", tail: "a", rel: "s", chunk: "c2"}}, 1, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r Report
			factMetrics(&r, tc.facts)
			if r.MultiTypePairs != tc.multi || r.InversePairs != tc.inverse || r.SelfLoops != tc.self {
				t.Errorf("multi_type_pairs=%d inverse_pairs=%d self_loops=%d; want %d, %d, %d", r.MultiTypePairs, r.InversePairs, r.SelfLoops, tc.multi, tc.inverse, tc.self)
			}
		})
	}
}

func TestHygieneSampleDeterministicAndPrivate(t *testing.T) {
	d := fixture()
	for i := 0; i < 20; i++ {
		d.entities = append(d.entities, entity{fmt.Sprintf("drop%d", i), fmt.Sprintf("--zorbo%d", i), "entity"}, entity{fmt.Sprintf("rename%d", i), fmt.Sprintf("Quillon%d Ltd", i), "organization"})
		d.mentions = append(d.mentions, mention{fmt.Sprintf("drop%d", i), "c1"}, mention{fmt.Sprintf("rename%d", i), "c1"})
	}
	a := compute(d, Options{HygieneSample: 5, SampleSeed: 7}, policy(t))
	b := compute(d, Options{HygieneSample: 5, SampleSeed: 7}, policy(t))
	if !reflect.DeepEqual(a.Sample, b.Sample) || len(a.Sample.Dropped) != 5 || len(a.Sample.Renamed) != 5 {
		t.Fatalf("sample: %+v / %+v", a.Sample, b.Sample)
	}
	c := compute(d, Options{HygieneSample: 5, SampleSeed: 8}, policy(t))
	if reflect.DeepEqual(a.Sample, c.Sample) {
		t.Fatal("different seeds returned identical sample")
	}
	// Database row order must not influence the sample.
	for i, j := 0, len(d.entities)-1; i < j; i, j = i+1, j-1 {
		d.entities[i], d.entities[j] = d.entities[j], d.entities[i]
	}
	if r := compute(d, Options{HygieneSample: 5, SampleSeed: 7}, policy(t)); !reflect.DeepEqual(a.Sample, r.Sample) {
		t.Fatal("sample depends on row order")
	}
	for _, item := range a.Sample.Dropped {
		if item.Rule == "" || item.Original == "" {
			t.Fatalf("missing drop evidence: %+v", item)
		}
	}
	for _, item := range a.Sample.Renamed {
		if item.Rule == "" || item.Original == item.Normalized {
			t.Fatalf("missing rename evidence: %+v", item)
		}
	}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Quillon") || strings.Contains(string(data), "--zorbo") {
		t.Fatal("private names leaked into report")
	}
}

func raw(t *testing.T, q string, p map[string]any) {
	t.Helper()
	ctx := context.Background()
	drv, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(ctx)
	sess := drv.NewSession(ctx, neo4j.SessionConfig{})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, q, p)
	if err == nil {
		_, err = res.Consume(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestQualityOnFixtureGraph(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ingest.SpoolDir = t.TempDir()
	cfg.Ingest.ChunkDedup = "off"
	cfg.Ingest.ChunkWords = 100
	pipe := ingest.NewPipeline(cfg, st, &extract.Mock{}, dedup.New(), func(_ context.Context, in ingest.Input) (ingest.Loaded, error) {
		return ingest.Loaded{Text: in.Text}, nil
	})
	tag := fmt.Sprintf("quality-%d", time.Now().UnixNano())
	seed := domain.Seed{Schema: 1, Rules: []domain.SeedRule{{Match: tag + "/", Domain: "fixture"}}}
	var d dataset
	d.version = graph.ExtractionSchemaVersion
	// Two real mock-ingested documents: Acme -> Zorbo and Acme -> Quillon.
	for i, tail := range []string{"Zorbo", "Quillon"} {
		source := fmt.Sprintf("%s/%d", tag, i)
		res, err := pipe.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: "Acme connects " + tail})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := st.DeleteDocument(ctx, res.DocumentID); err != nil {
				t.Error(err)
			}
		})
		if err := pipe.Process(ctx, res.DocumentID); err != nil {
			t.Fatal(err)
		}
		ch, err := st.Chunks(ctx, res.DocumentID)
		if err != nil || len(ch) != 1 {
			t.Fatalf("chunks: %+v %v", ch, err)
		}
		d.documents = append(d.documents, document{id: res.DocumentID, source: source})
		d.chunks = append(d.chunks, chunk{ch[0].ID, res.DocumentID})
		d.mentions = append(d.mentions, mention{"acme", ch[0].ID}, mention{strings.ToLower(tail), ch[0].ID})
		d.facts = append(d.facts, fact{"acme", strings.ToLower(tail), "works_at", ch[0].ID, res.DocumentID, .9, false})
	}
	d.entities = []entity{{"acme", "Acme", "person"}, {"zorbo", "Zorbo", "person"}, {"quillon", "Quillon", "person"}}
	opt := Options{Domain: "fixture", Seed: &seed}
	got, err := Compute(ctx, st, opt)
	if err != nil {
		t.Fatal(err)
	}
	if got.Documents != 2 || got.Chunks != 2 || got.Entities != 3 || got.Mentions != 4 || got.Facts.Typed != 2 {
		t.Fatalf("fixture population: %+v", got)
	}
	want := compute(d, opt, policy(t))
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		w, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("Compute differs from hand graph\ngot %s\nwant %s", g, w)
	}
}

func TestQualityStoredPopulationAndReadOnly(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	tag := fmt.Sprintf("quality-%d", time.Now().UnixNano())
	p := map[string]any{"tag": tag}
	raw(t, `CREATE (d:Document {id:$tag,source:$tag,status:'done',domain:'quality-fixture'}), (c:Chunk {id:$tag,doc_id:$tag}), (d)-[:HAS_CHUNK]->(c), (e:Entity {key:$tag,name:'--zorbo',type:'value'}), (c)-[:MENTIONS]->(e), (c)-[:MENTIONS]->(e), (x:Document {id:$tag+'-failed',source:$tag+'-failed',status:'failed',domain:'quality-fixture'}), (y:Chunk {id:$tag+'-failed',doc_id:$tag+'-failed'}), (x)-[:HAS_CHUNK]->(y), (z:Entity {key:$tag+'-failed',name:'Acme',type:'organization'}), (y)-[:MENTIONS]->(z), (e)-[:uses {doc_id:$tag+'-failed',chunk_id:$tag+'-failed',confidence:0.1}]->(z), (:FactCandidate {id:$tag+'-u',doc_id:$tag,judge:''}), (:FactCandidate {id:$tag+'-c',doc_id:$tag,judge:'unclear'}), (:RejectedFact {id:$tag+'-r',doc_id:$tag}), (:FactCandidate {id:$tag+'-f',doc_id:$tag+'-failed',judge:''})`, p)
	t.Cleanup(func() { raw(t, `MATCH (n) WHERE n.id STARTS WITH $tag OR n.key STARTS WITH $tag DETACH DELETE n`, p) })
	for _, version := range []int{1, 3, 4} {
		raw(t, `MATCH (m:GkgMeta {id:'extraction_schema'}) DELETE m`, nil)
		if version != 1 {
			raw(t, `CREATE (:GkgMeta {id:'extraction_schema',value:$v})`, map[string]any{"v": version})
		}
		before := graphtest.Snapshot(t)
		r, err := Compute(ctx, st, Options{Domain: "quality-fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if r.Documents != 1 || r.Chunks != 1 || r.Entities != 1 || r.Mentions != 1 || r.Facts.Typed != 0 || r.Facts.Rejected != 1 {
			t.Fatalf("done population: %+v", r)
		}
		number(t, "builtin ignores value scope exemptions", r.NoiseRuleShare, 1)
		if version < 4 {
			if r.Facts.CandidatesUnjudged != nil || r.Facts.CandidatesUnclear != nil {
				t.Fatal("pre-v4 candidate counts exposed")
			}
		} else if r.Facts.CandidatesUnjudged == nil || *r.Facts.CandidatesUnjudged != 1 || r.Facts.CandidatesUnclear == nil || *r.Facts.CandidatesUnclear != 1 {
			t.Fatalf("v4 counts: %+v", r.Facts)
		}
		before.AssertUnchanged(t, "Compute")
	}
}

func TestQualityConfiguredHygieneOverlay(t *testing.T) {
	st := graphtest.Store(t)
	dir := t.TempDir()
	overlay := filepath.Join(dir, "stop.txt")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(overlay, []byte("Zorbo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("ingest:\n  stoplist_file: "+overlay+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", cfgPath)
	tag := fmt.Sprintf("quality-%d", time.Now().UnixNano())
	p := map[string]any{"tag": tag}
	raw(t, `CREATE (d:Document {id:$tag,source:$tag,status:'done',domain:$tag}), (c:Chunk {id:$tag,doc_id:$tag}), (d)-[:HAS_CHUNK]->(c), (e:Entity {key:$tag,name:'Zorbo',type:'organization'}), (c)-[:MENTIONS]->(e)`, p)
	t.Cleanup(func() { raw(t, `MATCH (n) WHERE n.id=$tag OR n.key=$tag DETACH DELETE n`, p) })
	r, err := Compute(context.Background(), st, Options{Domain: tag})
	if err != nil {
		t.Fatal(err)
	}
	number(t, "overlay", r.NoiseRuleShare, 1)

	if err := os.Remove(overlay); err != nil {
		t.Fatal(err)
	}
	_, err = Compute(context.Background(), st, Options{Domain: tag})
	if err == nil || !strings.Contains(err.Error(), `load label preset "built-in"`) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Compute error = %v, want missing overlay error naming the built-in preset", err)
	}
}

func TestQualityDocumentHygienePresets(t *testing.T) {
	st := graphtest.Store(t)
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"a-custom.yaml": "allowlist: [XY]\ncode_tokens: true\nstoplist:\n  exact: [sdk/path]\nentities:\n  - name: date\n    scope: document\n",
		"z-plain.yaml":  "{}\n",
		"stop.txt":      "OverlayTerm\n",
		"config.yaml":   fmt.Sprintf("labels:\n  presets_dir: %q\ningest:\n  stoplist_file: %q\n", dir, filepath.Join(dir, "stop.txt")),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GKG_CONFIG", filepath.Join(dir, "config.yaml"))
	for _, tc := range []struct {
		name, entityName, entityType string
		presets                      []string
		dropRule, normalized         string
		missingPreset                string
	}{
		{name: "allowlist", entityName: "XY", entityType: "product", presets: []string{"a-custom"}},
		{name: "one-rejects", entityName: "  XY  ", entityType: "product", presets: []string{"", "a-custom"}, normalized: "XY"},
		{name: "both-reject", entityName: "sdk/path", entityType: "product", presets: []string{"z-plain", "a-custom"}, dropRule: "stoplist"},
		{name: "built-in-rejects-first", entityName: "sdk/path", entityType: "product", presets: []string{"a-custom", ""}, dropRule: "path_like"},
		{name: "both-accept", entityName: "  Acme  ", entityType: "product", presets: []string{"z-plain", "a-custom"}, normalized: "Acme"},
		{name: "value-label", entityName: "777", entityType: "date", presets: []string{"a-custom"}},
		{name: "code-token", entityName: "worker_task", entityType: "product", presets: []string{"a-custom"}},
		{name: "overlay", entityName: "OverlayTerm", entityType: "product", presets: []string{"a-custom", "z-plain"}, dropRule: "stoplist"},
		{name: "unknown-preset", entityName: "Acme", entityType: "product", presets: []string{"missing"}, missingPreset: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tag := fmt.Sprintf("quality-%d", time.Now().UnixNano())
			docs := make([]map[string]any, 0, len(tc.presets))
			for i, preset := range tc.presets {
				docs = append(docs, map[string]any{"id": fmt.Sprintf("%s-%d", tag, i), "preset": preset})
			}
			p := map[string]any{"tag": tag, "name": tc.entityName, "type": tc.entityType, "docs": docs}
			raw(t, `CREATE (e:Entity {key:$tag,name:$name,type:$type})
WITH e UNWIND $docs AS row
CREATE (d:Document {id:row.id,source:row.id,status:'done',domain:$tag,labels:row.preset}),
       (c:Chunk {id:row.id,doc_id:row.id}), (d)-[:HAS_CHUNK]->(c), (c)-[:MENTIONS]->(e)`, p)
			t.Cleanup(func() { raw(t, `MATCH (n) WHERE n.id STARTS WITH $tag OR n.key=$tag DETACH DELETE n`, p) })

			r, err := Compute(context.Background(), st, Options{Domain: tag, HygieneSample: 1})
			if tc.missingPreset != "" {
				if err == nil || !strings.Contains(err.Error(), tc.missingPreset) {
					t.Fatalf("Compute error = %v, want error naming the preset %q", err, tc.missingPreset)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Entities != 1 || r.Documents != len(docs) {
				t.Fatalf("got %d entities and %d documents, want 1 entity and %d documents", r.Entities, r.Documents, len(docs))
			}
			var dropped, renamed []SampleName
			if tc.dropRule != "" {
				dropped = append(dropped, SampleName{Key: tag, Type: tc.entityType, Original: tc.entityName, Rule: tc.dropRule})
			}
			if tc.normalized != "" {
				renamed = append(renamed, SampleName{Key: tag, Type: tc.entityType, Original: tc.entityName, Normalized: tc.normalized, Rule: "normalize"})
			}
			number(t, "noise rule share", r.NoiseRuleShare, float64(len(dropped)))
			if r.Sample.AvailableDropped != len(dropped) || r.Sample.AvailableRenamed != len(renamed) ||
				!slices.Equal(r.Sample.Dropped, dropped) || !slices.Equal(r.Sample.Renamed, renamed) {
				t.Errorf("sample dropped=%+v renamed=%+v, want dropped=%+v renamed=%+v", r.Sample.Dropped, r.Sample.Renamed, dropped, renamed)
			}
		})
	}
}

func TestQualitySynthetic11K(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	tag := fmt.Sprintf("quality-%d", time.Now().UnixNano())
	p := map[string]any{"tag": tag}
	raw(t, `CREATE (:Document {id:$tag,source:$tag,status:'done',domain:'quality-bench'})`, p)
	t.Cleanup(func() { raw(t, `MATCH (n) WHERE n.id STARTS WITH $tag OR n.key STARTS WITH $tag DETACH DELETE n`, p) })
	// More than two batches for chunks, mentions, and typed facts.
	for from := 0; from < 11000; from += 1000 {
		rows := make([]map[string]any, 1000)
		for i := range rows {
			rows[i] = map[string]any{"id": fmt.Sprintf("%s-%d", tag, from+i)}
		}
		raw(t, `UNWIND $rows AS row MATCH (d:Document {id:$tag}) CREATE (c:Chunk {id:row.id,doc_id:$tag}), (d)-[:HAS_CHUNK]->(c), (a:Entity {key:row.id+'-a',name:'Acme',type:'organization'}), (b:Entity {key:row.id+'-b',name:'Zorbo',type:'location'}), (c)-[:MENTIONS]->(a), (c)-[:MENTIONS]->(b), (a)-[:uses {doc_id:$tag,chunk_id:row.id,confidence:0.8}]->(b)`, map[string]any{"tag": tag, "rows": rows})
	}
	start := time.Now()
	r, err := Compute(ctx, st, Options{Domain: "quality-bench"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if r.Documents != 1 || r.Chunks != 11000 || r.Entities != 22000 || r.Mentions != 22000 || r.Facts.Typed != 11000 || r.CoMention.Edges != 11000 {
		t.Fatalf("batch loss: %+v", r)
	}
	t.Logf("synthetic 11k: chunks=%d entities=%d mentions=%d facts=%d elapsed=%s", r.Chunks, r.Entities, r.Mentions, r.Facts.Typed, elapsed)
	if elapsed >= 60*time.Second {
		t.Fatalf("Compute took %s; target <60s", elapsed)
	}
}

// TestQualityRefusesNewerSchema: a marker above v4 (written by a newer
// binary) is not read under v4 assumptions.
func TestQualityRefusesNewerSchema(t *testing.T) {
	st := graphtest.MigrationStore(t)
	raw(t, `MATCH (m:GkgMeta {id:'extraction_schema'}) DELETE m`, nil)
	raw(t, `CREATE (:GkgMeta {id:'extraction_schema',value:$v})`, map[string]any{"v": graph.SchemaVersionV4 + 1})
	t.Cleanup(func() { raw(t, `MATCH (m:GkgMeta {id:'extraction_schema'}) DELETE m`, nil) })
	if _, err := Compute(context.Background(), st, Options{}); !errors.Is(err, graph.ErrSchemaNewer) {
		t.Fatalf("Compute on v%d = %v, want ErrSchemaNewer", graph.SchemaVersionV4+1, err)
	}
}
