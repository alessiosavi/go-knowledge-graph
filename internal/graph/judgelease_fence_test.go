package graph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
	driverconfig "github.com/neo4j/neo4j-go-driver/v6/neo4j/config"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j/log"
)

func TestFencedLeasePreservesAbsentSchemaMarker(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	foldTestWrite(t, st, `MATCH (m:GkgMeta {id:'extraction_schema'}) DELETE m`, nil)
	foldTestWrite(t, st, `CREATE (:Document {id:'legacy-fence-fixture', status:'done'})`, nil)
	if ok, err := st.AcquireLeaseExcluding(ctx, BackfillLeaseName, "backfill", time.Minute, false, ValidityApplyLease, RejudgeLeaseName); !ok || err != nil {
		t.Fatalf("fenced acquire=%t, %v", ok, err)
	}
	rows, err := st.runRead(ctx, `MATCH (m:GkgMeta {id:'extraction_schema'}) RETURN m`, nil)
	if err != nil || len(rows) != 0 {
		t.Errorf("fenced acquisition recreated the extraction schema marker: %v (%v)", rows, err)
	}
	assertLeaseFenceSequence(t, st, 1)
}

func TestLeaseFenceInterleavedAcquisition(t *testing.T) {
	for _, opponent := range []string{ValidityApplyLease, RejudgeLeaseName} {
		for _, first := range []string{opponent, BackfillLeaseName} {
			t.Run(opponent+"/first="+first, func(t *testing.T) {
				a := foldIntegrationStore(t)
				seedLeaseFence(t, a)
				ctx := t.Context()
				second := opponent
				if first == opponent {
					second = BackfillLeaseName
				}
				tx := beginLeaseWrite(t, a)
				if err := txLockLeaseExclusions(ctx, tx); err != nil {
					t.Fatal(err)
				}
				if err := txAssertLeaseExclusions(ctx, tx, []string{second}, first == RejudgeLeaseName); err != nil {
					t.Fatal(err)
				}
				var edgeOwner any
				if first == BackfillLeaseName {
					edgeOwner = "first"
				}
				if err := txRun(ctx, tx, `MERGE (l:JudgeLease {id:$name})
SET l.owner='first', l.expires_at=$expires, l.edge_backfill_owner=$edge_owner`, map[string]any{
					"name": first, "expires": time.Now().Add(time.Minute).UnixMilli(),
					"edge_owner": edgeOwner,
				}); err != nil {
					t.Fatal(err)
				}
				assertLeaseContenderRefused(t, first, second, tx.Commit)
				assertLeaseFenceSequence(t, a, 1)
			})
		}
	}
}

func TestLeaseFenceInterleavedValidityRenewal(t *testing.T) {
	a := foldIntegrationStore(t)
	seedLeaseFence(t, a)
	ctx := t.Context()
	foldTestWrite(t, a, `CREATE (:Document {id:'fence-doc',status:'done'}),
(:Entity {key:'fence-head'})-[:uses {doc_id:'fence-doc',chunk_id:'fence-chunk',judge:''}]->(:Entity {key:'fence-tail'})`, nil)
	// Let the old expiry pass while the row transaction holds its renewal.
	// A competing snapshot would otherwise reject the still-live old lease.
	expiry := time.Now().Add(2 * time.Second).Truncate(time.Millisecond)
	foldTestWrite(t, a, `CREATE (:JudgeLease {id:$name, owner:'first', expires_at:$expires})`,
		map[string]any{"name": ValidityApplyLease, "expires": expiry.UnixMilli()})
	row := ValidityWrite{
		Source: RejudgeIdentity{ID: "fence-identity", HeadKey: "fence-head", TailKey: "fence-tail", Relation: "uses", ChunkID: "fence-chunk"},
		DocID:  "fence-doc", Outcome: validity.OutcomeDeferred,
	}
	renewed, resume, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- a.ApplyValidityFact(ctx, "first", true, row, func(RejudgeFact, CatalogReader) error {
			close(renewed)
			<-resume
			return nil
		})
	}()
	var rowErr error
	finish := sync.OnceFunc(func() {
		close(resume)
		rowErr = <-done
	})
	select {
	case <-renewed:
		defer finish()
	case err := <-done:
		t.Fatalf("validity row returned before renewal: %v", err)
	}
	time.Sleep(time.Until(expiry))
	assertLeaseContenderRefused(t, ValidityApplyLease, BackfillLeaseName, func(context.Context) error {
		finish()
		return rowErr
	})
	assertLeaseFenceSequence(t, a, 1)
}

