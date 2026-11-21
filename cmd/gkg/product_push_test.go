package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestProductPushCLI(t *testing.T) {
	_, _, query := productTestDB(t)
	t.Cleanup(func() {
		query(`MATCH (n) WHERE n:FeatureRegistry OR n:FeatureRev OR n:ProductRelease OR n:ProductStore
		    OR n:Product OR (n:GkgMeta AND n.id IN ['product_release','feature_registry_import'])
		    OR (n:Domain AND n.id='push-cli') DETACH DELETE n`, nil)
	})
	query(`CREATE (:Domain {id:'push-cli'})`, nil)
	dir := t.TempDir()
	digest := productFixtureDigest(snapshotRegistry)
	registry := writeProductFixture(t, dir, "R.json", snapshotRegistry)
	productCLI(t, 0, digest[:16], "features", "import", registry)
	body := fmt.Sprintf(`{"registry_sha256":%q,"keys":[{"store":"example.test","feature":"code","source_key":"codes"}]}`, digest)
	candidates := writeProductFixture(t, dir, "candidates.json", body)
	productCLI(t, 0, "push-cli", productReleaseTestArgs(t, "push-cli", digest[:16], candidates,
		productFixturePins(snapshotRegistry, body), "example.test=push-cli")...)
	id := "example.test:1"
	record := strings.TrimSpace(snapshotRecord(id, "first", `{"codes":"X1 / X2"}`))
	snapshot := writeProductFixture(t, dir, "snapshot.jsonl", record+"\n")
	productCLI(t, 0, "activated", "products", "release", "activate", "push-cli",
		"-materialize", "example.test=1:"+snapshot)
	event := func(seq int, record string) string {
		return fmt.Sprintf(`{"seq":%d,"record":%s}`+"\n", seq, record)
	}
	file := writeProductFixture(t, dir, "push.jsonl", event(2, record)+event(2, record)+event(1, record)+event(3, record))
	out := productCLI(t, 5, "line 3: stale", "products", "push", file)
	if !strings.Contains(out, `"status":"updated"`) || !strings.Contains(out, `"status":"duplicate"`) ||
		strings.Count(strings.TrimSpace(out), "\n") != 1 {
		t.Fatalf("push results = %q, want two ordered results", out)
	}
	rows := query(`MATCH (s:ProductStore {store:'example.test'}) RETURN s.seen_seq`, nil)
	if len(rows) != 1 || rows[0].Values[0] != int64(2) {
		t.Fatalf("watermark after rejected line: %v", rows)
	}
	productCLI(t, 5, "seq_conflict", "products", "remove", "-product", id, "-seq", "2")
	productCLI(t, 0, `"status":"deleted"`, "products", "remove", "-product", id, "-seq", "3")
	productCLI(t, 0, `"status":"duplicate"`, "products", "remove", "-product", id, "-seq", "3")
	productCLI(t, 0, `"status":"absent"`, "products", "remove", "-product", id, "-seq", "4")

	for _, tc := range []struct{ name, payload string }{
		{"duplicate envelope", `{"seq":5,"seq":5,"record":` + record + `}`},
		{"unknown envelope", `{"seq":5,"record":` + record + `,"other":true}`},
		{"trailing value", event(5, record)[:len(event(5, record))-1] + " {}"},
		{"non-integer seq", `{"seq":5.0,"record":` + record + `}`},
		{"missing seq", `{"record":` + record + `}`},
		{"zero seq", event(0, record)},
		{"bare prefix", event(5, strings.Replace(record, id, "example.test:", 1))},
		{"mismatched store", event(5, strings.Replace(record, id, "other.test:1", 1))},
		{"oversized envelope", event(5, strings.Repeat(" ", 128<<10)+record)},
		{"oversized record", event(5, strings.Replace(record, "first", strings.Repeat("x", 64<<10), 1))},
		{"released value cap", event(5, strings.Replace(record, "X1 / X2", strings.Repeat("x", 1025), 1))},
		{"released item cap", event(5, strings.Replace(record, "X1 / X2", strings.Repeat("X1/", 64)+"X2", 1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := writeProductFixture(t, dir, "invalid.jsonl", tc.payload)
			productCLI(t, 1, "line 1:", "products", "push", file)
		})
	}
	productCLI(t, 1, "non-empty suffix", "products", "remove", "-product", "example.test:", "-seq", "5")
	productCLI(t, 0, `"status":"created"`, "products", "push",
		writeProductFixture(t, dir, "final.jsonl", strings.TrimSpace(event(5, record))))
}
