// Package mcp exposes the gkg knowledge graph over stdio and streamable HTTP.
// Clients can ingest documents, query the graph directly, or use the optional
// server-side ask agent. Judge-confirmed typed facts are visible at any confidence.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alessiosavi/go-knowledge-graph/internal/api"
	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

// Server owns one SDK server with the gkg tool set registered.
type Server struct {
	cfg     config.Config
	store   *graph.Store
	in      api.Submitter
	emb     search.QueryEmbedder
	sdk     *mcpgo.Server
	metrics *obs.Metrics
	tools   map[string]struct{}
}

// NewServer wires the tool set. store is the open graph store; in accepts
// ingest submissions (nil answers the ingest tool with an actionable error).
func NewServer(cfg config.Config, store *graph.Store, in api.Submitter) *Server {
	s := &Server{cfg: cfg, store: store, in: in, tools: make(map[string]struct{})}
	sdk := mcpgo.NewServer(&mcpgo.Implementation{
		Name:    "go-knowledge-graph",
		Version: "v0.1.0",
	}, &mcpgo.ServerOptions{
		Instructions: "Document-to-knowledge-graph service. Ingest documents, then search entities and chunks, read entities with evidenced facts, traverse relations and run guarded read-only Cypher.",
	})
	s.sdk = sdk
	registerTools(s)
	sdk.AddReceivingMiddleware(s.measureToolCalls)
	return s
}

// MCP returns the SDK server carrying the registered tools.
func (s *Server) MCP() *mcpgo.Server { return s.sdk }

