package producttext

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/embed"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

type recordedRequest struct {
	texts []string
	start time.Time
}

type furlsFake struct {
	*embed.Furls
	mu       sync.Mutex
	requests []recordedRequest
	active   atomic.Int32
	peak     atomic.Int32
}

func newFurlsFake(t *testing.T, batchSize, dim int, respond func(*http.Request, []string, int) (int, string)) *furlsFake {
	t.Helper()
	f := &furlsFake{}
	vector := make([]float64, dim)
	vector[0] = 1
	type result struct {
		ID        string    `json:"id"`
		Embedding []float64 `json:"embedding,omitempty"`
		ErrorCode string    `json:"error_code,omitempty"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		active := f.active.Add(1)
		defer f.active.Add(-1)
		for peak := f.peak.Load(); active > peak && !f.peak.CompareAndSwap(peak, active); peak = f.peak.Load() {
		}
		var request struct {
			Entries []struct{ ID, Text string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		texts := make([]string, len(request.Entries))
		for i, entry := range request.Entries {
			texts[i] = entry.Text
		}
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{texts, start})
		call := len(f.requests)
		f.mu.Unlock()
		var bad string
		if respond != nil {
			status, entry := respond(r, texts, call)
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			bad = entry
		}
		results := make([]result, len(texts))
		for i, e := range request.Entries {
			if e.Text == bad {
				results[i] = result{ID: e.ID, ErrorCode: "UPSTREAM_ERROR"}
			} else {
				results[i] = result{ID: e.ID, Embedding: vector}
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"results": results}); err != nil && r.Context().Err() == nil {
			t.Error(err)
		}
	}))
	t.Cleanup(ts.Close)
	cfg := config.Config{Embedder: config.EmbedderConfig{Dim: dim, ProductFURL: ts.URL, BatchSize: batchSize}}
	f.Furls = embed.NewFurls(cfg)
	return f
}

func (f *furlsFake) calls() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

type memoryTextGraph struct {
	pending []graph.PendingTextVec
	results []graph.TextVecResult
	ensures int
}

func newMemoryTextGraph(n int) *memoryTextGraph {
	g := &memoryTextGraph{}
	for i := range n {
		s := fmt.Sprintf("t2-text-%03d", i)
		g.pending = append(g.pending, graph.PendingTextVec{VecID: productobs.ProductTextVecID(s), Text: s})
	}
	return g
}

func (g *memoryTextGraph) EnsureTextVecIndex(context.Context) error    { g.ensures++; return nil }
func (g *memoryTextGraph) GCTextVecs(context.Context, time.Time) error { return nil }
func (g *memoryTextGraph) CountTextVecs(context.Context) (int, error)  { return len(g.results), nil }
func (g *memoryTextGraph) TextVecStats(context.Context, string) (graph.TextVecStats, error) {
	return graph.TextVecStats{Referenced: len(g.pending) + len(g.results), Embedded: len(g.results)}, nil
}
func (g *memoryTextGraph) PendingTextVecs(_ context.Context, _ string, _ time.Time, limit int) ([]graph.PendingTextVec, error) {
	return slices.Clone(g.pending[:min(limit, len(g.pending))]), nil
}
func (g *memoryTextGraph) WriteTextVecBatch(_ context.Context, _ string, _ time.Time, batch []graph.TextVecResult) error {
	g.results = append(g.results, batch...)
	for _, result := range batch {
		g.pending = slices.DeleteFunc(g.pending, func(p graph.PendingTextVec) bool { return p.VecID == result.VecID })
	}
	return nil
}
func (g *memoryTextGraph) InFlight(context.Context) ([]graph.Document, error) { return nil, nil }

func productTextConfig() config.ProductTextConfig {
	return config.ProductTextConfig{Search: true, Embed: "run", EmbedMaxIngest: 2, MaxVectors: 65536}
}

func unitWorker(t *testing.T, g *memoryTextGraph, f *furlsFake, pause time.Duration) *Worker {
	t.Helper()
	cfg := productTextConfig()
	cfg.EmbedPause = pause
	w := NewWorker(cfg, g, f.Furls, obs.New())
	if !w.prepare(t.Context()) {
		t.Fatal("worker could not start")
	}
	return w
}

func TestWorkerBatchesOneRequestAtATime(t *testing.T) {
	g := newMemoryTextGraph(256)
	f := newFurlsFake(t, 16, 768, func(*http.Request, []string, int) (int, string) {
		time.Sleep(2 * time.Millisecond)
		return 0, ""
	})
	w := unitWorker(t, g, f, 0)
	w.poll(t.Context())
	calls := f.calls()
	if len(calls) != 16 || f.peak.Load() != 1 || len(g.results) != 256 || len(g.pending) != 0 {
		t.Fatalf("%d requests, peak %d, %d results, %d pending", len(calls), f.peak.Load(), len(g.results), len(g.pending))
	}
	for _, c := range calls {
		if len(c.texts) != 16 {
			t.Fatalf("request size = %d", len(c.texts))
		}
	}
}

func TestWorkerEntryErrorIsolation(t *testing.T) {
	g := newMemoryTextGraph(3)
	bad := g.pending[1].Text
	f := newFurlsFake(t, 16, 768, func(*http.Request, []string, int) (int, string) { return 0, bad })
	w := unitWorker(t, g, f, 0)
	w.poll(t.Context())
	if len(f.calls()) != 1 || len(g.results) != 3 {
		t.Fatalf("%d requests, %d results", len(f.calls()), len(g.results))
	}
	for _, e := range g.results {
		if e.Text == bad {
			if e.ErrorCode != "UPSTREAM_ERROR" || e.Embedding != nil {
				t.Errorf("bad entry received %+v", e)
			}
		} else if e.ErrorCode != "" || len(e.Embedding) != 768 {
			t.Errorf("entry %s failed along with its neighbor", e.Text)
		}
	}
}

func TestWorkerSystemicBackoff(t *testing.T) {
	for _, status := range []int{401, 403, 404, 408, 429, 415, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			g := newMemoryTextGraph(8)
			f := newFurlsFake(t, 4, 768, func(*http.Request, []string, int) (int, string) { return status, "" })
			w := unitWorker(t, g, f, 0)
			if d := w.poll(t.Context()); d != 30*time.Second || w.state != "backoff" {
				t.Fatalf("state=%s, delay=%s", w.state, d)
			}
			if len(f.calls()) != 1 || len(g.pending) != 8 || len(g.results) != 0 {
				t.Fatalf("%d requests, %d pending, %d results", len(f.calls()), len(g.pending), len(g.results))
			}
		})
	}
}

func TestWorkerBisectsContentErrors(t *testing.T) {
	for _, status := range []int{400, 413, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			g := newMemoryTextGraph(8)
			bad := g.pending[3].Text
			f := newFurlsFake(t, 4, 768, func(_ *http.Request, texts []string, call int) (int, string) {
				if status == 413 {
					if call == 1 {
						return status, ""
					}
					return 0, ""
				}
				if slices.Contains(texts, bad) {
					return status, ""
				}
				return 0, ""
			})
			w := unitWorker(t, g, f, 0)
			w.poll(t.Context())
			var sizes []int
			for _, r := range f.calls() {
				sizes = append(sizes, len(r.texts))
			}
			want := []int{4, 2, 2, 1, 1, 4}
			if status == 413 {
				want = []int{4, 2, 2, 2, 2}
			}
			if !slices.Equal(sizes, want) || len(g.results) != 8 {
				t.Fatalf("request sizes %v, want %v; results=%d", sizes, want, len(g.results))
			}
			for _, e := range g.results {
				if status != 413 && e.Text == bad {
					if e.ErrorCode != fmt.Sprintf("HTTP_%d", status) || e.Embedding != nil {
						t.Errorf("bad entry received %+v", e)
					}
				} else if e.ErrorCode != "" || len(e.Embedding) != 768 {
					t.Errorf("expected vector for %s", e.Text)
				}
			}
		})
	}
}

func TestWorkerBreaker(t *testing.T) {
	g := newMemoryTextGraph(3)
	f := newFurlsFake(t, 16, 768, func(_ *http.Request, _ []string, call int) (int, string) {
		statuses := []int{500, 502, 503, 0, 429}
		return statuses[call-1], ""
	})
	w := unitWorker(t, g, f, 0)
	for i, want := range []time.Duration{30 * time.Second, time.Minute, 10 * time.Minute, 0} {
		if d := w.poll(t.Context()); d != want {
			t.Fatalf("poll %d backoff = %s, want %s", i, d, want)
		}
		if got := len(f.calls()); got != i+1 {
			t.Fatalf("poll %d: calls = %d", i, got)
		}
	}
	g.pending = newMemoryTextGraph(1).pending
	if d := w.poll(t.Context()); d != 30*time.Second {
		t.Errorf("backoff after successful request = %s", d)
	}
}

func TestWorkerPacingIncludesBisection(t *testing.T) {
	g := newMemoryTextGraph(3)
	f := newFurlsFake(t, 2, 768, func(_ *http.Request, _ []string, call int) (int, string) {
		if call == 1 {
			return 422, ""
		}
		return 0, ""
	})
	const pause = 25 * time.Millisecond
	w := unitWorker(t, g, f, pause)
	w.poll(t.Context())
	calls := f.calls()
	if len(calls) != 4 {
		t.Fatalf("requests = %d, want 4", len(calls))
	}
	for i := 1; i < len(calls); i++ {
		if elapsed := calls[i].start.Sub(calls[i-1].start); elapsed < pause {
			t.Errorf("pause before request %d = %s, want at least %s", i+1, elapsed, pause)
		}
	}
}

func TestWorkerPauseAndFlagOff(t *testing.T) {
	for _, pause := range []bool{false, true} {
		g := newMemoryTextGraph(1)
		f := newFurlsFake(t, 16, 768, nil)
		cfg := productTextConfig()
		cfg.Search = pause
		want, ensures := "unavailable", 0
		if pause {
			cfg.Embed = "pause"
			want, ensures = "paused", 1
		}
		w := NewWorker(cfg, g, f.Furls, obs.New())
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		w.Run(ctx)
		cancel()
		if w.state != want || len(f.calls()) != 0 || g.ensures != ensures {
			t.Errorf("paused=%t: state=%s, calls=%v, ensures=%d", pause, w.state, f.calls(), g.ensures)
		}
	}
}

type pausedTextGraph struct {
	memoryTextGraph
	stats atomic.Pointer[graph.TextVecStats]
}

func (g *pausedTextGraph) TextVecStats(context.Context, string) (graph.TextVecStats, error) {
	return *g.stats.Load(), nil
}

func TestWorkerPausedCoverage(t *testing.T) {
	f := newFurlsFake(t, 16, 768, nil)
	synctest.Test(t, func(t *testing.T) {
		g := &pausedTextGraph{memoryTextGraph: *newMemoryTextGraph(1)}
		g.stats.Store(&graph.TextVecStats{Referenced: 2, Embedded: 1, Orphaned: 2})
		cfg := productTextConfig()
		cfg.Embed = "pause"
		metrics := obs.New()
		stop := startWorker(t, NewWorker(cfg, g, f.Furls, metrics))
		synctest.Wait()

		checkGauges := func(pending, coverage, orphaned float64) {
			t.Helper()
			for name, want := range map[string]float64{"pending": pending, "coverage": coverage, "orphaned": orphaned} {
				if got := workerMetric(t, metrics, name); got != want {
					t.Errorf("%s = %v, want %v", name, got, want)
				}
			}
		}
		checkGauges(1, 0.5, 2)
		g.stats.Store(&graph.TextVecStats{Referenced: 4, Embedded: 1, Orphaned: 4})
		time.Sleep(59 * time.Second)
		synctest.Wait()
		checkGauges(1, 0.5, 2)
		time.Sleep(time.Second)
		synctest.Wait()
		checkGauges(3, 0.25, 4)
		stop()
		if got := workerMetric(t, metrics, "worker_state", "configured", "paused"); got != 1 || len(f.calls()) != 0 {
			t.Fatalf("paused state=%v, requests=%v", got, f.calls())
		}
	})
}

func TestWorkerCapacityLogsTransitions(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	g := newMemoryTextGraph(0)
	f := newFurlsFake(t, 16, 768, nil)
	w := unitWorker(t, g, f, 0)
	w.cfg.MaxVectors = 1
	g.results = make([]graph.TextVecResult, 1)
	w.poll(t.Context())
	w.poll(t.Context())
	g.results = nil
	w.poll(t.Context())
	w.poll(t.Context())
	for _, message := range []string{"product text worker entered capacity", "product text worker left capacity"} {
		if got := strings.Count(logs.String(), `level=WARN msg="`+message+`"`); got != 1 {
			t.Errorf("warning %q logged %d times; logs: %s", message, got, &logs)
		}
	}
}
