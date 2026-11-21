package graph

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const productImportTimeout = 300 * time.Second

type ProductSnapshotInput struct {
	Store, Release string
	Seq            int64
	AllowShrink    bool
}

// ImportProductSnapshot projects a preflighted, rewound input against an
// immutable release and replaces the store in one non-retried transaction.
func (s *Store) ImportProductSnapshot(ctx context.Context, input ProductSnapshotInput, source io.Reader, report *ProductImportReport, stageReport func() error) error {
	var manifest productManifest
	var registry *productobs.Registry
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		m, _, err := txProductManifest(ctx, tx, input.Release)
		if err != nil {
			return err
		}
		if m.StoreDomains[input.Store] == "" {
			return fmt.Errorf("%w: release %q has no binding for store %q", ErrProductData, input.Release, input.Store)
		}
		r, err := txProductRegistry(ctx, tx, m.RegistryRev)
		if err != nil {
			return err
		}
		if r.Digest() != m.RegistrySHA256 {
			return fmt.Errorf("registry bytes do not match release %q", input.Release)
		}
		manifest, registry = m, r
		return nil
	})
	if err != nil {
		return err
	}

	release, err := registry.ProjectRelease(manifest.Triples)
	if err != nil {
		return fmt.Errorf("read stored release %q projection: %w", input.Release, err)
	}
	domain := manifest.StoreDomains[input.Store]
	batch, err := prepareProductReplacement(ctx, input, source, domain, manifest.RegistryRev, registry, release, report)
	if err != nil {
		return err
	}
	if err := s.checkProductImportTimeout(ctx); err != nil {
		return err
	}
	return s.writeProductSnapshot(ctx, input, domain, batch, report, stageReport)
}

func (s *Store) checkProductImportTimeout(ctx context.Context) error {
	rows, err := s.runRead(ctx, "SHOW CONFIG", nil)
	if err != nil {
		return fmt.Errorf("read Memgraph query execution timeout: %w", err)
	}
	for _, row := range rows {
		if getString(row, "name") == "query_execution_timeout_sec" {
			return validateProductImportTimeout(getString(row, "current_value"))
		}
	}
	return fmt.Errorf("Memgraph SHOW CONFIG did not report query_execution_timeout_sec")
}

func validateProductImportTimeout(current string) error {
	seconds, err := strconv.ParseFloat(current, 64)
	if err == nil && (seconds == 0 || seconds >= productImportTimeout.Seconds()) {
		return nil
	}
	return fmt.Errorf("Memgraph --query-execution-timeout-sec must be 0 (unlimited) or at least %.0f seconds (current: %q)",
		productImportTimeout.Seconds(), current)
}

