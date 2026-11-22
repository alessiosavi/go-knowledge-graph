package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

// Result shapes mirror the REST/MCP wire vocabulary so the model, the
// evidence array and the UI share one language.
type searchHitOut struct {
	Domain     string  `json:"domain"`
	Kind       string  `json:"kind"`
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Score      float64 `json:"score"`
	Snippet    string  `json:"snippet"`
	DocumentID string  `json:"document_id,omitempty"`
}

type searchOut struct {
	Hits []searchHitOut `json:"hits"`
}

type entityRefOut struct {
	Domain string `json:"domain"`
	ID     string `json:"id"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	Type   string `json:"type"`
}

type coMentionOut struct {
	entityRefOut
	SharedChunks int `json:"shared_chunks"`
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
	ID           string              `json:"id"`
	Key          string              `json:"key"`
	Name         string              `json:"name"`
	Type         string              `json:"type"`
	Domain       string              `json:"domain"`
	Aliases      []string            `json:"aliases"`
	MentionCount int                 `json:"mention_count"`
	Facts        []factOut           `json:"facts"`
	Together     []coMentionOut      `json:"together"`
	Hidden       *graph.HiddenCounts `json:"hidden,omitempty"`
	Requested    string              `json:"requested,omitempty"`
	ResolvedVia  string              `json:"resolved_via,omitempty"`
	Match        string              `json:"match,omitempty"`
}

type nameCandidateOut struct {
	entityRefOut
	Match string `json:"match,omitempty"`
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
	Nodes       []entityRefOut      `json:"nodes"`
	Edges       []edgeOut           `json:"edges"`
	Hidden      *graph.HiddenCounts `json:"hidden,omitempty"`
	Requested   string              `json:"requested,omitempty"`
	ResolvedVia string              `json:"resolved_via,omitempty"`
}

type documentOut struct {
	Domain     string    `json:"domain"`
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

// toolDefs returns the read-only graph and product tools exposed to the model.
func toolDefs() []toolDef {
	return []toolDef{
		functionTool("search",
			"Full-text search over entity names and chunk text; returns scored hits with ids, titles and snippets. Start with limit 10.",
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
			"One entity with aliases, co-mentions and evidenced facts. A merge redirect includes requested and resolved_via=merged. Name lookup tries exact matches first, then verified folds marked match=fold. Unscoped ambiguous names return one candidate per domain.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":                     map[string]any{"type": "string", "description": "live or retired entity id or key"},
					"name":                   map[string]any{"type": "string", "description": "entity name, used when id is omitted"},
					"include_low_confidence": map[string]any{"type": "boolean", "description": "include facts below the assert floor. Judge-confirmed facts are always shown"},
				},
			}),
		functionTool("get_chunks",
			"Read chunk text, up to 4 KiB per chunk; `truncated` marks a cut, by id (ids from search hits), optionally with up to 2 neighbouring chunks of the same document. Use this instead of cypher to read chunk text. omitted_for_budget may also list neighbour ids."+
				chunkread.ProductsToolDescription+" A domain-scoped agent cannot open a cross-domain result with get_product.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ids":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": chunkread.MaxIDs, "description": "1–20 chunk ids from search hits"},
					"neighbours":     map[string]any{"type": "integer", "minimum": 0, "maximum": chunkread.MaxNeighbours, "description": "chunks before and after each requested chunk (default 0, maximum 2)"},
					"with_products":  map[string]any{"type": "boolean", "default": false, "description": chunkread.WithProductsDescription},
					"products_limit": map[string]any{"type": "integer", "minimum": 1, "maximum": chunkread.MaxProductsLimit, "default": chunkread.DefaultProductsLimit, "description": chunkread.ProductsLimitDescription},
				},
				"required": []string{"ids"},
			}),
		functionTool("traverse",
			"Walk fact edges from a start entity up to two hops, with direction and relation filters and evidence excerpts. A merge redirect includes requested and resolved_via=merged. Start with limit 10.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entity":                 map[string]any{"type": "string", "description": "start entity id or key"},
					"relation":               map[string]any{"type": "string", "description": "relation filter (e.g. works_at)"},
					"direction":              map[string]any{"type": "string", "enum": []string{"out", "in", "both"}, "description": "edge direction (default out)"},
					"depth":                  map[string]any{"type": "integer", "description": "hops, 1 or 2 (default 1)"},
					"limit":                  map[string]any{"type": "integer", "description": "maximum edges (default 50, capped at 200)"},
					"include_low_confidence": map[string]any{"type": "boolean", "description": "include facts below the assert floor. Judge-confirmed facts are always shown"},
				},
				"required": []string{"entity"},
			}),
		functionTool("list_documents",
			"List documents with metadata, optionally filtered by pipeline status. Start with limit 10.",
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
		functionTool("get_product", graph.ProductLookupDescription+
			" Set with_mentions to include passages from done documents with exact matches for the product's catalogue_code or product_reference. Domain also filters passages."+
			" Retrieval is approximate: spellings that split a stored code (151L-Z for 151LZ, TC 2SL for TC2SL) or use fullwidth characters can be missed, and mentions_partial means search limits or skipped clauses may have left matches unchecked."+
			" Search can miss splits inside stored runs (151L-Z, A-B12345, TC 2SL), fullwidth forms, case-mapping differences (ß, İ), and tokens of 40 bytes or more."+
			" Use gkg products mentions-scan for the complete offline scan."+
			" To fit the result budget, mentions are dropped from the end before features. See mentions_omitted_for_budget and features_omitted for the counts.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":             map[string]any{"type": "string", "description": "Graph product ID"},
					"store":          map[string]any{"type": "string", "description": "Store for an exact lookup by product_id"},
					"product_id":     map[string]any{"type": "string", "description": "Full source product ID including its store prefix"},
					"domain":         map[string]any{"type": "string", "description": "Optional domain slug for associated pages and passages within the request scope. A domain outside the request scope is rejected."},
					"with_mentions":  map[string]any{"type": "boolean", "default": false, "description": "Include passages with exact identifier matches"},
					"mentions_limit": map[string]any{"type": "integer", "minimum": 1, "maximum": graph.MaxMentionsLimit, "default": graph.DefaultMentionsLimit, "description": "Maximum passages returned when with_mentions is true"},
				},
			}),
		functionTool("find_products", graph.ProductSearchDescription+
			" A page may contain fewer hits or less evidence to fit the result budget.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"domain":  map[string]any{"type": "string", "description": "Restrict matches to this domain ID within the request scope"},
					"feature": map[string]any{"type": "string", "description": "Feature ID from list_features"},
					"value":   map[string]any{"type": "string", "description": "Exact folded value or item. Use either value or numeric bounds"},
					"min":     map[string]any{"type": "string", "description": "Inclusive lower bound as a decimal string in the canonical unit"},
					"max":     map[string]any{"type": "string", "description": "Inclusive upper bound as a decimal string in the canonical unit"},
					"limit":   map[string]any{"type": "integer", "description": "Maximum hits (default 10, capped at 20)"},
					"cursor":  map[string]any{"type": "string", "description": "Opaque continuation cursor from the previous page with the same filters and limit"},
				},
				"required": []string{"feature"},
			}),
		functionTool("list_features", graph.ProductFeaturesDescription+
			", with release status, numeric-search support and stores with released keys in scope. If truncated, narrow with query.",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Case-insensitive substring of the feature ID or name"},
				},
			}),
	}
}

// execTool runs one tool call; its result marshals into the evidence array
// and into the tool message the model receives.
func (s *Server) execTool(ctx context.Context, name string, args json.RawMessage) (_ any, err error) {
	var run func(context.Context, json.RawMessage) (any, error)
	switch name {
	case "search":
		run = s.toolSearch
	case "get_entity":
		run = s.toolGetEntity
	case "get_chunks":
		run = s.toolGetChunks
	case "traverse":
		run = s.toolTraverse
	case "list_documents":
		run = s.toolListDocuments
	case "status":
		run = s.toolStatus
	case "cypher":
		run = s.toolCypher
	case "get_product":
		run = s.toolGetProduct
	case "find_products":
		run = s.toolFindProducts
	case "list_features":
		run = s.toolListFeatures
	}
	if run != nil && s.metrics != nil {
		start := time.Now()
		defer func() {
			elapsed := time.Since(start)
			s.metrics.ObserveRequest("agent_tool", name, obs.ToolOutcome(err), elapsed)
		}()
	}
	if s.store == nil {
		return nil, errors.New("knowledge graph unavailable: no store wired")
	}
	if run == nil {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	if name == "cypher" {
		return run(ctx, args)
	}
	readCtx, cancel := context.WithTimeout(ctx, s.cfg.Limits.ReadTimeout())
	defer cancel()
	result, err := run(readCtx, args)
	if graph.IsQueryTimeout(readCtx, err) {
		return nil, graph.ErrQueryTimeout
	}
	return result, err
}

func (s *Server) toolGetChunks(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		IDs           []string        `json:"ids"`
		Neighbours    int             `json:"neighbours"`
		WithProducts  json.RawMessage `json:"with_products"`
		ProductsLimit json.RawMessage `json:"products_limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	req := chunkread.Request{IDs: args.IDs, Neighbours: args.Neighbours, Domain: requestDomain(ctx)}
	if args.WithProducts != nil {
		var value *bool
		if err := json.Unmarshal(args.WithProducts, &value); err != nil || value == nil {
			return nil, argerr.New("with_products must be a boolean")
		}
		req.WithProducts = *value
	}
	limit, err := chunkread.DecodeProductsLimit(args.ProductsLimit)
	if err != nil {
		return nil, err
	}
	req.ProductsLimit = limit
	return chunkread.Read(ctx, s.store, req)
}

