package judge_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

type edgeBackfillFixture struct {
	backfillFixture
}

const fixtureEdges = "MATCH (h:Entity)-[r:uses]->(t:Entity) WHERE r.doc_id = $doc"

func newEdgeBackfillFixture(t *testing.T) edgeBackfillFixture {
	t.Helper()
	f := edgeBackfillFixture{newBackfillFixture(t, 0)}
	backfillQuery(t, "MATCH (d:Document {id:$doc}) SET d.domain=$domain", map[string]any{"doc": f.doc, "domain": f.domain})
	if err := f.st.SetStatus(t.Context(), f.doc, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f edgeBackfillFixture) input(mark string) ingest.JudgePayload {
	payload := ingest.CanonicalJudgeInput(ingest.FactInput{
		Head: "Acme-Max", Tail: "Zorbo", Rel: "uses", Confidence: 0.8,
		Evidence: "Acme-Max uses Zorbo.", DocContext: f.domain + mark,
	})
	return ingest.JudgePayload{Bytes: payload, InputHash: fmt.Sprintf("%x", sha256.Sum256(payload))}
}

func (f edgeBackfillFixture) add(t *testing.T, first, count int, payload ingest.JudgePayload) []graph.StoredFactRef {
	t.Helper()
	refs, err := f.addEdges(t.Context(), first, count, payload)
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

func (f edgeBackfillFixture) addEdges(ctx context.Context, first, count int, payload ingest.JudgePayload) ([]graph.StoredFactRef, error) {
	var refs []graph.StoredFactRef
	rows := make([]map[string]any, 0, count)
	for i := first; i < first+count; i++ {
		head := fmt.Sprintf("%s-edges-head-%04d", f.domain, i)
		ref := graph.StoredFactRef{HeadKey: head, Rel: "uses", TailKey: f.domain + "-edges-tail",
			ChunkID: f.chunk, JudgeGen: fmt.Sprintf("%032x", i+1), InputHash: payload.InputHash}
		rows = append(rows, map[string]any{"head": head, "tail": ref.TailKey, "gen": ref.JudgeGen})
		refs = append(refs, ref)
	}
	err := backfillQueryErr(ctx, `UNWIND $rows AS row
MERGE (h:Entity {key:row.head})
MERGE (t:Entity {key:row.tail})
CREATE (h)-[r:uses]->(t)
SET r.doc_id=$doc, r.chunk_id=$chunk, r.judge='', r.judge_input=$input, r.input_hash=$hash,
    r.judge_gen=row.gen, r.confidence=0.8, r.extractor_version='fixture:1'`,
		map[string]any{"rows": rows, "doc": f.doc, "chunk": f.chunk, "input": string(payload.Bytes), "hash": payload.InputHash})
	return refs, err
}

func (f edgeBackfillFixture) states(t *testing.T) []string {
	t.Helper()
	rows, err := f.st.RunReadQuery(t.Context(), fixtureEdges+" RETURN r.judge AS judge ORDER BY h.key", map[string]any{"doc": f.doc}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	for _, row := range rows.Rows {
		statuses = append(statuses, row["judge"].(string))
	}
	return statuses
}

func edgeConfirmed(w http.ResponseWriter) {
	_, _ = io.WriteString(w, chatBody(`{"verdict":"confirmed"}`))
}

func TestEdgeBackfillCursorAndPending(t *testing.T) {
	f := newEdgeBackfillFixture(t)
	p := f.input("")
	want := f.add(t, 1, 502, p)
	for _, status := range []graph.Status{graph.StatusReceived, graph.StatusFailed} {
		excluded := newEdgeBackfillFixture(t)
		excluded.add(t, 1, 1, p)
		backfillQuery(t, "MATCH (d:Document {id:$doc}) SET d.domain=$domain", map[string]any{"doc": excluded.doc, "domain": f.domain})
		if err := excluded.st.SetStatus(t.Context(), excluded.doc, status, ""); err != nil {
			t.Fatal(err)
		}
	}
	foreign := newEdgeBackfillFixture(t)
	foreign.add(t, 1, 1, p)
	for i, row := range []struct{ judge, input string }{{"confirmed", string(p.Bytes)}, {"", ""}} {
		excluded := f.add(t, 600+i, 1, p)[0]
		backfillQuery(t, fixtureEdges+" AND r.judge_gen=$gen SET r.judge=$judge, r.judge_input=$input",
			map[string]any{"doc": f.doc, "gen": excluded.JudgeGen, "judge": row.judge, "input": row.input})
	}
	first, err := f.st.PendingStoredJudgeEdges(t.Context(), f.domain, nil, 500)
	if err != nil || len(first) != 500 {
		t.Fatalf("first cursor page has %d rows, err=%v", len(first), err)
	}
	second, err := f.st.PendingStoredJudgeEdges(t.Context(), f.domain, &first[499].StoredFactRef, 500)
	if err != nil || len(second) != 2 {
		t.Fatalf("second cursor page has %d rows, err=%v", len(second), err)
	}
	for i, row := range append(first, second...) {
		if row.StoredFactRef != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, row.StoredFactRef, want[i])
		}
	}

	var calls atomic.Int32
	var handlerErr atomic.Pointer[error]
	j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			if _, err := f.addEdges(r.Context(), 0, 1, p); err != nil {
				handlerErr.Store(&err)
			}
		}
		edgeConfirmed(w)
	})
	j.SetBackoff(nil)
	r, err := judge.BackfillEdges(t.Context(), f.st, f.cached(j), j, f.opt())
	if err := handlerErr.Load(); err != nil {
		t.Fatal(*err)
	}
	if err != nil || r.Selected != 502 || r.Confirmed != 502 || r.Judged != 502 || r.Attempts != 1 || r.CacheHits != 501 {
		t.Fatalf("first run = %+v, %v", r, err)
	}
	r, err = judge.BackfillEdges(t.Context(), f.st, f.cached(j), j, f.opt())
	if err != nil || r.Selected != 1 || r.Confirmed != 1 || r.Attempts != 0 || r.CacheHits != 1 || calls.Load() != 1 {
		t.Fatalf("second run = %+v, %v, HTTP calls=%d", r, err, calls.Load())
	}
}

