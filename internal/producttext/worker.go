// Package producttext backfills embeddings for materialized product texts.
package producttext

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/embed"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

const (
	idleInterval           = 30 * time.Second
	ingestInterval         = time.Second
	pausedCoverageInterval = 60 * time.Second
	minBackoff             = 30 * time.Second
	maxBackoff             = 10 * time.Minute
)

type textStore interface {
	EnsureTextVecIndex(context.Context) error
	PendingTextVecs(context.Context, string, time.Time, int) ([]graph.PendingTextVec, error)
	WriteTextVecBatch(context.Context, string, time.Time, []graph.TextVecResult) error
	CountTextVecs(context.Context) (int, error)
	TextVecStats(context.Context, string) (graph.TextVecStats, error)
	GCTextVecs(context.Context, time.Time) error
	InFlight(context.Context) ([]graph.Document, error)
}

// Worker owns one serial embedding loop for a box.
type Worker struct {
	cfg      config.ProductTextConfig
	store    textStore
	provider embed.Provider
	metrics  *obs.Metrics

	furls           *embed.Furls
	batchSize       int
	pendingPageSize int
	state           string
	reason          string
	nextRequest     time.Time
	nextGC          time.Time
	backoff         time.Duration
	serverErrors    int
	retryBatch      map[string]struct{}
}

func NewWorker(cfg config.ProductTextConfig, store textStore, provider embed.Provider, metrics *obs.Metrics) *Worker {
	return &Worker{cfg: cfg, store: store, provider: provider, metrics: metrics, pendingPageSize: graph.MaxPendingTextVecs}
}

// Run prepares the index and runs the worker until ctx is canceled.
// The serve process starts at most one worker for its graph.
func (w *Worker) Run(ctx context.Context) {
	if !w.prepare(ctx) {
		return
	}
	for ctx.Err() == nil {
		wait(ctx, w.poll(ctx))
	}
}

func (w *Worker) prepare(ctx context.Context) bool {
	switch {
	case !w.cfg.Search:
		w.setState(ctx, "unavailable", "search_disabled")
		return false
	case w.provider == nil:
		w.setState(ctx, "unavailable", "embedder_disabled")
		return false
	}
	furls, ok := w.provider.(*embed.Furls)
	if !ok || furls.Space() != graph.ProductTextSpace {
		w.setState(ctx, "unavailable", "unsupported_space")
		slog.WarnContext(ctx, "product text embedding unavailable", "space", w.provider.Space(), "want", graph.ProductTextSpace)
		return false
	}
	if err := w.store.EnsureTextVecIndex(ctx); err != nil {
		w.setState(ctx, "unavailable", "index_unavailable")
		slog.WarnContext(ctx, "product text embedding unavailable", "error", err)
		return false
	}
	if w.cfg.Embed == "pause" {
		w.setState(ctx, "paused", "configured")
		return true
	}
	w.furls = furls
	w.batchSize = furls.BatchOnceLimit()
	w.setState(ctx, "running", "")
	return true
}

func (w *Worker) poll(ctx context.Context) time.Duration {
	if w.cfg.Embed == "pause" {
		if err := w.refreshCoverage(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "product text worker could not measure coverage while paused", "error", err)
		}
		return pausedCoverageInterval
	}

	delay, err := w.sweep(ctx)
	if err == nil || ctx.Err() != nil {
		return delay
	}

	w.backoff = min(maxBackoff, max(minBackoff, w.backoff*2))
	if w.serverErrors >= 3 {
		w.backoff = maxBackoff
	}
	w.setState(ctx, "backoff", "")
	slog.WarnContext(ctx, "product text worker backing off", "error", err, "delay", w.backoff,
		"consecutive_rate_limits_or_server_errors", w.serverErrors)
	return w.backoff
}

func (w *Worker) sweep(ctx context.Context) (time.Duration, error) {
	now := time.Now()
	refresh := !now.Before(w.nextGC)
	if refresh {
		if err := w.store.GCTextVecs(ctx, now); err != nil {
			return 0, err
		}
		w.nextGC = now.Add(idleInterval)
	}
	n, err := w.store.CountTextVecs(ctx)
	if err != nil {
		return 0, err
	}
	room := w.cfg.MaxVectors - n
	pageSize := max(1, min(w.pendingPageSize, graph.MaxPendingTextVecs))
	var pending []graph.PendingTextVec
	if room > 0 {
		pending, err = w.store.PendingTextVecs(ctx, graph.ProductTextSpace, now, pageSize)
		if err != nil {
			return 0, err
		}
		w.setState(ctx, "running", "")
		if err := w.embedBatches(ctx, pending, &room, false); err != nil {
			return 0, err
		}
	}
	// A short page exhausted the scan's eligible texts. Wait for the next
	// idle tick instead of issuing another selection just to confirm it.
	idle := room <= 0 || len(pending) < pageSize
	if refresh || idle {
		if err := w.refreshCoverage(ctx); err != nil {
			return 0, err
		}
	}
	if room <= 0 {
		w.setState(ctx, "capacity", "")
	}
	if idle {
		return idleInterval, nil
	}
	return 0, nil
}

