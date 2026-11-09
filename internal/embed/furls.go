package embed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
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

// furlsMaxParallel caps the concurrent chunk requests of one EmbedDocuments
// call.
const furlsMaxParallel = 4

// Furls embeds against the asymmetric embeddings service (one Function URL
// per side: embed_product for documents, embed_query for queries). The
// service packs the model-specific prompt prefixes itself — this client
// only ever sends id+text and never reformats the text. With BatchSize > 0
// the document texts are split into batches of that many entries and the
// chunks are dispatched concurrently (bounded), merged back in input order.
// Safe for concurrent use.
type Furls struct {
	productClient *http.Client
	queryClient   *http.Client
	productURL    string
	queryURL      string
	apiKey        string
	model         string
	dim           int
	batchSize     int
}

// NewFurls builds the client from Embedder config; an empty model falls
// back to furlsDefaultModel. Dim > 0 is requested and validated. A
// positive BatchSize caps the entries per request (0 = one request per
// call, up to whatever the caller passes).
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
		batchSize:     cfg.Embedder.BatchSize,
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

// chunkTexts splits texts into consecutive batches of at most size entries;
// a non-positive size (or a small enough input) is a single request.
func chunkTexts(texts []string, size int) [][]string {
	if size <= 0 || len(texts) <= size {
		return [][]string{texts}
	}
	chunks := make([][]string, 0, (len(texts)+size-1)/size)
	for start := 0; start < len(texts); start += size {
		chunks = append(chunks, texts[start:min(start+size, len(texts))])
	}
	return chunks
}

// embed dispatches one logical embedding call: a single request for one
// chunk, or bounded-concurrency chunk requests merged back in input order.
// The error of the first failing chunk in input order is returned.
func (f *Furls) embed(ctx context.Context, client *http.Client, url string, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, fmt.Errorf("embed: no texts to embed")
	}
	chunks := chunkTexts(texts, f.batchSize)
	if len(chunks) == 1 {
		return f.embedChunk(ctx, client, url, texts)
	}
	type outcome struct {
		start int
		vecs  [][]float64
		err   error
	}
	results := make([]outcome, len(chunks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, furlsMaxParallel)
	start := 0
	for i, chunk := range chunks {
		results[i].start = start
		start += len(chunk)
		wg.Add(1)
		go func(i int, chunk []string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i].vecs, results[i].err = f.embedChunk(ctx, client, url, chunk)
		}(i, chunk)
	}
	wg.Wait()
	out := make([][]float64, len(texts))
	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		copy(out[r.start:], r.vecs)
	}
	return out, nil
}

// embedChunk runs the single-request path for one chunk: one JSON POST,
// id-aligned results (position fallback), a single retry of the entries
// whose error_code is retryable, then fail on anything still without a
// vector.
func (f *Furls) embedChunk(ctx context.Context, client *http.Client, url string, texts []string) ([][]float64, error) {
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
		// failures2 is compacted (only entries still failing), so look each
		// one up by its index instead of its ordinal.
		byIndex := make(map[int]furlsFailure, len(failures2))
		for _, fl := range failures2 {
			byIndex[fl.index] = fl
		}
		for i, fl := range failures {
			if vecs2[i] != nil {
				vecs[fl.index] = vecs2[i]
				continue
			}
			code, msg := fl.errorCode, fl.message
			if retryFL, ok := byIndex[i]; ok {
				if retryFL.errorCode != "" {
					code = retryFL.errorCode
				}
				if retryFL.message != "" {
					msg = retryFL.message
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
// id; a slot without its own id is a positional fallback for lenient
// servers, while a slot carrying a different entry's id is never shifted
// (that would attach one entry's vector to another). A present non-empty
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
		// Never echo the endpoint (it is a capability that ends up in logs
		// and pasted evidence): classify the transport failure instead.
		switch {
		case errors.Is(err, context.Canceled):
			return nil, nil, fmt.Errorf("embed: furls: request canceled: %w", context.Canceled)
		case errors.Is(err, context.DeadlineExceeded):
			return nil, nil, fmt.Errorf("embed: furls: request timed out: %w", context.DeadlineExceeded)
		default:
			return nil, nil, errors.New("embed: furls: request failed (endpoint unreachable or invalid)")
		}
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
		if !ok && i < len(parsed.Results) && parsed.Results[i].ID == "" {
			// A slot without an id is safe to use positionally.
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
