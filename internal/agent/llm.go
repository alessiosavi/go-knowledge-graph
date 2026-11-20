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
	"github.com/alessiosavi/go-knowledge-graph/internal/llmauth"
)

// responseCap bounds one provider response (4 MiB, matching the other
// clients).
const responseCap = 4 << 20

// providerError classifies a provider failure: a transport failure carries
// status 0, anything else the HTTP status. Its message never contains the
// endpoint or the API key. Transport diagnostics are logged server-side with
// the key redacted.
type providerError struct {
	status int
	detail string
	inBand bool // a provider error object inside an HTTP 200 response
	// permanent marks a request that can never be sent (an unparsable base
	// URL): it reads as unreachable but is not retried.
	permanent bool
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
	return !e.permanent && (e.status == 0 || e.status == http.StatusTooManyRequests || e.status >= 500)
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
	Role             string          `json:"role"`
	Content          string          `json:"content,omitempty"`
	ToolCalls        []toolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ExtraContent     json.RawMessage `json:"extra_content,omitempty"`
}

// toolCall is one requested function call.
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

type chatRequest struct {
	Model            string            `json:"model"`
	Messages         []chatMessage     `json:"messages"`
	Tools            []toolDef         `json:"tools,omitempty"`
	ToolChoice       string            `json:"tool_choice,omitempty"`
	ResponseFormat   map[string]string `json:"response_format,omitempty"`
	Temperature      float64           `json:"temperature"`
	MaxTokens        *int              `json:"max_tokens,omitempty"`
	TopP             *float64          `json:"top_p,omitempty"`
	TopK             *int              `json:"top_k,omitempty"`
	ReasoningEffort  *string           `json:"reasoning_effort,omitempty"`
	Stream           bool              `json:"stream,omitempty"`
	customGeneration bool
}

// marshalChatRequest preserves provider-owned extra_content bytes. The JSON
// encoder compacts RawMessage values and escapes HTML, so reserve their slots
// in the transcript and fill them after encoding the rest of the request.
func marshalChatRequest(req chatRequest) ([]byte, error) {
	placeholder := json.RawMessage("null")
	var extras []json.RawMessage
	var messages []chatMessage
	for i, message := range req.Messages {
		var calls []toolCall
		for j, call := range message.ToolCalls {
			if len(call.ExtraContent) == 0 {
				continue
			}
			if calls == nil {
				calls = slices.Clone(message.ToolCalls)
			}
			extras = append(extras, call.ExtraContent)
			calls[j].ExtraContent = placeholder
		}
		if calls != nil {
			message.ToolCalls = calls
		}
		// tool_calls precedes the message's own extra_content on the wire.
		if len(message.ExtraContent) != 0 {
			extras = append(extras, message.ExtraContent)
			message.ExtraContent = placeholder
		} else if calls == nil {
			continue
		}
		if messages == nil {
			messages = slices.Clone(req.Messages)
		}
		messages[i] = message
	}
	if messages != nil {
		req.Messages = messages
	}
	body, err := json.Marshal(req)
	if err != nil || len(extras) == 0 {
		return body, err
	}
	// Messages precede the tool definitions, and their string fields cannot
	// contain the unescaped JSON key used to locate each reserved slot.
	const key = `"extra_content":`
	var result []byte
	for _, extra := range extras {
		before, after, found := bytes.Cut(body, []byte(key+"null"))
		if !found || !json.Valid(extra) {
			return nil, errors.New("invalid extra_content in agent transcript")
		}
		result = append(result, before...)
		result = append(result, key...)
		result = append(result, extra...)
		body = after
	}
	return append(result, body...), nil
}

// DeepSeek tool continuations with explicit reasoning can require the prior
// assistant reasoning on the provider wire. It is never an answer delta.
func (req chatRequest) keepToolReasoning() bool {
	return len(req.Tools) != 0 && req.ReasoningEffort != nil && *req.ReasoningEffort != "none"
}

type chatReply struct {
	Content          string          `json:"content"`
	ToolCalls        []toolCall      `json:"tool_calls"`
	ReasoningContent string          `json:"reasoning_content"`
	ExtraContent     json.RawMessage `json:"extra_content"`
}

