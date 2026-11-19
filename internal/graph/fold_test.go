package graph

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func foldTestExceptions(t *testing.T, keys string) FoldExceptions {
	t.Helper()
	ex, err := ParseFoldExceptions(strings.NewReader(keys))
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func TestFoldKeyFor(t *testing.T) {
	scoped := ScopedEntityKey("fold-doc", "Zorbulo")
	ex := foldTestExceptions(t, "@demo:veto")
	for _, tc := range []struct{ key, name, want string }{
		{"zorbulo", "Zor-bulo", "|zorbulo"},
		{DomainKey("demo", "zorbulo"), "Zor-bulo", "@demo|zorbulo"},
		{scoped, "Zorbulo", ""},
		{DomainKey("demo", scoped), "Zorbulo", ""},
		{"@demo:veto", "Zorbulo", ""},
		{"zorbulo", "", ""},
		{"zorbulo", "™", ""},
		{"@demo:zorbulo", "!!", ""},
		{"double-possessive", "Bob's's", "|bobs"},
	} {
		got := FoldKeyFor(tc.key, tc.name, ex)
		if tc.want == "" {
			if got != nil {
				t.Errorf("FoldKeyFor(%q, %q) = %q, want nil", tc.key, tc.name, *got)
			}
		} else if got == nil || *got != tc.want {
			t.Errorf("FoldKeyFor(%q, %q) = %v, want %q", tc.key, tc.name, got, tc.want)
		}
	}
}

func TestVerifyFoldCandidate(t *testing.T) {
	ex := foldTestExceptions(t, "@demo:veto")
	current := &FoldRecord{Version: FoldKeyVersion, Exceptions: ex}
	old := &FoldRecord{Version: FoldKeyVersion - 1, Exceptions: ex}
	query := FoldKeyFor("@demo:zorbulo", "Zor-bulo", ex)
	if query == nil {
		t.Fatal("expected a query fold")
	}
	for _, tc := range []struct {
		label     string
		record    *FoldRecord
		key, name string
		query     *string
		want      bool
	}{
		{"match", current, "@demo:zor-bulo", "Zorbulo", query, true},
		{"missing record", nil, "@demo:zor-bulo", "Zorbulo", query, false},
		{"wrong version", old, "@demo:zor-bulo", "Zorbulo", query, false},
		{"stale index hit", current, "@demo:zor-bulo", "Zorbun", query, false},
		{"wrong scope", current, "@other:zor-bulo", "Zorbulo", query, false},
		{"vetoed candidate", current, "@demo:veto", "Zorbulo", query, false},
		{"vetoed query", current, "@demo:zor-bulo", "Zorbulo", FoldKeyFor("@demo:veto", "Zorbulo", ex), false},
		{"null folds", current, "@demo:zor-bulo", "!!!", nil, false},
		{"null candidate", current, "@demo:zor-bulo", "!!!", query, false},
		{"empty query", current, "@demo:zor-bulo", "!!!", new(""), false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			if got := VerifyFoldCandidate(tc.record, tc.key, tc.name, tc.query); got != tc.want {
				t.Errorf("VerifyFoldCandidate = %t, want %t", got, tc.want)
			}
		})
	}
}

