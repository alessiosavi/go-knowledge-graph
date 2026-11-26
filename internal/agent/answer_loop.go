package agent

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// AnswerRequest is an in-process question scoped to one domain.
type AnswerRequest struct {
	Question   string              `json:"question"`
	Domain     string              `json:"domain"`
	Context    *AnswerContext      `json:"context,omitempty"`
	Generation *GenerationSettings `json:"generation,omitempty"`
}

// AnswerContext carries the caller's conversation from the same shop.
type AnswerContext struct {
	ProductIDs []string      `json:"product_ids,omitempty"`
	History    []HistoryTurn `json:"history,omitempty"`
}

type HistoryTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type answerOutcome uint8

const (
	outcomeAnswered answerOutcome = iota
	outcomeNoAnswer
	outcomeFinalTurnFailed
	outcomeTruncated
)

type answerRun struct {
	Text        string
	Outcome     answerOutcome
	BudgetFinal bool
	Seen        *seenIndex
	Steps       int
	Tools       []string
}

type answerModeKey struct{}

// Presence in the loop context selects answer mode and carries its request-local state.
type answerLoopState struct {
	answerRun
	conversation *AnswerContext
	stores       []string
}

func answerMode(ctx context.Context) *answerLoopState {
	state, _ := ctx.Value(answerModeKey{}).(*answerLoopState)
	return state
}

func (s *Server) runAnswer(ctx context.Context, req AnswerRequest, stores []string) (answerRun, error) {
	if !graph.ValidDomain(req.Domain) {
		return answerRun{}, errors.New("a valid domain is required")
	}
	ask, err := normalizeAskQuestion(AskRequest{Question: req.Question, Domain: req.Domain, Generation: req.Generation}, s.generationCapabilities())
	if err != nil {
		return answerRun{}, err
	}
	// A non-nil empty list restricts chunk links even when the domain has no stores.
	if stores == nil {
		stores = []string{}
	}
	state := &answerLoopState{
		answerRun:    answerRun{Seen: newSeenIndex(), Tools: []string{}},
		conversation: req.Context,
		stores:       stores,
	}
	ctx, cancel := context.WithTimeout(withDomain(ctx, req.Domain), s.timeout)
	defer cancel()
	_, err = s.runWithEvents(context.WithValue(ctx, answerModeKey{}, state), ask, askEvents{})
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return answerRun{}, err
	}
	if o := evalObservation(ctx); o != nil {
		o.AnswerText = state.Text
	}
	return state.answerRun, nil
}

func answerToolAllowed(name string) bool {
	switch name {
	case "search", "get_chunks", "list_documents", "get_product", "find_products", "list_features":
		return true
	default:
		return false
	}
}

var errAnswerToolUnavailable = errors.New("tool not available in answer mode")

func staticAuthFailure(err error) bool {
	var provider *providerError
	return errors.As(err, &provider) && (provider.status == http.StatusUnauthorized || provider.status == http.StatusForbidden)
}

func loopResponse(answer *answerLoopState, reply chatReply, evidence []EvidenceItem, steps int) AskResponse {
	if answer == nil {
		return answerResponse(reply.Content, evidence, steps)
	}
	answer.Text = reply.Content
	answer.Steps = steps
	switch {
	case strings.TrimSpace(reply.Content) == "":
		answer.Outcome = outcomeNoAnswer
	case reply.FinishReason == "length":
		answer.Outcome = outcomeTruncated
	default:
		answer.Outcome = outcomeAnswered
	}
	return AskResponse{Steps: steps}
}
