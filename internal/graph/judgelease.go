package graph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	BackfillLeaseName = "judge-backfill"
	RejudgeLeaseName  = "judge-rejudge"
)

var (
	ErrLeaseExcluded = errors.New("graph: judge lease blocked by a live lease")
	ErrLeaseLost     = errors.New("graph: judge lease lost")
	errLeaseHeld     = errors.New("graph: judge lease is held")
)

// AcquireLease acquires a named judge lease, taking over expired leases or,
// with force, a live lease. The unique id constraint arbitrates new owners
// across processes; conditional updates and renewals stay transactional.
func (s *Store) AcquireLease(ctx context.Context, name, owner string, ttl time.Duration, force bool) (bool, error) {
	return s.acquireLease(ctx, name, owner, ttl, force, nil, nil, false)
}

// AcquireLeaseExcluding refuses an acquisition while any named exclusion is
// live. Force applies only to the lease being acquired.
func (s *Store) AcquireLeaseExcluding(ctx context.Context, name, owner string, ttl time.Duration, force bool, excludes ...string) (bool, error) {
	return s.acquireLease(ctx, name, owner, ttl, force, nil, excludes, false)
}

// AcquireRejudgeLease excludes edge backfill while preserving candidate backfill.
func (s *Store) AcquireRejudgeLease(ctx context.Context, owner string, ttl time.Duration) (bool, error) {
	return s.acquireLease(ctx, RejudgeLeaseName, owner, ttl, false, nil, []string{BackfillLeaseName}, true)
}

func (s *Store) acquireLease(ctx context.Context, name, owner string, ttl time.Duration, force bool, previous *map[string]any, excludes []string, edgeBackfillOnly bool) (bool, error) {
	if name == "" || owner == "" || ttl <= 0 {
		return false, fmt.Errorf("graph: lease needs a name, owner and positive TTL")
	}
	acquired := false
	fenced := len(excludes) > 0
	acquire := func(tx neo4j.ManagedTransaction) error {
		acquired = false
		if fenced {
			if err := txLockLeaseExclusions(ctx, tx); err != nil {
				return err
			}
			if err := txAssertLeaseExclusions(ctx, tx, excludes, edgeBackfillOnly); err != nil {
				return err
			}
		}
		if previous != nil {
			*previous = nil
			rows, err := txRows(ctx, tx, `MATCH (l:JudgeLease {id:$name}) RETURN l.owner AS owner, l.expires_at AS expires_at`,
				map[string]any{"name": name})
			if err != nil {
				return err
			}
			if len(rows) != 0 {
				*previous = rows[0].AsMap()
			}
		}
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
		if err == nil && !acquired && fenced {
			return errLeaseHeld
		}
		if err == nil && acquired && name == BackfillLeaseName && fenced {
			// A legacy takeover can change owner without touching this
			// property. Rejudge checks that it identifies the current owner.
			err = txRun(ctx, tx, `MATCH (l:JudgeLease {id: $name}) SET l.edge_backfill_owner = $owner`,
				map[string]any{"name": name, "owner": owner})
		}
		return err
	}
	err := s.runLeaseWrite(ctx, fenced, acquire)
	if errors.Is(err, errLeaseHeld) {
		return false, nil
	}
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

// runLeaseWrite retries a fenced transaction once if the first creation of
// the fence lost a uniqueness race at commit. The whole transaction must run
// again so renewal, exclusion checks and any row write share a fresh snapshot.
func (s *Store) runLeaseWrite(ctx context.Context, fenced bool, write func(neo4j.ManagedTransaction) error) error {
	err := s.runWriteTxLocked(ctx, write)
	if fenced && err != nil && isConstraintViolation(err) {
		return s.runWriteTxLocked(ctx, write)
	}
	return err
}

// Under snapshot isolation, reading the opposing lease alone allows two
// transactions to acquire different lease nodes. Advancing a shared sequence
// makes concurrent transactions conflict even when they touch different leases.
func txLockLeaseExclusions(ctx context.Context, tx neo4j.ManagedTransaction) error {
	return txRun(ctx, tx, `
MERGE (m:GkgMeta {id:'judge_lease_fence'})
SET m.seq = coalesce(m.seq, 0) + 1`, nil)
}

func txAssertLeaseExclusions(ctx context.Context, tx neo4j.ManagedTransaction, excludes []string, edgeBackfillOnly bool) error {
	rows, err := txRows(ctx, tx, `MATCH (l:JudgeLease)
WHERE l.id IN $excludes AND l.expires_at > $now
  AND (NOT $edge_only OR l.edge_backfill_owner = l.owner)
RETURN l.id AS id ORDER BY id LIMIT 1`,
		map[string]any{"excludes": excludes, "now": time.Now().UnixMilli(), "edge_only": edgeBackfillOnly})
	if err != nil || len(rows) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", ErrLeaseExcluded, getString(rows[0], "id"))
}

// CheckLeaseExcluding reads ownership and exclusions in the same snapshot.
func (s *Store) CheckLeaseExcluding(ctx context.Context, name, owner string, excludes ...string) error {
	return s.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := txRows(ctx, tx, `MATCH (l:JudgeLease {id: $name})
WHERE l.owner = $owner AND l.expires_at > $now RETURN l.id`,
			map[string]any{"name": name, "owner": owner, "now": time.Now().UnixMilli()})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrLeaseLost
		}
		return txAssertLeaseExclusions(ctx, tx, excludes, false)
	})
}

// FenceLeaseExcluding renews the live lease and checks exclusions in the
// caller's write transaction, which must roll back on error.
func FenceLeaseExcluding(ctx context.Context, tx neo4j.ManagedTransaction, name, owner string, ttl time.Duration, excludes ...string) error {
	ok, err := txRenewFencedLease(ctx, tx, name, owner, ttl)
	if err != nil {
		return err
	}
	if !ok {
		return ErrLeaseLost
	}
	return txAssertLeaseExclusions(ctx, tx, excludes, false)
}

// RenewLeaseExcluding fences a heartbeat against mutually exclusive acquirers.
func (s *Store) RenewLeaseExcluding(ctx context.Context, name, owner string, ttl time.Duration, excludes ...string) error {
	return s.runLeaseWrite(ctx, true, func(tx neo4j.ManagedTransaction) error {
		return FenceLeaseExcluding(ctx, tx, name, owner, ttl, excludes...)
	})
}

// RenewLease extends only a live lease still held by owner.
func (s *Store) RenewLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, fmt.Errorf("graph: lease TTL must be positive")
	}
	renewed := false
	fenced := name == ValidityApplyLease || name == RejudgeLeaseName
	err := s.runLeaseWrite(ctx, fenced, func(tx neo4j.ManagedTransaction) error {
		var err error
		if fenced {
			renewed, err = txRenewFencedLease(ctx, tx, name, owner, ttl)
		} else {
			renewed, err = txRenewLease(ctx, tx, name, owner, ttl)
		}
		return err
	})
	if err != nil {
		return false, fmt.Errorf("graph: renew judge lease: %w", err)
	}
	return renewed, nil
}

func txRenewFencedLease(ctx context.Context, tx neo4j.ManagedTransaction, name, owner string, ttl time.Duration) (bool, error) {
	// An opposing acquisition can see the old lease as expired before this
	// renewal commits. Serialize it on the same fence as acquisitions.
	if err := txLockLeaseExclusions(ctx, tx); err != nil {
		return false, err
	}
	return txRenewLease(ctx, tx, name, owner, ttl)
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
