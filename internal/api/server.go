// Package api implements the gkg HTTP surface: ingestion, documents,
// search, entities, traversal, the guarded read-only cypher endpoint, the
// embedded web UI (/ui/), the health probes (/healthz, /readyz) and the
// Prometheus scrape (/metrics).
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
	"github.com/alessiosavi/go-knowledge-graph/internal/webui"
)

// Submitter accepts ingestion submissions; *ingest.Pipeline implements it.
type Submitter interface {
	Submit(ctx context.Context, in ingest.Input) (ingest.Result, error)
}

// jsonBodyCap bounds the JSON endpoints; ingestion submissions have their
// own config-driven cap (Ingest.MaxUploadMB).
const jsonBodyCap = 1 << 20

// Server serves the /v1 REST surface over a graph store and an ingestion
// submission sink.
type Server struct {
	cfg     config.Config
	store   *graph.Store
	in      Submitter
	emb     search.QueryEmbedder
	extra   []mount
	metrics *obs.Metrics
	ready   func(context.Context) error
}

// mount is an extra handler registered under a pattern.
type mount struct {
	pattern string
	handler http.Handler
}

// NewServer wires the REST surface. store is the open graph store; in
// accepts ingestion submissions (nil answers 501 on POST /v1/ingest).
func NewServer(cfg config.Config, store *graph.Store, in Submitter) *Server {
	return &Server{cfg: cfg, store: store, in: in}
}

// Mount registers an extra handler under pattern (e.g. the MCP streamable
// endpoint); the bearer policy applies to it like any other non-exempt
// path. Call it before Handler().
func (s *Server) Mount(pattern string, h http.Handler) {
	s.extra = append(s.extra, mount{pattern: pattern, handler: h})
}

// SetEmbedder attaches the optional query embedder used by the search
// endpoint's entity vector leg; nil (the default) keeps search text-only.
func (s *Server) SetEmbedder(emb search.QueryEmbedder) {
	s.emb = emb
}

// SetMetrics attaches the Prometheus registry served at /metrics.
func (s *Server) SetMetrics(m *obs.Metrics) {
	s.metrics = m
}

// SetReady attaches the readiness probe behind /readyz (typically the graph
// store's Ping); without one, /readyz answers 503 (fail-closed).
func (s *Server) SetReady(fn func(context.Context) error) {
	s.ready = fn
}

// Handler returns the routed surface with bearer authentication applied
// and one request log record per call.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics.Handler())
	}
	mux.HandleFunc("POST /v1/ingest", s.handleIngest)
	mux.HandleFunc("GET /v1/documents", s.handleDocuments)
	mux.HandleFunc("GET /v1/documents/{id}", s.handleDocument)
	mux.HandleFunc("GET /v1/documents/{id}/status", s.handleStatus)
	mux.HandleFunc("POST /v1/search", s.handleSearch)
	mux.HandleFunc("GET /v1/entities/{id}", s.handleEntity)
	mux.HandleFunc("POST /v1/traverse", s.handleTraverse)
	mux.HandleFunc("POST /v1/cypher", s.handleCypher)
	mux.Handle("GET /ui", http.RedirectHandler("/ui/", http.StatusMovedPermanently))
	mux.Handle("GET /ui/", http.StripPrefix("/ui/", webui.Handler()))
	for _, m := range s.extra {
		mux.Handle(m.pattern, m.handler)
	}
	return obs.RequestLogger(nil)(s.withAuth(mux))
}

// handleHealthz is the process liveness probe.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz probes the wired dependency (Memgraph connectivity);
// fail-closed without one.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.ready == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "no readiness probe wired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.ready(ctx); err != nil {
		// The probe is auth-exempt: log the full error, answer generically.
		slog.Warn("readyz probe failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "dependency unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// authExempt lists the paths that never require the bearer token: the
// health probes, the metrics scrape, and the embedded UI shell. The shell
// must load in a browser without the key — a top-level navigation cannot
// attach an Authorization header — so the page can ask for the key it then
// sends on its fetches. Everything with data behind it (/v1/*, /mcp) stays
// guarded. Keep the shell list in sync with internal/webui/assets (pinned
// by TestAssetsFileSet and TestUIBearerPolicy).
func authExempt(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/metrics":
		return true
	case "/ui", "/ui/", "/ui/index.html", "/ui/app.css", "/ui/app.js":
		return true
	}
	return false
}

// withAuth enforces the static bearer token when Server.APIKey is set.
func (s *Server) withAuth(next http.Handler) http.Handler {
	key := s.cfg.Server.APIKey
	if key == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(token), []byte(key)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// errorBody is the JSON error shape shared by every endpoint.
type errorBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, kind, detail string) {
	writeJSON(w, code, errorBody{Error: kind, Detail: detail})
}

// writeStoreError maps a store error onto a status and writes the body.
func writeStoreError(w http.ResponseWriter, err error, what string) {
	writeError(w, storeErrStatus(err), "store_error", fmt.Sprintf("%s: %v", what, err))
}

// storeErrStatus maps store errors onto HTTP statuses.
func storeErrStatus(err error) int {
	switch {
	case errors.Is(err, graph.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// storeOr503 writes the availability error when no store is wired and
// reports whether the handler may proceed.
func (s *Server) storeOr503(w http.ResponseWriter) bool {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "graph store not wired")
		return false
	}
	return true
}

// readJSON decodes a JSON request body bounded by maxBytes, writing the
// error response itself. An oversized body is a 413; a malformed one a 400.
func readJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("request body exceeds %d bytes", maxBytes))
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

// isTooLarge reports whether err is (or wraps) the body-cap error from
// http.MaxBytesReader; multipart parsing wraps it in prose of its own.
func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return true
	}
	return strings.Contains(err.Error(), "request body too large")
}
