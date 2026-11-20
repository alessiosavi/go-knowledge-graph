package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const snapshotRegistry = `{
 "schema":"gkg-features/v1",
 "features":[
   {"id":"length","name":"Length","definition":"Nominal length","kind":"measure","cardinality":"single","checkability":"page_checkable","measure":{"canonical_unit":"mm","aliases":{"mm":"1","cm":"10","um":"0.000001","nm":"0.000000001"}}},
   {"id":"code","name":"Code","definition":"Product code","kind":"code","cardinality":"multi","checkability":"page_checkable","measure":null}
 ],
 "stores":{
   "example.test":{"keys":{
     "length":{"feature":"length","separator":null,"source_labels":["Length"],"dev_evidence":5,"numeric_search":true},
     "size":{"feature":"length","separator":null,"source_labels":["Size"],"dev_evidence":5,"numeric_search":false},
     "codes":{"feature":"code","separator":"/","source_labels":["Codes"],"dev_evidence":5,"numeric_search":false}
   },"ignore":{},"deferred":[]},
   "z.example.test":{"keys":{
     "length":{"feature":"length","separator":null,"source_labels":["Length"],"dev_evidence":5,"numeric_search":true}
   },"ignore":{},"deferred":[]}
 }
}`

func snapshotRecord(id, title, attributes string) string {
	store, _, _ := strings.Cut(id, ":")
	return fmt.Sprintf(`{"store":%q,"product_id":%q,"title":%q,"brand":"Maker","category":"A","subcategory":"B","link":"https://example.test/item","product_type":null,"page_specs":null,"attributes":%s}`+"\n",
		store, id, title, attributes)
}

func TestProductImportPreflightCLI(t *testing.T) {
	t.Setenv("GKG_MEMGRAPH_URI", "://invalid")
	dir := t.TempDir()
	valid := snapshotRecord("example.test:1", "", "{}")
	var tooMany strings.Builder
	for i := range 10001 {
		fmt.Fprint(&tooMany, snapshotRecord(fmt.Sprintf("example.test:%d", i), "", "{}"))
	}
	attrs := make(map[string]string)
	for i := range 513 {
		attrs[fmt.Sprint(i)] = ""
	}
	manyAttrs, err := json.Marshal(attrs)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, data, reason string }{
		{"invalid", valid + strings.Repeat("{}\n", 1001), "records rejected"},
		{"duplicate", valid + valid, "duplicate product_id"},
		{"duplicate_member", strings.Replace(valid, `"attributes":{}`, `"attributes":{"one":"","one":""}`, 1), "records rejected"},
		{"store", snapshotRecord("z.example.test:1", "", "{}"), "store"},
		{"long_row", strings.Repeat(" ", 1<<20) + "\n" + valid, "row"},
		{"long_title", snapshotRecord("example.test:1", strings.Repeat("a", 4097), "{}"), "title value exceeds 4 KiB"},
		{"many_attrs", snapshotRecord("example.test:1", "", string(manyAttrs)), "attributes"},
		{"many_products", tooMany.String(), "10,000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := writeProductFixture(t, dir, tc.name+".jsonl", tc.data)
			report := filepath.Join(dir, tc.name+".json")
			productCLI(t, 5, tc.reason, "products", "import", "-store", "example.test", "-release", "unused",
				"-snapshot-seq", "1", "-report", report, file)
			var result struct {
				RejectionCount int               `json:"rejection_count"`
				Rejections     []json.RawMessage `json:"rejections"`
			}
			data, err := os.ReadFile(report)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if tc.name == "invalid" && (result.RejectionCount != 1001 || len(result.Rejections) != 1000) {
				t.Fatalf("rejection count/details = %d/%d", result.RejectionCount, len(result.Rejections))
			}
		})
	}
	file := writeProductFixture(t, dir, "valid.jsonl", valid)
	report := filepath.Join(dir, "valid.json")
	productCLI(t, 2, "flag provided but not defined", "products", "import", "-allow-rejects")
	productCLI(t, 2, "flag needs an argument", "products", "import", "-store", "example.test", "-release", "unused", "-snapshot-seq", "1", file, "-report")
	productCLI(t, 1, "must differ", "products", "import", "-store", "example.test", "-release", "unused", "-snapshot-seq", "1", "-report", file, file)
	productCLI(t, 1, "gkg products import:", "products", "import", "-store", "example.test", "-release", "unused", "-snapshot-seq", "1", "-report", report, file)
}

