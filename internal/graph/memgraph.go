// Package graph is the Memgraph persistence layer: connection, schema
// bootstrap and the document/chunk/entity store used by the ingest pipeline.
package graph

import (
	"context"
	"fmt"
	"sync"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// Store is the Memgraph-backed graph store.
type Store struct {
	driver neo4j.DriverWithContext
	// mu serializes document upserts: the identity pre-read and the create
	// cannot share one transaction, so concurrent submits of one source
	// must not interleave. Multiple processes against one database are out
	// of scope for v0 (one gkg process owns the store).
	mu sync.Mutex
}

// Open connects to Memgraph over Bolt and verifies connectivity and
// credentials before returning, so misconfiguration fails fast at startup.
func Open(ctx context.Context, cfg config.Config) (*Store, error) {
	auth := neo4j.NoAuth()
	if cfg.Memgraph.User != "" || cfg.Memgraph.Password != "" {
		auth = neo4j.BasicAuth(cfg.Memgraph.User, cfg.Memgraph.Password, "")
	}
	drv, err := neo4j.NewDriverWithContext(cfg.Memgraph.URI, auth)
	if err != nil {
		return nil, fmt.Errorf("graph: open %s: %w", cfg.Memgraph.URI, err)
	}
	if err := drv.VerifyConnectivity(ctx); err != nil {
		_ = drv.Close(ctx)
		return nil, fmt.Errorf("graph: connect %s: %w", cfg.Memgraph.URI, err)
	}
	return &Store{driver: drv}, nil
}

// Close releases the driver's resources.
func (s *Store) Close(ctx context.Context) error { return s.driver.Close(ctx) }

// runRead executes a single read query and returns all records.
func (s *Store) runRead(ctx context.Context, query string, params map[string]any) ([]*neo4j.Record, error) {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)
	res, err := sess.Run(ctx, query, params)
	if err != nil {
		return nil, err
	}
	return res.Collect(ctx)
}

// runWrite executes a single write query inside one managed transaction.
func (s *Store) runWrite(ctx context.Context, query string, params map[string]any) error {
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	_, err := sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, query, params)
		if err != nil {
			return nil, err
		}
		// Drain the result so the write is executed even when it yields rows.
		if _, err := res.Consume(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	})
	return err
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
