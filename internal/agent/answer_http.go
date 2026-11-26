package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

type answerHTTPRequest struct {
	Question   string              `json:"question"`
	Domain     string              `json:"domain"`
	Context    *answerHTTPContext  `json:"context"`
	Generation *GenerationSettings `json:"generation"`
}

type answerHTTPContext struct {
	ProductIDs []string          `json:"product_ids"`
	History    answerHTTPHistory `json:"history"`
}

type answerHTTPHistory []struct {
	Role string  `json:"role"`
	Text *string `json:"text"` // An explicitly empty string is a valid turn.
}

func (h *answerHTTPHistory) UnmarshalJSON(data []byte) error {
	// A repeated history field must decode without reusing earlier turns.
	type wireHistory answerHTTPHistory
	var turns wireHistory
	if err := json.Unmarshal(data, &turns); err != nil {
		return err
	}
	*h = answerHTTPHistory(turns)
	return nil
}

// AnswerHandler serves the domain-scoped structured answer route.
func (s *Server) AnswerHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if !s.answerEnabled() {
			writeError(w, http.StatusNotImplemented, "agent_disabled", "structured answers are not enabled on this server")
			return
		}
		var req answerHTTPRequest
		if !decodeAnswerRequest(w, r, &req) {
			return
		}
		req.Question = strings.TrimSpace(req.Question)
		switch {
		case req.Question == "":
			writeError(w, http.StatusBadRequest, "invalid_request", "question is required")
			return
		case req.Domain == "":
			writeError(w, http.StatusBadRequest, "invalid_request", "domain_required")
			return
		case !graph.ValidDomain(req.Domain):
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid_domain")
			return
		}
		if err := req.Generation.validate(s.generationCapabilities()); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
		defer cancel()
		if s.store == nil {
			writeAnswerReadError(w, ctx, errors.New("graph store is unavailable"))
			return
		}
		exists, stores, err := s.store.DomainStores(ctx, req.Domain)
		if writeAnswerReadError(w, ctx, err) {
			return
		}
		if !exists {
			writeError(w, http.StatusBadRequest, "invalid_request", "unknown_domain")
			return
		}
		conversation, err := req.Context.validate(stores)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_context", err.Error())
			return
		}

		run, err := s.runAnswer(ctx, AnswerRequest{
			Question: req.Question, Domain: req.Domain, Context: conversation, Generation: req.Generation,
		}, stores)
		if writeAnswerTimeout(w, ctx, err) {
			return
		}
		if err != nil {
			slog.Warn("agent: answer failed", "err", err)
			kind, detail := classifyStreamError(err)
			writeError(w, http.StatusBadGateway, kind, detail)
			return
		}

		marks := resolveMarks(run.Text, run.Seen)
		cards, err := s.store.ProductCards(ctx, req.Domain, marks.Products, maxAnswerFeatures)
		if writeAnswerReadError(w, ctx, err) {
			return
		}
		docs, err := s.store.ChunkDocuments(ctx, req.Domain, marks.Chunks)
		if writeAnswerReadError(w, ctx, err) {
			return
		}
		answer := buildMarkedAnswer(run, marks, cards, docs, req.Domain)
		if writeAnswerTimeout(w, ctx, nil) {
			return
		}
		answer.Trace.MS = time.Since(start).Milliseconds()
		writeJSON(w, http.StatusOK, answer)
	})
}

func (s *Server) answerEnabled() bool {
	return s.cfg.Agent.Enabled && s.cfg.Agent.AnswerEnabled
}

func decodeAnswerRequest(w http.ResponseWriter, r *http.Request, dst *answerHTTPRequest) bool {
	if r.ContentLength > responseBodyCap {
		writeAgentRequestDecodeError(w, &http.MaxBytesError{Limit: responseBodyCap})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, responseBodyCap)
	// Enforce the limit on the entire body before validating its JSON.
	body, err := io.ReadAll(r.Body)
	if err == nil {
		err = json.Unmarshal(body, dst)
	}
	if err != nil {
		writeAgentRequestDecodeError(w, err)
		return false
	}
	return true
}

func (c *answerHTTPContext) validate(stores []string) (*AnswerContext, error) {
	if c == nil {
		return nil, nil
	}
	if len(c.ProductIDs) > 10 {
		return nil, errors.New("context accepts at most 10 product IDs")
	}
	for _, id := range c.ProductIDs {
		store, err := productobs.ImportIdentityPrefix(id)
		if err != nil || len(id) > productobs.MaxImportIdentityBytes || !slices.Contains(stores, store) {
			return nil, errors.New("context product IDs must have a valid store prefix bound to the domain and be at most 1 KiB")
		}
	}
	if len(c.History) > 6 {
		return nil, errors.New("context history accepts at most 6 turns")
	}
	conversation := &AnswerContext{ProductIDs: c.ProductIDs, History: make([]HistoryTurn, 0, len(c.History))}
	total := 0
	for _, turn := range c.History {
		if turn.Role != "user" && turn.Role != "assistant" {
			return nil, errors.New("context history roles must be user or assistant")
		}
		if turn.Text == nil {
			return nil, errors.New("context history text is required")
		}
		if utf8.RuneCountInString(*turn.Text) > 1000 {
			return nil, errors.New("context history text must be at most 1000 characters per turn")
		}
		total += len(*turn.Text)
		conversation.History = append(conversation.History, HistoryTurn{Role: turn.Role, Text: *turn.Text})
	}
	if total > 4<<10 {
		return nil, errors.New("context history text must be at most 4 KiB in total")
	}
	return conversation, nil
}

// A graph read can return an unrelated error after its context expires.
func writeAnswerReadError(w http.ResponseWriter, ctx context.Context, err error) bool {
	if writeAnswerTimeout(w, ctx, err) {
		return true
	}
	if err == nil {
		return false
	}
	slog.Warn("agent: answer graph read failed", "err", err)
	writeError(w, http.StatusBadGateway, "hydration_error", "could not read graph data for the answer")
	return true
}

func writeAnswerTimeout(w http.ResponseWriter, ctx context.Context, err error) bool {
	deadline, hasDeadline := ctx.Deadline()
	expired := hasDeadline && !time.Now().Before(deadline)
	if !expired && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return false
	}
	kind, detail := classifyStreamError(context.DeadlineExceeded)
	writeError(w, http.StatusGatewayTimeout, kind, detail)
	return true
}
