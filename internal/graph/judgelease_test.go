package graph_test

import (
	"context"
	"fmt"
	"os"
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
