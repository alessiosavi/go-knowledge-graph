package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

// handleAdminRebuild is an operator route, excluded from the edge allowlist.
func (s *Server) handleAdminRebuild(w http.ResponseWriter, r *http.Request) {
	rebuilder, ok := s.in.(Rebuilder)
	if !ok {
		writeError(w, http.StatusNotImplemented, "rebuild_disabled", "no rebuild pipeline wired")
		return
	}
	var req struct {
		Labels     string `json:"labels"`
		DocumentID string `json:"document_id"`
	}
	if !readJSON(w, r, jsonBodyCap, &req) {
		return
	}
	res, err := rebuilder.Rebuild(r.Context(), req.DocumentID, req.Labels)
	if err != nil {
		switch {
		case errors.Is(err, graph.ErrNotDone):
			writeError(w, http.StatusConflict, "not_done", err.Error())
		case errors.Is(err, graph.ErrNotFound), errors.Is(err, ingest.ErrStore):
			writeStoreError(w, err, "rebuild")
		default:
			writeError(w, http.StatusBadRequest, "invalid_labels", err.Error())
		}
		return
	}
	if res.Busy == nil {
		res.Busy = []string{}
	}
	writeJSON(w, http.StatusAccepted, struct {
		Queued int      `json:"queued"`
		Busy   []string `json:"busy"`
	}{Queued: len(res.Queued), Busy: res.Busy})
}

// ingestRequest is the JSON submission shape.
type ingestRequest struct {
	URL      string          `json:"url"`
	Text     string          `json:"text"`
	Source   string          `json:"source"`
	Labels   string          `json:"labels"`
	Force    bool            `json:"force"`
	Products json.RawMessage `json:"products"`
}

type ingestResponse struct {
	DocumentID string `json:"document_id"`
	Noop       bool   `json:"noop"`
}

type documentDTO struct {
	Domain      string    `json:"domain"`
	ID          string    `json:"id"`
	SourceKind  string    `json:"source_kind"`
	Source      string    `json:"source"`
	FinalURL    string    `json:"final_url,omitempty"`
	Title       string    `json:"title,omitempty"`
	TitleSource string    `json:"title_source"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	Labels      string    `json:"labels,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Supersedes  string    `json:"supersedes,omitempty"`
}

type documentsResponse struct {
	Documents []documentDTO `json:"documents"`
}

type statusResponse struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// deleteResponse reports how many document versions a DELETE removed.
type deleteResponse struct {
	Deleted int `json:"deleted"`
}

type hitDTO struct {
	Domain     string  `json:"domain"`
	Kind       string  `json:"kind"`
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Score      float64 `json:"score"`
	Snippet    string  `json:"snippet"`
	DocumentID string  `json:"document_id,omitempty"`
}

type searchResponse struct {
	Hits []hitDTO `json:"hits"`
}

type entityRefDTO struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Domain string `json:"domain"`
}

type coMentionDTO struct {
	entityRefDTO
	SharedChunks int `json:"shared_chunks"`
}

type factDTO struct {
	Rel           string       `json:"rel"`
	Other         entityRefDTO `json:"other"`
	Confidence    float64      `json:"confidence"`
	Judge         *string      `json:"judge,omitempty"`
	Unverified    bool         `json:"unverified,omitempty"`
	LowConfidence bool         `json:"low_confidence"`
	Outgoing      bool         `json:"outgoing"`
	DocID         string       `json:"doc_id"`
	ChunkID       string       `json:"chunk_id"`
	Excerpt       string       `json:"excerpt"`
}

type entityDTO struct {
	ID                string              `json:"id"`
	Key               string              `json:"key"`
	Name              string              `json:"name"`
	Type              string              `json:"type"`
	Domain            string              `json:"domain"`
	Aliases           []string            `json:"aliases"`
	MentionCount      int                 `json:"mention_count"`
	Facts             []factDTO           `json:"facts"`
	Together          []coMentionDTO      `json:"together"`
	Hidden            *graph.HiddenCounts `json:"hidden,omitempty"`
	UnverifiedIgnored bool                `json:"unverified_ignored,omitempty"`
	Requested         string              `json:"requested,omitempty"`
	ResolvedVia       string              `json:"resolved_via,omitempty"`
}

