package graph

import (
	"context"
	"fmt"
)

// PathEdge is one traversed fact edge with provenance and evidence; From
// and To carry entity keys.
type PathEdge struct {
	From          string
	To            string
	Rel           string
	Confidence    float64
	Judge         string
	LowConfidence bool
	DocID         string
	ChunkID       string
	Excerpt       string
}

// Subgraph is the nodes and edges reachable from a traversal start.
type Subgraph struct {
	Nodes []EntityRef
	Edges []PathEdge
}

// traverseDepthCap is the agent-tool ceiling for traversal depth.
const traverseDepthCap = 2

// Traverse walks fact edges from the start entity (id or key) up to depth
// hops, optionally filtered by relation and direction (out|in|both).
// Low-confidence facts are excluded unless opted in. depth above the tool
// cap is an error; limit caps the total edges returned.
func (s *Store) Traverse(ctx context.Context, startID, rel, direction string, depth, limit int, includeLowConfidence bool) (Subgraph, error) {
	if depth < 1 || depth > traverseDepthCap {
		return Subgraph{}, fmt.Errorf("graph: traverse: depth %d outside 1..%d", depth, traverseDepthCap)
	}
	switch direction {
	case "":
		direction = "out"
	case "out", "in", "both":
	default:
		return Subgraph{}, fmt.Errorf("graph: traverse: direction %q not in out|in|both", direction)
	}
	if limit <= 0 {
		limit = 50
	}

	start, err := s.resolveEntityRef(ctx, startID)
	if err != nil {
		return Subgraph{}, err
	}
	sub := Subgraph{Nodes: []EntityRef{start}}
	visited := map[string]bool{start.ID: true}
	frontier := []EntityRef{start}

	for hop := 0; hop < depth && len(frontier) > 0; hop++ {
		var next []EntityRef
		seenEdges := map[string]bool{}
		for _, existing := range sub.Edges {
			seenEdges[existing.From+"|"+existing.Rel+"|"+existing.To+"|"+existing.ChunkID] = true
		}
		for _, node := range frontier {
			if len(sub.Edges) >= limit {
				break
			}
			edges, nodes, err := s.traverseHop(ctx, node, rel, direction, limit-len(sub.Edges), includeLowConfidence)
			if err != nil {
				return Subgraph{}, err
			}
			for _, e := range edges {
				key := e.From + "|" + e.Rel + "|" + e.To + "|" + e.ChunkID
				if seenEdges[key] {
					continue // the same edge reached from both endpoints
				}
				seenEdges[key] = true
				sub.Edges = append(sub.Edges, e)
			}
			for _, n := range nodes {
				if !visited[n.ID] {
					visited[n.ID] = true
					sub.Nodes = append(sub.Nodes, n)
					next = append(next, n)
				}
			}
		}
		frontier = next
	}
	return sub, nil
}

func (s *Store) resolveEntityRef(ctx context.Context, idOrKey string) (EntityRef, error) {
	recs, err := s.runRead(ctx, `
MATCH (e:Entity) WHERE e.id = $q OR e.key = $q
RETURN e.id AS id, e.key AS key, e.name AS name, e.type AS type`,
		map[string]any{"q": idOrKey})
	if err != nil {
		return EntityRef{}, fmt.Errorf("graph: traverse: %w", err)
	}
	if len(recs) == 0 {
		return EntityRef{}, fmt.Errorf("graph: entity %s: %w", idOrKey, ErrNotFound)
	}
	return EntityRef{
		ID:   getString(recs[0], "id"),
		Key:  getString(recs[0], "key"),
		Name: getString(recs[0], "name"),
		Type: getString(recs[0], "type"),
	}, nil
}

// traverseHop expands one node in one direction.
func (s *Store) traverseHop(ctx context.Context, from EntityRef, rel, direction string, remaining int, includeLowConfidence bool) ([]PathEdge, []EntityRef, error) {
	if remaining <= 0 {
		return nil, nil, nil
	}
	if direction == "out" || direction == "both" {
		edges, nodes, err := s.hopQuery(ctx, from, rel, false, remaining, includeLowConfidence)
		if err != nil {
			return nil, nil, err
		}
		if direction == "out" {
			return edges, nodes, nil
		}
		in, inNodes, err := s.hopQuery(ctx, from, rel, true, remaining-len(edges), includeLowConfidence)
		if err != nil {
			return nil, nil, err
		}
		return append(edges, in...), append(nodes, inNodes...), nil
	}
	return s.hopQuery(ctx, from, rel, true, remaining, includeLowConfidence)
}

