package quality

import (
	"context"
	"fmt"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const readBatch = 5000

// readPages bounds Bolt result sets to 5,000 rows. Queries filter id(node) or
// id(relationship) > $last and return that stable id AS cursor, ordered by cursor.
// These are read queries only; no bootstrap, migration or DDL is needed.
func readPages(ctx context.Context, st *graph.Store, query string, params map[string]any, visit func(map[string]any)) error {
	args := map[string]any{"last": int64(-1), "batch": readBatch}
	for key, value := range params {
		args[key] = value
	}
	for {
		result, err := st.RunReadQuery(ctx, query+" LIMIT $batch", args, 0)
		if err != nil {
			return fmt.Errorf("quality: read batch after %v: %w", args["last"], err)
		}
		for _, row := range result.Rows {
			visit(row)
		}
		if len(result.Rows) < readBatch {
			return nil
		}
		args["last"] = result.Rows[len(result.Rows)-1]["cursor"]
	}
}

func textValue(row map[string]any, key string) string { s, _ := row[key].(string); return s }
func floatValue(row map[string]any, key string) float64 {
	switch v := row[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	}
	return 0
}

func load(ctx context.Context, st *graph.Store, opt Options) (dataset, error) {
	var d dataset
	version, err := st.SchemaVersion(ctx)
	if err != nil {
		return d, err
	}
	// The metrics know schemas up to v4; a newer graph (written by a newer
	// binary) is refused rather than read under v4 assumptions.
	if version > graph.SchemaVersionV4 {
		return d, fmt.Errorf("quality: graph extraction schema v%d is newer than this report understands (v%d): %w", version, graph.SchemaVersionV4, graph.ErrSchemaNewer)
	}
	d.version = version
	params := map[string]any{}
	docFilter := ""
	// A seed can override stored domains, so resolve it before pushing the
	// selected document ids into subsequent scoped queries.
	if opt.Domain != "" && opt.Seed == nil {
		docFilter = " AND d.domain = $domain"
		params["domain"] = opt.Domain
	}
	if err := readPages(ctx, st, `MATCH (d:Document {status: 'done'}) WHERE id(d) > $last`+docFilter+` RETURN id(d) AS cursor, d.id AS id, coalesce(d.source, '') AS source, coalesce(d.domain, '') AS domain, coalesce(d.labels, '') AS preset ORDER BY cursor`, params, func(row map[string]any) {
		d.documents = append(d.documents, document{
			id: textValue(row, "id"), source: textValue(row, "source"), domain: textValue(row, "domain"), preset: textValue(row, "preset"),
		})
	}); err != nil {
		return d, err
	}
	docs := selectedDocuments(d.documents, opt)
	// Retain the selected documents' resolved domains for the pure computation.
	selected := d.documents[:0]
	for _, doc := range d.documents {
		if resolved, ok := docs[doc.id]; ok {
			selected = append(selected, resolved)
		}
	}
	d.documents = selected
	factFilter, candidateFilter, rejectedFilter := "", "", ""
	mentionMatch := `MATCH (c:Chunk)-[m:MENTIONS]->(e:Entity)`
	mentionFilter := ""
	if opt.Domain != "" {
		ids := make([]string, 0, len(docs))
		for _, doc := range selected {
			ids = append(ids, doc.id)
		}
		params["doc_ids"] = ids
		if opt.Seed != nil {
			docFilter = " AND d.id IN $doc_ids"
		}
		mentionMatch = `MATCH (d:Document {status: 'done'})-[:HAS_CHUNK]->(c:Chunk)-[m:MENTIONS]->(e:Entity)`
		mentionFilter = docFilter
		factFilter = " AND r.doc_id IN $doc_ids"
		candidateFilter = " AND c.doc_id IN $doc_ids"
		rejectedFilter = " AND r.doc_id IN $doc_ids"
	}
	chunks := make(map[string]string)
	if err := readPages(ctx, st, `MATCH (d:Document {status: 'done'})-[:HAS_CHUNK]->(c:Chunk) WHERE id(c) > $last`+docFilter+` RETURN id(c) AS cursor, c.id AS id, d.id AS doc ORDER BY cursor`, params, func(row map[string]any) {
		doc := textValue(row, "doc")
		if _, ok := docs[doc]; !ok {
			return
		}
		id := textValue(row, "id")
		chunks[id] = doc
		d.chunks = append(d.chunks, chunk{id, doc})
	}); err != nil {
		return d, err
	}
	active := make(stringSet)
	if err := readPages(ctx, st, mentionMatch+` WHERE id(m) > $last`+mentionFilter+` RETURN id(m) AS cursor, c.id AS chunk, e.key AS entity ORDER BY cursor`, params, func(row map[string]any) {
		cid := textValue(row, "chunk")
		if _, ok := chunks[cid]; !ok {
			return
		}
		key := textValue(row, "entity")
		active[key] = true
		d.mentions = append(d.mentions, mention{key, cid})
	}); err != nil {
		return d, err
	}
	if err := readPages(ctx, st, `MATCH (a:Entity)-[r]->(b:Entity) WHERE id(r) > $last`+factFilter+` RETURN id(r) AS cursor, a.key AS head, b.key AS tail, type(r) AS rel, coalesce(r.doc_id, '') AS doc, coalesce(r.chunk_id, '') AS chunk, coalesce(r.confidence, 0.0) AS confidence, coalesce(r.low_confidence, false) AS low ORDER BY cursor`, params, func(row map[string]any) {
		doc, cid := textValue(row, "doc"), textValue(row, "chunk")
		if _, ok := docs[doc]; !ok {
			return
		}
		if chunkDoc, ok := chunks[cid]; !ok || chunkDoc != doc {
			return
		}
		head, tail := textValue(row, "head"), textValue(row, "tail")
		active[head] = true
		active[tail] = true
		low, _ := row["low"].(bool)
		d.facts = append(d.facts, fact{head, tail, textValue(row, "rel"), cid, doc, floatValue(row, "confidence"), low})
	}); err != nil {
		return d, err
	}
	if err := readPages(ctx, st, `MATCH (e:Entity) WHERE id(e) > $last RETURN id(e) AS cursor, e.key AS key, coalesce(e.name, '') AS name, coalesce(e.type, '') AS type ORDER BY cursor`, nil, func(row map[string]any) {
		key := textValue(row, "key")
		if active[key] {
			d.entities = append(d.entities, entity{key, textValue(row, "name"), textValue(row, "type")})
		}
	}); err != nil {
		return d, err
	}
	if version >= graph.SchemaVersionV4 {
		if err := readPages(ctx, st, `MATCH (c:FactCandidate) WHERE id(c) > $last`+candidateFilter+` RETURN id(c) AS cursor, c.doc_id AS doc, coalesce(c.judge, '') AS judge ORDER BY cursor`, params, func(row map[string]any) {
			if _, ok := docs[textValue(row, "doc")]; !ok {
				return
			}
			switch textValue(row, "judge") {
			case "":
				d.unjudged++
			case "unclear":
				d.unclear++
			}
		}); err != nil {
			return d, err
		}
	}
	if err := readPages(ctx, st, `MATCH (r:RejectedFact) WHERE id(r) > $last`+rejectedFilter+` RETURN id(r) AS cursor, r.doc_id AS doc ORDER BY cursor`, params, func(row map[string]any) {
		if _, ok := docs[textValue(row, "doc")]; ok {
			d.rejected++
		}
	}); err != nil {
		return d, err
	}
	return d, nil
}