type edgeDTO struct {
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
}

type traverseResponse struct {
	Nodes             []entityRefDTO      `json:"nodes"`
	Edges             []edgeDTO           `json:"edges"`
	Hidden            *graph.HiddenCounts `json:"hidden,omitempty"`
	UnverifiedIgnored bool                `json:"unverified_ignored,omitempty"`
	Requested         string              `json:"requested,omitempty"`
	ResolvedVia       string              `json:"resolved_via,omitempty"`
}

type cypherResponse struct {
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
}

// handleIngest accepts a JSON or multipart submission and queues it for processing.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if s.in == nil {
		writeError(w, http.StatusNotImplemented, "ingest_disabled", "no ingestion pipeline wired")
		return
	}
	maxBytes := int64(s.cfg.Ingest.MaxUploadMB) << 20
	if maxBytes <= 0 {
		maxBytes = 20 << 20
	}
	in, ok := s.decodeIngest(w, r, maxBytes)
	if !ok {
		return
	}
	res, err := s.in.Submit(r.Context(), in)
	if err != nil {
		var productsErr *graph.DocumentRefsError
		if errors.As(err, &productsErr) {
			status := http.StatusConflict
			if productsErr.Code == "invalid_products" || productsErr.Code == "products_unknown_store" {
				status = http.StatusBadRequest
			}
			writeError(w, status, productsErr.Code, productsErr.Detail)
			return
		}

		if errors.Is(err, ingest.ErrBusy) {
			w.Header().Set("Retry-After", "30")
			writeError(w, http.StatusServiceUnavailable, "document_busy", "document is being processed; retry later")
			return
		}
		if errors.Is(err, ingest.ErrSpoolFull) {
			w.Header().Set("Retry-After", "30")
			writeError(w, http.StatusServiceUnavailable, "ingest_busy", "ingest spool is full; retry later")
			return
		}
		if errors.Is(err, ingest.ErrSpool) {
			writeError(w, http.StatusInternalServerError, "ingest_unavailable", "could not persist submission")
			return
		}
		if errors.Is(err, ingest.ErrStore) {
			writeStoreError(w, err, "ingest")
			return
		}
		writeError(w, http.StatusBadRequest, "ingest_failed", err.Error())
		return
	}
	code := http.StatusAccepted
	if res.Noop {
		code = http.StatusOK
	}
	writeJSON(w, code, ingestResponse{DocumentID: res.DocumentID, Noop: res.Noop})
}

// decodeIngest turns the request into an ingest.Input, writing the error
// response itself when the request is unusable.
func (s *Server) decodeIngest(w http.ResponseWriter, r *http.Request, maxBytes int64) (ingest.Input, bool) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		if err := r.ParseMultipartForm(maxBytes); err != nil {
			if isTooLarge(err) {
				writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("request body exceeds %d bytes", maxBytes))
				return ingest.Input{}, false
			}
			writeError(w, http.StatusBadRequest, "invalid_multipart", err.Error())
			return ingest.Input{}, false
		}
		products := r.MultipartForm.Value["products"]
		clear := r.MultipartForm.Value["products_clear"]
		if len(clear) > 1 || (len(clear) == 1 && clear[0] != "true" && clear[0] != "false") {
			writeError(w, http.StatusBadRequest, "invalid_products", "products_clear must be a single true or false value")
			return ingest.Input{}, false
		}
		if len(clear) == 1 && clear[0] == "true" {
			if len(products) > 0 {
				writeError(w, http.StatusBadRequest, "invalid_products", "products and products_clear cannot be used together")
				return ingest.Input{}, false
			}
			products = []string{}
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", `multipart field "file" is required`)
			return ingest.Input{}, false
		}
		defer func() { _ = file.Close() }()
		data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "read upload: "+err.Error())
			return ingest.Input{}, false
		}
		if int64(len(data)) > maxBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("upload exceeds %d bytes", maxBytes))
			return ingest.Input{}, false
		}
		source := r.FormValue("source")
		if source == "" {
			source = filepath.Base(hdr.Filename)
			if source == "" || source == "." || source == "/" {
				source = "upload"
			}
		}
		return ingest.Input{
			Kind:     "file",
			Bytes:    data,
			Source:   source,
			Labels:   r.FormValue("labels"),
			Force:    formBool(r.FormValue("force")),
			Products: products,
		}, true
	}

	var req ingestRequest
	if !readJSON(w, r, maxBytes, &req) {
		return ingest.Input{}, false
	}
	var products []string
	if req.Products != nil {
		if err := json.Unmarshal(req.Products, &products); err != nil || products == nil {
			writeError(w, http.StatusBadRequest, "invalid_products", "products must be an array of product IDs")
			return ingest.Input{}, false
		}
	}
	u := strings.TrimSpace(req.URL)
	switch {
	case u != "" && req.Text != "":
		writeError(w, http.StatusBadRequest, "invalid_request", "give either url or text, not both")
		return ingest.Input{}, false
	case u == "" && req.Text == "":
		writeError(w, http.StatusBadRequest, "invalid_request", "url or text is required")
		return ingest.Input{}, false
	case u != "":
		return ingest.Input{Kind: "url", URL: u, Source: req.Source, Labels: req.Labels, Force: req.Force, Products: products}, true
	default:
		return ingest.Input{Kind: "text", Text: req.Text, Source: req.Source, Labels: req.Labels, Force: req.Force, Products: products}, true
	}
}

