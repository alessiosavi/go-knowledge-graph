package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

type ProductReleaseInput struct {
	ID          string
	RegistryRev string
	Candidates  []byte
	StoreDomain map[string]string
	ApprovedBy  string
	Pins        map[string]ProductReleasePin
}

type ProductReleaseCreated struct {
	ID             string
	ManifestDigest string
}

type productApproval struct {
	ApprovedAt string `json:"approved_at"`
	ApprovedBy string `json:"approved_by"`
}

// Field order, sorted maps and sorted triples define the manifest encoding.
type productManifest struct {
	Approval                productApproval            `json:"approval"`
	Pins                    map[string]string          `json:"pins"`
	RegistryRev             string                     `json:"registry_rev"`
	RegistrySHA256          string                     `json:"registry_sha256"`
	ReleaseCandidatesSHA256 string                     `json:"release_candidates_sha256"`
	StoreDomains            map[string]string          `json:"store_domains"`
	Triples                 []productobs.ReleaseTriple `json:"triples"`
}

// CreateProductRelease validates the release artifacts and atomically admits
// the release and its immutable store bindings.
func (s *Store) CreateProductRelease(ctx context.Context, input ProductReleaseInput) (ProductReleaseCreated, error) {
	var result ProductReleaseCreated
	if strings.TrimSpace(input.ApprovedBy) == "" || len(input.StoreDomain) == 0 {
		return result, fmt.Errorf("%w: release requires approval and store bindings", ErrProductData)
	}
	if missing := MissingProductReleasePins(input.Pins); len(missing) != 0 {
		return result, fmt.Errorf("%w: missing required pins: %s", ErrProductData, strings.Join(missing, ", "))
	}
	at := time.Now().UTC().Format(timeLayout)
	err := s.runProductWrite(ctx, func(tx neo4j.ManagedTransaction) error {
		registry, err := txProductRegistry(ctx, tx, input.RegistryRev)
		if err != nil {
			return err
		}
		release, err := registry.ParseRelease(input.Candidates)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrProductData, err)
		}
		manifest := productManifest{
			Approval: productApproval{ApprovedBy: input.ApprovedBy},
			Pins:     make(map[string]string, len(input.Pins)), RegistryRev: input.RegistryRev,
			RegistrySHA256: registry.Digest(), ReleaseCandidatesSHA256: productDigest(input.Candidates),
			StoreDomains: input.StoreDomain, Triples: release.Triples(),
		}
		releasedStores := make(map[string]struct{})
		for _, triple := range manifest.Triples {
			releasedStores[triple.Store] = struct{}{}
			if _, ok := input.StoreDomain[triple.Store]; !ok {
				return fmt.Errorf("%w: released store %q needs a -store-domain binding", ErrProductData, triple.Store)
			}
		}
		stores := slices.Sorted(maps.Keys(input.StoreDomain))
		for _, store := range stores {
			if _, ok := releasedStores[store]; !ok {
				return fmt.Errorf("%w: store %q does not appear in released triples", ErrProductData, store)
			}
		}
		for name, pin := range input.Pins {
			manifest.Pins[name] = pin.Digest
		}
		if err := validateProductReleasePins(input.Pins, manifest.RegistrySHA256, manifest.ReleaseCandidatesSHA256); err != nil {
			return err
		}
		id := input.ID
		if id == "" {
			// Approval time records the first creation; retries keep the same ID.
			key, err := json.Marshal(manifest)
			if err != nil {
				return err
			}
			id = "rel-" + productDigest(key)[:16]
		}
		params := map[string]any{"id": id}
		rows, err := txRows(ctx, tx, `MATCH (r:ProductRelease {id:$id})
			RETURN r.manifest_json AS manifest, r.manifest_digest AS digest, r.approved_at AS at`, params)
		if err != nil {
			return err
		}
		manifest.Approval.ApprovedAt = at
		if len(rows) != 0 {
			manifest.Approval.ApprovedAt = getString(rows[0], "at")
		}
		data, err := json.Marshal(manifest)
		if err != nil {
			return err
		}
		result = ProductReleaseCreated{ID: id, ManifestDigest: productDigest(data)}
		if len(rows) != 0 {
			if string(data) != getString(rows[0], "manifest") {
				return fmt.Errorf("%w: release ID %q already has a different manifest", ErrProductData, id)
			}
			if result.ManifestDigest != getString(rows[0], "digest") {
				return fmt.Errorf("stored release %q has an invalid manifest digest", id)
			}
			return nil
		}
		for _, store := range stores {
			if err := txBindProductStore(ctx, tx, store, input.StoreDomain[store]); err != nil {
				return err
			}
		}
		return txRun(ctx, tx, `
CREATE (:ProductRelease {id:$id, registry_rev:$rev, manifest_json:$manifest,
    manifest_digest:$digest, state:'created', approved_by:$by, approved_at:$at})`,
			map[string]any{"id": id, "rev": input.RegistryRev, "manifest": string(data),
				"digest": result.ManifestDigest, "by": input.ApprovedBy, "at": at})
	})
	return result, err
}

