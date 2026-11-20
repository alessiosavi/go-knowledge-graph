package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const productRegistryFixture = `{
 "schema":"gkg-features/v1",
 "features":[{"id":"length","name":"Length","definition":"Nominal length","kind":"measure","cardinality":"multi","checkability":"page_checkable","measure":{"canonical_unit":"mm","aliases":{"mm":"1","cm":"10"}}}],
 "stores":{"example.test":{"keys":{
   "length":{"feature":"length","separator":"/","source_labels":["Length"],"dev_evidence":5,"numeric_search":false},
   "size":{"feature":"length","separator":null,"source_labels":["Size"],"dev_evidence":5,"numeric_search":false}
 },"ignore":{},"deferred":[]},
 "z.example.test":{"keys":{
   "length":{"feature":"length","separator":null,"source_labels":["Length"],"dev_evidence":5,"numeric_search":false}
 },"ignore":{},"deferred":[]}}
}`

func productFixtureDigest(data string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(data)))
}

func writeProductFixture(t *testing.T, dir, name, data string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func productFixturePins(registry, candidates string) map[string]string {
	const kit, e2 = "synthetic kit", "synthetic E2"
	registrySHA := productFixtureDigest(registry)
	score := fmt.Sprintf(`{"registry_sha256":%q,"release_candidates_sha256":%q}`, registrySHA, productFixtureDigest(candidates))
	return map[string]string{
		"R.json":           registry,
		"rf_kit.py":        kit,
		"score.json":       score,
		"report.json":      strings.TrimSuffix(score, "}") + `,"release_allowed":true,"owner_decision_required":false}`,
		"freeze.json":      fmt.Sprintf(`{"sha256":{"R.json":%q,"rf_kit.py":%q},"e2_sha256":%q}`, registrySHA, productFixtureDigest(kit), productFixtureDigest(e2)),
		"E2":               e2,
		"record-obs.jsonl": `{"type":"product"}` + "\n",
	}
}

func productReleaseTestArgs(t *testing.T, id, rev, candidates string, pins map[string]string, bindings ...string) []string {
	t.Helper()
	args := []string{"products", "release", "create", "-registry", rev, "-candidates", candidates, "-approved-by", "owner"}
	if id != "" {
		args = append(args, "-id", id)
	}
	for _, binding := range bindings {
		args = append(args, "-store-domain", binding)
	}
	dir := t.TempDir()
	for _, name := range slices.Sorted(maps.Keys(pins)) {
		path := writeProductFixture(t, dir, name, pins[name])
		args = append(args, "-pin", name+"="+path)
	}
	return args
}

func productCLI(t *testing.T, code int, contains string, args ...string) string {
	t.Helper()
	var out, stderr bytes.Buffer
	if got := run(args, &out, &stderr); got != code || !strings.Contains(out.String()+stderr.String(), contains) {
		t.Fatalf("%v = exit %d, stdout %q, stderr %q; want exit %d and %q", args, got, out.String(), stderr.String(), code, contains)
	}
	return out.String()
}

func productTestDB(t *testing.T) (*graph.Store, neo4j.Driver, func(string, map[string]any) []*neo4j.Record) {
	t.Helper()
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	t.Setenv("GKG_CONFIG", "")
	t.Setenv("GKG_MEMGRAPH_URI", uri)
	t.Setenv("GKG_MEMGRAPH_USER", "")
	t.Setenv("GKG_MEMGRAPH_PASSWORD", "")
	driver, err := neo4j.NewDriver(uri, neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Cleanup(func() { _ = driver.Close(ctx) })
	return st, driver, func(query string, params map[string]any) []*neo4j.Record {
		t.Helper()
		result, err := neo4j.ExecuteQuery(ctx, driver, query, params, neo4j.EagerResultTransformer)
		if err != nil {
			t.Fatalf("run %s: %v", query, err)
		}
		return result.Records
	}
}

func TestProductReleaseUsage(t *testing.T) {
	productCLI(t, 2, "not yet supported", "products", "release", "activate", "example-release", "-materialize", "records.jsonl")
	productCLI(t, 2, "not yet supported", "products", "release", "activate", "-materialize=records.jsonl", "example-release")
	productCLI(t, 2, "usage:", "features", "import")
	productCLI(t, 2, "usage:", "products", "release", "create")
	productCLI(t, 0, "release list", "products", "release", "list", "-h")
	pins := productFixturePins(productRegistryFixture, "")
	for _, name := range slices.Sorted(maps.Keys(pins)) {
		t.Run("missing_"+name, func(t *testing.T) {
			incomplete := maps.Clone(pins)
			delete(incomplete, name)
			args := productReleaseTestArgs(t, "", "unused", "unused", incomplete, "example.test=product-test")
			productCLI(t, 2, "missing required -pin: "+name+"\nusage:", args...)
		})
	}
}

func TestProductReleaseCreateRejections(t *testing.T) {
	_, _, query := productTestDB(t)
	sha := productFixtureDigest(productRegistryFixture)
	rev := sha[:16]
	t.Cleanup(func() {
		query(`MATCH (n) WHERE (n:FeatureRegistry OR n:FeatureRev) AND n.rev=$rev
		    OR (n:GkgMeta AND n.id='feature_registry_import')
		    OR (n:Domain AND n.id IN ['reject-test','reject-extra'])
		    DETACH DELETE n`, map[string]any{"rev": rev})
	})
	dir := t.TempDir()
	productCLI(t, 0, rev, "features", "import", writeProductFixture(t, dir, "R.json", productRegistryFixture))
	query(`CREATE (:Domain {id:'reject-test'}), (:Domain {id:'reject-extra'})`, nil)
	candidates := fmt.Sprintf(`{"registry_sha256":%q,"keys":[{"store":"example.test","feature":"length","source_key":"length"}]}`, sha)
	file := writeProductFixture(t, dir, "candidates.json", candidates)
	pins := productFixturePins(productRegistryFixture, candidates)
	const binding = "example.test=reject-test"
	for _, tc := range []struct {
		name       string
		code       int
		diagnostic string
	}{
		{"failed_domain_op", 5, `failed operation "reject-test-op"`},
		{"missing_pins", 2, "missing required -pin: R.json, rf_kit.py, score.json, report.json, freeze.json, E2, record-obs.jsonl"},
		{"extra_store_domain", 5, `store "typo.test" does not appear in released triples`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() {
				query(`MATCH (n) WHERE (n:ProductRelease AND n.id='rejected-release')
				    OR (n:ProductStore AND n.domain IN ['reject-test','reject-extra'])
				    OR (n:DomainOp AND n.op_id='reject-test-op') DETACH DELETE n`, nil)
			})
			bindings := []string{binding}
			selectedPins := pins
			switch tc.name {
			case "failed_domain_op":
				query(`CREATE (:DomainOp {op_id:'reject-test-op',state:'failed',from:'reject-test'})`, nil)
			case "missing_pins":
				selectedPins = map[string]string{"other": "synthetic extra"}
			case "extra_store_domain":
				bindings = append(bindings, "typo.test=reject-extra")
			}
			productCLI(t, tc.code, tc.diagnostic, productReleaseTestArgs(t, "rejected-release", rev, file, selectedPins, bindings...)...)
			for _, cypher := range []string{
				`MATCH (n:ProductRelease {id:'rejected-release'}) RETURN n.id`,
				`MATCH (n:ProductStore) WHERE n.domain IN ['reject-test','reject-extra'] RETURN n.store`,
				`MATCH (n:Domain) WHERE n.id IN ['reject-test','reject-extra'] AND n.product_guard IS NOT NULL RETURN n.id`,
			} {
				if rows := query(cypher, nil); len(rows) != 0 {
					t.Errorf("rejected create wrote data: %s returned %v", cypher, rows[0].Values)
				}
			}
		})
	}
}

