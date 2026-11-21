package graph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const productPushTimeout = 30 * time.Second

type ProductMutationResult struct {
	Status    string `json:"status"`
	ProductID string `json:"product_id"`
	ID        string `json:"id"`
	SeenSeq   int64  `json:"seen_seq"`
}

// ProductMutationError carries a conflict status or unknown_outcome. SeenSeq
// is populated for the sequence refusals.
type ProductMutationError struct {
	Status  string
	SeenSeq *int64
	cause   error
}

func (e *ProductMutationError) Error() string {
	if e.SeenSeq != nil {
		return fmt.Sprintf("%s (seen_seq=%d)", e.Status, *e.SeenSeq)
	}
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.Status, e.cause)
	}
	return e.Status
}

func (e *ProductMutationError) Unwrap() error { return e.cause }

type productEvent struct {
	store, productID, op string
	seq                  int64
}

type preparedProductEvent struct {
	productEvent
	release, domain string
	activation      int64
	props           map[string]any
	edges           []map[string]any
}

// PushProduct validates and projects one A1 record, then applies it at seq.
func (s *Store) PushProduct(ctx context.Context, seq int64, rawRecord []byte) (ProductMutationResult, error) {
	if seq < 1 {
		return ProductMutationResult{}, argerr.New("seq must be a positive int64")
	}
	record, err := productobs.ParseImportRecord(rawRecord)
	if err != nil {
		return ProductMutationResult{}, argerr.Wrap(err)
	}
	store, err := productobs.ProductStoreFromID(record.ProductID)
	if err != nil {
		return ProductMutationResult{}, argerr.Wrap(err)
	}
	event := productEvent{store: store, productID: record.ProductID, op: "put", seq: seq}
	prepared, err := s.prepareProductEvent(ctx, event, &record)
	if err != nil {
		return ProductMutationResult{}, err
	}
	return s.writeProductEvent(ctx, prepared)
}

// RemoveProduct applies one delete event. An absent product still advances seq.
func (s *Store) RemoveProduct(ctx context.Context, productID string, seq int64) (ProductMutationResult, error) {
	if seq < 1 {
		return ProductMutationResult{}, argerr.New("seq must be a positive int64")
	}
	store, err := productobs.ProductStoreFromID(productID)
	if err != nil {
		return ProductMutationResult{}, argerr.Wrap(err)
	}
	event := productEvent{store: store, productID: productID, op: "delete", seq: seq}
	prepared, err := s.prepareProductEvent(ctx, event, nil)
	if err != nil {
		return ProductMutationResult{}, err
	}
	return s.writeProductEvent(ctx, prepared)
}

func (s *Store) prepareProductEvent(ctx context.Context, event productEvent, record *productobs.Record) (preparedProductEvent, error) {
	prepared := preparedProductEvent{productEvent: event}
	var manifest productManifest
	var registry *productobs.Registry
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := txRows(ctx, tx, `MATCH (m:GkgMeta {id:'product_release'})
RETURN m.value AS release, m.activation_seq AS activation_seq`, nil)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return &ProductMutationError{Status: "release_changed"}
		}
		prepared.release = getString(rows[0], "release")
		activation, null, err := neo4j.GetRecordValue[int64](rows[0], "activation_seq")
		if err != nil {
			return err
		}
		if null || prepared.release == "" {
			return &ProductMutationError{Status: "release_changed"}
		}
		prepared.activation = activation
		if record == nil {
			manifest, _, err = txProductManifest(ctx, tx, prepared.release)
		} else {
			manifest, registry, err = txProductReadRegistry(ctx, tx, prepared.release)
		}
		return err
	})
	if err != nil {
		return preparedProductEvent{}, err
	}
	prepared.domain = manifest.StoreDomains[event.store]
	if record == nil || prepared.domain == "" {
		return prepared, nil
	}
	release, err := registry.ProjectRelease(manifest.Triples)
	if err != nil {
		return preparedProductEvent{}, fmt.Errorf("read active release %q projection: %w", prepared.release, err)
	}
	observations := registry.Observations(*record, release)
	if err := validateProductObservations(observations); err != nil {
		return preparedProductEvent{}, argerr.Wrap(err)
	}
	prepared.props = productRecordProperties(*record, prepared.domain)
	id := prepared.props["id"].(string)
	propBytes := productPropertyBytes(prepared.props)
	for _, obs := range observations {
		edge, bytes := productProjectedEdge(*record, obs, registry, prepared.release, manifest.RegistryRev, id)
		propBytes += bytes
		prepared.edges = append(prepared.edges, edge)
	}
	if propBytes > maxProductImportPropBytes {
		return preparedProductEvent{}, &ProductMutationError{Status: "store_cap"}
	}
	prepared.props["prop_bytes"] = int64(propBytes)
	return prepared, nil
}