func (r *chatReply) redact(replace func(string) string) {
	r.Content = replace(r.Content)
	r.ReasoningContent = replace(r.ReasoningContent)
	for i := range r.ToolCalls {
		call := &r.ToolCalls[i]
		call.ID = replace(call.ID)
		call.Type = replace(call.Type)
		call.Function.Name = replace(call.Function.Name)
		call.Function.Arguments = replace(call.Function.Arguments)
	}
}

type chatWireResponse struct {
	Choices []struct {
		Message chatReply `json:"message"`
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
	tokens  llmauth.TokenProvider
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
	c := &client{
		baseURL: base,
		apiKey:  cfg.APIKey,
		model:   model,
		http:    &http.Client{Timeout: 120 * time.Second},
		backoff: []time.Duration{time.Second, 4 * time.Second},
	}
	if cfg.Auth == config.AuthGCP {
		c.tokens = llmauth.GCE()
		c.http.CheckRedirect = llmauth.RejectRedirect
	}
	return c
}

// preparedAuthorization carries the SSE preflight credential into the
// first attempt. Only the ask goroutine consumes it, and retries fetch anew.
type preparedAuthorization struct{ value string }
type authorizationContextKey struct{}

func (c *client) prepareStream(ctx context.Context) (context.Context, error) {
	if c.tokens == nil {
		return ctx, nil
	}
	value, err := llmauth.Bearer(ctx, c.tokens)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, authorizationContextKey{}, &preparedAuthorization{value}), nil
}

func (c *client) authorize(req *http.Request) (func(string) string, error) {
	if c.tokens == nil {
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		return c.redactAPIKey, nil
	}

	var value string
	ctx := req.Context()
	if prepared, ok := ctx.Value(authorizationContextKey{}).(*preparedAuthorization); ok && prepared.value != "" {
		value, prepared.value = prepared.value, ""
	} else {
		var err error
		value, err = llmauth.Bearer(ctx, c.tokens)
		if err != nil {
			return nil, err
		}
	}
	req.Header.Set("Authorization", value)
	return llmauth.Redactor(value), nil
}

func retryableAttempt(err error) bool {
	var pe *providerError
	return llmauth.Class(err) == llmauth.TransientClass || (errors.As(err, &pe) && pe.retryable())
}

// chat performs one chat-completions call, retrying retryable failures with
// the configured backoff. The caller's context bounds the whole loop.
func (c *client) chat(ctx context.Context, req chatRequest) (chatReply, error) {
	body, err := marshalChatRequest(req)
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
		err = req.classifyRejection(err)
		lastErr = err
		if !retryableAttempt(err) {
			return chatReply{}, err
		}
	}
	return chatReply{}, lastErr
}

