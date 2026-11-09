package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

// Result shapes mirror the REST/MCP wire vocabulary so the model, the
// evidence array and the UI share one language.
type searchHitOut struct {
	Kind    string  `json:"kind"`
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Score   float64 `json:"score"`
	Snippet string  `json:"snippet"`
}

type searchOut struct {
	Hits []searchHitOut `json:"hits"`
}

type entityRefOut struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type factOut struct {
	Rel           string       `json:"rel"`
	Other         entityRefOut `json:"other"`
	Confidence    float64      `json:"confidence"`
	Judge         string       `json:"judge,omitempty"`
	LowConfidence bool         `json:"low_confidence"`
	Outgoing      bool         `json:"outgoing"`
	DocID         string       `json:"doc_id"`
	ChunkID       string       `json:"chunk_id"`
	Excerpt       string       `json:"excerpt"`
}

type entityOut struct {
	ID           string    `json:"id"`
	Key          string    `json:"key"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	Aliases      []string  `json:"aliases"`
	MentionCount int       `json:"mention_count"`
	Facts        []factOut `json:"facts"`
}

type edgeOut struct {
	From          string  `json:"from"`
	To            string  `json:"to"`
	Rel           string  `json:"rel"`
	Confidence    float64 `json:"confidence"`
	Judge         string  `json:"judge,omitempty"`
	LowConfidence bool    `json:"low_confidence"`
	DocID         string  `json:"doc_id"`
	ChunkID       string  `json:"chunk_id"`
	Excerpt       string  `json:"excerpt"`
}

type traverseOut struct {
	Nodes []entityRefOut `json:"nodes"`
	Edges []edgeOut      `json:"edges"`
}

type documentOut struct {
	ID         string    `json:"id"`
	SourceKind string    `json:"source_kind"`
	Source     string    `json:"source"`
	Title      string    `json:"title,omitempty"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type documentsOut struct {
	Documents []documentOut `json:"documents"`
}

type cypherOut struct {
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
}

const (
	searchDefaultLimit = 20
	searchMaxLimit     = 100
	statusListLimit    = 100
	docsDefaultLimit   = 100
	docsMaxLimit       = 1000
	traverseMaxLimit   = 200
)

// toolDefs returns the read-only tool set exposed to the model: the MCP
// read set (search, get_entity, traverse, list_documents, status, cypher);
// no ingestion in v1.
func toolDefs() []toolDef {
	return []toolDef{
		functionTool("search",
			"Full-text search over entity names and chunk text; returns scored hits with ids, titles and snippets.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "full-text query over entity names and chunk text"},
					"kind":  map[string]any{"type": "string", "enum": []string{"entity", "chunk", "auto"}, "description": "hit kind filter (default auto)"},
					"limit": map[string]any{"type": "integer", "description": "maximum hits (default 20, capped at 100)"},
				},
				"required": []string{"query"},
			}),
		functionTool("get_entity",
			"One entity with aliases and evidenced facts (excerpt, confidence, judge status); resolve by id or exact name.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":                     map[string]any{"type": "string", "description": "entity id"},
					"name":                   map[string]any{"type": "string", "description": "entity name (exact after case folding; used when id is omitted)"},
					"include_low_confidence": map[string]any{"type": "boolean", "description": "include facts below the assert floor"},
				},
			}),
		functionTool("traverse",
			"Walk fact edges from a start entity up to two hops, with direction and relation filters and evidence excerpts.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entity":                 map[string]any{"type": "string", "description": "start entity id or key"},
					"relation":               map[string]any{"type": "string", "description": "relation filter (e.g. works_at)"},
					"direction":              map[string]any{"type": "string", "enum": []string{"out", "in", "both"}, "description": "edge direction (default out)"},
					"depth":                  map[string]any{"type": "integer", "description": "hops, 1 or 2 (default 1)"},
					"limit":                  map[string]any{"type": "integer", "description": "maximum edges (default 50, capped at 200)"},
					"include_low_confidence": map[string]any{"type": "boolean", "description": "include facts below the assert floor"},
				},
				"required": []string{"entity"},
			}),
		functionTool("list_documents",
			"List documents with metadata, optionally filtered by pipeline status.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit":  map[string]any{"type": "integer", "description": "maximum documents (default 100, capped at 1000)"},
					"status": map[string]any{"type": "string", "description": "filter by pipeline status (received|fetched|chunked|extracted|merged|verifying|done|failed|superseded)"},
				},
			}),
		functionTool("status",
			"Pipeline status and error of one document, or of the most recently updated documents when no id is given.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"document_id": map[string]any{"type": "string", "description": "one document id; omit to list documents"},
				},
			}),
		functionTool("cypher",
			"Run a guarded read-only Cypher query with the configured row cap and timeout; write clauses are rejected.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "single read-only Cypher statement"},
					"limit": map[string]any{"type": "integer", "description": "row cap; clamped to the configured cypher_rows"},
				},
				"required": []string{"query"},
			}),
	}
}

