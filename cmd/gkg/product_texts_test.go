package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProductTextImportCLI(t *testing.T) {
	_, _, query := productTestDB(t)
	const store, release = "t1b-cli.test", "t1b-cli"
	registry := strings.NewReplacer("z.example.test", "t1b-cli-other.test", "example.test", store).Replace(productRegistryFixture)
	digest := productFixtureDigest(registry)
	params := map[string]any{"store": store, "release": release, "rev": digest[:16]}
	t.Cleanup(func() {
		query(`MATCH (n) WHERE
(n.store=$store AND (n:Product OR n:ProductText OR n:ProductStore))
OR (n.id=$release AND (n:Domain OR n:ProductRelease))
OR (n.rev=$rev AND (n:FeatureRegistry OR n:FeatureRev))
OR (n:GkgMeta AND n.id='product_release' AND n.value=$release)
DETACH DELETE n`, params)
	})
	query(`CREATE (:Domain {id:$release})`, params)
	dir := t.TempDir()
	path := writeProductFixture(t, dir, "R.json", registry)
	productCLI(t, 0, digest[:16], "features", "import", path)
	body := fmt.Sprintf(`{"registry_sha256":%q,"keys":[{"store":%q,"feature":"length","source_key":"length"}]}`, digest, store)
	candidates := writeProductFixture(t, dir, "candidates.json", body)
	productCLI(t, 0, release, productReleaseTestArgs(t, release, digest[:16], candidates,
		productFixturePins(registry, body), store+"="+release)...)
	productCLI(t, 0, "activated", "products", "release", "activate", release)

	attr := fmt.Sprintf(`{"length":"2 cm","empty":"","finish":%q,%q:"unused"}`, strings.Repeat("x", 2048), strings.Repeat("k", 257))
	record := snapshotRecord(store+":1", "original", attr)
	records := writeProductFixture(t, dir, "records.jsonl", record)
	importAt := func(seq, exit int, want string, extra ...string) map[string]any {
		t.Helper()
		report := filepath.Join(dir, fmt.Sprintf("report-%d.json", seq))
		args := []string{"products", "import", "-store", store, "-release", release,
			"-snapshot-seq", fmt.Sprint(seq), records, "-report", report}
		output := productCLI(t, exit, want, append(args, extra...)...)
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if exit == 0 {
			wantOutput := fmt.Sprintf("%s store=%s release=%s seq=%d products=1 edges=1 text_action=%s text_count=1\n",
				want, store, release, seq, fields["text_action"])
			if output != wantOutput {
				t.Fatalf("import stdout = %q, want %q", output, wantOutput)
			}
		}
		return fields
	}
	requireReport := func(report map[string]any, action string) {
		t.Helper()
		for key, want := range map[string]any{
			"text_count": float64(1), "text_edge_count": float64(1), "text_bytes": float64(2048 + len("finish")),
			"texts_skipped": float64(2), "texts_skipped_key": float64(1), "texts_truncated": float64(1), "text_action": action,
		} {
			if got := report[key]; got != want {
				t.Errorf("report[%q] = %v, want %v", key, got, want)
			}
		}
	}
	readState := func(cypher string) [][]any {
		rows := query(cypher, params)
		out := make([][]any, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.Values)
		}
		return out
	}
	const products = `MATCH (p:Product {store:$store}) RETURN id(p), properties(p)`
	const textEdges = `MATCH (:Product {store:$store})-[e:HAS_TEXT]->(n) RETURN id(e), id(n), properties(e), properties(n)`
	const storeProperties = `MATCH (s:ProductStore {store:$store}) RETURN properties(s)`
	t.Setenv("GKG_PRODUCT_TEXT_SEARCH", "off")
	requireReport(importAt(1, 0, "imported"), "written")
	originalEdges := readState(textEdges)
	t.Setenv("GKG_PRODUCT_TEXT_SEARCH", "on")
	requireReport(importAt(2, 0, "unchanged"), "unchanged")
	if after := readState(textEdges); !reflect.DeepEqual(after, originalEdges) {
		t.Fatal("unchanged import rewrote the projection")
	}

	query(`MATCH (n:ProductText {store:$store}) DETACH DELETE n`, params)
	query(`MATCH (p:Product {store:$store}), (s:ProductStore {store:$store})
SET p.t1b_marker=true REMOVE s.text_version, s.text_count, s.text_edge_count, s.text_bytes`, params)
	originalProducts := readState(products)
	requireReport(importAt(3, 0, "unchanged"), "materialized")
	if after := readState(products); !reflect.DeepEqual(after, originalProducts) {
		t.Fatal("legacy backfill modified products")
	}
	query(`MATCH (s:ProductStore {store:$store}), (:Product {store:$store})-[e:HAS_TEXT]->()
SET s.text_edge_count=0 DELETE e`, params)
	query(`CREATE (:ProductText {store:$store,id:'t1b-cli-orphan',text:'orphan'})`, params)
	requireReport(importAt(4, 0, "unchanged"), "materialized")
	if got := readState(textEdges); len(got) != 1 {
		t.Fatalf("count mismatch backfill has %d text edges, want 1", len(got))
	}
	if got := readState(`MATCH (n:ProductText {store:$store}) RETURN count(n)`); !reflect.DeepEqual(got, [][]any{{int64(1)}}) {
		t.Fatalf("texts after backfill = %v, want one node", got)
	}

	// Seed values an ordinary import would restamp, and mark texts for backfill.
	query(`MATCH (p:Product {store:$store}), (s:ProductStore {store:$store})
SET s.text_version=0, s.materialized_activation_seq=0, s.prop_bytes=0,
    s.last_event_op='push', s.last_event_product_id=p.product_id, s.last_event_seq=s.seen_seq,
    s.imported_at='2026-01-01T00:00:00Z'
REMOVE p.prop_bytes`, params)
	originalProducts = readState(products)
	wantStore := readState(storeProperties)
	props := wantStore[0][0].(map[string]any)
	if props["seen_seq"] != int64(4) {
		t.Fatalf("seen_seq before texts-only = %v, want 4", props["seen_seq"])
	}
	requireReport(importAt(5, 0, "unchanged", "-texts-only"), "materialized")
	if after := readState(products); !reflect.DeepEqual(after, originalProducts) {
		t.Fatal("texts-only modified products")
	}
	props["seen_seq"], props["text_version"] = int64(5), int64(1)
	if after := readState(storeProperties); !reflect.DeepEqual(after, wantStore) {
		t.Fatalf("store after texts-only = %v, want %v", after, wantStore)
	}

	originalEdges = readState(textEdges)
	requireReport(importAt(6, 0, "unchanged", "-texts-only"), "unchanged")
	if after := readState(textEdges); !reflect.DeepEqual(after, originalEdges) {
		t.Fatal("repeated texts-only rewrote the projection")
	}
	props["seen_seq"] = int64(6)
	if after := readState(storeProperties); !reflect.DeepEqual(after, wantStore) {
		t.Fatalf("store after repeated texts-only = %v, want %v", after, wantStore)
	}

	requireReport(importAt(7, 0, "unchanged"), "unchanged") // Backfill product bytes for push.
	updated := snapshotRecord(store+":1", "pushed", `{"finish":"brushed steel"}`)
	push := writeProductFixture(t, dir, "push.jsonl", fmt.Sprintf("{\"seq\":8,\"record\":%s}\n", strings.TrimSpace(updated)))
	productCLI(t, 0, `"status":"updated"`, "products", "push", push)
	queries := []string{products, textEdges, storeProperties,
		`MATCH (m:GkgMeta {id:'product_release'}) RETURN properties(m)`}
	before := make([][][]any, len(queries))
	for i, q := range queries {
		before[i] = readState(q)
	}
	report := importAt(9, 5, "-texts-only requires an unchanged snapshot", "-texts-only")
	if report["status"] != "rejected" || report["text_action"] != "" {
		t.Fatalf("refusal report = %v", report)
	}
	for i, q := range queries {
		if after := readState(q); !reflect.DeepEqual(after, before[i]) {
			t.Fatalf("refused -texts-only import changed state for %s", q)
		}
	}
}