func TestStoredVerdictLeaseFenceRetriesFirstCreation(t *testing.T) {
	a := foldIntegrationStore(t)
	b := openLeaseContender(t, log.ToVoid())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	foldTestWrite(t, a, `MATCH (m:GkgMeta {id:'extraction_schema'}) DELETE m`, nil)
	if ok, err := b.AcquireLease(ctx, BackfillLeaseName, "backfill", time.Minute, false); !ok || err != nil {
		t.Fatalf("seed backfill lease=%t, %v", ok, err)
	}
	first := beginLeaseWrite(t, a)
	if err := txLockLeaseExclusions(ctx, first); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	applied, err := b.ApplyStoredFactVerdict(ctx, StoredFactRef{}, "confirmed", "", "", StoredFactVerdictOptions{
		BeforeWrite: func(ctx context.Context, tx neo4j.ManagedTransaction) error {
			attempts++
			if err := FenceLeaseExcluding(ctx, tx, BackfillLeaseName, "backfill", time.Minute, ValidityApplyLease, RejudgeLeaseName); err != nil {
				return err
			}
			if attempts == 1 {
				// Both transactions have created the fence in their own snapshot.
				// Commit the first while the contender is still inside its callback.
				return first.Commit(ctx)
			}
			return nil
		},
	})
	if applied || err != nil || attempts != 2 {
		t.Fatalf("apply=%t, err=%v, transactions=%d; want a single retry and no matching edge", applied, err, attempts)
	}
	assertLeaseFenceSequence(t, a, 2)
}

func beginLeaseWrite(t *testing.T, st *Store) neo4j.ExplicitTransaction {
	t.Helper()
	sess := st.driver.NewSession(t.Context(), neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	tx, err := sess.BeginTransaction(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Close(context.Background()) })
	return tx
}

func seedLeaseFence(t *testing.T, st *Store) {
	t.Helper()
	foldTestWrite(t, st, `CREATE (:GkgMeta {id:'judge_lease_fence',seq:0})`, nil)
}

func assertLeaseFenceSequence(t *testing.T, st *Store, want int64) {
	t.Helper()
	rows, err := st.runRead(t.Context(), `MATCH (m:GkgMeta {id:'judge_lease_fence'}) RETURN m.seq AS seq`, nil)
	if err != nil || len(rows) != 1 || rows[0].Values[0] != want {
		t.Fatalf("fence sequence rows=%v, err=%v; want one fence with seq=%d", rows, err, want)
	}
}

func openLeaseContender(t *testing.T, logger log.Logger) *Store {
	t.Helper()
	drv, err := neo4j.NewDriver(os.Getenv("GKG_TEST_MEMGRAPH_URI"), neo4j.NoAuth(), func(c *driverconfig.Config) {
		c.Log = logger
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close(context.Background()) })
	return &Store{driver: drv}
}

type leaseRetryLog struct {
	log.Logger
	retry  chan<- string
	resume <-chan struct{}
}

func (l leaseRetryLog) Debugf(_, _ string, format string, args ...any) {
	if strings.HasPrefix(format, "Retrying transaction") {
		l.retry <- fmt.Sprintf(format, args...)
		<-l.resume
	}
}

func assertLeaseContenderRefused(t *testing.T, first, second string, commit func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	retry, resume := make(chan string, 1), make(chan struct{})
	resumeRetry := sync.OnceFunc(func() { close(resume) })
	defer resumeRetry()
	b := openLeaseContender(t, leaseRetryLog{log.ToVoid(), retry, resume})
	type outcome struct {
		acquired bool
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		var r outcome
		switch second {
		case ValidityApplyLease:
			rollback, blocker, err := b.AcquireValidityApplyLease(ctx, "second")
			r = outcome{rollback != nil, err}
			if strings.Contains(blocker, first) {
				r.err = ErrLeaseExcluded
			}
		case RejudgeLeaseName:
			r.acquired, r.err = b.AcquireRejudgeLease(ctx, "second", time.Minute)
		case BackfillLeaseName:
			r.acquired, r.err = b.AcquireLeaseExcluding(ctx, BackfillLeaseName, "second", time.Minute, false, ValidityApplyLease, RejudgeLeaseName)
		}
		done <- r
	}()
	select {
	case msg := <-retry:
		t.Logf("%s retry with %s uncommitted: %s", second, first, msg)
	case r := <-done:
		t.Fatalf("%s finished while %s was uncommitted: acquired=%t, err=%v", second, first, r.acquired, r.err)
	case <-ctx.Done():
		t.Fatal("waiting for serialization conflict: ", ctx.Err())
	}
	if err := commit(ctx); err != nil {
		t.Fatal(err)
	}
	resumeRetry()
	select {
	case r := <-done:
		if r.acquired || !errors.Is(r.err, ErrLeaseExcluded) {
			t.Fatalf("%s after %s committed: acquired=%t, err=%v; want refusal", second, first, r.acquired, r.err)
		}
	case <-ctx.Done():
		t.Fatal("waiting for contender retry: ", ctx.Err())
	}
}
