package search

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type timedQueryEmbedder struct {
	calls atomic.Int32
}

func (e *timedQueryEmbedder) EmbedQuery(ctx context.Context, query string) ([]float64, error) {
	e.calls.Add(1)
	if query != "ceramic" {
		return nil, errors.New("wrong query")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return []float64{1}, nil
	}
}

func TestSearchQueryEmbeddingIndependentWaiters(t *testing.T) {
	for _, first := range []string{"entity", "meaning"} {
		t.Run(first, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				short, cancelShort := context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancelShort()
				emb := &timedQueryEmbedder{}
				future := StartQueryEmbedding(ctx, emb, "ceramic")
				entityCtx, meaningCtx := ctx, short
				if first == "entity" {
					entityCtx, meaningCtx = short, ctx
				}
				entityErr := make(chan error, 1)
				go func() {
					vec, err := future.EmbedQuery(entityCtx, "ceramic")
					if first == "meaning" && !slices.Equal(vec, []float64{1}) {
						t.Errorf("entity vector = %v, want [1]", vec)
					}
					entityErr <- err
				}()
				meaningVec, meaningErr := future.Wait(meaningCtx)
				if first == "entity" {
					if err := <-entityErr; !errors.Is(err, context.DeadlineExceeded) ||
						meaningErr != nil || !slices.Equal(meaningVec, []float64{1}) {
						t.Fatalf("entity error = %v, meaning vector = %v, meaning error = %v", err, meaningVec, meaningErr)
					}
				} else if err := <-entityErr; !errors.Is(meaningErr, context.DeadlineExceeded) || err != nil {
					t.Fatalf("meaning error = %v, entity error = %v", meaningErr, err)
				}
				if n := emb.calls.Load(); n != 1 {
					t.Errorf("EmbedQuery calls = %d, want 1", n)
				}
			})
		})
	}
}
