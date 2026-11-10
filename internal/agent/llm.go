package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
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
	Stream         bool              `json:"stream,omitempty"`
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

// errStreamClosedBeforeEvent marks a streaming response that ended before
// delivering any event: the signal that the endpoint likely does not
// support streaming, so the caller retries the request non-streamed once.
var errStreamClosedBeforeEvent = errors.New("agent: stream closed before the first event")

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

	// mu guards the stream-negotiation memos; both are set once and last
	// for the process lifetime (one client per running server).
	mu                   sync.Mutex
	streamUnsupported    bool
	contentOnlyStreaming bool
}

// memoStreamUnsupported records that the provider rejected (or closed) a
// streaming attempt before its first event; later asks skip the attempt.
func (c *client) memoStreamUnsupported() {
	c.mu.Lock()
	c.streamUnsupported = true
	c.mu.Unlock()
}

// memoContentOnly records that streamed turns can deliver content but no
// usable tool calls; tool-bearing turns run non-streamed from then on.
func (c *client) memoContentOnly() {
	c.mu.Lock()
	c.contentOnlyStreaming = true
	c.mu.Unlock()
}

// streamState reports the stream-negotiation memos.
func (c *client) streamState() (unsupported, contentOnly bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streamUnsupported, c.contentOnlyStreaming
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

// chatStream performs one streamed chat-completions call, forwarding every
// content fragment to onDelta (nil disables forwarding) and assembling the
// assistant message from the SSE frames. The retry envelope mirrors chat;
// a stream that never delivers an event reports errStreamClosedBeforeEvent
// so the caller can fall back to the non-streamed path once.
func (c *client) chatStream(ctx context.Context, req chatRequest, onDelta func(string) error) (chatReply, error) {
	req.Stream = true
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
		reply, err := c.doStream(ctx, body, onDelta)
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

func (c *client) doStream(ctx context.Context, body []byte, onDelta func(string) error) (chatReply, error) {
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
		slog.Warn("agent: provider stream request failed", "err", err)
		return chatReply{}, &providerError{status: 0}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, responseCap+1))
		return chatReply{}, &providerError{status: resp.StatusCode, detail: trimPreview(string(data))}
	}
	return readSSE(resp.Body, onDelta)
}

// streamWireChunk is one SSE frame of a chat-completions stream.
type streamWireChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// readSSE consumes one SSE chat-completions stream: content and
// index-keyed tool-call fragments assemble into the assistant message and
// [DONE] terminates. A stream that never delivered a data frame reports
// errStreamClosedBeforeEvent; a stream cut after events reports a
// classified premature-close error.
func readSSE(body io.Reader, onDelta func(string) error) (chatReply, error) {
	reader := bufio.NewReader(body)
	var (
		content   strings.Builder
		calls     = map[int]*toolCall{}
		order     []int
		frames    int
		assembled int
	)
	for {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, ":"):
			// Frame separator or keep-alive comment.
		case strings.HasPrefix(trimmed, "data:"):
			data := strings.TrimPrefix(trimmed, "data:")
			data = strings.TrimPrefix(data, " ")
			if data == "[DONE]" {
				reply := chatReply{Content: content.String()}
				slices.Sort(order)
				for _, i := range order {
					reply.ToolCalls = append(reply.ToolCalls, *calls[i])
				}
				return reply, nil
			}
			var chunk streamWireChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				return chatReply{}, fmt.Errorf("agent: decode stream frame: %w", err)
			}
			frames++
			if chunk.Error != nil {
				return chatReply{}, &providerError{status: http.StatusBadGateway, detail: trimPreview(chunk.Error.Message)}
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			delta := chunk.Choices[0].Delta
			if delta.Content != "" {
				assembled += len(delta.Content)
				if assembled > responseCap {
					return chatReply{}, fmt.Errorf("agent: streamed response exceeds the %d MiB cap", responseCap>>20)
				}
				content.WriteString(delta.Content)
				if onDelta != nil {
					if err := onDelta(delta.Content); err != nil {
						return chatReply{}, err
					}
				}
			}
			for _, frag := range delta.ToolCalls {
				assembled += len(frag.Function.Arguments)
				if assembled > responseCap {
					return chatReply{}, fmt.Errorf("agent: streamed response exceeds the %d MiB cap", responseCap>>20)
				}
				call := calls[frag.Index]
				if call == nil {
					call = &toolCall{Type: "function"}
					calls[frag.Index] = call
					order = append(order, frag.Index)
				}
				if frag.ID != "" {
					call.ID = frag.ID
				}
				call.Function.Name += frag.Function.Name
				call.Function.Arguments += frag.Function.Arguments
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if frames == 0 {
					return chatReply{}, errStreamClosedBeforeEvent
				}
				return chatReply{}, errors.New("agent: stream ended before the completion marker")
			}
			slog.Warn("agent: provider stream read failed", "err", readErr)
			return chatReply{}, &providerError{status: 0}
		}
	}
}

// trimPreview caps a provider payload preview; previews are the provider's
// own words about the failed request.
func trimPreview(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