func TestEdgeBackfillGuardsAndInvalidInput(t *testing.T) {
	for _, scenario := range []string{"reset document", "change domain", "duplicate", "invalid payload", "invalid hash"} {
		t.Run(scenario, func(t *testing.T) {
			f := newEdgeBackfillFixture(t)
			payload := f.input("")
			switch scenario {
			case "invalid payload":
				payload.Bytes = []byte(`{"fact":null}`)
				payload.InputHash = fmt.Sprintf("%x", sha256.Sum256(payload.Bytes))
			case "invalid hash":
				payload.InputHash = "bad-hash"
			}
			f.add(t, 1, 1, payload)
			other := graphtest.Store(t)
			var handlerErr atomic.Pointer[error]
			j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
				var err error
				switch scenario {
				case "reset document":
					err = other.SetStatus(r.Context(), f.doc, graph.StatusReceived, "")
				case "change domain":
					err = backfillQueryErr(r.Context(), "MATCH (d:Document {id:$doc}) SET d.domain='changed'", map[string]any{"doc": f.doc})
				case "duplicate":
					err = backfillQueryErr(r.Context(), fixtureEdges+" CREATE (h)-[dup:uses]->(t) SET dup=properties(r)", map[string]any{"doc": f.doc})
				default:
					err = errors.New("invalid inputs must not reach provider")
				}
				if err != nil {
					handlerErr.Store(&err)
				}
				_, _ = io.WriteString(w, chatBody(`{"verdict":"refuted","reason":"fixture reason"}`))
			})
			j.SetBackoff(nil)
			got, err := judge.BackfillEdges(t.Context(), f.st, f.cached(j), j, f.opt())
			if err := handlerErr.Load(); err != nil {
				t.Fatal(*err)
			}
			pending := 1
			switch scenario {
			case "duplicate":
				pending = 2
				if err == nil || !strings.Contains(err.Error(), "multiple edges") {
					t.Fatalf("duplicate run = %+v, %v", got, err)
				}
			case "invalid payload", "invalid hash":
				if err != nil || got.Invalid != 1 || got.Selected != 1 || got.Attempts != 0 || got.CacheHits != 0 {
					t.Fatalf("invalid run = %+v, %v", got, err)
				}
			default:
				if err != nil || got.Discarded != 1 || got.Attempts != 1 {
					t.Fatalf("drift run = %+v, %v", got, err)
				}
			}
			if states, audits := f.states(t), f.count(t, "RejectedFact"); len(states) != pending || slices.ContainsFunc(states, func(s string) bool { return s != "" }) || audits != 0 {
				t.Fatalf("states=%v audits=%d", states, audits)
			}
		})
	}
}

