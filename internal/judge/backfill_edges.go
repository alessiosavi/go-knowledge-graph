package judge

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const edgeBackfillPageSize = 500

var ErrBackfillLeaseRefused = errors.New("judge: edge backfill lease refused")

type EdgeBackfillResult struct {
	Selected, Invalid, Judged, CacheHits, Attempts int
	Confirmed, Unclear, Refuted, Failed, Discarded int
	StoppedForBudget, StoppedProvider, LeaseLost   bool
	Model                                          string
	ProviderStop                                   error
}

// NewEdgeBackfillClient checks the effective HTTP endpoint before a run can
// acquire the lease. An explicitly slash-only base is a configuration error.
func NewEdgeBackfillClient(cfg config.Config) (*Client, error) {
	client := NewClient(cfg)
	u, err := url.Parse(client.baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		(cfg.Judge.BaseURL != "" && strings.TrimRight(cfg.Judge.BaseURL, "/") == "") {
		return nil, errors.New("judge: edge backfill requires a valid HTTP(S) judge base URL")
	}
	return client, nil
}

// BackfillEdges replays stored v1 inputs from pending typed edges. The clients
// must be exclusively owned by this run, as with candidate Backfill.
func BackfillEdges(ctx context.Context, st *graph.Store, j *Cached, gate *Client, opt BackfillOptions) (result EdgeBackfillResult, err error) {
	if opt.Limit < 0 || opt.BudgetCalls < 0 {
		return result, fmt.Errorf("judge: limit and budget-calls must be non-negative")
	}
	if st == nil || j == nil || gate == nil || j.inner != gate {
		return result, fmt.Errorf("judge: backfill needs a store and cache wrapping the attempt-gated client")
	}
	result.Model = gate.Model()
	excludes := []string{graph.ValidityApplyLease, graph.RejudgeLeaseName}
	owner := rand.Text()
	ok, err := st.AcquireLeaseExcluding(ctx, backfillLeaseName, owner, backfillLeaseTTL, opt.Force, excludes...)
	if errors.Is(err, graph.ErrLeaseExcluded) {
		return result, fmt.Errorf("%w: %w", ErrBackfillLeaseRefused, err)
	}
	if err != nil {
		return result, err
	}
	if !ok {
		return result, fmt.Errorf("%w: held by another owner (use --force to take over)", ErrBackfillLeaseRefused)
	}
	ctx, release := maintainBackfillLease(ctx, st, owner, func(ctx context.Context) error {
		return st.RenewLeaseExcluding(ctx, backfillLeaseName, owner, backfillLeaseTTL, excludes...)
	})
	defer func() {
		result.LeaseLost = errors.Is(err, graph.ErrLeaseLost)
		err = errors.Join(err, release())
	}()

	oldGate, oldMetrics := gate.attemptGate, j.metrics
	gate.SetAttemptGate(func() error {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if err := st.CheckLeaseExcluding(ctx, backfillLeaseName, owner, excludes...); err != nil {
			return edgeAttemptDenied{err}
		}
		if opt.BudgetCalls > 0 && result.Attempts >= opt.BudgetCalls {
			return errBackfillBudget
		}
		result.Attempts++
		return nil
	})
	j.SetCacheMetrics(backfillCacheMetrics{&result.CacheHits, oldMetrics})
	defer func() { gate.SetAttemptGate(oldGate); j.SetCacheMetrics(oldMetrics) }()

	apply := graph.StoredFactVerdictOptions{
		RequireDone: true,
		Domain:      opt.Domain,
		BeforeWrite: func(ctx context.Context, tx neo4j.ManagedTransaction) error {
			return graph.FenceLeaseExcluding(ctx, tx, backfillLeaseName, owner, backfillLeaseTTL, excludes...)
		},
	}
	var cursor *graph.StoredFactRef
	for opt.Limit == 0 || result.Selected < opt.Limit {
		if cause := context.Cause(ctx); cause != nil {
			return result, cause
		}
		size := edgeBackfillPageSize
		if opt.Limit > 0 {
			size = min(size, opt.Limit-result.Selected)
		}
		edges, err := st.PendingStoredJudgeEdges(ctx, opt.Domain, cursor, size)
		if cause := context.Cause(ctx); cause != nil {
			return result, cause
		}
		if err != nil {
			return result, err
		}
		for _, edge := range edges {
			result.Selected++
			if err := backfillStoredEdge(ctx, st, j, edge, apply, &result); err != nil || result.StoppedForBudget || result.StoppedProvider {
				return result, err
			}
		}
		if len(edges) < size {
			break
		}
		cursor = &edges[len(edges)-1].StoredFactRef
	}
	return result, nil
}

func backfillStoredEdge(ctx context.Context, st *graph.Store, j *Cached, edge graph.StoredJudgeEdge, apply graph.StoredFactVerdictOptions, result *EdgeBackfillResult) error {
	payload := ingest.JudgePayload{Bytes: []byte(edge.JudgeInput), InputHash: edge.InputHash}
	verdict, err := j.verifyPayload(ctx, payload, edge.DocID, verdictCachePolicy{detachedWrite: true})
	if err == nil {
		switch verdict.Status {
		case "confirmed", "unclear", "refuted":
			result.Judged++
		default:
			err = fmt.Errorf("judge: invalid cached verdict status")
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	switch {
	case errors.Is(err, errBackfillBudget):
		result.StoppedForBudget = true
		return nil
	case errors.Is(err, ingest.ErrInvalidJudgePayload):
		result.Invalid++
		return nil
	case IsProviderStop(err):
		result.StoppedProvider = true
		result.ProviderStop = err
		return nil
	case err != nil:
		// A failed attempt gate must stop the run, including graph errors.
		var denied edgeAttemptDenied
		if errors.As(err, &denied) {
			return denied.error
		}
		result.Failed++
		slog.Warn("judge: backfill edge failed", "doc", edge.DocID, "head", edge.HeadKey, "rel", edge.Rel,
			"tail", edge.TailKey, "error_class", "row_specific", "err", err)
		return nil
	}

	applied, err := st.ApplyStoredFactVerdict(ctx, edge.StoredFactRef, verdict.Status, verdict.Reason, apply)
	if err != nil {
		return errors.Join(err, context.Cause(ctx))
	}
	if !applied {
		result.Discarded++
		return nil
	}
	switch verdict.Status {
	case "confirmed":
		result.Confirmed++
	case "unclear":
		result.Unclear++
	case "refuted":
		result.Refuted++
	}
	return nil
}

type edgeAttemptDenied struct{ error }
