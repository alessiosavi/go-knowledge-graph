package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// FoldRecord describes the graph's advisory fold index. Status records whether
// the current rebuild finished. Legacy records without a status read as unknown.
type FoldRecord struct {
	Version          int
	Exceptions       FoldExceptions
	ExceptionsDigest string
	Status           string
	RebuiltAt        string
	RowsWritten      int
	RowsSkipped      int
	RebuildOwner     string
}

// FoldKeyFor uses the persisted display name as-is. Document-scoped keys,
// exceptions, and names with no fold identity do not have a fold key.
func FoldKeyFor(key, name string, ex FoldExceptions) *string {
	if key == "" || strings.HasPrefix(key, "#") || ex.Contains(key) {
		return nil
	}
	scope := ""
	if prefix, surface, ok := strings.Cut(key, ":"); ok && strings.HasPrefix(prefix, "@") {
		if strings.HasPrefix(surface, "#") {
			return nil
		}
		scope = prefix
	}
	fold := FoldKeyV2(name)
	if fold == "" {
		return nil
	}
	value := scope + "|" + fold
	return &value
}

// VerifyFoldCandidate checks an index hit against the current key and name.
// Consumers must read the record and candidate and verify and use the result
// in the same transaction. Compute queryFold with FoldKeyFor on the query's
// own exact key so a query-side exception also prevents matching.
func VerifyFoldCandidate(record *FoldRecord, key, name string, queryFold *string) bool {
	if record == nil || record.Version != FoldKeyVersion || queryFold == nil {
		return false
	}
	candidate := FoldKeyFor(key, name, record.Exceptions)
	return candidate != nil && *candidate == *queryFold
}

const foldRecordQuery = `MATCH (m:GkgMeta {id: 'fold'})
RETURN m.fold_version AS version, m.exceptions_keys AS exceptions,
       m.exceptions_digest AS digest, m.status AS status, m.rebuilt_at AS rebuilt_at,
       m.rows_written AS rows_written, m.rows_skipped AS rows_skipped,
       m.rebuild_owner AS rebuild_owner`

// ReadFoldRecord returns nil when the graph has no fold metadata.
func (s *Store) ReadFoldRecord(ctx context.Context) (*FoldRecord, error) {
	rows, err := s.runRead(ctx, foldRecordQuery, nil)
	return decodeFoldRecord(rows, err)
}

func txReadFoldRecord(ctx context.Context, tx neo4j.ManagedTransaction) (*FoldRecord, error) {
	rows, err := txRows(ctx, tx, foldRecordQuery, nil)
	return decodeFoldRecord(rows, err)
}

func decodeFoldRecord(rows []*neo4j.Record, err error) (*FoldRecord, error) {
	if err != nil {
		return nil, fmt.Errorf("graph: read fold record: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("graph: found %d fold records, expected one", len(rows))
	}
	row := rows[0]
	status := getString(row, "status")
	if status == "" {
		status = "unknown"
	}
	keys := getStringSlice(row, "exceptions")
	ex := FoldExceptions{keys: make(map[string]struct{}, len(keys))}
	for _, key := range keys {
		ex.keys[key] = struct{}{}
	}
	return &FoldRecord{
		Version: getInt(row, "version"), Exceptions: ex, ExceptionsDigest: getString(row, "digest"), Status: status,
		RebuiltAt: getString(row, "rebuilt_at"), RowsWritten: getInt(row, "rows_written"),
		RowsSkipped: getInt(row, "rows_skipped"), RebuildOwner: getString(row, "rebuild_owner"),
	}, nil
}

func foldUpdate(row *neo4j.Record, ex FoldExceptions) (map[string]any, bool) {
	key := getString(row, "key")
	var value any
	if fold := FoldKeyFor(key, getString(row, "name"), ex); fold != nil {
		value = *fold
	}
	stored, _ := row.Get("fold_key")
	if stored == value {
		return nil, false
	}
	return map[string]any{"key": key, "fold_key": value}, true
}

func txApplyFoldUpdates(ctx context.Context, tx neo4j.ManagedTransaction, updates []map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	return txRun(ctx, tx, `UNWIND $updates AS u
MATCH (e:Entity {key: u.key})
FOREACH (_ IN CASE WHEN u.fold_key IS NULL THEN [1] ELSE [] END | REMOVE e.fold_key)
FOREACH (_ IN CASE WHEN u.fold_key IS NOT NULL THEN [1] ELSE [] END | SET e.fold_key = u.fold_key)`,
		map[string]any{"updates": updates})
}
