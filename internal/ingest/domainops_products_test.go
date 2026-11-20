package ingest_test

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// Simulate the commit of a release binding whose snapshot predates admission.
// Release create started after admission would see the running DomainOp.
func bindAdminDomain(t *testing.T, slug string) func() {
	t.Helper()
	uri := os.Getenv(graphtest.URIEnv)
	const dropStore = `MATCH (s:ProductStore {store:'domain-op-test'}) DELETE s RETURN count(*)`
	t.Cleanup(func() { rawRecords(t, uri, dropStore) })
	rows := rawRecords(t, uri, fmt.Sprintf(`
MATCH (d:Domain {id:'%s'})
SET d.product_guard=coalesce(d.product_guard,0)+1
CREATE (:ProductStore {store:'domain-op-test',domain:d.id,product_count:0})
RETURN d.id`, slug))
	if len(rows) != 1 {
		t.Fatalf("binding %s matched %d domains", slug, len(rows))
	}
	return func() {
		rawRecords(t, uri, `MATCH (s:ProductStore {store:'domain-op-test'})
MATCH (d:Domain {id:s.domain}) SET d.product_guard=coalesce(d.product_guard,0)+1
DELETE s RETURN count(*)`)
	}
}

func requireDomainHasProducts(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, graph.ErrDomainHasProducts) {
		t.Fatalf("error=%v, want domain_has_products", err)
	}
}

func TestProductDomainOpRefusesBeforeAdmission(t *testing.T) {
	for _, action := range []string{"move", "merge", "undo"} {
		for _, bound := range []string{"acme-admin", "zorbo-admin"} {
			t.Run(action+"/"+bound, func(t *testing.T) {
				env := adminEnv(t)
				id := adminDoc(t, env, "one", "acme-admin")
				adminDoc(t, env, "target", "zorbo-admin")
				req := ingest.DomainOpRequest{OpID: "product-guard", Action: action, Target: "zorbo-admin"}
				switch action {
				case "move", "undo":
					req.DocIDs = []string{id}
				case "merge":
					req.From = "acme-admin"
				}
				ctx := t.Context()
				if action == "undo" {
					req.Action = "move"
					if _, err := env.p.RunDomainOp(ctx, req); err != nil {
						t.Fatal(err)
					}
					detectProcess(t, env, id)
				}
				bindAdminDomain(t, bound)
				opID, wantDomain := req.OpID, "acme-admin"
				var err error
				if action == "undo" {
					_, err = env.p.UndoDomainOp(ctx, req.OpID)
					opID, wantDomain = req.OpID+"-undo", "zorbo-admin"
				} else {
					_, err = env.p.RunDomainOp(ctx, req)
				}
				requireDomainHasProducts(t, err)
				if _, err := env.st.DomainOperation(ctx, opID); !errors.Is(err, graph.ErrNotFound) {
					t.Fatalf("refused operation persisted: %v", err)
				}
				assertAdminDomain(t, env, id, wantDomain)
			})
		}
	}
}

func TestProductDomainOpBindingBeforeMove(t *testing.T) {
	for _, bound := range []string{"acme-admin", "zorbo-admin"} {
		t.Run(bound, func(t *testing.T) {
			env := adminEnv(t)
			id := adminDoc(t, env, "one", "acme-admin")
			adminDoc(t, env, "target", "zorbo-admin")
			ctx := t.Context()
			req := ingest.DomainOpRequest{OpID: "product-race", Action: "move", DocIDs: []string{id}, Target: "zorbo-admin"}
			var unbind func()
			store := &adminInterceptStore{Store: env.st, beforeMove: func() error {
				plan, err := env.st.DomainOperation(ctx, req.OpID)
				if err != nil || plan.Op.State != "running" {
					t.Fatalf("operation at binding: %+v, %v", plan, err)
				}
				unbind = bindAdminDomain(t, bound)
				return nil
			}}
			ingest.SetDomainOpStore(env.p, store)
			op, err := env.p.RunDomainOp(ctx, req)
			requireDomainHasProducts(t, err)
			plan, err := env.st.DomainOperation(ctx, req.OpID)
			if err != nil || op.State != "incomplete" || plan.Op.State != "incomplete" {
				t.Fatalf("op=%+v stored=%+v err=%v", op, plan, err)
			}
			assertAdminDomain(t, env, id, "acme-admin")
			doc, err := env.st.Document(ctx, id)
			if err != nil || doc.Status != graph.StatusDone {
				t.Fatalf("refused move reset extraction: %+v err=%v", doc, err)
			}
			records, err := env.st.DomainDecisions(ctx, req.OpID)
			if err != nil || len(records) != 0 {
				t.Fatalf("refused move wrote ledger: %+v err=%v", records, err)
			}
			_, err = env.p.RunDomainOp(ctx, req)
			requireDomainHasProducts(t, err)
			if store.calls != 1 {
				t.Fatalf("refused resume entered MoveDocument, calls=%d", store.calls)
			}
			unbind()
			if op, err := env.p.RunDomainOp(ctx, req); err != nil || op.State != "completed" {
				t.Fatalf("resume after unbind: op=%+v err=%v", op, err)
			}
			assertAdminDomain(t, env, id, "zorbo-admin")
		})
	}
}