func TestEdgeBackfillBudgetAndSeparateCache(t *testing.T) {
	f := newEdgeBackfillFixture(t)
	seed := newEdgeBackfillFixture(t)
	shared, uncached := f.input("shared"), f.input("uncached")
	f.add(t, 1, 1, shared)
	seed.add(t, 1, 1, shared)
	var calls atomic.Int32
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			edgeConfirmed(w)
		default:
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	j.SetBackoff([]time.Duration{0, 0, 0})
	cache := seed.cached(j)
	if v, err := cache.VerifyPayload(t.Context(), shared, seed.doc); err != nil || v.Status != "confirmed" {
		t.Fatalf("seed verdict=%+v, %v", v, err)
	}
	key := judge.RequestKey(shared.Bytes, j.Model())
	backfillQuery(t, "MATCH (v:JudgeVerdict {key:$key}) SET v.model = 'cached-model'", map[string]any{"key": key})
	opt := f.opt()
	opt.BudgetCalls = 2
	r, err := judge.BackfillEdges(t.Context(), f.st, f.cached(j), j, opt)
	if err != nil || r.CacheHits != 1 || r.Confirmed != 1 || r.Attempts != 0 || calls.Load() != 1 {
		t.Fatalf("separately seeded cache run = %+v, %v, HTTP calls=%d", r, err, calls.Load())
	}
	rows, err := f.st.RunReadQuery(t.Context(), fixtureEdges+" RETURN r.judge_model AS model", map[string]any{"doc": f.doc}, 0)
	if err != nil || len(rows.Rows) != 1 || rows.Rows[0]["model"] != "cached-model" {
		t.Errorf("backfilled edge provenance = %+v, %v", rows, err)
	}
	f.add(t, 2, 1, uncached)
	r, err = judge.BackfillEdges(t.Context(), f.st, f.cached(j), j, opt)
	if err != nil || !r.StoppedForBudget || r.StoppedProvider || r.Attempts != 2 || calls.Load() != 3 {
		t.Fatalf("budget run = %+v, %v, HTTP calls=%d", r, err, calls.Load())
	}
	if states := f.states(t); !slices.Equal(states, []string{"confirmed", ""}) {
		t.Fatalf("budget stop changed an unjudged edge: %v", states)
	}
	if got := seed.states(t); !slices.Equal(got, []string{""}) {
		t.Fatalf("cache seeding altered the source edge: %v", got)
	}
}

type edgeCacheHook struct {
	*graph.Store
	before func(context.Context)
	after  func()
}

func (s edgeCacheHook) PutJudgeVerdict(ctx context.Context, v graph.JudgeVerdict) error {
	if s.before != nil {
		s.before(ctx)
	}
	err := s.Store.PutJudgeVerdict(ctx, v)
	if err == nil && s.after != nil {
		s.after()
	}
	return err
}

