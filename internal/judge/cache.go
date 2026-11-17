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
// read_error, write_error). Attach it before the first VerifyFact.
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
	key := RequestKey(payload, c.model)
	v, ok, err := c.store.JudgeVerdict(ctx, key)
	switch {
	case err != nil && ctx.Err() != nil:
		return ingest.Verdict{}, ctx.Err()
	case err != nil:
		slog.Warn("judge: verdict cache read failed, asking the provider", "key", key, "err", err)
		c.count("read_error")
	case ok:
		c.count("hit")
		return ingest.Verdict{Status: v.Status, Confidence: v.Confidence, Reason: v.Reason}, nil
	default:
		c.count("miss")
	}
	verdict, err := c.inner.VerifyFact(ctx, in)
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
		Model: c.model, PromptVersion: PromptVersion, At: time.Now().UTC(),
	}); err != nil {
		slog.Warn("judge: verdict cache write failed, verdict applied uncached", "key", key, "err", err)
		c.count("write_error")
	}
	return verdict, nil
}