func (c *client) doChat(ctx context.Context, body []byte) (chatReply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		// The parse error quotes the endpoint, which may embed a credential.
		slog.Warn("agent: provider request could not be built; check the agent base URL")
		return chatReply{}, &providerError{status: 0, permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	redact, err := c.authorize(req)
	if err != nil {
		return chatReply{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return chatReply{}, err
		}
		slog.Warn("agent: provider request failed", "err", redact(err.Error()))
		return chatReply{}, &providerError{status: 0}
	}
	defer resp.Body.Close()
	if c.tokens != nil {
		if err := llmauth.VertexStatus(resp, redact); err != nil {
			return chatReply{}, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, responseCap+1))
	if err != nil {
		slog.Warn("agent: provider response read failed", "err", redact(err.Error()))
		return chatReply{}, &providerError{status: 0}
	}
	if len(data) > responseCap {
		return chatReply{}, fmt.Errorf("agent: response exceeds the %d MiB cap", responseCap>>20)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return chatReply{}, &providerError{status: resp.StatusCode, detail: llmauth.TrimPreview(redact(string(data)), llmauth.PreviewPrefixBytes)}
	}
	var wire chatWireResponse
	if err := json.Unmarshal(data, &wire); err != nil {
		if c.tokens != nil {
			return chatReply{}, fmt.Errorf("agent: decode response: %s", redact(err.Error()))
		}
		return chatReply{}, fmt.Errorf("agent: decode response: %w", err)
	}
	if wire.Error != nil {
		return chatReply{}, &providerError{status: http.StatusBadGateway, inBand: true, detail: llmauth.TrimPreview(redact(wire.Error.Message), llmauth.PreviewPrefixBytes)}
	}
	if len(wire.Choices) == 0 {
		return chatReply{}, fmt.Errorf("agent: response without choices")
	}
	reply := wire.Choices[0].Message
	if c.tokens != nil {
		reply.redact(redact)
	}
	return reply, nil
}

// chatStream performs one streamed chat-completions call, forwarding every
// content fragment to onDelta (nil disables forwarding) and assembling the
// assistant message from the SSE frames. The retry envelope mirrors chat
// until a fragment has been forwarded: retrying after delivery would
// concatenate two attempts in the caller's progressive state, so a late
// failure surfaces as the classified error instead (the fallback table
// treats anything past the first event as "any other failure"). A stream
// that never delivers an event reports errStreamClosedBeforeEvent so the
// caller can fall back to the non-streamed path once.
func (c *client) chatStream(ctx context.Context, req chatRequest, onDelta func(string) error) (chatReply, error) {
	req.Stream = true
	body, err := marshalChatRequest(req)
	if err != nil {
		return chatReply{}, fmt.Errorf("agent: encode request: %w", err)
	}
	var lastErr error
	delivered := false
	forward := func(text string) error {
		if onDelta == nil {
			return nil
		}
		delivered = true
		return onDelta(text)
	}
	for attempt := 0; attempt <= len(c.backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(c.backoff[attempt-1]):
			case <-ctx.Done():
				return chatReply{}, ctx.Err()
			}
		}
		reply, err := c.doStream(ctx, body, req.keepToolReasoning(), forward)
		if err == nil {
			return reply, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return chatReply{}, ctxErr
		}
		err = req.classifyRejection(err)
		lastErr = err
		if !retryableAttempt(err) {
			return chatReply{}, err
		}
		if delivered {
			return chatReply{}, err
		}
	}
	return chatReply{}, lastErr
}

func (c *client) doStream(ctx context.Context, body []byte, keepReasoning bool, onDelta func(string) error) (chatReply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		// The parse error quotes the endpoint, which may embed a credential.
		slog.Warn("agent: provider request could not be built; check the agent base URL")
		return chatReply{}, &providerError{status: 0, permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	redact, err := c.authorize(req)
	if err != nil {
		return chatReply{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return chatReply{}, err
		}
		slog.Warn("agent: provider stream request failed", "err", redact(err.Error()))
		return chatReply{}, &providerError{status: 0}
	}
	defer resp.Body.Close()
	if c.tokens != nil {
		if err := llmauth.VertexStatus(resp, redact); err != nil {
			return chatReply{}, err
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, responseCap+1))
		return chatReply{}, &providerError{status: resp.StatusCode, detail: llmauth.TrimPreview(redact(string(data)), llmauth.PreviewPrefixBytes)}
	}
	if c.tokens == nil {
		return readSSE(resp.Body, keepReasoning, onDelta, redact)
	}
	var forward func(string) error
	if onDelta != nil {
		forward = func(delta string) error { return onDelta(redact(delta)) }
	}
	reply, err := readSSE(resp.Body, keepReasoning, forward, redact)
	if err == nil {
		reply.redact(redact)
	}
	return reply, err
}

