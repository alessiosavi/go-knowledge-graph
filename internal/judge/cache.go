package judge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

// RequestKey identifies the complete judge request, model and prompt version.
func RequestKey(judgeInput []byte, model string) string {
	h := sha256.New()
	_, _ = h.Write(judgeInput)
	_, _ = h.Write([]byte("\n" + model + "\n" + PromptVersion))
	return hex.EncodeToString(h.Sum(nil))
}

// VerdictStore persists verdicts independently of derived extraction data.
type VerdictStore interface {
	JudgeVerdict(ctx context.Context, key string) (graph.JudgeVerdict, bool, error)
	PutJudgeVerdict(ctx context.Context, v graph.JudgeVerdict) error
}

// Cached reuses durable verdicts for identical requests.
type Cached struct {
	inner   ingest.FactJudge
	store   VerdictStore
	model   string
	metrics ingest.JudgeCacheMetrics
}

var _ ingest.FactJudge = (*Cached)(nil)

// NewCached wraps a judge with a persistent verdict cache.
func NewCached(inner ingest.FactJudge, st VerdictStore, model string) *Cached {
	return &Cached{inner: inner, store: st, model: model}
}

// SetCacheMetrics attaches the counter of cache results (hit, miss,
// read_error, write_error). Attach it before the first verification.
func (c *Cached) SetCacheMetrics(m ingest.JudgeCacheMetrics) { c.metrics = m }

func (c *Cached) count(result string) {
	if c.metrics != nil {
		c.metrics.JudgeCache(result)
	}
}

// VerifyFact looks up a verdict before calling the provider and persists a
// successful verdict before returning it for application to the graph. The
// cache is an optimization: a failed lookup asks the provider as on a miss,
// and a failed write still returns the verdict that was paid for. Both are
// logged and counted.
func (c *Cached) VerifyFact(ctx context.Context, in ingest.FactInput) (ingest.Verdict, error) {
	payload := ingest.CanonicalJudgeInput(in)
	if payload == nil {
		return ingest.Verdict{}, fmt.Errorf("judge: encode payload: confidence must be finite")
	}
	return c.verify(ctx, RequestKey(payload, c.model), PromptVersion, in.DocID, false, func() (ingest.Verdict, error) {
		return c.inner.VerifyFact(ctx, in)
	})
}

// VerifyPayload validates the stored v1 message before consulting the same
// cache used by VerifyFact. The document ID only identifies the cache owner.
func (c *Cached) VerifyPayload(ctx context.Context, p ingest.JudgePayload, docID string) (ingest.Verdict, error) {
	if err := validatePayload(p); err != nil {
		return ingest.Verdict{}, err
	}
	return c.verify(ctx, RequestKey(p.Bytes, c.model), PromptVersion, docID, false, func() (ingest.Verdict, error) {
		inner, ok := c.inner.(interface {
			VerifyPayload(context.Context, ingest.JudgePayload) (ingest.Verdict, error)
		})
		if !ok {
			return ingest.Verdict{}, fmt.Errorf("judge: wrapped provider does not support VerifyPayload")
		}
		return inner.VerifyPayload(ctx, p)
	})
}

// VerifyRequest checks the frozen profile against the current definitions
// before using the strict cache. A lookup error prevents a provider call. A
// write error returns the paid verdict with the error so the caller can record
// it before stopping.
func (c *Cached) VerifyRequest(ctx context.Context, req Request, definitions map[string]map[string]string) (ingest.Verdict, error) {
	if _, err := req.validate(c.model, definitions); err != nil {
		return ingest.Verdict{}, err
	}
	key := RequestKeyV2(req.RequestBytes, req.Profile.Fingerprint)
	return c.verify(ctx, key, req.Profile.Fingerprint, req.DocID, true, func() (ingest.Verdict, error) {
		inner, ok := c.inner.(interface {
			VerifyRequest(context.Context, Request, map[string]map[string]string) (ingest.Verdict, error)
		})
		if !ok {
			return ingest.Verdict{}, fmt.Errorf("judge: wrapped provider does not support VerifyRequest")
		}
		return inner.VerifyRequest(ctx, req, definitions)
	})
}

func (c *Cached) verify(ctx context.Context, key, promptVersion, docID string, strict bool, call func() (ingest.Verdict, error)) (ingest.Verdict, error) {
	v, ok, err := c.store.JudgeVerdict(ctx, key)
	switch {
	case err != nil && ctx.Err() != nil:
		return ingest.Verdict{}, ctx.Err()
	case err != nil:
		if !strict {
			slog.Warn("judge: verdict cache read failed, asking the provider", "key", key, "err", err)
		}
		c.count("read_error")
		if strict {
			return ingest.Verdict{}, fmt.Errorf("judge: read verdict cache: %w", err)
		}
	case ok:
		c.count("hit")
		return ingest.Verdict{Status: v.Status, Confidence: v.Confidence, Reason: v.Reason}, nil
	default:
		c.count("miss")
	}
	verdict, err := call()
	if err != nil {
		return ingest.Verdict{}, err
	}
	switch verdict.Status {
	case "confirmed", "unclear", "refuted":
	default:
		return ingest.Verdict{}, fmt.Errorf("judge: invalid verdict status %q", verdict.Status)
	}
	if err := c.store.PutJudgeVerdict(ctx, graph.JudgeVerdict{
		Key: key, Status: verdict.Status, Confidence: verdict.Confidence, Reason: verdict.Reason,
		Model: c.model, PromptVersion: promptVersion, DocID: docID, At: time.Now().UTC(),
	}); err != nil {
		if !strict {
			slog.Warn("judge: verdict cache write failed, verdict applied uncached", "key", key, "err", err)
		}
		c.count("write_error")
		if strict {
			return verdict, fmt.Errorf("judge: write verdict cache: %w", err)
		}
	}
	return verdict, nil
}
