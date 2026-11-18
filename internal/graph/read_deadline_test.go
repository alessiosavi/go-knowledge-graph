package graph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestReadTxConfig(t *testing.T) {
	opts, err := readTxConfig(context.Background())
	if err != nil || len(opts) != 0 {
		t.Fatalf("readTxConfig without a deadline = %d options, %v", len(opts), err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 0)
	defer cancel()
	opts, err = readTxConfig(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || len(opts) != 0 {
		t.Fatalf("readTxConfig with an expired deadline = %d options, %v", len(opts), err)
	}

	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	opts, err = readTxConfig(ctx)
	if err != nil || len(opts) != 1 {
		t.Fatalf("readTxConfig with a future deadline = %d options, %v", len(opts), err)
	}
	var tx neo4j.TransactionConfig
	opts[0](&tx)
	if tx.Timeout <= 0 || tx.Timeout > time.Second {
		t.Errorf("transaction timeout = %v, want a positive duration up to 1s", tx.Timeout)
	}
}

func TestReadErrorRecognizesMemgraphTimeout(t *testing.T) {
	const timeout = "Transaction was asked to abort because of transaction timeout."
	for _, tc := range []struct {
		name, code, message string
		wantTimeout         bool
	}{
		{"timeout", "Memgraph.TransientError.MemgraphError.MemgraphError", timeout, true},
		{"other transient error", "Memgraph.TransientError.MemgraphError.MemgraphError", "Transaction was terminated by a user.", false},
		{"client error mentioning timeout", "Neo.ClientError.Statement.SyntaxError", timeout, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := &neo4j.Neo4jError{Code: tc.code, Msg: tc.message}
			err := readError(t.Context(), fmt.Errorf("read failed: %w", original))
			if got := errors.Is(err, ErrQueryTimeout); got != tc.wantTimeout {
				t.Errorf("errors.Is(%v, ErrQueryTimeout) = %v, want %v", err, got, tc.wantTimeout)
			}
			if !errors.Is(err, original) || !strings.Contains(err.Error(), original.Error()) {
				t.Errorf("error %v lost original error %v", err, original)
			}
		})
	}
}

func TestReadDeadlineStopsServerTransaction(t *testing.T) {
	uri := os.Getenv("GKG_TEST_MEMGRAPH_URI")
	if uri == "" {
		t.Skip("requires a disposable Memgraph integration database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	st, err := Open(ctx, config.Config{Memgraph: config.MemgraphConfig{URI: uri}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })

	for _, tc := range []struct {
		name string
		run  func(context.Context, string) error
	}{
		{"RunReadQuery", func(ctx context.Context, query string) error {
			_, err := st.RunReadQuery(ctx, query, nil, 0)
			return err
		}},
		{"runRead", func(ctx context.Context, query string) error {
			_, err := st.runRead(ctx, query, nil)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The unique alias lets the observer identify this test's query.
			marker := fmt.Sprintf("gkg_deadline_%d", time.Now().UnixNano())
			query := "UNWIND range(1, 100000) AS a UNWIND range(1, 100000) AS b RETURN count(*) AS " + marker
			start := time.Now()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			err := tc.run(ctx, query)
			elapsed := time.Since(start)
			cancel()
			if elapsed > 2*time.Second || (!errors.Is(err, ErrQueryTimeout) && !errors.Is(err, context.DeadlineExceeded)) {
				t.Errorf("read returned %v after %v, want timeout within 2s", err, elapsed)
			}
			time.Sleep(500 * time.Millisecond)
			assertNoReadTransaction(t, st, marker)
		})
	}
}

func assertNoReadTransaction(t *testing.T, st *Store, marker string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sess := st.driver.NewSession(ctx, neo4j.SessionConfig{})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, "SHOW TRANSACTIONS", nil)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		query, ok := rec.Get("query")
		if !ok {
			t.Fatalf("SHOW TRANSACTIONS returned keys %v without query", rec.Keys)
		}
		if strings.Contains(fmt.Sprint(query), marker) {
			id := getString(rec, "transaction_id")
			t.Errorf("slow read is still running on Memgraph (transaction %s)", id)
			// Clean up the test's own query if the timeout regresses.
			res, err := sess.Run(ctx, fmt.Sprintf("TERMINATE TRANSACTIONS %q", id), nil)
			if err == nil {
				_, err = res.Consume(ctx)
			}
			if err != nil {
				t.Errorf("terminate lingering test transaction: %v", err)
			}
		}
	}
}
