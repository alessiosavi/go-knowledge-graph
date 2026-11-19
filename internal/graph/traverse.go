package graph

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
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
	Nodes       []EntityRef
	Edges       []PathEdge
	Hidden      HiddenCounts
	Requested   string `json:"requested,omitempty"`
	ResolvedVia string `json:"resolved_via,omitempty"`
}

// traverseDepthCap is the agent-tool ceiling for traversal depth.
const traverseDepthCap = 2

// Traverse walks fact edges from the start entity (id or key) up to depth
// hops, optionally filtered by relation and direction (out|in|both).
// Judge-confirmed facts are visible regardless of includeLowConfidence.
// Depth above the tool cap is an error. The limit caps the total edges returned.
func (s *Store) Traverse(ctx context.Context, startID, rel, direction string, depth, limit int, includeLowConfidence bool) (Subgraph, error) {
	return s.TraverseOpts(ctx, startID, rel, direction, depth, limit, ViewOptions{IncludeLowConfidence: includeLowConfidence})
}

// TraverseOpts applies fact visibility options at each hop. Included candidates
// are marked unverified and their far endpoints are excluded from expansion.
// Hidden counts always describe the start entity, regardless of depth or filters.
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
		return Subgraph{}, argerr.Errorf("graph: invalid domain slug %q", opt.Domain)
	}
	var start EntityRef
	var provenance Provenance
	err := s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		var err error
		start, provenance, err = resolveEntitySelectorTx(ctx, tx, startID, opt.Domain)
		return err
	})
	if err != nil {
		return Subgraph{}, fmt.Errorf("graph: traverse: %w", err)
	}
	sub := Subgraph{Nodes: []EntityRef{start}, Requested: provenance.Requested, ResolvedVia: provenance.ResolvedVia}
	sub.Hidden, err = s.hiddenCounts(ctx, start.Key)
	if err != nil {
		return Subgraph{}, err
	}
	visited := map[string]bool{start.ID: true}
	enqueued := map[string]bool{start.ID: true}
	seenEdges := map[string]bool{}
	frontier := []EntityRef{start}

	for hop := 0; hop < depth && len(frontier) > 0 && len(sub.Edges) < limit; hop++ {
		rows, err := s.traverseHop(ctx, frontier, rel, direction, limit-len(sub.Edges), opt, seenEdges)
		if err != nil {
			return Subgraph{}, err
		}
		var next []EntityRef
		for _, row := range rows {
			e, n := row.edge, row.other
			sub.Edges = append(sub.Edges, e)
			if !visited[n.ID] {
				visited[n.ID] = true
				sub.Nodes = append(sub.Nodes, n)
			}
			// A candidate may reveal a node, but only a typed edge can enqueue it.
			if !e.Unverified && !enqueued[n.ID] {
				enqueued[n.ID] = true
				next = append(next, n)
			}
		}
		frontier = next
	}
	return sub, nil
}

type traverseRow struct {
	edge  PathEdge
	other EntityRef
}

func (e PathEdge) key() string {
	return e.From + "|" + e.Rel + "|" + e.To + "|" + e.ChunkID
}

func appendTraverseKeys(keys []string, rows map[string][]traverseRow) []string {
	for _, batch := range rows {
		for _, row := range batch {
			keys = append(keys, row.edge.key())
		}
	}
	return keys
}