func TestProductReleaseLifecycle(t *testing.T) {
	st, driver, query := productTestDB(t)
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n:FeatureRegistry OR n:FeatureRev OR n:ProductRelease OR n:ProductStore
		    OR n:Product OR (n:GkgMeta AND n.id IN ['product_release','feature_registry_import'])
		    OR (n:Domain AND n.id IN ['product-test','other-test'])
		    OR (n:Document AND n.id='product-test-op-doc')
		    OR ((n:DomainOp OR n:DomainDecision) AND n.op_id='product-test-op')
		    DETACH DELETE n`, nil)
	})
	fields := func(q string, want ...[]any) {
		t.Helper()
		rows := query(q, nil)
		if len(rows) != len(want) {
			t.Fatalf("%s returned %d rows, want %d", q, len(rows), len(want))
		}
		for i, row := range rows {
			if !reflect.DeepEqual(row.Values, want[i]) {
				t.Fatalf("%s row %d = %v, want %v", q, i, row.Values, want[i])
			}
		}
	}
	dir := t.TempDir()
	write := func(name, data string) string {
		return writeProductFixture(t, dir, name, data)
	}
	digest := productFixtureDigest

	registryFile := write("R.json", productRegistryFixture)
	registryDigest := digest(productRegistryFixture)
	rev := registryDigest[:16]
	productCLI(t, 0, rev, "features", "import", registryFile)
	productCLI(t, 0, "unchanged", "features", "import", registryFile)
	productCLI(t, 1, "read registry", "features", "import", filepath.Join(dir, "missing.json"))
	productCLI(t, 5, "registry", "features", "import", write("invalid.json", "{}"))
	fields(`MATCH (r:FeatureRegistry) RETURN r.rev,r.digest,r.registry_json`, []any{rev, registryDigest, productRegistryFixture})
	fields(`MATCH (f:FeatureRev) RETURN f.key,f.rev,f.id,f.name,f.definition,f.definition_digest,f.kind,f.cardinality,f.canonical_unit,f.replaces`,
		[]any{rev + "/length", rev, "length", "Length", "Nominal length", digest("Nominal length"), "measure", "multi", "mm", ""})

	renamed := strings.Replace(productRegistryFixture, `"name":"Length"`, `"name":"Measured length"`, 1)
	newRev := digest(renamed)[:16]
	productCLI(t, 0, newRev, "features", "import", write("renamed.json", renamed))
	for _, tc := range []struct {
		field, before, after string
	}{
		{"definition", "Nominal length", "Maximum length"},
		{"kind", `"kind":"measure","cardinality":"multi","checkability":"page_checkable","measure":{"canonical_unit":"mm","aliases":{"mm":"1","cm":"10"}}`,
			`"kind":"enum","cardinality":"multi","checkability":"page_checkable","measure":null`},
		{"cardinality", `"cardinality":"multi"`, `"cardinality":"single"`},
		{"canonical_unit", `"canonical_unit":"mm"`, `"canonical_unit":"cm"`},
		{"aliases", `"cm":"10"`, `"cm":"11"`},
		{"separator", `"separator":"/"`, `"separator":"|"`},
	} {
		t.Run("semantic_"+tc.field, func(t *testing.T) {
			changed := strings.Replace(productRegistryFixture, tc.before, tc.after, 1)
			if tc.field == "cardinality" {
				changed = strings.Replace(changed, `"separator":"/"`, `"separator":null`, 1)
			}
			productCLI(t, 5, tc.field, "features", "import", write("changed-"+tc.field+".json", changed))
		})
	}
	fields(`MATCH (r:FeatureRegistry) RETURN count(r)`, []any{int64(2)})
	fields(`MATCH (f:FeatureRev) RETURN count(f)`, []any{int64(2)})

	brokenRegistry := productRegistryFixture + "\n"
	brokenRev := digest(brokenRegistry)[:16]
	query(`CREATE (:FeatureRev {key:$key,id:'orphan'})`, map[string]any{"key": brokenRev + "/length"})
	productCLI(t, 1, "constraint", "features", "import", write("conflicting-key.json", brokenRegistry))
	fields(`MATCH (r:FeatureRegistry) RETURN count(r)`, []any{int64(2)})
	query(`MATCH (f:FeatureRev {id:'orphan'}) DELETE f`, nil)

	candidates := func(regDigest, keys string) string {
		return fmt.Sprintf(`{"registry_sha256":%q,"keys":[%s]}`, regDigest, keys)
	}
	const length = `{"store":"example.test","feature":"length","source_key":"length"}`
	const size = `{"store":"example.test","feature":"length","source_key":"size"}`
	body := candidates(registryDigest, size+","+length)
	candidatesFile := write("candidates.json", body)
	pinContents := productFixturePins(productRegistryFixture, body)
	const binding = "example.test=product-test"
	create := func(id, rev, file string) []string {
		return productReleaseTestArgs(t, id, rev, file, pinContents, binding)
	}
	productCLI(t, 5, "registry", create("missing-rev", "absent", candidatesFile)...)
	productCLI(t, 5, "registry_sha256", create("wrong-rev", newRev, candidatesFile)...)
	productCLI(t, 5, "not mapped", create("not-mapped", rev, write("not-mapped.json",
		candidates(registryDigest, strings.Replace(length, `"length"}`, `"unknown"}`, 1))))...)
	productCLI(t, 5, "domain", create("missing-domain", rev, candidatesFile)...)
	query(`CREATE (:Domain {id:'product-test'}), (:Domain {id:'other-test'})`, nil)
	for _, state := range []string{"running", "incomplete"} {
		query(`MERGE (o:DomainOp {op_id:'product-test-op'}) SET o.state=$state,o.from='product-test',o.target='other-test'`, map[string]any{"state": state})
		productCLI(t, 5, state, create("busy-domain", rev, candidatesFile)...)
	}
	query(`MATCH (o:DomainOp {op_id:'product-test-op'})
		SET o.action='undo',o.from='',o.target='',o.targets={doc:'product-test'}`, nil)
	productCLI(t, 5, "incomplete", create("undo-target", rev, candidatesFile)...)
	query(`MATCH (o:DomainOp {op_id:'product-test-op'})
		SET o.action='move',o.target='other-test',o.targets={doc:'other-test'},o.docs=['product-test-op-doc']`, nil)
	query(`CREATE (:Document {id:'product-test-op-doc',domain:'product-test'})`, nil)
	productCLI(t, 5, "incomplete", create("move-source", rev, candidatesFile)...)
	query(`MATCH (d:Document {id:'product-test-op-doc'}) DELETE d`, nil)
	query(`CREATE (:DomainDecision {op_id:'product-test-op',doc_id:'product-test-op-doc',domain:'other-test',previous_domain:'product-test'})`, nil)
	productCLI(t, 5, "incomplete", create("move-ledger", rev, candidatesFile)...)
	query(`MATCH (o:DomainOp {op_id:'product-test-op'}) SET o.state='completed'`, nil)
	partial := candidates(registryDigest, length+","+strings.Replace(length, "example.test", "z.example.test", 1))
	productCLI(t, 5, "missing-test", productReleaseTestArgs(t, "partial", rev, write("partial.json", partial),
		productFixturePins(productRegistryFixture, partial), binding, "z.example.test=missing-test")...)
	fields(`MATCH (r:ProductRelease) RETURN count(r)`, []any{int64(0)})
	fields(`MATCH (s:ProductStore) RETURN count(s)`, []any{int64(0)})
	const guard = `MATCH (d:Domain {id:'product-test'}) RETURN d.product_guard`
	fields(guard, []any{nil})

	for _, tc := range []struct{ pin, field, old, replacement string }{
		{"score.json", "registry_sha256", registryDigest, "wrong"},
		{"score.json", "release_candidates_sha256", digest(body), "wrong"},
		{"report.json", "registry_sha256", registryDigest, "wrong"},
		{"report.json", "release_candidates_sha256", digest(body), "wrong"},
		{"report.json", "release_allowed", `"release_allowed":true`, `"release_allowed":false`},
		{"report.json", "owner_decision_required", `"owner_decision_required":false`, `"owner_decision_required":true`},
		{"report.json", "owner_decision_required", `,"owner_decision_required":false`, ""},
		{"freeze.json", "R.json", registryDigest, "wrong"},
		{"freeze.json", "rf_kit.py", digest(pinContents["rf_kit.py"]), "wrong"},
		{"freeze.json", "e2_sha256", digest(pinContents["E2"]), "wrong"},
		{"R.json", "R.json", "Nominal length", "Wrong length"},
	} {
		t.Run(tc.pin+"_"+tc.field, func(t *testing.T) {
			badPins := maps.Clone(pinContents)
			badPins[tc.pin] = strings.Replace(badPins[tc.pin], tc.old, tc.replacement, 1)
			productCLI(t, 5, tc.field, productReleaseTestArgs(t, "bad-pin", rev, candidatesFile, badPins, binding)...)
		})
	}
	fields(`MATCH (r:ProductRelease) RETURN count(r)`, []any{int64(0)})
	fields(`MATCH (s:ProductStore) RETURN count(s)`, []any{int64(0)})

	widePins := maps.Clone(pinContents)
	widePins["other"] = "synthetic extra"
	output := productCLI(t, 0, "manifest_digest=", productReleaseTestArgs(t, "wide", rev, candidatesFile, widePins, binding)...)
	fields(guard, []any{int64(1)})
	row := query(`MATCH (r:ProductRelease {id:'wide'}) RETURN r.manifest_json,r.manifest_digest`, nil)[0]
	manifest, hash := row.Values[0].(string), row.Values[1].(string)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(manifest), &decoded); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil || string(canonical) != manifest || digest(manifest) != hash || !strings.Contains(output, hash) {
		t.Fatalf("manifest is not canonical or does not match output digest: %s %s %s", manifest, hash, output)
	}
	pins, _ := decoded["pins"].(map[string]any)
	sortedTriples := `[{"feature":"length","source_key":"length","store":"example.test"},{"feature":"length","source_key":"size","store":"example.test"}]`
	if len(pins) != 8 || !strings.Contains(manifest, sortedTriples) {
		t.Fatalf("manifest missing sorted triples or pins: %s", manifest)
	}
	for name, body := range widePins {
		if pins[name] != digest(body) {
			t.Errorf("manifest pin %q = %v, want %s", name, pins[name], digest(body))
		}
	}
	fields(`MATCH (s:ProductStore) RETURN s.store,s.domain,s.seen_seq,s.snapshot_seq,s.product_count,s.edge_count`,
		[]any{"example.test", "product-test", int64(0), int64(0), int64(0), int64(0)})
	productCLI(t, 5, "binding", productReleaseTestArgs(t, "rebind", rev, candidatesFile, pinContents, "example.test=other-test")...)
	fields(`MATCH (s:ProductStore) RETURN s.store`, []any{"example.test"})
	fields(`MATCH (r:ProductRelease) RETURN r.id`, []any{"wide"})
	fields(guard, []any{int64(1)})
	narrow := candidates(registryDigest, length)
	productCLI(t, 0, "narrow", productReleaseTestArgs(t, "narrow", rev, write("narrow.json", narrow),
		productFixturePins(productRegistryFixture, narrow), binding)...)
	fields(guard, []any{int64(2)})
	t.Run("concurrent_create", func(t *testing.T) {
		body := candidates(digest(renamed), length)
		args := productReleaseTestArgs(t, "", newRev, write("new-rev.json", body), productFixturePins(renamed, body), binding)
		type result struct {
			code           int
			stdout, stderr string
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		for range 2 {
			go func() {
				<-start
				var out, stderr bytes.Buffer
				code := run(args, &out, &stderr)
				results <- result{code, out.String(), stderr.String()}
			}()
		}
		close(start)
		first, second := <-results, <-results
		if first.code != 0 || second.code != 0 || first.stdout != second.stdout {
			t.Fatalf("concurrent release creates returned %+v and %+v", first, second)
		}
		rows := query(`MATCH (r:ProductRelease) WHERE r.registry_rev=$rev RETURN r.id,r.manifest_digest`,
			map[string]any{"rev": newRev})
		if len(rows) != 1 {
			t.Fatalf("concurrent create produced %d releases, want one", len(rows))
		}
		id, digest := rows[0].Values[0].(string), rows[0].Values[1].(string)
		if !strings.HasPrefix(id, "rel-") || first.stdout != fmt.Sprintf("id=%s manifest_digest=%s\n", id, digest) {
			t.Fatalf("CLI output %q differs from derived release %q with digest %q", first.stdout, id, digest)
		}
		if repeated := productCLI(t, 0, "rel-", args...); first.stdout != repeated {
			t.Fatalf("identical create changed from %q to %q when repeated", first.stdout, repeated)
		}
	})
	fields(guard, []any{int64(3)})
	list := productCLI(t, 0, "example.test", "products", "release", "list")
	var summaries []struct {
		ID          string   `json:"id"`
		State       string   `json:"state"`
		RegistryRev string   `json:"registry_rev"`
		ApprovedBy  string   `json:"approved_by"`
		ApprovedAt  string   `json:"approved_at"`
		Stores      []string `json:"stores"`
	}
	if err := json.Unmarshal([]byte(list), &summaries); err != nil || len(summaries) != 3 {
		t.Fatalf("release list = %q: %v", list, err)
	}
	wide := summaries[2]
	if wide.ID != "wide" || wide.State != "created" || wide.RegistryRev != rev || wide.ApprovedBy != "owner" ||
		wide.ApprovedAt == "" || !reflect.DeepEqual(wide.Stores, []string{"example.test"}) {
		t.Fatalf("wide release summary = %+v", wide)
	}
	fields(`MATCH (s:ProductStore) RETURN count(s)`, []any{int64(1)})

	productCLI(t, 0, "wide", "products", "release", "activate", "wide")
	query(`MATCH (old:FeatureRev {rev:$rev}), (later:FeatureRev {rev:$new_rev})
		CREATE (p:Product {id:'p1',store:'example.test'}), (q:Product {id:'p2',store:'other.example.test'})
		CREATE (p)-[:HAS_FEATURE {tag:'keep',source_key:'length'}]->(old),
		       (p)-[:HAS_FEATURE {tag:'dropped',source_key:'size'}]->(old),
		       (p)-[:HAS_FEATURE {tag:'wrong-rev',source_key:'length'}]->(later),
		       (q)-[:HAS_FEATURE {tag:'wrong-store',source_key:'length'}]->(old),
		       (p)-[:HAS_FEATURE {tag:'missing-key'}]->(old)`, map[string]any{"rev": rev, "new_rev": newRev})
	productCLI(t, 5, "active", "products", "release", "activate", "wide")
	productCLI(t, 5, "absent", "products", "release", "activate", "absent")
	t.Run("activation_edge_write_conflict", func(t *testing.T) {
		// Hold an uncommitted write on an edge that activation must delete.
		// The pointer and states are written before the conflicting delete.
		sess := driver.NewSession(t.Context(), neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
		defer sess.Close(context.Background())
		tx, err := sess.BeginTransaction(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Close(context.Background())
		res, err := tx.Run(t.Context(), `MATCH ()-[e:HAS_FEATURE {tag:'dropped'}]->() SET e.held=true RETURN e.tag`, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := res.Single(t.Context()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		if err := st.ActivateProductRelease(ctx, "narrow"); err == nil {
			t.Fatal("activation committed despite the conflicting edge write")
		}
	})
	fields(`MATCH ()-[e:HAS_FEATURE]->() RETURN count(e)`, []any{int64(5)})
	fields(`MATCH (m:GkgMeta {id:'product_release'}) RETURN m.value`, []any{"wide"})
	fields(`MATCH (r:ProductRelease) WHERE r.id IN ['narrow','wide'] RETURN r.id,r.state ORDER BY r.id`, []any{"narrow", "created"}, []any{"wide", "active"})
	productCLI(t, 0, "narrow", "products", "release", "activate", "narrow")
	fields(`MATCH ()-[e:HAS_FEATURE]->() RETURN e.tag`, []any{"keep"})
	fields(`MATCH (r:ProductRelease) WHERE r.id IN ['narrow','wide'] RETURN r.id,r.state ORDER BY r.id`, []any{"narrow", "active"}, []any{"wide", "retired"})
	fields(`MATCH (m:GkgMeta {id:'product_release'}) RETURN m.value`, []any{"narrow"})
	productCLI(t, 0, "wide", "products", "release", "activate", "wide")
	fields(`MATCH (r:ProductRelease) WHERE r.id IN ['narrow','wide'] RETURN r.id,r.state ORDER BY r.id`, []any{"narrow", "retired"}, []any{"wide", "active"})
	fields(`MATCH ()-[e:HAS_FEATURE]->() RETURN e.tag`, []any{"keep"})
}