func (s *Store) writeProductSnapshot(ctx context.Context, input ProductSnapshotInput, domain string,
	batch productReplacement, report *ProductImportReport, stageReport func() error) error {
	cleanup := context.WithoutCancel(ctx)
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(cleanup)
	tx, err := session.BeginTransaction(ctx, neo4j.WithTxTimeout(productImportTimeout))
	if err != nil {
		return err
	}
	defer tx.Close(cleanup)

	params := map[string]any{"release": input.Release, "store": input.Store, "domain": domain, "seq": input.Seq,
		"digest": report.SnapshotDigest, "products": int64(len(batch.products)), "edges": int64(len(batch.edges)),
		"at": time.Now().UTC().Format(timeLayout), "bytes": int64(report.PropertyBytes)}
	pointer, err := txRows(ctx, tx, `
MATCH (m:GkgMeta {id:'product_release',value:$release})
WITH m, m.touched AS prior
SET m.touched=coalesce(prior, 0)+1
RETURN prior, m.activation_seq AS activation_seq`, params)
	if err != nil {
		return err
	}
	if len(pointer) != 1 {
		return fmt.Errorf("%w: release %q is not active", ErrProductData, input.Release)
	}

	stores, err := txRows(ctx, tx, `
MATCH (s:ProductStore {store:$store})
WHERE s.domain IS NOT NULL
RETURN s.domain AS domain, coalesce(s.seen_seq, s.snapshot_seq, 0) AS seen_seq,
    coalesce(s.wiped_seq, 0) AS wiped_seq, coalesce(s.snapshot_seq, 0) AS snapshot_seq,
    s.snapshot_digest AS digest, s.release_id AS release, s.prop_bytes AS prop_bytes,
    coalesce(s.product_count, 0) AS products, coalesce(s.edge_count, 0) AS edges,
    EXISTS { MATCH (:Domain {id:$domain}) } AS domain_exists`, params)
	if err != nil {
		return err
	}
	if len(stores) == 0 || getString(stores[0], "domain") != domain {
		return fmt.Errorf("%w: store %q requires its release binding to domain %q", ErrProductData, input.Store, domain)
	}
	store := stores[0]
	if exists, _ := store.Get("domain_exists"); exists != true {
		return fmt.Errorf("%w: bound domain %q for store %q does not exist", ErrProductData, domain, input.Store)
	}
	seen, _, seenErr := neo4j.GetRecordValue[int64](store, "seen_seq")
	wiped, _, wipedErr := neo4j.GetRecordValue[int64](store, "wiped_seq")
	seq, _, seqErr := neo4j.GetRecordValue[int64](store, "snapshot_seq")
	if seenErr != nil || wipedErr != nil || seqErr != nil {
		return fmt.Errorf("invalid sequence watermark for store %q", input.Store)
	}
	if input.Seq <= seen || input.Seq <= wiped {
		return fmt.Errorf("%w: stale snapshot sequence %d (seen_seq=%d, wiped_seq=%d)", ErrProductData, input.Seq, seen, wiped)
	}
	report.PreviousProductCount = getInt(store, "products")
	shrink := int64(len(batch.products))*10 < int64(report.PreviousProductCount)*9
	if shrink && !input.AllowShrink {
		return fmt.Errorf("%w: snapshot would shrink from %d to %d products by more than 10%%; use -allow-shrink to approve",
			ErrProductData, report.PreviousProductCount, len(batch.products))
	}

	// Write the store before touching products. Wipes and unbinds also write
	// this node, so a concurrent administrative change cannot slip through.
	if err := txRun(ctx, tx, `MATCH (s:ProductStore {store:$store}) SET s.seen_seq=$seq`, params); err != nil {
		return err
	}
	unchanged := getString(store, "digest") == report.SnapshotDigest && getString(store, "release") == input.Release &&
		seq > wiped && report.PreviousProductCount == len(batch.products) && getInt(store, "edges") == len(batch.edges)
	if !unchanged {
		if err := txReplaceProducts(ctx, tx, params, batch); err != nil {
			return err
		}
	} else if bytes, _ := store.Get("prop_bytes"); bytes == nil {
		if err := txBackfillProductBytes(ctx, tx, batch.products); err != nil {
			return err
		}
	}
	params["act"], _ = pointer[0].Get("activation_seq")
	if err := txRun(ctx, tx, `MATCH (s:ProductStore {store:$store})
SET s.materialized_activation_seq=$act, s.prop_bytes=$bytes,
    s.last_event_op=null, s.last_event_product_id=null, s.last_event_seq=null`, params); err != nil {
		return err
	}
	// The transaction retains its write conflict with activation. Restore the
	// pointer's prior state on commit, including an originally absent property.
	params["prior"], _ = pointer[0].Get("prior")
	if err := txRestoreProductPointer(ctx, tx, params["prior"]); err != nil {
		return err
	}
	report.Status = "imported"
	report.ShrinkAllowed = shrink
	if unchanged {
		report.Status = "unchanged"
	}
	// Stage the report before committing so a failed report write rolls back
	// the graph transaction. The CLI publishes it after commit.
	if stageReport != nil {
		if err := stageReport(); err != nil {
			return fmt.Errorf("stage product import report: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit product snapshot: %w", err)
	}
	return nil
}

const deleteStoreProducts = `MATCH (p:Product {store:$store}) DETACH DELETE p`

func txReplaceProducts(ctx context.Context, tx neo4j.ExplicitTransaction, params map[string]any, batch productReplacement) error {
	if err := txRun(ctx, tx, deleteStoreProducts, params); err != nil {
		return fmt.Errorf("delete previous snapshot: %w", err)
	}
	for start := 0; start < len(batch.products); start += 512 {
		rows := batch.products[start:min(start+512, len(batch.products))]
		if err := txRun(ctx, tx, `UNWIND $rows AS props CREATE (p:Product) SET p=props`, map[string]any{"rows": rows}); err != nil {
			return fmt.Errorf("create snapshot products: %w", err)
		}
	}
	if err := txCreateProductEdges(ctx, tx, batch.edges); err != nil {
		return err
	}
	return txRun(ctx, tx, `
MATCH (s:ProductStore {store:$store})
SET s.snapshot_seq=$seq, s.snapshot_digest=$digest, s.release_id=$release,
    s.product_count=$products, s.edge_count=$edges, s.imported_at=$at`, params)
}

func txCreateProductEdges(ctx context.Context, tx neo4j.ManagedTransaction, edges []map[string]any) error {
	for start := 0; start < len(edges); start += 512 {
		rows := edges[start:min(start+512, len(edges))]
		created, err := txRows(ctx, tx, `
UNWIND $rows AS row
MATCH (p:Product {id:row.product}), (f:FeatureRev {key:row.feature})
CREATE (p)-[e:HAS_FEATURE]->(f)
SET e=row.props
RETURN count(e) AS created`, map[string]any{"rows": rows})
		if err != nil {
			return fmt.Errorf("create product feature edges: %w", err)
		}
		if len(created) != 1 || getInt(created[0], "created") != len(rows) {
			return fmt.Errorf("missing feature revision or product when creating feature edges")
		}
	}
	return nil
}

func txBackfillProductBytes(ctx context.Context, tx neo4j.ManagedTransaction, products []map[string]any) error {
	for start := 0; start < len(products); start += 512 {
		batch := products[start:min(start+512, len(products))]
		rows := make([]map[string]any, 0, len(batch))
		for _, props := range batch {
			rows = append(rows, map[string]any{"id": props["id"], "bytes": props["prop_bytes"]})
		}
		updated, err := txRows(ctx, tx, `UNWIND $rows AS row MATCH (p:Product {id:row.id})
SET p.prop_bytes=row.bytes RETURN count(p) AS updated`, map[string]any{"rows": rows})
		if err != nil {
			return err
		}
		if len(updated) != 1 || getInt(updated[0], "updated") != len(rows) {
			return fmt.Errorf("missing product when backfilling projected bytes")
		}
	}
	return nil
}

func txRestoreProductPointer(ctx context.Context, tx neo4j.ManagedTransaction, prior any) error {
	return txRun(ctx, tx, `MATCH (m:GkgMeta {id:'product_release'}) SET m.touched=$prior`, map[string]any{"prior": prior})
}

// DeleteProductStore wipes the current snapshot and retains its binding and
// sequence watermark as a tombstone.
func (s *Store) DeleteProductStore(ctx context.Context, store string) error {
	params := map[string]any{"store": store}
	return s.runProductWrite(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := txRows(ctx, tx, `
MATCH (s:ProductStore {store:$store})
WHERE s.domain IS NOT NULL
SET s.wiped_seq=coalesce(s.seen_seq, s.snapshot_seq, 0), s.product_count=0, s.edge_count=0,
    s.snapshot_digest=null, s.release_id=null, s.imported_at=null, s.prop_bytes=0,
    s.materialized_activation_seq=null, s.last_event_op=null, s.last_event_product_id=null, s.last_event_seq=null
RETURN s.store`, params)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("%w: store %q has no binding", ErrProductData, store)
		}
		return txRun(ctx, tx, deleteStoreProducts, params)
	})
}

// UnbindProductStore removes an empty binding and advances the bound domain's guard.
func (s *Store) UnbindProductStore(ctx context.Context, store string) error {
	params := map[string]any{"store": store}
	return s.runProductWrite(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := txRows(ctx, tx, `
MATCH (s:ProductStore {store:$store})
WHERE s.domain IS NOT NULL
RETURN s.domain AS domain, (coalesce(s.product_count, 0)>0 OR EXISTS { MATCH (:Product {store:$store}) }) AS nonempty`, params)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("%w: store %q has no binding", ErrProductData, store)
		}
		if nonempty, _ := rows[0].Get("nonempty"); nonempty == true {
			return fmt.Errorf("%w: store %q still has products", ErrProductData, store)
		}
		if err := txBumpProductDomainGuard(ctx, tx, store, getString(rows[0], "domain")); err != nil {
			return err
		}
		return txRun(ctx, tx, `MATCH (s:ProductStore {store:$store}) SET s.domain=null`, params)
	})
}
