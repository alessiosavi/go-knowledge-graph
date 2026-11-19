package judge

import (
	"cmp"
	"context"
	"slices"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
)

const (
	catalogPlanned   = "planned"
	catalogAmbiguous = "ambiguous"
	unverifiable     = "unverifiable:"
)

type validityInventoryRow struct {
	ID         string `json:"id"`
	EdgeID     int    `json:"edge_id"`
	Bucket     string `json:"bucket"`
	PriorJudge string `json:"prior_judge"`
}

type validityCatalog struct {
	inventory []validityInventoryRow
	planned   map[string]rejudgeEvidence
	occupants map[validity.DestinationKey][]validity.Occupant
}

func classifyValidityCatalog(edges []graph.CatalogEdge, reader graph.CatalogReader, definitions map[string]string) (validityCatalog, error) {
	catalog := validityCatalog{
		planned:   make(map[string]rejudgeEvidence),
		occupants: make(map[validity.DestinationKey][]validity.Occupant),
	}
	for _, edge := range edges {
		key := catalogEdgeKey(edge)
		catalog.occupants[key] = append(catalog.occupants[key], validity.Occupant{
			ID: edge.ID, Judge: edge.Judge, EdgeID: &edge.EdgeID,
		})
	}
	var hydrate []graph.CatalogEdge
	buckets := make(map[int]string)
	for _, edge := range edges {
		if !edge.InScope {
			continue
		}
		switch {
		case len(catalog.occupants[catalogEdgeKey(edge)]) > 1:
			buckets[edge.EdgeID] = catalogAmbiguous
		case !edge.DocumentExists:
			buckets[edge.EdgeID] = unverifiable + "missing-document"
		case !edge.ChunkExists:
			buckets[edge.EdgeID] = unverifiable + "missing-chunk"
		default:
			hydrate = append(hydrate, edge)
		}
	}
	unbuilt := make(map[string]string)
	err := reader.WalkFacts(hydrate, func(facts []graph.RejudgeFact) error {
		evidence := make([]rejudgeEvidence, 0, len(facts))
		for _, fact := range facts {
			if !fact.ChunkOwned {
				unbuilt[fact.ID] = unverifiable + "unowned-chunk"
				continue
			}
			e, reason, err := buildRejudgeEvidence(fact, definitions[fact.Relation])
			if err != nil {
				return err
			}
			if reason != "" {
				unbuilt[fact.ID] = unverifiable + reason
				continue
			}
			evidence = append(evidence, e)
		}
		duplicates := make(map[string]bool)
		if err := completeRejudgeEvidence(evidence, func(chunks map[string]string) (map[string]string, error) {
			previous, duplicateChunks, err := reader.PreviousChunks(chunks)
			for _, chunk := range duplicateChunks {
				duplicates[chunk] = true
				delete(previous, chunk)
			}
			return previous, err
		}); err != nil {
			return err
		}
		for _, e := range evidence {
			if e.tryPrevious && duplicates[e.fact.ChunkID] {
				unbuilt[e.fact.ID] = unverifiable + "multiple preceding chunks"
				continue
			}
			catalog.planned[e.fact.ID] = e
		}
		return nil
	})
	if err != nil {
		return validityCatalog{}, err
	}
	for _, edge := range edges {
		if !edge.InScope {
			continue
		}
		bucket := buckets[edge.EdgeID]
		if bucket == "" {
			bucket = unbuilt[edge.ID]
		}
		if bucket == "" {
			bucket = catalogPlanned
		}
		catalog.inventory = append(catalog.inventory, validityInventoryRow{edge.ID, edge.EdgeID, bucket, edge.Judge})
	}
	slices.SortFunc(catalog.inventory, func(a, b validityInventoryRow) int {
		return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.EdgeID, b.EdgeID))
	})
	return catalog, nil
}

func catalogEdgeKey(e graph.CatalogEdge) validity.DestinationKey {
	return validity.DestinationKey{HeadKey: e.HeadKey, Rel: e.Relation, TailKey: e.TailKey, ChunkID: e.ChunkID}
}

func inventoryArtifacts(preset string, inventory []validityInventoryRow) ([]validityFile, string, error) {
	type idRow struct {
		ID string `json:"id"`
	}
	var planned []idRow
	for _, row := range inventory {
		if row.Bucket == catalogPlanned {
			planned = append(planned, idRow{row.ID})
		}
	}
	inventoryBytes, err := validityJSONLines(inventory)
	if err != nil {
		return nil, "", err
	}
	ids, err := validityJSONLines(planned)
	if err != nil {
		return nil, "", err
	}
	digest := validityDigest([]byte(preset), inventoryBytes, ids)
	return []validityFile{
		{"inventory.jsonl", inventoryBytes},
		{"planned-ids.txt", ids},
		{"inventory.sha256", []byte(digest + "\n")},
		{"preset.txt", []byte(preset + "\n")},
	}, digest, nil
}

// ClassifyValidity publishes the catalog partition and the matching -ids input.
func ClassifyValidity(ctx context.Context, st *graph.Store, presetsDir, preset, out string) (string, error) {
	defs, err := loadRejudgeDefinitions(presetsDir, preset)
	if err != nil {
		return "", validityInputError(err)
	}
	var inventory []validityInventoryRow
	err = st.ReadValidityCatalog(ctx, preset, func(edges []graph.CatalogEdge, reader graph.CatalogReader) error {
		catalog, err := classifyValidityCatalog(edges, reader, defs)
		inventory = catalog.inventory
		return err
	})
	if err != nil {
		return "", err
	}
	files, digest, err := inventoryArtifacts(preset, inventory)
	if err != nil {
		return "", err
	}
	return digest, writeValidityFiles(ctx, out, files)
}