func txProductRegistry(ctx context.Context, tx neo4j.ManagedTransaction, rev string) (*productobs.Registry, error) {
	rows, err := txRows(ctx, tx, `MATCH (r:FeatureRegistry {rev:$rev})
		RETURN r.digest AS digest, r.registry_json AS registry_json`, map[string]any{"rev": rev})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: registry revision %q does not exist", ErrProductData, rev)
	}
	r, err := productobs.ParseRegistry([]byte(getString(rows[0], "registry_json")))
	if err != nil {
		return nil, fmt.Errorf("read stored registry %q: %w", rev, err)
	}
	if r.Digest() != getString(rows[0], "digest") {
		return nil, fmt.Errorf("stored registry %q has an invalid digest", rev)
	}
	return r, nil
}

func txBindProductStore(ctx context.Context, tx neo4j.ManagedTransaction, store, domain string) error {
	if err := txBumpProductDomainGuard(ctx, tx, store, domain); err != nil {
		return err
	}
	if err := txCheckDomainOpsForProductBinding(ctx, tx, domain); err != nil {
		return err
	}
	params := map[string]any{"store": store, "domain": domain}
	rows, err := txRows(ctx, tx, `MERGE (s:ProductStore {store:$store})
		ON CREATE SET s.seen_seq=0,s.snapshot_seq=0,s.product_count=0,s.edge_count=0
		SET s.domain=CASE WHEN s.domain IS NOT NULL THEN s.domain ELSE $domain END
		RETURN s.domain AS domain`, params)
	if err != nil {
		return err
	}
	if bound := getString(rows[0], "domain"); bound != domain {
		return fmt.Errorf("%w: store %q binding is %q, cannot bind to %q", ErrProductData, store, bound, domain)
	}
	return nil
}

func txBumpProductDomainGuard(ctx context.Context, tx neo4j.ManagedTransaction, store, domain string) error {
	// Domain mutations write the same counter, so domain operations and
	// changes to store bindings serialize by write-write conflict.
	rows, err := txRows(ctx, tx, `MATCH (d:Domain {id:$domain})
		SET d.product_guard = coalesce(d.product_guard, 0) + 1
		RETURN d.id`, map[string]any{"domain": domain})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("%w: domain %q for store %q does not exist", ErrProductData, domain, store)
	}
	return nil
}

func txCheckDomainOpsForProductBinding(ctx context.Context, tx neo4j.ManagedTransaction, domain string) error {
	// Move and undo record source domains on their documents or move
	// ledger. Undo can also have per-document destinations in targets.
	rows, err := txRows(ctx, tx, `
MATCH (o:DomainOp)
WHERE coalesce(o.state, '') <> 'completed' AND (
    o.from = $domain OR o.target = $domain
    OR any(k IN keys(coalesce(o.targets, {})) WHERE o.targets[k] = $domain)
    OR EXISTS { MATCH (d:Document {domain:$domain}) WHERE d.id IN o.docs }
    OR EXISTS { MATCH (r:DomainDecision {op_id:o.op_id})
                WHERE r.domain = $domain OR r.previous_domain = $domain }
)
RETURN o.op_id AS id, o.state AS state ORDER BY id LIMIT 1`, map[string]any{"domain": domain})
	if err != nil {
		return err
	}
	if len(rows) != 0 {
		return fmt.Errorf("%w: domain %q has %s operation %q", ErrProductData, domain,
			getString(rows[0], "state"), getString(rows[0], "id"))
	}
	return nil
}

