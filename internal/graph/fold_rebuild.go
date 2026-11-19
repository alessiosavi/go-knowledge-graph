package graph

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

const (
	foldLeaseName   = "fold-rebuild"
	foldLeaseTTL    = 120 * time.Second
	foldBatchSize   = 2000
	foldBatchBudget = 20 * time.Second
)

var (
	errFoldLeaseHeld = errors.New("graph: another fold rebuild holds the lease")
	errFoldLeaseLost = errors.New("graph: fold rebuild lost its lease")
	errFoldFirstRun  = argerr.New("the first rebuild requires -exceptions FILE or -clear-exceptions")
)

const foldPageQuery = `MATCH (e:Entity) WHERE e.key > $last
RETURN e.key AS key, e.id AS id, e.name AS name, e.fold_key AS fold_key
ORDER BY e.key LIMIT $limit`

// FoldRebuildResult counts committed entity changes and rows without a usable
// key or id. MissingExceptions lists exact exception keys absent at startup.
type FoldRebuildResult struct {
	RowsWritten       int
	RowsSkipped       int
	MissingExceptions []string
}

// RebuildFold recomputes advisory folds on the current extraction schema.
// A nil replacement keeps the existing exceptions. Pass an empty set to
// explicitly clear them, or to initialize a graph with no exceptions.
func (s *Store) RebuildFold(ctx context.Context, replacement *FoldExceptions) (FoldRebuildResult, error) {
	return s.rebuildFold(ctx, replacement, nil)
}

func (s *Store) rebuildFold(ctx context.Context, replacement *FoldExceptions, afterBatch func()) (out FoldRebuildResult, err error) {
	if replacement == nil {
		record, err := s.ReadFoldRecord(ctx)
		if err != nil {
			return out, err
		}
		if record == nil {
			return out, errFoldFirstRun
		}
	}
	current, err := s.ExtractionSchemaCurrent(ctx)
	if err != nil {
		return out, err
	}
	if !current {
		return out, errors.New("graph: fold rebuild requires the current extraction schema; initialize a fresh graph or run gkg migrate")
	}

	owner := rand.Text()
	acquired, err := s.AcquireLease(ctx, foldLeaseName, owner, foldLeaseTTL, false)
	if err != nil {
		return out, err
	}
	if !acquired {
		return out, errFoldLeaseHeld
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, s.ReleaseLease(releaseCtx, foldLeaseName, owner))
	}()

	for _, prop := range []string{"key", "id", "fold_key"} {
		if err := s.ensureIndex(ctx, "Entity", []string{prop}); err != nil {
			return out, err
		}
	}
	ex, missing, err := s.startFoldRebuild(ctx, owner, replacement)
	if err != nil {
		return out, fmt.Errorf("graph: start fold rebuild: %w", err)
	}
	out.MissingExceptions = missing

	// The key cursor cannot visit missing or empty keys. Count these once,
	// independently of missing ids on the rows the cursor does visit.
	rows, err := s.runRead(ctx, `MATCH (e:Entity) WHERE e.key IS NULL OR e.key = '' RETURN count(e) AS n`, nil)
	if err != nil {
		return out, fmt.Errorf("graph: count entities without a key: %w", err)
	}
	out.RowsSkipped = getInt(rows[0], "n")

	last, limit := "", foldBatchSize
	scopes := make(map[string]struct{})
	for {
		batchCtx, cancel := context.WithTimeout(ctx, foldBatchBudget)
		batch, err := s.rebuildFoldBatch(batchCtx, owner, last, limit, ex)
		timedOut := IsQueryTimeout(batchCtx, err)
		cancel()
		if err != nil {
			if timedOut && ctx.Err() == nil && limit > 1 {
				limit /= 2
				continue
			}
			return out, fmt.Errorf("graph: rebuild fold page: %w", err)
		}
		out.RowsWritten += batch.written
		out.RowsSkipped += batch.skipped
		maps.Copy(scopes, batch.scopes)
		if afterBatch != nil && batch.read > 0 {
			afterBatch()
		}
		if batch.read < limit {
			break
		}
		last = batch.last
	}
	err = s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
		if err := txGuardFoldLease(ctx, tx, owner); err != nil {
			return err
		}
		rows, err := txRows(ctx, tx, `MATCH (m:GkgMeta {id: 'fold'})
WHERE m.rebuild_owner = $owner
SET m.status = 'complete', m.rebuilt_at = $at, m.rows_written = $written, m.rows_skipped = $skipped,
    m.scopes = $scopes
RETURN m.id`, map[string]any{
			"owner": owner, "at": time.Now().UTC().Format(timeLayout), "written": out.RowsWritten, "skipped": out.RowsSkipped,
			"scopes": slices.Sorted(maps.Keys(scopes)),
		})
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return errFoldLeaseLost
		}
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("graph: finish fold rebuild: %w", err)
	}
	return out, nil
}