// RunStdio serves the tools over stdio until the peer disconnects or ctx is
// cancelled.
func (s *Server) RunStdio(ctx context.Context) error {
	session, err := s.sdk.Connect(ctx, &mcpgo.StdioTransport{}, nil)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
	warmCtx, cancelWarm := context.WithCancel(ctx)
	defer cancelWarm()
	if s.store != nil {
		go func() {
			if err := s.store.WarmProductLinks(warmCtx); err != nil {
				slog.WarnContext(warmCtx, "warm product link index", "error", err)
			}
		}()
	}
	err = session.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// HTTPHandler returns the streamable-HTTP handler for the /mcp endpoint;
// mount it behind the REST surface's bearer policy.
func (s *Server) HTTPHandler() http.Handler {
	return mcpgo.NewStreamableHTTPHandler(func(*http.Request) *mcpgo.Server { return s.sdk }, nil)
}

// SetEmbedder attaches the optional query embedder used by the search
// tool's entity vector leg; nil (the default) keeps search text-only.
func (s *Server) SetEmbedder(emb search.QueryEmbedder) {
	s.emb = emb
}

// SetMetrics attaches the metrics registry before serving tool calls.
func (s *Server) SetMetrics(m *obs.Metrics) { s.metrics = m }

// Tool input shapes. Optional fields carry omitempty so the inferred JSON
// schema does not require them; empty values are treated as absent.
type ingestArgs struct {
	URL      string   `json:"url,omitempty" jsonschema:"URL of the document to fetch and ingest"`
	Text     string   `json:"text,omitempty" jsonschema:"inline text to ingest"`
	Source   string   `json:"source,omitempty" jsonschema:"identity override; defaults to the URL or a content hash"`
	Labels   string   `json:"labels,omitempty" jsonschema:"label preset name"`
	Force    bool     `json:"force,omitempty" jsonschema:"re-ingest even when the content is unchanged"`
	Products []string `json:"products,omitempty" jsonschema:"up to 50 unique store-prefixed product IDs; omit to retain or inherit refs, pass [] to clear"`

	DocumentDate string `json:"document_date,omitempty" jsonschema:"source date as YYYY-MM-DD or RFC 3339 with an offset"`
}

type statusArgs struct {
	Domain     string `json:"domain,omitempty" jsonschema:"optional domain slug; empty selects all"`
	DocumentID string `json:"document_id,omitempty" jsonschema:"one document id; omit to list documents"`
}

type dateWindowArgs struct {
	Since string `json:"since,omitempty" jsonschema:"inclusive lower bound on the effective document date as YYYY-MM-DD (start of the UTC day) or RFC 3339 with an offset"`
	Until string `json:"until,omitempty" jsonschema:"inclusive upper bound on the effective document date as YYYY-MM-DD (end of the UTC day) or RFC 3339 with an offset"`
}

type searchArgs struct {
	dateWindowArgs
	Domain               string `json:"domain,omitempty" jsonschema:"optional domain slug; empty selects all"`
	Query                string `json:"query" jsonschema:"full-text query over entity names and chunk text"`
	Kind                 string `json:"kind,omitempty" jsonschema:"entity | chunk | auto (default auto)"`
	Limit                int    `json:"limit,omitempty" jsonschema:"maximum hits (default 20, capped at 100)"`
	IncludeLowConfidence bool   `json:"include_low_confidence,omitempty" jsonschema:"accepted for shape parity; text hits carry no confidence"`
}

type getEntityArgs struct {
	dateWindowArgs
	Domain               string `json:"domain,omitempty" jsonschema:"optional domain slug; empty selects all"`
	ID                   string `json:"id,omitempty" jsonschema:"live or retired entity id or key"`
	Name                 string `json:"name,omitempty" jsonschema:"entity name, used when id is omitted"`
	IncludeLowConfidence bool   `json:"include_low_confidence,omitempty" jsonschema:"include all typed facts below the assert floor. Judge-confirmed facts are always shown regardless of this flag"`
	IncludeUnverified    bool   `json:"include_unverified,omitempty" jsonschema:"include marked candidates only when facts.allow_unverified_reads is enabled; otherwise unverified_ignored is true"`
	ConfirmedOnly        bool   `json:"confirmed_only,omitempty" jsonschema:"return only facts the judge confirmed. Overrides include_low_confidence and include_unverified"`
}

type getChunksArgs struct {
	Domain       string   `json:"domain,omitempty"`
	IDs          []string `json:"ids"`
	Neighbours   int      `json:"neighbours,omitempty"`
	WithProducts bool     `json:"with_products,omitempty"`
}

type traverseArgs struct {
	dateWindowArgs
	Domain               string `json:"domain,omitempty" jsonschema:"optional domain slug; empty selects all"`
	Entity               string `json:"entity" jsonschema:"start entity id or key"`
	Relation             string `json:"relation,omitempty" jsonschema:"relation filter (sanitized, e.g. works_at)"`
	Direction            string `json:"direction,omitempty" jsonschema:"out | in | both (default out)"`
	Depth                int    `json:"depth,omitempty" jsonschema:"hops, 1 or 2 (default 1)"`
	Limit                int    `json:"limit,omitempty" jsonschema:"maximum edges (default 50, capped at 200)"`
	IncludeLowConfidence bool   `json:"include_low_confidence,omitempty" jsonschema:"include all typed facts below the assert floor. Judge-confirmed facts are always shown regardless of this flag"`
	IncludeUnverified    bool   `json:"include_unverified,omitempty" jsonschema:"include marked candidates only when facts.allow_unverified_reads is enabled; otherwise unverified_ignored is true"`
	ConfirmedOnly        bool   `json:"confirmed_only,omitempty" jsonschema:"return only facts the judge confirmed. Overrides include_low_confidence and include_unverified"`
}

type listDocumentsArgs struct {
	dateWindowArgs
	Domain string `json:"domain,omitempty" jsonschema:"optional domain slug; empty selects all"`
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
	Domain     string    `json:"domain"`
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
	Products   []string  `json:"products,omitempty" jsonschema:"Canonical list of store-prefixed product IDs referenced by this document"`

	DocumentDate       *time.Time `json:"document_date,omitempty"`
	DocumentDateSource *string    `json:"document_date_source,omitempty"`
}

type statusOut struct {
	Documents []documentOut `json:"documents"`
}

type listDocumentsOut struct {
	Documents []documentOut `json:"documents"`
}

type searchHit struct {
	Domain     string  `json:"domain"`
	Kind       string  `json:"kind"`
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Score      float64 `json:"score"`
	Snippet    string  `json:"snippet"`
	DocumentID string  `json:"document_id,omitempty"`

	DocumentDate       *time.Time `json:"document_date,omitempty"`
	DocumentDateSource *string    `json:"document_date_source,omitempty"`
}

type searchOut struct {
	Hits []searchHit `json:"hits"`
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
	Judge         *string      `json:"judge,omitempty"`
	Unverified    bool         `json:"unverified,omitempty"`
	LowConfidence bool         `json:"low_confidence"`
	Outgoing      bool         `json:"outgoing"`
	DocID         string       `json:"doc_id"`
	ChunkID       string       `json:"chunk_id"`
	Excerpt       string       `json:"excerpt"`

	DocumentDate       *time.Time `json:"document_date,omitempty"`
	DocumentDateSource *string    `json:"document_date_source,omitempty"`
}

type entityOut struct {
	ID                string              `json:"id"`
	Key               string              `json:"key"`
	Name              string              `json:"name"`
	Type              string              `json:"type"`
	Domain            string              `json:"domain"`
	Aliases           []string            `json:"aliases"`
	MentionCount      int                 `json:"mention_count"`
	Facts             []factOut           `json:"facts"`
	Together          []coMentionOut      `json:"together"`
	Hidden            *graph.HiddenCounts `json:"hidden,omitempty"`
	UnverifiedIgnored bool                `json:"unverified_ignored,omitempty"`
	Requested         string              `json:"requested,omitempty"`
	ResolvedVia       string              `json:"resolved_via,omitempty"`
	Match             string              `json:"match,omitempty"`
}

type nameCandidateOut struct {
	entityRefOut
	Match string `json:"match,omitempty"`
}

type candidatesOut struct {
	Candidates []nameCandidateOut `json:"candidates"`
}

type edgeOut struct {
	From          string  `json:"from"`
	To            string  `json:"to"`
	Rel           string  `json:"rel"`
	Confidence    float64 `json:"confidence"`
	Judge         *string `json:"judge,omitempty"`
	Unverified    bool    `json:"unverified,omitempty"`
	LowConfidence bool    `json:"low_confidence"`
	DocID         string  `json:"doc_id"`
	ChunkID       string  `json:"chunk_id"`
	Excerpt       string  `json:"excerpt"`

	DocumentDate       *time.Time `json:"document_date,omitempty"`
	DocumentDateSource *string    `json:"document_date_source,omitempty"`
}

type traverseOut struct {
	Nodes             []entityRefOut      `json:"nodes"`
	Edges             []edgeOut           `json:"edges"`
	Hidden            *graph.HiddenCounts `json:"hidden,omitempty"`
	UnverifiedIgnored bool                `json:"unverified_ignored,omitempty"`
	Requested         string              `json:"requested,omitempty"`
	ResolvedVia       string              `json:"resolved_via,omitempty"`
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

func registerTools(s *Server) {
	addReadTool(s, &mcpgo.Tool{
		Name:         "get_product",
		Description:  getProductDescription,
		OutputSchema: getProductOutputSchema(),
	}, s.handleGetProduct)
	addReadTool(s, &mcpgo.Tool{
		Name:         "find_products",
		Description:  graph.ProductSearchDescription + " Pages over 256 KiB are returned only in structuredContent, with empty text content.",
		InputSchema:  findProductsInputSchema(),
		OutputSchema: findProductsOutputSchema(),
	}, s.handleFindProducts)
	addReadTool(s, &mcpgo.Tool{
		Name:        "list_features",
		Description: graph.ProductFeaturesDescription + ", with release status and numeric-search support per source key and store.",
	}, s.handleListFeatures)
	addTool(s, &mcpgo.Tool{
		Name:        "ingest",
		Description: "Ingest a document from a URL or inline text; returns the document id. Poll `status` until done or failed.",
	}, s.handleIngest)
	addReadTool(s, &mcpgo.Tool{
		Name:        "status",
		Description: "Pipeline status and error of one document, or of the most recently updated documents when no id is given.",
	}, s.handleStatus)
	addReadTool(s, &mcpgo.Tool{
		Name:        "search",
		Description: "Full-text search over entity names and chunk text, with scores and snippets; entity hits fuse embeddings when the embedder is enabled. Since/until filter chunk hits by document date. Start with limit 10.",
	}, s.handleSearch)
	addReadTool(s, &mcpgo.Tool{
		Name:        "get_entity",
		Description: "One entity with aliases, co-mentions and evidenced facts. Since/until filter fact evidence by document date. Low-confidence facts need include_low_confidence unless the judge confirmed them. A merge redirect includes requested and resolved_via=merged. Name lookup tries exact matches first, then verified folds marked match=fold. Unscoped ambiguous names return one candidate per domain.",
	}, s.handleGetEntity)
	addReadTool(s, &mcpgo.Tool{
		Name:        "get_chunks",
		Description: chunkread.ToolDescription + chunkread.ProductsToolDescription,
		InputSchema: map[string]any{
			"type": "object", "required": []string{"ids"}, "additionalProperties": false,
			"properties": map[string]any{
				"domain":         map[string]any{"type": "string", "description": "optional domain slug; empty selects all"},
				"ids":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "1–20 chunk ids from search hits"},
				"neighbours":     map[string]any{"type": "integer", "description": "chunks before and after each requested chunk (default 0, maximum 2)"},
				"with_products":  map[string]any{"type": "boolean", "default": false, "description": chunkread.WithProductsDescription},
				"products_limit": map[string]any{"type": "integer", "minimum": 1, "maximum": chunkread.MaxProductsLimit, "default": chunkread.DefaultProductsLimit, "description": chunkread.ProductsLimitDescription},
			},
		},
	}, s.handleGetChunks)
	addReadTool(s, &mcpgo.Tool{
		Name:        "traverse",
		Description: "Walk fact edges from a start entity up to two hops, with direction and relation filters and evidence excerpts. Since/until filter edges by document date. A merge redirect includes requested and resolved_via=merged. Start with limit 10.",
	}, s.handleTraverse)
	addReadTool(s, &mcpgo.Tool{
		Name:        "list_documents",
		Description: "List documents with metadata, optionally filtered by pipeline status and document date (since/until). Start with limit 10.",
	}, s.handleListDocuments)
	addTool(s, &mcpgo.Tool{
		Name:        "cypher",
		Description: "Run a guarded read-only Cypher query with the configured row cap and timeout; nodes and relationships come back as JSON objects.",
	}, s.handleCypher)
}

func addReadTool[In, Out any](s *Server, tool *mcpgo.Tool, handle mcpgo.ToolHandlerFor[In, Out]) {
	addTool(s, tool, func(ctx context.Context, req *mcpgo.CallToolRequest, args In) (*mcpgo.CallToolResult, Out, error) {
		ctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.ReadTimeout())
		defer cancel()
		result, out, err := handle(ctx, req, args)
		if graph.IsQueryTimeout(ctx, err) {
			return nil, out, graph.ErrQueryTimeout
		}
		return result, out, err
	})
}

