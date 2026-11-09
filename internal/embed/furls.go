package embed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// furlsDefaultModel is used when Embedder.Model is empty; it is the only
// model the embeddings service allows today (buildPipeline validates a
// configured value against that allowlist before constructing the client).
const furlsDefaultModel = "gemini-embedding-2"

// Response hygiene mirrors the OpenAI client: a 4KB error-body cap and an
// 8MB response cap.
const (
	furlsErrorBodyCap = 4 << 10 // 4KB
	furlsResponseCap  = 8 << 20 // 8MB
)

// Per-side client budgets track the service's Lambda budgets: the document
// side may persist rows and is slower, the query side is cache-only.
const (
	furlsProductTimeout = 300 * time.Second
	furlsQueryTimeout   = 60 * time.Second
)

// Furls embeds against the asymmetric embeddings service (one Function URL
// per side: embed_product for documents, embed_query for queries). The
// service packs the model-specific prompt prefixes itself — this client
// only ever sends id+text and never reformats the text. Safe for
// concurrent use.
type Furls struct {
	productClient *http.Client
	queryClient   *http.Client
	productURL    string
	queryURL      string
	apiKey        string
	model         string
	dim           int
}

// NewFurls builds the client from Embedder config; an empty model falls
// back to furlsDefaultModel. Dim > 0 is requested and validated.
func NewFurls(cfg config.Config) *Furls {
	model := cfg.Embedder.Model
	if model == "" {
		model = furlsDefaultModel
	}
	return &Furls{
		productClient: &http.Client{Timeout: furlsProductTimeout},
		queryClient:   &http.Client{Timeout: furlsQueryTimeout},
		productURL:    cfg.Embedder.ProductFURL,
		queryURL:      cfg.Embedder.QueryFURL,
		apiKey:        cfg.Embedder.APIKey,
		model:         model,
		dim:           cfg.Embedder.Dim,
	}
}

// Name implements Provider.
func (f *Furls) Name() string { return "furls" }

// Space implements Provider.
func (f *Furls) Space() string { return "furls|" + f.model + "|" + strconv.Itoa(f.dim) }

// EmbedDocuments embeds document-side texts through the product FURL: the
// service persists one row per entry, and the content-hash entry id makes
// repeated ingests reuse the persisted row.
func (f *Furls) EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error) {
	return f.embed(ctx, f.productClient, f.productURL, texts)
}

// EmbedQuery embeds one query text through the query FURL (cache-only
// side; a single-entry body).
func (f *Furls) EmbedQuery(ctx context.Context, text string) ([]float64, error) {
	vecs, err := f.embed(ctx, f.queryClient, f.queryURL, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// furlsEntry is one request entry.
type furlsEntry struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// furlsResult is one decoded response entry.
type furlsResult struct {
	ID        string    `json:"id"`
	Embedding []float64 `json:"embedding"`
	Error     string    `json:"error"`
	ErrorCode string    `json:"error_code"`
}

// furlsFailure is one entry without a usable vector.
type furlsFailure struct {
	index     int
	errorCode string
	message   string
}

// furlsEntryID derives the stable entry id: the service caches by content
// hash, so identical texts hit the cache regardless of entry id.
func furlsEntryID(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "gkg:" + hex.EncodeToString(sum[:])[:32]
}

// furlsRetryable reports whether an error_code is documented as transient
// (only these two are retried, exactly once).
func furlsRetryable(code string) bool {
	return code == "RATE_LIMITED" || code == "UPSTREAM_ERROR"
}

// furlsEntryError names the failed entry and the service's reason
// (error_code preferred over the free-text error).
func furlsEntryError(id, code, message string) error {
	reason := code
	if reason == "" {
		reason = message
	}
	if reason == "" {
		reason = "no embedding"
	}
	return fmt.Errorf("embed: furls: entry %s failed: %s", id, reason)
}

// embed runs the shared request path: one JSON POST, id-aligned results
// (position fallback), a single retry of the entries whose error_code is
// retryable, then fail on anything still without a vector.
func (f *Furls) embed(ctx context.Context, client *http.Client, url string, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, fmt.Errorf("embed: no texts to embed")
	}
	entries := make([]furlsEntry, len(texts))
	for i, t := range texts {
		entries[i] = furlsEntry{ID: furlsEntryID(t), Text: t}
	}
	vecs, failures, err := f.call(ctx, client, url, entries)
	if err != nil {
		return nil, err
	}
	if len(failures) > 0 {
		for _, fl := range failures {
			if !furlsRetryable(fl.errorCode) {
				return nil, furlsEntryError(entries[fl.index].ID, fl.errorCode, fl.message)
			}
		}
		failed := make([]furlsEntry, len(failures))
		for i, fl := range failures {
			failed[i] = entries[fl.index]
		}
		vecs2, failures2, err := f.call(ctx, client, url, failed)
		if err != nil {
			return nil, err
		}
		for i, fl := range failures {
			if vecs2[i] != nil {
				vecs[fl.index] = vecs2[i]
				continue
			}
			code, msg := fl.errorCode, fl.message
			if i < len(failures2) {
				if failures2[i].errorCode != "" {
					code = failures2[i].errorCode
				}
				if failures2[i].message != "" {
					msg = failures2[i].message
				}
			}
			return nil, furlsEntryError(entries[fl.index].ID, code, msg)
		}
	}
	for i, v := range vecs {
		if len(v) == 0 {
			// call() reports every vectorless entry as a failure; a nil
			// here would be a bug — fail loudly rather than index a hole.
			return nil, furlsEntryError(entries[i].ID, "", "no embedding")
		}
		if f.dim > 0 && len(v) != f.dim {
			return nil, fmt.Errorf("embed: furls: vector dimension %d does not match the configured dim %d", len(v), f.dim)
		}
	}
	return vecs, nil
}

// call posts one batch and returns vectors aligned to entries (nil where
// the entry has no usable vector) plus its failures. Results are mapped by
// id with a position fallback for lenient servers. A present non-empty
// embedding is usable even when error/error_code accompany it (the service
// may fail persistence after the vector was computed).
func (f *Furls) call(ctx context.Context, client *http.Client, url string, entries []furlsEntry) ([][]float64, []furlsFailure, error) {
	payload := struct {
		Entries    []furlsEntry `json:"entries"`
		Dimensions int          `json:"dimensions,omitempty"`
		Model      string       `json:"model"`
	}{Entries: entries, Model: f.model}
	if f.dim > 0 {
		payload.Dimensions = f.dim
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("embed: furls: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("embed: furls: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if f.apiKey != "" {
		req.Header.Set("x-api-key", f.apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("embed: furls: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, furlsErrorBodyCap))
		return nil, nil, fmt.Errorf("embed: furls: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var parsed struct {
		Results []furlsResult `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, furlsResponseCap)).Decode(&parsed); err != nil {
		return nil, nil, fmt.Errorf("embed: furls: decode response: %w", err)
	}
	byID := make(map[string]furlsResult, len(parsed.Results))
	for _, r := range parsed.Results {
		byID[r.ID] = r
	}
	vecs := make([][]float64, len(entries))
	var failures []furlsFailure
	for i, e := range entries {
		r, ok := byID[e.ID]
		if !ok && i < len(parsed.Results) {
			r = parsed.Results[i]
		}
		if len(r.Embedding) > 0 {
			vecs[i] = r.Embedding
			continue
		}
		failures = append(failures, furlsFailure{index: i, errorCode: r.ErrorCode, message: r.Error})
	}
	return vecs, failures, nil
}
