package producttext

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

type workerFixture struct {
	t      *testing.T
	ctx    context.Context
	cfg    config.ProductTextConfig
	store  *graph.Store
	driver neo4j.Driver
	prefix string
	vecIDs []string
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()
	st := graphtest.Store(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	inFlight, err := st.InFlight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg := productTextConfig()
	// Earlier graph tests may leave in-flight documents on the shared
	// instance. Measure the gate relative to that pre-existing count.
	cfg.EmbedMaxIngest += len(inFlight)
	driver, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	f := &workerFixture{t: t, ctx: ctx, cfg: cfg, store: st, driver: driver, prefix: fmt.Sprintf("t2-%d-", time.Now().UnixNano())}
	t.Cleanup(func() {
		defer driver.Close(context.Background())
		f.raw(`MATCH (v:TextVec) WHERE v.vec_id IN $ids SET v.embedding = null DETACH DELETE v`, map[string]any{"ids": f.vecIDs})
		f.raw(`MATCH (v:TextVecFailure) WHERE v.id IN $ids DELETE v`, map[string]any{"ids": f.vecIDs})
		f.raw(`MATCH (n) WHERE n.id STARTS WITH $prefix OR ((n:ProductText OR n:ProductStore) AND n.store STARTS WITH $prefix) DETACH DELETE n`,
			map[string]any{"prefix": f.prefix})
		if f.hasIndex() {
			f.raw("DROP VECTOR INDEX text_vec", nil)
		}
	})
	return f
}

func (f *workerFixture) raw(q string, params map[string]any) []*neo4j.Record {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := f.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer s.Close(ctx)
	res, err := s.Run(ctx, q, params)
	if err != nil {
		f.t.Fatal(err)
	}
	rows, err := res.Collect(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

func (f *workerFixture) hasIndex() bool {
	f.t.Helper()
	for _, r := range f.raw("SHOW VECTOR INDEX INFO", nil) {
		if name, _ := r.Get("index_name"); name == "text_vec" {
			return true
		}
	}
	return false
}

func (f *workerFixture) seed(storeSuffix string, suffixes ...string) []string {
	f.t.Helper()
	store := f.prefix + storeSuffix
	f.raw(`CREATE (:ProductStore {store:$store, text_version:1})`, map[string]any{"store": store})
	texts := make([]string, len(suffixes))
	for i, suffix := range suffixes {
		text := f.prefix + suffix
		texts[i] = text
		vecID := productobs.ProductTextVecID(text)
		f.vecIDs = append(f.vecIDs, vecID)
		f.raw(`
CREATE (:Product {id:$product, store:$store})-[:HAS_TEXT {key:'material', truncated:false}]->
       (:ProductText {id:$id, vec_id:$vec, store:$store, text:$text})`,
			map[string]any{
				"id": productobs.ProductTextID(store, text), "product": fmt.Sprintf("%s-%d", store, i),
				"vec": vecID, "store": store, "text": text,
			})
	}
	return texts
}

func (f *workerFixture) vectors() int {
	f.t.Helper()
	n, err := f.store.CountTextVecs(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func workerMetric(t *testing.T, metrics *obs.Metrics, name string, labelValues ...string) float64 {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "gkg_product_text_"+name {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := m.GetLabel()
			vals := make([]string, len(labels))
			for i, label := range labels {
				vals[i] = label.GetValue()
			}
			if slices.Equal(vals, labelValues) {
				if m.Gauge != nil {
					return m.Gauge.GetValue()
				}
				return m.Counter.GetValue()
			}
		}
	}
	return -1
}

func startWorker(t *testing.T, w *Worker) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop after cancellation")
		}
	}
	t.Cleanup(stop)
	return stop
}

func waitForWorker(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("worker condition was not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkerBackfillAndStoreReuse(t *testing.T) {
	f := newWorkerFixture(t)
	f.seed("first", "cotton", "linen", "wool")
	emb := newFurlsFake(t, 2, 768, nil)
	metrics := obs.New()
	w := NewWorker(f.cfg, f.store, emb.Furls, metrics)
	if !w.prepare(f.ctx) {
		t.Fatal("worker failed to prepare")
	}
	w.poll(f.ctx)
	w.poll(f.ctx)
	if got := f.vectors(); got != 3 || len(emb.calls()) != 2 {
		t.Fatalf("vectors=%d, requests=%v", got, emb.calls())
	}
	if workerMetric(t, metrics, "pending") != 0 ||
		workerMetric(t, metrics, "coverage") != 1 ||
		workerMetric(t, metrics, "embedded_total") != 3 ||
		workerMetric(t, metrics, "last_success_timestamp_seconds") <= 0 {
		t.Fatal("backfill metrics did not record three embeddings and full coverage")
	}
	f.seed("second", "cotton")
	w.poll(f.ctx)
	if got := f.vectors(); got != 3 || len(emb.calls()) != 2 || workerMetric(t, metrics, "coverage") != 1 {
		t.Fatalf("duplicate store text: vectors=%d, requests=%v", got, emb.calls())
	}
}

func TestWorkerPersistsEntryBackoff(t *testing.T) {
	f := newWorkerFixture(t)
	texts := f.seed("first", "good", "bad")
	emb := newFurlsFake(t, 16, 768, func(_ *http.Request, _ []string, call int) (int, string) {
		if call <= 2 {
			return 0, texts[1]
		}
		return 0, ""
	})
	metrics := obs.New()
	w := NewWorker(f.cfg, f.store, emb.Furls, metrics)
	if !w.prepare(f.ctx) {
		t.Fatal("worker failed to prepare")
	}
	w.poll(f.ctx)
	w.poll(f.ctx)
	if f.vectors() != 1 || len(emb.calls()) != 1 || workerMetric(t, metrics, "failed_total") != 1 ||
		workerMetric(t, metrics, "pending") != 1 || workerMetric(t, metrics, "coverage") != 0.5 {
		t.Fatal("failure did not isolate one entry or suppress a premature retry")
	}
	params := map[string]any{"id": productobs.ProductTextVecID(texts[1])}
	failure := func(attempts int64, delay time.Duration) {
		t.Helper()
		rows := f.raw(`MATCH (f:TextVecFailure {id:$id})
RETURN f.attempts AS attempts, f.error_code AS code, f.next_at AS next_at`, params)
		if len(rows) != 1 {
			t.Fatalf("failure row count = %d", len(rows))
		}
		got, _ := rows[0].Get("attempts")
		code, _ := rows[0].Get("code")
		nextAt, _ := rows[0].Get("next_at")
		wait := nextAt.(int64) - time.Now().UnixMilli()
		if got != attempts || code != "UPSTREAM_ERROR" || wait > delay.Milliseconds() ||
			wait < (delay-5*time.Second).Milliseconds() {
			t.Fatalf("failure state = %v", rows[0])
		}
	}
	failure(1, 30*time.Second)
	f.raw(`MATCH (f:TextVecFailure {id:$id}) SET f.attempts=12, f.next_at=0`, params)
	w.poll(f.ctx)
	failure(13, 24*time.Hour)
	f.raw(`MATCH (f:TextVecFailure {id:$id}) SET f.next_at=0`, params)
	w.poll(f.ctx)
	calls := emb.calls()
	if len(calls) != 3 || !slices.Equal(calls[1].texts, texts[1:]) || !slices.Equal(calls[2].texts, texts[1:]) ||
		f.vectors() != 2 || workerMetric(t, metrics, "failed_total") != 2 || workerMetric(t, metrics, "retried_total") != 2 {
		t.Fatalf("entry retry did not complete: %v", calls)
	}
	if rows := f.raw(`MATCH (f:TextVecFailure {id:$id}) RETURN f.id`, params); len(rows) != 0 {
		t.Fatal("successful retry left a failure record")
	}
}

func TestWorkerRestartMidBatch(t *testing.T) {
	f := newWorkerFixture(t)
	f.seed("first", "one", "two", "three", "four")
	blocked := make(chan struct{})
	emb := newFurlsFake(t, 2, 768, func(r *http.Request, _ []string, call int) (int, string) {
		if call == 2 {
			close(blocked)
			<-r.Context().Done()
			return 499, ""
		}
		return 0, ""
	})
	cfg := f.cfg
	stop := startWorker(t, NewWorker(cfg, f.store, emb.Furls, obs.New()))
	select {
	case <-blocked:
	case <-time.After(8 * time.Second):
		t.Fatal("second batch did not start")
	}
	stop()
	if got := f.vectors(); got != 2 {
		t.Fatalf("interrupted request wrote a partial batch: %d vectors", got)
	}
	w := NewWorker(cfg, f.store, emb.Furls, obs.New())
	if !w.prepare(f.ctx) {
		t.Fatal("restarted worker failed to prepare")
	}
	w.poll(f.ctx)
	calls := emb.calls()
	if len(calls) != 3 || f.vectors() != 4 {
		t.Fatalf("restart left %d vectors after %d calls", f.vectors(), len(calls))
	}
	slices.Sort(calls[1].texts)
	slices.Sort(calls[2].texts)
	if !slices.Equal(calls[1].texts, calls[2].texts) {
		t.Fatalf("restart embedded %v, interrupted batch was %v", calls[2].texts, calls[1].texts)
	}
}

func TestWorkerAdmissionCapacity(t *testing.T) {
	f := newWorkerFixture(t)
	f.seed("first", "one", "two", "three")
	emb := newFurlsFake(t, 16, 768, nil)
	cfg := f.cfg
	cfg.MaxVectors = 2
	metrics := obs.New()
	w := NewWorker(cfg, f.store, emb.Furls, metrics)
	if !w.prepare(f.ctx) {
		t.Fatal("worker failed to prepare")
	}
	w.poll(f.ctx)
	w.poll(f.ctx)
	calls := emb.calls()
	if len(calls) != 1 || len(calls[0].texts) != 2 || f.vectors() != 2 ||
		workerMetric(t, metrics, "worker_state", "", "capacity") != 1 ||
		workerMetric(t, metrics, "pending") != 1 {
		t.Fatalf("capacity not enforced: vectors=%d, calls=%v, state=%s", f.vectors(), calls, w.state)
	}
}

func TestWorkerUnavailable(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		name := "unsupported_space"
		if mismatch {
			name = "index_unavailable"
		}
		t.Run(name, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.seed("first", "one")
			dim := 8
			if mismatch {
				dim = 768
				f.raw(`CREATE VECTOR INDEX text_vec ON :TextVec(embedding)
WITH CONFIG {"dimension": 8, "capacity": 32768, "metric": "cos", "scalar_kind": "f32"}`, nil)
			}
			emb := newFurlsFake(t, 16, dim, nil)
			metrics := obs.New()
			w := NewWorker(f.cfg, f.store, emb.Furls, metrics)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			w.Run(ctx)
			if f.hasIndex() != mismatch || len(emb.calls()) != 0 || f.vectors() != 0 ||
				workerMetric(t, metrics, "worker_state", name, "unavailable") != 1 {
				t.Fatalf("unavailable worker changed graph: vectors=%d, calls=%v, state=%s", f.vectors(), emb.calls(), w.state)
			}
		})
	}
}

