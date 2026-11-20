package graph

import (
	"fmt"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestIsTransientWriteError(t *testing.T) {
	const memgraphCode = "Memgraph.TransientError.MemgraphError.MemgraphError"
	conflict := &neo4j.Neo4jError{Code: memgraphCode, Msg: "Cannot resolve conflicting transactions. Retry this transaction when the conflicting transaction is finished."}
	timeout := &neo4j.Neo4jError{Code: "Neo.TransientError.General.TransactionTimeout"}
	for _, tc := range []struct {
		name         string
		err          error
		wantConflict bool
	}{
		{"Memgraph conflict", conflict, true},
		{"Neo4j deadlock", &neo4j.Neo4jError{Code: "Neo.TransientError.Transaction.DeadlockDetected"}, true},
		{"Neo4j lock client stopped", &neo4j.Neo4jError{Code: "Neo.TransientError.Transaction.LockClientStopped"}, true},
		{"Neo4j timeout", timeout, false},
		{"Memgraph timeout", &neo4j.Neo4jError{Code: memgraphCode, Msg: "Transaction was asked to abort because of transaction timeout."}, false},
		{"Memgraph user termination", &neo4j.Neo4jError{Code: memgraphCode, Msg: "Transaction was terminated by a user."}, false},
		{"other transient error", &neo4j.Neo4jError{Code: "Neo.TransientError.General.DatabaseUnavailable"}, false},
		{"conflict on the final attempt", &neo4j.TransactionExecutionLimit{Errors: []error{timeout, conflict}}, true},
		{"timeout on the final attempt", &neo4j.TransactionExecutionLimit{Errors: []error{conflict, timeout}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientWriteError(fmt.Errorf("write failed: %w", tc.err)); got != tc.wantConflict {
				t.Errorf("IsTransientWriteError(%v) = %t, want %t", tc.err, got, tc.wantConflict)
			}
		})
	}
}
