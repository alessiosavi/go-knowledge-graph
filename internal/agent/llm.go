package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// responseCap bounds one provider response (4 MiB, matching the other
// clients).
const responseCap = 4 << 20

// providerError classifies a provider failure: a transport failure carries
// status 0, anything else the HTTP status. Its message never contains the
// endpoint or the API key — the full transport error goes to the server log
// only.
type providerError struct {
	status int
	detail string
}

func (e *providerError) Error() string {
	switch {
	case e.status == 0:
		return "the agent provider is unreachable"
	case e.detail != "":
		return fmt.Sprintf("the agent provider rejected the request (HTTP %d): %s", e.status, e.detail)
	default:
		return fmt.Sprintf("the agent provider rejected the request (HTTP %d)", e.status)
	}
}

// retryable reports whether another attempt is worth it: transport
// failures, 429 and 5xx.
func (e *providerError) retryable() bool {
	return e.status == 0 || e.status == http.StatusTooManyRequests || e.status >= 500
}

// toolDef is one OpenAI-compatible function definition.
type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// functionTool builds one function definition.
func functionTool(name, description string, parameters map[string]any) toolDef {
	t := toolDef{Type: "function"}
	t.Function.Name = name
	t.Function.Description = description
	t.Function.Parameters = parameters
	return t
}

// chatMessage is one chat turn; tool results carry ToolCallID, assistant
// turns carry ToolCalls.
type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// toolCall is one requested function call.
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	Tools          []toolDef         `json:"tools,omitempty"`
	ToolChoice     string            `json:"tool_choice,omitempty"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
	Temperature    float64           `json:"temperature"`
}

type chatReply struct {
	Content   string
	ToolCalls []toolCall
}

type chatWireResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// client is the OpenAI-compatible chat client the agent loop drives. It
// mirrors the judge client's transport discipline: response cap, retries
// only on rate limits/5xx/network, classified errors that never echo the
// endpoint or the key.
type client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
	backoff []time.Duration
}

// newClient builds the chat client from the Agent configuration.
func newClient(cfg config.AgentConfig) *client {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	model := cfg.Model
	if model == "" {
		model = "deepseek-v4.1-flash"
	}
	return &client{
		baseURL: base,
		apiKey:  cfg.APIKey,
		model:   model,
		http:    &http.Client{Timeout: 120 * time.Second},
		backoff: []time.Duration{time.Second, 4 * time.Second},
	}
}

// chat performs one chat-completions call, retrying retryable failures with
// the configured backoff. The caller's context bounds the whole loop.
func (c *client) chat(ctx context.Context, req chatRequest) (chatReply, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return chatReply{}, fmt.Errorf("agent: encode request: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt <= len(c.backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(c.backoff[attempt-1]):
			case <-ctx.Done():
				return chatReply{}, ctx.Err()
			}
		}
		reply, err := c.doChat(ctx, body)
		if err == nil {
			return reply, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return chatReply{}, ctxErr
		}
		lastErr = err
		var pe *providerError
		if !errors.As(err, &pe) || !pe.retryable() {
			return chatReply{}, err
		}
	}
	return chatReply{}, lastErr
}

func (c *client) doChat(ctx context.Context, body []byte) (chatReply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatReply{}, fmt.Errorf("agent: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return chatReply{}, err
		}
		slog.Warn("agent: provider request failed", "err", err)
		return chatReply{}, &providerError{status: 0}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, responseCap+1))
	if err != nil {
		slog.Warn("agent: provider response read failed", "err", err)
		return chatReply{}, &providerError{status: 0}
	}
	if len(data) > responseCap {
		return chatReply{}, fmt.Errorf("agent: response exceeds the %d MiB cap", responseCap>>20)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return chatReply{}, &providerError{status: resp.StatusCode, detail: trimPreview(string(data))}
	}
	var wire chatWireResponse
	if err := json.Unmarshal(data, &wire); err != nil {
		return chatReply{}, fmt.Errorf("agent: decode response: %w", err)
	}
	if wire.Error != nil {
		return chatReply{}, &providerError{status: http.StatusBadGateway, detail: trimPreview(wire.Error.Message)}
	}
	if len(wire.Choices) == 0 {
		return chatReply{}, fmt.Errorf("agent: response without choices")
	}
	msg := wire.Choices[0].Message
	return chatReply{Content: msg.Content, ToolCalls: msg.ToolCalls}, nil
}

// trimPreview caps a provider payload preview; previews are the provider's
// own words about the failed request.
func trimPreview(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
