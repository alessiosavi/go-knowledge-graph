package judge

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

const backfillLeaseName = "judge-backfill"
const backfillLeaseTTL = 120 * time.Second
const backfillRenewEvery = 30 * time.Second

var errBackfillBudget = errors.New("judge: backfill attempt budget reached")
var errBackfillLeaseLost = errors.New("judge: backfill lease lost")

type BackfillOptions struct {
	Limit, BudgetCalls int
	Domain             string
	Force              bool
}
type BackfillResult struct {
	Judged, Confirmed, Unclear, Refuted, Vanished, Orphaned, Failed, CacheHits, Attempts int
	StoppedForBudget                                                                     bool
}

type backfillCacheMetrics struct {
	result   *BackfillResult
	previous ingest.JudgeCacheMetrics
}

func (m backfillCacheMetrics) JudgeCache(result string) {
	if result == "hit" {
		m.result.CacheHits++
	}
	if m.previous != nil {
		m.previous.JudgeCache(result)
	}
}

// Backfill judges only unjudged candidates. Cached persists each verdict before
// the hash-guarded application, allowing recovery without repeat provider calls.
// j must wrap gate; both clients are exclusively owned by this run.
func Backfill(ctx context.Context, st *graph.Store, j *Cached, gate *Client, opt BackfillOptions) (result BackfillResult, err error) {
	if opt.Limit < 0 || opt.BudgetCalls < 0 {
		return result, fmt.Errorf("judge: limit and budget-calls must be non-negative")
	}
	if st == nil || j == nil || gate == nil || j.inner != gate {
		return result, fmt.Errorf("judge: backfill needs a store and cache wrapping the attempt-gated client")
	}
	owner := rand.Text()
	ok, err := st.AcquireLease(ctx, backfillLeaseName, owner, backfillLeaseTTL, opt.Force)
	if err != nil {
		return result, err
	}
	if !ok {
		return result, fmt.Errorf("judge: backfill lease is held by another owner (use --force to take over)")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(backfillRenewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if renewErr := renewBackfillLease(ctx, st, owner); renewErr != nil {
					cancel(renewErr)
					return
				}
			}
		}
	}()
	defer func() {
		cancel(nil)
		<-done
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		err = errors.Join(err, st.ReleaseLease(cleanup, backfillLeaseName, owner))
	}()
	oldGate, oldMetrics := gate.attemptGate, j.metrics
	gate.SetAttemptGate(func() error {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if opt.BudgetCalls > 0 && result.Attempts >= opt.BudgetCalls {
			return errBackfillBudget
		}
		result.Attempts++
		return nil
	})
	j.SetCacheMetrics(backfillCacheMetrics{&result, oldMetrics})
	defer func() { gate.SetAttemptGate(oldGate); j.SetCacheMetrics(oldMetrics) }()
	candidates, err := st.UnjudgedCandidates(ctx, opt.Domain, opt.Limit)
	if err != nil {
		return result, err
	}
	for _, c := range candidates {
		if cause := context.Cause(ctx); cause != nil {
			return result, cause
		}
		if err := renewBackfillLease(ctx, st, owner); err != nil {
			return result, err
		}
		in, err := ingest.DecodeJudgeInput([]byte(c.JudgeInput))
		if err != nil {
			result.Failed++
			slog.Warn("judge: backfill candidate failed", "id", c.ID, "err", err)
			continue
		}
		// DocID is ownership metadata, absent from the canonical judge payload.
		in.DocID = c.DocID
		verdict, err := j.VerifyFact(ctx, in)
		if errors.Is(err, errBackfillBudget) {
			result.StoppedForBudget = true
			return result, nil
		}
		if cause := context.Cause(ctx); cause != nil {
			return result, cause
		}
		// A failed provider response must not hide a forced lease takeover.
		if leaseErr := renewBackfillLease(ctx, st, owner); leaseErr != nil {
			return result, leaseErr
		}
		if err != nil {
			result.Failed++
			slog.Warn("judge: backfill candidate failed", "id", c.ID, "err", err)
			continue
		}
		result.Judged++
		var applied bool
		switch verdict.Status {
		case "confirmed":
			applied, err = st.PromoteCandidate(ctx, c.ID, c.InputHash)
		case "unclear":
			applied, err = st.SetCandidateJudge(ctx, c.ID, c.InputHash, verdict.Status)
		case "refuted":
			applied, err = st.RefuteCandidate(ctx, c.ID, c.InputHash, graph.FactAudit{
				HeadKey: c.HeadKey, Rel: c.Relation, TailKey: c.TailKey, DocID: c.DocID, ChunkID: c.ChunkID,
				Score: c.Confidence, JudgeVerdict: verdict.Status, Reason: verdict.Reason, ExtractorVersion: c.ExtractorVersion,
			})
		default:
			err = fmt.Errorf("judge: invalid cached verdict status %q", verdict.Status)
		}
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return result, cause
			}
			result.Failed++
			slog.Warn("judge: backfill candidate failed", "id", c.ID, "err", err)
			continue
		}
		if !applied {
			if verdict.Status == "confirmed" {
				orphaned, err := st.DeleteOrphanedCandidate(ctx, c.ID, c.InputHash)
				if err != nil {
					if cause := context.Cause(ctx); cause != nil {
						return result, cause
					}
					result.Failed++
					slog.Warn("judge: backfill candidate failed", "id", c.ID, "err", err)
					continue
				}
				if orphaned {
					result.Orphaned++
					continue
				}
			}
			result.Vanished++
			continue
		}
		switch verdict.Status {
		case "confirmed":
			result.Confirmed++
		case "unclear":
			result.Unclear++
		case "refuted":
			result.Refuted++
		}
	}
	return result, nil
}

func renewBackfillLease(ctx context.Context, st *graph.Store, owner string) error {
	ok, err := st.RenewLease(ctx, backfillLeaseName, owner, backfillLeaseTTL)
	if err != nil {
		return err
	}
	if !ok {
		return errBackfillLeaseLost
	}
	return nil
}
