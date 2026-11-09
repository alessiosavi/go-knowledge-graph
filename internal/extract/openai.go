package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// openAIBatch is the maximum number of chunks per provider call.
const openAIBatch = 8

// OpenAI is any OpenAI-compatible /chat/completions endpoint (the
// zero-setup example is DeepSeek-V4.1-Flash).
type OpenAI struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
	backoff []time.Duration
}

// NewOpenAI builds the provider from the Extractor.OpenAI configuration.
func NewOpenAI(cfg config.Config) *OpenAI {
	base := strings.TrimRight(cfg.Extractor.OpenAI.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return &OpenAI{
		baseURL: base,
		apiKey:  cfg.Extractor.OpenAI.APIKey,
		model:   cfg.Extractor.OpenAI.Model,
		client:  &http.Client{Timeout: 120 * time.Second},
		backoff: []time.Duration{time.Second, 4 * time.Second},
	}
}

// Name implements Provider.
func (o *OpenAI) Name() string { return "openai" }

// Version implements Provider.
func (o *OpenAI) Version() string { return "openai:" + o.model }

// Extract implements Provider, batching at most openAIBatch chunks per
// HTTP call and merging the per-chunk results. The result slice always
// carries one entry per submitted chunk (in submitted order), even when
// the model omits chunks.
func (o *OpenAI) Extract(ctx context.Context, req Request) ([]ChunkResult, error) {
	entitySet := make(map[string]bool, len(req.Entities))
	for _, l := range req.Entities {
		entitySet[l.Name] = true
	}
	relationSet := make(map[string]bool, len(req.Relations))
	for _, l := range req.Relations {
		relationSet[l.Name] = true
	}
	out := make([]ChunkResult, 0, len(req.Chunks))
	for start := 0; start < len(req.Chunks); start += openAIBatch {
		end := min(start+openAIBatch, len(req.Chunks))
		results, err := o.extractBatch(ctx, req, req.Chunks[start:end], entitySet, relationSet)
		if err != nil {
			return nil, err
		}
		out = append(out, results...)
	}
	return out, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
	Temperature    float64           `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (o *OpenAI) extractBatch(ctx context.Context, req Request, chunks []RequestChunk, entitySet, relationSet map[string]bool) ([]ChunkResult, error) {
	body, err := json.Marshal(chatRequest{
		Model: o.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: buildUserPayload(chunks, req.Entities, req.Relations)},
		},
		ResponseFormat: map[string]string{"type": "json_object"},
		Temperature:    0,
	})
	if err != nil {
		return nil, fmt.Errorf("extract openai: encode request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= len(o.backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(o.backoff[attempt-1]):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		results, retryable, err := o.doBatch(ctx, body, chunks, entitySet, relationSet)
		if err == nil {
			return results, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("extract openai: retries exhausted: %w", lastErr)
}

// doBatch performs one HTTP call. retryable reports 429/5xx responses, the
// only ones worth retrying.
func (o *OpenAI) doBatch(ctx context.Context, body []byte, chunks []RequestChunk, entitySet, relationSet map[string]bool) ([]ChunkResult, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("extract openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, true, fmt.Errorf("extract openai: request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return nil, true, fmt.Errorf("extract openai: read response: %w", err)
	}
	if len(data) > 8<<20 {
		return nil, false, fmt.Errorf("extract openai: response exceeds the %d MiB cap", 8)
	}

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, true, fmt.Errorf("extract openai: HTTP %d: %s", resp.StatusCode, errorMessage(data))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("extract openai: HTTP %d: %s", resp.StatusCode, errorMessage(data))
	}

	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return nil, false, fmt.Errorf("extract openai: decode chat response: %w", err)
	}
	if cr.Error != nil {
		return nil, false, fmt.Errorf("extract openai: provider error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return nil, false, fmt.Errorf("extract openai: response without choices")
	}
	var wire wireResponse
	if err := json.Unmarshal([]byte(cr.Choices[0].Message.Content), &wire); err != nil {
		return nil, false, fmt.Errorf("extract openai: decode extraction JSON: %w (content preview: %q)", err, previewContent(cr.Choices[0].Message.Content))
	}

	byID := make(map[string]wireChunk, len(wire.Results))
	for _, wc := range wire.Results {
		byID[wc.ChunkID] = wc
	}
	// Contract: one result per submitted chunk, in submitted order. Model
	// omissions become empty results; unknown chunk ids are dropped.
	out := make([]ChunkResult, 0, len(chunks))
	for _, c := range chunks {
		wc, ok := byID[c.ID]
		if !ok {
			wc = wireChunk{ChunkID: c.ID}
		}
		out = append(out, validateChunkResult(c.Text, wc, entitySet, relationSet))
	}
	return out, false, nil
}

func errorMessage(data []byte) string {
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err == nil && cr.Error != nil {
		return cr.Error.Message
	}
	return previewContent(string(data))
}

// previewContent returns up to 200 bytes of raw provider content for error
// diagnostics.
func previewContent(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// validateChunkResult drops out-of-bounds spans and relation indexes,
// remaps relation indexes after filtering, clamps scores to [0,1] and
// defaults missing scores to 0.5. Labels must belong to the requested sets
// (unless the request specified none, in which case no filtering applies).
func validateChunkResult(text string, wc wireChunk, entitySet, relationSet map[string]bool) ChunkResult {
	res := ChunkResult{ChunkID: wc.ChunkID}

	keptIdx := make(map[int]int, len(wc.Entities))
	for oldIdx, e := range wc.Entities {
		if strings.TrimSpace(e.Label) == "" || strings.TrimSpace(e.Text) == "" {
			continue
		}
		if len(entitySet) > 0 && !entitySet[e.Label] {
			continue
		}
		if e.Start < 0 || e.End <= e.Start || e.End > len(text) {
			continue
		}
		// The offsets must be byte-exact into the chunk: this catches
		// rune-vs-byte drift on multi-byte text, where the provenance
		// chain (excerpts, spans) would silently shift.
		if text[e.Start:e.End] != e.Text {
			continue
		}
		keptIdx[oldIdx] = len(res.Mentions)
		res.Mentions = append(res.Mentions, Mention{
			Label: e.Label,
			Text:  e.Text,
			Start: e.Start,
			End:   e.End,
			Score: clampScore(e.Score),
		})
	}

	for _, r := range wc.Relations {
		if strings.TrimSpace(r.Label) == "" {
			continue
		}
		if len(relationSet) > 0 && !relationSet[r.Label] {
			continue
		}
		newHead, okH := keptIdx[r.Head]
		newTail, okT := keptIdx[r.Tail]
		if !okH || !okT {
			continue
		}
		res.Candidates = append(res.Candidates, Candidate{
			Label: r.Label,
			Head:  newHead,
			Tail:  newTail,
			Score: clampScore(r.Score),
		})
	}
	return res
}

func clampScore(score *float64) float64 {
	if score == nil {
		return 0.5
	}
	return min(max(*score, 0), 1)
}
