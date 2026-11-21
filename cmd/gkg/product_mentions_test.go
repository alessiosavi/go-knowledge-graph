package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProductMentionsScanUsage(t *testing.T) {
	productCLI(t, 0, "products mentions-scan", "products", "-h")
	productCLI(t, 0, "products mentions-scan", "products", "mentions-scan", "-h")
	for _, args := range [][]string{
		{"-out", "out.jsonl"},
		{"-doc", "id", "-domain", "d", "-out", "out.jsonl"},
		{"-doc", "id"},
		{"-doc", "id", "-source-prefix", "docs/", "-out", "out.jsonl"},
		{"-doc", "", "-out", "out.jsonl"},
	} {
		productCLI(t, 2, "usage: gkg products mentions-scan", append([]string{"products", "mentions-scan"}, args...)...)
	}
}

func TestProductMentionsScan(t *testing.T) {
	_, _, query := productTestDB(t)
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n.id STARTS WITH 'mentions-' OR n.store STARTS WITH 'mentions-'
			OR (n:GkgMeta AND n.id='product_release') DETACH DELETE n`, nil)
	})
	dir := t.TempDir()
	out, summaryPath := filepath.Join(dir, "pairs.jsonl"), filepath.Join(dir, "summary.json")
	baseArgs := []string{"products", "mentions-scan", "-out", out, "-summary", summaryPath}
	productCLI(t, 1, "no active product release", append(baseArgs, "-domain", "catalogue-mentions")...)
	query(`CREATE (:GkgMeta {id:'product_release',value:'mentions-release',activation_seq:12,touched:7}),
		(:ProductRelease {id:'mentions-release'}),
		(:ProductStore {store:'mentions-a.test',domain:'mentions-products',release_id:'mentions-release',seen_seq:8,snapshot_seq:6,wiped_seq:4,product_count:2}),
		(:ProductStore {store:'mentions-b.test',domain:'mentions-other-products',release_id:'mentions-release',seen_seq:11,snapshot_seq:11,product_count:3}),
		(:ProductStore {store:'mentions-old.test',domain:'mentions-products',release_id:'mentions-old-release',seen_seq:5,snapshot_seq:5,product_count:1}),
		(:ProductStore {store:'mentions-unbound.test',seen_seq:20,snapshot_seq:19,product_count:1}),
		(a:Product {id:'mentions-product-a',product_id:'mentions-a.test:2',store:'mentions-a.test',domain:'mentions-products',title:'Handpiece'}),
		(b:Product {id:'mentions-product-b',product_id:'mentions-b.test:1',store:'mentions-b.test',domain:'mentions-other-products',title:'Mirror'}),
		(ba:Product {id:'mentions-product-b-ambiguous',product_id:'mentions-b.test:2',store:'mentions-b.test',domain:'mentions-other-products'}),
		(bi:Product {id:'mentions-product-b-ineligible',product_id:'mentions-b.test:3',store:'mentions-b.test',domain:'mentions-other-products'}),
		(c:Product {id:'mentions-product-c',product_id:'mentions-a.test:1',store:'mentions-a.test',domain:'mentions-products',title:'Handle'}),
		(o:Product {id:'mentions-product-old',product_id:'mentions-old.test:1',store:'mentions-old.test',domain:'mentions-products'}),
		(u:Product {id:'mentions-product-u',product_id:'mentions-unbound.test:1',store:'mentions-unbound.test',title:'Unbound'}),
		(code:FeatureRev {id:'catalogue_code',key:'mentions-registry/catalogue_code',kind:'code'}),
		(ref:FeatureRev {id:'product_reference',key:'mentions-registry/product_reference',kind:'code'}),
		(iso:FeatureRev {id:'iso_size',key:'mentions-registry/iso_size',kind:'code'}),
		(a)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'AB-12345'}]->(code),
		(a)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'AB-12345'}]->(code),
		(a)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'SH-12345'}]->(code),
		(a)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'ABCDE'}]->(code),
		(a)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'ABCDE'}]->(ref),
		(a)-[:HAS_FEATURE {release_id:'mentions-release',items_fold:['ZX_98765','AB12345','ZX_98765','12345','12345']}]->(ref),
		(a)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'ISO12345'}]->(iso),
		(a)-[:HAS_FEATURE {release_id:'mentions-old-release',value_fold:'OLD12345'}]->(code),
		(ba)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'SH12345'}]->(code),
		(bi)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'ABCDE'}]->(code),
		(b)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'BC12345'}]->(ref),
		(c)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'AA12345'}]->(code),
		(o)-[:HAS_FEATURE {release_id:'mentions-old-release',value_fold:'OLD12345'}]->(code),
		(u)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'AB12345'}]->(code),
		(u)-[:HAS_FEATURE {release_id:'mentions-release',value_fold:'NO12345'}]->(code)`, nil)
	t.Cleanup(func() {
		query(`MATCH (f:FeatureRev) WHERE f.key STARTS WITH 'mentions-registry/' DETACH DELETE f`, nil)
	})
	prefix := strings.Repeat("界", 100) + " "
	chunkText := prefix + "AB-12345., ZX 98765; SH12345; ISO12345; OLD12345; NO12345; " + strings.Repeat("界", 100)
	query(`CREATE (a:Document {id:'mentions-doc-a',source:'catalogue/a',title:'Catalogue A',hash:'doc-a-hash',domain:'catalogue-mentions',status:'done'}),
		(a)-[:HAS_CHUNK]->(:Chunk {id:'mentions-chunk-a1',seq:1,text:'ZX_98765',hash:'a1-hash'}),
		(a)-[:HAS_CHUNK]->(a0:Chunk {id:'mentions-chunk-a0',seq:0,text:$chunkText,hash:'a0-hash'}),
		(a)-[:HAS_CHUNK]->(a0),
		(a2:Document {id:'mentions-doc-a2',source:'catalogue/a',title:'Catalogue A2',hash:'doc-a2-hash',domain:'catalogue-mentions',status:'done'}),
		(a2)-[:HAS_CHUNK]->(:Chunk {id:'mentions-chunk-a2',seq:0,text:'AA12345 BC12345',hash:'a2-hash'}),
		(b:Document {id:'mentions-doc-b',source:'catalogue/b',title:'Catalogue B',hash:'doc-b-hash',domain:'other-catalogue',status:'done'}),
		(b)-[:HAS_CHUNK]->(:Chunk {id:'mentions-chunk-b',seq:0,text:'BC12345',hash:'b-hash'}),
		(c:Document {id:'mentions-doc-c',source:'other/c',title:'Catalogue C',hash:'doc-c-hash',domain:'catalogue-mentions',status:'done'}),
		(c)-[:HAS_CHUNK]->(:Chunk {id:'mentions-chunk-c',seq:0,text:'AA12345',hash:'c-hash'}),
		(p:Document {id:'mentions-doc-pending',source:'catalogue/pending',title:'Pending',hash:'doc-p-hash',domain:'catalogue-mentions',status:'fetched'}),
		(p)-[:HAS_CHUNK]->(:Chunk {id:'mentions-chunk-pending',seq:0,text:'AB12345',hash:'pending-hash'}),
		(s:Document {id:'mentions-doc-superseded',source:'catalogue/superseded',hash:'doc-s-hash',domain:'catalogue-mentions',status:'superseded'}),
		(s)-[:HAS_CHUNK]->(:Chunk {id:'mentions-chunk-superseded',seq:0,text:'AB12345',hash:'superseded-hash'}),
		(:Document {id:'mentions-doc-empty',source:'catalogue/z',title:'Empty',hash:'doc-empty-hash',domain:'catalogue-mentions',status:'done'})`,
		map[string]any{"chunkText": chunkText})

	snapshot := func() []any {
		t.Helper()
		state := []any{}
		for _, q := range []string{
			`MATCH (n) RETURN count(n)`,
			`MATCH ()-[r]->() RETURN count(r)`,
			`MATCH (n:GkgMeta) RETURN properties(n) ORDER BY n.id`,
			`MATCH (n:ProductStore) RETURN properties(n) ORDER BY n.store`,
		} {
			for _, row := range query(q, nil) {
				state = append(state, row.Values)
			}
		}
		return state
	}
	before := snapshot()
	readPrivate := func(t *testing.T, path string) []byte {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("%s mode = %o, want 0600", path, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	readPairs := func(data []byte) []productReadJSON {
		t.Helper()
		var pairs []productReadJSON
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			pairs = append(pairs, readProductJSON(t, line))
		}
		return pairs
	}
	checkJSON := func(t *testing.T, got any, want string) {
		t.Helper()
		var decoded any
		if err := json.Unmarshal([]byte(want), &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, decoded) {
			t.Errorf("got %#v, want %s", got, want)
		}
	}
	t.Run("invalid_domain", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "pairs.jsonl")
		productCLI(t, 2, "usage: gkg products mentions-scan", "products", "mentions-scan", "-domain", "Dental", "-out", out)
		if _, err := os.Lstat(out); !os.IsNotExist(err) {
			t.Errorf("invalid domain output: %v, want no file", err)
		}
	})
	t.Run("empty_selection", func(t *testing.T) {
		dir := t.TempDir()
		out, summary := filepath.Join(dir, "pairs.jsonl"), filepath.Join(dir, "summary.json")
		var stdout, stderr bytes.Buffer
		args := []string{"products", "mentions-scan", "-domain", "catalogue-mentions", "-source-prefix", "missing/",
			"-out", out, "-summary", summary}
		if code := run(args, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "warning: no documents selected") {
			t.Errorf("empty selection exit %d, stderr %q, want success with warning", code, stderr.String())
		}
		if data := readPrivate(t, out); len(data) != 0 {
			t.Errorf("empty selection produced pairs: %s", data)
		}
		result := readProductJSON(t, readPrivate(t, summary))
		if docs, ok := result["documents"].([]any); !ok || len(docs) != 0 {
			t.Errorf("empty selection documents: %v", result["documents"])
		}
	})
	for _, tc := range []struct {
		name, out, summary, diagnostic string
	}{
		{"case_alias", "pairs.jsonl", "PAIRS.jsonl", "must name different files"},
		{"same_file", "pairs.jsonl", "pairs.jsonl", "must name different files"},
		{"output_directory", ".", "summary.json", "-out must name a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			var stdout, stderr bytes.Buffer
			args := []string{"products", "mentions-scan", "-domain", "catalogue-mentions", "-out", tc.out, "-summary", tc.summary}
			if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.diagnostic) {
				t.Errorf("invalid output exit %d, stderr %q, want exit 1 and %q", code, stderr.String(), tc.diagnostic)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Errorf("invalid output left files: %v, error %v", entries, err)
			}
		})
	}

	productCLI(t, 0, "", append(baseArgs, "-doc", "mentions-doc-b", "-doc", "mentions-doc-b")...)
	pairs := readPairs(readPrivate(t, out))
	if len(pairs) != 1 || pairs[0]["doc_id"] != "mentions-doc-b" || pairs[0]["id"] != "mentions-product-b" {
		t.Fatalf("explicit scan pairs: %v", pairs)
	}
	for _, tc := range []struct{ id, diagnostic string }{
		{"mentions-doc-pending", "not done"},
		{"mentions-doc-missing", "not found"},
	} {
		previous, previousSummary := readPrivate(t, out), readPrivate(t, summaryPath)
		productCLI(t, 1, tc.diagnostic, append(baseArgs, "-doc", "mentions-doc-b", "-doc", tc.id)...)
		if !bytes.Equal(previous, readPrivate(t, out)) || !bytes.Equal(previousSummary, readPrivate(t, summaryPath)) {
			t.Error("invalid selector replaced an output file")
		}
	}
	if err := os.Chmod(out, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(summaryPath, 0644); err != nil {
		t.Fatal(err)
	}
	domainArgs := append(baseArgs, "-domain", "catalogue-mentions", "-source-prefix", "catalogue/")
	productCLI(t, 0, "", domainArgs...)
	pairBytes, summaryBytes := readPrivate(t, out), readPrivate(t, summaryPath)
	productCLI(t, 0, "", domainArgs...)
	if !bytes.Equal(pairBytes, readPrivate(t, out)) || !bytes.Equal(summaryBytes, readPrivate(t, summaryPath)) {
		t.Error("repeated scans produced different bytes")
	}

	pairs = readPairs(pairBytes)
	if len(pairs) != 4 {
		t.Fatalf("domain scan pairs: %v", pairs)
	}
	for i, want := range [][2]string{
		{"mentions-product-c", "mentions-chunk-a2"}, {"mentions-product-a", "mentions-chunk-a0"},
		{"mentions-product-b", "mentions-chunk-a2"}, {"mentions-product-a", "mentions-chunk-a1"},
	} {
		if pairs[i]["id"] != want[0] || pairs[i]["chunk_id"] != want[1] {
			t.Errorf("pair %d = %v, want %v", i, pairs[i], want)
		}
	}
	pair := pairs[1]
	checkJSON(t, pair["features"], `["catalogue_code","product_reference"]`)
	if pair["product_id"] != "mentions-a.test:2" || pair["store"] != "mentions-a.test" ||
		pair["product_domain"] != "mentions-products" || pair["title"] != "Handpiece" ||
		pair["doc_id"] != "mentions-doc-a" || pair["doc_source"] != "catalogue/a" ||
		pair["doc_title"] != "Catalogue A" || pair["chunk_seq"] != float64(0) ||
		pair["separator_run"] != true || pairs[2]["separator_run"] != false {
		t.Errorf("pair metadata: %v", pair)
	}
	checkJSON(t, pair["matches"], `[
		{"key":"AB12345","feature":"catalogue_code","value_fold":"AB-12345","span":[301,310],"text":"AB-12345.","tokens":1},
		{"key":"AB12345","feature":"product_reference","value_fold":"AB12345","span":[301,310],"text":"AB-12345.","tokens":1},
		{"key":"ZX98765","feature":"product_reference","value_fold":"ZX_98765","span":[312,320],"text":"ZX 98765","tokens":2}
	]`)
	excerpt, _ := pair["excerpt"].(string)
	if !utf8.ValidString(excerpt) || len(excerpt) < 390 || len(excerpt) > 409 ||
		!strings.Contains(chunkText, excerpt) || !strings.Contains(excerpt, "AB-12345.") {
		t.Errorf("invalid excerpt: %q", excerpt)
	}

	summary := readProductJSON(t, summaryBytes)
	if summary["matcher_revision"] != "codekey-v1/window3" || summary["release"] != "mentions-release" ||
		summary["activation_seq"] != float64(12) {
		t.Errorf("summary metadata: %v", summary)
	}
	t.Run("store_watermarks", func(t *testing.T) {
		checkJSON(t, summary["stores"], `[
			{"store":"mentions-a.test","domain":"mentions-products","release_id":"mentions-release","seen_seq":8,"snapshot_seq":6,
			 "wiped_seq":4,"product_count":2,"identifier_products":2,"eligible_products":2},
			{"store":"mentions-b.test","domain":"mentions-other-products","release_id":"mentions-release","seen_seq":11,"snapshot_seq":11,
			 "wiped_seq":0,"product_count":3,"identifier_products":3,"eligible_products":2},
			{"store":"mentions-old.test","domain":"mentions-products","release_id":"mentions-old-release","seen_seq":5,"snapshot_seq":5,
			 "wiped_seq":0,"product_count":1,"identifier_products":0,"eligible_products":0}
		]`)
	})
	t.Run("distinct_values", func(t *testing.T) {
		checkJSON(t, summary["counts"], `{"identifier_values":11,"eligible_keys":5,"ineligible_values":4,
			"ambiguous_keys":1,"pairs":4,"chunks_with_pairs":3,"products_with_pairs":3}`)
		divergence, _ := summary["divergence"].(map[string]any)
		if examples, ok := divergence["examples"].([]any); divergence["count"] != float64(3) || !ok || len(examples) != 3 {
			t.Errorf("divergence: %v", divergence)
		}
	})
	checkJSON(t, summary["ambiguous"], `[{"key":"SH12345","products":[
		{"id":"mentions-product-a","store":"mentions-a.test"},
		{"id":"mentions-product-b-ambiguous","store":"mentions-b.test"}
	]}]`)
	docs, _ := summary["documents"].([]any)
	aHash := productFixtureDigest("mentions-chunk-a0\ta0-hash\nmentions-chunk-a1\ta1-hash\n")
	a2Hash := productFixtureDigest("mentions-chunk-a2\ta2-hash\n")
	if len(docs) != 3 {
		t.Fatalf("documents: %v", docs)
	}
	for i, want := range []struct {
		id, source, hash, chunksHash string
		count                        int
	}{
		{"mentions-doc-a", "catalogue/a", "doc-a-hash", aHash, 2},
		{"mentions-doc-a2", "catalogue/a", "doc-a2-hash", a2Hash, 1},
		{"mentions-doc-empty", "catalogue/z", "doc-empty-hash", productFixtureDigest(""), 0},
	} {
		doc := docs[i].(map[string]any)
		if doc["id"] != want.id || doc["source"] != want.source || doc["hash"] != want.hash ||
			doc["chunks_sha256"] != want.chunksHash || doc["chunk_count"] != float64(want.count) {
			t.Errorf("document %d = %v, want %+v", i, doc, want)
		}
	}
	domainOut := filepath.Join(dir, "domain.jsonl")
	productCLI(t, 0, "", "products", "mentions-scan", "-domain", "catalogue-mentions", "-out", domainOut)
	domainPairs := readPairs(readPrivate(t, domainOut))
	if len(domainPairs) != 5 || domainPairs[4]["doc_id"] != "mentions-doc-c" {
		t.Errorf("domain scan without prefix: %v", domainPairs)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Errorf("scan changed graph state: before %v, after %v", before, after)
	}
}