// execTool runs one tool call; its result marshals into the evidence array
// and into the tool message the model receives.
func (s *Server) execTool(ctx context.Context, name string, args json.RawMessage) (any, error) {
	if s.store == nil {
		return nil, errors.New("knowledge graph unavailable: no store wired")
	}
	switch name {
	case "search":
		return s.toolSearch(ctx, args)
	case "get_entity":
		return s.toolGetEntity(ctx, args)
	case "traverse":
		return s.toolTraverse(ctx, args)
	case "list_documents":
		return s.toolListDocuments(ctx, args)
	case "status":
		return s.toolStatus(ctx, args)
	case "cypher":
		return s.toolCypher(ctx, args)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func decodeArgs(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}

func (s *Server) toolSearch(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Query string `json:"query"`
		Kind  string `json:"kind"`
		Limit int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Query == "" {
		return nil, errors.New("query is required")
	}
	kind := args.Kind
	if kind == "" {
		kind = "auto"
	}
	switch kind {
	case "entity", "chunk", "auto":
	default:
		return nil, fmt.Errorf("kind %q not in entity|chunk|auto", kind)
	}
	limit := args.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}
	hits, err := search.Query(ctx, s.store, s.emb, args.Query, kind, limit)
	if err != nil {
		return nil, err
	}
	out := make([]searchHitOut, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchHitOut{Kind: h.Kind, ID: h.ID, Title: h.Title, Score: h.Score, Snippet: h.Snippet})
	}
	return searchOut{Hits: out}, nil
}

func (s *Server) toolGetEntity(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ID                   string `json:"id"`
		Name                 string `json:"name"`
		IncludeLowConfidence bool   `json:"include_low_confidence"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	id := args.ID
	switch {
	case id == "" && args.Name == "":
		return nil, errors.New("id or name is required")
	case id == "":
		ref, err := s.store.EntityByName(ctx, args.Name)
		if err != nil {
			if errors.Is(err, graph.ErrNotFound) {
				return nil, fmt.Errorf("no entity matching %q", args.Name)
			}
			return nil, fmt.Errorf("entity lookup: %w", err)
		}
		id = ref.ID
	}
	view, err := s.store.EntityView(ctx, id, args.IncludeLowConfidence)
	if err != nil {
		return nil, fmt.Errorf("entity %s: %w", id, err)
	}
	facts := make([]factOut, 0, len(view.Facts))
	for _, f := range view.Facts {
		facts = append(facts, factOut{
			Rel:           f.Rel,
			Other:         entityRefOut{ID: f.Other.ID, Key: f.Other.Key, Name: f.Other.Name, Type: f.Other.Type},
			Confidence:    f.Confidence,
			Judge:         f.Judge,
			LowConfidence: f.LowConfidence,
			Outgoing:      f.Outgoing,
			DocID:         f.DocID,
			ChunkID:       f.ChunkID,
			Excerpt:       f.Excerpt,
		})
	}
	aliases := view.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	return entityOut{
		ID:           view.ID,
		Key:          view.Key,
		Name:         view.Name,
		Type:         view.Type,
		Aliases:      aliases,
		MentionCount: view.MentionCount,
		Facts:        facts,
	}, nil
}

func (s *Server) toolTraverse(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Entity               string `json:"entity"`
		Relation             string `json:"relation"`
		Direction            string `json:"direction"`
		Depth                int    `json:"depth"`
		Limit                int    `json:"limit"`
		IncludeLowConfidence bool   `json:"include_low_confidence"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Entity == "" {
		return nil, errors.New("entity is required")
	}
	depth := args.Depth
	if depth == 0 {
		depth = 1
	}
	if depth < 1 || depth > 2 {
		return nil, fmt.Errorf("depth must be 1 or 2, got %d", depth)
	}
	direction := args.Direction
	if direction == "" {
		direction = "out"
	}
	switch direction {
	case "out", "in", "both":
	default:
		return nil, fmt.Errorf("direction %q not in out|in|both", direction)
	}
	limit := args.Limit
	if limit < 0 {
		return nil, errors.New("limit must not be negative")
	}
	if limit > traverseMaxLimit {
		limit = traverseMaxLimit
	}
	sub, err := s.store.Traverse(ctx, args.Entity, args.Relation, direction, depth, limit, args.IncludeLowConfidence)
	if err != nil {
		return nil, err
	}
	nodes := make([]entityRefOut, 0, len(sub.Nodes))
	for _, n := range sub.Nodes {
		nodes = append(nodes, entityRefOut{ID: n.ID, Key: n.Key, Name: n.Name, Type: n.Type})
	}
	edges := make([]edgeOut, 0, len(sub.Edges))
	for _, e := range sub.Edges {
		edges = append(edges, edgeOut{
			From:          e.From,
			To:            e.To,
			Rel:           e.Rel,
			Confidence:    e.Confidence,
			Judge:         e.Judge,
			LowConfidence: e.LowConfidence,
			DocID:         e.DocID,
			ChunkID:       e.ChunkID,
			Excerpt:       e.Excerpt,
		})
	}
	return traverseOut{Nodes: nodes, Edges: edges}, nil
}