func (s *Store) hopQuery(ctx context.Context, from EntityRef, rel string, incoming bool, limit int, includeLowConfidence bool) ([]PathEdge, []EntityRef, error) {
	if limit <= 0 {
		return nil, nil, nil
	}
	pattern := "MATCH (a:Entity {id: $id})-[r]->(b:Entity)"
	relFilter := ""
	if rel != "" {
		relFilter = " AND type(r) = $rel"
	}
	if incoming {
		pattern = "MATCH (a:Entity)-[r]->(b:Entity {id: $id})"
	}
	query := pattern + `
WHERE ($include OR coalesce(r.low_confidence, false) = false)` + relFilter + `
OPTIONAL MATCH (c:Chunk {id: r.chunk_id})
RETURN type(r) AS rel, a.id AS from_id, a.key AS from_key, a.name AS from_name, a.type AS from_type,
       b.id AS to_id, b.key AS to_key, b.name AS to_name, b.type AS to_type,
       coalesce(r.confidence, 0) AS confidence, coalesce(r.judge, '') AS judge,
       coalesce(r.low_confidence, false) AS low, coalesce(r.doc_id, '') AS doc_id,
       coalesce(r.chunk_id, '') AS chunk_id, coalesce(r.head_key, '') AS head_key,
       coalesce(r.head_start, -1) AS head_start, coalesce(r.tail_start, -1) AS tail_start,
       coalesce(c.text, '') AS text
ORDER BY rel, from_key, to_key, chunk_id
LIMIT $limit`
	params := map[string]any{"id": from.ID, "include": includeLowConfidence, "limit": limit}
	if rel != "" {
		params["rel"] = sanitizeRel(rel)
	}
	recs, err := s.runRead(ctx, query, params)
	if err != nil {
		return nil, nil, fmt.Errorf("graph: traverse hop: %w", err)
	}
	edges := make([]PathEdge, 0, len(recs))
	nodes := make([]EntityRef, 0, len(recs))
	for _, r := range recs {
		// The excerpt anchors on the node being expanded: the head for a
		// forward hop, the tail for a backward hop.
		center := getInt(r, "head_start")
		if incoming {
			center = getInt(r, "tail_start")
		}
		edges = append(edges, PathEdge{
			From:          getString(r, "from_key"),
			To:            getString(r, "to_key"),
			Rel:           getString(r, "rel"),
			Confidence:    getFloat(r, "confidence"),
			Judge:         getString(r, "judge"),
			LowConfidence: getBool(r, "low"),
			DocID:         getString(r, "doc_id"),
			ChunkID:       getString(r, "chunk_id"),
			Excerpt:       ExcerptWindow(getString(r, "text"), center, 120),
		})
		// The neighbor to enqueue is the endpoint that is not the anchor.
		ref := EntityRef{
			ID:   getString(r, "to_id"),
			Key:  getString(r, "to_key"),
			Name: getString(r, "to_name"),
			Type: getString(r, "to_type"),
		}
		if incoming {
			ref = EntityRef{
				ID:   getString(r, "from_id"),
				Key:  getString(r, "from_key"),
				Name: getString(r, "from_name"),
				Type: getString(r, "from_type"),
			}
		}
		nodes = append(nodes, ref)
	}
	return edges, nodes, nil
}

// EntitySearchRow is one entity text-search hit.
type EntitySearchRow struct {
	ID   string
	Name string
	Type string
}

// EntityTextSearch runs the Tantivy entity index (name + aliases).
func (s *Store) EntityTextSearch(ctx context.Context, q string, limit int) ([]EntitySearchRow, error) {
	recs, err := s.runRead(ctx, `
CALL text_search.search_all('entity_text', $q) YIELD node, score
RETURN node.id AS id, node.name AS name, node.type AS type
ORDER BY score DESC
LIMIT $limit`,
		map[string]any{"q": q, "limit": limit})
	if err != nil {
		return nil, fmt.Errorf("graph: entity text search: %w", err)
	}
	out := make([]EntitySearchRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, EntitySearchRow{
			ID:   getString(r, "id"),
			Name: getString(r, "name"),
			Type: getString(r, "type"),
		})
	}
	return out, nil
}

// ChunkSearchRow is one chunk text-search hit with its document title.
type ChunkSearchRow struct {
	ID    string
	DocID string
	Title string
	Text  string
}

// ChunkTextSearch runs the Tantivy chunk index.
func (s *Store) ChunkTextSearch(ctx context.Context, q string, limit int) ([]ChunkSearchRow, error) {
	recs, err := s.runRead(ctx, `
CALL text_search.search_all('chunk_text', $q) YIELD node, score
OPTIONAL MATCH (d:Document {id: node.doc_id})
RETURN node.id AS id, node.doc_id AS doc_id, node.text AS text, coalesce(d.title, '') AS title
ORDER BY score DESC
LIMIT $limit`,
		map[string]any{"q": q, "limit": limit})
	if err != nil {
		return nil, fmt.Errorf("graph: chunk text search: %w", err)
	}
	out := make([]ChunkSearchRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, ChunkSearchRow{
			ID:    getString(r, "id"),
			DocID: getString(r, "doc_id"),
			Title: getString(r, "title"),
			Text:  getString(r, "text"),
		})
	}
	return out, nil
}

// EntityVectorSearch returns entity ids ordered by vector distance; it
// errors when the vector index does not exist (callers degrade to text).
func (s *Store) EntityVectorSearch(ctx context.Context, vec []float64, limit int) ([]string, error) {
	recs, err := s.runRead(ctx, `
CALL vector_search.search('entity_vec', $limit, $vec) YIELD node, distance
RETURN node.id AS id
ORDER BY distance ASC`,
		map[string]any{"vec": vec, "limit": limit})
	if err != nil {
		return nil, fmt.Errorf("graph: entity vector search: %w", err)
	}
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, getString(r, "id"))
	}
	return out, nil
}

// EntityRef returns one entity by id, ErrNotFound when missing.
func (s *Store) EntityRef(ctx context.Context, id string) (EntityRef, error) {
	recs, err := s.runRead(ctx, `
MATCH (e:Entity {id: $id})
RETURN e.id AS id, e.key AS key, e.name AS name, e.type AS type`,
		map[string]any{"id": id})
	if err != nil {
		return EntityRef{}, fmt.Errorf("graph: entity ref: %w", err)
	}
	if len(recs) == 0 {
		return EntityRef{}, fmt.Errorf("graph: entity %s: %w", id, ErrNotFound)
	}
	return EntityRef{
		ID:   getString(recs[0], "id"),
		Key:  getString(recs[0], "key"),
		Name: getString(recs[0], "name"),
		Type: getString(recs[0], "type"),
	}, nil
}
