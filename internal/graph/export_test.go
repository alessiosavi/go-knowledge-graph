package graph

import (
	"context"
	"errors"
	"maps"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

var RepairEntityNamePageForTest = (*Store).repairEntityNamePage

func AssignDomainFailForTest(ctx context.Context, s *Store, a Assignment, err error) error {
	return s.assignDomain(ctx, a, func() error { return err })
}

// Expose the production queries only to the external integration tests.
const (
	EntityByIDQuery  = entityByIDQuery
	EntityByKeyQuery = entityByKeyQuery
)

type CapturedReadQuery struct {
	Query  string
	Params map[string]any
}

// CaptureReadQueries observes real store reads without changing their execution.
// Callers must not use the store concurrently while recording.
func CaptureReadQueries(st *Store, read func()) []CapturedReadQuery {
	var queries []CapturedReadQuery
	driver := st.driver
	st.driver = recordingReadDriver{Driver: driver, queries: &queries}
	defer func() { st.driver = driver }()
	read()
	return queries
}

type recordingReadDriver struct {
	neo4j.Driver
	queries *[]CapturedReadQuery
}

func (d recordingReadDriver) NewSession(ctx context.Context, cfg neo4j.SessionConfig) neo4j.Session {
	return recordingReadSession{Session: d.Driver.NewSession(ctx, cfg), queries: d.queries}
}

type recordingReadSession struct {
	neo4j.Session
	queries *[]CapturedReadQuery
}

func (s recordingReadSession) Run(ctx context.Context, query string, params map[string]any, cfg ...func(*neo4j.TransactionConfig)) (neo4j.Result, error) {
	*s.queries = append(*s.queries, CapturedReadQuery{query, maps.Clone(params)})
	return s.Session.Run(ctx, query, params, cfg...)
}

func (s recordingReadSession) BeginTransaction(ctx context.Context, cfg ...func(*neo4j.TransactionConfig)) (neo4j.ExplicitTransaction, error) {
	tx, err := s.Session.BeginTransaction(ctx, cfg...)
	if err != nil {
		return nil, err
	}
	return recordingReadTx{ExplicitTransaction: tx, queries: s.queries}, nil
}

type recordingReadTx struct {
	neo4j.ExplicitTransaction
	queries *[]CapturedReadQuery
}

func (tx recordingReadTx) Run(ctx context.Context, query string, params map[string]any) (neo4j.Result, error) {
	*tx.queries = append(*tx.queries, CapturedReadQuery{query, maps.Clone(params)})
	return tx.ExplicitTransaction.Run(ctx, query, params)
}

// InterruptMigrationForTest injects a failure between committed transactions.
// It exercises the actual database phase/ledger recovery without production hooks.
func InterruptMigrationForTest(st *Store, mode string, run func() error) error {
	driver := st.driver
	state := &migrationInterruption{mode: mode}
	st.driver = interruptMigrationDriver{Driver: driver, state: state}
	defer func() { st.driver = driver }()
	return run()
}

type migrationInterruption struct {
	mode        string
	assignments int
	stop        bool
}
type interruptMigrationDriver struct {
	neo4j.Driver
	state *migrationInterruption
}

func (d interruptMigrationDriver) NewSession(ctx context.Context, cfg neo4j.SessionConfig) neo4j.Session {
	return interruptMigrationSession{Session: d.Driver.NewSession(ctx, cfg), state: d.state}
}

type interruptMigrationSession struct {
	neo4j.Session
	state *migrationInterruption
}

func (s interruptMigrationSession) ExecuteWrite(ctx context.Context, work neo4j.ManagedTransactionWork, cfg ...func(*neo4j.TransactionConfig)) (any, error) {
	if s.state.stop {
		return nil, errors.New("synthetic migration interruption")
	}
	var assigned, cleared bool
	result, err := s.Session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return work(interruptMigrationTx{ManagedTransaction: tx, assigned: &assigned, cleared: &cleared})
	}, cfg...)
	if err == nil {
		if assigned {
			s.state.assignments++
		}
		s.state.stop = s.state.mode == "assign" && s.state.assignments == 3 || s.state.mode == "clear" && cleared
	}
	return result, err
}

type interruptMigrationTx struct {
	neo4j.ManagedTransaction
	assigned, cleared *bool
}

func (tx interruptMigrationTx) Run(ctx context.Context, q string, p map[string]any) (neo4j.Result, error) {
	if strings.Contains(q, "CREATE (:DomainDecision") {
		*tx.assigned = true
	}
	if strings.Contains(q, "DELETE m RETURN count(*)") {
		*tx.cleared = true
	}
	return tx.ManagedTransaction.Run(ctx, q, p)
}