func TestEdgeBackfillKilledAfterCacheWrite(t *testing.T) {
	const marker = "GKG_EDGE_BACKFILL_KILL_DOMAIN"
	if domain := os.Getenv(marker); domain != "" {
		st := graphtest.Store(t)
		var cfg config.Config
		cfg.Judge.BaseURL = os.Getenv("GKG_JUDGE_BASE_URL")
		client := judge.NewClient(cfg)
		client.SetBackoff(nil)
		cache := judge.NewCached(client, edgeCacheHook{Store: st, after: func() {
			process, err := os.FindProcess(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			if err := process.Kill(); err != nil {
				t.Fatal(err)
			}
		}}, client.Model())
		_, err := judge.BackfillEdges(t.Context(), st, cache, client, judge.BackfillOptions{Domain: domain})
		t.Fatalf("worker returned before the cache-write kill: %v", err)
	}

	f := newEdgeBackfillFixture(t)
	p := f.input("")
	f.add(t, 1, 1, p)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		edgeConfirmed(w)
	}))
	defer provider.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestEdgeBackfillKilledAfterCacheWrite$")
	cmd.Env = append(os.Environ(), marker+"="+f.domain, "GKG_JUDGE_BASE_URL="+provider.URL)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL || ctx.Err() != nil {
		t.Fatalf("worker exit=%v context=%v output=%s", err, ctx.Err(), output)
	}
	leases, err := f.st.RunReadQuery(t.Context(), "MATCH (l:JudgeLease {id:$id}) RETURN l.owner AS owner",
		map[string]any{"id": graph.BackfillLeaseName}, 1)
	if err != nil || len(leases.Rows) != 1 {
		t.Fatalf("killed worker lease = %+v, %v", leases, err)
	}
	t.Cleanup(func() {
		_ = f.st.ReleaseLease(context.Background(), graph.BackfillLeaseName, leases.Rows[0]["owner"].(string))
	})
	if got := f.states(t); !slices.Equal(got, []string{""}) {
		t.Fatalf("killed worker changed edge: %v", got)
	}
	var cfg config.Config
	cfg.Judge.BaseURL = provider.URL
	client := judge.NewClient(cfg)
	client.SetBackoff(nil)
	opt := f.opt()
	opt.Force = true
	r, err := judge.BackfillEdges(t.Context(), f.st, f.cached(client), client, opt)
	if err != nil || r.Confirmed != 1 || r.Attempts != 0 || r.CacheHits != 1 || calls.Load() != 1 {
		t.Fatalf("resumed run = %+v, %v, HTTP calls=%d", r, err, calls.Load())
	}
}

func TestEdgeBackfillPersistsPaidVerdictAfterCancellation(t *testing.T) {
	f := newEdgeBackfillFixture(t)
	p := f.input("")
	f.add(t, 1, 1, p)
	j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) { edgeConfirmed(w) })
	j.SetBackoff(nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cache := judge.NewCached(j, edgeCacheHook{Store: f.st, before: func(writeCtx context.Context) {
		cancel()
		if writeCtx.Err() != nil {
			t.Errorf("cache write was canceled with run: %v", writeCtx.Err())
		}
		if deadline, ok := writeCtx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Errorf("missing cache write bound: %v, %t", deadline, ok)
		}
	}}, j.Model())
	if r, err := judge.BackfillEdges(ctx, f.st, cache, j, f.opt()); !errors.Is(err, context.Canceled) || r.Attempts != 1 || r.Confirmed != 0 {
		t.Fatalf("canceled run = %+v, %v", r, err)
	}
	if cached, found, err := f.st.JudgeVerdict(t.Context(), judge.RequestKey(p.Bytes, j.Model())); !found || err != nil || cached.Status != "confirmed" {
		t.Fatalf("paid verdict not cached: %+v, %t, %v", cached, found, err)
	}
	if got := f.states(t); !slices.Equal(got, []string{""}) {
		t.Fatalf("canceled run changed edge: %v", got)
	}
}