func (w *Worker) refreshCoverage(ctx context.Context) error {
	stats, err := w.store.TextVecStats(ctx, graph.ProductTextSpace)
	if err != nil {
		return err
	}
	w.metrics.ProductTextCoverage(stats.Referenced, stats.Embedded, stats.Orphaned)
	return nil
}

func (w *Worker) embedBatches(ctx context.Context, pending []graph.PendingTextVec, room *int, bisected bool) error {
	for len(pending) > 0 && *room > 0 {
		n := min(len(pending), w.batchSize, *room)
		if err := w.embedBatch(ctx, pending[:n], room, bisected); err != nil {
			return err
		}
		pending = pending[n:]
	}
	return nil
}

func (w *Worker) embedBatch(ctx context.Context, batch []graph.PendingTextVec, room *int, bisected bool) error {
	if err := w.beforeRequest(ctx); err != nil {
		return err
	}
	texts := make([]string, len(batch))
	var retried int
	for i, entry := range batch {
		texts[i] = entry.Text
		_, retry := w.retryBatch[entry.VecID]
		if bisected || retry || entry.Attempts > 0 {
			retried++
		}
		delete(w.retryBatch, entry.VecID)
	}
	w.metrics.ProductTextRetried(retried)
	results, err := w.furls.EmbedBatchOnce(ctx, texts)
	w.nextRequest = time.Now().Add(w.cfg.EmbedPause)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	var httpErr embed.HTTPError
	status := 0
	if errors.As(err, &httpErr) {
		status = httpErr.Status
	}
	if errors.Is(err, embed.ErrRateLimited) || (status >= 500 && status < 600) {
		w.serverErrors++
	} else {
		w.serverErrors = 0
	}
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		w.backoff = 0
		if status == http.StatusRequestEntityTooLarge {
			w.batchSize = max(1, w.batchSize/2)
		}
		if len(batch) == 1 {
			return w.commit(ctx, batch, []embed.EntryResult{{ErrorCode: fmt.Sprintf("HTTP_%d", status)}}, room)
		}
		mid := len(batch) / 2
		if err := w.embedBatches(ctx, batch[:mid], room, true); err != nil {
			return err
		}
		return w.embedBatches(ctx, batch[mid:], room, true)
	}
	if err != nil {
		w.retryBatch = make(map[string]struct{}, len(batch))
		for _, entry := range batch {
			w.retryBatch[entry.VecID] = struct{}{}
		}
		return err
	}
	w.backoff = 0
	return w.commit(ctx, batch, results, room)
}

func (w *Worker) beforeRequest(ctx context.Context) error {
	wait(ctx, time.Until(w.nextRequest))
	for ctx.Err() == nil {
		inFlight, err := w.store.InFlight(ctx)
		if err != nil {
			return err
		}
		if len(inFlight) <= w.cfg.EmbedMaxIngest {
			w.setState(ctx, "running", "")
			break
		}
		w.setState(ctx, "paused", "ingest")
		wait(ctx, ingestInterval)
	}
	return ctx.Err()
}

func (w *Worker) commit(ctx context.Context, batch []graph.PendingTextVec, results []embed.EntryResult, room *int) error {
	type entryError struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	var entryErrors []entryError
	rows := make([]graph.TextVecResult, len(batch))
	var failed int
	for i, entry := range batch {
		r := results[i]
		rows[i] = graph.TextVecResult{PendingTextVec: entry, Embedding: r.Embedding, ErrorCode: r.ErrorCode}
		if r.ErrorCode != "" {
			failed++
			if r.Error != "" && len(entryErrors) < 3 {
				entryErrors = append(entryErrors, entryError{Code: r.ErrorCode, Message: r.Error})
			}
		}
	}
	if len(entryErrors) > 0 {
		slog.WarnContext(ctx, "product text embedding batch had entry errors", "failed", failed, "entry_errors", entryErrors)
	}
	if err := w.store.WriteTextVecBatch(ctx, graph.ProductTextSpace, time.Now(), rows); err != nil {
		return err
	}
	// Only vectors successfully committed since the scan consume admission room.
	embedded := len(batch) - failed
	*room -= embedded
	w.metrics.ProductTextBatch(embedded, failed, time.Now())
	return nil
}

func (w *Worker) setState(ctx context.Context, state, reason string) {
	if w.state == state && w.reason == reason {
		return
	}
	if state != w.state {
		switch {
		case state == "capacity":
			slog.WarnContext(ctx, "product text worker entered capacity", "max_vectors", w.cfg.MaxVectors)
		case w.state == "capacity":
			slog.WarnContext(ctx, "product text worker left capacity", "state", state)
		}
	}
	w.state, w.reason = state, reason
	w.metrics.ProductTextWorkerState(state, reason)
}

func wait(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
