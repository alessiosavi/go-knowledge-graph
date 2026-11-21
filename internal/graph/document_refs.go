package graph

import (
	"context"
	"fmt"
	"slices"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// DocumentRefsError is an input or lifecycle rejection that callers can
// report even when it is wrapped by an ingest store error.
type DocumentRefsError struct {
	Code   string
	Detail string
}

func (e *DocumentRefsError) Error() string { return e.Code + ": " + e.Detail }

// ValidateProductRefs keeps exact A1 identities and returns their canonical
// order. Nil means omitted; a non-nil empty list explicitly clears refs.
func ValidateProductRefs(refs []string) ([]string, error) {
	if len(refs) > 50 {
		return nil, &DocumentRefsError{Code: "invalid_products", Detail: "products can contain at most 50 IDs"}
	}
	canonical := slices.Clone(refs)
	for _, id := range canonical {
		if _, err := productobs.ProductStoreFromID(id); err != nil {
			return nil, &DocumentRefsError{Code: "invalid_products", Detail: err.Error()}
		}
	}
	slices.Sort(canonical)
	if len(slices.Compact(canonical)) != len(canonical) {
		return nil, &DocumentRefsError{Code: "invalid_products", Detail: "duplicate product ID"}
	}
	return canonical, nil
}

// CheckProductRefsNoop enforces the same refs rule on both pipeline no-op
// paths and on the final recheck in the upsert transaction.
func CheckProductRefsNoop(supplied, stored []string) error {
	if supplied != nil && !slices.Equal(supplied, stored) {
		return &DocumentRefsError{Code: "refs_changed_use_force", Detail: "unchanged content requires force to change product refs"}
	}
	return nil
}

const newestDoneRefsQuery = `
MATCH (d:Document {source_kind:$kind, source:$source, status:'done'})
RETURN d.product_refs_raw AS refs
ORDER BY coalesce(d.version_at, d.created_at) DESC, d.id DESC LIMIT 1`

// PrepareDocumentRefs checks the admission snapshot before the payload is
// written. UpsertDocument rechecks lifecycle rules against its own snapshot,
// reusing the admitted binding if the product set still needs validation.
func (s *Store) PrepareDocumentRefs(ctx context.Context, in *DocumentInput, current *Document) error {
	var stored []string
	if current != nil {
		stored = current.ProductRefs
	} else {
		rows, err := s.runRead(ctx, newestDoneRefsQuery, map[string]any{"kind": in.Kind, "source": in.Source})
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			stored = getStringSlice(rows[0], "refs")
		}
	}
	if slices.Equal(in.ProductRefs, stored) {
		return nil
	}
	pin, err := productRefDomain(in.ProductRefs, func(query string, params map[string]any) ([]*neo4j.Record, error) {
		return s.runRead(ctx, query, params)
	})
	if err != nil {
		return err
	}
	if current != nil {
		if err := checkProductRefPin(pin, current.Domain, current.Status); err != nil {
			return err
		}
	}
	in.Domain, in.productRefsValidated = pin, true
	return nil
}

func productRefDomain(refs []string, read func(string, map[string]any) ([]*neo4j.Record, error)) (string, error) {
	stores := make([]string, 0, len(refs))
	for _, ref := range refs {
		store, err := productobs.ProductStoreFromID(ref)
		if err != nil {
			return "", &DocumentRefsError{Code: "invalid_products", Detail: err.Error()}
		}
		stores = append(stores, store)
	}
	stores = slices.Compact(stores)
	rows, err := read(`
UNWIND $stores AS store
OPTIONAL MATCH (s:ProductStore {store:store})
RETURN store AS store, s.domain AS domain ORDER BY store`, map[string]any{"stores": stores})
	if err != nil {
		return "", err
	}
	var pin string
	multiple := false
	for _, r := range rows {
		domain := getString(r, "domain")
		if domain == "" {
			return "", &DocumentRefsError{Code: "products_unknown_store", Detail: fmt.Sprintf("product store %q is unknown or unbound", getString(r, "store"))}
		}
		if pin == "" {
			pin = domain
		}
		multiple = multiple || domain != pin
	}
	if multiple {
		return "", nil
	}
	return pin, nil
}

func checkProductRefPin(pin, current string, status Status) error {
	if pin != "" && current != "" && pin != current && status != StatusSuperseded {
		return &DocumentRefsError{Code: "domain_pin_conflict", Detail: fmt.Sprintf("product refs pin domain %q but this version is assigned to %q; delete it before re-ingesting", pin, current)}
	}
	return nil
}

func txCheckProductRefDomain(ctx context.Context, tx neo4j.ManagedTransaction, pin string) error {
	// Empty-domain deletion and binding changes write this same Domain
	// property, preventing a concurrent deletion under snapshot isolation.
	rows, err := txRows(ctx, tx, `
MATCH (d:Domain {id:$domain})
SET d.product_guard = coalesce(d.product_guard, 0) + 1
RETURN d.id`, map[string]any{"domain": pin})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return &DocumentRefsError{Code: "domain_missing", Detail: fmt.Sprintf("product refs pin domain %q, which does not exist", pin)}
	}
	return nil
}

func txReplaceProductRefs(ctx context.Context, tx neo4j.ManagedTransaction, id string, refs []string) error {
	if len(refs) == 0 {
		refs = []string{}
	}
	if err := txRun(ctx, tx, `
MATCH (d:Document {id:$id})
SET d.product_refs_raw = $refs
WITH d MATCH (d)-[r:REFS]->(:ProductRef) DELETE r`, map[string]any{"id": id, "refs": refs}); err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	anchors := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		store, err := productobs.ProductStoreFromID(ref)
		if err != nil {
			return err
		}
		anchors = append(anchors, map[string]any{"id": productobs.ProductID(store, ref), "store": store, "product_id": ref})
	}
	return txRun(ctx, tx, `
MATCH (d:Document {id:$id})
UNWIND $anchors AS row
MERGE (a:ProductRef {id:row.id})
ON CREATE SET a.store = row.store, a.product_id = row.product_id
MERGE (d)-[:REFS]->(a)`, map[string]any{"id": id, "anchors": anchors})
}