func TestProductImportValueCaps(t *testing.T) {
	_, _, query := productTestDB(t)
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n:FeatureRegistry OR n:FeatureRev OR n:ProductRelease OR n:ProductStore
		    OR n:Product OR (n:GkgMeta AND n.id IN ['product_release','feature_registry_import'])
		    OR (n:Domain AND n.id='snapshot-value-test') DETACH DELETE n`, nil)
	})
	query(`CREATE (:Domain {id:'snapshot-value-test'})`, nil)
	dir := t.TempDir()
	digest := productFixtureDigest(snapshotRegistry)
	registry := writeProductFixture(t, dir, "R.json", snapshotRegistry)
	productCLI(t, 0, digest[:16], "features", "import", registry)
	const release = "snapshot-value-test"
	candidates := fmt.Sprintf(`{"registry_sha256":%q,"keys":[
		{"store":"example.test","feature":"length","source_key":"length"},
		{"store":"example.test","feature":"code","source_key":"codes"}]}`, digest)
	path := writeProductFixture(t, dir, "candidates.json", candidates)
	productCLI(t, 0, release, productReleaseTestArgs(t, release, digest[:16], path,
		productFixturePins(snapshotRegistry, candidates), "example.test=snapshot-value-test")...)
	productCLI(t, 0, "activated", "products", "release", "activate", release)
	const id = "example.test:1"
	long := strings.Repeat("a", 2<<10)
	importRow := func(t *testing.T, seq string, exit int, want, row string) {
		t.Helper()
		file := writeProductFixture(t, dir, seq+".jsonl", row)
		productCLI(t, exit, want, "products", "import", "-store", "example.test", "-release", release,
			"-snapshot-seq", seq, "-report", filepath.Join(dir, seq+".json"), file)
	}
	t.Run("unmapped", func(t *testing.T) {
		importRow(t, "1", 0, "products=1 edges=0", snapshotRecord(id, "", fmt.Sprintf(`{"unmapped":%q}`, long)))
	})
	t.Run("unreleased", func(t *testing.T) {
		importRow(t, "2", 0, "products=1 edges=1", snapshotRecord(id, "", fmt.Sprintf(
			`{"length":"2 cm","size":%q}`, long)))
		rows := query(`MATCH (:Product)-[e:HAS_FEATURE]->() RETURN e.source_key,e.conflict`, nil)
		if len(rows) != 1 || !reflect.DeepEqual(rows[0].Values, []any{"length", true}) {
			t.Fatalf("expected only the released edge, conflicting with its unreleased alias, got %v", rows)
		}
	})
	t.Run("released", func(t *testing.T) {
		baseline := graphtest.Snapshot(t)
		importRow(t, "3", 5, "released feature value exceeds 1 KiB", snapshotRecord(id, "", fmt.Sprintf(`{"length":%q}`, long)))
		baseline.AssertUnchanged(t, "oversized released value")
	})
	t.Run("released_item", func(t *testing.T) {
		// Compatibility normalization expands this Arabic ligature to several words.
		row := snapshotRecord(id, "", fmt.Sprintf(`{"codes":%q}`, strings.Repeat("\ufdfa", 100)))
		baseline := graphtest.Snapshot(t)
		importRow(t, "4", 5, "item exceeds 1 KiB", row)
		baseline.AssertUnchanged(t, "oversized released item")
	})
	t.Run("product_scalar_and_page_specs", func(t *testing.T) {
		row := strings.Replace(snapshotRecord(id, strings.Repeat("a", 4<<10), "{}"), `"page_specs":null`,
			fmt.Sprintf(`"page_specs":[{"label":"unused","value":%q}]`, long), 1)
		importRow(t, "5", 0, "products=1 edges=0", row)
	})
}

func TestProductSnapshotLifecycle(t *testing.T) {
	st, _, query := productTestDB(t)
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n:FeatureRegistry OR n:FeatureRev OR n:ProductRelease OR n:ProductStore
		    OR n:Product OR (n:GkgMeta AND n.id IN ['product_release','feature_registry_import'])
		    OR (n:Domain AND n.id IN ['snapshot-test','other-snapshot-test'])
		    OR (n:DomainOp AND n.op_id='snapshot-test-op') DETACH DELETE n`, nil)
	})
	check := func(q string, want ...[]any) {
		t.Helper()
		rows := query(q, nil)
		got := make([][]any, 0, len(rows))
		for _, row := range rows {
			got = append(got, row.Values)
		}
		if !slices.EqualFunc(got, want, func(a, b []any) bool { return reflect.DeepEqual(a, b) }) {
			t.Fatalf("%s = %v, want %v", q, got, want)
		}
	}
	query(`CREATE (:Domain {id:'snapshot-test'}), (:Domain {id:'other-snapshot-test'})`, nil)
	dir := t.TempDir()
	digest := productFixtureDigest(snapshotRegistry)
	registryFile := writeProductFixture(t, dir, "R.json", snapshotRegistry)
	productCLI(t, 0, digest[:16], "features", "import", registryFile)
	const (
		length = `{"store":"example.test","feature":"length","source_key":"length"}`
		size   = `{"store":"example.test","feature":"length","source_key":"size"}`
		codes  = `{"store":"example.test","feature":"code","source_key":"codes"}`
		other  = `{"store":"z.example.test","feature":"length","source_key":"length"}`
	)
	create := func(id, keys string) string {
		t.Helper()
		body := fmt.Sprintf(`{"registry_sha256":%q,"keys":[%s]}`, digest, keys)
		path := writeProductFixture(t, dir, id+".json", body)
		bindings := []string{"example.test=snapshot-test"}
		if strings.Contains(keys, other) {
			bindings = append(bindings, "z.example.test=other-snapshot-test")
		}
		productCLI(t, 0, id, productReleaseTestArgs(t, id, digest[:16], path, productFixturePins(snapshotRegistry, body), bindings...)...)
		return path
	}
	wide := create("wide-snapshot", strings.Join([]string{length, size, codes, other}, ","))
	const guard = `MATCH (d:Domain {id:'other-snapshot-test'}) RETURN d.product_guard`
	query(`CREATE (:DomainOp {op_id:'snapshot-test-op',state:'incomplete',target:'other-snapshot-test'})`, nil)
	check(guard, []any{int64(1)})
	productCLI(t, 0, "unbound", "products", "unbind", "z.example.test")
	check(guard, []any{int64(2)})
	check(`MATCH (s:ProductStore {store:'z.example.test'}) RETURN s.store`)
	check(`MATCH (o:DomainOp {op_id:'snapshot-test-op'}) RETURN o.state`, []any{"incomplete"})
	query(`MATCH (o:DomainOp {op_id:'snapshot-test-op'}) SET o.state='completed'`, nil)
	create("rebound-snapshot", length+","+other)
	check(guard, []any{int64(3)})

	rows := []string{
		snapshotRecord("example.test:1", "first", `{"length":"2 cm","size":"3 cm","codes":" c 1 / B2","missing":"value"}`),
		snapshotRecord("example.test:2", "range", `{"length":"1-2 cm"}`),
		snapshotRecord("example.test:3", "overflow", `{"length":"9223372036855 mm"}`),
		snapshotRecord("example.test:4", "fraction", `{"length":"1 nm"}`),
		snapshotRecord("example.test:5", "zero edges", "{}"),
	}
	file := writeProductFixture(t, dir, "full.jsonl", strings.Join(rows, ""))
	report := filepath.Join(dir, "report.json")
	args := func(release, seq, file string, extra ...string) []string {
		cmd := []string{"products", "import", "-store", "example.test", "-release", release, "-snapshot-seq", seq, "-report", report, file}
		return append(cmd, extra...)
	}
	baseline := graphtest.Snapshot(t)
	productCLI(t, 5, "active", args("wide-snapshot", "1", file)...)
	baseline.AssertUnchanged(t, "inactive release")
	productCLI(t, 0, "activated", "products", "release", "activate", "wide-snapshot")
	productCLI(t, 0, "imported", args("wide-snapshot", "1", file)...)
	var importReport graph.ProductImportReport
	readReport := func() {
		t.Helper()
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		importReport = graph.ProductImportReport{}
		if err := json.Unmarshal(data, &importReport); err != nil {
			t.Fatal(err)
		}
	}
	readReport()
	if importReport.SnapshotDigest != productFixtureDigest(strings.Join(rows, "")) || importReport.ProductCount != 5 ||
		importReport.EdgeCount != 6 || importReport.UnmappedKeys["missing"] != 1 || importReport.UnmappedCount != 1 ||
		importReport.Conflicts != 2 || importReport.ParseStatus["length"]["ok"] != 5 || importReport.ParseStatus["code"]["not_applicable"] != 1 {
		t.Fatalf("unexpected import report: %+v", importReport)
	}
	const watermark = `MATCH (s:ProductStore {store:'example.test'}) RETURN s.snapshot_seq,s.seen_seq,s.product_count,s.edge_count,s.wiped_seq`
	check(watermark, []any{int64(1), int64(1), int64(5), int64(6), nil})
	check(`MATCH (p:Product)-[e:HAS_FEATURE]->(:FeatureRev)
		WHERE e.source_key='length' RETURN p.product_id,e.qualifier,e.numeric,e.lo_scaled,e.hi_scaled ORDER BY p.product_id`,
		[]any{"example.test:1", "exact", true, int64(20000000), int64(20000000)},
		[]any{"example.test:2", "range", true, nil, nil},
		[]any{"example.test:3", "exact", true, nil, nil},
		[]any{"example.test:4", "exact", true, nil, nil})
	check(`MATCH (:Product)-[e:HAS_FEATURE {source_key:'size'}]->() RETURN e.numeric,e.lo_scaled,e.hi_scaled,e.conflict`,
		[]any{false, nil, nil, true})
	check(`MATCH (:Product)-[e:HAS_FEATURE {source_key:'codes'}]->() RETURN e.items_fold,e.value_fold`,
		[]any{[]any{"C1", "B2"}, nil})
	check(`MATCH (p:Product {product_id:'example.test:1'}) RETURN p.id`,
		[]any{productFixtureDigest(`["product","example.test","example.test:1"]`)[:32]})
	assertProductSnapshotEmit(t, query, registryFile, wide, "wide-snapshot", file)

	tooManyItems := writeProductFixture(t, dir, "items.jsonl",
		snapshotRecord("example.test:1", "", fmt.Sprintf(`{"codes":%q}`, strings.Repeat("a/", 64)+"a")))
	baseline = graphtest.Snapshot(t)
	productCLI(t, 5, "stale", args("wide-snapshot", "1", file)...)
	productCLI(t, 5, "products", "products", "unbind", "example.test")
	shrink := writeProductFixture(t, dir, "shrink.jsonl", strings.Join(rows[:4], ""))
	productCLI(t, 5, "shrink", args("wide-snapshot", "2", shrink)...)
	strict := writeProductFixture(t, dir, "strict.jsonl", rows[0]+rows[0])
	productCLI(t, 5, "duplicate", args("wide-snapshot", "2", strict)...)
	productCLI(t, 5, "more than 64 items", args("wide-snapshot", "2", tooManyItems)...)
	baseline.AssertUnchanged(t, "stale, strict, nonempty unbind and shrink refusals")
	productCLI(t, 0, "unchanged", args("wide-snapshot", "2", file)...)
	check(watermark, []any{int64(1), int64(2), int64(5), int64(6), nil})
	query(`MATCH (s:ProductStore {store:'example.test'}) SET s.seen_seq=1`, nil)
	baseline.AssertUnchanged(t, "no-op except seen_seq")
	query(`MATCH (s:ProductStore {store:'example.test'}) SET s.seen_seq=2`, nil)

	// Remove a target revision so a new snapshot fails after store deletion and product creation.
	query(`MATCH (f:FeatureRev {id:'code'}) SET f.key='fault/key'`, nil)
	faulted := graphtest.Snapshot(t)
	productCLI(t, 1, "feature", args("wide-snapshot", "3", writeProductFixture(t, dir, "changed.jsonl",
		strings.ReplaceAll(strings.Join(rows, ""), "Maker", "Different")))...)
	faulted.AssertUnchanged(t, "failed replacement")
	query(`MATCH (f:FeatureRev {id:'code'}) SET f.key=$key`, map[string]any{"key": digest[:16] + "/code"})

	// Both calls have the same sequence and different payloads, so exactly one
	// can write even if the two transactions arrive consecutively.
	left := writeProductFixture(t, dir, "left.jsonl", strings.NewReplacer("Maker", "Left", "2 cm", "4 cm").Replace(strings.Join(rows, "")))
	right := writeProductFixture(t, dir, "right.jsonl", strings.NewReplacer("Maker", "Right", "2 cm", "6 cm").Replace(strings.Join(rows, "")))
	outcomes := concurrentProductCLI(
		[]string{"products", "import", "-store", "example.test", "-release", "wide-snapshot", "-snapshot-seq", "3", "-report", report + ".left", left},
		[]string{"products", "import", "-store", "example.test", "-release", "wide-snapshot", "-snapshot-seq", "3", "-report", report + ".right", right})
	winnerIndex, loser := productRaceWinner(t, outcomes)
	if strings.Contains(loser.message, "stale snapshot sequence 3 (seen_seq=3, wiped_seq=0)") {
		if loser.code != 5 {
			t.Fatalf("stale import exited %d, want 5: %s", loser.code, loser.message)
		}
		t.Log("same-store loser: stale sequence, exit 5")
	} else {
		assertProductImportWriteConflict(t, loser)
		t.Log("same-store loser: write conflict, exit 1")
	}
	winner, winnerLength := "Left", "4 cm"
	if winnerIndex == 1 {
		winner, winnerLength = "Right", "6 cm"
	}
	check(`MATCH (p:Product {store:'example.test'}) RETURN DISTINCT p.brand`, []any{winner})
	check(`MATCH (:Product {product_id:'example.test:1'})-[e:HAS_FEATURE {source_key:'length'}]->() RETURN e.raw`, []any{winnerLength})
	check(watermark, []any{int64(3), int64(3), int64(5), int64(6), nil})

	otherFile := writeProductFixture(t, dir, "other.jsonl", snapshotRecord("z.example.test:1", "", `{"length":"1 mm"}`))
	otherArgs := []string{"products", "import", "-store", "z.example.test", "-release", "wide-snapshot",
		"-snapshot-seq", "1", otherFile, "-report", report}
	const otherWatermark = `MATCH (s:ProductStore {store:'z.example.test'}) RETURN s.product_count,s.edge_count`
	replacement := []string{right, left}[winnerIndex]
	t.Run("different_stores", func(t *testing.T) {
		input := graph.ProductSnapshotInput{Store: "example.test", Release: "wide-snapshot", Seq: 4}
		firstReport := startProductImportReport(input)
		// At report staging the first import still holds the release pointer write.
		// The second runs through the CLI on a separate connection before it commits.
		err := importProducts(t.Context(), input, replacement, &firstReport, func() error {
			assertProductImportWriteConflict(t, runProductCLI(otherArgs))
			return nil
		}, func(ctx context.Context, run func(context.Context, *graph.Store) error) error {
			return run(ctx, st)
		})
		if err != nil || firstReport.Status != "imported" {
			t.Fatalf("first import = %s, %v; want imported", firstReport.Status, err)
		}
		check(otherWatermark, []any{int64(0), int64(0)})
		check(`MATCH (p:Product {store:'z.example.test'}) RETURN p.id`)
		productCLI(t, 0, "imported", otherArgs...)
		check(otherWatermark, []any{int64(1), int64(1)})
		t.Log("different-store loser: write conflict, exit 1; same-sequence rerun succeeded")
	})
	narrow := create("narrow-snapshot", length)
	productCLI(t, 0, "activated", "products", "release", "activate", "narrow-snapshot")
	check(watermark, []any{int64(4), int64(4), int64(5), int64(4), nil})
	check(otherWatermark, []any{int64(1), int64(0)})
	check(`MATCH ()-[e:HAS_FEATURE]->() RETURN DISTINCT e.source_key`, []any{"length"})
	cypher, err := st.RunReadQuery(t.Context(), `MATCH ()-[e:HAS_FEATURE]->() RETURN DISTINCT e.source_key AS source_key`, nil, 0)
	if err != nil || !reflect.DeepEqual(cypher.Rows, []map[string]any{{"source_key": "length"}}) {
		t.Fatalf("shared Cypher read returned %+v, %v", cypher, err)
	}
	// Reactivation can restore release eligibility but cannot restore pruned edges.
	productCLI(t, 0, "imported", "products", "release", "activate", "wide-snapshot", "-materialize", "example.test=5:"+replacement)
	check(watermark, []any{int64(5), int64(5), int64(5), int64(6), nil})
	productCLI(t, 5, "stale", "products", "release", "activate", "narrow-snapshot",
		"-materialize", "example.test=5:"+shrink, "-materialize", "example.test=6:"+replacement)
	check(`MATCH (m:GkgMeta {id:'product_release'}) RETURN m.value`, []any{"narrow-snapshot"})
	check(watermark, []any{int64(5), int64(5), int64(5), int64(4), nil})
	productCLI(t, 0, "imported", args("narrow-snapshot", "6", shrink, "-allow-shrink")...)
	readReport()
	if !importReport.ShrinkAllowed || importReport.PreviousProductCount != 5 || importReport.ProductCount != 4 {
		t.Fatalf("shrink override report: %+v", importReport)
	}
	check(watermark, []any{int64(6), int64(6), int64(4), int64(4), nil})
	check(`MATCH ()-[e:HAS_FEATURE]->() RETURN DISTINCT e.source_key,e.release_id`, []any{"length", "narrow-snapshot"})
	assertProductSnapshotEmit(t, query, registryFile, narrow, "narrow-snapshot", shrink)

	productCLI(t, 0, "unchanged", args("narrow-snapshot", "7", shrink)...)
	productCLI(t, 0, "deleted", "products", "delete-store", "example.test")
	const tombstone = `MATCH (s:ProductStore {store:'example.test'})
		RETURN s.domain,s.snapshot_seq,s.seen_seq,s.wiped_seq,s.snapshot_digest,s.release_id,s.imported_at,s.product_count,s.edge_count`
	check(tombstone, []any{"snapshot-test", int64(6), int64(7), int64(7), nil, nil, nil, int64(0), int64(0)})
	check(`MATCH (p:Product {store:'example.test'}) RETURN p.id`)
	productCLI(t, 5, "stale snapshot sequence 7 (seen_seq=7, wiped_seq=7)", args("narrow-snapshot", "7", shrink)...)
	productCLI(t, 0, "imported", args("narrow-snapshot", "8", shrink)...)
	check(watermark, []any{int64(8), int64(8), int64(4), int64(4), int64(7)})
	productCLI(t, 0, "deleted", "products", "delete-store", "example.test")
	productCLI(t, 0, "unbound", "products", "unbind", "example.test")
	productCLI(t, 5, "binding", args("narrow-snapshot", "9", shrink)...)
}

