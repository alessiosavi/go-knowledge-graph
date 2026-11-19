package graph

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// RejudgeIdentity identifies one typed edge in the evaluation population.
type RejudgeIdentity struct {
	ID               string  `json:"id"`
	Domain           string  `json:"domain"`
	HeadKey          string  `json:"head_key"`
	Relation         string  `json:"rel"`
	TailKey          string  `json:"tail_key"`
	ChunkID          string  `json:"chunk_id"`
	HeadStart        int     `json:"head_start"`
	HeadEnd          int     `json:"head_end"`
	TailStart        int     `json:"tail_start"`
	TailEnd          int     `json:"tail_end"`
	Confidence       float64 `json:"confidence"`
	LowConfidence    bool    `json:"low_confidence"`
	ExtractorVersion string  `json:"extractor_version"`
	Judge            string  `json:"judge"`
}

// RejudgeFact is an edge before-image and the source data needed to rebuild a
// request. ChunkOwned is false when the edge's document does not own its chunk.
type RejudgeFact struct {
	RejudgeIdentity
	Before                             map[string]any
	DocID, Title, Preset, Text         string
	ChunkOwned                         bool
	HeadName, TailName                 string
	HeadSurface                        *string // nil when the edge has no head_surface property
	HeadType, TailType                 string
	HeadTypeFallback, TailTypeFallback bool
}

const rejudgeBatchSize = 256

const rejudgeScan = `MATCH (h:Entity)
WHERE h.key > $after AND ($domain = '' OR coalesce(h.domain, '') = $domain)
WITH h ORDER BY h.key LIMIT $size
OPTIONAL MATCH (h)-[r]->(t:Entity) WHERE r.judge IN ['confirmed', '']
RETURN h.key AS head_key, type(r) AS rel, t.key AS tail_key, coalesce(r.chunk_id, '') AS chunk_id,
       id(r) AS edge, coalesce(h.domain, '') AS domain
ORDER BY head_key, rel, tail_key, chunk_id, edge`

// The V2b/V3 rewrite writer must set head_surface to the trimmed target entity
// name. A re-attached edge keeps the original head span as an evidence anchor.
// Skip its head mention lookup so the type and fallback come from the head entity.
const rejudgeHydrate = `UNWIND $refs AS f
MATCH (h:Entity {key:f.head_key})-[r]->(t:Entity)
WHERE id(r) = f.edge AND type(r) = f.rel AND t.key = f.tail_key
  AND coalesce(r.chunk_id, '') = f.chunk_id AND r.judge IN ['confirmed', '']
OPTIONAL MATCH (d:Document {id:r.doc_id})
OPTIONAL MATCH (c:Chunk {id:r.chunk_id})
WHERE c.doc_id = d.id AND (d)-[:HAS_CHUNK]->(c)
OPTIONAL MATCH (c)-[hm:MENTIONS]->(h)
WHERE r.head_surface IS NULL AND hm.start = r.head_start AND hm.end = r.head_end AND hm.type <> ''
WITH f, r, d, c, t, h, min(hm.type) AS ht
OPTIONAL MATCH (c)-[tm:MENTIONS]->(t)
WHERE tm.start = r.tail_start AND tm.end = r.tail_end AND tm.type <> ''
WITH f, r, d, c, t, h, ht, min(tm.type) AS tt
RETURN f AS ref, properties(r) AS before, r.doc_id AS doc_id, d.title AS title, d.labels AS preset,
       c.id IS NOT NULL AS owned, c.text AS text, r.judge AS judge, r.extractor_version AS extractor_version,
       r.head_start AS head_start, r.head_end AS head_end, r.tail_start AS tail_start, r.tail_end AS tail_end,
       r.confidence AS confidence, r.low_confidence AS low_confidence,
       coalesce(h.name, f.head_key) AS head_name, coalesce(t.name, f.tail_key) AS tail_name,
       r.head_surface AS head_surface,
       coalesce(ht, h.type, '') AS head_type, coalesce(tt, t.type, '') AS tail_type,
       ht IS NULL AS head_fallback, tt IS NULL AS tail_fallback
ORDER BY f.idx`

