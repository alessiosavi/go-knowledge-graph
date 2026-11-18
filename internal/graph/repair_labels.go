package graph

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// ChunkMentionSpan identifies an occurrence by entity key and byte offsets.
type ChunkMentionSpan struct {
	Key        string
	Start, End int
}

type StoredChunkMention struct {
	ChunkMentionSpan
	Name, Type string
}

type ChunkMentions struct {
	ID, DocID, Text, Labels, Domain string
	Status                          Status
	Mentions                        []StoredChunkMention
}

// PageChunkMentions reads up to limit chunks in id order after the given id.
// The optional domain matches the owning document's domain.
func (s *Store) PageChunkMentions(ctx context.Context, after, domain string, limit int) ([]ChunkMentions, error) {
	filter := ""
	if domain != "" {
		filter = " AND d.domain = $domain"
	}
	rows, err := s.runRead(ctx, `
MATCH (c:Chunk)<-[:HAS_CHUNK]-(d:Document)
WHERE c.id > $after`+filter+`
WITH c, d ORDER BY c.id LIMIT $limit
OPTIONAL MATCH (c)-[m:MENTIONS]->(e:Entity)
WHERE d.status = $done AND m.start IS NOT NULL AND m.end IS NOT NULL
WITH c, d, m, e ORDER BY m.start, m.end, e.key, m.type
RETURN c.id AS id, d.id AS doc_id, c.text AS text, d.labels AS labels, d.domain AS domain,
       d.status AS status,
       collect({key: e.key, name: e.name, type: m.type, start: m.start, end: m.end}) AS mentions
ORDER BY id`, map[string]any{"after": after, "domain": domain, "limit": limit, "done": string(StatusDone)})
	if err != nil {
		return nil, fmt.Errorf("graph: page chunk mentions: %w", err)
	}
	chunks := make([]ChunkMentions, 0, len(rows))
	for _, row := range rows {
		chunk := ChunkMentions{
			ID: getString(row, "id"), DocID: getString(row, "doc_id"), Text: getString(row, "text"),
			Labels: getString(row, "labels"), Domain: getString(row, "domain"),
			Status: Status(getString(row, "status")),
		}
		mentions, _ := row.Get("mentions")
		for _, v := range mentions.([]any) {
			m := v.(map[string]any)
			key, _ := m["key"].(string)
			start, hasStart := m["start"].(int64)
			end, hasEnd := m["end"].(int64)
			if key == "" || !hasStart || !hasEnd {
				continue
			}
			name, _ := m["name"].(string)
			kind, _ := m["type"].(string)
			chunk.Mentions = append(chunk.Mentions, StoredChunkMention{
				ChunkMentionSpan: ChunkMentionSpan{Key: key, Start: int(start), End: int(end)}, Name: name, Type: kind,
			})
		}
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}

// CountMentionEdges counts actual mentions of each key across all chunks.
func (s *Store) CountMentionEdges(ctx context.Context, keys []string) (map[string]int, error) {
	rows, err := s.runRead(ctx, `
UNWIND $keys AS key
WITH DISTINCT key
MATCH (e:Entity {key: key})<-[m:MENTIONS]-(:Chunk)
RETURN key, count(m) AS n`, map[string]any{"keys": keys})
	if err != nil {
		return nil, fmt.Errorf("graph: count mention edges: %w", err)
	}
	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[getString(row, "key")] = getInt(row, "n")
	}
	return counts, nil
}

// ChunkSpanChanges describes matched edges and dependent facts. Mentions counts
// physical edges per span. EntitiesRemoved counts entities collected on apply.
type ChunkSpanChanges struct {
	Mentions                   map[ChunkMentionSpan]int
	FactsConfirmed, FactsOther int
	Candidates, Rejected       int
	EntitiesRemoved            int
}

// PreviewChunkMentionSpans counts deletions for a completed document in a read transaction.
func (s *Store) PreviewChunkMentionSpans(ctx context.Context, id, text string, spans []ChunkMentionSpan) (ChunkSpanChanges, error) {
	return s.repairChunkMentionSpans(ctx, id, text, spans, false)
}

// DeleteChunkMentionSpans removes matching mentions and their dependent facts
// atomically. The chunk must still have the supplied text and belong to a
// completed document. Missing spans are skipped.
func (s *Store) DeleteChunkMentionSpans(ctx context.Context, id, text string, spans []ChunkMentionSpan) (ChunkSpanChanges, error) {
	return s.repairChunkMentionSpans(ctx, id, text, spans, true)
}

func (s *Store) repairChunkMentionSpans(ctx context.Context, id, text string, spans []ChunkMentionSpan, apply bool) (ChunkSpanChanges, error) {
	if len(spans) == 0 {
		return ChunkSpanChanges{}, nil
	}
	var changes ChunkSpanChanges
	run := func(tx neo4j.ManagedTransaction) error {
		var err error
		changes, err = txRepairChunkMentionSpans(ctx, tx, id, text, spans, apply)
		return err
	}
	var err error
	if apply {
		err = s.runWriteTxLocked(ctx, run)
	} else {
		session := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
		defer session.Close(ctx)
		_, err = session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
			return nil, run(tx)
		})
	}
	if err != nil {
		return ChunkSpanChanges{}, fmt.Errorf("graph: repair mentions in chunk %s: %w", id, err)
	}
	return changes, nil
}