func formBool(s string) bool { return s == "true" || s == "1" }

// handleDocuments lists documents by status, most recently updated first;
// the list defaults to 100 rows and is capped at 1000.
func (s *Server) handleDocuments(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be a positive integer")
			return
		}
		limit = n
		if limit > 1000 {
			limit = 1000
		}
	}
	status := r.URL.Query().Get("status")
	slug := r.URL.Query().Get("domain")
	if slug != "" && !graph.ValidDomain(slug) {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid domain slug")
		return
	}
	docs, err := s.store.DocumentsIn(r.Context(), graph.Status(status), slug, limit)
	if err != nil {
		writeReadError(w, r.Context(), err, "documents")
		return
	}
	out := make([]documentDTO, 0, len(docs))
	for _, d := range docs {
		out = append(out, toDocumentDTO(d))
	}
	writeJSON(w, http.StatusOK, documentsResponse{Documents: out})
}

// handleDocument returns one document's metadata.
func (s *Server) handleDocument(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	doc, err := s.store.Document(r.Context(), r.PathValue("id"))
	if err != nil {
		writeReadError(w, r.Context(), err, "document")
		return
	}
	writeJSON(w, http.StatusOK, toDocumentDTO(*doc))
}

// handleStatus returns one document's pipeline status and error.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	doc, err := s.store.Document(r.Context(), r.PathValue("id"))
	if err != nil {
		writeReadError(w, r.Context(), err, "document")
		return
	}
	writeJSON(w, http.StatusOK, statusResponse{
		ID: doc.ID, Status: string(doc.Status), Error: doc.Error, UpdatedAt: doc.UpdatedAt,
	})
}

// handleDocumentDelete removes one document version, or with ?chain=1 the
// whole version chain of its source identity, in one transaction. The
// response reports how many versions were deleted.
func (s *Server) handleDocumentDelete(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	id := r.PathValue("id")
	if formBool(r.URL.Query().Get("chain")) {
		n, err := s.store.DeleteChain(r.Context(), id)
		if err != nil {
			writeStoreError(w, err, "delete chain")
			return
		}
		writeJSON(w, http.StatusOK, deleteResponse{Deleted: n})
		return
	}
	if err := s.store.DeleteDocument(r.Context(), id); err != nil {
		writeStoreError(w, err, "delete document")
		return
	}
	writeJSON(w, http.StatusOK, deleteResponse{Deleted: 1})
}

// handleSearch runs a text search over entities, chunks or both.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	var req struct {
		Domain string `json:"domain"`
		Query  string `json:"query"`
		Kind   string `json:"kind"`
		Limit  int    `json:"limit"`
	}
	if !readJSON(w, r, jsonBodyCap, &req) {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "query is required")
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = "auto"
	}
	switch kind {
	case "entity", "chunk", "auto":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("kind %q not in entity|chunk|auto", kind))
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if req.Domain != "" && !graph.ValidDomain(req.Domain) {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid domain slug")
		return
	}
	hits, err := search.Query(r.Context(), s.store, s.emb, req.Query, kind, limit, search.InDomain(req.Domain))
	if err != nil {
		writeReadError(w, r.Context(), err, "search")
		return
	}
	writeJSON(w, http.StatusOK, searchResponse{Hits: toHitDTOs(hits)})
}

