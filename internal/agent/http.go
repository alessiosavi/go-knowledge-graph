package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
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

// decodeAskQuestion decodes and validates the ask request body, writing the
// JSON error response itself when the request is unusable.
func decodeAskQuestion(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, responseBodyCap)
	var req struct {
		Question string `json:"question"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if isTooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds the agent limit")
			return "", false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return "", false
	}
	question := strings.TrimSpace(req.Question)
	if question == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "question is required")
		return "", false
	}
	return question, true
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
		question, ok := decodeAskQuestion(w, r)
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
		defer cancel()
		resp, err := s.ask(ctx, question)
		if err != nil {
			slog.Warn("agent: ask failed", "err", err)
			if errors.Is(err, context.DeadlineExceeded) {
				writeError(w, http.StatusGatewayTimeout, "agent_timeout", "the agent timed out before answering")
				return
			}
			// Provider failures are pre-classified: the message never
			// carries the endpoint or the API key.
			writeError(w, http.StatusBadGateway, "agent_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// StatusHandler serves GET /v1/agent/status: the UI's capability probe
// (200 {"enabled":bool,"stream":bool}), behind the same bearer policy.
// "stream" reports whether the ask stream endpoint will run the provider
// calls streamed: the agent must be enabled, streaming on, and streaming
// not memoized unsupported for this process.
func (s *Server) StatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unsupported, _ := s.llm.streamState()
		writeJSON(w, http.StatusOK, map[string]bool{
			"enabled": s.cfg.Agent.Enabled,
			"stream":  s.cfg.Agent.Enabled && s.cfg.Agent.Stream && !unsupported,
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
