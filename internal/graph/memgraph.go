// Package graph is the Memgraph persistence layer: connection, schema
// bootstrap and the document/chunk/entity store used by the ingest pipeline.
package graph

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
	driverconfig "github.com/neo4j/neo4j-go-driver/v6/neo4j/config"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// Store is the Memgraph-backed graph store.
type Store struct {
	driver neo4j.Driver
	// mu serializes document upserts and extraction merges: their
	// pre-read/write pairs cannot share one transaction, and concurrent
	// transactions that MERGE the same entity key abort at commit with a
	// unique-constraint violation the driver does not retry. Multiple
	// processes against one database are out of scope for v0 (one gkg
	// process owns the store).
	mu                  sync.Mutex
	writeLockWait       func(time.Duration)
	productLinks        productLinkCache
	productTextFloor    float64
	productTextCoverage productTextCoverageCache
}

// SetWriteLockWaitObserver registers the optional lock wait observer.
// Set it before using the store concurrently. The callback runs outside mu.
func (s *Store) SetWriteLockWaitObserver(observe func(time.Duration)) {
	s.writeLockWait = observe
}

// Open connects to Memgraph over Bolt and verifies connectivity and
// credentials before returning, so misconfiguration fails fast at startup.
func Open(ctx context.Context, cfg config.Config) (*Store, error) {
	auth := neo4j.NoAuth()
	if cfg.Memgraph.User != "" || cfg.Memgraph.Password != "" {
		auth = neo4j.BasicAuth(cfg.Memgraph.User, cfg.Memgraph.Password, "")
	}
	drv, err := neo4j.NewDriver(cfg.Memgraph.URI, auth, func(c *driverconfig.Config) {
		// v5 never retried Session.Run. Keep that policy for reads and DDL;
		// managed ExecuteWrite transactions retain the driver retry defaults.
		c.DisableAutoCommitRetries = true
	})
	if err != nil {
		return nil, fmt.Errorf("graph: open %s: %w", cfg.Memgraph.URI, err)
	}
	if err := drv.VerifyConnectivity(ctx); err != nil {
		_ = drv.Close(ctx)
		return nil, fmt.Errorf("graph: connect %s: %w", cfg.Memgraph.URI, err)
	}
	return &Store{driver: drv, productTextFloor: cfg.ProductText.SimilarityFloor()}, nil
}

// Close releases the driver's resources.
func (s *Store) Close(ctx context.Context) error {
	s.productLinks.stop(ctx)
	return s.driver.Close(ctx)
}

// Ping verifies the database connection; the readiness probe uses it.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.driver.VerifyConnectivity(ctx); err != nil {
		return fmt.Errorf("graph: ping: %w", err)
	}
	return nil
}

// runRead executes a single read query and returns all records.
func (s *Store) runRead(ctx context.Context, query string, params map[string]any) ([]*neo4j.Record, error) {
	opts, err := readTxConfig(ctx)
	if err != nil {
		return nil, err
	}
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, query, params, opts...)
	if err != nil {
		return nil, readError(ctx, err)
	}
	recs, err := res.Collect(ctx)
	return recs, readError(ctx, err)
}

// runReadTx keeps related reads in one snapshot without the retries of a
// managed read transaction, which can retry a server-side timeout.
func (s *Store) runReadTx(ctx context.Context, read func(neo4j.ManagedTransaction) error) error {
	opts, err := readTxConfig(ctx)
	if err != nil {
		return err
	}
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	tx, err := sess.BeginTransaction(ctx, opts...)
	if err != nil {
		return readError(ctx, err)
	}
	defer tx.Close(ctx)
	if err := read(tx); err != nil {
		return readError(ctx, err)
	}
	return readError(ctx, tx.Commit(ctx))
}

// readTxConfig gives Memgraph the remaining request budget.
func readTxConfig(ctx context.Context) ([]func(*neo4j.TransactionConfig), error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, nil
	}
	remaining := time.Until(deadline)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if remaining <= 0 {
		// The deadline may pass before the context timer runs.
		return nil, context.DeadlineExceeded
	}
	return []func(*neo4j.TransactionConfig){neo4j.WithTxTimeout(remaining)}, nil
}

// ErrQueryTimeout marks an abort by Memgraph's transaction timeout.
var ErrQueryTimeout = errors.New("the query exceeded the configured timeout")

// IsQueryTimeout reports whether a read failed at the server or client deadline.
func IsQueryTimeout(ctx context.Context, err error) bool {
	if err == nil || isReadClientError(err) {
		return false
	}
	return errors.Is(err, ErrQueryTimeout) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(ctx.Err(), context.DeadlineExceeded)
}

