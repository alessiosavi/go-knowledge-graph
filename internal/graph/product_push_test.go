package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	pushStore  = "push.test"
	pushDomain = "push-domain"
	pushA      = "push-release-a"
	pushB      = "push-release-b"
	pushOne    = "push.test:1"
	pushTwo    = "push.test:2"
)

const pushRegistry = `{
  "schema":"gkg-features/v1",
  "features":[{"id":"code","name":"Code","definition":"Push test code","kind":"code",
    "cardinality":"multi","checkability":"page_checkable","measure":null}],
  "stores":{"push.test":{"keys":{
    "scalar":{"feature":"code","separator":null,"source_labels":["Scalar"],"dev_evidence":5,"numeric_search":false},
    "list":{"feature":"code","separator":"/","source_labels":["List"],"dev_evidence":5,"numeric_search":false}
  },"ignore":{},"deferred":[]}}
}`

func TestProductMutationPrepareTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Store) (ProductMutationResult, error)
	}{
		{"put", func(s *Store) (ProductMutationResult, error) {
			record := pushRecord(pushOne, "one", "null", `{}`)
			return s.PushProduct(context.Background(), 1, []byte(record))
		}},
		{"delete", func(s *Store) (ProductMutationResult, error) {
			return s.RemoveProduct(context.Background(), pushOne, 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := errors.New("stop at prepare transaction")
			session := &productPrepareSession{err: stop}
			store := &Store{driver: productPrepareDriver{session: session}}
			if _, err := tc.mutate(store); !errors.Is(err, stop) {
				t.Fatalf("prepare error = %v, want %v", err, stop)
			}
			if session.budget <= 0 || session.budget > productPushTimeout {
				t.Errorf("prepare context budget = %v, want (0, %v]", session.budget, productPushTimeout)
			}
			if session.config.Timeout <= 0 || session.config.Timeout > productPushTimeout {
				t.Errorf("prepare transaction timeout = %v, want (0, %v]", session.config.Timeout, productPushTimeout)
			}
		})
	}
}

type productPrepareDriver struct {
	neo4j.Driver
	session *productPrepareSession
}

func (d productPrepareDriver) NewSession(context.Context, neo4j.SessionConfig) neo4j.Session {
	return d.session
}

type productPrepareSession struct {
	neo4j.Session
	err    error
	budget time.Duration
	config neo4j.TransactionConfig
}

func (s *productPrepareSession) BeginTransaction(ctx context.Context, opts ...func(*neo4j.TransactionConfig)) (neo4j.ExplicitTransaction, error) {
	if deadline, ok := ctx.Deadline(); ok {
		s.budget = time.Until(deadline)
	}
	for _, opt := range opts {
		opt(&s.config)
	}
	return nil, s.err
}

func (*productPrepareSession) Close(context.Context) error { return nil }

type pushFixture struct {
	t        *testing.T
	store    *Store
	other    *Store
	storeID  string
	rev      string
	registry string
	feature  string
}

func newPushFixture(t *testing.T) pushFixture {
	t.Helper()
	return newPushFixtureWithFeature(t, "code")
}

func newPushFixtureWithFeature(t *testing.T, feature string) pushFixture {
	t.Helper()
	return newPushFixtureForStore(t, pushStore, feature)
}