func txRepairChunkMentionSpans(ctx context.Context, tx neo4j.ManagedTransaction, id, text string, spans []ChunkMentionSpan, apply bool) (ChunkSpanChanges, error) {
	params := map[string]any{"cid": id, "text": text, "spans": chunkSpanParams(spans), "done": string(StatusDone)}
	run := func(match, deletion, result string) ([]*neo4j.Record, error) {
		if apply {
			match += "\n" + deletion
		}
		res, err := tx.Run(ctx, match+"\n"+result, params)
		if err != nil {
			return nil, err
		}
		return res.Collect(ctx)
	}
	mentions, err := run(`
MATCH (:Document {status: $done})-[:HAS_CHUNK]->(c:Chunk {id: $cid, text: $text})-[m:MENTIONS]->(e:Entity)
WHERE any(s IN $spans WHERE s.key = e.key AND s.start = m.start AND s.end = m.end)
WITH m, e.key AS key, m.start AS start, m.end AS end`,
		"DELETE m", "RETURN key, start, end, count(*) AS n")
	if err != nil || len(mentions) == 0 {
		return ChunkSpanChanges{}, err
	}
	changes := ChunkSpanChanges{Mentions: make(map[ChunkMentionSpan]int, len(mentions))}
	keys := make(map[string]struct{})
	for _, row := range mentions {
		key := getString(row, "key")
		keys[key] = struct{}{}
		span := ChunkMentionSpan{Key: key, Start: getInt(row, "start"), End: getInt(row, "end")}
		changes.Mentions[span] = getInt(row, "n")
	}
	params["keys"] = slices.Collect(maps.Keys(keys))
	params["spans"] = chunkSpanParams(slices.Collect(maps.Keys(changes.Mentions)))

	facts, err := run(`
UNWIND $keys AS key
MATCH (:Entity {key: key})-[r]-(:Entity)
WHERE r.chunk_id = $cid AND any(s IN $spans WHERE
      (r.head_key = s.key AND r.head_start = s.start AND r.head_end = s.end) OR
      (r.tail_key = s.key AND r.tail_start = s.start AND r.tail_end = s.end))
WITH DISTINCT r, startNode(r).key AS head, endNode(r).key AS tail,
     CASE WHEN r.judge = 'confirmed' THEN 1 ELSE 0 END AS confirmed`,
		"DELETE r", "RETURN head, tail, sum(confirmed) AS confirmed, count(*) AS total")
	if err != nil {
		return ChunkSpanChanges{}, err
	}
	for _, row := range facts {
		confirmed := getInt(row, "confirmed")
		changes.FactsConfirmed += confirmed
		changes.FactsOther += getInt(row, "total") - confirmed
		for _, endpoint := range []string{"head", "tail"} {
			if key := getString(row, endpoint); key != "" {
				keys[key] = struct{}{}
			}
		}
	}

	candidates, err := run(`
MATCH (f:FactCandidate {chunk_id: $cid})
WHERE any(s IN $spans WHERE
      (f.head_key = s.key AND f.head_start = s.start AND f.head_end = s.end) OR
      (f.tail_key = s.key AND f.tail_start = s.start AND f.tail_end = s.end))`,
		"DELETE f", "RETURN count(*) AS n")
	if err != nil {
		return ChunkSpanChanges{}, err
	}
	changes.Candidates = getInt(candidates[0], "n")

	// Audit nodes lack offsets. Keep them as long as both affected endpoints
	// still have a mention anywhere in this chunk.
	rejected, err := run(`
MATCH (c:Chunk {id: $cid})
UNWIND $keys AS key
WITH c, key
WHERE NOT EXISTS {
    MATCH (c)-[m:MENTIONS]->(:Entity {key: key})
    WHERE NOT coalesce(any(s IN $spans WHERE s.key = key AND s.start = m.start AND s.end = m.end), false)
}
OPTIONAL MATCH (h:RejectedFact {head_key: key, chunk_id: $cid})
WITH key, collect(h) AS heads
OPTIONAL MATCH (t:RejectedFact {tail_key: key, chunk_id: $cid})
WITH heads + collect(t) AS audits
UNWIND audits AS f
WITH DISTINCT f`,
		"DELETE f", "RETURN count(*) AS n")
	if err != nil {
		return ChunkSpanChanges{}, err
	}
	changes.Rejected = getInt(rejected[0], "n")
	if !apply {
		return changes, nil
	}

	touched := slices.Collect(maps.Keys(keys))
	if err := txRefreshMentionCounts(ctx, tx, touched); err != nil {
		return ChunkSpanChanges{}, err
	}
	changes.EntitiesRemoved, err = txGCCount(ctx, tx, touched)
	return changes, err
}

func chunkSpanParams(spans []ChunkMentionSpan) []map[string]any {
	params := make([]map[string]any, 0, len(spans))
	for _, span := range spans {
		params = append(params, map[string]any{"key": span.Key, "start": span.Start, "end": span.End})
	}
	return params
}