type productCLIOutcome struct {
	code    int
	message string
}

func runProductCLI(args []string) productCLIOutcome {
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return productCLIOutcome{code, out.String() + errOut.String()}
}

func concurrentProductCLI(a, b []string) [2]productCLIOutcome {
	start := make(chan struct{})
	var done [2]chan productCLIOutcome
	for i, args := range [][]string{a, b} {
		done[i] = make(chan productCLIOutcome, 1)
		go func(result chan<- productCLIOutcome) {
			<-start
			result <- runProductCLI(args)
		}(done[i])
	}
	close(start)
	return [2]productCLIOutcome{<-done[0], <-done[1]}
}

func productRaceWinner(t *testing.T, outcomes [2]productCLIOutcome) (int, productCLIOutcome) {
	t.Helper()
	if (outcomes[0].code == 0) == (outcomes[1].code == 0) {
		t.Fatalf("concurrent outcomes = %+v, want exactly one success", outcomes)
	}
	if outcomes[0].code == 0 {
		return 0, outcomes[1]
	}
	return 1, outcomes[0]
}

func assertProductImportWriteConflict(t *testing.T, loser productCLIOutcome) {
	t.Helper()
	if loser.code != 1 || !strings.Contains(loser.message, "conflicting transactions") {
		t.Fatalf("losing import = exit %d, %q; want exit 1 for a write conflict", loser.code, loser.message)
	}
}