// traverseHop fills in frontier order, taking out rows before in rows on each
// pass. The typed and candidate passes can each top up once per direction,
// giving a traversal at most depth × directions × 4 expansion reads.
func (s *Store) traverseHop(ctx context.Context, frontier []EntityRef, rel, direction string, remaining int, opt ViewOptions, seenEdges map[string]bool) ([]traverseRow, error) {
	type batch struct {
		incoming         bool
		capped           []string
		cappedCandidates []map[string]any
		excluded         []string
	}
	batches := []batch{{incoming: direction == "in"}}
	if direction == "both" {
		batches = append(batches, batch{incoming: true})
	}
	share := max(5, 1+(remaining-1)/len(frontier))
	ids := make([]string, 0, len(frontier))
	for _, node := range frontier {
		ids = append(ids, node.ID)
	}
	rows := make(map[string][]traverseRow, len(frontier))
	excluded := slices.Collect(maps.Keys(seenEdges))
	for i := range batches {
		b := &batches[i]
		b.excluded = excluded
		typed := make(map[string][]traverseRow, len(frontier))
		params := map[string]any{
			"ids": ids, "share": share, "include": opt.IncludeLowConfidence,
			"confirmed_only": opt.ConfirmedOnly, "excluded": excluded,
		}
		if err := s.readTraverseRows(ctx, typed, rel, b.incoming, false, params); err != nil {
			return nil, fmt.Errorf("graph: traverse hop: %w", err)
		}
		for _, id := range ids {
			if len(typed[id]) == share {
				b.capped = append(b.capped, id)
			}
			rows[id] = append(rows[id], typed[id]...)
		}
		// Rows already returned from the outgoing batch use no incoming slots.
		if i+1 < len(batches) {
			excluded = appendTraverseKeys(excluded, typed)
		}
	}

	var kept []traverseRow
	typedCounts := make(map[string]int, len(frontier))
	fill := func(rows map[string][]traverseRow) {
		for _, id := range ids {
			for _, row := range rows[id] {
				key := row.edge.key()
				if seenEdges[key] {
					continue
				}
				seenEdges[key] = true
				kept = append(kept, row)
				if !row.edge.Unverified {
					typedCounts[id]++
				}
				if len(kept) == remaining {
					return
				}
			}
		}
	}
	fill(rows)
	if len(kept) == remaining {
		return kept, nil
	}

	topUp := make(map[string][]traverseRow)
	for _, b := range batches {
		if len(b.capped) == 0 {
			continue
		}
		params := map[string]any{
			"ids": b.capped, "offset": share, "share": remaining - len(kept),
			"include": opt.IncludeLowConfidence, "confirmed_only": opt.ConfirmedOnly,
			"excluded": b.excluded, "filled": slices.Collect(maps.Keys(seenEdges)),
		}
		if err := s.readTraverseRows(ctx, topUp, rel, b.incoming, false, params); err != nil {
			return nil, fmt.Errorf("graph: traverse top-up: %w", err)
		}
	}
	fill(topUp)

	if len(kept) < remaining && opt.IncludeUnverified && !opt.ConfirmedOnly {
		candidates := make(map[string][]traverseRow)
		excluded = slices.Collect(maps.Keys(seenEdges))
		for i := range batches {
			b := &batches[i]
			var anchors []map[string]any
			for _, node := range frontier {
				// The unused share after distinct typed rows is shared across
				// candidate directions. Outgoing candidates use those slots first.
				if slots := share - typedCounts[node.ID] - len(candidates[node.ID]); slots > 0 {
					anchors = append(anchors, map[string]any{"id": node.ID, "key": node.Key, "share": slots})
				}
			}
			if len(anchors) == 0 {
				break
			}
			b.excluded = excluded
			batchCandidates := make(map[string][]traverseRow)
			if err := s.readTraverseRows(ctx, batchCandidates, rel, b.incoming, true, map[string]any{"anchors": anchors, "excluded": b.excluded}); err != nil {
				return nil, fmt.Errorf("graph: traverse candidates: %w", err)
			}
			for _, node := range frontier {
				id := node.ID
				read := batchCandidates[id]
				if len(read) == 0 {
					continue
				}
				if len(read) == share-typedCounts[id]-len(candidates[id]) {
					b.cappedCandidates = append(b.cappedCandidates, map[string]any{"id": id, "key": node.Key, "offset": len(read)})
				}
				candidates[id] = append(candidates[id], read...)
			}
			if i+1 < len(batches) {
				excluded = appendTraverseKeys(excluded, candidates)
			}
		}
		fill(candidates)
		if len(kept) == remaining {
			return kept, nil
		}

		candidateTopUp := make(map[string][]traverseRow)
		filled := slices.Collect(maps.Keys(seenEdges))
		for _, b := range batches {
			if len(b.cappedCandidates) == 0 {
				continue
			}
			for _, anchor := range b.cappedCandidates {
				anchor["share"] = remaining - len(kept)
			}
			params := map[string]any{"anchors": b.cappedCandidates, "excluded": b.excluded, "filled": filled}
			if err := s.readTraverseRows(ctx, candidateTopUp, rel, b.incoming, true, params); err != nil {
				return nil, fmt.Errorf("graph: traverse candidates top-up: %w", err)
			}
		}
		fill(candidateTopUp)
	}
	return kept, nil
}

