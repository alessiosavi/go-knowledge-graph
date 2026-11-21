package main

import (
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestProductCommandError(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   int
	}{
		{"stale", 5}, {"seq_conflict", 5}, {"release_changed", 5},
		{"release_not_materialized", 5}, {"store_not_bound", 5}, {"store_cap", 5},
		{"retry", 5}, {"reimport_required", 5}, {"unknown_outcome", 1},
		{"unrecognized_status", 1},
	} {
		t.Run(tc.status, func(t *testing.T) {
			err := fmt.Errorf("line 1: %w", &graph.ProductMutationError{Status: tc.status})
			if got := productCommandError("products push", io.Discard, err); got != tc.want {
				t.Errorf("exit %d, want %d", got, tc.want)
			}
		})
	}
	if got := productCommandError("products push", io.Discard, nil); got != 0 {
		t.Errorf("successful mutation exit %d, want 0", got)
	}
}

func TestProductMutationCLISeqUsage(t *testing.T) {
	for _, seq := range []string{"abc", "0", "-1"} {
		t.Run("remove/"+seq, func(t *testing.T) {
			productCLI(t, 2, productsRemoveUsage, "products", "remove", "-product", "example.test:42", "-seq", seq)
		})
		t.Run("push/"+seq, func(t *testing.T) {
			productCLI(t, 2, productsPushUsage, "products", "push", "-seq", seq, "X")
		})
	}
}

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
	id := "example.test:42"
	record := strings.TrimSpace(snapshotRecord(id, "first", `{"codes":"X1 / X2"}`))
	snapshot := writeProductFixture(t, dir, "snapshot.jsonl", record+"\n")
	event := func(seq int, record string) string {
		return fmt.Sprintf(`{"seq":%d,"record":%s}`+"\n", seq, record)
	}
	productCLI(t, 0, "activated", "products", "release", "activate", "push-cli")
	productCLI(t, 5, "release_not_materialized", "products", "push",
		writeProductFixture(t, dir, "before-import.jsonl", event(1, record)))
	productCLI(t, 0, "imported", "products", "import", "-store", "example.test", "-release", "push-cli",
		"-snapshot-seq", "1", "-report", filepath.Join(dir, "import-report.json"), snapshot)

	if !t.Run("features_dispatch", func(t *testing.T) {
		writeProductFixture(t, dir, "X", event(5, record))
		t.Chdir(dir)
		const state = `MATCH (s:ProductStore {store:'example.test'})
OPTIONAL MATCH (p:Product {store:s.store}) OPTIONAL MATCH (p)-[e:HAS_FEATURE]->()
RETURN properties(s), properties(p), count(e)`
		for _, args := range [][]string{{"remove", "-product", id, "-seq", "4"}, {"push", "X"}} {
			t.Run(args[0], func(t *testing.T) {
				before := query(state, nil)
				defer func() {
					if after := query(state, nil); !reflect.DeepEqual(after, before) {
						t.Error("features command changed product store state")
					}
				}()
				productCLI(t, 2, featuresCheckUsage+"\n"+featuresImportUsage, append([]string{"features"}, args...)...)
			})
		}
	}) {
		return
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

	for _, tc := range []struct{ name, payload string }{
		{"duplicate envelope", `{"seq":5,"seq":5,"record":` + record + `}`},
		{"unknown envelope", `{"seq":5,"record":` + record + `,"other":true}`},
		{"trailing value", event(5, record)[:len(event(5, record))-1] + " {}"},
		{"non-integer seq", `{"seq":5.0,"record":` + record + `}`},
		{"malformed seq", `{"seq":"abc","record":` + record + `}`},
		{"missing seq", `{"record":` + record + `}`},
		{"zero seq", event(0, record)},
		{"negative seq", event(-1, record)},
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
	create := writeProductFixture(t, dir, "create.jsonl", strings.TrimSpace(event(5, record)))
	update := writeProductFixture(t, dir, "update.jsonl", event(6, record))
	for _, tc := range []struct {
		status string
		args   []string
	}{
		{"deleted", []string{"remove", "-product", id, "-seq", "3"}},
		{"duplicate", []string{"remove", "-product", id, "-seq", "3"}},
		{"absent", []string{"remove", "-product", id, "-seq", "4"}},
		{"created", []string{"push", create}},
		{"updated", []string{"push", update}},
	} {
		t.Run(tc.status, func(t *testing.T) {
			productCLI(t, 0, fmt.Sprintf(`"status":%q`, tc.status), append([]string{"products"}, tc.args...)...)
		})
	}
}