func newPushFixtureForStore(t *testing.T, storeID, feature string) pushFixture {
	t.Helper()
	registry := strings.ReplaceAll(pushRegistry, `"id":"code"`, `"id":"`+feature+`"`)
	registry = strings.ReplaceAll(registry, `"feature":"code"`, `"feature":"`+feature+`"`)
	registry = strings.ReplaceAll(registry, pushStore, storeID)
	uri := os.Getenv("GKG_TEST_MEMGRAPH_URI")
	if uri == "" {
		t.Skip("requires a disposable Memgraph integration database")
	}
	cfg := config.Config{}
	cfg.Memgraph.URI = uri
	open := func() *Store {
		st, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close(context.Background()) })
		return st
	}
	f := pushFixture{t: t, store: open(), other: open(), storeID: storeID, registry: registry, feature: feature}
	if err := f.store.Bootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		err := f.store.runWrite(context.Background(), `MATCH (n)
WHERE n:Product OR n:ProductStore OR n:ProductRelease OR n:FeatureRev OR n:FeatureRegistry
   OR (n:ProductText AND n.store=$store)
   OR (n:GkgMeta AND n.id IN ['product_release','feature_registry_import'])
   OR (n:Domain AND n.id IN ['push-domain','push-rebound'])
DETACH DELETE n`, map[string]any{"store": storeID})
		if err != nil {
			t.Error(err)
		}
	})
	reg, err := f.store.ImportFeatureRegistry(t.Context(), []byte(registry))
	if err != nil {
		t.Fatal(err)
	}
	f.rev = reg.Rev
	params := map[string]any{"store": storeID, "domain": pushDomain}
	f.query(`CREATE (:Domain {id:$domain}), (:Domain {id:'push-rebound'}),
		(:ProductStore {store:$store,domain:$domain,seen_seq:0,snapshot_seq:0,product_count:0,edge_count:0})`, params)
	f.seedRelease(pushA, pushDomain)
	f.seedRelease(pushB, pushDomain)
	f.activate(pushA)
	return f
}

