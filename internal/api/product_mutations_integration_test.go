package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestProductMutationLifecycle(t *testing.T) {
	st := graphtest.Store(t)
	const store = "rest-push.test"
	const release = "rest-push-fixture"
	const record = `{"store":"rest-push.test","product_id":"rest-push.test:1","title":"Original","brand":"","category":"","subcategory":"","product_type":null,"link":"","attributes":{"code":"A1"},"page_specs":null}`
	seedProductMutationRelease(t, st, store, release)
	snapshot := record + "\n"
	report := graph.ProductImportReport{SnapshotDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(snapshot)))}
	if err := st.ImportProductSnapshot(t.Context(), graph.ProductSnapshotInput{Store: store, Release: release, Seq: 1}, strings.NewReader(snapshot), &report, nil); err != nil || report.Status != "imported" {
		t.Fatalf("initial import: status %q, err %v", report.Status, err)
	}
	h := NewServer(config.Config{}, st, nil).Handler()
	update := strings.Replace(record, "Original", "Updated", 1)
	put := func(seq int64, record string) *httptest.ResponseRecorder {
		return doJSON(t, h, http.MethodPut, testProductPut, fmt.Sprintf(`{"seq":%d,"record":%s}`, seq, record))
	}
	for _, id := range []string{"rest-push.test:", "other.test:1"} {
		assertProductMutationError(t, put(2, strings.Replace(record, testProductID, id, 1)), 400, "invalid_request", nil)
	}
	invalidDelete := testProductPut + "?product_id=rest-push.test%3A&seq=2"
	assertProductMutationError(t, do(t, h, http.MethodDelete, invalidDelete, nil, nil), 400, "invalid_request", nil)

	assertResult := func(rr *httptest.ResponseRecorder, status string, seq int64) graph.ProductMutationResult {
		t.Helper()
		if rr.Code != 200 {
			t.Fatalf("%s: response = %d %s, want 200", status, rr.Code, rr.Body)
		}
		got := decode[graph.ProductMutationResult](t, rr)
		if got.Status != status || got.ProductID != testProductID || got.SeenSeq != seq || got.ID == "" {
			t.Fatalf("response = %+v, want %s for %s at seq %d with a graph ID", got, status, testProductID, seq)
		}
		return got
	}
	applied := assertResult(put(2, update), "updated", 2)
	duplicate := assertResult(put(2, update), "duplicate", 2)
	if duplicate.ID != applied.ID {
		t.Fatalf("duplicate changed graph ID from %s to %s", applied.ID, duplicate.ID)
	}
	seen := int64(2)
	assertProductMutationError(t, put(1, record), 409, "stale", &seen)
	path := testProductPut + "/" + applied.ID
	rr := do(t, h, http.MethodGet, path, nil, nil)
	if rr.Code != 200 || decode[graph.ProductDetail](t, rr).Title != "Updated" {
		t.Fatalf("GET after stale retry = %d %s, want updated product", rr.Code, rr.Body)
	}
	assertResult(do(t, h, http.MethodDelete, testProductPut+"?product_id=rest-push.test%3A1&seq=3", nil, nil), "deleted", 3)
	rr = do(t, h, http.MethodGet, path, nil, nil)
	if rr.Code != 404 {
		t.Fatalf("GET after DELETE = %d %s, want 404", rr.Code, rr.Body)
	}
}

func seedProductMutationRelease(t *testing.T, st *graph.Store, store, release string) {
	t.Helper()
	const registry = `{
	  "schema":"gkg-features/v1",
	  "features":[{"id":"rest_push_code","name":"Code","definition":"REST push fixture code","kind":"code",
	    "cardinality":"single","checkability":"page_checkable","measure":null}],
	  "stores":{"rest-push.test":{"keys":{"code":{"feature":"rest_push_code","separator":null,
	    "source_labels":["Code"],"dev_evidence":5,"numeric_search":false}},"ignore":{},"deferred":[]}}
	}`
	reg, err := st.ImportFeatureRegistry(t.Context(), []byte(registry))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{
		"registry_rev": reg.Rev, "registry_sha256": reg.Digest,
		"store_domains": map[string]string{store: release},
		"triples":       []productobs.ReleaseTriple{{Store: store, Feature: "rest_push_code", SourceKey: "code"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	driver, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })
	query := func(cypher string, params map[string]any) []*neo4j.Record {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sess := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
		defer sess.Close(ctx)
		res, err := sess.Run(ctx, cypher, params)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := res.Collect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	previous := query(`MATCH (m:GkgMeta {id:'product_release'}) RETURN properties(m) AS props`, nil)
	params := map[string]any{"store": store, "release": release, "rev": reg.Rev, "manifest": string(manifest), "digest": fmt.Sprintf("%x", sha256.Sum256(manifest))}
	t.Cleanup(func() {
		query(`MATCH (n)
WHERE (n:Product OR n:ProductStore) AND n.store=$store
   OR n:Domain AND n.id=$release OR n:ProductRelease AND n.id=$release
DETACH DELETE n`, params)
		if len(previous) == 0 {
			query(`MATCH (m:GkgMeta {id:'product_release'}) DELETE m`, nil)
		} else {
			props, _ := previous[0].Get("props")
			query(`MATCH (m:GkgMeta {id:'product_release'}) SET m=$props`, map[string]any{"props": props})
		}
	})
	// Pin the fixture release without activation pruning unrelated stores' edges.
	query(`CREATE (:Domain {id:$release}), (:ProductStore {store:$store,domain:$release}),
       (:ProductRelease {id:$release,state:'active',registry_rev:$rev,manifest_json:$manifest,manifest_digest:$digest})
WITH 1 AS seeded
MERGE (m:GkgMeta {id:'product_release'})
SET m.value=$release, m.activation_seq=coalesce(m.activation_seq,0)+1`, params)
}
