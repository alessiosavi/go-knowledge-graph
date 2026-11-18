package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
)

// PathEdge is one traversed fact edge with provenance and evidence; From
// and To carry entity keys.
type PathEdge struct {
	From          string
	To            string
	Rel           string
	Confidence    float64
	Judge         string
	Unverified    bool
	LowConfidence bool
	DocID         string
	ChunkID       string
	Excerpt       string
}

// Subgraph is the nodes and edges reachable from a traversal start.
type Subgraph struct {
	Nodes  []EntityRef
	Edges  []PathEdge
	Hidden HiddenCounts
}

// traverseDepthCap is the agent-tool ceiling for traversal depth.
const traverseDepthCap = 2

// Traverse walks fact edges from the start entity (id or key) up to depth
// hops, optionally filtered by relation and direction (out|in|both).
// Low-confidence facts are excluded unless opted in. depth above the tool
// cap is an error; limit caps the total edges returned.
func (s *Store) Traverse(ctx context.Context, startID, rel, direction string, depth, limit int, includeLowConfidence bool) (Subgraph, error) {
	return s.TraverseOpts(ctx, startID, rel, direction, depth, limit, ViewOptions{IncludeLowConfidence: includeLowConfidence})
}

// TraverseOpts can also include candidate facts, marked as unverified, without
// expanding their far endpoints. Hidden counts always describe the start entity,
// regardless of depth or filters.
func (s *Store) TraverseOpts(ctx context.Context, startID, rel, direction string, depth, limit int, opt ViewOptions) (Subgraph, error) {
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

	if opt.Domain != "" && !ValidDomain(opt.Domain) {
		return Subgraph{}, fmt.Errorf("graph: invalid domain slug %q", opt.Domain)
	}
	start, err := s.resolveEntityRef(ctx, startID)
	if err != nil {
		return Subgraph{}, err
	}
	if opt.Domain != "" && start.Domain != opt.Domain {
		return Subgraph{}, fmt.Errorf("graph: entity %s: %w", startID, ErrNotFound)
	}
	sub := Subgraph{Nodes: []EntityRef{start}}
	sub.Hidden, err = s.hiddenCounts(ctx, start.Key)
	if err != nil {
		return Subgraph{}, err
	}
	visited := map[string]bool{start.ID: true}
	enqueued := map[string]bool{start.ID: true}
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
			edges, nodes, err := s.traverseHop(ctx, node, rel, direction, limit-len(sub.Edges), opt)
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
			for i, n := range nodes {
				if !visited[n.ID] {
					visited[n.ID] = true
					sub.Nodes = append(sub.Nodes, n)
				}
				// Each returned node corresponds to the edge at the same index.
				// A candidate may reveal a node, but only a typed edge can enqueue it.
				if !edges[i].Unverified && !enqueued[n.ID] {
					enqueued[n.ID] = true
					next = append(next, n)
				}
			}
		}
		frontier = next
	}
	return sub, nil
}

func (s *Store) resolveEntityRef(ctx context.Context, idOrKey string) (EntityRef, error) {
	params := map[string]any{"q": idOrKey}
	recs, err := s.runRead(ctx, entityByIDQuery, params)
	if err == nil && len(recs) == 0 {
		recs, err = s.runRead(ctx, entityByKeyQuery, params)
	}
	if err != nil {
		return EntityRef{}, fmt.Errorf("graph: traverse: %w", err)
	}
	if len(recs) == 0 {
		return EntityRef{}, fmt.Errorf("graph: entity %s: %w", idOrKey, ErrNotFound)
	}
	return EntityRef{
		ID:     getString(recs[0], "id"),
		Key:    getString(recs[0], "key"),
		Name:   getString(recs[0], "name"),
		Type:   getString(recs[0], "type"),
		Domain: getString(recs[0], "domain"),
	}, nil
}

// traverseHop expands one node in one direction.
func (s *Store) traverseHop(ctx context.Context, from EntityRef, rel, direction string, remaining int, opt ViewOptions) ([]PathEdge, []EntityRef, error) {
	if remaining <= 0 {
		return nil, nil, nil
	}
	if direction == "out" || direction == "both" {
		edges, nodes, err := s.hopQuery(ctx, from, rel, false, remaining, opt)
		if err != nil {
			return nil, nil, err
		}
		if direction == "out" {
			return edges, nodes, nil
		}
		in, inNodes, err := s.hopQuery(ctx, from, rel, true, remaining-len(edges), opt)
		if err != nil {
			return nil, nil, err
		}
		return append(edges, in...), append(nodes, inNodes...), nil
	}
	return s.hopQuery(ctx, from, rel, true, remaining, opt)
}