func (s *Store) writeProductEvent(ctx context.Context, event preparedProductEvent) (ProductMutationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, productPushTimeout)
	defer cancel()
	cleanup := context.WithoutCancel(ctx)
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(cleanup)
	tx, err := session.BeginTransaction(ctx, neo4j.WithTxTimeout(productPushTimeout))
	if err != nil {
		return ProductMutationResult{}, productEventWriteError(ctx, err)
	}
	defer tx.Close(cleanup)

	result, err := txApplyProductEvent(ctx, tx, event)
	if err != nil {
		return ProductMutationResult{}, productEventWriteError(ctx, err)
	}
	if result.Status == "duplicate" {
		return result, nil // Closing rolls back both provisional fence writes.
	}
	if err := tx.Commit(ctx); err != nil {
		var dbErr *neo4j.Neo4jError
		if !errors.As(err, &dbErr) {
			// A lost commit response doesn't establish whether the write landed.
			return ProductMutationResult{}, &ProductMutationError{Status: "unknown_outcome", cause: err}
		}
		return ProductMutationResult{}, productEventWriteError(ctx, err)
	}
	return result, nil
}

func productEventWriteError(ctx context.Context, err error) error {
	var eventErr *ProductMutationError
	switch {
	case errors.As(err, &eventErr):
		return err
	case IsTransientWriteError(err):
		return &ProductMutationError{Status: "retry", cause: err}
	case IsQueryTimeout(ctx, readError(ctx, err)):
		return &ProductMutationError{Status: "unknown_outcome", cause: err}
	default:
		return err
	}
}

func txApplyProductEvent(ctx context.Context, tx neo4j.ManagedTransaction, event preparedProductEvent) (ProductMutationResult, error) {
	id := productobs.ProductID(event.store, event.productID)
	params := map[string]any{"release": event.release, "act": event.activation, "store": event.store,
		"domain": event.domain, "seq": event.seq, "id": id, "product": event.productID, "op": event.op}
	pointer, err := txRows(ctx, tx, `
MATCH (m:GkgMeta {id:'product_release',value:$release,activation_seq:$act})
WITH m, m.touched AS prior
SET m.touched=coalesce(prior,0)+1
RETURN prior`, params)
	if err != nil {
		return ProductMutationResult{}, err
	}
	if len(pointer) != 1 {
		return ProductMutationResult{}, &ProductMutationError{Status: "release_changed"}
	}
	// Claim the store before touching its products. A refused event rolls
	// back the claim, including any temporary decrease of seen_seq.
	stores, err := txRows(ctx, tx, `
MATCH (s:ProductStore {store:$store})
WHERE s.domain IS NOT NULL AND s.domain=$domain
WITH s, coalesce(s.seen_seq,s.snapshot_seq,0) AS seen, coalesce(s.wiped_seq,0) AS wiped,
     s.last_event_op AS last_op, s.last_event_product_id AS last_product, s.last_event_seq AS last_seq
SET s.seen_seq=$seq
RETURN s.domain AS domain, s.materialized_activation_seq AS materialized, seen, wiped, last_op, last_product, last_seq`, params)
	if err != nil {
		return ProductMutationResult{}, err
	}
	if len(stores) != 1 {
		return ProductMutationResult{}, &ProductMutationError{Status: "store_not_bound"}
	}
	store := stores[0]
	result := ProductMutationResult{ID: id, ProductID: event.productID, SeenSeq: event.seq}
	status, err := productEventOrder(store, event)
	if err != nil {
		return ProductMutationResult{}, err
	}
	if status == "duplicate" {
		result.Status = status
		return result, nil
	}
	params["domain"] = getString(store, "domain")
	if event.op == "put" {
		result.Status, err = txUpsertPushedProduct(ctx, tx, params, event)
	} else {
		result.Status, err = txRemovePushedProduct(ctx, tx, params)
	}
	if err != nil {
		return ProductMutationResult{}, err
	}
	if err := txFinishProductEvent(ctx, tx, params); err != nil {
		return ProductMutationResult{}, err
	}
	prior, _ := pointer[0].Get("prior")
	if err := txRestoreProductPointer(ctx, tx, prior); err != nil {
		return ProductMutationResult{}, err
	}
	return result, nil
}

