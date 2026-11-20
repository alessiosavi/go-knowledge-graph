package graph

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestWriteLockWaitObserver(t *testing.T) {
	uri := os.Getenv("GKG_TEST_MEMGRAPH_URI")
	if uri == "" {
		t.Skip("requires a disposable Memgraph integration database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := Open(ctx, config.Config{Memgraph: config.MemgraphConfig{URI: uri}})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(context.Background())
	var waits []time.Duration
	st.SetWriteLockWaitObserver(func(d time.Duration) { waits = append(waits, d) })
	locked := make(chan struct{})
	startWrite := make(chan struct{})
	go func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		close(locked)
		<-startWrite
		time.Sleep(50 * time.Millisecond)
	}()
	<-locked
	start := time.Now()
	close(startWrite)
	var beforeWork time.Duration
	if err := st.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		beforeWork = time.Since(start)
		// Make the transaction work itself longer than the expected lock wait.
		time.Sleep(100 * time.Millisecond)
		return txRun(ctx, tx, "RETURN 1", nil)
	}); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] < 40*time.Millisecond || waits[0] > beforeWork {
		t.Errorf("wait observations = %v, want one duration between 40ms and transaction work start (%v)", waits, beforeWork)
	}
}

func TestManagedWriteRetryAndCancellation(t *testing.T) {
	uri := os.Getenv("GKG_TEST_MEMGRAPH_URI")
	if uri == "" {
		t.Skip("requires a disposable Memgraph integration database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := config.Config{}
	cfg.Memgraph.URI = uri
	st, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(context.Background())
	attempts := 0
	err = st.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
		attempts++
		if attempts == 1 {
			return &neo4j.Neo4jError{Code: "Neo.TransientError.Transaction.DeadlockDetected", Msg: "synthetic retryable test failure"}
		}
		return txRun(ctx, tx, "RETURN 1", nil)
	})
	if err != nil || attempts != 2 {
		t.Fatalf("managed transaction must retry the transient failure: attempts=%d err=%v", attempts, err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	called := false
	err = st.runWriteTx(canceled, func(tx neo4j.ManagedTransaction) error {
		called = true
		return txRun(canceled, tx, "RETURN 1", nil)
	})
	if err == nil || called {
		t.Fatalf("canceled context must prevent transaction work: called=%t err=%v", called, err)
	}
}

func TestProductWriteRetriesReleaseConstraint(t *testing.T) {
	uri := os.Getenv("GKG_TEST_MEMGRAPH_URI")
	if uri == "" {
		t.Skip("requires a disposable Memgraph integration database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	st, err := Open(ctx, config.Config{Memgraph: config.MemgraphConfig{URI: uri}})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(context.Background())
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"id": "test-product-write-retry"}
	defer func() {
		if err := st.runWrite(context.Background(), `MATCH (r:ProductRelease {id:$id}) DELETE r`, params); err != nil {
			t.Error(err)
		}
	}()

	const find = `MATCH (r:ProductRelease {id:$id}) RETURN r.manifest_digest AS digest`
	const create = `CREATE (:ProductRelease {id:$id, manifest_digest:'winner'})`
	attempts := 0
	st.SetWriteLockWaitObserver(func(time.Duration) { attempts++ })
	var digest string
	err = st.runProductWrite(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := txRows(ctx, tx, find, params)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			digest = getString(rows[0], "digest")
			return nil
		}
		// Commit in a separate transaction after the first attempt's read.
		// The first attempt's CREATE then collides on the unique release ID.
		if err := st.runWrite(ctx, create, params); err != nil {
			return err
		}
		return txRun(ctx, tx, create, params)
	})
	if err != nil || attempts != 2 || digest != "winner" {
		t.Fatalf("product write = %v after %d attempts, observed digest %q", err, attempts, digest)
	}
	rows, err := st.runRead(ctx, find, params)
	if err != nil || len(rows) != 1 {
		t.Fatalf("winner query = %d rows, err %v, want one row", len(rows), err)
	}
}