func (s *Server) toolListDocuments(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Limit  int    `json:"limit"`
		Status string `json:"status"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	limit := args.Limit
	if limit < 0 {
		return nil, errors.New("limit must not be negative")
	}
	if limit == 0 {
		limit = docsDefaultLimit
	}
	if limit > docsMaxLimit {
		limit = docsMaxLimit
	}
	var docs []graph.Document
	var err error
	if args.Status != "" {
		docs, err = s.store.DocumentsWithStatus(ctx, graph.Status(args.Status), limit)
	} else {
		docs, err = s.store.Documents(ctx, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	return documentsOut{Documents: toDocumentOuts(docs)}, nil
}

func (s *Server) toolStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		DocumentID string `json:"document_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.DocumentID != "" {
		doc, err := s.store.Document(ctx, args.DocumentID)
		if err != nil {
			return nil, fmt.Errorf("document %s: %w", args.DocumentID, err)
		}
		return documentsOut{Documents: []documentOut{toDocumentOut(*doc)}}, nil
	}
	docs, err := s.store.Documents(ctx, statusListLimit)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	return documentsOut{Documents: toDocumentOuts(docs)}, nil
}

func (s *Server) toolCypher(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	rewritten, err := api.CheckReadOnly(args.Query)
	if err != nil {
		return nil, err
	}
	capRows := s.cfg.Limits.CypherRows
	if capRows <= 0 {
		capRows = 1000
	}
	if n, ok := api.LimitClause(rewritten); ok && n > capRows {
		return nil, fmt.Errorf("LIMIT %d exceeds the configured cypher_rows cap %d", n, capRows)
	}
	rows := capRows
	if args.Limit > 0 && args.Limit < rows {
		rows = args.Limit
	}
	timeout := time.Duration(s.cfg.Limits.CypherTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := s.store.RunReadQuery(qctx, rewritten, nil, rows)
	if err != nil {
		if errors.Is(qctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("query timed out")
		}
		var qerr *graph.QueryError
		if errors.As(err, &qerr) {
			return nil, fmt.Errorf("query failed: %s", qerr.Error())
		}
		return nil, err
	}
	return cypherOut{Columns: res.Columns, Rows: res.Rows}, nil
}

func toDocumentOut(d graph.Document) documentOut {
	return documentOut{
		ID:         d.ID,
		SourceKind: d.SourceKind,
		Source:     d.Source,
		Title:      d.Title,
		Status:     string(d.Status),
		Error:      d.Error,
		CreatedAt:  d.CreatedAt,
		UpdatedAt:  d.UpdatedAt,
	}
}

func toDocumentOuts(docs []graph.Document) []documentOut {
	out := make([]documentOut, 0, len(docs))
	for _, d := range docs {
		out = append(out, toDocumentOut(d))
	}
	return out
}