func productEventOrder(store *neo4j.Record, event preparedProductEvent) (string, error) {
	seen, _, seenErr := neo4j.GetRecordValue[int64](store, "seen")
	wiped, _, wipedErr := neo4j.GetRecordValue[int64](store, "wiped")
	materialized, null, materializedErr := neo4j.GetRecordValue[int64](store, "materialized")
	lastSeq, _, lastSeqErr := neo4j.GetRecordValue[int64](store, "last_seq")
	if seenErr != nil || wipedErr != nil || materializedErr != nil || lastSeqErr != nil {
		return "", fmt.Errorf("invalid sequence watermark for store %q", event.store)
	}
	switch {
	case event.seq <= wiped:
		return "", &ProductMutationError{Status: "stale", SeenSeq: &seen}
	case null || materialized != event.activation:
		return "", &ProductMutationError{Status: "release_not_materialized"}
	case event.seq == seen && event.seq == lastSeq && getString(store, "last_op") == event.op &&
		getString(store, "last_product") == event.productID:
		return "duplicate", nil
	case event.seq == seen:
		return "", &ProductMutationError{Status: "seq_conflict", SeenSeq: &seen}
	case event.seq < seen:
		return "", &ProductMutationError{Status: "stale", SeenSeq: &seen}
	default:
		return "", nil
	}
}

func txUpsertPushedProduct(ctx context.Context, tx neo4j.ManagedTransaction, params map[string]any, event preparedProductEvent) (string, error) {
	params["props"] = event.props
	rows, err := txRows(ctx, tx, `
OPTIONAL MATCH (previous:Product {id:$id})
WITH previous IS NOT NULL AS existed
MERGE (p:Product {id:$id})
SET p=$props, p.domain=$domain
RETURN existed`, params)
	if err != nil {
		return "", err
	}
	if len(rows) != 1 {
		return "", errors.New("expected one product after upsert")
	}
	if err := txRun(ctx, tx, `MATCH (:Product {id:$id})-[e:HAS_FEATURE]->() DELETE e`, params); err != nil {
		return "", err
	}
	if err := txCreateProductEdges(ctx, tx, event.edges); err != nil {
		return "", err
	}
	if getBool(rows[0], "existed") {
		return "updated", nil
	}
	return "created", nil
}

func txRemovePushedProduct(ctx context.Context, tx neo4j.ManagedTransaction, params map[string]any) (string, error) {
	rows, err := txRows(ctx, tx, `MATCH (p:Product {id:$id}) DETACH DELETE p RETURN count(*) AS deleted`, params)
	if err != nil {
		return "", err
	}
	if len(rows) != 1 || getInt(rows[0], "deleted") > 1 {
		return "", errors.New("unexpected product count after delete")
	}
	if getInt(rows[0], "deleted") == 1 {
		return "deleted", nil
	}
	return "absent", nil
}

func txFinishProductEvent(ctx context.Context, tx neo4j.ManagedTransaction, params map[string]any) error {
	rows, err := txRows(ctx, tx, `
MATCH (p:Product {store:$store})
OPTIONAL MATCH (p)-[e:HAS_FEATURE]->()
WITH p, count(e) AS edges
RETURN count(p) AS products, sum(edges) AS edges, sum(p.prop_bytes) AS bytes,
       count(p.prop_bytes) AS tracked`, params)
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return errors.New("expected one product store tally")
	}
	row := rows[0]
	products, edges, bytes := getInt(row, "products"), getInt(row, "edges"), getInt(row, "bytes")
	if products > productobs.MaxImportProducts || edges > maxProductImportEdges || bytes > maxProductImportPropBytes {
		return &ProductMutationError{Status: "store_cap"}
	}
	if getInt(row, "tracked") != products {
		return errors.New("missing projected product bytes in materialized store")
	}
	params["products"], params["edges"], params["bytes"] = int64(products), int64(edges), int64(bytes)
	params["at"] = time.Now().UTC().Format(timeLayout)
	return txRun(ctx, tx, `
MATCH (s:ProductStore {store:$store})
SET s.snapshot_digest=null, s.release_id=$release, s.product_count=$products, s.edge_count=$edges,
    s.prop_bytes=$bytes, s.pushed_at=$at, s.last_event_op=$op, s.last_event_product_id=$product,
    s.last_event_seq=$seq`, params)
}