func (f pushFixture) seedRelease(id, domain string) {
	f.t.Helper()
	manifest, err := json.Marshal(productManifest{
		RegistryRev: f.rev, RegistrySHA256: productDigest([]byte(f.registry)),
		StoreDomains: map[string]string{f.storeID: domain},
		Triples: []productobs.ReleaseTriple{
			{Store: f.storeID, Feature: f.feature, SourceKey: "scalar"},
			{Store: f.storeID, Feature: f.feature, SourceKey: "list"},
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.query(`CREATE (:ProductRelease {id:$id, state:'created', registry_rev:$rev,
		manifest_json:$manifest, manifest_digest:$digest})`, map[string]any{
		"id": id, "rev": f.rev, "manifest": string(manifest), "digest": productDigest(manifest),
	})
}

func (f pushFixture) activate(id string) {
	f.t.Helper()
	if err := f.store.ActivateProductRelease(f.t.Context(), id); err != nil {
		f.t.Fatal(err)
	}
}

func (f pushFixture) query(cypher string, params map[string]any) [][]any {
	f.t.Helper()
	session := f.store.driver.NewSession(f.t.Context(), neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(context.Background())
	res, err := session.Run(f.t.Context(), cypher, params)
	if err != nil {
		f.t.Fatal(err)
	}
	rows, err := res.Collect(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	values := make([][]any, 0, len(rows))
	for _, row := range rows {
		values = append(values, row.Values)
	}
	return values
}

func pushRecord(id, title, kind, attrs string) string {
	return fmt.Sprintf(`{"store":"push.test","product_id":%q,"title":%q,"brand":"","category":"",
		"subcategory":"","link":"https://push.test/p","product_type":%s,"attributes":%s,"page_specs":null}`,
		id, title, kind, attrs)
}

func (f pushFixture) importRows(st *Store, release string, seq int64, stage func() error, records ...string) (ProductImportReport, error) {
	input := ProductSnapshotInput{Store: f.storeID, Release: release, Seq: seq, AllowShrink: true}
	// Build one JSONL row per input record.
	var data strings.Builder
	for _, record := range records {
		data.WriteString(strings.ReplaceAll(record, "\n", ""))
		data.WriteByte('\n')
	}
	report := ProductImportReport{SnapshotDigest: productDigest([]byte(data.String()))}
	err := st.ImportProductSnapshot(f.t.Context(), input, strings.NewReader(data.String()), &report, stage)
	return report, err
}

func (f pushFixture) snapshot(release string, seq int64, status string, records ...string) ProductImportReport {
	f.t.Helper()
	report, err := f.importRows(f.store, release, seq, nil, records...)
	if err != nil || report.Status != status {
		f.t.Fatalf("import at %d: status=%q err=%v, want %s", seq, report.Status, err, status)
	}
	return report
}

func (f pushFixture) put(seq int64, record, status string) ProductMutationResult {
	f.t.Helper()
	result, err := f.store.PushProduct(f.t.Context(), seq, []byte(record))
	if err != nil || result.Status != status || result.SeenSeq != seq ||
		result.ID != productobs.ProductID(f.storeID, result.ProductID) {
		f.t.Fatalf("push at %d: %+v %v, want %s", seq, result, err, status)
	}
	return result
}

func (f pushFixture) remove(seq int64, id, status string) {
	f.t.Helper()
	got, err := f.store.RemoveProduct(f.t.Context(), id, seq)
	if err != nil || got.Status != status || got.SeenSeq != seq || got.ProductID != id ||
		got.ID != productobs.ProductID(f.storeID, id) {
		f.t.Fatalf("remove at %d: %+v %v, want %s", seq, got, err, status)
	}
}

func assertProductMutationError(t *testing.T, err error, status string, seen *int64) {
	t.Helper()
	var mutation *ProductMutationError
	if !errors.As(err, &mutation) || mutation.Status != status || !reflect.DeepEqual(mutation.SeenSeq, seen) {
		t.Fatalf("product mutation error = %v, want %s, seen_seq %v", err, status, seen)
	}
}

const pushWatermarkQuery = `MATCH (s:ProductStore {store:'push.test'})
RETURN s.seen_seq, s.snapshot_seq, s.wiped_seq, s.last_event_op, s.last_event_product_id, s.last_event_seq`

func (f pushFixture) watermark(want ...any) {
	f.t.Helper()
	if got := f.query(pushWatermarkQuery, nil); !reflect.DeepEqual(got, [][]any{want}) {
		f.t.Fatalf("watermark = %v, want %v", got, want)
	}
}

func (f pushFixture) cursor() ProductSearchRequest {
	f.t.Helper()
	value, limit := "A1", 1
	req := ProductSearchRequest{Feature: "code", Value: &value, Limit: &limit}
	first, err := f.store.FindProducts(f.t.Context(), req)
	if err != nil || len(first.Hits) != 1 || first.Cursor == "" {
		f.t.Fatalf("initial search = %+v, %v", first, err)
	}
	req.Cursor = first.Cursor
	return req
}

func TestProductPushOrderingAndImport(t *testing.T) {
	f := newPushFixture(t)
	ctx := t.Context()
	a := pushRecord(pushOne, "A", `"device"`, `{"scalar":"A1"}`)
	b := pushRecord(pushTwo, "B", "null", `{"list":"A1 / B2"}`)

	_, err := f.store.PushProduct(ctx, 1, []byte(a))
	assertProductMutationError(t, err, "release_not_materialized", nil)
	report := f.snapshot(pushA, 1, "imported", a, b)
	if got := f.query(`MATCH (s:ProductStore {store:'push.test'}), (p:Product {store:s.store})
RETURN s.prop_bytes, sum(p.prop_bytes), s.materialized_activation_seq`, nil); !reflect.DeepEqual(got,
		[][]any{{int64(report.PropertyBytes), int64(report.PropertyBytes), int64(1)}}) {
		t.Fatalf("import byte ledger/stamp = %v", got)
	}

	cursor := f.cursor()
	f.query(`MATCH (p:Product {product_id:'push.test:1'}) SET p.domain='wrong'`, nil)
	updated := pushRecord(pushOne, "updated", "null", `{}`)
	result := f.put(2, updated, "updated")
	got, err := f.store.GetProduct(ctx, ProductLookup{ID: result.ID})
	if err != nil || got.ProductType != nil || got.Domain != pushDomain || got.Title != "updated" || len(got.Features) != 0 {
		t.Fatalf("updated product = %+v, %v", got, err)
	}
	if _, err := f.store.FindProducts(ctx, cursor); !errors.Is(err, ErrProductCursor) {
		t.Fatalf("pre-push cursor: %v", err)
	}
	// A3 identifies a duplicate by the event tuple, even when the caller
	// resends a different payload. The accepted data stays authoritative.
	f.put(2, a, "duplicate")
	f.put(2, strings.Replace(updated, `"page_specs":null`, `"page_specs":[{"label":"x","value":"y"}]`, 1), "duplicate")
	seen := int64(2)
	_, err = f.store.RemoveProduct(ctx, pushOne, 2)
	assertProductMutationError(t, err, "seq_conflict", &seen)
	_, err = f.store.PushProduct(ctx, 2, []byte(b))
	assertProductMutationError(t, err, "seq_conflict", &seen)
	_, err = f.store.PushProduct(ctx, 1, []byte(a))
	assertProductMutationError(t, err, "stale", &seen)
	f.watermark(int64(2), int64(1), nil, "put", pushOne, int64(2))
	stores, err := f.store.ListProductStores(ctx)
	if err != nil || len(stores.Stores) != 1 || stores.Stores[0].PushedAt == nil ||
		stores.Stores[0].SeenSeq != 2 || stores.Stores[0].MaterializedActivationSeq == nil ||
		*stores.Stores[0].MaterializedActivationSeq != 1 {
		t.Fatalf("store DTO = %+v, %v", stores, err)
	}
	if _, err := f.importRows(f.store, pushA, 2, nil, b); !errors.Is(err, ErrProductData) {
		t.Fatalf("same-seq import after push: %v", err)
	}
	f.snapshot(pushA, 3, "imported", b)
	f.watermark(int64(3), int64(3), nil, nil, nil, nil)
	seen = 3
	_, err = f.store.PushProduct(ctx, 3, []byte(a))
	assertProductMutationError(t, err, "seq_conflict", &seen)
	f.put(4, a, "created")

	// Simulate an unchanged import encountering a leftover event marker.
	// A real push clears the digest and always forces a replacement.
	f.snapshot(pushA, 5, "imported", a, b)
	f.query(`MATCH (s:ProductStore {store:'push.test'})
SET s.last_event_op='put', s.last_event_product_id='push.test:1', s.last_event_seq=6,
    s.prop_bytes=null, s.materialized_activation_seq=null
WITH s MATCH (p:Product {store:s.store}) SET p.prop_bytes=null`, nil)
	report = f.snapshot(pushA, 6, "unchanged", a, b)
	f.watermark(int64(6), int64(5), nil, nil, nil, nil)
	if got := f.query(`MATCH (s:ProductStore {store:'push.test'}), (p:Product {store:s.store})
RETURN s.prop_bytes, sum(p.prop_bytes), s.materialized_activation_seq`, nil); !reflect.DeepEqual(got,
		[][]any{{int64(report.PropertyBytes), int64(report.PropertyBytes), int64(1)}}) {
		t.Fatalf("unchanged import byte ledger/stamp = %v", got)
	}
	seen = 6
	_, err = f.store.PushProduct(ctx, 6, []byte(a))
	assertProductMutationError(t, err, "seq_conflict", &seen)
	f.put(7, a, "updated")
	f.remove(8, pushOne, "deleted")
	f.remove(9, pushOne, "absent")
	f.remove(9, pushOne, "duplicate")
	f.watermark(int64(9), int64(5), nil, "delete", pushOne, int64(9))
	if got := f.query(`MATCH (m:GkgMeta {id:'product_release'}) RETURN m.touched`, nil); !reflect.DeepEqual(got, [][]any{{nil}}) {
		t.Fatalf("pointer touched after pushes/imports = %v", got)
	}
}

func TestProductPushActivationRetry(t *testing.T) {
	f := newPushFixture(t)
	record := pushRecord(pushOne, "one", "null", `{"scalar":"A1"}`)
	f.snapshot(pushA, 1, "imported", record)
	f.put(2, record, "updated")
	f.activate(pushB)
	_, err := f.store.PushProduct(t.Context(), 2, []byte(record))
	assertProductMutationError(t, err, "release_not_materialized", nil)
	f.watermark(int64(2), int64(1), nil, "put", pushOne, int64(2))
	f.snapshot(pushB, 3, "imported", record)
	f.put(4, record, "updated")
}

func TestProductPushActivationWipeAndRebind(t *testing.T) {
	f := newPushFixture(t)
	ctx := t.Context()
	record := pushRecord(pushOne, "one", "null", `{"scalar":"A1"}`)
	f.snapshot(pushA, 1, "imported", record)
	decoded, err := productobs.ParseImportRecord([]byte(record))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := f.store.prepareProductEvent(ctx, productEvent{store: pushStore, productID: pushOne, op: "put", seq: 2}, &decoded)
	if err != nil {
		t.Fatal(err)
	}
	f.activate(pushB)
	_, err = f.store.writeProductEvent(ctx, prepared)
	assertProductMutationError(t, err, "release_changed", nil)
	_, err = f.store.PushProduct(ctx, 2, []byte(record))
	assertProductMutationError(t, err, "release_not_materialized", nil)
	f.activate(pushA)
	_, err = f.store.writeProductEvent(ctx, prepared)
	assertProductMutationError(t, err, "release_changed", nil)
	_, err = f.store.PushProduct(ctx, 2, []byte(record))
	assertProductMutationError(t, err, "release_not_materialized", nil)

	f.snapshot(pushA, 2, "unchanged", record)
	f.put(3, record, "updated")
	if err := f.store.DeleteProductStore(ctx, pushStore); err != nil {
		t.Fatal(err)
	}
	f.watermark(int64(3), int64(1), int64(3), nil, nil, nil)
	seen := int64(3)
	_, err = f.store.PushProduct(ctx, 3, []byte(record))
	assertProductMutationError(t, err, "stale", &seen)
	_, err = f.store.PushProduct(ctx, 4, []byte(record))
	assertProductMutationError(t, err, "release_not_materialized", nil)
	f.snapshot(pushA, 4, "imported", record)
	f.put(5, record, "updated")
	// Unbinding requires an empty store.
	if err := f.store.DeleteProductStore(ctx, pushStore); err != nil {
		t.Fatal(err)
	}
	node := f.query(`MATCH (s:ProductStore {store:'push.test'}) RETURN id(s)`, nil)
	if err := f.store.UnbindProductStore(ctx, pushStore); err != nil {
		t.Fatal(err)
	}
	f.watermark(int64(5), int64(4), int64(5), nil, nil, nil)
	_, err = f.store.RemoveProduct(ctx, pushOne, 6)
	assertProductMutationError(t, err, "store_not_bound", nil)
	out, err := f.store.ListProductStores(ctx)
	if err != nil || len(out.Stores) != 0 || out.ActiveReleaseID == nil || *out.ActiveReleaseID != pushA {
		t.Fatalf("after unbind stores = %+v, %v", out, err)
	}
	if _, err := f.store.ReportProductPages(ctx, pushStore); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after unbind pages-report: %v", err)
	}
	if err := f.store.RequireUnboundDomains(ctx, []string{pushDomain}, nil); err != nil {
		t.Fatal(err)
	}
	domains, err := f.store.ListDomains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range domains {
		if d.ID == pushDomain && d.ProductsBound {
			t.Fatalf("unbound domain still marked bound: %+v", d)
		}
	}
	if err := f.store.runProductWrite(ctx, func(tx neo4j.ManagedTransaction) error {
		return txBindProductStore(ctx, tx, pushStore, "push-rebound")
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.query(`MATCH (s:ProductStore {store:'push.test'}) RETURN id(s)`, nil); !reflect.DeepEqual(got, node) {
		t.Fatalf("rebound node = %v, want %v", got, node)
	}
	f.seedRelease("push-release-rebound", "push-rebound")
	f.activate("push-release-rebound")
	if _, err := f.importRows(f.store, "push-release-rebound", 5, nil, record); !errors.Is(err, ErrProductData) {
		t.Fatalf("import at old floor after rebind: %v", err)
	}
	f.snapshot("push-release-rebound", 6, "imported", record)
	f.put(7, record, "updated")
	got := f.query(`MATCH (p:Product {product_id:'push.test:1'}) RETURN p.domain`, nil)
	if !reflect.DeepEqual(got, [][]any{{"push-rebound"}}) {
		t.Fatalf("push domain after rebind = %v", got)
	}
}

func TestProductPushReimportRequired(t *testing.T) {
	for _, tc := range []struct {
		importStatus string
		priorPush    bool
	}{
		{"unchanged", false},
		{"imported", true},
	} {
		t.Run(tc.importStatus, func(t *testing.T) {
			f := newPushFixture(t)
			one := pushRecord(pushOne, "first", "null", `{"scalar":"A1"}`)
			two := pushRecord(pushTwo, "second", "null", `{"scalar":"B2"}`)
			f.snapshot(pushA, 1, "imported", one)
			next := int64(2)
			if tc.priorPush {
				f.put(next, one, "updated")
				next++
			}
			f.query(`MATCH (p:Product {store:$store, product_id:$product}) REMOVE p.prop_bytes`,
				map[string]any{"store": pushStore, "product": pushOne})
			const state = `MATCH (s:ProductStore {store:'push.test'})
MATCH (p:Product {store:s.store}) OPTIONAL MATCH (p)-[e:HAS_FEATURE]->()
WITH s, p, count(e) AS edges
RETURN properties(s), count(p), sum(edges), count(p.prop_bytes)`
			before := f.query(state, nil)
			_, err := f.store.PushProduct(t.Context(), next, []byte(two))
			assertProductMutationError(t, err, "reimport_required", nil)
			if msg := err.Error(); !strings.Contains(msg, "above its seen_seq") ||
				!strings.Contains(msg, `gkg products import -store "push.test" -snapshot-seq N`) {
				t.Errorf("missing repair instructions in %q", msg)
			}
			if got := f.query(state, nil); !reflect.DeepEqual(got, before) {
				t.Fatalf("refused push changed store state: got %v, before %v", got, before)
			}
			f.snapshot(pushA, next, tc.importStatus, one)
			f.put(next+1, two, "created")
		})
	}
}

func TestProductPushExpiredDeadline(t *testing.T) {
	f := newPushFixture(t)
	record := pushRecord(pushOne, "one", "null", `{"scalar":"A1"}`)
	f.snapshot(pushA, 1, "imported", record)
	decoded, err := productobs.ParseImportRecord([]byte(record))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := f.store.prepareProductEvent(t.Context(), productEvent{store: pushStore, productID: pushOne, op: "put", seq: 2}, &decoded)
	if err != nil {
		t.Fatal(err)
	}
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = f.store.writeProductEvent(expired, prepared)
	assertProductMutationError(t, err, "unknown_outcome", nil)
	f.watermark(int64(1), int64(1), nil, nil, nil, nil)
	f.put(2, record, "updated")
}

func TestProductPushCapRollback(t *testing.T) {
	for _, capName := range []string{"products", "edges", "bytes"} {
		t.Run(capName, func(t *testing.T) {
			f := newPushFixture(t)
			record := pushRecord(pushOne, "original", "null", `{}`)
			f.snapshot(pushA, 1, "imported", record)
			f.put(2, record, "updated")
			switch capName {
			case "products":
				f.query(`UNWIND range(1,$filler) AS i
CREATE (:Product {id:'filler-'+toString(i),store:$store,prop_bytes:1})`,
					map[string]any{"filler": productobs.MaxImportProducts - 1, "store": pushStore})
			case "edges":
				f.query(`MATCH (p:Product {product_id:$id}), (f:FeatureRev {key:$feature})
UNWIND range(1,$edges) AS i CREATE (p)-[:HAS_FEATURE]->(f)`,
					map[string]any{"id": pushOne, "feature": f.rev + "/code", "edges": maxProductImportEdges})
			case "bytes":
				// Seed the ledger at its ceiling without transmitting 64 MiB of fixtures.
				f.query(`MATCH (p:Product {product_id:$id}), (s:ProductStore {store:$store})
SET p.prop_bytes=$bytes, s.prop_bytes=$bytes`,
					map[string]any{"id": pushOne, "store": pushStore, "bytes": maxProductImportPropBytes})
			}
			const state = `MATCH (s:ProductStore {store:'push.test'})
MATCH (p:Product {store:s.store}) OPTIONAL MATCH (p)-[e:HAS_FEATURE]->()
WITH s, p, count(e) AS edges
RETURN properties(s), count(p), sum(edges), sum(p.prop_bytes)`
			before := f.query(state, nil)
			_, err := f.store.PushProduct(t.Context(), 3, []byte(pushRecord(pushTwo, "too many", "null", `{"scalar":"A1"}`)))
			assertProductMutationError(t, err, "store_cap", nil)
			if got := f.query(state, nil); !reflect.DeepEqual(got, before) {
				t.Fatalf("rejected push changed store state: got %v, before %v", got, before)
			}
			f.remove(3, pushOne, "deleted")
			f.watermark(int64(3), int64(1), nil, "delete", pushOne, int64(3))
		})
	}
}

func TestProductPushImportConcurrency(t *testing.T) {
	const expectedConflict = "Cannot resolve conflicting transactions"
	for _, first := range []string{"import", "push"} {
		t.Run(first+"_first", func(t *testing.T) {
			f := newPushFixture(t)
			ctx := t.Context()
			one := pushRecord(pushOne, "original 1", "null", `{"scalar":"A1"}`)
			two := pushRecord(pushTwo, "original 2", "null", `{"list":"A1 / B2"}`)
			pushed := pushRecord(pushOne, "pushed", "null", `{"scalar":"C3"}`)
			imported := pushRecord(pushOne, "imported", "null", `{"scalar":"D4"}`)
			f.snapshot(pushA, 1, "imported", one, two)
			cursor := f.cursor()
			assertVisible := func(title string, seq int64, oldCursorValid bool) {
				t.Helper()
				got, err := f.other.GetProduct(ctx, ProductLookup{Store: pushStore, ProductID: pushOne})
				if err != nil || got.Title != title || len(got.Features) != 1 {
					t.Fatalf("visible product at seq %d = %+v, %v, want %s", seq, got, err, title)
				}
				_, err = f.other.FindProducts(ctx, cursor)
				if (oldCursorValid && err != nil) || (!oldCursorValid && !errors.Is(err, ErrProductCursor)) {
					t.Fatalf("cursor at seq %d = %v, want valid=%t", seq, err, oldCursorValid)
				}
			}

			if first == "import" {
				held, release, done := make(chan struct{}), make(chan struct{}, 1), make(chan error, 1)
				defer func() {
					select {
					case release <- struct{}{}:
					default:
					}
				}()
				go func() {
					_, err := f.importRows(f.store, pushA, 2, func() error {
						close(held)
						<-release
						return nil
					}, imported, two)
					done <- err
				}()
				select {
				case <-held:
				case err := <-done:
					t.Fatalf("import returned before barrier: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("import did not reach pre-commit barrier")
				}
				assertVisible("original 1", 1, true)
				f.watermark(int64(1), int64(1), nil, nil, nil, nil)
				start := time.Now()
				_, err := f.other.PushProduct(ctx, 3, []byte(pushed))
				elapsed := time.Since(start)
				assertProductMutationError(t, err, "retry", nil)
				if !strings.Contains(err.Error(), expectedConflict) || elapsed >= 5*time.Second {
					t.Fatalf("push did not conflict immediately: %v after %s", err, elapsed)
				}
				t.Logf("import held pointer; push lost after %s: %v", elapsed, err)
				release <- struct{}{}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				assertVisible("imported", 2, false)
				f.watermark(int64(2), int64(2), nil, nil, nil, nil)
				f.put(3, pushed, "updated")
				f.put(3, pushed, "duplicate")
				f.watermark(int64(3), int64(2), nil, "put", pushOne, int64(3))
				assertVisible("pushed", 3, false)
				f.put(4, one, "updated")
				_, err = f.other.PushProduct(ctx, 3, []byte(pushed))
				seen := int64(4)
				assertProductMutationError(t, err, "stale", &seen)
				assertVisible("original 1", 4, false)
				return
			}

			record, err := productobs.ParseImportRecord([]byte(pushed))
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := f.store.prepareProductEvent(ctx, productEvent{store: pushStore, productID: pushOne, op: "put", seq: 2}, &record)
			if err != nil {
				t.Fatal(err)
			}
			session := f.store.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
			defer session.Close(context.Background())
			tx, err := session.BeginTransaction(ctx, neo4j.WithTxTimeout(productPushTimeout))
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Close(context.Background())
			result, err := txApplyProductEvent(ctx, tx, prepared)
			if err != nil || result.Status != "updated" {
				t.Fatalf("staged push: %+v, %v", result, err)
			}
			// The actual push transaction body has returned, and commit is
			// withheld until the import on the separate driver has returned.
			assertVisible("original 1", 1, true)
			f.watermark(int64(1), int64(1), nil, nil, nil, nil)
			start := time.Now()
			_, err = f.importRows(f.other, pushA, 3, nil, imported, two)
			elapsed := time.Since(start)
			if !IsTransientWriteError(err) || errors.Is(err, ErrProductData) ||
				!strings.Contains(err.Error(), expectedConflict) || elapsed >= 5*time.Second {
				t.Fatalf("import did not conflict immediately: %v after %s", err, elapsed)
			}
			t.Logf("push held pointer; import lost after %s: %v", elapsed, err)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			assertVisible("pushed", 2, false)
			f.watermark(int64(2), int64(1), nil, "put", pushOne, int64(2))
			f.put(2, pushed, "duplicate")
			f.snapshot(pushA, 3, "imported", imported, two)
			f.watermark(int64(3), int64(3), nil, nil, nil, nil)
			assertVisible("imported", 3, false)
		})
	}
}

func TestProductPushNodeSeededSimilarity(t *testing.T) {
	f := newPushFixture(t)
	f.snapshot(pushA, 1, "imported",
		pushRecord(pushOne, "scalar", "null", `{"scalar":"a1"}`),
		pushRecord(pushTwo, "list", "null", `{"list":"A1 / B2"}`),
		pushRecord("push.test:3", "other list", "null", `{"list":"B2 / C3"}`),
		pushRecord("push.test:4", "unrelated", "null", `{"scalar":"X9"}`))
	f.query(`MATCH (p:Product {product_id:'push.test:3'}) SET p.domain='other'`, nil)
	const cypher = `
MATCH (m:GkgMeta {id:'product_release'})
MATCH (p:Product {id:$id})-[e1:HAS_FEATURE]->(f:FeatureRev)<-[e2:HAS_FEATURE]-(q:Product)
WHERE e1.release_id = m.value AND e2.release_id = m.value AND q <> p
AND (e1.value_fold IS NOT NULL AND (e2.value_fold = e1.value_fold OR e1.value_fold IN coalesce(e2.items_fold, []))
     OR any(v IN coalesce(e1.items_fold, []) WHERE v = e2.value_fold OR v IN coalesce(e2.items_fold, [])))
AND ($domain = '' OR q.domain = $domain)
RETURN q.id, q.title, collect(DISTINCT f.id) AS shared ORDER BY size(shared) DESC LIMIT 20`
	for _, tc := range []struct {
		id, domain string
		want       map[string]string
	}{
		{pushOne, "", map[string]string{pushTwo: "list"}},
		{pushTwo, "", map[string]string{pushOne: "scalar", "push.test:3": "other list"}},
		{pushTwo, pushDomain, map[string]string{pushOne: "scalar"}},
	} {
		rows, err := f.store.RunReadQuery(t.Context(), cypher,
			map[string]any{"id": productobs.ProductID(pushStore, tc.id), "domain": tc.domain}, 0)
		if err != nil || len(rows.Rows) != len(tc.want) {
			t.Fatalf("similarity for %s in %q = %+v, %v, want %v", tc.id, tc.domain, rows, err, tc.want)
		}
		want := make(map[string]string)
		for id, title := range tc.want {
			want[productobs.ProductID(pushStore, id)] = title
		}
		for _, row := range rows.Rows {
			id, _ := row["q.id"].(string)
			if want[id] != row["q.title"] || !reflect.DeepEqual(row["shared"], []any{"code"}) {
				t.Fatalf("unexpected similarity evidence: %v", row)
			}
			delete(want, id)
		}
	}
}