func (s *Server) handleGetChunks(ctx context.Context, req *mcpgo.CallToolRequest, args getChunksArgs) (*mcpgo.CallToolResult, chunkread.Result, error) {
	// The SDK normalizes numbers while applying schema defaults. Decode the
	// original limit to distinguish JSON integers from values like 3.0 and 1e1.
	var raw struct {
		ProductsLimit json.RawMessage `json:"products_limit"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &raw); err != nil {
		return nil, chunkread.Result{}, argerr.Errorf("bad arguments: %w", err)
	}
	limit, err := chunkread.DecodeProductsLimit(raw.ProductsLimit)
	if err != nil {
		return nil, chunkread.Result{}, err
	}
	if s.store == nil {
		return nil, chunkread.Result{}, errNoStore
	}
	result, err := chunkread.Read(ctx, s.store, chunkread.Request{
		IDs: args.IDs, Neighbours: args.Neighbours, Domain: args.Domain,
		WithProducts: args.WithProducts, ProductsLimit: limit,
	})
	return nil, result, err
}

func (s *Server) handleIngest(ctx context.Context, _ *mcpgo.CallToolRequest, args ingestArgs) (*mcpgo.CallToolResult, ingestOut, error) {
	if s.in == nil {
		return nil, ingestOut{}, errors.New("ingest is not available: no ingestion pipeline wired")
	}
	u := strings.TrimSpace(args.URL)
	switch {
	case u != "" && args.Text != "":
		return nil, ingestOut{}, argerr.New("give either url or text, not both")
	case u == "" && args.Text == "":
		return nil, ingestOut{}, argerr.New("url or text is required")
	}
	date, err := graph.ParseDocumentDate(args.DocumentDate, time.Now)
	if err != nil {
		return nil, ingestOut{}, err
	}
	in := ingest.Input{Source: args.Source, Labels: args.Labels, Force: args.Force, Products: args.Products, DocumentDate: date}
	if u != "" {
		in.Kind, in.URL = "url", u
	} else {
		in.Kind, in.Text = "text", args.Text
	}
	res, err := s.in.Submit(ctx, in)
	if err != nil {
		var refsErr *graph.DocumentRefsError
		if errors.As(err, &refsErr) {
			return nil, ingestOut{}, argerr.Wrap(refsErr)
		}
		return nil, ingestOut{}, err
	}
	return nil, ingestOut{DocumentID: res.DocumentID, Noop: res.Noop}, nil
}

func (s *Server) handleStatus(ctx context.Context, _ *mcpgo.CallToolRequest, args statusArgs) (*mcpgo.CallToolResult, statusOut, error) {
	if s.store == nil {
		return nil, statusOut{}, errNoStore
	}
	if args.Domain != "" && !graph.ValidDomain(args.Domain) {
		return nil, statusOut{}, argerr.New("invalid domain slug")
	}
	if args.DocumentID != "" {
		doc, err := s.store.Document(ctx, args.DocumentID)
		if err != nil {
			return nil, statusOut{}, fmt.Errorf("document %s: %w", args.DocumentID, err)
		}
		if args.Domain != "" && doc.Domain != args.Domain {
			return nil, statusOut{}, graph.ErrNotFound
		}
		return nil, statusOut{Documents: []documentOut{toDocumentOut(*doc)}}, nil
	}
	docs, err := s.store.DocumentsIn(ctx, "", args.Domain, statusListLimit)
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
		return nil, searchOut{}, argerr.New("query is required")
	}
	kind := args.Kind
	if kind == "" {
		kind = "auto"
	}
	switch kind {
	case "entity", "chunk", "auto":
	default:
		return nil, searchOut{}, argerr.Errorf("kind %q not in entity|chunk|auto", kind)
	}
	limit := args.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}
	window, err := graph.ParseDateWindow(args.Since, args.Until)
	if err != nil {
		return nil, searchOut{}, err
	}
	hits, err := search.Query(ctx, s.store, s.emb, args.Query, kind, limit, search.InDomain(args.Domain), search.InDateWindow(window))
	if err != nil {
		return nil, searchOut{}, err
	}
	out := make([]searchHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchHit{
			Kind: h.Kind, ID: h.ID, Title: h.Title, Score: h.Score, Snippet: h.Snippet, DocumentID: h.DocID, Domain: h.Domain,
			DocumentDate: h.DocumentDate, DocumentDateSource: h.DocumentDateSource,
		})
	}
	return nil, searchOut{Hits: out}, nil
}

func (s *Server) handleGetEntity(ctx context.Context, _ *mcpgo.CallToolRequest, args getEntityArgs) (*mcpgo.CallToolResult, any, error) {
	if s.store == nil {
		return nil, entityOut{}, errNoStore
	}
	window, err := graph.ParseDateWindow(args.Since, args.Until)
	if err != nil {
		return nil, entityOut{}, err
	}
	id := strings.TrimSpace(args.ID)
	var match string
	switch {
	case id == "" && strings.TrimSpace(args.Name) == "":
		return nil, entityOut{}, argerr.New("id or name is required")
	case id == "":
		refs, err := s.store.EntityCandidatesByName(ctx, strings.TrimSpace(args.Name), args.Domain)
		if err != nil {
			return nil, entityOut{}, err
		}
		if len(refs) == 0 {
			return nil, entityOut{}, fmt.Errorf("no entity matching %q: %w", args.Name, graph.ErrNotFound)
		}
		if len(refs) > 1 {
			candidates := make([]nameCandidateOut, 0, len(refs))
			for _, ref := range refs {
				candidates = append(candidates, toNameCandidateOut(ref))
			}
			return nil, candidatesOut{Candidates: candidates}, nil
		}
		id = refs[0].ID
		if refs[0].Match == graph.EntityMatchFold {
			match = refs[0].Match
		}
	}
	view, err := s.store.EntityViewOpts(ctx, id, graph.ViewOptions{
		Domain: args.Domain, IncludeLowConfidence: args.IncludeLowConfidence,
		IncludeUnverified: args.IncludeUnverified && s.cfg.Facts.AllowUnverifiedReads,
		ConfirmedOnly:     args.ConfirmedOnly,
		DateWindow:        window,
	})
	if err != nil {
		return nil, entityOut{}, fmt.Errorf("entity %s: %w", id, err)
	}
	out := toEntityOut(view)
	out.Match = match
	out.UnverifiedIgnored = args.IncludeUnverified && !s.cfg.Facts.AllowUnverifiedReads
	return nil, out, nil
}

func (s *Server) handleTraverse(ctx context.Context, _ *mcpgo.CallToolRequest, args traverseArgs) (*mcpgo.CallToolResult, traverseOut, error) {
	if s.store == nil {
		return nil, traverseOut{}, errNoStore
	}
	entity := strings.TrimSpace(args.Entity)
	if entity == "" {
		return nil, traverseOut{}, argerr.New("entity is required")
	}
	depth := args.Depth
	if depth == 0 {
		depth = 1
	}
	if depth < 1 || depth > 2 {
		return nil, traverseOut{}, argerr.Errorf("depth must be 1 or 2, got %d", depth)
	}
	direction := args.Direction
	if direction == "" {
		direction = "out"
	}
	switch direction {
	case "out", "in", "both":
	default:
		return nil, traverseOut{}, argerr.Errorf("direction %q not in out|in|both", direction)
	}
	limit := args.Limit
	if limit < 0 {
		return nil, traverseOut{}, argerr.New("limit must not be negative")
	}
	if limit > traverseMaxLimit {
		limit = traverseMaxLimit
	}
	window, err := graph.ParseDateWindow(args.Since, args.Until)
	if err != nil {
		return nil, traverseOut{}, err
	}
	sub, err := s.store.TraverseOpts(ctx, entity, args.Relation, direction, depth, limit, graph.ViewOptions{
		Domain: args.Domain, IncludeLowConfidence: args.IncludeLowConfidence,
		IncludeUnverified: args.IncludeUnverified && s.cfg.Facts.AllowUnverifiedReads,
		ConfirmedOnly:     args.ConfirmedOnly,
		DateWindow:        window,
	})
	if err != nil {
		return nil, traverseOut{}, err
	}
	nodes := make([]entityRefOut, 0, len(sub.Nodes))
	for _, n := range sub.Nodes {
		nodes = append(nodes, toEntityRefOut(n))
	}
	edges := make([]edgeOut, 0, len(sub.Edges))
	for _, e := range sub.Edges {
		var judge *string
		if e.Judge != "" || e.Unverified {
			judge = &e.Judge
		}
		edges = append(edges, edgeOut{
			From:          e.From,
			To:            e.To,
			Rel:           e.Rel,
			Confidence:    e.Confidence,
			Judge:         judge,
			Unverified:    e.Unverified,
			LowConfidence: e.LowConfidence,
			DocID:         e.DocID,
			ChunkID:       e.ChunkID,
			Excerpt:       e.Excerpt,

			DocumentDate:       e.DocumentDate,
			DocumentDateSource: e.DocumentDateSource,
		})
	}
	var hidden *graph.HiddenCounts
	if sub.Hidden != (graph.HiddenCounts{}) {
		hidden = &sub.Hidden
	}
	return nil, traverseOut{Nodes: nodes, Edges: edges, Hidden: hidden,
		Requested: sub.Requested, ResolvedVia: sub.ResolvedVia,
		UnverifiedIgnored: args.IncludeUnverified && !s.cfg.Facts.AllowUnverifiedReads}, nil
}

func (s *Server) handleListDocuments(ctx context.Context, _ *mcpgo.CallToolRequest, args listDocumentsArgs) (*mcpgo.CallToolResult, listDocumentsOut, error) {
	if s.store == nil {
		return nil, listDocumentsOut{}, errNoStore
	}
	limit := args.Limit
	if limit < 0 {
		return nil, listDocumentsOut{}, argerr.New("limit must not be negative")
	}
	if limit == 0 {
		limit = docsDefaultLimit
	}
	if limit > docsMaxLimit {
		limit = docsMaxLimit
	}
	window, err := graph.ParseDateWindow(args.Since, args.Until)
	if err != nil {
		return nil, listDocumentsOut{}, err
	}
	docs, err := s.store.DocumentsInWindow(ctx, graph.Status(args.Status), args.Domain, limit, window)
	if err != nil {
		return nil, listDocumentsOut{}, fmt.Errorf("list documents: %w", err)
	}
	return nil, listDocumentsOut{Documents: toDocumentOuts(docs)}, nil
}

func (s *Server) handleCypher(ctx context.Context, _ *mcpgo.CallToolRequest, args cypherArgs) (*mcpgo.CallToolResult, cypherOut, error) {
	rewritten, err := api.CheckReadOnly(args.Query)
	if err != nil {
		return nil, cypherOut{}, argerr.Wrap(err)
	}
	capRows := s.cfg.Limits.CypherRows
	if capRows <= 0 {
		capRows = 1000
	}
	if n, ok := api.LimitClause(rewritten); ok && n > capRows {
		return nil, cypherOut{}, argerr.Errorf("LIMIT %d exceeds the configured cypher_rows cap %d", n, capRows)
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
		if graph.IsQueryTimeout(qctx, err) {
			return nil, cypherOut{}, graph.ErrQueryTimeout
		}
		var qerr *graph.QueryError
		if errors.As(err, &qerr) {
			return nil, cypherOut{}, argerr.Errorf("query failed: %s", qerr.Error())
		}
		return nil, cypherOut{}, err
	}
	return nil, cypherOut{Columns: res.Columns, Rows: res.Rows}, nil
}

func toDocumentOut(d graph.Document) documentOut {
	return documentOut{
		Domain:             d.Domain,
		ID:                 d.ID,
		SourceKind:         d.SourceKind,
		Source:             d.Source,
		FinalURL:           d.FinalURL,
		Title:              d.Title,
		Status:             string(d.Status),
		Error:              d.Error,
		CreatedAt:          d.CreatedAt,
		UpdatedAt:          d.UpdatedAt,
		Supersedes:         d.Supersedes,
		Products:           d.ProductRefs,
		DocumentDate:       d.DocumentDate,
		DocumentDateSource: d.DocumentDateSource,
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
	return entityRefOut{ID: ref.ID, Key: ref.Key, Name: ref.Name, Type: ref.Type, Domain: ref.Domain}
}

func toNameCandidateOut(candidate graph.EntityCandidate) nameCandidateOut {
	out := nameCandidateOut{entityRefOut: toEntityRefOut(candidate.EntityRef)}
	if candidate.Match == graph.EntityMatchFold {
		out.Match = candidate.Match
	}
	return out
}

func toEntityOut(view graph.EntityView) entityOut {
	aliases := view.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	facts := make([]factOut, 0, len(view.Facts))
	for _, f := range view.Facts {
		var judge *string
		if f.Judge != "" || f.Unverified {
			judge = &f.Judge
		}
		facts = append(facts, factOut{
			Rel:           f.Rel,
			Other:         toEntityRefOut(f.Other),
			Confidence:    f.Confidence,
			Judge:         judge,
			Unverified:    f.Unverified,
			LowConfidence: f.LowConfidence,
			Outgoing:      f.Outgoing,
			DocID:         f.DocID,
			ChunkID:       f.ChunkID,
			Excerpt:       f.Excerpt,

			DocumentDate:       f.DocumentDate,
			DocumentDateSource: f.DocumentDateSource,
		})
	}
	together := make([]coMentionOut, 0, len(view.Together))
	for _, other := range view.Together {
		together = append(together, coMentionOut{
			entityRefOut: toEntityRefOut(other.EntityRef),
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
	}
}