func TestWorkerIngestGate(t *testing.T) {
	f := newWorkerFixture(t)
	f.seed("first", "one", "two")
	firstRequest, releaseFirst := make(chan struct{}), make(chan struct{})
	emb := newFurlsFake(t, 1, 768, func(r *http.Request, _ []string, call int) (int, string) {
		if call == 1 {
			close(firstRequest)
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
			}
		}
		return 0, ""
	})
	metrics := obs.New()
	stop := startWorker(t, NewWorker(f.cfg, f.store, emb.Furls, metrics))
	select {
	case <-firstRequest:
	case <-time.After(8 * time.Second):
		t.Fatal("first embedding request did not start")
	}
	f.raw(`UNWIND range(0,2) AS i
CREATE (:Document {id:$prefix+toString(i), status:'queued'})`, map[string]any{"prefix": f.prefix})
	close(releaseFirst)
	waitForWorker(t, func() bool { return workerMetric(t, metrics, "worker_state", "ingest", "paused") == 1 })
	if len(emb.calls()) != 1 || f.vectors() != 1 {
		t.Fatal("a second request went out while ingest was above the threshold")
	}
	f.raw(`MATCH (d:Document {id:$doc}) SET d.status='done'`, map[string]any{"doc": f.prefix + "0"})
	waitForWorker(t, func() bool { return workerMetric(t, metrics, "coverage") == 1 })
	stop()
	if len(emb.calls()) != 2 || f.vectors() != 2 {
		t.Fatal("worker did not resume at the ingest threshold")
	}
}
