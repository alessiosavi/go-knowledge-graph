package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

func newTextReadFixture(t *testing.T, store, domain string) pushFixture {
	t.Helper()
	uri := os.Getenv("GKG_TEST_MEMGRAPH_URI")
	if uri == "" {
		t.Skip("requires a disposable Memgraph integration database")
	}
	cfg := config.Config{}
	cfg.Memgraph.URI = uri
	st, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	if err := st.Bootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := pushFixture{t: t, store: st, storeID: store, feature: "code", registry: strings.ReplaceAll(pushRegistry, pushStore, store)}
	params := map[string]any{"store": store, "domain": domain}
	f.query(`CREATE (:Domain {id:$domain}), (:ProductStore {store:$store, domain:$domain})`, params)
	t.Cleanup(func() {
		if err := st.runAuto(context.Background(), `
MATCH (n) WHERE n.store=$store OR (n:Domain AND n.id=$domain) DETACH DELETE n`, params); err != nil {
			t.Error(err)
		}
	})
	return f
}

func preserveTextReadMeta(f pushFixture, id string) {
	f.t.Helper()
	params := map[string]any{"id": id}
	before := f.query(`MATCH (m:GkgMeta {id:$id}) RETURN properties(m)`, params)
	f.t.Cleanup(func() {
		query := `MATCH (m:GkgMeta {id:$id}) DELETE m`
		if len(before) > 0 {
			query = `MERGE (m:GkgMeta {id:$id}) SET m = $props`
			params["props"] = before[0][0]
		}
		if err := f.store.runAuto(context.Background(), query, params); err != nil {
			f.t.Error(err)
		}
	})
}

// Switch the fixture pointer while preserving unrelated released observations.
func setTextReadRelease(f pushFixture, id, domain, key string, additionalStores ...string) {
	f.t.Helper()
	manifest := productManifest{
		RegistryRev: f.rev, RegistrySHA256: productDigest([]byte(f.registry)),
		StoreDomains: map[string]string{f.storeID: domain},
		Triples:      []productobs.ReleaseTriple{{Store: f.storeID, Feature: f.feature, SourceKey: key}},
	}
	for _, store := range additionalStores {
		manifest.StoreDomains[store] = domain
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		f.t.Fatal(err)
	}
	f.query(`
CREATE (:ProductRelease {id:$id, state:'active', registry_rev:$rev, manifest_json:$manifest, manifest_digest:$digest})
MERGE (m:GkgMeta {id:'product_release'})
SET m.value=$id, m.activation_seq=1`, map[string]any{"id": id, "rev": f.rev, "manifest": string(data), "digest": productDigest(data)})
	f.t.Cleanup(func() {
		if err := f.store.runAuto(context.Background(), `MATCH (r:ProductRelease {id:$id}) DETACH DELETE r`, map[string]any{"id": id}); err != nil {
			f.t.Error(err)
		}
	})
}