// WalkRejudgeFacts pages through indexed head keys and hydrates only matching
// edges. Audit ids are derived hashes, so an id selection scans identities once
// without reading the unselected edges' properties or evidence. A nil ids
// slice selects the population, and a zero limit leaves it unrestricted.
func (s *Store) WalkRejudgeFacts(ctx context.Context, domain string, ids []string, limit int, visit func([]RejudgeFact) error) error {
	if limit < 0 || (ids != nil && limit != 0) {
		return fmt.Errorf("graph: invalid rejudge selection")
	}
	if ids != nil && len(ids) == 0 {
		return nil
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	var after string
	selected := 0
	for {
		rows, err := s.runRead(ctx, rejudgeScan, map[string]any{"after": after, "domain": domain, "size": rejudgeBatchSize})
		if err != nil {
			return fmt.Errorf("graph: scan rejudge edges: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		for start := 0; start < len(rows); start += rejudgeBatchSize {
			var refs []map[string]any
			for _, row := range rows[start:min(start+rejudgeBatchSize, len(rows))] {
				if getString(row, "rel") == "" {
					continue
				}
				head, rel := getString(row, "head_key"), getString(row, "rel")
				tail, chunk := getString(row, "tail_key"), getString(row, "chunk_id")
				id := FactAuditID(head, rel, tail, chunk)
				if ids != nil && !wanted[id] {
					continue
				}
				refs = append(refs, map[string]any{
					"id": id, "head_key": head, "rel": rel, "tail_key": tail, "chunk_id": chunk,
					"edge": getInt(row, "edge"), "domain": getString(row, "domain"), "idx": len(refs),
				})
				selected++
				if selected == limit {
					break
				}
			}
			if len(refs) != 0 {
				facts, err := s.rejudgeFacts(ctx, refs)
				if err != nil {
					return err
				}
				if err := visit(facts); err != nil {
					return err
				}
			}
			if selected == limit && limit != 0 {
				return nil
			}
		}
		after = getString(rows[len(rows)-1], "head_key")
	}
}

func (s *Store) rejudgeFacts(ctx context.Context, refs []map[string]any) ([]RejudgeFact, error) {
	return readRejudgeFacts(func(query string, params map[string]any) ([]*neo4j.Record, error) {
		return s.runRead(ctx, query, params)
	}, refs)
}

type rejudgeRead func(string, map[string]any) ([]*neo4j.Record, error)

func readRejudgeFacts(read rejudgeRead, refs []map[string]any) ([]RejudgeFact, error) {
	rows, err := read(rejudgeHydrate, map[string]any{"refs": refs})
	if err != nil {
		return nil, fmt.Errorf("graph: read rejudge evidence: %w", err)
	}
	facts := make([]RejudgeFact, 0, len(rows))
	for _, row := range rows {
		rawBefore, _ := row.Get("before")
		before, _ := rawBefore.(map[string]any)
		rawRef, _ := row.Get("ref")
		ref := rawRef.(map[string]any)
		rawHeadSurface, _ := row.Get("head_surface")
		var headSurface *string
		if rawHeadSurface != nil {
			// A non-string property is present but invalid, just like an empty string.
			surface, _ := rawHeadSurface.(string)
			headSurface = &surface
		}
		facts = append(facts, RejudgeFact{
			RejudgeIdentity: RejudgeIdentity{
				ID: ref["id"].(string), Domain: ref["domain"].(string), HeadKey: ref["head_key"].(string),
				Relation: ref["rel"].(string), TailKey: ref["tail_key"].(string), ChunkID: ref["chunk_id"].(string),
				HeadStart: getInt(row, "head_start"), HeadEnd: getInt(row, "head_end"),
				TailStart: getInt(row, "tail_start"), TailEnd: getInt(row, "tail_end"),
				Confidence: getFloat(row, "confidence"), LowConfidence: getBool(row, "low_confidence"),
				ExtractorVersion: getString(row, "extractor_version"), Judge: getString(row, "judge"),
			},
			Before: before, DocID: getString(row, "doc_id"), Title: getString(row, "title"),
			Preset: getString(row, "preset"), Text: getString(row, "text"), ChunkOwned: getBool(row, "owned"),
			HeadName: getString(row, "head_name"), TailName: getString(row, "tail_name"),
			HeadSurface: headSurface,
			HeadType:    getString(row, "head_type"), TailType: getString(row, "tail_type"),
			HeadTypeFallback: getBool(row, "head_fallback"), TailTypeFallback: getBool(row, "tail_fallback"),
		})
	}
	return facts, nil
}

// RejudgePreviousChunks retrieves the immediately preceding chunk by sequence
// for the selected chunks, verifying both chunks belong to the same document.
func (s *Store) RejudgePreviousChunks(ctx context.Context, chunks map[string]string) (map[string]string, error) {
	previous, duplicates, err := rejudgePreviousChunks(func(query string, params map[string]any) ([]*neo4j.Record, error) {
		return s.runRead(ctx, query, params)
	}, chunks)
	if err != nil {
		return nil, err
	}
	if len(duplicates) != 0 {
		return nil, fmt.Errorf("graph: multiple preceding chunks for %s", duplicates[0])
	}
	return previous, nil
}

func rejudgePreviousChunks(read rejudgeRead, chunks map[string]string) (map[string]string, []string, error) {
	refs := make([]map[string]any, 0, len(chunks))
	for chunk, doc := range chunks {
		refs = append(refs, map[string]any{"chunk": chunk, "doc": doc})
	}
	out := make(map[string]string, len(refs))
	var duplicates []string
	if len(refs) == 0 {
		return out, duplicates, nil
	}
	rows, err := read(`UNWIND $refs AS f
MATCH (d:Document {id:f.doc}), (c:Chunk {id:f.chunk})
WHERE c.doc_id = d.id AND (d)-[:HAS_CHUNK]->(c)
MATCH (d)-[:HAS_CHUNK]->(p:Chunk)
WHERE p.doc_id = d.id AND p.seq = c.seq - 1
RETURN c.id AS chunk, p.id AS previous, p.text AS text ORDER BY chunk, previous`, map[string]any{"refs": refs})
	if err != nil {
		return nil, nil, fmt.Errorf("graph: read previous rejudge chunks: %w", err)
	}
	for _, row := range rows {
		id := getString(row, "chunk")
		if _, ok := out[id]; ok {
			duplicates = append(duplicates, id)
			continue
		}
		out[id] = getString(row, "text")
	}
	return out, duplicates, nil
}

// CountCachedJudgeRequests counts present keys with bounded, indexed lookups.
// Callers provide distinct keys.
func (s *Store) CountCachedJudgeRequests(ctx context.Context, keys []string) (int, error) {
	count := 0
	for start := 0; start < len(keys); start += rejudgeBatchSize {
		rows, err := s.runRead(ctx, `UNWIND $keys AS key
MATCH (v:JudgeVerdict {key:key}) RETURN count(DISTINCT key) AS n`,
			map[string]any{"keys": keys[start:min(start+rejudgeBatchSize, len(keys))]})
		if err != nil {
			return 0, fmt.Errorf("graph: count cached judge requests: %w", err)
		}
		count += getInt(rows[0], "n")
	}
	return count, nil
}
