// Package embed provides the optional text-embedding providers: an
// OpenAI-compatible /embeddings client and the asymmetric furls client for
// an embeddings service with per-side Function URLs. When unconfigured the
// service runs text-only — no vector index is created and search never
// depends on it.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// defaultModel is used when Embedder.Model is empty for the OpenAI provider.
const defaultModel = "text-embedding-3-small"

// Provider embeds texts; implementations are safe for concurrent use.
// Documents (ingest) and queries (search) go through separate methods: the
// furls service packs a different prompt prefix per side, so the two must
// never be conflated.
type Provider interface {
	Name() string
	Space() string // "provider|model|dim" — the embedding-space marker value
	EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error)
	EmbedQuery(ctx context.Context, text string) ([]float64, error)
}

// OpenAI is the OpenAI-compatible embeddings client.
type OpenAI struct {
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
	dim     int
}

// NewOpenAI builds the client from Embedder config; an empty model falls
// back to defaultModel. Dim > 0 is validated against every response.
func NewOpenAI(cfg config.Config) *OpenAI {
	model := cfg.Embedder.Model
	if model == "" {
		model = defaultModel
	}
	return &OpenAI{
		client:  &http.Client{Timeout: 30 * time.Second},
		baseURL: strings.TrimRight(cfg.Embedder.BaseURL, "/"),
		apiKey:  cfg.Embedder.APIKey,
		model:   model,
		dim:     cfg.Embedder.Dim,
	}
}

// Name implements Provider.
func (o *OpenAI) Name() string { return "openai" }

// Space implements Provider: the marker that must match the stored
// vectors' space, so a provider/model/dim switch never mixes spaces
// silently (both common spaces can share a dimension).
func (o *OpenAI) Space() string {
	return "openai|" + o.model + "|" + strconv.Itoa(o.dim)
}

// EmbedDocuments POSTs the texts to /embeddings and returns the vectors in
// input order. A configured Dim > 0 is requested from the provider via the
// `dimensions` parameter (OpenAI v3 models honor it) and validated against
// every response: a mismatch is an error, not a silently broken vector
// index. Dim = 0 accepts the provider's native dimension.
func (o *OpenAI) EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, fmt.Errorf("embed: no texts to embed")
	}
	payload := map[string]any{"model": o.model, "input": texts}
	if o.dim > 0 {
		payload["dimensions"] = o.dim
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("embed: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("embed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embed: response has %d vectors for %d texts", len(parsed.Data), len(texts))
	}
	vecs := make([][]float64, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("embed: response index %d out of range", d.Index)
		}
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embed: empty vector at index %d", d.Index)
		}
		if o.dim > 0 && len(d.Embedding) != o.dim {
			return nil, fmt.Errorf("embed: vector dimension %d does not match the configured dim %d", len(d.Embedding), o.dim)
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("embed: missing vector for input %d", i)
		}
	}
	return vecs, nil
}

// EmbedQuery embeds exactly one query text through the same single-call
// path; a response that does not carry exactly one vector is an error.
func (o *OpenAI) EmbedQuery(ctx context.Context, text string) ([]float64, error) {
	vecs, err := o.EmbedDocuments(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("embed: query response has %d vectors, want 1", len(vecs))
	}
	return vecs[0], nil
}