func TestEdgeBackfillChecksLeaseOnRetryAndApply(t *testing.T) {
	for _, name := range []string{graph.BackfillLeaseName, graph.ValidityApplyLease, graph.RejudgeLeaseName} {
		t.Run(name, func(t *testing.T) {
			f := newEdgeBackfillFixture(t)
			f.add(t, 1, 2, f.input(""))
			other := graphtest.Store(t)
			var calls atomic.Int32
			var handlerErr atomic.Pointer[error]
			j := newTestJudge(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					ok, err := other.AcquireLease(r.Context(), name, "test-displacement", time.Minute, true)
					if !ok || err != nil {
						failure := fmt.Errorf("seed interfering lease=%t, %v", ok, err)
						handlerErr.Store(&failure)
					}
				}
				w.WriteHeader(http.StatusTooManyRequests)
			})
			t.Cleanup(func() { _ = other.ReleaseLease(context.Background(), name, "test-displacement") })
			j.SetBackoff([]time.Duration{0})
			r, err := judge.BackfillEdges(t.Context(), f.st, f.cached(j), j, f.opt())
			if err := handlerErr.Load(); err != nil {
				t.Fatal(*err)
			}
			wantErr := graph.ErrLeaseExcluded
			if name == graph.BackfillLeaseName {
				wantErr = graph.ErrLeaseLost
			}
			if !errors.Is(err, wantErr) || r.LeaseLost != (name == graph.BackfillLeaseName) || r.Confirmed != 0 || r.Attempts != 1 || calls.Load() != 1 {
				t.Fatalf("interfered run = %+v, %v, HTTP calls=%d", r, err, calls.Load())
			}
			if got := f.states(t); !slices.Equal(got, []string{"", ""}) {
				t.Fatalf("interfered run changed edges: %v", got)
			}
		})
	}

	t.Run("after response before persistence", func(t *testing.T) {
		f := newEdgeBackfillFixture(t)
		p := f.input("")
		f.add(t, 1, 1, p)
		other := graphtest.Store(t)
		var calls atomic.Int32
		j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); edgeConfirmed(w) })
		j.SetBackoff(nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		cache := judge.NewCached(j, edgeCacheHook{Store: other, before: func(writeCtx context.Context) {
			ok, err := other.AcquireLease(t.Context(), graph.BackfillLeaseName, "new-owner", time.Minute, true)
			if !ok || err != nil {
				t.Fatalf("takeover=%t, %v", ok, err)
			}
			if deadline, ok := writeCtx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
				t.Errorf("missing cache write bound: %v, %t", deadline, ok)
			}
		}}, j.Model())
		t.Cleanup(func() { _ = other.ReleaseLease(context.Background(), graph.BackfillLeaseName, "new-owner") })
		r, err := judge.BackfillEdges(ctx, f.st, cache, j, f.opt())
		if !errors.Is(err, graph.ErrLeaseLost) || !r.LeaseLost || r.Judged != 1 || r.Confirmed != 0 || r.Attempts != 1 || calls.Load() != 1 {
			t.Fatalf("displaced run = %+v, %v, HTTP calls=%d", r, err, calls.Load())
		}
		if cached, found, err := other.JudgeVerdict(t.Context(), judge.RequestKey(p.Bytes, j.Model())); !found || err != nil || cached.Status != "confirmed" {
			t.Fatalf("paid verdict not cached: %+v, %t, %v", cached, found, err)
		}
		if got := f.states(t); !slices.Equal(got, []string{""}) {
			t.Fatalf("displaced run changed edge: %v", got)
		}
	})
}

func TestEdgeBackfillProviderWideErrorsStopAndResume(t *testing.T) {
	for _, code := range []int{503, 429, 401, 403, 404, 405, 415} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newEdgeBackfillFixture(t)
			f.add(t, 1, 1, f.input("first"))
			f.add(t, 2, 1, f.input("second"))
			var calls atomic.Int32
			var healthy atomic.Bool
			j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if healthy.Load() {
					edgeConfirmed(w)
					return
				}
				w.WriteHeader(code)
			})
			j.SetBackoff([]time.Duration{0})
			want := 1
			if code == 429 || code == 503 {
				want = 2
			}
			r, err := judge.BackfillEdges(t.Context(), f.st, f.cached(j), j, f.opt())
			if err != nil || !r.StoppedProvider || r.Attempts != want || r.Selected != 1 || r.Failed != 0 || r.Judged != 0 || int(calls.Load()) != want {
				t.Fatalf("outage run = %+v, %v, HTTP calls=%d", r, err, calls.Load())
			}
			if got := f.states(t); !slices.Equal(got, []string{"", ""}) {
				t.Fatalf("outage marked edges: %v", got)
			}
			healthy.Store(true)
			r, err = judge.BackfillEdges(t.Context(), f.st, f.cached(j), j, f.opt())
			if err != nil || r.StoppedProvider || r.Confirmed != 2 || r.Attempts != 2 || int(calls.Load()) != want+2 {
				t.Fatalf("resumed run = %+v, %v, HTTP calls=%d", r, err, calls.Load())
			}
		})
	}
}

