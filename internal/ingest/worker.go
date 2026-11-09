package ingest

import (
	"context"
	"log/slog"
)

// Worker is the in-process worker pool: it consumes submitted document ids
// and runs Pipeline.Process for each. Job state lives on the documents
// themselves, so a restart requeues in-flight work via Pipeline.Recover.
type Worker struct {
	Workers  int
	Pipeline *Pipeline
}

// Start launches the pool; it returns immediately and runs until ctx is
// cancelled.
func (w *Worker) Start(ctx context.Context) {
	n := w.Workers
	if n <= 0 {
		n = 1
	}
	for i := 0; i < n; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-w.Pipeline.jobs:
					if err := w.Pipeline.Process(ctx, id); err != nil {
						slog.Warn("ingest: processing failed", "doc", id, "err", err)
					}
				}
			}
		}()
	}
}