func (s *Store) readTraverseRows(ctx context.Context, rows map[string][]traverseRow, rel string, incoming, unverified bool, params map[string]any) error {
	_, topUp := params["filled"]
	offset := "0"
	if topUp {
		offset = "$offset"
	}
	pattern := `UNWIND $ids AS id
WITH id, $share AS share, ` + offset + ` AS row_offset
MATCH (expanded:Entity {id: id})-[r]->(other:Entity)`
	if incoming {
		pattern = `UNWIND $ids AS id
WITH id, $share AS share, ` + offset + ` AS row_offset
MATCH (expanded:Entity {id: id})<-[r]-(other:Entity)`
	}
	visibility, relation := typedFactVisibility, "type(r)"
	if unverified {
		anchor, endpoint := "head_key", "tail_key"
		if incoming {
			anchor, endpoint = endpoint, anchor
		}
		if topUp {
			offset = "anchor.offset"
		}
		// Bind the candidate before joining its endpoints so the planner can
		// use the candidate and entity indexes without starting from an entity scan.
		pattern = `UNWIND $anchors AS anchor
WITH anchor.id AS id, anchor.key AS key, anchor.share AS share, ` + offset + ` AS row_offset
MATCH (r:FactCandidate {` + anchor + `: key})
WITH id, key, share, row_offset, r
MATCH (expanded:Entity {key: key}), (other:Entity {key: r.` + endpoint + `})`
		visibility, relation = "coalesce(r.judge, '') IN ['', 'unclear']", "r.rel"
	}
	if rel != "" {
		visibility += " AND " + relation + " = $rel"
		params["rel"] = sanitizeRel(rel)
	}
	fromKey, toKey := "expanded.key", "other.key"
	if incoming {
		fromKey, toKey = toKey, fromKey
	}
	edgeKey := fmt.Sprintf("coalesce(%s, '') + '|' + coalesce(%s, '') + '|' + coalesce(%s, '') + '|' + coalesce(r.chunk_id, '')", fromKey, relation, toKey)
	page := `WITH id, edges[..share] AS kept`
	if topUp {
		// Keep the same ranked prefix as the first read when applying the
		// offset, then remove any rows already filled by the other direction.
		page = `WITH id, share, edges[row_offset..] AS rest
WITH id, [edge IN rest WHERE NOT edge.key IN $filled][..share] AS kept
WHERE size(kept) > 0`
	}
	// Keep the ordered prefix per source before joining chunks. Carry its rank
	// through UNWIND and the join so the result order is explicit.
	query := pattern + "\nWHERE " + visibility + `
WITH id, share, row_offset, expanded, other, r, ` + relation + ` AS rel, ` + edgeKey + ` AS edge_key
WHERE NOT edge_key IN $excluded
WITH id, share, row_offset, expanded, other, r, rel, edge_key
ORDER BY coalesce(r.judge, '') = 'confirmed' DESC, coalesce(r.low_confidence, false),
         coalesce(r.confidence, 0) DESC, rel, other.key, coalesce(r.chunk_id, '')
WITH id, share, row_offset, collect({source_key: expanded.key, other: other, fact: r, rel: rel, key: edge_key}) AS edges
` + page + `
UNWIND range(0, size(kept) - 1) AS rank
WITH id, rank, kept[rank] AS edge
WITH id, rank, edge.source_key AS source_key, edge.other AS other, edge.fact AS r, edge.rel AS rel
OPTIONAL MATCH (c:Chunk {id: r.chunk_id})
RETURN id AS source_id, source_key, rel, other.id AS other_id, other.key AS other_key,
       other.name AS other_name, other.type AS other_type, other.domain AS other_domain,
       coalesce(r.confidence, 0) AS confidence, coalesce(r.judge, '') AS judge,
       coalesce(r.low_confidence, false) AS low, coalesce(r.doc_id, '') AS doc_id,
       coalesce(r.chunk_id, '') AS chunk_id, coalesce(r.head_start, -1) AS head_start,
       coalesce(r.tail_start, -1) AS tail_start, coalesce(c.text, '') AS text
ORDER BY source_id, rank`
	recs, err := s.runRead(ctx, query, params)
	if err != nil {
		return err
	}
	for _, r := range recs {
		from, to := getString(r, "source_key"), getString(r, "other_key")
		// The excerpt anchors on the node being expanded: the head for a
		// forward hop, the tail for a backward hop.
		center := getInt(r, "head_start")
		if incoming {
			from, to = to, from
			center = getInt(r, "tail_start")
		}
		id := getString(r, "source_id")
		rows[id] = append(rows[id], traverseRow{
			edge: PathEdge{
				From:          from,
				To:            to,
				Rel:           getString(r, "rel"),
				Confidence:    getFloat(r, "confidence"),
				Judge:         getString(r, "judge"),
				Unverified:    unverified,
				LowConfidence: getBool(r, "low"),
				DocID:         getString(r, "doc_id"),
				ChunkID:       getString(r, "chunk_id"),
				Excerpt:       ExcerptWindow(getString(r, "text"), center, 120),
			},
			other: EntityRef{
				ID:     getString(r, "other_id"),
				Key:    getString(r, "other_key"),
				Name:   getString(r, "other_name"),
				Type:   getString(r, "other_type"),
				Domain: getString(r, "other_domain"),
			},
		})
	}
	return nil
}