func TestEdgeBackfillRowErrorsSkipAndRetryNextRun(t *testing.T) {
	for _, scenario := range []string{"HTTP 400", "undecodable", "unknown verdict"} {
		t.Run(scenario, func(t *testing.T) {
			f := newEdgeBackfillFixture(t)
			poison, healthy := f.input("first"), f.input("second")
			if poison.InputHash > healthy.InputHash {
				poison, healthy = healthy, poison
			}
			f.add(t, 1, 1, poison)
			f.add(t, 2, 1, healthy)
			var calls atomic.Int32
			j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 2 {
					edgeConfirmed(w)
					return
				}
				switch scenario {
				case "HTTP 400":
					w.WriteHeader(http.StatusBadRequest)
				case "undecodable":
					_, _ = io.WriteString(w, "{")
				default:
					_, _ = io.WriteString(w, chatBody(`{"verdict":"unexpected"}`))
				}
			})
			j.SetBackoff([]time.Duration{0})
			r, err := judge.BackfillEdges(t.Context(), f.st, f.cached(j), j, f.opt())
			if err != nil || r.Failed != 1 || r.Confirmed != 1 || r.Attempts != 2 || r.StoppedProvider {
				t.Fatalf("first run = %+v, %v", r, err)
			}
			r, err = judge.BackfillEdges(t.Context(), f.st, f.cached(j), j, f.opt())
			if err != nil || r.Failed != 1 || r.Selected != 1 || r.Attempts != 1 || r.StoppedProvider || calls.Load() != 3 {
				t.Fatalf("second run = %+v, %v, HTTP calls=%d", r, err, calls.Load())
			}
			if got := f.states(t); !slices.Equal(got, []string{"", "confirmed"}) {
				t.Fatalf("row failure marked edge: %v", got)
			}
		})
	}
}

func TestEdgeBackfillAndOnlineVerdictRace(t *testing.T) {
	for _, onlineFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("onlineFirst=%t", onlineFirst), func(t *testing.T) {
			f := newEdgeBackfillFixture(t)
			ref := f.add(t, 1, 1, f.input(""))[0]
			online := graphtest.Store(t)
			release, onlineDone := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseOnline := func() { releaseOnce.Do(func() { close(release) }) }
			type appliedResult struct {
				applied bool
				err     error
			}
			var onlineResult appliedResult
			go func() {
				defer close(onlineDone)
				<-release
				status := "refuted"
				if onlineFirst {
					status = "confirmed"
				}
				applied, err := online.ApplyStoredFactVerdict(t.Context(), ref, status, "online reason", "", graph.StoredFactVerdictOptions{})
				onlineResult = appliedResult{applied, err}
			}()
			j := newTestJudge(t, func(w http.ResponseWriter, _ *http.Request) {
				if onlineFirst {
					releaseOnline()
					<-onlineDone
					_, _ = io.WriteString(w, chatBody(`{"verdict":"refuted","reason":"backfill reason"}`))
					return
				}
				edgeConfirmed(w)
			})
			j.SetBackoff(nil)
			r, err := judge.BackfillEdges(t.Context(), f.st, f.cached(j), j, f.opt())
			releaseOnline()
			<-onlineDone
			if err != nil || onlineResult.err != nil || onlineResult.applied != onlineFirst ||
				(onlineFirst && r.Discarded != 1) || (!onlineFirst && r.Confirmed != 1) {
				t.Fatalf("online=%+v backfill=%+v, %v", onlineResult, r, err)
			}
			if got := f.states(t); !slices.Equal(got, []string{"confirmed"}) || f.count(t, "RejectedFact") != 0 {
				t.Fatalf("late refutation damaged confirmed edge: %v", got)
			}
		})
	}
}