func readError(ctx context.Context, err error) error {
	if err == nil || isReadClientError(err) {
		return err
	}
	var nerr *neo4j.Neo4jError
	if errors.As(err, &nerr) && strings.Contains(nerr.Msg, "transaction timeout") &&
		nerr.Classification() == "TransientError" {
		return fmt.Errorf("%w: %w", ErrQueryTimeout, err)
	}
	// The driver's connectivity errors may omit the context cause.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", context.DeadlineExceeded, err)
	}
	return err
}

// isReadClientError reports caller mistakes that a timeout must not mask.
// Invariant: argerr wraps argument validation only, never a store or driver
// error, so a Memgraph timeout cannot be classified as a client error here.
func isReadClientError(err error) bool {
	var query *QueryError
	var arg argerr.Error
	var argPtr *argerr.Error
	return errors.As(err, &query) || errors.As(err, &arg) || errors.As(err, &argPtr)
}

// runWrite executes a single write query inside one managed transaction.
func (s *Store) runWrite(ctx context.Context, query string, params map[string]any) error {
	return s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
		return txRun(ctx, tx, query, params)
	})
}

// runWriteTx executes work inside one managed write transaction. All
// statements run by fn are atomic together.
func (s *Store) runWriteTx(ctx context.Context, fn func(tx neo4j.ManagedTransaction) error, opts ...func(*neo4j.TransactionConfig)) error {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	_, err := sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return nil, fn(tx)
	}, opts...)
	return err
}

// IsTransientWriteError reports whether a write failed due to a transaction
// conflict or deadlock, including failures surfaced after driver retries.
func IsTransientWriteError(err error) bool {
	var limit *neo4j.TransactionExecutionLimit
	if errors.As(err, &limit) && len(limit.Errors) > 0 {
		// The driver exposes the last attempt separately from the error chain.
		err = limit.Errors[len(limit.Errors)-1]
	}
	var dbErr *neo4j.Neo4jError
	if !errors.As(err, &dbErr) {
		return false
	}
	switch dbErr.Code {
	case "Neo.TransientError.Transaction.DeadlockDetected", "Neo.TransientError.Transaction.LockClientStopped":
		return true
	}
	// Memgraph shares this code family across conflicts, timeouts and other
	// transaction aborts. The message identifies the write conflict.
	return strings.HasPrefix(dbErr.Code, "Memgraph.TransientError.MemgraphError.") &&
		strings.HasPrefix(dbErr.Msg, "Cannot resolve conflicting transactions.")
}

// runWriteTxLocked serializes the whole transaction behind Store.mu before
// running it; see Store.mu for why.
func (s *Store) runWriteTxLocked(ctx context.Context, fn func(tx neo4j.ManagedTransaction) error) error {
	observe := s.writeLockWait
	var start time.Time
	if observe != nil {
		start = time.Now()
	}
	s.mu.Lock()
	if observe != nil {
		wait := time.Since(start)
		defer observe(wait)
	}
	defer s.mu.Unlock()
	return s.runWriteTx(ctx, fn)
}

// txRun runs one query inside a managed transaction, draining its result.
func txRun(ctx context.Context, tx neo4j.ManagedTransaction, query string, params map[string]any) error {
	res, err := tx.Run(ctx, query, params)
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}

func txRows(ctx context.Context, tx neo4j.ManagedTransaction, query string, params map[string]any) ([]*neo4j.Record, error) {
	res, err := tx.Run(ctx, query, params)
	if err != nil {
		return nil, err
	}
	return res.Collect(ctx)
}

// runAuto executes a single query in an autocommit (implicit) transaction.
// Memgraph rejects schema manipulation (constraints, indexes) inside
// explicit transactions, so DDL must go through this path.
func (s *Store) runAuto(ctx context.Context, query string, params map[string]any) error {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, query, params)
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}

// getString returns a string property, "" when absent or null.
func getString(r *neo4j.Record, key string) string {
	v, ok := r.Get(key)
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// getInt returns an int property, 0 when absent or null.
func getInt(r *neo4j.Record, key string) int {
	v, ok := r.Get(key)
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// getInt64Slice returns an int64 list property, nil when absent or null.
// The Bolt driver hydrates integer lists as []any of int64.
func getInt64Slice(r *neo4j.Record, key string) []int64 {
	v, ok := r.Get(key)
	if !ok || v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(list))
	for _, e := range list {
		switch n := e.(type) {
		case int64:
			out = append(out, n)
		case float64:
			out = append(out, int64(n))
		}
	}
	return out
}

// getFloat returns a float property (Bolt floats and integers both count),
// 0 when absent or null.
func getFloat(r *neo4j.Record, key string) float64 {
	v, ok := r.Get(key)
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

// getBool returns a bool property, false when absent or null.
func getBool(r *neo4j.Record, key string) bool {
	v, ok := r.Get(key)
	if !ok || v == nil {
		return false
	}
	b, _ := v.(bool)
	return b
}

// getStringSlice returns a string list property, nil when absent or null.
func getStringSlice(r *neo4j.Record, key string) []string {
	v, ok := r.Get(key)
	if !ok || v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
