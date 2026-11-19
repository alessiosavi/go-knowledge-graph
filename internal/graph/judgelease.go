package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// AcquireLease acquires a named judge lease, taking over expired leases or,
// with force, a live lease. The unique id constraint arbitrates new owners
// across processes; conditional updates and renewals stay transactional.
func (s *Store) AcquireLease(ctx context.Context, name, owner string, ttl time.Duration, force bool) (bool, error) {
	if name == "" || owner == "" || ttl <= 0 {
		return false, fmt.Errorf("graph: lease needs a name, owner and positive TTL")
	}
	acquired := false
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		acquired = false
		now := time.Now()
		res, err := tx.Run(ctx, `
MERGE (l:JudgeLease {id: $name})
ON CREATE SET l.owner = '', l.expires_at = 0
WITH l WHERE l.owner = $owner OR l.expires_at <= $now OR $force
SET l.owner = $owner, l.expires_at = $expires
RETURN l.id`, map[string]any{"name": name, "owner": owner, "now": now.UnixMilli(), "expires": now.Add(ttl).UnixMilli(), "force": force})
		if err != nil {
			return err
		}
		rows, err := res.Collect(ctx)
		acquired = len(rows) > 0
		return err
	})
	if err != nil {
		// Concurrent first acquisition can fail the uniqueness constraint
		// instead of returning an empty conditional match on Memgraph.
		if isConstraintViolation(err) {
			rows, readErr := s.runRead(ctx, `MATCH (l:JudgeLease {id: $name}) RETURN l.owner AS owner, l.expires_at AS expires`, map[string]any{"name": name})
			if readErr == nil && len(rows) == 1 && getString(rows[0], "owner") != owner && int64(getInt(rows[0], "expires")) > time.Now().UnixMilli() && !force {
				return false, nil
			}
		}
		return false, fmt.Errorf("graph: acquire judge lease: %w", err)
	}
	return acquired, nil
}

// RenewLease extends only a live lease still held by owner.
func (s *Store) RenewLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("graph: lease TTL must be positive")
	}
	renewed := false
	err := s.runWriteTxLocked(ctx, func(tx neo4j.ManagedTransaction) error {
		var err error
		renewed, err = txRenewLease(ctx, tx, name, owner, ttl)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("graph: renew judge lease: %w", err)
	}
	return renewed, nil
}

func txRenewLease(ctx context.Context, tx neo4j.ManagedTransaction, name, owner string, ttl time.Duration) (bool, error) {
	now := time.Now()
	rows, err := txRows(ctx, tx, `MATCH (l:JudgeLease {id: $name})
WHERE l.owner = $owner AND l.expires_at > $now
SET l.expires_at = $expires RETURN l.id`, map[string]any{"name": name, "owner": owner, "now": now.UnixMilli(), "expires": now.Add(ttl).UnixMilli()})
	return len(rows) > 0, err
}

// ReleaseLease cannot remove another owner's lease after a takeover.
func (s *Store) ReleaseLease(ctx context.Context, name, owner string) error {
	if err := s.runWrite(ctx, `MATCH (l:JudgeLease {id: $name}) WHERE l.owner = $owner DELETE l`, map[string]any{"name": name, "owner": owner}); err != nil {
		return fmt.Errorf("graph: release judge lease: %w", err)
	}
	return nil
}