// handleEntity returns one entity with its aliases and evidenced facts.
// Judge-confirmed typed facts are visible at any confidence. Candidates need
// include_unverified=true and the server's AllowUnverifiedReads setting.
func (s *Server) handleEntity(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	includeUnverified := queryBool(r, "include_unverified")
	view, err := s.store.EntityViewOpts(r.Context(), r.PathValue("id"), graph.ViewOptions{
		IncludeLowConfidence: queryBool(r, "include_low_confidence"),
		IncludeUnverified:    includeUnverified && s.cfg.Facts.AllowUnverifiedReads,
		ConfirmedOnly:        queryBool(r, "confirmed_only"),
	})
	if err != nil {
		writeReadError(w, r.Context(), err, "entity")
		return
	}
	out := toEntityDTO(view)
	out.UnverifiedIgnored = includeUnverified && !s.cfg.Facts.AllowUnverifiedReads
	writeJSON(w, http.StatusOK, out)
}

// handleTraverse walks fact edges from a start entity.
func (s *Server) handleTraverse(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	var req struct {
		Entity               string `json:"entity"`
		Relation             string `json:"relation"`
		Direction            string `json:"direction"`
		Depth                int    `json:"depth"`
		Limit                int    `json:"limit"`
		IncludeLowConfidence bool   `json:"include_low_confidence"`
		IncludeUnverified    bool   `json:"include_unverified"`
		ConfirmedOnly        bool   `json:"confirmed_only"`
	}
	if !readJSON(w, r, jsonBodyCap, &req) {
		return
	}
	if strings.TrimSpace(req.Entity) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "entity is required")
		return
	}
	depth := req.Depth
	if depth == 0 {
		depth = 1
	}
	if depth < 1 || depth > 2 {
		writeError(w, http.StatusBadRequest, "invalid_request", "depth must be 1 or 2")
		return
	}
	direction := req.Direction
	if direction == "" {
		direction = "out"
	}
	switch direction {
	case "out", "in", "both":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("direction %q not in out|in|both", direction))
		return
	}
	limit := req.Limit
	if limit < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must not be negative")
		return
	}
	if limit > 200 {
		limit = 200
	}
	sub, err := s.store.TraverseOpts(r.Context(), req.Entity, req.Relation, direction, depth, limit, graph.ViewOptions{
		IncludeLowConfidence: req.IncludeLowConfidence,
		IncludeUnverified:    req.IncludeUnverified && s.cfg.Facts.AllowUnverifiedReads,
		ConfirmedOnly:        req.ConfirmedOnly,
	})
	if err != nil {
		writeReadError(w, r.Context(), err, "traverse")
		return
	}
	out := toTraverseResponse(sub)
	out.UnverifiedIgnored = req.IncludeUnverified && !s.cfg.Facts.AllowUnverifiedReads
	writeJSON(w, http.StatusOK, out)
}