// streamWireChunk is one SSE frame of a chat-completions stream.
type streamWireChunk struct {
	Choices []struct {
		Delta struct {
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ExtraContent     json.RawMessage `json:"extra_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
				ExtraContent json.RawMessage `json:"extra_content"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// mergeStreamExtra preserves native JSON values. A structured value split
// across JSON frames starts with a quoted fragment containing '{' or '['.
func mergeStreamExtra(current, next json.RawMessage) json.RawMessage {
	if len(next) == 0 {
		return current
	}
	if len(current) != 0 && (bytes.Equal(next, []byte("null")) ||
		(next[0] == '{' && next[len(next)-1] == '}' && len(bytes.TrimSpace(next[1:len(next)-1])) == 0)) {
		return current
	}
	assembling := len(current) != 0 && !json.Valid(current)
	if next[0] == '"' {
		var fragment string
		// The stream frame has already been validated by json.Unmarshal.
		_ = json.Unmarshal(next, &fragment)
		start := strings.TrimLeft(fragment, " \t\r\n")
		if assembling || strings.HasPrefix(start, "{") || strings.HasPrefix(start, "[") {
			next = json.RawMessage(fragment)
		}
	}
	if assembling {
		return append(current, next...)
	}
	return next
}

// streamRawCap bounds the bytes read from one streamed body, framing
// included; tests override it. It stays above the assembled cap so normal
// SSE overhead cannot trip it.
var streamRawCap = int64(responseCap * 2)

// readSSE consumes one SSE chat-completions stream: content and
// index-keyed tool-call fragments assemble into the assistant message and
// [DONE] terminates. A stream that never delivered a data frame reports
// errStreamClosedBeforeEvent; a stream cut after events — including a
// connection that ends mid-line — reports a classified premature-close
// error instead of blaming the truncated frame.
func readSSE(body io.Reader, keepReasoning bool, onDelta func(string) error, redact func(string) string) (chatReply, error) {
	limited := &io.LimitedReader{R: body, N: streamRawCap + 1}
	reader := bufio.NewReader(limited)
	var (
		content   strings.Builder
		reasoning strings.Builder
		extra     json.RawMessage
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
				invalidExtra := false
				validateExtra := func(value json.RawMessage) json.RawMessage {
					if len(value) != 0 && !json.Valid(value) {
						invalidExtra = true
						return nil
					}
					return value
				}
				reply := chatReply{Content: content.String(), ReasoningContent: reasoning.String(), ExtraContent: validateExtra(extra)}
				slices.Sort(order)
				for _, i := range order {
					call := *calls[i]
					call.ExtraContent = validateExtra(call.ExtraContent)
					reply.ToolCalls = append(reply.ToolCalls, call)
				}
				if invalidExtra {
					slog.Warn("agent: discarded invalid extra_content in provider stream")
				}
				return reply, nil
			}
			var chunk streamWireChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				if readErr == nil {
					return chatReply{}, fmt.Errorf("agent: decode stream frame: %w", err)
				}
				// The connection ended mid-line: classify the stream end
				// below instead of blaming the truncated JSON.
			} else {
				frames++
				if chunk.Error != nil {
					return chatReply{}, &providerError{status: http.StatusBadGateway, inBand: true, detail: llmauth.TrimPreview(redact(chunk.Error.Message), llmauth.PreviewPrefixBytes)}
				}
				if len(chunk.Choices) > 0 {
					delta := chunk.Choices[0].Delta
					assembled += len(delta.Content) + len(delta.ExtraContent)
					if keepReasoning {
						assembled += len(delta.ReasoningContent)
					}
					if assembled > responseCap {
						return chatReply{}, fmt.Errorf("agent: streamed response exceeds the %d MiB cap", responseCap>>20)
					}
					if keepReasoning {
						reasoning.WriteString(delta.ReasoningContent)
					}
					extra = mergeStreamExtra(extra, delta.ExtraContent)
					if delta.Content != "" {
						content.WriteString(delta.Content)
						if onDelta != nil {
							if err := onDelta(delta.Content); err != nil {
								return chatReply{}, err
							}
						}
					}
					for _, frag := range delta.ToolCalls {
						assembled += len(frag.Function.Arguments) + len(frag.Function.Name) + len(frag.ExtraContent)
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
						call.ExtraContent = mergeStreamExtra(call.ExtraContent, frag.ExtraContent)
						call.Function.Name += frag.Function.Name
						call.Function.Arguments += frag.Function.Arguments
					}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if limited.N <= 0 {
					return chatReply{}, fmt.Errorf("agent: streamed response exceeds the %d MiB cap", responseCap>>20)
				}
				if frames == 0 {
					return chatReply{}, errStreamClosedBeforeEvent
				}
				return chatReply{}, errors.New("agent: stream ended before the completion marker")
			}
			slog.Warn("agent: provider stream read failed", "err", redact(readErr.Error()))
			return chatReply{}, &providerError{status: 0}
		}
	}
}

// redactAPIKey handles provider diagnostics that echo the bearer credential
// in plain text or JSON. Redact before trimming so a clipped key stays private.
func (c *client) redactAPIKey(s string) string {
	if c.apiKey == "" {
		return s
	}
	quoted, _ := json.Marshal(c.apiKey)
	return strings.NewReplacer(string(quoted[1:len(quoted)-1]), "[redacted]", c.apiKey, "[redacted]").Replace(s)
}
