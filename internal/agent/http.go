package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// errorBody is the JSON error shape shared with the REST surface.
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

// decodeAgentRequest handles the shared agent body limit and JSON errors.
func decodeAgentRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, responseBodyCap)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds the agent limit")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

// decodeAskQuestion decodes and validates the ask request body, writing the
// JSON error response itself when the request is unusable.
func (s *Server) decodeAskQuestion(w http.ResponseWriter, r *http.Request) (AskRequest, bool) {
	var req AskRequest
	if !decodeAgentRequest(w, r, &req) {
		return AskRequest{}, false
	}
	req, err := normalizeAskQuestion(req, s.generationCapabilities())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return AskRequest{}, false
	}
	return req, true
}

func normalizeAskQuestion(req AskRequest, caps generationCapabilities) (AskRequest, error) {
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		return AskRequest{}, errors.New("question is required")
	}
	if req.Domain != "" && !graph.ValidDomain(req.Domain) {
		return AskRequest{}, errors.New("invalid domain slug")
	}
	return req, req.Generation.validate(caps)
}

// AskHandler serves POST /v1/agent/ask: one question, one bounded
// read-only agent run, one JSON answer with its evidence. Mount it behind
// the REST bearer policy (like /mcp). While the agent is disabled the
// endpoint answers 501.
func (s *Server) AskHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Agent.Enabled {
			writeError(w, http.StatusNotImplemented, "agent_disabled", "the ask agent is not enabled on this server")
			return
		}
		req, ok := s.decodeAskQuestion(w, r)
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(withDomain(r.Context(), req.Domain), s.timeout)
		defer cancel()
		resp, err := s.runWithEvents(ctx, req, askEvents{})
		if err != nil {
			slog.Warn("agent: ask failed", "err", err)
			kind, detail := classifyStreamError(err)
			status := http.StatusBadGateway
			if kind == "agent_timeout" {
				status = http.StatusGatewayTimeout
			}
			writeError(w, status, kind, detail)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// StatusHandler serves GET /v1/agent/status: the UI's capability probe
// (enabled, answer, stream, and generation capabilities), behind the same bearer policy.
// "stream" reports whether the ask stream endpoint will run the provider
// calls streamed: the agent must be enabled, streaming on, and streaming
// not memoized unsupported for this process.
func (s *Server) StatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unsupported, _ := s.llm.streamState()
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":    s.cfg.Agent.Enabled,
			"answer":     s.answerEnabled(),
			"generation": s.generationCapabilities(),
			"stream":     s.cfg.Agent.Enabled && s.cfg.Agent.Stream && !unsupported,
		})
	})
}

// isTooLarge reports whether err is (or wraps) the body-cap error.
func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return true
	}
	return strings.Contains(err.Error(), "request body too large")
}