func (s *Store) hopQuery(ctx context.Context, from EntityRef, rel string, incoming bool, limit int, opt ViewOptions) ([]PathEdge, []EntityRef, error) {
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
RETURN type(r) AS rel, a.id AS from_id, a.key AS from_key, a.name AS from_name, a.type AS from_type, a.domain AS from_domain,
       b.id AS to_id, b.key AS to_key, b.name AS to_name, b.type AS to_type, b.domain AS to_domain,
       coalesce(r.confidence, 0) AS confidence, coalesce(r.judge, '') AS judge,
       coalesce(r.low_confidence, false) AS low, coalesce(r.doc_id, '') AS doc_id,
       coalesce(r.chunk_id, '') AS chunk_id, coalesce(r.head_key, '') AS head_key,
       coalesce(r.head_start, -1) AS head_start, coalesce(r.tail_start, -1) AS tail_start,
       coalesce(c.text, '') AS text
ORDER BY rel, from_key, to_key, chunk_id
LIMIT $limit`
	params := map[string]any{"id": from.ID, "include": opt.IncludeLowConfidence, "limit": limit}
	if rel != "" {
		params["rel"] = sanitizeRel(rel)
	}
	recs, err := s.runRead(ctx, query, params)
	if err != nil {
		return nil, nil, fmt.Errorf("graph: traverse hop: %w", err)
	}
	if opt.IncludeUnverified && len(recs) < limit {
		anchor := "head_key"
		if incoming {
			anchor = "tail_key"
		}
		candidatePattern := "MATCH (r:FactCandidate {" + anchor + ": $key})\n" +
			"MATCH (a:Entity {key: r.head_key}), (b:Entity {key: r.tail_key})"
		candidateQuery := strings.Replace(query, pattern, candidatePattern, 1)
		candidateQuery = strings.ReplaceAll(candidateQuery, "type(r)", "r.rel")
		candidateQuery = strings.Replace(candidateQuery, "($include OR coalesce(r.low_confidence, false) = false)", "coalesce(r.judge, '') IN ['', 'unclear']", 1)
		candidateQuery = strings.Replace(candidateQuery, "RETURN r.rel AS rel,", "RETURN true AS unverified, r.rel AS rel,", 1)
		params["key"] = from.Key
		params["limit"] = limit - len(recs)
		candidates, err := s.runRead(ctx, candidateQuery, params)
		if err != nil {
			return nil, nil, fmt.Errorf("graph: traverse candidates: %w", err)
		}
		recs = append(recs, candidates...)
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
			Unverified:    getBool(r, "unverified"),
			LowConfidence: getBool(r, "low"),
			DocID:         getString(r, "doc_id"),
			ChunkID:       getString(r, "chunk_id"),
			Excerpt:       ExcerptWindow(getString(r, "text"), center, 120),
		})
		// Return the far endpoint alongside its edge; the caller decides whether
		// it can be enqueued based on the edge's verification state.
		ref := EntityRef{
			ID:     getString(r, "to_id"),
			Key:    getString(r, "to_key"),
			Name:   getString(r, "to_name"),
			Type:   getString(r, "to_type"),
			Domain: getString(r, "to_domain"),
		}
		if incoming {
			ref = EntityRef{
				ID:     getString(r, "from_id"),
				Key:    getString(r, "from_key"),
				Name:   getString(r, "from_name"),
				Type:   getString(r, "from_type"),
				Domain: getString(r, "from_domain"),
			}
		}
		nodes = append(nodes, ref)
	}
	return edges, nodes, nil
}

// EntitySearchRow is one entity text-search hit.
type EntitySearchRow struct {
	Domain string
	ID     string
	Name   string
	Type   string
}

// EntityTextSearch runs the Tantivy entity_text_v2 index. Entity search is
// name-only by design; aliases are excluded (docs/known-limitations.md).
func (s *Store) EntityTextSearch(ctx context.Context, q string, limit int) ([]EntitySearchRow, error) {
	return s.EntityTextSearchIn(ctx, q, "", limit)
}

func (s *Store) EntityTextSearchIn(ctx context.Context, q, slug string, limit int) ([]EntitySearchRow, error) {
	call, q, err := scopedTextQuery("entity_text_v2", "name", q, slug)
	if err != nil {
		return nil, err
	}
	ok := q != ""
	if !ok {
		return nil, nil
	}
	recs, err := s.runRead(ctx, `
`+call+`
RETURN node.id AS id, node.name AS name, node.type AS type, node.domain AS domain
ORDER BY score DESC
LIMIT $limit`,
		map[string]any{"q": q, "limit": limit, "domain": slug})
	if err != nil {
		return nil, fmt.Errorf("graph: entity text search: %w", err)
	}
	out := make([]EntitySearchRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, EntitySearchRow{
			ID:     getString(r, "id"),
			Name:   getString(r, "name"),
			Type:   getString(r, "type"),
			Domain: getString(r, "domain"),
		})
	}
	return out, nil
}

// ChunkSearchRow is one chunk text-search hit with its document title.
type ChunkSearchRow struct {
	Domain string
	ID     string
	DocID  string
	Title  string
	Text   string
}

// ChunkTextSearch runs the Tantivy chunk index. The hits are cut to limit
// before the Document join: the index can return up to 1,000 hits, and
// joining all of them first made a chunk search take seconds.
func (s *Store) ChunkTextSearch(ctx context.Context, q string, limit int) ([]ChunkSearchRow, error) {
	return s.ChunkTextSearchIn(ctx, q, "", limit)
}

func (s *Store) ChunkTextSearchIn(ctx context.Context, q, slug string, limit int) ([]ChunkSearchRow, error) {
	call, q, err := scopedTextQuery("chunk_text_v2", "text", q, slug)
	if err != nil {
		return nil, err
	}
	ok := q != ""
	if !ok {
		return nil, nil
	}
	recs, err := s.runRead(ctx, `
`+call+`
WITH node, score
ORDER BY score DESC
LIMIT $limit
OPTIONAL MATCH (d:Document {id: node.doc_id})
RETURN node.id AS id, node.doc_id AS doc_id, node.text AS text, node.domain AS domain, coalesce(d.title, '') AS title, score
ORDER BY score DESC`,
		map[string]any{"q": q, "limit": limit, "domain": slug})
	if err != nil {
		return nil, fmt.Errorf("graph: chunk text search: %w", err)
	}
	out := make([]ChunkSearchRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, ChunkSearchRow{
			ID:     getString(r, "id"),
			DocID:  getString(r, "doc_id"),
			Title:  getString(r, "title"),
			Text:   getString(r, "text"),
			Domain: getString(r, "domain"),
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
RETURN e.id AS id, e.key AS key, e.name AS name, e.type AS type, e.domain AS domain`,
		map[string]any{"id": id})
	if err != nil {
		return EntityRef{}, fmt.Errorf("graph: entity ref: %w", err)
	}
	if len(recs) == 0 {
		return EntityRef{}, fmt.Errorf("graph: entity %s: %w", id, ErrNotFound)
	}
	return EntityRef{
		ID:     getString(recs[0], "id"),
		Key:    getString(recs[0], "key"),
		Name:   getString(recs[0], "name"),
		Type:   getString(recs[0], "type"),
		Domain: getString(recs[0], "domain"),
	}, nil
}

// scopedTextQuery restricts Tantivy before its internal 1,000-hit cap, then
// checks the exact slug. Unscoped search retains search_all's name-only semantics.
func scopedTextQuery(index, field, q, slug string) (string, string, error) {
	if slug != "" && !ValidDomain(slug) {
		return "", "", fmt.Errorf("graph: invalid domain slug %q", slug)
	}
	terms := strings.Fields(TextQueryTerms(q))
	if len(terms) == 0 {
		return "", "", nil
	}
	if slug == "" {
		return "CALL text_search.search_all('" + index + "', $q) YIELD node, score", strings.Join(terms, " "), nil
	}
	for i := range terms {
		terms[i] = "data." + field + ":" + terms[i]
	}
	query := `data.domain_tok:"` + domain.DomainToken(slug) + `" AND (` + strings.Join(terms, " ") + `)`
	return "CALL text_search.search('" + index + "', $q) YIELD node, score WITH node, score WHERE node.domain = $domain", query, nil
}