func TestConcurrentProductFeatureRegistrySemantics(t *testing.T) {
	_, _, query := productTestDB(t)
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n:FeatureRegistry OR n:FeatureRev OR (n:GkgMeta AND n.id='feature_registry_import') DETACH DELETE n`, nil)
	})
	dir := t.TempDir()
	changed := strings.Replace(snapshotRegistry, "Nominal length", "Maximum length", 1)
	first := writeProductFixture(t, dir, "first.json", snapshotRegistry)
	second := writeProductFixture(t, dir, "second.json", changed)
	outcomes := concurrentProductCLI([]string{"features", "import", first}, []string{"features", "import", second})
	winnerIndex, failed := productRaceWinner(t, outcomes)
	if failed.code != 1 && failed.code != 5 {
		t.Fatalf("losing registry import = exit %d, %q; want exit 1 or 5", failed.code, failed.message)
	}
	winner, loser := snapshotRegistry, second
	if winnerIndex == 1 {
		winner, loser = changed, first
	}
	rows := query(`MATCH (r:FeatureRegistry) RETURN r.rev`, nil)
	if len(rows) != 1 || rows[0].Values[0] != productFixtureDigest(winner)[:16] {
		t.Fatalf("unexpected committed registries: %v", rows)
	}
	rows = query(`MATCH (f:FeatureRev {id:'length'}) RETURN f.rev`, nil)
	if len(rows) != 1 || rows[0].Values[0] != productFixtureDigest(winner)[:16] {
		t.Fatalf("unexpected committed revisions: %v", rows)
	}
	productCLI(t, 5, "definition_digest", "features", "import", loser)
}

func assertProductSnapshotEmit(t *testing.T, query func(string, map[string]any) []*neo4j.Record, registryPath, releasePath, releaseID, file string) {
	t.Helper()
	stream := productCLI(t, 0, `"type":"obs"`, "products", "emit", "-registry", registryPath, "-release", releasePath, file)
	registry, err := readProductRegistry(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	releaseBytes, err := os.ReadFile(releasePath)
	if err != nil {
		t.Fatal(err)
	}
	release, err := registry.ParseRelease(releaseBytes)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	records, err := productobs.ReadRecords(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	indexes := make(map[[3]string]map[string]any)
	for _, record := range records {
		for _, obs := range registry.Observations(record, release) {
			mapping, _ := registry.Lookup(record.Store, obs.SourceKey)
			props := map[string]any{"release_id": releaseID, "numeric": mapping.NumericSearch,
				"value_fold": nil, "items_fold": nil, "lo_scaled": nil, "hi_scaled": nil}
			if folds := obs.ItemsFolds(); folds != nil {
				items := make([]any, len(folds))
				for i, fold := range folds {
					items[i] = fold
				}
				props["items_fold"] = items
			} else {
				props["value_fold"] = obs.ValueFold()
			}
			typed := obs.Typed
			if mapping.NumericSearch && typed != nil && typed.Status == "ok" && typed.Qualifier != nil && *typed.Qualifier == "exact" {
				props["lo_scaled"] = expectedProductScaled(t, typed.LoCanonical)
				props["hi_scaled"] = expectedProductScaled(t, typed.HiCanonical)
			}
			indexes[[3]string{obs.ProductID, obs.Feature, obs.SourceKey}] = props
		}
	}

	read := func(line string) map[string]any {
		t.Helper()
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	var want []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(stream, "\n"), "\n") {
		if obj := read(line); obj["type"] == "obs" {
			key := [3]string{obj["product_id"].(string), obj["feature"].(string), obj["source_key"].(string)}
			obj["index"] = indexes[key]
			want = append(want, obj)
		}
	}
	var got []map[string]any
	for _, row := range query(`MATCH (p:Product)-[e:HAS_FEATURE]->(f:FeatureRev)
	    RETURN p.product_id,f.id,properties(e) ORDER BY p.product_id,f.id,e.source_key`, nil) {
		props := row.Values[2].(map[string]any)
		var items any
		if raw, ok := props["items_json"].(string); ok {
			if err := json.Unmarshal([]byte(raw), &items); err != nil {
				t.Fatal(err)
			}
		}
		var typed any
		if props["status"] != nil {
			fields := make(map[string]any)
			for _, key := range strings.Fields("canonical_unit hi hi_canonical inclusive lo lo_canonical qualifier status unit_in") {
				fields[key] = props[key]
			}
			typed = fields
		}
		index := make(map[string]any)
		for _, key := range strings.Fields("release_id numeric value_fold items_fold lo_scaled hi_scaled") {
			index[key] = props[key]
		}
		got = append(got, map[string]any{"product_id": row.Values[0], "feature": row.Values[1],
			"type": "obs", "source_key": props["source_key"], "raw": props["raw"], "items": items,
			"split_status": props["split_status"], "typed": typed, "conflict": props["conflict"], "index": index})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("graph observations = %+v; emitted observations = %+v", got, want)
	}
}

func expectedProductScaled(t *testing.T, value *string) any {
	t.Helper()
	if value == nil {
		return nil
	}
	decimal, ok := new(big.Rat).SetString(*value)
	if !ok {
		t.Fatalf("invalid canonical decimal %q", *value)
	}
	scaled := decimal.Mul(decimal, big.NewRat(1_000_000, 1))
	if scaled.Sign() < 0 || !scaled.IsInt() || !scaled.Num().IsInt64() {
		return nil
	}
	return scaled.Num().Int64()
}