func txProductManifest(ctx context.Context, tx neo4j.ManagedTransaction, id string) (productManifest, string, error) {
	var manifest productManifest
	rows, err := txRows(ctx, tx, `MATCH (r:ProductRelease {id:$id})
		RETURN r.state AS state,r.registry_rev AS rev,r.manifest_json AS manifest,r.manifest_digest AS digest`, map[string]any{"id": id})
	if err != nil {
		return manifest, "", err
	}
	if len(rows) == 0 {
		return manifest, "", fmt.Errorf("%w: release %q does not exist", ErrProductData, id)
	}
	row := rows[0]
	data := []byte(getString(row, "manifest"))
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, "", fmt.Errorf("read manifest for release %q: %w", id, err)
	}
	if productDigest(data) != getString(row, "digest") || manifest.RegistryRev != getString(row, "rev") {
		return manifest, "", fmt.Errorf("stored release %q has inconsistent manifest metadata", id)
	}
	return manifest, getString(row, "state"), nil
}

// ActivateProductRelease swaps the active pointer and removes all edges
// outside the new registry revision and release projection.
func (s *Store) ActivateProductRelease(ctx context.Context, id string) error {
	return s.runProductWrite(ctx, func(tx neo4j.ManagedTransaction) error {
		params := map[string]any{"id": id}
		manifest, state, err := txProductManifest(ctx, tx, id)
		if err != nil {
			return err
		}
		if state != "created" && state != "retired" {
			return fmt.Errorf("%w: release %q is %q, expected created or retired", ErrProductData, id, state)
		}
		if err := txRun(ctx, tx, `MERGE (m:GkgMeta {id:'product_release'})
			SET m.value=$id, m.activation_seq=coalesce(m.activation_seq, 0)+1`, params); err != nil {
			return err
		}
		if err := txRun(ctx, tx, `MATCH (r:ProductRelease) WHERE r.state='active' AND r.id <> $id SET r.state='retired'`, params); err != nil {
			return err
		}
		if err := txRun(ctx, tx, `MATCH (r:ProductRelease {id:$id}) SET r.state='active'`, params); err != nil {
			return err
		}
		released := make(map[string]map[string][]string, len(manifest.StoreDomains))
		for _, triple := range manifest.Triples {
			if released[triple.Store] == nil {
				released[triple.Store] = make(map[string][]string)
			}
			features := released[triple.Store]
			features[triple.Feature] = append(features[triple.Feature], triple.SourceKey)
		}
		pruned, err := txRows(ctx, tx, `
MATCH (p)-[e:HAS_FEATURE]->(f)
WHERE NOT coalesce(p:Product AND f:FeatureRev AND f.rev = $rev
    AND e.source_key IN coalesce(coalesce($released[coalesce(p.store, '')], {})[coalesce(f.id, '')], []), false)
WITH p.store AS store, e
DELETE e
RETURN DISTINCT store`, map[string]any{"rev": manifest.RegistryRev, "released": released})
		if err != nil || len(pruned) == 0 {
			return err
		}
		stores := make([]string, 0, len(pruned))
		for _, row := range pruned {
			if store := getString(row, "store"); store != "" {
				stores = append(stores, store)
			}
		}
		// Pruning leaves Product nodes and product_count intact.
		return txRun(ctx, tx, `
UNWIND $stores AS store
MATCH (s:ProductStore {store:store})
OPTIONAL MATCH (p:Product {store:store})-[e:HAS_FEATURE]->()
WITH s, count(e) AS edges
SET s.edge_count=edges`, map[string]any{"stores": stores})
	})
}

type ProductReleaseSummary struct {
	ID          string   `json:"id"`
	State       string   `json:"state"`
	RegistryRev string   `json:"registry_rev"`
	ApprovedBy  string   `json:"approved_by"`
	ApprovedAt  string   `json:"approved_at"`
	Stores      []string `json:"stores"`
}

func (s *Store) ListProductReleases(ctx context.Context) ([]ProductReleaseSummary, error) {
	rows, err := s.runRead(ctx, `
MATCH (r:ProductRelease)
RETURN r.id AS id,r.state AS state,r.registry_rev AS rev,
    r.approved_by AS approved_by,r.approved_at AS approved_at,r.manifest_json AS manifest
ORDER BY id`, nil)
	if err != nil {
		return nil, err
	}
	out := make([]ProductReleaseSummary, 0, len(rows))
	for _, row := range rows {
		id := getString(row, "id")
		var manifest productManifest
		if err := json.Unmarshal([]byte(getString(row, "manifest")), &manifest); err != nil {
			return nil, fmt.Errorf("read manifest for release %q: %w", id, err)
		}
		out = append(out, ProductReleaseSummary{ID: id, State: getString(row, "state"), RegistryRev: getString(row, "rev"),
			ApprovedBy: getString(row, "approved_by"), ApprovedAt: getString(row, "approved_at"),
			Stores: slices.Sorted(maps.Keys(manifest.StoreDomains))})
	}
	return out, nil
}