func TestProductDomainOpResumedUndo(t *testing.T) {
	env := adminEnv(t)
	ctx := t.Context()
	id := adminDoc(t, env, "one", "acme-admin")
	op, err := env.p.RunDomainOp(ctx, ingest.DomainOpRequest{OpID: "product-undo", Action: "move", DocIDs: []string{id}, Target: "zorbo-admin"})
	if err != nil {
		t.Fatal(err)
	}
	detectProcess(t, env, id)
	plan := graph.DomainOpPlan{
		Op:      graph.DomainOp{OpID: op.OpID + "-undo", Action: "undo", Docs: []string{id}, At: time.Now()},
		Targets: map[string]string{id: "acme-admin"},
		UndoOf:  op.OpID,
	}
	if _, err := env.st.CreateDomainOp(ctx, plan); err != nil {
		t.Fatal(err)
	}
	bindAdminDomain(t, "acme-admin")
	_, err = env.p.UndoDomainOp(ctx, op.OpID)
	requireDomainHasProducts(t, err)
	assertAdminDomain(t, env, id, "zorbo-admin")
}

func TestProductDomainOpBindingBeforeMergeDelete(t *testing.T) {
	for _, bound := range []string{"acme-admin", "zorbo-admin"} {
		t.Run(bound, func(t *testing.T) {
			env := adminEnv(t)
			uri := os.Getenv(graphtest.URIEnv)
			rawRecords(t, uri, `CREATE (:Domain {id:'acme-admin',created_by:'admin'}),
(:Domain {id:'zorbo-admin',created_by:'admin'}) RETURN 1`)
			ctx := t.Context()
			req := ingest.DomainOpRequest{OpID: "empty-merge-product-race", Action: "merge", From: "acme-admin", Target: "zorbo-admin"}
			store := &adminInterceptStore{Store: env.st, beforeDelete: func() error {
				bindAdminDomain(t, bound)
				return nil
			}}
			ingest.SetDomainOpStore(env.p, store)
			op, err := env.p.RunDomainOp(ctx, req)
			requireDomainHasProducts(t, err)
			if op.State != "incomplete" {
				t.Fatalf("op=%+v", op)
			}
			if ids, err := env.st.DomainDocumentIDs(ctx, req.From); err != nil || len(ids) != 0 {
				t.Fatalf("empty source should remain: ids=%v err=%v", ids, err)
			}
			_, err = env.p.RunDomainOp(ctx, req)
			requireDomainHasProducts(t, err)
		})
	}
}

