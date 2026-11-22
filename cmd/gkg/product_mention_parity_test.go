package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestProductMentionsParityUsage(t *testing.T) {
	productCLI(t, 0, "products mentions-parity", "products", "-h")
	productCLI(t, 0, "products mentions-parity", "products", "mentions-parity", "-h")
	productCLI(t, 2, "usage: gkg products mentions-parity", "products", "mentions-parity", "-summary", "summary.json")
}

func TestProductMentionsParity(t *testing.T) {
	st, _, query := productTestDB(t)
	ctx := t.Context()
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n.id STARTS WITH 'parity-' OR n.store STARTS WITH 'parity-' OR n.key STARTS WITH 'parity-'
			OR (n:GkgMeta AND n.id='product_release' AND n.value='parity-release') DETACH DELETE n`, nil)
	})
	query(`CREATE (:GkgMeta {id:'product_release',value:'parity-release',activation_seq:12}),
		(:ProductStore {store:'parity-a.test',domain:'parity-products',release_id:'parity-release',seen_seq:8,snapshot_seq:6,wiped_seq:4,materialized_activation_seq:12,product_count:2}),
		(:ProductStore {store:'parity-b.test',domain:'parity-other-products',release_id:'parity-release',seen_seq:11,snapshot_seq:11,materialized_activation_seq:12,product_count:1}),
		(a:Product {id:'parity-product-a',product_id:'parity-a.test:1',store:'parity-a.test',domain:'parity-products',title:'Handpiece'}),
		(n:Product {id:'parity-product-negative',product_id:'parity-a.test:2',store:'parity-a.test',domain:'parity-products',title:'Negative Control'}),
		(b:Product {id:'parity-product-b',product_id:'parity-b.test:1',store:'parity-b.test',domain:'parity-other-products',title:'Mirror'}),
		(code:FeatureRev {id:'catalogue_code',key:'parity-code',kind:'code'}),
		(ref:FeatureRev {id:'product_reference',key:'parity-reference',kind:'code'}),
		(a)-[:HAS_FEATURE {release_id:'parity-release',value_fold:'AB-12345'}]->(code),
		(a)-[:HAS_FEATURE {release_id:'parity-release',value_fold:'ZX_98765'}]->(ref),
		(n)-[:HAS_FEATURE {release_id:'parity-release',value_fold:'NE12345'}]->(code),
		(b)-[:HAS_FEATURE {release_id:'parity-release',value_fold:'BC12345'}]->(ref)`, nil)
	seed := func(source, scope string, texts ...string) string {
		t.Helper()
		doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Source: source, Title: source, Kind: "text", Domain: scope, Hash: productFixtureDigest(strings.Join(texts, "\n"))})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := st.DeleteDocument(context.Background(), doc.ID); err != nil {
				t.Error(err)
			}
		})
		chunks := make([]graph.Chunk, len(texts))
		for i, text := range texts {
			chunks[i] = graph.Chunk{Seq: i, Text: text, Hash: productFixtureDigest(text)}
		}
		if err := st.PutChunks(ctx, doc.ID, chunks); err != nil {
			t.Fatal(err)
		}
		if err := st.SetStatus(ctx, doc.ID, graph.StatusDone, ""); err != nil {
			t.Fatal(err)
		}
		return doc.ID
	}
	const firstText = "AB-12345, ZX 98765, BC12345"
	first := seed("parity/catalogue/a", "parity-docs", firstText, "ab12345")
	second := seed("parity/catalogue/b", "parity-docs", "BC12345")
	seed("parity/outside", "parity-outside", "NE12345")
	dir := t.TempDir()
	pairsPath, summaryPath := filepath.Join(dir, "pairs.jsonl"), filepath.Join(dir, "summary.json")
	productCLI(t, 0, "", "products", "mentions-scan", "-doc", first, "-doc", second, "-out", pairsPath, "-summary", summaryPath)
	read := func(path string) []byte {
		t.Helper()
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	for path, golden := range map[string]string{pairsPath: "product_mention_parity_pairs.jsonl", summaryPath: "product_mention_parity_summary.json"} {
		actual := read(path)
		if !bytes.Equal(actual, read(filepath.Join("testdata", golden))) {
			t.Fatalf("%s differs from the ingest-hash golden", golden)
		}
	}
	digests := func(summary, pairs string) []string {
		return []string{"products", "mentions-parity", "-summary", summary, "-pairs", pairs,
			"-summary-sha256", productFixtureDigest(string(read(summary))), "-pairs-sha256", productFixtureDigest(string(read(pairs)))}
	}
	base := digests(summaryPath, pairsPath)
	outPath := filepath.Join(dir, "report.json")
	output := productCLI(t, 0, `"status": "PASS"`, append(slices.Clone(base), "-out", outPath)...)
	if !bytes.Equal([]byte(output), read(outPath)) {
		t.Fatal("parity report differs between stdout and -out")
	}
	t.Log(output)
	report := readProductJSON(t, []byte(output))
	if report["negative_controls"] != float64(1) || report["products"] != float64(3) ||
		report["pairs"].(map[string]any)["expected"] != float64(4) ||
		report["entries"].(map[string]any)["expected"] != float64(5) {
		t.Fatalf("parity report: %s", output)
	}
	// Keep the refusal and FAIL diagnostics in the test log for the acceptance run.
	runRejected := func(want string, args ...string) string {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := run(args, &out, &stderr); code != 1 || !strings.Contains(out.String()+stderr.String(), want) {
			t.Fatalf("exit %d, stdout %s, stderr %s, expected %q", code, &out, &stderr, want)
		}
		t.Logf("exit 1: %s", strings.TrimSpace(stderr.String()))
		return out.String()
	}
	bad := strings.Repeat("0", 64)
	for _, tc := range []struct {
		i    int
		flag string
	}{{7, "summary-sha256"}, {9, "pairs-sha256"}} {
		args := slices.Clone(base)
		args[tc.i] = bad
		runRejected("REFUSED: -"+tc.flag, args...)
	}
	malformed := bytes.Replace(read(summaryPath), []byte(`"release_id":"parity-release"`), []byte(`"release_id":42`), 1)
	malformedPath := writeProductFixture(t, dir, "malformed-summary.json", string(malformed))
	if got := runRejected("REFUSED: store parity-a.test release_id:", digests(malformedPath, pairsPath)...); got != "" {
		t.Fatalf("malformed summary wrote a report: %s", got)
	}
	requirePreflightFail := func(want string, args ...string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "rejected-report.json")
		output := runRejected("FAIL: pre-read frozen metadata: "+want, append(slices.Clone(args), "-out", path)...)
		var report productParityReport
		if err := json.Unmarshal([]byte(output), &report); err != nil {
			t.Fatalf("decode FAIL report: %v, stdout %q", err, output)
		}
		if report.Status != "FAIL" || report.Reads != 0 || len(report.Failures) != 1 || !strings.Contains(report.Failures[0], want) {
			t.Fatalf("expected preflight FAIL without product reads: %s", output)
		}
		if !bytes.Equal([]byte(output), read(path)) {
			t.Fatal("preflight FAIL report differs between stdout and -out")
		}
	}
	const active = `MATCH (n:GkgMeta {id:'product_release'})`
	const bound = `MATCH (n:ProductStore {store:'parity-a.test'})`
	const negative = `MATCH (:Product {id:'parity-product-negative'})-[n:HAS_FEATURE]->()`
	const firstDocument = `MATCH (n:Document {source:'parity/catalogue/a'})`
	for _, tc := range []struct {
		match, field, want string
		frozen, changed    any
	}{
		{active, "value", "release:", "parity-release", "parity-changed"},
		{active, "value", "no active product release", "parity-release", nil},
		{active, "activation_seq", "activation_seq:", 12, 13},
		{bound, "seen_seq", "store parity-a.test seen_seq:", 8, 9},
		{bound, "release_id", "store parity-a.test release_id differs", "parity-release", nil},
		{firstDocument, "status", "document \"" + first + "\" (status \"pending\"):", "done", "pending"},
		{negative, "value_fold", "summary identifier_values", "NE12345", nil},
	} {
		set := tc.match + " SET n." + tc.field + "=$value"
		query(set, map[string]any{"value": tc.changed})
		requirePreflightFail(tc.want, base...)
		query(set, map[string]any{"value": tc.frozen})
	}
	query(negative+` SET n.value_fold=12345`, nil)
	if got := runRejected("REFUSED:", base...); got != "" {
		t.Fatalf("database read error wrote a report: %s", got)
	}
	query(negative+` SET n.value_fold='NE12345'`, nil)
	setChunk := func(text, hash string) {
		query(`MATCH (c:Chunk {id:$id}) SET c.text=$text, c.hash=$hash`,
			map[string]any{"id": graph.ChunkID(first, 0), "text": text, "hash": hash})
	}
	setChunk("changed text", productFixtureDigest("changed text"))
	requirePreflightFail("document "+first+" chunks_sha256:", base...)
	setChunk("changed without changing stored hash", productFixtureDigest(firstText))
	if got := runRejected("FAIL: stored chunk hash", base...); !strings.Contains(got, `"status": "FAIL"`) {
		t.Fatalf("expected FAIL report: %s", got)
	}
	setChunk(firstText, productFixtureDigest(firstText))
	query(`MATCH (:Document {id:$id})-[r:HAS_CHUNK]->(:Chunk {id:$chunk}) DELETE r`,
		map[string]any{"id": first, "chunk": graph.ChunkID(first, 1)})
	requirePreflightFail("document "+first+" chunk_count:", base...)
	query(`MATCH (d:Document {id:$id}), (c:Chunk {id:$chunk}) CREATE (d)-[:HAS_CHUNK]->(c)`,
		map[string]any{"id": first, "chunk": graph.ChunkID(first, 1)})
	runRejected("FAIL: context deadline exceeded", append(slices.Clone(base), "-timeout", "1ns")...)

	// Remove a valid oracle pair while keeping its product and chunk in the
	// remaining oracle. A production read must expose the resulting extra.
	var smaller bytes.Buffer
	for _, line := range bytes.Split(bytes.TrimSpace(read(pairsPath)), []byte("\n")) {
		pair := readProductJSON(t, line)
		if pair["id"] == "parity-product-b" && pair["chunk_id"] == graph.ChunkID(first, 0) {
			continue
		}
		smaller.Write(line)
		smaller.WriteByte('\n')
	}
	oracleSummary := readProductJSON(t, read(summaryPath))
	oracleSummary["counts"].(map[string]any)["pairs"] = 3
	data, err := json.Marshal(oracleSummary)
	if err != nil {
		t.Fatal(err)
	}
	shortSummary := writeProductFixture(t, dir, "short-summary.json", string(data))
	shortPairs := writeProductFixture(t, dir, "short-pairs.jsonl", smaller.String())
	if got := runRejected("extra pairs=1, extra entries=1", digests(shortSummary, shortPairs)...); !strings.Contains(got, `"status": "FAIL"`) {
		t.Fatalf("expected FAIL report: %s", got)
	}

	summary, oracle, err := decodeProductParityInputs(read(summaryPath), read(pairsPath))
	if err != nil {
		t.Fatal(err)
	}
	drift := &parityDriftStore{Store: st, mutate: func() {
		query(`MATCH (s:ProductStore {store:'parity-a.test'}) SET s.materialized_activation_seq=13`, nil)
	}}
	post, err := measureProductMentionParity(ctx, drift, summary, oracle)
	if err != nil || post.Status != "FAIL" || !strings.Contains(strings.Join(post.Failures, " "), "materialized_activation_seq") {
		t.Fatalf("mid-run drift report: %+v, err %v", post, err)
	}
	t.Logf("mid-run drift status=%s failures=%v", post.Status, post.Failures)
}

type parityDriftStore struct {
	*graph.Store
	mutate func()
}

func (s *parityDriftStore) GetProductWithMentions(ctx context.Context, lookup graph.ProductLookup) (graph.ProductDetailWithMentions, graph.ProductMentionRead, error) {
	out, stats, err := s.Store.GetProductWithMentions(ctx, lookup)
	if s.mutate != nil {
		s.mutate()
		s.mutate = nil
	}
	return out, stats, err
}
