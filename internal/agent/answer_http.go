package agent

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

// AnswerHandler serves the domain-scoped structured answer route.
func (s *Server) AnswerHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if !s.answerEnabled() {
			writeError(w, http.StatusNotImplemented, "agent_disabled", "structured answers are not enabled on this server")
			return
		}
		var req AnswerRequest
		if !decodeAgentRequest(w, r, &req) {
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
		if err := validateAnswerContext(req.Context, stores); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_context", err.Error())
			return
		}

		run, err := s.runAnswer(ctx, req, stores)
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

func validateAnswerContext(conversation *AnswerContext, stores []string) error {
	if conversation == nil {
		return nil
	}
	if len(conversation.ProductIDs) > 10 {
		return errors.New("context accepts at most 10 product IDs")
	}
	for _, id := range conversation.ProductIDs {
		store, err := productobs.ImportIdentityPrefix(id)
		if err != nil || len(id) > productobs.MaxImportIdentityBytes || !slices.Contains(stores, store) {
			return errors.New("context product IDs must have a valid store prefix bound to the domain and be at most 1 KiB")
		}
	}
	if len(conversation.History) > 6 {
		return errors.New("context history accepts at most 6 turns")
	}
	total := 0
	for _, turn := range conversation.History {
		if turn.Role != "user" && turn.Role != "assistant" {
			return errors.New("context history roles must be user or assistant")
		}
		if utf8.RuneCountInString(turn.Text) > 1000 {
			return errors.New("context history text must be at most 1000 characters per turn")
		}
		total += len(turn.Text)
	}
	if total > 4<<10 {
		return errors.New("context history text must be at most 4 KiB in total")
	}
	return nil
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