// handleCypher runs a guarded read-only query with the configured row cap
// and timeout. The guard answers before the store is touched.
func (s *Server) handleCypher(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if !readJSON(w, r, jsonBodyCap, &req) {
		return
	}
	rewritten, err := CheckReadOnly(req.Query)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	capRows := s.cfg.Limits.CypherRows
	if capRows <= 0 {
		capRows = 1000
	}
	// The query's own LIMIT is judged against the configured cap: the
	// request limit only tightens the injected parameter for queries that
	// carry no LIMIT of their own.
	if n, ok := LimitClause(rewritten); ok && n > capRows {
		writeError(w, http.StatusBadRequest, "invalid_query",
			fmt.Sprintf("LIMIT %d exceeds the configured cypher_rows cap %d", n, capRows))
		return
	}
	rows := capRows
	if req.Limit > 0 && req.Limit < rows {
		rows = req.Limit
	}
	if !s.storeOr503(w) {
		return
	}
	timeout := time.Duration(s.cfg.Limits.CypherTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	res, err := s.store.RunReadQuery(ctx, rewritten, nil, rows)
	if err != nil {
		if writeQueryTimeout(w, ctx, err) {
			return
		}
		var qerr *graph.QueryError
		if errors.As(err, &qerr) {
			writeError(w, http.StatusBadRequest, "query_failed", qerr.Error())
			return
		}
		writeStoreError(w, err, "cypher")
		return
	}
	writeJSON(w, http.StatusOK, cypherResponse{Columns: res.Columns, Rows: res.Rows})
}

func queryBool(r *http.Request, name string) bool {
	v := r.URL.Query().Get(name)
	return v == "true" || v == "1"
}

func toDocumentDTO(d graph.Document) documentDTO {
	return documentDTO{
		Domain:      d.Domain,
		ID:          d.ID,
		SourceKind:  d.SourceKind,
		Source:      d.Source,
		FinalURL:    d.FinalURL,
		Title:       d.Title,
		TitleSource: d.TitleSource,
		Status:      string(d.Status),
		Error:       d.Error,
		Labels:      d.Labels,
		CreatedAt:   d.CreatedAt,
		UpdatedAt:   d.UpdatedAt,
		Supersedes:  d.Supersedes,
	}
}

func toHitDTOs(hits []search.Hit) []hitDTO {
	out := make([]hitDTO, 0, len(hits))
	for _, h := range hits {
		out = append(out, hitDTO{Kind: h.Kind, ID: h.ID, Title: h.Title, Score: h.Score, Snippet: h.Snippet, DocumentID: h.DocID, Domain: h.Domain})
	}
	return out
}

func toEntityDTO(v graph.EntityView) entityDTO {
	aliases := v.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	facts := make([]factDTO, 0, len(v.Facts))
	for _, f := range v.Facts {
		var judge *string
		if f.Judge != "" || f.Unverified {
			judge = &f.Judge
		}
		facts = append(facts, factDTO{
			Rel:           f.Rel,
			Other:         entityRefDTO{ID: f.Other.ID, Key: f.Other.Key, Name: f.Other.Name, Type: f.Other.Type, Domain: f.Other.Domain},
			Confidence:    f.Confidence,
			Judge:         judge,
			Unverified:    f.Unverified,
			LowConfidence: f.LowConfidence,
			Outgoing:      f.Outgoing,
			DocID:         f.DocID,
			ChunkID:       f.ChunkID,
			Excerpt:       f.Excerpt,
		})
	}
	together := make([]coMentionDTO, 0, len(v.Together))
	for _, other := range v.Together {
		together = append(together, coMentionDTO{
			entityRefDTO: entityRefDTO{ID: other.ID, Key: other.Key, Name: other.Name, Type: other.Type, Domain: other.Domain},
			SharedChunks: other.SharedChunks,
		})
	}
	var hidden *graph.HiddenCounts
	if v.Hidden != (graph.HiddenCounts{}) {
		hidden = &v.Hidden
	}
	return entityDTO{
		ID:           v.ID,
		Key:          v.Key,
		Name:         v.Name,
		Type:         v.Type,
		Domain:       v.Domain,
		Aliases:      aliases,
		MentionCount: v.MentionCount,
		Facts:        facts,
		Together:     together,
		Hidden:       hidden,
		Requested:    v.Requested,
		ResolvedVia:  v.ResolvedVia,
	}
}

func toTraverseResponse(sub graph.Subgraph) traverseResponse {
	nodes := make([]entityRefDTO, 0, len(sub.Nodes))
	for _, n := range sub.Nodes {
		nodes = append(nodes, entityRefDTO{ID: n.ID, Key: n.Key, Name: n.Name, Type: n.Type, Domain: n.Domain})
	}
	edges := make([]edgeDTO, 0, len(sub.Edges))
	for _, e := range sub.Edges {
		var judge *string
		if e.Judge != "" || e.Unverified {
			judge = &e.Judge
		}
		edges = append(edges, edgeDTO{
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
		})
	}
	var hidden *graph.HiddenCounts
	if sub.Hidden != (graph.HiddenCounts{}) {
		hidden = &sub.Hidden
	}
	return traverseResponse{Nodes: nodes, Edges: edges, Hidden: hidden, Requested: sub.Requested, ResolvedVia: sub.ResolvedVia}
}

func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	rows, err := s.store.ListDomains(r.Context())
	if err != nil {
		writeReadError(w, r.Context(), err, "domains")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}
