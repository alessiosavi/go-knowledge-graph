package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// CatalogEdge identifies a live typed edge. EdgeID distinguishes physical
// relationships even when several share the same FactAuditID.
type CatalogEdge struct {
	ID, HeadKey, Relation, TailKey, ChunkID, Domain, Judge string
	EdgeID                                                 int
	InScope, DocumentExists, ChunkExists                   bool
}

// CatalogReader reads evidence and title matches in the inventory snapshot.
// It is valid only during the ReadValidityCatalog callback.
type CatalogReader struct{ read rejudgeRead }

// ReadValidityCatalog scans all typed edges once. Edges outside the preset
// still participate in occupancy and ambiguity checks. A broken document link
// can be attributed by the chunk's owner. An orphan with neither link is
// reported for any selected preset, since it cannot safely be attributed.
func (s *Store) ReadValidityCatalog(ctx context.Context, preset string, visit func([]CatalogEdge, CatalogReader) error) error {
	return s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		read := func(query string, params map[string]any) ([]*neo4j.Record, error) {
			result, err := tx.Run(ctx, query, params)
			if err != nil {
				return nil, err
			}
			return result.Collect(ctx)
		}
		rows, err := read(`MATCH (h:Entity)-[r]->(t:Entity)
OPTIONAL MATCH (d:Document {id:r.doc_id})
OPTIONAL MATCH (c:Chunk {id:r.chunk_id})
OPTIONAL MATCH (owner:Document {id:c.doc_id})
RETURN h.key AS head, t.key AS tail, type(r) AS rel, r.chunk_id AS chunk, h.domain AS domain,
       id(r) AS edge, r.judge AS judge, r.judge IN ['confirmed', ''] AS eligible,
       d.id IS NOT NULL AS has_doc, c.id IS NOT NULL AS has_chunk,
       (d.labels = $preset OR (d IS NULL AND (owner IS NULL OR owner.labels = $preset))) AS selected`,
			map[string]any{"preset": preset})
		if err != nil {
			return fmt.Errorf("graph: scan validity inventory: %w", err)
		}
		edges := make([]CatalogEdge, 0, len(rows))
		for _, row := range rows {
			head, rel := getString(row, "head"), getString(row, "rel")
			tail, chunk := getString(row, "tail"), getString(row, "chunk")
			edges = append(edges, CatalogEdge{
				ID: FactAuditID(head, rel, tail, chunk), HeadKey: head, Relation: rel, TailKey: tail, ChunkID: chunk,
				Domain: getString(row, "domain"), EdgeID: getInt(row, "edge"), Judge: getString(row, "judge"),
				InScope: getBool(row, "selected") && getBool(row, "eligible"), DocumentExists: getBool(row, "has_doc"),
				ChunkExists: getBool(row, "has_chunk"),
			})
		}
		return visit(edges, CatalogReader{read})
	})
}

// WalkFacts hydrates eligible, unambiguous inventory edges in bounded batches.
func (r CatalogReader) WalkFacts(edges []CatalogEdge, visit func([]RejudgeFact) error) error {
	for start := 0; start < len(edges); start += rejudgeBatchSize {
		batch := edges[start:min(start+rejudgeBatchSize, len(edges))]
		refs := make([]map[string]any, 0, len(batch))
		for _, edge := range batch {
			refs = append(refs, map[string]any{
				"id": edge.ID, "head_key": edge.HeadKey, "rel": edge.Relation, "tail_key": edge.TailKey,
				"chunk_id": edge.ChunkID, "edge": edge.EdgeID, "domain": edge.Domain, "idx": len(refs),
			})
		}
		facts, err := readRejudgeFacts(r.read, refs)
		if err != nil {
			return err
		}
		if len(facts) != len(refs) {
			return fmt.Errorf("graph: hydrated %d validity facts, expected %d", len(facts), len(refs))
		}
		if err := visit(facts); err != nil {
			return err
		}
	}
	return nil
}

func (r CatalogReader) PreviousChunks(chunks map[string]string) (map[string]string, []string, error) {
	return rejudgePreviousChunks(r.read, chunks)
}

type CatalogTitleMatch struct{ Key, Name, Type string }

// TitleMatches resolves matches over all owned chunks, once per document.
func (r CatalogReader) TitleMatches(docIDs []string) (map[string][]CatalogTitleMatch, error) {
	matches := make(map[string][]CatalogTitleMatch)
	for start := 0; start < len(docIDs); start += rejudgeBatchSize {
		rows, err := r.read(`UNWIND $docs AS doc
MATCH (d:Document {id:doc})-[:HAS_CHUNK]->(c:Chunk)-[:MENTIONS]->(e:Entity)
WHERE c.doc_id = d.id
RETURN DISTINCT doc, d.title AS title, e.key AS key, e.name AS name, e.type AS type
ORDER BY doc, key`, map[string]any{"docs": docIDs[start:min(start+rejudgeBatchSize, len(docIDs))]})
		if err != nil {
			return nil, fmt.Errorf("graph: read validity title matches: %w", err)
		}
		for _, row := range rows {
			title, name := getString(row, "title"), getString(row, "name")
			if strings.TrimSpace(title) != "" && strings.EqualFold(title, name) {
				doc := getString(row, "doc")
				matches[doc] = append(matches[doc], CatalogTitleMatch{getString(row, "key"), name, getString(row, "type")})
			}
		}
	}
	return matches, nil
}
