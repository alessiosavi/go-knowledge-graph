package graph

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// MergeFoldBlock identifies an ID conflict and the data needed to inspect it.
type MergeFoldBlock struct {
	Label         string         `json:"label"`
	SourceID      string         `json:"source_id"`
	TargetID      string         `json:"target_id"`
	Rewritten     MergeFoldTuple `json:"rewritten"`
	ConflictingID string         `json:"conflicting_id"`
	Conflicting   MergeFoldTuple `json:"conflicting"`
	Projected     bool           `json:"projected,omitempty"`
}

type MergeFoldTuple struct {
	HeadKey string `json:"head_key"`
	Rel     string `json:"rel"`
	TailKey string `json:"tail_key"`
	ChunkID string `json:"chunk_id"`
	DocID   string `json:"doc_id,omitempty"` // Disambiguates legacy chunkless rows.
}

const mergePlanFactFields = `id(f) AS row_id, f.id AS id, f.head_key AS head_key, f.rel AS rel,
f.tail_key AS tail_key, f.chunk_id AS chunk_id, f.doc_id AS doc_id`

type mergePlanFact struct {
	rowID int
	id    string
	tuple MergeFoldTuple
}

func mergePlanFactFromRow(row *neo4j.Record) mergePlanFact {
	tuple := MergeFoldTuple{
		HeadKey: getString(row, "head_key"), Rel: getString(row, "rel"),
		TailKey: getString(row, "tail_key"), ChunkID: getString(row, "chunk_id"),
	}
	if tuple.ChunkID == "" {
		tuple.DocID = getString(row, "doc_id")
	}
	return mergePlanFact{rowID: getInt(row, "row_id"), id: getString(row, "id"), tuple: tuple}
}

type mergePlanProjection struct {
	source      mergePlanFact
	tuple       MergeFoldTuple
	groups      []*MergeFoldGroup
	hasExisting bool
	blocked     bool
}

func loadMergeFoldFacts(ctx context.Context, tx neo4j.ManagedTransaction, keys []string, members map[string]*MergeFoldMember, losers map[string]*MergeFoldGroup) error {
	for _, kind := range []struct {
		label     string
		projectID func(string, string, string, string) string
	}{
		{"FactCandidate", CandidateID},
		{"RejectedFact", FactAuditID},
	} {
		query := `UNWIND $keys AS key
MATCH (f:` + kind.label + ` {head_key: key})
RETURN key, ` + mergePlanFactFields + `
UNION ALL
UNWIND $keys AS key
MATCH (f:` + kind.label + ` {tail_key: key}) WHERE f.head_key IS NULL OR f.head_key <> key
RETURN key, ` + mergePlanFactFields
		affected := make(map[int]mergePlanFact)
		for batch := range slices.Chunk(keys, mergePlanStatsBatch) {
			err := readMergePlanFacts(ctx, tx, query, map[string]any{"keys": batch}, func(row *neo4j.Record) {
				key := getString(row, "key")
				member := members[key]
				if kind.label == "FactCandidate" {
					member.Candidates++
				} else {
					member.Rejected++
				}
				if losers[key] != nil {
					fact := mergePlanFactFromRow(row)
					affected[fact.rowID] = fact
				}
			})
			if err != nil {
				return fmt.Errorf("graph: read merge plan %s references: %w", kind.label, err)
			}
		}
		projections := make(map[string][]*mergePlanProjection)
		for _, fact := range affected {
			tuple := fact.tuple
			var groups []*MergeFoldGroup
			head, tail := losers[tuple.HeadKey], losers[tuple.TailKey]
			if head != nil {
				tuple.HeadKey = head.Canonical
				groups = append(groups, head)
			}
			if tail != nil {
				tuple.TailKey = tail.Canonical
				if tail != head {
					groups = append(groups, tail)
				}
			}
			for _, group := range groups {
				group.Rewire++
			}
			targetID := kind.projectID(tuple.HeadKey, tuple.Rel, tuple.TailKey, tuple.ChunkID)
			projections[targetID] = append(projections[targetID], &mergePlanProjection{source: fact, tuple: tuple, groups: groups})
		}
		if err := checkMergeFoldIDs(ctx, tx, kind.label, projections); err != nil {
			return fmt.Errorf("graph: precheck merge plan %s IDs: %w", kind.label, err)
		}
	}
	return nil
}

func checkMergeFoldIDs(ctx context.Context, tx neo4j.ManagedTransaction, label string, projections map[string][]*mergePlanProjection) error {
	if len(projections) == 0 {
		return nil
	}
	checkExisting := func(row *neo4j.Record) {
		existing := mergePlanFactFromRow(row)
		for _, p := range projections[existing.id] {
			switch {
			case existing.rowID == p.source.rowID:
			case p.tuple == existing.tuple:
				p.hasExisting = true
			default:
				p.block(label, existing.id, existing.id, existing.tuple, false)
			}
		}
	}
	ids := slices.Sorted(maps.Keys(projections))
	query := `UNWIND $ids AS id MATCH (f:` + label + ` {id: id}) RETURN ` + mergePlanFactFields
	for batch := range slices.Chunk(ids, mergePlanStatsBatch) {
		if err := readMergePlanFacts(ctx, tx, query, map[string]any{"ids": batch}, checkExisting); err != nil {
			return err
		}
	}
	for _, id := range ids {
		checkMergeFoldProjections(label, id, projections[id])
	}
	return nil
}

func readMergePlanFacts(ctx context.Context, tx neo4j.ManagedTransaction, query string, params map[string]any, visit func(*neo4j.Record)) error {
	result, err := tx.Run(ctx, query, params)
	if err != nil {
		return err
	}
	for result.Next(ctx) {
		visit(result.Record())
	}
	return result.Err()
}

func checkMergeFoldProjections(label, id string, entries []*mergePlanProjection) {
	slices.SortFunc(entries, func(a, b *mergePlanProjection) int {
		return cmp.Or(cmp.Compare(a.source.id, b.source.id), cmp.Compare(a.source.rowID, b.source.rowID))
	})
	first := entries[0]
	for _, p := range entries[1:] {
		if p.tuple != first.tuple {
			// Show each source a conflicting tuple, even if the new ID
			// currently has no occupant in the graph.
			for _, entry := range entries {
				other := first
				if entry.tuple == first.tuple {
					other = p
				}
				entry.block(label, id, other.source.id, other.tuple, true)
			}
			return
		}
	}
	occupied := false
	for _, p := range entries {
		// A blocked projection cannot occupy or collapse into the ID.
		if p.blocked {
			continue
		}
		if p.hasExisting || occupied {
			for _, group := range p.groups {
				group.Collapses++
			}
		}
		occupied = true
	}
}

func (p *mergePlanProjection) block(label, targetID, conflictingID string, conflicting MergeFoldTuple, projected bool) {
	p.blocked = true
	for _, group := range p.groups {
		group.Blocking = append(group.Blocking, MergeFoldBlock{
			Label: label, SourceID: p.source.id, TargetID: targetID, Rewritten: p.tuple,
			ConflictingID: conflictingID, Conflicting: conflicting, Projected: projected,
		})
	}
}