func decodeArgs(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return argerr.Errorf("bad arguments: %w", err)
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
		return nil, argerr.New("query is required")
	}
	kind := args.Kind
	if kind == "" {
		kind = "auto"
	}
	switch kind {
	case "entity", "chunk", "auto":
	default:
		return nil, argerr.Errorf("kind %q not in entity|chunk|auto", kind)
	}
	limit := args.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}
	hits, err := search.Query(ctx, s.store, s.emb, args.Query, kind, limit, search.InDomain(requestDomain(ctx)))
	if err != nil {
		return nil, err
	}
	out := make([]searchHitOut, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchHitOut{Kind: h.Kind, ID: h.ID, Title: h.Title, Score: h.Score, Snippet: h.Snippet, DocumentID: h.DocID, Domain: h.Domain})
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
	var match string
	switch {
	case id == "" && args.Name == "":
		return nil, argerr.New("id or name is required")
	case id == "":
		refs, err := s.store.EntityCandidatesByName(ctx, args.Name, requestDomain(ctx))
		if err != nil {
			return nil, fmt.Errorf("entity lookup: %w", err)
		}
		if len(refs) == 0 {
			return nil, fmt.Errorf("no entity matching %q: %w", args.Name, graph.ErrNotFound)
		}
		if len(refs) > 1 {
			candidates := make([]nameCandidateOut, 0, len(refs))
			for _, ref := range refs {
				candidates = append(candidates, toNameCandidateOut(ref))
			}
			return struct {
				Candidates []nameCandidateOut `json:"candidates"`
			}{candidates}, nil
		}
		id = refs[0].ID
		if refs[0].Match == graph.EntityMatchFold {
			match = refs[0].Match
		}
	}
	view, err := s.store.EntityViewOpts(ctx, id, graph.ViewOptions{Domain: requestDomain(ctx), IncludeLowConfidence: args.IncludeLowConfidence, IncludeUnverified: false})
	if err != nil {
		return nil, fmt.Errorf("entity %s: %w", id, err)
	}
	facts := make([]factOut, 0, len(view.Facts))
	for _, f := range view.Facts {
		facts = append(facts, factOut{
			Rel:           f.Rel,
			Other:         entityRefOut{ID: f.Other.ID, Key: f.Other.Key, Name: f.Other.Name, Type: f.Other.Type, Domain: f.Other.Domain},
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
	together := make([]coMentionOut, 0, len(view.Together))
	for _, other := range view.Together {
		together = append(together, coMentionOut{
			entityRefOut: entityRefOut{ID: other.ID, Key: other.Key, Name: other.Name, Type: other.Type, Domain: other.Domain},
			SharedChunks: other.SharedChunks,
		})
	}
	var hidden *graph.HiddenCounts
	if view.Hidden != (graph.HiddenCounts{}) {
		hidden = &view.Hidden
	}
	return entityOut{
		ID:           view.ID,
		Key:          view.Key,
		Name:         view.Name,
		Type:         view.Type,
		Domain:       view.Domain,
		Aliases:      aliases,
		MentionCount: view.MentionCount,
		Facts:        facts,
		Together:     together,
		Hidden:       hidden,
		Requested:    view.Requested,
		ResolvedVia:  view.ResolvedVia,
		Match:        match,
	}, nil
}

func toNameCandidateOut(candidate graph.EntityCandidate) nameCandidateOut {
	out := nameCandidateOut{entityRefOut: entityRefOut{
		ID: candidate.ID, Key: candidate.Key, Name: candidate.Name, Type: candidate.Type, Domain: candidate.Domain,
	}}
	if candidate.Match == graph.EntityMatchFold {
		out.Match = candidate.Match
	}
	return out
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
		return nil, argerr.New("entity is required")
	}
	depth := args.Depth
	if depth == 0 {
		depth = 1
	}
	if depth < 1 || depth > 2 {
		return nil, argerr.Errorf("depth must be 1 or 2, got %d", depth)
	}
	direction := args.Direction
	if direction == "" {
		direction = "out"
	}
	switch direction {
	case "out", "in", "both":
	default:
		return nil, argerr.Errorf("direction %q not in out|in|both", direction)
	}
	limit := args.Limit
	if limit < 0 {
		return nil, argerr.New("limit must not be negative")
	}
	if limit > traverseMaxLimit {
		limit = traverseMaxLimit
	}
	sub, err := s.store.TraverseOpts(ctx, args.Entity, args.Relation, direction, depth, limit, graph.ViewOptions{Domain: requestDomain(ctx), IncludeLowConfidence: args.IncludeLowConfidence, IncludeUnverified: false})
	if err != nil {
		return nil, err
	}
	nodes := make([]entityRefOut, 0, len(sub.Nodes))
	for _, n := range sub.Nodes {
		nodes = append(nodes, entityRefOut{ID: n.ID, Key: n.Key, Name: n.Name, Type: n.Type, Domain: n.Domain})
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
	var hidden *graph.HiddenCounts
	if sub.Hidden != (graph.HiddenCounts{}) {
		hidden = &sub.Hidden
	}
	return traverseOut{Nodes: nodes, Edges: edges, Hidden: hidden, Requested: sub.Requested, ResolvedVia: sub.ResolvedVia}, nil
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
		return nil, argerr.New("limit must not be negative")
	}
	if limit == 0 {
		limit = docsDefaultLimit
	}
	if limit > docsMaxLimit {
		limit = docsMaxLimit
	}
	docs, err := s.store.DocumentsIn(ctx, graph.Status(args.Status), requestDomain(ctx), limit)
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
		if d := requestDomain(ctx); d != "" && doc.Domain != d {
			return nil, graph.ErrNotFound
		}
		return documentsOut{Documents: []documentOut{toDocumentOut(*doc)}}, nil
	}
	docs, err := s.store.DocumentsIn(ctx, "", requestDomain(ctx), statusListLimit)
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
		return nil, argerr.Wrap(err)
	}
	capRows := s.cfg.Limits.CypherRows
	if capRows <= 0 {
		capRows = 1000
	}
	if n, ok := api.LimitClause(rewritten); ok && n > capRows {
		return nil, argerr.Errorf("LIMIT %d exceeds the configured cypher_rows cap %d", n, capRows)
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
		if graph.IsQueryTimeout(qctx, err) {
			return nil, graph.ErrQueryTimeout
		}
		var qerr *graph.QueryError
		if errors.As(err, &qerr) {
			return nil, argerr.Errorf("query failed: %s", qerr.Error())
		}
		return nil, err
	}
	return cypherOut{Columns: res.Columns, Rows: res.Rows}, nil
}

func toDocumentOut(d graph.Document) documentOut {
	return documentOut{
		Domain:     d.Domain,
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