func TestProductSearchFeatureWireGolden(t *testing.T) {
	const store, domain, release = "t3-golden.test", "t3-golden", "t3-golden-release"
	f := newTextReadFixture(t, store, domain)
	preserveTextReadMeta(f, "product_release")
	preserveTextReadMeta(f, "feature_registry_import")
	reg, err := f.store.ImportFeatureRegistry(t.Context(), []byte(f.registry))
	if err != nil {
		t.Fatal(err)
	}
	f.rev = reg.Rev
	if !reg.Unchanged {
		t.Cleanup(func() {
			if err := f.store.runAuto(context.Background(), `
MATCH (n) WHERE n.rev=$rev AND (n:FeatureRegistry OR n:FeatureRev) DETACH DELETE n`, map[string]any{"rev": f.rev}); err != nil {
				t.Error(err)
			}
		})
	}
	setTextReadRelease(f, release, domain, "scalar")
	record := func(id string) string {
		return strings.ReplaceAll(pushRecord(store+":"+id, "fixture "+id, "null", `{"scalar":"A1"}`), pushStore, store)
	}
	f.snapshot(release, 1, "imported", record("one"), record("two"))
	f.query(`MATCH (s:ProductStore {store:$store}) SET s.imported_at='2026-11-25T00:00:00Z'`, map[string]any{"store": store})
	value, limit := "a1", 1
	args := ProductSearchRequest{Feature: "code", Domain: domain, Value: &value, Limit: &limit}
	out, err := f.store.FindProducts(t.Context(), args)
	if err != nil || len(out.Hits) != 1 || out.Cursor == "" {
		t.Fatalf("first page = %+v, %v", out, err)
	}
	wire, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"hits":[{"product":{"id":"27e080003414486c90f9be6f09ec8f5a","store":"t3-golden.test","product_id":"t3-golden.` +
		`test:two","domain":"t3-golden","title":"fixture two","brand":"","category":"","subcategory":"","link":"https:/` +
		`/t3-golden.test/p"},"evidence":[{"feature":{"id":"code","name":"Code","kind":"code","cardinality":"multi","can` +
		`onical_unit":"","key":"17e07a1fdf7ec38a/code","rev":"17e07a1fdf7ec38a","definition":"Push test code","definiti` +
		`on_digest":"e028e4a105a53bee2a6082818801a1020079ec67370a7d0dc19cfe48aa174481","replaces":""},"source_key":"sca` +
		`lar","raw":"A1","items":["A1"],"split_status":"split","conflict":false,"numeric":false,"release_id":"t3-golden` +
		`-release"}],"evidence_truncated":false}],"cursor":"eyJyZWxlYXNlIjoiZGVkZWNmNzMyZGM1ZGVkOWNkNzBmZjVjZTgwMmRhZjk` +
		`zNjc1ODIwNWJiZGFjMDE2MGNiZGQwNWNlNWM0ZDM3MyIsInNuYXBzaG90IjoiYjE2ZWVhYTBhZjUyMTg5M2NkZDk2Y2Q0ZjIyZjU5MDYyZTRhO` +
		`DQwNDM5NTRlYmI3MDk5YjBhNmNiYTY1ZjlmMCIsImZpbHRlcnMiOiI5MDI2NThmZGUzOGE4M2YxYWJhNDEyNGExYjFjOGZkZGY5YTE2NGZmOWV` +
		`lYmVhYTUzZmUyOGY1YjM4OTRiNjljIiwibGFzdF9pZCI6IjI3ZTA4MDAwMzQxNDQ4NmM5MGY5YmU2ZjA5ZWM4ZjVhIn0"}`
	if string(wire) != want {
		t.Fatalf("feature wire changed:\n%s", wire)
	}
	args.Cursor = out.Cursor
	page, err := f.store.FindProducts(t.Context(), args)
	if err != nil || len(page.Hits) != 1 || page.Cursor != "" || page.Hits[0].Product.ID == out.Hits[0].Product.ID {
		t.Fatalf("continuation = %+v, %v", page, err)
	}
}

func t3Vector(x, y float64) []float64 {
	v := make([]float64, 768)
	v[0], v[1] = x, y
	return v
}

func TestProductTextSearchScoresAndScope(t *testing.T) {
	const store, domain = "t3-score.test", "t3-score"
	f := newTextReadFixture(t, store, domain)
	preserveTextReadMeta(f, "product_release")
	f.rev = "t3-score-rev"
	setTextReadRelease(f, "t3-score-first", domain, "scalar", "t3-legacy.test")
	f.store.productTextFloor = -1
	if err := f.store.EnsureTextVecIndex(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.query(`
CREATE (:ProductStore {store:'t3-legacy.test', domain:$domain, product_count:1}),
       (:ProductStore {store:'t3-foreign.test', domain:'t3-other', text_version:1}),
       (:ProductStore {store:'t3-unbound.test', text_version:1})
WITH 1 AS ignored
MATCH (s:ProductStore {store:$store}) SET s.text_version=1, s.product_count=5`, map[string]any{"domain": domain, "store": store})
	defer f.query("DROP VECTOR INDEX text_vec", nil)
	defer f.query(`
MATCH (n) WHERE (n:TextVec AND n.id STARTS WITH 't3-score-') OR n.store IN
['t3-legacy.test','t3-foreign.test','t3-unbound.test'] SET n.embedding=null DETACH DELETE n`, nil)
	row := func(id, store, domain, key, space string, vec []float64) map[string]any {
		return map[string]any{"id": "t3-score-" + id, "store": store, "domain": domain, "key": key, "space": space, "vec": vec}
	}
	f.query(`
UNWIND $rows AS r
MERGE (p:Product {id:r.id})
ON CREATE SET p.store=r.store, p.domain=r.domain, p.title=r.id, p.product_id=r.store + ':' + r.id
CREATE (p)-[:HAS_TEXT {key:r.key,truncated:true}]->(:ProductText {id:r.id+r.key, store:r.store, vec_id:r.id+r.key, text:$text})
FOREACH (x IN CASE WHEN r.vec IS NULL THEN [] ELSE [1] END |
  CREATE (v:TextVec {id:r.id+r.key, vec_id:r.id+r.key, space:r.space}) SET v.embedding=r.vec)`, map[string]any{
		"text": strings.Repeat("é", 201),
		"rows": []map[string]any{
			row("a", store, domain, "scalar", ProductTextSpace, t3Vector(1, 0)),
			row("a", store, domain, "b", ProductTextSpace, t3Vector(1, 0)),
			row("a", store, domain, "c", ProductTextSpace, t3Vector(1, 0)),
			row("a", store, domain, "d", ProductTextSpace, t3Vector(1, 0)),
			row("b", store, domain, "scalar", ProductTextSpace, t3Vector(1, 0)),
			row("c", store, domain, "scalar", ProductTextSpace, t3Vector(0, 1)),
			row("d", store, domain, "scalar", ProductTextSpace, t3Vector(-1, 0)),
			row("wrong-space", store, domain, "scalar", "t3-unsupported", t3Vector(1, 0)),
			row("legacy", "t3-legacy.test", domain, "scalar", ProductTextSpace, t3Vector(1, 0)),
			row("foreign", "t3-foreign.test", "t3-other", "scalar", ProductTextSpace, t3Vector(1, 0)),
			row("unbound", "t3-unbound.test", domain, "scalar", ProductTextSpace, t3Vector(1, 0)),
		},
	})
	got, err := f.store.SearchProductTexts(t.Context(), t3Vector(1, 0), domain, 10, nil)
	if err != nil || len(got.Hits) != 4 || !got.Exhausted || got.CandidatesTruncated || !got.Partial ||
		fmt.Sprint(got.StoresNotMaterialized) != "[t3-legacy.test]" {
		t.Fatalf("search = %+v, %v", got, err)
	}
	for i, score := range []float64{1, 1, 0, -1} {
		hit := got.Hits[i]
		if hit.Score != score || hit.ID != fmt.Sprintf("t3-score-%c", 'a'+i) {
			t.Fatalf("hit %d = %+v", i, hit)
		}
	}
	if len(got.Hits[0].Matched) != 3 || got.Hits[0].Matched[0].Key != "b" {
		t.Fatalf("top texts = %+v", got.Hits[0].Matched)
	}
	match := got.Hits[1].Matched[0]
	if !match.Checked || !match.TextTruncated || !match.ExcerptCut || match.Text != strings.Repeat("é", 200) || match.TextID != "t3-score-bscalar" {
		t.Fatalf("matched text = %+v", match)
	}

	setTextReadRelease(f, "t3-score-second", domain, "list")

	f.store.productTextFloor = .72
	got, err = f.store.SearchProductTexts(t.Context(), t3Vector(1, 0), domain, 10, []string{"t3-score-a"})
	if err != nil || len(got.Hits) != 1 || got.Hits[0].ID != "t3-score-b" || got.Hits[0].Matched[0].Checked {
		t.Fatalf("search after release switch, exclusions and floor = %+v, %v", got, err)
	}
	detail, err := f.store.GetProduct(t.Context(), ProductLookup{ID: "t3-score-b", WithTexts: true, TextKeys: []string{"scalar"}})
	if err != nil || detail.ProductTextFields == nil || len(detail.Texts) != 1 ||
		detail.Texts[0].Checked || detail.Texts[0].Text != strings.Repeat("é", 201) {
		t.Fatalf("stored texts after release switch = %+v, %v", detail.ProductTextFields, err)
	}
}

func TestProductTextSearchWidening(t *testing.T) {
	const store, domain = "t3-widen.test", "t3-widen"
	f := newTextReadFixture(t, store, domain)
	preserveTextReadMeta(f, "product_release")
	setTextReadRelease(f, "t3-widen-release", domain, "scalar")
	f.store.productTextFloor = .72
	if err := f.store.EnsureTextVecIndex(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer f.query("DROP VECTOR INDEX text_vec", nil)
	defer f.query(`
MATCH (n) WHERE (n:TextVec AND n.id STARTS WITH 't3-widen-') OR n.store='t3-widen-foreign.test'
SET n.embedding=null DETACH DELETE n`, nil)
	f.query(`
MATCH (s:ProductStore {store:$store}) SET s.text_version=1, s.product_count=2
CREATE (p:Product {id:'t3-widen-hit', store:$store, domain:$domain})-[:HAS_TEXT {key:'scalar'}]->
       (:ProductText {id:'t3-widen-hit', store:$store, vec_id:'t3-widen-hit', text:'Material: ceramic'})
CREATE (v:TextVec {id:'t3-widen-hit', vec_id:'t3-widen-hit', space:$space}) SET v.embedding=$target
CREATE (:ProductStore {store:'t3-widen-foreign.test', domain:'t3-foreign', text_version:1})
CREATE (:Product {id:'t3-widen-foreign', store:'t3-widen-foreign.test', domain:'t3-foreign'})
CREATE (p3:Product {id:'t3-widen-excluded', store:$store, domain:$domain})
WITH p3
UNWIND range(1, 260) AS i
WITH p3, i, 't3-widen-' + toString(i) AS id
CREATE (v:TextVec {id:id, vec_id:id, space:$space}) SET v.embedding=[1.0, toFloat(i)/400.0] + [x IN range(1, 766) | 0.0]
CREATE (p3)-[:HAS_TEXT {key:id}]->(:ProductText {id:id, store:$store, vec_id:id, text:'Material: bronze'})`,
		map[string]any{"store": store, "domain": domain, "space": ProductTextSpace, "target": t3Vector(.8, .6)})
	got, err := f.store.SearchProductTexts(t.Context(), t3Vector(1, 0), domain, 1, []string{"t3-widen-excluded"})
	if err != nil || !got.Exhausted || got.CandidatesTruncated || len(got.Hits) != 1 || got.Hits[0].ID != "t3-widen-hit" {
		t.Fatalf("widened result = %+v, %v", got, err)
	}

	f.store.productTextFloor = 1
	got, err = f.store.SearchProductTexts(t.Context(), t3Vector(1, 0), domain, 1, nil)
	if err != nil || !got.Exhausted || got.CandidatesTruncated || len(got.Hits) != 0 {
		t.Fatalf("widening after floor = %+v, %v", got, err)
	}
	f.store.productTextFloor = .72

	for start := 1000; start < 18000; start += 1000 {
		f.query(`
MATCH (p:Product {id:'t3-widen-foreign'})
UNWIND range($start, $start+999) AS i
WITH p, 't3-widen-foreign-' + toString(i) AS id
CREATE (v:TextVec {id:id, vec_id:id, space:$space}) SET v.embedding=$vec
CREATE (p)-[:HAS_TEXT {key:id}]->(:ProductText {id:id, store:p.store, vec_id:id, text:'Material: bronze'})`,
			map[string]any{"start": start, "space": ProductTextSpace, "vec": t3Vector(1, 0)})
	}
	got, err = f.store.SearchProductTexts(t.Context(), t3Vector(1, 0), domain, 2, []string{"t3-widen-excluded"})
	if err != nil || got.Exhausted || !got.CandidatesTruncated || len(got.Hits) > 1 ||
		(len(got.Hits) == 1 && got.Hits[0].ID != "t3-widen-hit") {
		t.Fatalf("capped result = %+v, %v", got, err)
	}
	got, err = f.store.SearchProductTexts(t.Context(), t3Vector(1, 0), "t3-unknown", 2, nil)
	if err != nil || !got.Exhausted || got.CandidatesTruncated || len(got.Hits) != 0 {
		t.Fatalf("empty scope with a large index = %+v, %v", got, err)
	}
}

func TestProductTextSearchReleaseScope(t *testing.T) {
	const store, other, domain = "t3-scope.test", "t3-scope-other.test", "t3-scope"
	f := newTextReadFixture(t, store, domain)
	preserveTextReadMeta(f, "product_release")
	f.rev = "t3-scope-rev"
	setTextReadRelease(f, "t3-scope-wide", domain, "scalar", other)
	if err := f.store.EnsureTextVecIndex(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer f.query("DROP VECTOR INDEX text_vec", nil)
	defer f.query(`
MATCH (n) WHERE (n:TextVec AND n.id IN [$store,$other]) OR n.store=$other
SET n.embedding=null DETACH DELETE n`, map[string]any{"store": store, "other": other})
	f.query(`
CREATE (:ProductStore {store:$other, domain:$domain})
WITH 1 AS ignored
MATCH (s:ProductStore) WHERE s.store IN [$store, $other]
SET s.text_version=1, s.product_count=1, s.text_count=1, s.text_edge_count=1
CREATE (:Product {id:s.store, store:s.store, domain:s.domain})-[:HAS_TEXT {key:'scalar'}]->
       (:ProductText {id:s.store, store:s.store, vec_id:s.store, text:'Material: ceramic'})
CREATE (v:TextVec {id:s.store, vec_id:s.store, space:$space}) SET v.embedding=$vec`,
		map[string]any{"store": store, "other": other, "domain": domain, "space": ProductTextSpace, "vec": t3Vector(1, 0)})
	check := func(want ...string) {
		t.Helper()
		got, err := f.store.SearchProductTexts(t.Context(), t3Vector(1, 0), domain, 10, nil)
		ids := make([]string, 0, len(got.Hits))
		for _, hit := range got.Hits {
			ids = append(ids, hit.ID)
		}
		if err != nil || got.Partial || len(got.StoresNotMaterialized) != 0 || fmt.Sprint(ids) != fmt.Sprint(want) {
			t.Errorf("scope got ids %v, partial=%v, not materialized=%v, err=%v; want ids %v with full coverage",
				ids, got.Partial, got.StoresNotMaterialized, err, want)
		}
	}
	check(other, store)
	if err := f.store.DeleteProductStore(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	check(store)

	narrow := f
	narrow.storeID = other
	narrow.seedRelease("t3-scope-narrow", domain)
	defer f.query(`MATCH (r:ProductRelease {id:'t3-scope-narrow'}) DELETE r`, nil)
	narrow.activate("t3-scope-narrow")
	check()
}

func TestProductTextSearchCoverageCache(t *testing.T) {
	const store, domain = "t3-coverage.test", "t3-coverage"
	f := newTextReadFixture(t, store, domain)
	preserveTextReadMeta(f, "product_release")
	setTextReadRelease(f, "t3-coverage-release", domain, "scalar")
	if err := f.store.EnsureTextVecIndex(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer f.query("DROP VECTOR INDEX text_vec", nil)
	defer f.query(`MATCH (v:TextVec {id:$store}) SET v.embedding=null DELETE v`, map[string]any{"store": store})
	params := map[string]any{"store": store, "domain": domain, "space": ProductTextSpace, "vec": t3Vector(1, 0)}
	f.query(`
MATCH (s:ProductStore {store:$store})
SET s.text_version=1, s.product_count=1, s.text_count=2, s.text_edge_count=1
CREATE (:ProductText {id:'t3-coverage-orphan', store:$store, vec_id:'t3-coverage-orphan'})
CREATE (:Product {id:$store, store:$store, domain:$domain})-[:HAS_TEXT]->(:ProductText {id:$store, store:$store, vec_id:$store})
CREATE (v:TextVec {id:$store, vec_id:$store, space:$space}) SET v.embedding=$vec`, params)
	check := func(wantPartial bool) {
		t.Helper()
		got, err := f.store.SearchProductTexts(t.Context(), t3Vector(1, 0), domain, 1, nil)
		if err != nil || got.Partial != wantPartial {
			t.Errorf("partial = %t, err = %v; want partial = %t", got.Partial, err, wantPartial)
		}
	}
	check(false) // An unreferenced text without a vector does not affect coverage.
	f.query(`MATCH (v:TextVec {id:$store}) SET v.space='other-space'`, params)
	check(false) // The cached coverage survives a vector-only change within the TTL.
	f.query(`
MATCH (s:ProductStore {store:$store}), (p:Product {id:$store}), (n:ProductText {id:'t3-coverage-orphan'})
CREATE (p)-[:HAS_TEXT]->(n)
SET s.text_edge_count=s.text_edge_count+1`, params)
	check(true) // A changed store fingerprint forces a fresh probe.
}

func TestProductTextCoverageProbeTiming(t *testing.T) {
	const store, domain, count = "t3-probe.test", "t3-probe", 30_000
	f := newTextReadFixture(t, store, domain)
	params := map[string]any{"stores": []string{store}, "domain": domain, "space": ProductTextSpace}
	plan := fmt.Sprint(f.query("EXPLAIN "+missingProductTextVectorsQuery, params))
	// Memgraph picks the plan from the current graph statistics, so its shape
	// differs between an empty and a populated database; log it, do not pin it.
	if !strings.Contains(plan, ":TextVec {vec_id}") {
		t.Fatalf("coverage probe plan does not look up TextVec by vec_id: %s", plan)
	}
	t.Logf("coverage probe plan: %s", plan)
	if err := f.store.EnsureTextVecIndex(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer f.query("DROP VECTOR INDEX text_vec", nil)
	defer f.query(`
MATCH (v:TextVec) WHERE v.id STARTS WITH $prefix
SET v.embedding=null DELETE v`, map[string]any{"prefix": store + ":"})
	for start := 0; start < count; start += 1000 {
		f.query(`
UNWIND range($start, $start+999) AS i
WITH i, $store + ':' + toString(i) AS id
CREATE (:Product {id:id, store:$store, domain:$domain})-[:HAS_TEXT]->(:ProductText {id:id, store:$store, vec_id:id})
CREATE (v:TextVec {id:id, vec_id:id, space:$space})
SET v.embedding=[1.0, toFloat(i)/30000.0] + [x IN range(1, 766) | 0.0]`,
			map[string]any{"start": start, "store": store, "domain": domain, "space": ProductTextSpace})
	}
	start := time.Now()
	rows := f.query(missingProductTextVectorsQuery, params)
	t.Logf("coverage probe over %d synthetic texts with full coverage: %s", count, time.Since(start))
	if len(rows) != 0 {
		t.Fatalf("full coverage reported missing vectors: %v", rows)
	}
}

func TestProductTextSearchIndexReadiness(t *testing.T) {
	f := newTextReadFixture(t, "t3-index.test", "t3-index")
	ctx := t.Context()
	check := func() {
		t.Helper()
		if err := f.store.CheckProductTextIndex(ctx); !errors.Is(err, ErrProductTextIndexUnavailable) {
			t.Fatalf("unavailable index check = %v", err)
		}
		if _, err := f.store.SearchProductTexts(ctx, t3Vector(1, 0), "t3-index", 1, nil); !errors.Is(err, ErrProductTextIndexUnavailable) {
			t.Fatalf("unavailable search = %v", err)
		}
	}
	check()
	f.query(`CREATE VECTOR INDEX text_vec ON :TextVec(embedding)
WITH CONFIG {"dimension":8,"capacity":256,"metric":"cos","scalar_kind":"f32"}`, nil)
	defer f.query("DROP VECTOR INDEX text_vec", nil)
	check()
}
