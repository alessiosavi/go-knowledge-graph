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

// sidecarBatch is the maximum number of chunks per provider call; the
// service also batches internally, but client-side batching keeps each
// request's timeout budget bounded to one batch.
const sidecarBatch = 8

// Sidecar talks to the local GLiNEREL extraction service over the frozen
// HTTP contract (design §5.4). Extraction is health-gated: an unhealthy
// sidecar produces a provider error before any extraction request reaches
// the model.
type Sidecar struct {
	baseURL string
	model   string
	client  *http.Client
}

// NewSidecar builds the provider from the Extractor.Sidecar configuration.
func NewSidecar(cfg config.Config) *Sidecar {
	base := strings.TrimRight(cfg.Extractor.Sidecar.URL, "/")
	if base == "" {
		base = "http://localhost:8090"
	}
	model := cfg.Extractor.Sidecar.Model
	if model == "" {
		model = "glinerel-314m"
	}
	timeout := cfg.Extractor.Sidecar.TimeoutS
	if timeout <= 0 {
		timeout = 300
	}
	return &Sidecar{
		baseURL: base,
		model:   model,
		client:  &http.Client{Timeout: time.Duration(timeout) * time.Second},
	}
}

// Name implements Provider.
func (s *Sidecar) Name() string { return "sidecar" }

// Version implements Provider.
func (s *Sidecar) Version() string { return "sidecar:" + s.model }

type sidecarLabel struct {
	Label     string  `json:"label"`
	Threshold float64 `json:"threshold"`
}

type sidecarRequest struct {
	Chunks    []RequestChunk `json:"chunks"`
	Entities  []sidecarLabel `json:"entities"`
	Relations []sidecarLabel `json:"relations"`
}

// Extract implements Provider: health check, then POST /v1/extract in
// batches of at most sidecarBatch chunks (the service batches internally
// as well), then contract validation. The result slice always carries one
// entry per submitted chunk, in submitted order (model omissions become
// empty results; unknown chunk ids drop). Empty requests short-circuit
// with no HTTP call.
func (s *Sidecar) Extract(ctx context.Context, req Request) ([]ChunkResult, error) {
	if len(req.Chunks) == 0 {
		return nil, nil
	}
	if err := s.healthy(ctx); err != nil {
		return nil, err
	}
	out := make([]ChunkResult, 0, len(req.Chunks))
	for start := 0; start < len(req.Chunks); start += sidecarBatch {
		end := min(start+sidecarBatch, len(req.Chunks))
		results, err := s.extractBatch(ctx, req, req.Chunks[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, results...)
	}
	return out, nil
}

// extractBatch posts one batch and validates its results.
func (s *Sidecar) extractBatch(ctx context.Context, req Request, chunks []RequestChunk) ([]ChunkResult, error) {
	body, err := json.Marshal(sidecarRequest{
		Chunks:    chunks,
		Entities:  wireLabels(req.Entities),
		Relations: wireLabels(req.Relations),
	})
	if err != nil {
		return nil, fmt.Errorf("extract sidecar: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/v1/extract", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("extract sidecar: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("extract sidecar: request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("extract sidecar: read response: %w", err)
	}
	if len(data) > 32<<20 {
		return nil, fmt.Errorf("extract sidecar: response exceeds the %d MiB cap", 32)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("extract sidecar: HTTP %d: %s", resp.StatusCode, previewContent(string(data)))
	}
	var wire wireResponse
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("extract sidecar: decode response: %w (content preview: %q)", err, previewContent(string(data)))
	}

	entityNames := labelNames(req.Entities)
	relationNames := labelNames(req.Relations)
	entitySet := make(map[string]bool, len(req.Entities))
	for _, l := range req.Entities {
		entitySet[l.Name] = true
	}
	relationSet := make(map[string]bool, len(req.Relations))
	for _, l := range req.Relations {
		relationSet[l.Name] = true
	}
	byID := make(map[string]wireChunk, len(wire.Results))
	for _, wc := range wire.Results {
		byID[wc.ChunkID] = wc
	}
	out := make([]ChunkResult, 0, len(chunks))
	for _, c := range chunks {
		wc, ok := byID[c.ID]
		if !ok {
			wc = wireChunk{ChunkID: c.ID}
		}
		// Map the sidecar's answer (label text or name, any case) back to the
		// stored name, then validate against the requested names: a label
		// that was never requested is dropped, as before label text existed.
		for i, e := range wc.Entities {
			if name, ok := entityNames[strings.ToLower(strings.TrimSpace(e.Label))]; ok {
				wc.Entities[i].Label = name
			}
		}
		for i, r := range wc.Relations {
			if name, ok := relationNames[strings.ToLower(strings.TrimSpace(r.Label))]; ok {
				wc.Relations[i].Label = name
			}
		}
		out = append(out, validateChunkResult(c.Text, wc, entitySet, relationSet))
	}
	return out, nil
}

// healthy performs the pre-extraction health gate, surfacing the service's
// diagnostic body (e.g. "model load failed: ...") on failure.
func (s *Sidecar) healthy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("extract sidecar: build health request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("extract sidecar: unhealthy: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("extract sidecar: unhealthy: HTTP %d: %s", resp.StatusCode, previewContent(string(data)))
	}
	return nil
}

func wireLabels(labels []Label) []sidecarLabel {
	out := make([]sidecarLabel, 0, len(labels))
	for _, l := range labels {
		text := l.Text
		if text == "" {
			text = l.Name
		}
		out = append(out, sidecarLabel{Label: text, Threshold: l.Threshold})
	}
	return out
}

// labelNames accepts both the sidecar's text and the stored name as answers.
// Entity and relation maps stay separate because their vocabularies may overlap.
func labelNames(labels []Label) map[string]string {
	names := make(map[string]string, len(labels)*2)
	for _, l := range labels {
		names[strings.ToLower(strings.TrimSpace(l.Name))] = l.Name
		if l.Text != "" {
			names[strings.ToLower(strings.TrimSpace(l.Text))] = l.Name
		}
	}
	return names
}
