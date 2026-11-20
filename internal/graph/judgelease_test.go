package graph_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestLease(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	name := fmt.Sprintf("Acme-Max-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = st.ReleaseLease(context.Background(), name, "a")
		_ = st.ReleaseLease(context.Background(), name, "b")
	})
	acquire := func(owner string, force, want bool) {
		t.Helper()
		ok, err := st.AcquireLease(ctx, name, owner, 120*time.Second, force)
		if err != nil || ok != want {
			t.Fatalf("acquire %s force=%v: %v %v want=%v", owner, force, ok, err, want)
		}
	}
	acquire("a", false, true)
	acquire("b", false, false)
	if ok, err := st.RenewLease(ctx, name, "b", time.Minute); err != nil || ok {
		t.Fatalf("wrong-owner renewal=%v %v", ok, err)
	}
	if err := st.ReleaseLease(ctx, name, "b"); err != nil {
		t.Fatal(err)
	}
	acquire("b", false, false)
	if ok, err := st.RenewLease(ctx, name, "a", time.Minute); err != nil || !ok {
		t.Fatalf("renew=%v %v", ok, err)
	}
	acquire("b", true, true)
	if ok, err := st.RenewLease(ctx, name, "a", time.Minute); err != nil || ok {
		t.Fatalf("old-owner renewal=%v %v", ok, err)
	}
	if err := st.ReleaseLease(ctx, name, "a"); err != nil {
		t.Fatal(err)
	}
	acquire("a", false, false)
	rawRun(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (l:JudgeLease {id: '%s'}) SET l.expires_at = 0", name))
	if ok, err := st.RenewLease(ctx, name, "b", time.Minute); err != nil || ok {
		t.Fatalf("expired lease resurrected=%v %v", ok, err)
	}
	acquire("a", false, true)
	if err := st.ReleaseLease(ctx, name, "a"); err != nil {
		t.Fatal(err)
	}
	acquire("b", false, true)
}

func TestLeaseConcurrentOwners(t *testing.T) {
	st := graphtest.Store(t)
	cfg := config.Config{}
	cfg.Memgraph.URI = os.Getenv(graphtest.URIEnv)
	other, err := graph.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	name := fmt.Sprintf("Quillon-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = st.ReleaseLease(context.Background(), name, "a")
		_ = st.ReleaseLease(context.Background(), name, "b")
	})
	var wg sync.WaitGroup
	start := make(chan struct{})
	wins := make(chan bool, 2)
	for i, s := range []*graph.Store{st, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.AcquireLease(t.Context(), name, []string{"a", "b"}[i], time.Minute, false)
			if err != nil {
				t.Error(err)
			}
			wins <- ok
		}()
	}
	close(start)
	wg.Wait()
	close(wins)
	n := 0
	for win := range wins {
		if win {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("owners=%d want 1", n)
	}
}

func TestValidityLeaseRefusesLiveBackfill(t *testing.T) {
	backfill := graphtest.Store(t)
	validity := graphtest.Store(t)
	const name, owner = "judge-backfill", "edge-backfill"
	if ok, err := backfill.AcquireLease(t.Context(), name, owner, time.Minute, false); err != nil || !ok {
		t.Fatalf("seed backfill lease=%v, %v", ok, err)
	}
	t.Cleanup(func() { _ = backfill.ReleaseLease(context.Background(), name, owner) })
	before := graphtest.Snapshot(t)
	rollback, blocker, err := validity.AcquireValidityApplyLease(t.Context(), "validity")
	if rollback != nil {
		defer func() { _ = rollback(t.Context()) }()
	}
	if err != nil || rollback != nil || !strings.Contains(blocker, name) {
		t.Fatalf("validity acquired=%t blocker=%q err=%v; want live backfill refusal", rollback != nil, blocker, err)
	}
	before.AssertUnchanged(t, "blocked validity acquisition")
}

func TestEdgeBackfillLeaseExclusionAcrossStores(t *testing.T) {
	backfill, other := graphtest.Store(t), graphtest.Store(t)
	ctx := context.Background()
	const backfillOwner, otherOwner = "backfill-owner", "other-owner"
	acquireBackfill := func(force bool) (bool, error) {
		return backfill.AcquireLeaseExcluding(ctx, graph.BackfillLeaseName, backfillOwner, time.Minute, force, graph.ValidityApplyLease, graph.RejudgeLeaseName)
	}
	acquireValidity := func() (bool, error) {
		rollback, _, err := other.AcquireValidityApplyLease(ctx, otherOwner)
		return rollback != nil, err
	}
	acquireRejudge := func() (bool, error) {
		return other.AcquireRejudgeLease(ctx, otherOwner, time.Minute)
	}
	for _, pair := range []struct {
		name string
		take func() (bool, error)
	}{{graph.ValidityApplyLease, acquireValidity}, {graph.RejudgeLeaseName, acquireRejudge}} {
		t.Run(pair.name, func(t *testing.T) {
			release := func() {
				t.Helper()
				if err := backfill.ReleaseLease(ctx, graph.BackfillLeaseName, backfillOwner); err != nil {
					t.Fatal(err)
				}
				if err := other.ReleaseLease(ctx, pair.name, otherOwner); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(release)
			if ok, err := pair.take(); !ok || err != nil {
				t.Fatalf("initial %s acquisition=%t, %v", pair.name, ok, err)
			}
			before := graphtest.Snapshot(t)
			if ok, err := acquireBackfill(true); ok || !errors.Is(err, graph.ErrLeaseExcluded) {
				t.Fatalf("forced backfill=%t, %v; want exclusion", ok, err)
			}
			before.AssertUnchanged(t, "blocked backfill acquisition")
			release()
			if ok, err := acquireBackfill(false); !ok || err != nil {
				t.Fatalf("initial backfill acquisition=%t, %v", ok, err)
			}
			if ok, err := pair.take(); ok || (err != nil && !errors.Is(err, graph.ErrLeaseExcluded)) {
				t.Fatalf("%s after backfill=%t, %v; want exclusion", pair.name, ok, err)
			}
			release()

			for i := 0; i < 8; i++ {
				start := make(chan struct{})
				type outcome struct {
					ok  bool
					err error
				}
				take := func(acquire func() (bool, error)) <-chan outcome {
					done := make(chan outcome, 1)
					go func() {
						<-start
						ok, err := acquire()
						done <- outcome{ok, err}
					}()
					return done
				}
				a := take(func() (bool, error) { return acquireBackfill(false) })
				b := take(pair.take)
				close(start)
				ar, br := <-a, <-b
				if ar.ok == br.ok || (ar.err != nil && !errors.Is(ar.err, graph.ErrLeaseExcluded)) ||
					(br.err != nil && !errors.Is(br.err, graph.ErrLeaseExcluded)) {
					t.Errorf("contenders: backfill=%+v %s=%+v; want one owner and one refusal", ar, pair.name, br)
				}
				release()
			}
		})
	}
}