// The rebuild and its counters are global. These tests require the disposable
// integration database and must run serially with the other graph tests.
func foldIntegrationStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("GKG_TEST_MEMGRAPH_URI")
	if uri == "" {
		t.Skip("requires a disposable Memgraph integration database")
	}
	st, err := Open(t.Context(), config.Config{Memgraph: config.MemgraphConfig{URI: uri}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	wipe := func() {
		// Clear vectors first: deleting a node that still has an embedding
		// leaves a stale vector-index entry that breaks later vector searches
		// in the shared test database (see txGCCount).
		foldTestWrite(t, st, "MATCH (n) WHERE n.embedding IS NOT NULL SET n.embedding = null", nil)
		foldTestWrite(t, st, "MATCH (n) DETACH DELETE n", nil)
	}
	wipe()
	t.Cleanup(wipe)
	if err := st.Bootstrap(t.Context()); err != nil {
		t.Fatal(err)
	}
	return st
}

func foldTestWrite(t *testing.T, st *Store, query string, params map[string]any) {
	t.Helper()
	if err := st.runWrite(context.Background(), query, params); err != nil {
		t.Fatal(err)
	}
}

func foldTestRebuild(t *testing.T, st *Store, ex *FoldExceptions, written, skipped int) FoldRebuildResult {
	t.Helper()
	result, err := st.RebuildFold(t.Context(), ex)
	if err != nil || result.RowsWritten != written || result.RowsSkipped != skipped {
		t.Fatalf("RebuildFold = %+v, %v, want %d written, %d skipped", result, err, written, skipped)
	}
	return result
}

func foldTestRecord(t *testing.T, st *Store) *FoldRecord {
	t.Helper()
	rec, err := st.ReadFoldRecord(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestFoldRebuildFreshAndSchemaGuard(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	if hasIndex, err := st.indexExists(ctx, "Entity", []string{"fold_key"}); err != nil {
		t.Fatal(err)
	} else if hasIndex {
		if err := st.runAuto(ctx, "DROP INDEX ON :Entity(fold_key)", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if have, err := st.indexExists(ctx, "Entity", []string{"fold_key"}); err != nil || have {
		t.Fatalf("Bootstrap fold index = %t, %v", have, err)
	}
	if rec := foldTestRecord(t, st); rec != nil {
		t.Fatalf("Bootstrap created fold record: %+v", rec)
	}
	for _, version := range []int{ExtractionSchemaVersion - 1, ExtractionSchemaVersion + 1} {
		foldTestWrite(t, st, "MATCH (m:GkgMeta {id:'extraction_schema'}) SET m.value=$v", map[string]any{"v": version})
		if _, err := st.RebuildFold(ctx, &FoldExceptions{}); err == nil {
			t.Fatalf("rebuilt non-current schema %d", version)
		}
		if have, err := st.indexExists(ctx, "Entity", []string{"fold_key"}); err != nil || have {
			t.Fatalf("schema %d refusal created fold index: %t, %v", version, have, err)
		}
	}
	foldTestWrite(t, st, "MATCH (m:GkgMeta {id:'extraction_schema'}) SET m.value=$v", map[string]any{"v": ExtractionSchemaVersion})
	for _, prop := range []string{"key", "id"} {
		if err := st.runAuto(ctx, "DROP INDEX ON :Entity("+prop+")", nil); err != nil {
			t.Fatal(err)
		}
	}
	foldTestRebuild(t, st, &FoldExceptions{}, 0, 0)
	for _, prop := range []string{"key", "id", "fold_key"} {
		if have, err := st.indexExists(ctx, "Entity", []string{prop}); err != nil || !have {
			t.Errorf("missing index %s after rebuild: %t, %v", prop, have, err)
		}
	}
	rec := foldTestRecord(t, st)
	if rec == nil || rec.Version != FoldKeyVersion || rec.RebuiltAt == "" || rec.RebuildOwner == "" ||
		rec.ExceptionsDigest != (FoldExceptions{}).Digest() || len(rec.Exceptions.Keys()) != 0 ||
		rec.RowsWritten != 0 || rec.RowsSkipped != 0 {
		t.Fatalf("fresh record = %+v", rec)
	}
	ex := foldTestExceptions(t, "zorbulo")
	if got := foldTestRebuild(t, st, &ex, 0, 0); !slices.Equal(got.MissingExceptions, []string{"zorbulo"}) {
		t.Fatalf("bare exception on a graph without domains: %+v", got)
	}
}

func TestBootstrapIndexesExistingGkgMeta(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	if err := st.runAuto(ctx, "DROP INDEX ON :GkgMeta(id)", nil); err != nil {
		t.Fatal(err)
	}
	foldTestWrite(t, st, `CREATE (:GkgMeta {id:'fixture', value:'preserved'})`, nil)
	for range 2 {
		if err := st.Bootstrap(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if have, err := st.indexExists(ctx, "GkgMeta", []string{"id"}); err != nil || !have {
		t.Fatalf("Bootstrap GkgMeta index = %t, %v", have, err)
	}
	rows, err := st.runRead(ctx, `MATCH (m:GkgMeta {id:'fixture'}) RETURN m.value AS value`, nil)
	if err != nil || len(rows) != 1 || getString(rows[0], "value") != "preserved" {
		t.Fatalf("existing GkgMeta marker changed: %v, %v", rows, err)
	}
}

func TestBootstrapMergedEntitySchemaOnPopulatedGraph(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	for _, prop := range []string{"old_id", "old_key"} {
		if err := st.runAuto(ctx, "DROP CONSTRAINT ON (m:MergedEntity) ASSERT m."+prop+" IS UNIQUE", nil); err != nil {
			t.Fatal(err)
		}
	}
	props := []string{"old_id", "old_key", "survivor_id"}
	for _, prop := range props {
		if err := st.runAuto(ctx, "DROP INDEX ON :MergedEntity("+prop+")", nil); err != nil {
			t.Fatal(err)
		}
	}
	const composite = `CONSTRAINT ON (m:MergedEntity) ASSERT m.old_id, m.old_key IS UNIQUE`
	if err := st.runAuto(ctx, "CREATE "+composite, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.runAuto(context.Background(), "DROP "+composite, nil); err != nil {
			t.Error(err)
		}
	})
	foldTestWrite(t, st, `CREATE (:Entity {id:'live', key:'zervix', name:'Zervix'}),
(:MergedEntity {old_id:'retired', old_key:'zer-vix', survivor_id:'live'})`, nil)
	for range 2 {
		if err := st.Bootstrap(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, prop := range props {
		if have, err := st.indexExists(ctx, "MergedEntity", []string{prop}); err != nil || !have {
			t.Errorf("index %s exists = %t, %v", prop, have, err)
		}
	}
	for _, duplicate := range []struct{ prop, oldID, oldKey string }{
		{"old_id", "retired", "other-key"},
		{"old_key", "other-id", "zer-vix"},
	} {
		if have, err := st.constraintExists(ctx, "MergedEntity", duplicate.prop); err != nil || !have {
			t.Errorf("constraint %s exists = %t, %v", duplicate.prop, have, err)
		}
		if err := st.runWrite(ctx, `CREATE (:MergedEntity {old_id:$id, old_key:$key, survivor_id:'live'})`, map[string]any{
			"id": duplicate.oldID, "key": duplicate.oldKey,
		}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "constraint") {
			t.Errorf("duplicate %s = %v, want a constraint error", duplicate.prop, err)
		}
	}
	for _, selector := range []string{"retired", "zer-vix"} {
		if view, err := st.EntityView(ctx, selector, false); err != nil || view.ID != "live" || view.ResolvedVia != "merged" {
			t.Errorf("redirect for %q after repeat Bootstrap = %+v, %v", selector, view, err)
		}
	}
}

func TestMergedEntityUniqueConstraintWithMissingOldID(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	if have, err := st.constraintExists(ctx, "MergedEntity", "old_id"); err != nil || !have {
		t.Fatalf("old_id unique constraint exists = %t, %v", have, err)
	}
	// Deliberately omit old_id to characterize what the database enforces.
	foldTestWrite(t, st, `CREATE (:MergedEntity {old_key:'retired-a', survivor_id:'live'})`, nil)
	foldTestWrite(t, st, `CREATE (:MergedEntity {old_key:'retired-b', survivor_id:'live'})`, nil)
	rows, err := st.runRead(ctx, `MATCH (m:MergedEntity) WHERE m.old_id IS NULL RETURN count(m) AS n`, nil)
	if err != nil || len(rows) != 1 || getInt(rows[0], "n") != 2 {
		t.Fatalf("count of ledger rows without old_id = %v, %v, want 2", rows, err)
	}
	t.Log("unique old_id constraint accepted both rows without old_id")
}

func TestFoldRebuildPopulatedAndIndexedRemoval(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	bareDoc := ScopedEntityKey("fold-doc", "Zorbulo")
	scopedDoc := DomainKey("demo", bareDoc)
	foldTestWrite(t, st, `CREATE
(:Entity {key:'@demo:zor-bulo', id:'a', name:'Zor-bulo'}),
(:Entity {key:'@demo:zorbulo', id:'b', name:'Zorbulo'}),
(:Entity {key:'@demo:other', id:'c', name:'Zorbon'}),
(:Entity {key:'@demo:nameless', id:'d', fold_key:'stale'}),
(:Entity {key:'@demo:emptyfold', id:'e', name:'©'}),
(:Entity {key:$bare, id:'f', name:'Zorbulo', fold_key:'stale'}),
(:Entity {key:$scoped, id:'g', name:'Zorbulo'}),
(:Entity {key:'no-id', name:'Zorbulo', fold_key:'skipped'}),
(:Entity {id:'no-key', name:'Zorbulo'})`, map[string]any{"bare": bareDoc, "scoped": scopedDoc})
	const hitsQuery = `MATCH (e:Entity {fold_key:$fold}) RETURN e.key AS key ORDER BY e.key`
	hits := func(query string, want ...string) {
		t.Helper()
		rows, err := st.runRead(ctx, query, map[string]any{"fold": "@demo|zorbulo"})
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(rows))
		for _, row := range rows {
			got = append(got, getString(row, "key"))
		}
		if !slices.Equal(got, want) {
			t.Fatalf("hits %v, want %v", got, want)
		}
	}

	foldTestRebuild(t, st, &FoldExceptions{}, 5, 2)
	hits(hitsQuery, "@demo:zor-bulo", "@demo:zorbulo")
	plan, err := st.runRead(ctx, "EXPLAIN "+hitsQuery, map[string]any{"fold": "@demo|zorbulo"})
	if err != nil || !slices.ContainsFunc(plan, func(r *neo4j.Record) bool {
		return strings.Contains(getString(r, "QUERY PLAN"), "ScanAllByLabelProperties") &&
			strings.Contains(getString(r, "QUERY PLAN"), "{fold_key}")
	}) {
		t.Fatalf("equality index plan = %v, %v", plan, err)
	}
	for _, key := range []string{"@demo:nameless", "@demo:emptyfold", bareDoc, scopedDoc} {
		requireStoredFold(t, st, key, nil)
	}
	requireStoredFold(t, st, "no-id", "skipped")
	foldTestRebuild(t, st, nil, 0, 2)
	foldTestWrite(t, st, `MATCH (e:Entity {id:'c'}) SET e.name = 'Zorbon New'`, nil)
	foldTestRebuild(t, st, nil, 1, 2)
	requireStoredFold(t, st, "@demo:other", "@demo|zorbonnew")

	ex := foldTestExceptions(t, "@demo:zorbulo\n@demo:absent\n")
	result := foldTestRebuild(t, st, &ex, 1, 2)
	if !slices.Equal(result.MissingExceptions, []string{"@demo:absent"}) {
		t.Error(result.MissingExceptions)
	}
	hits(hitsQuery, "@demo:zor-bulo")
	foldTestRebuild(t, st, nil, 0, 2)
	rec := foldTestRecord(t, st)
	if rec == nil || rec.ExceptionsDigest != ex.Digest() || !slices.Equal(rec.Exceptions.Keys(), []string{"@demo:absent", "@demo:zorbulo"}) ||
		rec.RowsWritten != 0 || rec.RowsSkipped != 2 {
		t.Fatalf("kept record = %+v", rec)
	}
	foldTestRebuild(t, st, &FoldExceptions{}, 1, 2)
	hits(hitsQuery, "@demo:zor-bulo", "@demo:zorbulo")
}

func requireStoredFold(t *testing.T, st *Store, key string, want any) {
	t.Helper()
	rows, err := st.runRead(t.Context(), `MATCH (e:Entity {key:$key}) RETURN e.fold_key AS fold_key`, map[string]any{"key": key})
	if err != nil || len(rows) != 1 {
		t.Fatalf("read key %q: %v, %v", key, rows, err)
	}
	if got, _ := rows[0].Get("fold_key"); got != want {
		t.Fatalf("fold for %q = %#v, want %#v", key, got, want)
	}
}

func seedFoldPages(t *testing.T, st *Store) {
	t.Helper()
	foldTestWrite(t, st, `UNWIND range(10000,12001) AS n
WITH toString(n) AS n CREATE (:Entity {key:'seed-' + n, id:n, name:'Zorbulo ' + n})`, nil)
}

func TestFoldRebuildInterruptedConverges(t *testing.T) {
	st := foldIntegrationStore(t)
	seedFoldPages(t, st)
	foldTestWrite(t, st, `CREATE (:Entity {id:'missing-key'})`, nil)
	foldTestRebuild(t, st, &FoldExceptions{}, 2002, 1)
	foldTestWrite(t, st, `MATCH (e:Entity) REMOVE e.fold_key`, nil)
	ex := foldTestExceptions(t, "absent")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := st.rebuildFold(ctx, &ex, cancel)
	if !errors.Is(err, context.Canceled) && (err == nil || !strings.Contains(err.Error(), "context canceled")) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if result.RowsWritten != foldBatchSize {
		t.Fatalf("interrupted rebuild = %+v", result)
	}
	if rec := foldTestRecord(t, st); rec == nil || rec.ExceptionsDigest != ex.Digest() ||
		rec.Status != "running" || rec.RebuiltAt != "" || rec.RowsWritten != 0 || rec.RowsSkipped != 0 {
		t.Fatalf("interrupted record = %+v", rec)
	}
	rows, err := st.runRead(t.Context(), `MATCH (m:GkgMeta {id:'fold'})
RETURN m.rebuilt_at AS rebuilt_at, m.rows_written AS rows_written, m.rows_skipped AS rows_skipped`, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read interrupted record properties: %v, %v", rows, err)
	}
	for _, prop := range []string{"rebuilt_at", "rows_written", "rows_skipped"} {
		if value, _ := rows[0].Get(prop); value != nil {
			t.Errorf("interrupted rebuild retained %s = %v", prop, value)
		}
	}
	foldTestRebuild(t, st, nil, 2, 1)
	if rec := foldTestRecord(t, st); rec == nil || rec.ExceptionsDigest != ex.Digest() ||
		rec.Status != "complete" || rec.RebuiltAt == "" || rec.RowsWritten != 2 || rec.RowsSkipped != 1 {
		t.Fatalf("completed record = %+v", rec)
	}
	foldTestRebuild(t, st, nil, 0, 1)
}

func TestFoldRebuildFinishRequiresRecordOwner(t *testing.T) {
	st := foldIntegrationStore(t)
	foldTestWrite(t, st, `CREATE (:Entity {key:'zorbulo', id:'existing', name:'Zorbulo'})`, nil)
	result, err := st.rebuildFold(t.Context(), &FoldExceptions{}, func() {
		// The lease still belongs to this run. Change the record owner so
		// the finish write must independently enforce ownership.
		foldTestWrite(t, st, `MATCH (m:GkgMeta {id:'fold'}) SET m.rebuild_owner = 'other-owner'`, nil)
	})
	if !errors.Is(err, errFoldLeaseLost) || result.RowsWritten != 1 {
		t.Fatalf("finish with wrong record owner = %+v, %v", result, err)
	}
	if rec := foldTestRecord(t, st); rec == nil || rec.RebuildOwner != "other-owner" ||
		rec.Status != "running" || rec.RebuiltAt != "" || rec.RowsWritten != 0 {
		t.Fatalf("finish changed another owner's record: %+v", rec)
	}
}

func TestFoldRebuildLeaseRefusalAndLoss(t *testing.T) {
	st := foldIntegrationStore(t)
	seedFoldPages(t, st)
	ctx := t.Context()
	ex := foldTestExceptions(t, "#fold-doc:zorbon")
	result, err := st.rebuildFold(ctx, &ex, func() {
		// The first batch is committed but the run still holds its lease.
		done := make(chan error, 1)
		go func() {
			_, err := st.RebuildFold(ctx, &FoldExceptions{})
			done <- err
		}()
		if err := <-done; !errors.Is(err, errFoldLeaseHeld) {
			t.Fatalf("second rebuild = %v, want lease refusal", err)
		}
		acquired, err := st.AcquireLease(ctx, foldLeaseName, "synthetic-takeover", foldLeaseTTL, true)
		if err != nil || !acquired {
			t.Fatalf("takeover = %t, %v", acquired, err)
		}
	})
	if !errors.Is(err, errFoldLeaseLost) || result.RowsWritten != foldBatchSize {
		t.Fatalf("rebuild after takeover = %+v, %v", result, err)
	}
	rec := foldTestRecord(t, st)
	if rec == nil || rec.RebuiltAt != "" || rec.ExceptionsDigest != ex.Digest() || rec.RebuildOwner == "synthetic-takeover" {
		t.Fatalf("lease loser overwrote record: %+v", rec)
	}
	requireStoredFold(t, st, "seed-12000", nil)
	if renewed, err := st.RenewLease(ctx, foldLeaseName, "synthetic-takeover", foldLeaseTTL); err != nil || !renewed {
		t.Fatalf("old cleanup touched new lease: %t, %v", renewed, err)
	}
}

func TestFoldRebuildConcurrentIngest(t *testing.T) {
	st := foldIntegrationStore(t)
	seedFoldPages(t, st)
	foldTestWrite(t, st, "CREATE (:Chunk {id:'fold-chunk'})", nil)
	ctx := t.Context()
	paused, resume := make(chan struct{}), make(chan struct{})
	type outcome struct {
		result FoldRebuildResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		first := true
		result, err := st.rebuildFold(ctx, &FoldExceptions{}, func() {
			if first {
				first = false
				close(paused)
				<-resume
			}
		})
		done <- outcome{result, err}
	}()
	select {
	case <-paused:
	case result := <-done:
		t.Fatalf("rebuild exited before first page: %+v", result)
	}
	// The existing key and the new scoped-domain key are behind the
	// rebuild's cursor. Ingest must set both hints using the active record.
	err := st.UpsertExtractionOpts(ctx, "fold-doc", "fold-chunk", "mock:1", []EntityUpsert{
		{Key: "seed-10000", Name: "Zorbulo Changed"},
		{Key: DomainKey("demo", "new"), Name: "Zorbulo New"},
	}, nil, WriteOptions{Domain: "demo"})
	close(resume)
	got := <-done
	if err != nil || got.err != nil || got.result.RowsWritten != 2002 {
		t.Fatalf("concurrent ingest = %v, rebuild = %+v", err, got)
	}
	requireStoredFold(t, st, "seed-10000", "|zorbulochanged")
	requireStoredFold(t, st, DomainKey("demo", "new"), "@demo|zorbulonew")
	foldTestRebuild(t, st, nil, 0, 0)
}

type foldConflictTx struct {
	neo4j.ManagedTransaction
	beforeGuard func() error
}

func (tx foldConflictTx) Run(ctx context.Context, query string, params map[string]any) (neo4j.Result, error) {
	if strings.Contains(query, "MATCH (l:JudgeLease") {
		if err := tx.beforeGuard(); err != nil {
			return nil, err
		}
	}
	return tx.ManagedTransaction.Run(ctx, query, params)
}

func TestFoldRebuildLosesLeaseInsideBatch(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	foldTestWrite(t, st, `CREATE (:Entity {key:'zorbulo', id:'existing', name:'Zorbulo', fold_key:'old'})`, nil)
	const owner = "synthetic-rebuild"
	const takeover = "synthetic-takeover"
	if ok, err := st.AcquireLease(ctx, foldLeaseName, owner, foldLeaseTTL, false); err != nil || !ok {
		t.Fatalf("acquire = %t, %v", ok, err)
	}
	ex, _, err := st.startFoldRebuild(ctx, owner, &FoldExceptions{})
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	err = st.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
		attempts++
		tx = foldConflictTx{tx, func() error {
			if attempts == 1 {
				// The page has already been read in this transaction.
				if ok, err := st.AcquireLease(ctx, foldLeaseName, takeover, foldLeaseTTL, true); err != nil || !ok {
					t.Fatalf("takeover = %t, %v", ok, err)
				}
			}
			return nil
		}}
		_, err := txRebuildFoldBatch(ctx, tx, owner, "", foldBatchSize, ex)
		return err
	})
	if !errors.Is(err, errFoldLeaseLost) || attempts != 2 {
		t.Fatalf("batch after takeover: attempts=%d, err=%v; want a conflict retry and lease loss", attempts, err)
	}
	requireStoredFold(t, st, "zorbulo", "old")
}

func TestFoldRebuildRetriesIngestConflict(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	foldTestWrite(t, st, `CREATE (:Chunk {id:'fold-chunk'}),
(:Entity {key:'zorbulo', id:'existing', name:'Zorbulo', fold_key:'old'})`, nil)
	owner := "synthetic-rebuild"
	if ok, err := st.AcquireLease(ctx, foldLeaseName, owner, foldLeaseTTL, false); err != nil || !ok {
		t.Fatalf("acquire = %t, %v", ok, err)
	}
	ex, _, err := st.startFoldRebuild(ctx, owner, &FoldExceptions{})
	if err != nil {
		t.Fatal(err)
	}
	var batch foldBatch
	attempts := 0
	err = st.runWriteTx(ctx, func(tx neo4j.ManagedTransaction) error {
		attempts++
		// Commit a real ingest on a second Bolt transaction after the
		// rebuild reads the old name and before it tries to write the fold.
		tx = foldConflictTx{tx, func() error {
			if attempts != 1 {
				return nil
			}
			return st.UpsertExtraction(ctx, "fold-doc", "fold-chunk", "mock:1",
				[]EntityUpsert{{Key: "zorbulo", Name: "Zorbulo New"}}, nil)
		}}
		var err error
		batch, err = txRebuildFoldBatch(ctx, tx, owner, "", foldBatchSize, ex)
		return err
	})
	if err != nil || attempts != 2 || batch.written != 0 {
		t.Fatalf("conflicting page = %+v, attempts=%d, err=%v", batch, attempts, err)
	}
	t.Logf("Memgraph serialization conflict retried, attempts=%d", attempts)
	requireStoredFold(t, st, "zorbulo", "|zorbulonew")
	if err := st.ReleaseLease(ctx, foldLeaseName, owner); err != nil {
		t.Fatal(err)
	}
	foldTestRebuild(t, st, nil, 0, 0)
}

func TestFoldHintsIngestAndRepairNames(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	foldTestWrite(t, st, `CREATE (:Chunk {id:'fold-chunk'}),
(:Entity {id:'repair', key:'@demo:repair', name:'(Zorbulo)'}),
(:Entity {id:'no-record', key:'@demo:no-record', name:'Zorbon', fold_key:'legacy'})`, nil)
	key := DomainKey("demo", "ingest")
	upsert := func(key, name string) {
		t.Helper()
		if err := st.UpsertExtractionOpts(ctx, "fold-doc", "fold-chunk", "mock:1",
			[]EntityUpsert{{Key: key, Name: name}}, nil, WriteOptions{Domain: "demo"}); err != nil {
			t.Fatal(err)
		}
	}
	upsert(key, "Zorbulo")
	upsert("@demo:no-record", "Zorbon New")
	requireStoredFold(t, st, key, nil)
	requireStoredFold(t, st, "@demo:no-record", "legacy")
	if _, changed, err := st.RepairEntityNames(ctx, false); err != nil || changed != 1 {
		t.Fatalf("repair before record = %d, %v", changed, err)
	}
	requireStoredFold(t, st, "@demo:repair", nil)
	ex := foldTestExceptions(t, "@demo:repair\n@demo:veto")
	foldTestRebuild(t, st, &ex, 2, 0)
	upsert(DomainKey("demo", "new"), "Zorbulo New")
	requireStoredFold(t, st, "@demo:new", "@demo|zorbulonew")
	upsert("@demo:veto", "Zorbulo")
	requireStoredFold(t, st, "@demo:veto", nil)
	foldTestWrite(t, st, `MATCH (e:Entity {key:'@demo:ingest'}) SET e.fold_key='stale'`, nil)
	upsert(key, "Zorbulo")
	requireStoredFold(t, st, key, "stale")
	upsert(key, "Zorbon")
	requireStoredFold(t, st, key, "@demo|zorbon")
	upsert(key, "™")
	requireStoredFold(t, st, key, nil)

	foldTestWrite(t, st, `MATCH (e:Entity {key:'@demo:repair'}) SET e.name='(Zorbulo)', e.fold_key='stale'`, nil)
	foldTestWrite(t, st, `CREATE (:Entity {id:'repair-peer', key:'@demo:repair-peer', name:'(Zorbulo)', fold_key:'stale'})`, nil)
	if _, changed, err := st.RepairEntityNames(ctx, true); err != nil || changed != 2 {
		t.Fatalf("dry-run repair = %d, %v", changed, err)
	}
	requireStoredFold(t, st, "@demo:repair", "stale")
	if _, changed, err := st.RepairEntityNames(ctx, false); err != nil || changed != 2 {
		t.Fatalf("actual repair = %d, %v", changed, err)
	}
	requireStoredFold(t, st, "@demo:repair", nil)
	requireStoredFold(t, st, "@demo:repair-peer", "@demo|zorbulo")
	foldTestRebuild(t, st, nil, 0, 0)
}
