package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/llmauth"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

// EvalOption configures an agent used by the offline evaluation runner.
type EvalOption func(*Server)

// NewEvalServer builds the same ask agent with evaluation-only options.
func NewEvalServer(cfg config.Config, store *graph.Store, emb search.QueryEmbedder, options ...EvalOption) *Server {
	s := NewServer(cfg, store, emb)
	for _, opt := range options {
		opt(s)
	}
	return s
}

// WithEvalTransport meters provider attempts and checks for a ledger stop
// before the agent turns a refused send into a retry or fallback. Redirects
// are disabled so a provider cannot send a request outside the metered host.
func WithEvalTransport(transport http.RoundTripper, gate func() error) EvalOption {
	return func(s *Server) {
		s.llm.http.Transport = transport
		s.llm.http.CheckRedirect = llmauth.RejectRedirect
		s.llm.attemptGate = gate
	}
}

// WithEvalTokenProvider supplies a scripted credential for provider tests.
func WithEvalTokenProvider(tokens llmauth.TokenProvider) EvalOption {
	return func(s *Server) { s.llm.tokens = tokens }
}

// EvalEndpoint returns the effective model and host for ledger validation.
func EvalEndpoint(cfg config.AgentConfig) (model, host string, err error) {
	base, model := ResolveEndpoint(cfg)
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", "", errors.New("invalid agent base URL")
	}
	return model, u.Host, nil
}

// NormalizeEvalAsk uses the JSON endpoint's request validation before the
// runner sends any ask from the frozen input.
func NormalizeEvalAsk(cfg config.AgentConfig, req AskRequest) (AskRequest, error) {
	_, model := ResolveEndpoint(cfg)
	return normalizeAskQuestion(req, configuredGeneration(cfg, model))
}

// EvalStreamMemoEmpty reports whether neither streaming fallback is memoized.
func (s *Server) EvalStreamMemoEmpty() bool {
	unsupported, contentOnly := s.llm.streamState()
	return !unsupported && !contentOnly
}

// EvalEvidence records the capped text sent to the model and classifies the
// full tool result without retaining that result.
type EvalEvidence struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Visible   string          `json:"visible"`
	Error     bool            `json:"error"`
}

// EvalObservation is an optional, per-ask observer. Only the ask writes it.
// Collect it after Ask returns or StreamHandler has finished serving the ask.
type EvalObservation struct {
	Evidence              []EvalEvidence
	ToolErrors            int
	UnknownTools          int
	DiscardedExtraContent int
	Steps                 int
	Err                   error
}

type evalObservationKey struct{}

// WithEvalObservation installs the observer for one in-process ask.
func WithEvalObservation(ctx context.Context, o *EvalObservation) context.Context {
	return context.WithValue(ctx, evalObservationKey{}, o)
}

func evalObservation(ctx context.Context) *EvalObservation {
	o, _ := ctx.Value(evalObservationKey{}).(*EvalObservation)
	return o
}

func (s *Server) observeEvalEvidence(ctx context.Context, item EvidenceItem, visible string) {
	o := evalObservation(ctx)
	if o == nil {
		return
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(item.Result, &result)
	_, failed := result["error"]
	o.Evidence = append(o.Evidence, EvalEvidence{Tool: item.Tool, Arguments: item.Arguments, Visible: visible, Error: failed})
	if failed {
		o.ToolErrors++
	}
	if s.toolRunner(item.Tool) == nil {
		o.UnknownTools++
	}
}

// EvalErrorClass preserves the evaluation vocabulary across plain and SSE asks.
func EvalErrorClass(err error) (class string, permanent bool) {
	if err == nil {
		return "", false
	}
	if auth := llmauth.Class(err); auth != "" {
		return "auth", auth == llmauth.PermanentClass
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", false
	}
	var provider *providerError
	var rejection *generationRejection
	if errors.As(err, &rejection) {
		provider = rejection.providerError
	} else if !errors.As(err, &provider) {
		return "other", false
	}
	switch {
	case provider.status == 0:
		return "unreachable", false
	case provider.status == http.StatusUnauthorized || provider.status == http.StatusForbidden:
		// A rejected static credential invalidates the arm instead of scoring as model failures.
		return "auth", true
	case provider.status == http.StatusTooManyRequests:
		return "rate_limited", false
	case provider.status >= 500 && provider.status < 600:
		return "provider_5xx", false
	case provider.status >= 400 && provider.status < 500:
		return "provider_4xx", false
	default:
		return "other", false
	}
}
