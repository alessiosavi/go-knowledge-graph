// Package mcp exposes the gkg knowledge graph to MCP clients: seven tools
// (ingest, status, search, get_entity, traverse, list_documents, cypher)
// over stdio and streamable HTTP, thin over the search and traversal read
// paths and the guarded read-only cypher surface. Low-confidence facts are
// excluded unless the caller opts in.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

// Server owns one SDK server with the gkg tool set registered.
type Server struct {
	cfg   config.Config
	store *graph.Store
	in    api.Submitter
	emb   search.Embedder
	sdk   *mcpgo.Server
}

// NewServer wires the tool set. store is the open graph store; in accepts
// ingest submissions (nil answers the ingest tool with an actionable error).
func NewServer(cfg config.Config, store *graph.Store, in api.Submitter) *Server {
	s := &Server{cfg: cfg, store: store, in: in}
	sdk := mcpgo.NewServer(&mcpgo.Implementation{
		Name:    "go-knowledge-graph",
		Version: "v0.1.0",
	}, &mcpgo.ServerOptions{
		Instructions: "Document-to-knowledge-graph service. Ingest documents, then search entities and chunks, read entities with evidenced facts, traverse relations and run guarded read-only Cypher.",
	})
	s.sdk = sdk
	registerTools(s, sdk)
	return s
}

// MCP returns the SDK server carrying the registered tools.
func (s *Server) MCP() *mcpgo.Server { return s.sdk }

// RunStdio serves the tools over stdio until the peer disconnects or ctx is
// cancelled.
func (s *Server) RunStdio(ctx context.Context) error {
	return s.sdk.Run(ctx, &mcpgo.StdioTransport{})
}

// HTTPHandler returns the streamable-HTTP handler for the /mcp endpoint;
// mount it behind the REST surface's bearer policy.
func (s *Server) HTTPHandler() http.Handler {
	return mcpgo.NewStreamableHTTPHandler(func(*http.Request) *mcpgo.Server { return s.sdk }, nil)
}

// SetEmbedder attaches the optional embeddings provider used by the search
// tool's entity vector leg; nil (the default) keeps search text-only.
func (s *Server) SetEmbedder(emb search.Embedder) {
	s.emb = emb
}

// Tool input shapes. Optional fields carry omitempty so the inferred JSON
// schema does not require them; empty values are treated as absent.
type ingestArgs struct {
	URL    string `json:"url,omitempty" jsonschema:"URL of the document to fetch and ingest"`
	Text   string `json:"text,omitempty" jsonschema:"inline text to ingest"`
	Source string `json:"source,omitempty" jsonschema:"identity override; defaults to the URL or a content hash"`
	Labels string `json:"labels,omitempty" jsonschema:"label preset name"`
	Force  bool   `json:"force,omitempty" jsonschema:"re-ingest even when the content is unchanged"`
}

type statusArgs struct {
	DocumentID string `json:"document_id,omitempty" jsonschema:"one document id; omit to list documents"`
}

type searchArgs struct {
	Query                string `json:"query" jsonschema:"full-text query over entity names and chunk text"`
	Kind                 string `json:"kind,omitempty" jsonschema:"entity | chunk | auto (default auto)"`
	Limit                int    `json:"limit,omitempty" jsonschema:"maximum hits (default 20, capped at 100)"`
	IncludeLowConfidence bool   `json:"include_low_confidence,omitempty" jsonschema:"accepted for shape parity; text hits carry no confidence"`
}

type getEntityArgs struct {
	ID                   string `json:"id,omitempty" jsonschema:"entity id"`
	Name                 string `json:"name,omitempty" jsonschema:"entity name (exact after case folding)"`
	IncludeLowConfidence bool   `json:"include_low_confidence,omitempty" jsonschema:"include facts below the assert floor"`
}

type traverseArgs struct {
	Entity               string `json:"entity" jsonschema:"start entity id or key"`
	Relation             string `json:"relation,omitempty" jsonschema:"relation filter (sanitized, e.g. works_at)"`
	Direction            string `json:"direction,omitempty" jsonschema:"out | in | both (default out)"`
	Depth                int    `json:"depth,omitempty" jsonschema:"hops, 1 or 2 (default 1)"`
	Limit                int    `json:"limit,omitempty" jsonschema:"maximum edges (default 50, capped at 200)"`
	IncludeLowConfidence bool   `json:"include_low_confidence,omitempty" jsonschema:"include facts below the assert floor"`
}