// EntitySearchRow holds the fields needed to render and rank an entity hit.
type EntitySearchRow struct {
	Domain       string
	ID           string
	Name         string
	Type         string
	MentionCount int
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
RETURN node.id AS id, node.name AS name, node.type AS type, node.domain AS domain,
       coalesce(node.mention_count, 0) AS mention_count
ORDER BY score DESC
LIMIT $limit`,
		map[string]any{"q": q, "limit": limit, "domain": slug})
	if err != nil {
		return nil, fmt.Errorf("graph: entity text search: %w", err)
	}
	return entitySearchRows(recs), nil
}

// EntitySearchRowsByID resolves entity ids in one read. Missing ids are
// omitted, and callers restore their own ranking order.
func (s *Store) EntitySearchRowsByID(ctx context.Context, ids []string) ([]EntitySearchRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	recs, err := s.runRead(ctx, `
UNWIND $ids AS entity_id
MATCH (e:Entity {id: entity_id})
RETURN e.id AS id, e.name AS name, e.type AS type, e.domain AS domain,
       coalesce(e.mention_count, 0) AS mention_count`, map[string]any{"ids": ids})
	if err != nil {
		return nil, fmt.Errorf("graph: entity search rows by id: %w", err)
	}
	return entitySearchRows(recs), nil
}

func entitySearchRows(recs []*neo4j.Record) []EntitySearchRow {
	out := make([]EntitySearchRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, EntitySearchRow{
			ID:           getString(r, "id"),
			Name:         getString(r, "name"),
			Type:         getString(r, "type"),
			Domain:       getString(r, "domain"),
			MentionCount: getInt(r, "mention_count"),
		})
	}
	return out
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