func TestProductDomainOpWriteConflict(t *testing.T) {
	const src, dst = "acme-admin", "zorbo-admin"
	conflict := &neo4j.Neo4jError{
		Code: "Memgraph.TransientError.MemgraphError.MemgraphError",
		Msg:  "Cannot resolve conflicting transactions.",
	}
	limit := &neo4j.TransactionExecutionLimit{Cause: "retry budget exhausted", Errors: []error{conflict}}
	deadlock := &neo4j.Neo4jError{Code: "Neo.TransientError.Transaction.DeadlockDetected"}
	timeout := &neo4j.Neo4jError{Code: "Neo.TransientError.General.TransactionTimeout"}
	permanent := &neo4j.Neo4jError{Code: "Neo.ClientError.Statement.SyntaxError"}
	for _, tc := range []struct {
		name, action, bound, state string
		failure, wantError         error
	}{
		{"move source bound", "move", src, "incomplete", conflict, graph.ErrDomainHasProducts},
		{"move target bound", "move", dst, "incomplete", deadlock, graph.ErrDomainHasProducts},
		{"merge target bound after retries", "merge", dst, "incomplete", limit, graph.ErrDomainHasProducts},
		{"conflict without binding", "move", "", "failed", conflict, conflict},
		{"timeout with binding", "move", dst, "failed", timeout, timeout},
		{"permanent error with binding", "move", dst, "failed", permanent, permanent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := adminEnv(t)
			ctx := t.Context()
			req := ingest.DomainOpRequest{OpID: "product-write-conflict", Action: tc.action, Target: dst}
			fail := func() error {
				if tc.bound != "" {
					bindAdminDomain(t, tc.bound)
				}
				return fmt.Errorf("injected transaction failure: %w", tc.failure)
			}
			store := &adminInterceptStore{Store: env.st}
			if tc.action == "move" {
				req.DocIDs = []string{adminDoc(t, env, "one", src)}
				adminDoc(t, env, "target", dst)
				store.beforeMove = fail
			} else {
				uri := os.Getenv(graphtest.URIEnv)
				rawRecords(t, uri, `CREATE (:Domain {id:'acme-admin',created_by:'admin'}),
(:Domain {id:'zorbo-admin',created_by:'admin'}) RETURN 1`)
				req.From = src
				store.beforeDelete = fail
			}
			ingest.SetDomainOpStore(env.p, store)
			op, err := env.p.RunDomainOp(ctx, req)
			if !errors.Is(err, tc.wantError) || op.State != tc.state {
				t.Fatalf("operation=%+v error=%v, want state=%s error=%v", op, err, tc.state, tc.wantError)
			}
			stored, err := env.st.DomainOperation(ctx, req.OpID)
			if err != nil || stored.Op.State != tc.state {
				t.Fatalf("stored operation=%+v error=%v, want state=%s", stored.Op, err, tc.state)
			}
			if tc.action == "move" {
				assertAdminDomain(t, env, req.DocIDs[0], src)
			} else if _, err := env.st.DomainDocumentIDs(ctx, src); err != nil {
				t.Fatalf("merge source should remain: %v", err)
			}
		})
	}
}

func TestProductDomainOpCompletedReplay(t *testing.T) {
	for _, action := range []string{"move", "merge"} {
		t.Run(action, func(t *testing.T) {
			env := adminEnv(t)
			ctx := t.Context()
			id := adminDoc(t, env, "one", "acme-admin")
			adminDoc(t, env, "target", "zorbo-admin")
			req := ingest.DomainOpRequest{OpID: "completed-before-binding", Action: action, Target: "zorbo-admin"}
			if action == "move" {
				req.DocIDs = []string{id}
			} else {
				req.From = "acme-admin"
			}
			if op, err := env.p.RunDomainOp(ctx, req); err != nil || op.State != "completed" {
				t.Fatalf("initial operation=%+v error=%v", op, err)
			}
			stored, err := env.st.DomainOperation(ctx, req.OpID)
			if err != nil {
				t.Fatal(err)
			}
			bindAdminDomain(t, req.Target)
			op, err := env.p.RunDomainOp(ctx, req)
			if err != nil || !reflect.DeepEqual(op, stored.Op) {
				t.Fatalf("replayed operation=%+v error=%v, want stored operation=%+v", op, err, stored.Op)
			}
		})
	}
}

func TestProductDomainOpCompletedUndoReplay(t *testing.T) {
	env := adminEnv(t)
	ctx := t.Context()
	const source = "acme-admin"
	id := adminDoc(t, env, "one", source)
	req := ingest.DomainOpRequest{OpID: "completed-before-binding", Action: "move", DocIDs: []string{id}, Target: "zorbo-admin"}
	if op, err := env.p.RunDomainOp(ctx, req); err != nil || op.State != "completed" {
		t.Fatalf("initial move=%+v error=%v", op, err)
	}
	detectProcess(t, env, id)
	undo, err := env.p.UndoDomainOp(ctx, req.OpID)
	if err != nil || undo.State != "completed" {
		t.Fatalf("initial undo=%+v error=%v", undo, err)
	}
	assertAdminDomain(t, env, id, source)
	stored, err := env.st.DomainOperation(ctx, undo.OpID)
	if err != nil {
		t.Fatal(err)
	}
	bindAdminDomain(t, source)
	op, err := env.p.UndoDomainOp(ctx, req.OpID)
	if err != nil || !reflect.DeepEqual(op, stored.Op) {
		t.Fatalf("replayed undo=%+v error=%v, want stored operation=%+v", op, err, stored.Op)
	}
}