type listDocumentsArgs struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum documents (default 100, capped at 1000)"`
	Status string `json:"status,omitempty" jsonschema:"filter by pipeline status (received|fetched|chunked|extracted|merged|verifying|done|failed|superseded)"`
}

type cypherArgs struct {
	Query string `json:"query" jsonschema:"single read-only Cypher statement; write clauses, UNION and LIMIT expressions are rejected"`
	Limit int    `json:"limit,omitempty" jsonschema:"row cap; clamped to Limits.CypherRows"`
}

// Tool output shapes.
type ingestOut struct {
	DocumentID string `json:"document_id"`
	Noop       bool   `json:"noop"`
}

type documentOut struct {
	ID         string    `json:"id"`
	SourceKind string    `json:"source_kind"`
	Source     string    `json:"source"`
	FinalURL   string    `json:"final_url,omitempty"`
	Title      string    `json:"title,omitempty"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Supersedes string    `json:"supersedes,omitempty"`
}

type statusOut struct {
	Documents []documentOut `json:"documents"`
}

type listDocumentsOut struct {
	Documents []documentOut `json:"documents"`
}

type searchHit struct {
	Kind    string  `json:"kind"`
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Score   float64 `json:"score"`
	Snippet string  `json:"snippet"`
}

type searchOut struct {
	Hits []searchHit `json:"hits"`
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

// errNoStore is the shared unavailability error.
var errNoStore = errors.New("knowledge graph unavailable: no store wired")

func registerTools(s *Server, sdk *mcpgo.Server) {
	mcpgo.AddTool(sdk, &mcpgo.Tool{
		Name:        "ingest",
		Description: "Ingest a document from a URL or inline text; returns the document id. Poll `status` until done or failed.",
	}, s.handleIngest)
	mcpgo.AddTool(sdk, &mcpgo.Tool{
		Name:        "status",
		Description: "Pipeline status and error of one document, or of the most recently updated documents when no id is given.",
	}, s.handleStatus)
	mcpgo.AddTool(sdk, &mcpgo.Tool{
		Name:        "search",
		Description: "Full-text search over entity names and chunk text, with scores and snippets; entity hits fuse embeddings when the embedder is enabled.",
	}, s.handleSearch)
	mcpgo.AddTool(sdk, &mcpgo.Tool{
		Name:        "get_entity",
		Description: "One entity with aliases and evidenced facts (excerpt, confidence, judge status). Low-confidence facts need include_low_confidence.",
	}, s.handleGetEntity)
	mcpgo.AddTool(sdk, &mcpgo.Tool{
		Name:        "traverse",
		Description: "Walk fact edges from a start entity up to two hops, with direction and relation filters and evidence excerpts.",
	}, s.handleTraverse)
	mcpgo.AddTool(sdk, &mcpgo.Tool{
		Name:        "list_documents",
		Description: "List documents with metadata, optionally filtered by pipeline status.",
	}, s.handleListDocuments)
	mcpgo.AddTool(sdk, &mcpgo.Tool{
		Name:        "cypher",
		Description: "Run a guarded read-only Cypher query with the configured row cap and timeout; nodes and relationships come back as JSON objects.",
	}, s.handleCypher)
}

func (s *Server) handleIngest(ctx context.Context, _ *mcpgo.CallToolRequest, args ingestArgs) (*mcpgo.CallToolResult, ingestOut, error) {
	if s.in == nil {
		return nil, ingestOut{}, errors.New("ingest is not available: no ingestion pipeline wired")
	}
	u := strings.TrimSpace(args.URL)
	switch {
	case u != "" && args.Text != "":
		return nil, ingestOut{}, errors.New("give either url or text, not both")
	case u == "" && args.Text == "":
		return nil, ingestOut{}, errors.New("url or text is required")
	}
	in := ingest.Input{Source: args.Source, Labels: args.Labels, Force: args.Force}
	if u != "" {
		in.Kind, in.URL = "url", u
	} else {
		in.Kind, in.Text = "text", args.Text
	}
	res, err := s.in.Submit(ctx, in)
	if err != nil {
		return nil, ingestOut{}, err
	}
	return nil, ingestOut{DocumentID: res.DocumentID, Noop: res.Noop}, nil
}

func (s *Server) handleStatus(ctx context.Context, _ *mcpgo.CallToolRequest, args statusArgs) (*mcpgo.CallToolResult, statusOut, error) {
	if s.store == nil {
		return nil, statusOut{}, errNoStore
	}
	if args.DocumentID != "" {
		doc, err := s.store.Document(ctx, args.DocumentID)
		if err != nil {
			return nil, statusOut{}, fmt.Errorf("document %s: %w", args.DocumentID, err)
		}
		return nil, statusOut{Documents: []documentOut{toDocumentOut(*doc)}}, nil
	}
	docs, err := s.store.Documents(ctx, statusListLimit)
	if err != nil {
		return nil, statusOut{}, fmt.Errorf("list documents: %w", err)
	}
	return nil, statusOut{Documents: toDocumentOuts(docs)}, nil
}

func (s *Server) handleSearch(ctx context.Context, _ *mcpgo.CallToolRequest, args searchArgs) (*mcpgo.CallToolResult, searchOut, error) {
	if s.store == nil {
		return nil, searchOut{}, errNoStore
	}
	if strings.TrimSpace(args.Query) == "" {
		return nil, searchOut{}, errors.New("query is required")
	}
	kind := args.Kind
	if kind == "" {
		kind = "auto"
	}
	switch kind {
	case "entity", "chunk", "auto":
	default:
		return nil, searchOut{}, fmt.Errorf("kind %q not in entity|chunk|auto", kind)
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
		return nil, searchOut{}, err
	}
	out := make([]searchHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchHit{Kind: h.Kind, ID: h.ID, Title: h.Title, Score: h.Score, Snippet: h.Snippet})
	}
	return nil, searchOut{Hits: out}, nil
}

func (s *Server) handleGetEntity(ctx context.Context, _ *mcpgo.CallToolRequest, args getEntityArgs) (*mcpgo.CallToolResult, entityOut, error) {
	if s.store == nil {
		return nil, entityOut{}, errNoStore
	}
	id := strings.TrimSpace(args.ID)
	switch {
	case id == "" && strings.TrimSpace(args.Name) == "":
		return nil, entityOut{}, errors.New("id or name is required")
	case id == "":
		resolved, err := s.entityByName(ctx, strings.TrimSpace(args.Name))
		if err != nil {
			return nil, entityOut{}, err
		}
		id = resolved
	}
	view, err := s.store.EntityView(ctx, id, args.IncludeLowConfidence)
	if err != nil {
		return nil, entityOut{}, fmt.Errorf("entity %s: %w", id, err)
	}
	return nil, toEntityOut(view), nil
}

// entityByName resolves an exact entity name (trimmed, case-folded) through
// the store; a miss is an actionable error, never a silent empty view.
func (s *Server) entityByName(ctx context.Context, name string) (string, error) {
	ref, err := s.store.EntityByName(ctx, name)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			return "", fmt.Errorf("no entity matching %q", name)
		}
		return "", fmt.Errorf("entity lookup: %w", err)
	}
	return ref.ID, nil
}

func (s *Server) handleTraverse(ctx context.Context, _ *mcpgo.CallToolRequest, args traverseArgs) (*mcpgo.CallToolResult, traverseOut, error) {
	if s.store == nil {
		return nil, traverseOut{}, errNoStore
	}
	entity := strings.TrimSpace(args.Entity)
	if entity == "" {
		return nil, traverseOut{}, errors.New("entity is required")
	}
	depth := args.Depth
	if depth == 0 {
		depth = 1
	}
	if depth < 1 || depth > 2 {
		return nil, traverseOut{}, fmt.Errorf("depth must be 1 or 2, got %d", depth)
	}
	direction := args.Direction
	if direction == "" {
		direction = "out"
	}
	switch direction {
	case "out", "in", "both":
	default:
		return nil, traverseOut{}, fmt.Errorf("direction %q not in out|in|both", direction)
	}
	limit := args.Limit
	if limit < 0 {
		return nil, traverseOut{}, errors.New("limit must not be negative")
	}
	if limit > traverseMaxLimit {
		limit = traverseMaxLimit
	}
	sub, err := s.store.Traverse(ctx, entity, args.Relation, direction, depth, limit, args.IncludeLowConfidence)
	if err != nil {
		return nil, traverseOut{}, err
	}
	nodes := make([]entityRefOut, 0, len(sub.Nodes))
	for _, n := range sub.Nodes {
		nodes = append(nodes, toEntityRefOut(n))
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
	return nil, traverseOut{Nodes: nodes, Edges: edges}, nil
}

func (s *Server) handleListDocuments(ctx context.Context, _ *mcpgo.CallToolRequest, args listDocumentsArgs) (*mcpgo.CallToolResult, listDocumentsOut, error) {
	if s.store == nil {
		return nil, listDocumentsOut{}, errNoStore
	}
	limit := args.Limit
	if limit < 0 {
		return nil, listDocumentsOut{}, errors.New("limit must not be negative")
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
		return nil, listDocumentsOut{}, fmt.Errorf("list documents: %w", err)
	}
	return nil, listDocumentsOut{Documents: toDocumentOuts(docs)}, nil
}

func (s *Server) handleCypher(ctx context.Context, _ *mcpgo.CallToolRequest, args cypherArgs) (*mcpgo.CallToolResult, cypherOut, error) {
	rewritten, err := api.CheckReadOnly(args.Query)
	if err != nil {
		return nil, cypherOut{}, err
	}
	capRows := s.cfg.Limits.CypherRows
	if capRows <= 0 {
		capRows = 1000
	}
	if n, ok := api.LimitClause(rewritten); ok && n > capRows {
		return nil, cypherOut{}, fmt.Errorf("LIMIT %d exceeds the configured cypher_rows cap %d", n, capRows)
	}
	if s.store == nil {
		return nil, cypherOut{}, errNoStore
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
			return nil, cypherOut{}, errors.New("query timed out")
		}
		var qerr *graph.QueryError
		if errors.As(err, &qerr) {
			return nil, cypherOut{}, fmt.Errorf("query failed: %s", qerr.Error())
		}
		return nil, cypherOut{}, err
	}
	return nil, cypherOut{Columns: res.Columns, Rows: res.Rows}, nil
}

func toDocumentOut(d graph.Document) documentOut {
	return documentOut{
		ID:         d.ID,
		SourceKind: d.SourceKind,
		Source:     d.Source,
		FinalURL:   d.FinalURL,
		Title:      d.Title,
		Status:     string(d.Status),
		Error:      d.Error,
		CreatedAt:  d.CreatedAt,
		UpdatedAt:  d.UpdatedAt,
		Supersedes: d.Supersedes,
	}
}

func toDocumentOuts(docs []graph.Document) []documentOut {
	out := make([]documentOut, 0, len(docs))
	for _, d := range docs {
		out = append(out, toDocumentOut(d))
	}
	return out
}

func toEntityRefOut(ref graph.EntityRef) entityRefOut {
	return entityRefOut{ID: ref.ID, Key: ref.Key, Name: ref.Name, Type: ref.Type}
}

func toEntityOut(view graph.EntityView) entityOut {
	aliases := view.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	facts := make([]factOut, 0, len(view.Facts))
	for _, f := range view.Facts {
		facts = append(facts, factOut{
			Rel:           f.Rel,
			Other:         toEntityRefOut(f.Other),
			Confidence:    f.Confidence,
			Judge:         f.Judge,
			LowConfidence: f.LowConfidence,
			Outgoing:      f.Outgoing,
			DocID:         f.DocID,
			ChunkID:       f.ChunkID,
			Excerpt:       f.Excerpt,
		})
	}
	return entityOut{
		ID:           view.ID,
		Key:          view.Key,
		Name:         view.Name,
		Type:         view.Type,
		Aliases:      aliases,
		MentionCount: view.MentionCount,
		Facts:        facts,
	}
}