func (s *Store) startFoldRebuild(ctx context.Context, owner string, replacement *FoldExceptions) (FoldExceptions, []string, error) {
	var ex FoldExceptions
	var missing []string
	err := s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
		if replacement == nil {
			record, err := txReadFoldRecord(ctx, tx)
			if err != nil {
				return err
			}
			if record == nil {
				return errFoldFirstRun
			}
			ex = record.Exceptions
		} else {
			ex = *replacement
		}
		keys := ex.Keys()
		if replacement != nil {
			if err := txValidateFoldExceptionScopes(ctx, tx, keys); err != nil {
				return err
			}
		}
		rows, err := txRows(ctx, tx, `UNWIND $keys AS key
OPTIONAL MATCH (e:Entity {key: key})
WITH key, e WHERE e IS NULL
RETURN key ORDER BY key`, map[string]any{"keys": keys})
		if err != nil {
			return err
		}
		missing = make([]string, 0, len(rows))
		for _, row := range rows {
			missing = append(missing, getString(row, "key"))
		}
		if err := txGuardFoldLease(ctx, tx, owner); err != nil {
			return err
		}
		return txRun(ctx, tx, `MERGE (m:GkgMeta {id: 'fold'})
SET m.fold_version = $version, m.exceptions_keys = $keys, m.exceptions_digest = $digest,
    m.rebuild_owner = $owner, m.status = 'running'
REMOVE m.rebuilt_at, m.rows_written, m.rows_skipped`, map[string]any{
			"version": FoldKeyVersion, "keys": keys, "digest": ex.Digest(), "owner": owner,
		})
	})
	return ex, missing, err
}

func txValidateFoldExceptionScopes(ctx context.Context, tx neo4j.ManagedTransaction, keys []string) error {
	for _, key := range keys {
		if strings.HasPrefix(key, "@") || strings.HasPrefix(key, "#") {
			continue
		}
		// Domain registration enables scoped writes before the first entity
		// appears. Also detect scoped legacy data with no Domain node.
		for _, query := range []string{
			`MATCH (:Domain) RETURN 1 LIMIT 1`,
			`MATCH (e:Entity) WHERE e.key STARTS WITH '@' RETURN 1 LIMIT 1`,
		} {
			rows, err := txRows(ctx, tx, query, nil)
			if err != nil {
				return err
			}
			if len(rows) > 0 {
				return argerr.Errorf("fold exception %q is a bare key on a graph with domains", key)
			}
		}
		break
	}
	return nil
}

func txGuardFoldLease(ctx context.Context, tx neo4j.ManagedTransaction, owner string) error {
	// Updating the lease creates a write conflict with a concurrent takeover,
	// even when this transaction's snapshot predates the new owner.
	renewed, err := txRenewLease(ctx, tx, foldLeaseName, owner, foldLeaseTTL)
	if err == nil && !renewed {
		return errFoldLeaseLost
	}
	return err
}

type foldBatch struct {
	read, written, skipped int
	last                   string
	scopes                 map[string]struct{}
}

func (s *Store) rebuildFoldBatch(ctx context.Context, owner, last string, limit int, ex FoldExceptions) (foldBatch, error) {
	var batch foldBatch
	err := s.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
		var err error
		batch, err = txRebuildFoldBatch(ctx, tx, owner, last, limit, ex)
		return err
	}, neo4j.WithTxTimeout(foldBatchBudget))
	return batch, err
}

func txRebuildFoldBatch(ctx context.Context, tx neo4j.ManagedTransaction, owner, last string, limit int, ex FoldExceptions) (foldBatch, error) {
	rows, err := txRows(ctx, tx, foldPageQuery, map[string]any{"last": last, "limit": limit})
	if err != nil {
		return foldBatch{}, err
	}
	batch := foldBatch{read: len(rows), scopes: make(map[string]struct{})}
	updates := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		batch.last = getString(row, "key")
		if !foldRowEligible(row) {
			batch.skipped++
			continue
		}
		update, fold := foldUpdate(row, ex)
		if fold != nil {
			scope, _, _ := strings.Cut(*fold, "|")
			batch.scopes[scope] = struct{}{}
		}
		if update != nil {
			updates = append(updates, update)
		}
	}
	if err := txGuardFoldLease(ctx, tx, owner); err != nil {
		return foldBatch{}, err
	}
	batch.written = len(updates)
	return batch, txApplyFoldUpdates(ctx, tx, updates)
}

func foldRowEligible(row *neo4j.Record) bool {
	return getString(row, "key") != "" && getString(row, "id") != ""
}

// CountStaleFolds checks hints for entities maintained by the rebuild in bounded
// read-only pages. Online writes can change the result between pages.
func (s *Store) CountStaleFolds(ctx context.Context, ex FoldExceptions) (int, error) {
	count, last := 0, ""
	for {
		rows, err := s.runRead(ctx, foldPageQuery, map[string]any{"last": last, "limit": foldBatchSize})
		if err != nil {
			return 0, fmt.Errorf("graph: check fold hints: %w", err)
		}
		for _, row := range rows {
			if !foldRowEligible(row) {
				continue
			}
			if update, _ := foldUpdate(row, ex); update != nil {
				count++
			}
		}
		if len(rows) < foldBatchSize {
			return count, nil
		}
		last = getString(rows[len(rows)-1], "key")
	}
}
