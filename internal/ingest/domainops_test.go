package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func adminEnv(t *testing.T) pipelineEnv {
	t.Helper()
	env, _ := detectionEnv(t, nil)
	uri := os.Getenv(graphtest.URIEnv)
	rawRecords(t, uri, `MERGE (m:GkgMeta {id:'extraction_schema'}) SET m.value=4 RETURN m`)
	t.Cleanup(func() {
		rawRecords(t, uri, fmt.Sprintf(`MATCH (m:GkgMeta {id:'extraction_schema'}) SET m.value=%d RETURN m`, graph.ExtractionSchemaVersion))
		rawRecords(t, uri, `MATCH (n:Domain) WHERE n.created_by='admin' DETACH DELETE n RETURN count(*)`)
		rawRecords(t, uri, `MATCH (n:DomainOp) DETACH DELETE n RETURN count(*)`)
	})
	return env
}
func adminDoc(t *testing.T, env pipelineEnv, suffix, slug string) string {
	t.Helper()
	id := detectSubmit(t, env, suffix, "Acme-Max explores Zorbo with Quillon "+suffix+".", "")
	if err := env.st.AssignDomain(t.Context(), graph.Assignment{DocID: id, Domain: slug, CreatedBy: "admin", Profile: domain.Profile{"acme": 1}, Decision: graph.DecisionRecord{Action: "seed"}}); err != nil {
		t.Fatal(err)
	}
	detectProcess(t, env, id)
	return id
}
func assertAdminDomain(t *testing.T, env pipelineEnv, id, slug string) {
	t.Helper()
	d, err := env.st.Document(t.Context(), id)
	if err != nil || d.Domain != slug {
		t.Fatalf("document=%+v error=%v want=%s", d, err, slug)
	}
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (:Document {id:'%s'})-[:HAS_CHUNK]->(c:Chunk) OPTIONAL MATCH (c)-[:MENTIONS]->(e:Entity) RETURN c.domain AS d,c.domain_tok AS tok,e.key AS key`, id))
	for _, r := range rows {
		got, _ := r.Get("d")
		tok, _ := r.Get("tok")
		key, _ := r.Get("key")
		if got != slug || tok != domain.DomainToken(slug) {
			t.Fatalf("chunk domain=%v token=%v", got, tok)
		}
		if s, ok := key.(string); ok && !strings.HasPrefix(s, "@"+slug+":") && !strings.HasPrefix(s, "#"+id+":") {
			t.Fatalf("wrong entity key %s", s)
		}
	}
}
func TestMoveThenUndo(t *testing.T) {
	env := adminEnv(t)
	id := adminDoc(t, env, "one", "acme-admin")
	beforeKeys := adminEntityKeys(t, id)
	if len(beforeKeys) == 0 {
		t.Fatal("fixture has no entity keys")
	}
	if err := env.st.PutJudgeVerdict(t.Context(), graph.JudgeVerdict{Key: id + "-verdict", DocID: id, Status: "confirmed"}); err != nil {
		t.Fatal(err)
	}
	op, err := env.p.RunDomainOp(t.Context(), ingest.DomainOpRequest{Action: "move", DocIDs: []string{id}, Target: "zorbo-admin"})
	if err != nil || op.State != "completed" {
		t.Fatalf("op=%+v err=%v", op, err)
	}
	assertAdminDomain(t, env, id, "zorbo-admin")
	if _, ok, err := env.st.JudgeVerdict(t.Context(), id+"-verdict"); err != nil || !ok {
		t.Fatalf("verdict cache lost: found=%v err=%v", ok, err)
	}
	if _, err := env.p.UndoDomainOp(t.Context(), op.OpID); !errors.Is(err, graph.ErrNotDone) {
		t.Fatalf("early undo=%v", err)
	}
	detectProcess(t, env, id)
	assertAdminDomain(t, env, id, "zorbo-admin")
	undo, err := env.p.UndoDomainOp(t.Context(), op.OpID)
	if err != nil || undo.OpID == op.OpID || undo.Action != "undo" {
		t.Fatalf("undo=%+v err=%v", undo, err)
	}
	detectProcess(t, env, id)
	assertAdminDomain(t, env, id, "acme-admin")
	if got := adminEntityKeys(t, id); !slices.Equal(got, beforeKeys) {
		t.Fatalf("undo keys=%v want=%v", got, beforeKeys)
	}
}
func TestMergeThenUndo(t *testing.T) {
	env := adminEnv(t)
	a := adminDoc(t, env, "one", "acme-admin")
	b := adminDoc(t, env, "two", "zorbo-admin")
	op, err := env.p.RunDomainOp(t.Context(), ingest.DomainOpRequest{Action: "merge", From: "acme-admin", Target: "zorbo-admin"})
	if err != nil {
		t.Fatal(err)
	}
	uri := os.Getenv(graphtest.URIEnv)
	if got := rawCount(t, uri, `MATCH (d:Domain {id:'zorbo-admin'}) RETURN d.product_guard AS n`); got != 2 {
		t.Errorf("merge move and deletion: target product_guard=%d, want 2", got)
	}
	rows, err := env.st.ListDomains(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rows {
		if d.ID == "acme-admin" {
			t.Fatal("empty merged domain remains")
		}
	}
	detectProcess(t, env, a)
	assertAdminDomain(t, env, a, "zorbo-admin")
	if _, err := env.p.UndoDomainOp(t.Context(), op.OpID); err != nil {
		t.Fatal(err)
	}
	if got := rawCount(t, uri, `MATCH (d:Domain {id:'zorbo-admin'}) RETURN d.product_guard AS n`); got != 3 {
		t.Errorf("undo: source product_guard=%d, want 3", got)
	}
	if got := rawCount(t, uri, `MATCH (d:Domain {id:'acme-admin'}) RETURN d.product_guard AS n`); got != 1 {
		t.Errorf("undo: recreated target product_guard=%d, want 1", got)
	}
	detectProcess(t, env, a)
	assertAdminDomain(t, env, a, "acme-admin")
	assertAdminDomain(t, env, b, "zorbo-admin")
}
func TestMoveClearsVectors(t *testing.T) {
	env := adminEnv(t)
	id := adminDoc(t, env, "one", "acme-admin")
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (:Document {id:'%s'})-[:HAS_CHUNK]->(:Chunk)-[:MENTIONS]->(e:Entity) RETURN e.key AS key LIMIT 1`, id))
	key, _ := rows[0].Get("key")
	vec := graphtest.Vector(id, 768)
	if err := env.st.EnsureVectorIndex(t.Context(), 768); err != nil {
		t.Fatal(err)
	}
	if _, err := env.st.SetEntityEmbeddings(t.Context(), []string{graph.EntityID(key.(string))}, [][]float64{vec}); err != nil {
		t.Fatal(err)
	}
	hitsBefore, err := env.st.EntityVectorSearch(t.Context(), vec, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, hit := range hitsBefore {
		if hit == graph.EntityID(key.(string)) {
			found = true
		}
	}
	if !found {
		t.Fatal("positive control: entity not indexed before move")
	}
	if _, err := env.p.RunDomainOp(t.Context(), ingest.DomainOpRequest{Action: "move", DocIDs: []string{id}, Target: "zorbo-admin"}); err != nil {
		t.Fatal(err)
	}
	hits, err := env.st.EntityVectorSearch(t.Context(), vec, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit == graph.EntityID(key.(string)) {
			t.Fatalf("deleted entity returned: %s", hit)
		}
	}
}
func TestResumeOp(t *testing.T) {
	env := adminEnv(t)
	a := adminDoc(t, env, "one", "acme-admin")
	b := adminDoc(t, env, "two", "acme-admin")
	store := &adminInterceptStore{Store: env.st, failAt: 2}
	ingest.SetDomainOpStore(env.p, store)
	req := ingest.DomainOpRequest{OpID: "resume-acme", Action: "move", DocIDs: []string{a, b}, Target: "zorbo-admin"}
	op, err := env.p.RunDomainOp(t.Context(), req)
	if err == nil || op.State != "failed" {
		t.Fatalf("op=%+v err=%v", op, err)
	}
	op, err = env.p.RunDomainOp(t.Context(), req)
	if err != nil || op.State != "completed" {
		t.Fatalf("resume=%+v err=%v", op, err)
	}
	records, err := env.st.DomainDecisions(t.Context(), req.OpID)
	if err != nil || len(records) != 2 {
		t.Fatalf("decisions=%+v err=%v", records, err)
	}
	// A repeated completed request must not reset/requeue already moved documents.
	if _, err = env.p.RunDomainOp(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a, b} {
		detectProcess(t, env, id)
		assertAdminDomain(t, env, id, "zorbo-admin")
	}
}
func TestUndoBeforeDone(t *testing.T) {
	env := adminEnv(t)
	id := adminDoc(t, env, "one", "acme-admin")
	op, err := env.p.RunDomainOp(t.Context(), ingest.DomainOpRequest{Action: "move", DocIDs: []string{id}, Target: "zorbo-admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.p.UndoDomainOp(context.Background(), op.OpID); !errors.Is(err, graph.ErrNotDone) {
		t.Fatalf("undo=%v", err)
	}
	assertAdminDomain(t, env, id, "zorbo-admin")
}
func TestMoveThenRechunk(t *testing.T) {
	env := adminEnv(t)
	id := detectSubmit(t, env, "many", strings.Repeat("Acme-Max explores Zorbo with Quillon every morning. ", 40), "")
	if err := env.st.AssignDomain(t.Context(), graph.Assignment{DocID: id, Domain: "acme-admin", CreatedBy: "admin", Profile: domain.Profile{"acme": 1}}); err != nil {
		t.Fatal(err)
	}
	detectProcess(t, env, id)
	before, err := env.st.Chunks(t.Context(), id)
	if err != nil || len(before) < 2 {
		t.Fatalf("initial chunks=%d err=%v", len(before), err)
	}
	if _, err := env.p.RunDomainOp(t.Context(), ingest.DomainOpRequest{Action: "move", DocIDs: []string{id}, Target: "zorbo-admin"}); err != nil {
		t.Fatal(err)
	}
	detectProcess(t, env, id)
	d, _ := env.st.Document(t.Context(), id)
	r, err := env.p.Submit(t.Context(), ingest.Input{Kind: "text", Source: d.Source, Text: "Acme-Max and Zorbo."})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.st.DeleteDocument(context.Background(), r.DocumentID) })
	detectProcess(t, env, r.DocumentID)
	assertAdminDomain(t, env, r.DocumentID, "zorbo-admin")
	after, err := env.st.Chunks(t.Context(), r.DocumentID)
	if err != nil || len(after) >= len(before) {
		t.Fatalf("rechunk before=%d after=%d err=%v", len(before), len(after), err)
	}
	t.Logf("rechunk_before=%d rechunk_after=%d", len(before), len(after))
}

func TestUndoRejectsOperationIDCollision(t *testing.T) {
	env := adminEnv(t)
	id := adminDoc(t, env, "one", "acme-admin")
	op, err := env.p.RunDomainOp(t.Context(), ingest.DomainOpRequest{OpID: "collision-acme", Action: "move", DocIDs: []string{id}, Target: "zorbo-admin"})
	if err != nil {
		t.Fatal(err)
	}
	detectProcess(t, env, id)
	other := adminDoc(t, env, "two", "acme-admin")
	_, err = env.p.RunDomainOp(t.Context(), ingest.DomainOpRequest{OpID: op.OpID + "-undo", Action: "move", DocIDs: []string{other}, Target: "zorbo-admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.p.UndoDomainOp(t.Context(), op.OpID); !errors.Is(err, graph.ErrInvalidDomainOp) {
		t.Fatalf("unrelated operation accepted as undo: %v", err)
	}
	assertAdminDomain(t, env, id, "zorbo-admin")
}

func TestDomainOpQueuesStoredChunks(t *testing.T) {
	env := adminEnv(t)
	id := adminDoc(t, env, "one", "acme-admin")
	// A fresh serve pipeline has no pending payload and no text loader.
	env.p = ingest.NewPipeline(env.cfg, env.st, &detectProvider{}, nil, nil)
	env.p.SetWriteMode(ingest.WriteMode{Domains: true, Hygiene: true})
	reqCtx, cancel := context.WithCancel(t.Context())
	op, err := env.p.RunDomainOp(reqCtx, ingest.DomainOpRequest{Action: "move", DocIDs: []string{id}, Target: "zorbo-admin"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stop := context.WithCancel(t.Context())
	defer stop()
	(&ingest.Worker{Pipeline: env.p, Workers: 1}).Start(workerCtx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, err := env.st.Document(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if d.Status == graph.StatusDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued op=%s document=%+v", op.OpID, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertAdminDomain(t, env, id, "zorbo-admin")
}

func adminEntityKeys(t *testing.T, id string) []string {
	t.Helper()
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (:Document {id:'%s'})-[:HAS_CHUNK]->(:Chunk)-[:MENTIONS]->(e:Entity) RETURN DISTINCT e.key AS key ORDER BY key`, id))
	out := []string{}
	for _, r := range rows {
		key, _ := r.Get("key")
		out = append(out, key.(string))
	}
	return out
}

// Intercept domain mutation boundaries around the real store.
type adminInterceptStore struct {
	*graph.Store
	beforeMove       func() error
	beforeDelete     func() error
	afterMove        func()
	afterDeleteCheck func()
	calls, failAt    int
}

func (s *adminInterceptStore) MoveDocument(ctx context.Context, id, slug, op, action, by string) error {
	s.calls++
	if s.beforeMove != nil {
		f := s.beforeMove
		s.beforeMove = nil
		if err := f(); err != nil {
			return err
		}
	}
	if s.calls == s.failAt {
		return errors.New("injected move failure")
	}
	if err := s.Store.MoveDocument(ctx, id, slug, op, action, by); err != nil {
		return err
	}
	if s.afterMove != nil {
		f := s.afterMove
		s.afterMove = nil
		f()
	}
	return nil
}

func TestMergeSupersededDocument(t *testing.T) {
	env := adminEnv(t)
	old := adminDoc(t, env, "old", "acme-admin")
	live := adminDoc(t, env, "live", "acme-admin")
	adminDoc(t, env, "target", "zorbo-admin")
	if err := env.st.Supersede(t.Context(), old, live); err != nil {
		t.Fatal(err)
	}
	op, err := env.p.RunDomainOp(t.Context(), ingest.DomainOpRequest{OpID: "superseded-merge", Action: "merge", From: "acme-admin", Target: "zorbo-admin"})
	if err != nil || op.State != "completed" {
		t.Fatalf("merge=%+v err=%v", op, err)
	}
	doc, err := env.st.Document(t.Context(), old)
	if err != nil || doc.Status != graph.StatusSuperseded || doc.Domain != "zorbo-admin" {
		t.Fatalf("terminal document=%+v err=%v", doc, err)
	}
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (d:Document {id:'%s'}) RETURN d.domain_tok AS tok`, old))
	if tok, _ := rows[0].Get("tok"); tok != domain.DomainToken("zorbo-admin") {
		t.Fatalf("terminal domain token=%v", tok)
	}
	if _, err := env.st.DomainDocumentIDs(t.Context(), "acme-admin"); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("source remains: %v", err)
	}
	if err := env.p.Process(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	doc, _ = env.st.Document(t.Context(), old)
	if doc.Status != graph.StatusSuperseded {
		t.Fatalf("terminal document requeued: %+v", doc)
	}
	detectProcess(t, env, live)
	undo, err := env.p.UndoDomainOp(t.Context(), op.OpID)
	if err != nil || undo.State != "completed" {
		t.Fatalf("undo=%+v err=%v", undo, err)
	}
	assertAdminDomain(t, env, live, "acme-admin")
	assertAdminDomain(t, env, old, "zorbo-admin")
}

func TestDomainOpPreflightNoChanges(t *testing.T) {
	for _, action := range []string{"move", "merge"} {
		for _, status := range []graph.Status{graph.StatusFailed, graph.StatusReceived, graph.StatusFetched, graph.StatusChunked, graph.StatusExtracted, graph.StatusMerged, graph.StatusVerifying} {
			t.Run(action+"-"+string(status), func(t *testing.T) {
				env := adminEnv(t)
				live := adminDoc(t, env, "live", "acme-admin")
				bad := adminDoc(t, env, "busy", "acme-admin")
				other := adminDoc(t, env, "other-busy", "acme-admin")
				adminDoc(t, env, "target", "zorbo-admin")
				beforeKeys := adminEntityKeys(t, live)
				for _, id := range []string{bad, other} {
					if err := env.st.SetStatus(t.Context(), id, status, "fixture"); err != nil {
						t.Fatal(err)
					}
				}
				req := ingest.DomainOpRequest{OpID: "preflight-acme", Action: action, DocIDs: []string{live, bad, other}, Target: "zorbo-admin"}
				if action == "merge" {
					req.From = "acme-admin"
					req.DocIDs = nil
				}
				_, err := env.p.RunDomainOp(t.Context(), req)
				if !errors.Is(err, graph.ErrNotDone) || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), other) {
					t.Fatalf("blocked ids missing: %v", err)
				}
				for _, id := range []string{live, bad, other} {
					assertAdminDomain(t, env, id, "acme-admin")
				}
				doc, _ := env.st.Document(t.Context(), live)
				if doc.Status != graph.StatusDone || !slices.Equal(adminEntityKeys(t, live), beforeKeys) {
					t.Fatalf("live document changed: %+v", doc)
				}
				if _, err := env.st.DomainOperation(t.Context(), req.OpID); !errors.Is(err, graph.ErrNotFound) {
					t.Fatalf("rejected operation persisted: %v", err)
				}
				records, err := env.st.DomainDecisions(t.Context(), req.OpID)
				if err != nil || len(records) != 0 {
					t.Fatalf("rejected operation ledger=%+v err=%v", records, err)
				}
			})
		}
	}
}

func TestUndoAfterLaterSupersede(t *testing.T) {
	env := adminEnv(t)
	old := adminDoc(t, env, "old", "acme-admin")
	live := adminDoc(t, env, "live", "acme-admin")
	op, err := env.p.RunDomainOp(t.Context(), ingest.DomainOpRequest{OpID: "later-supersede", Action: "move", DocIDs: []string{old, live}, Target: "zorbo-admin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{old, live} {
		detectProcess(t, env, id)
	}
	newer := adminDoc(t, env, "newer", "zorbo-admin")
	if err := env.st.Supersede(t.Context(), old, newer); err != nil {
		t.Fatal(err)
	}
	undo, err := env.p.UndoDomainOp(t.Context(), op.OpID)
	if err != nil || undo.State != "completed" {
		t.Fatalf("undo=%+v err=%v", undo, err)
	}
	assertAdminDomain(t, env, live, "acme-admin")
	assertAdminDomain(t, env, old, "zorbo-admin")
	assertAdminDomain(t, env, newer, "zorbo-admin")
	if !slices.Equal(undo.Skipped, []string{old}) {
		t.Fatalf("skipped result=%v", undo.Skipped)
	}
	stored, err := env.st.DomainOperation(t.Context(), undo.OpID)
	if err != nil || !slices.Equal(stored.Op.Skipped, []string{old}) {
		t.Fatalf("stored skipped=%v err=%v", stored.Op.Skipped, err)
	}
	if _, err := env.p.UndoDomainOp(t.Context(), op.OpID); err != nil {
		t.Fatalf("repeat undo: %v", err)
	}
}

func TestMergeConcurrentDocumentIncomplete(t *testing.T) {
	env := adminEnv(t)
	a := adminDoc(t, env, "one", "acme-admin")
	b := adminDoc(t, env, "two", "acme-admin")
	late := adminDoc(t, env, "late", "zorbo-admin")
	store := &adminInterceptStore{Store: env.st, afterMove: func() {
		if err := env.st.AssignDomain(t.Context(), graph.Assignment{DocID: late, Domain: "acme-admin", CreatedBy: "admin", Profile: domain.Profile{"quillon": 1}}); err != nil {
			t.Fatal(err)
		}
	}}
	ingest.SetDomainOpStore(env.p, store)
	req := ingest.DomainOpRequest{OpID: "concurrent-merge", Action: "merge", From: "acme-admin", Target: "zorbo-admin"}
	op, err := env.p.RunDomainOp(t.Context(), req)
	if err != nil || op.State != "incomplete" {
		t.Fatalf("merge=%+v err=%v", op, err)
	}
	stored, err := env.st.DomainOperation(t.Context(), op.OpID)
	if err != nil || stored.Op.State != "incomplete" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if !slices.Equal(op.Remaining, []string{late}) || !slices.Equal(stored.Op.Remaining, []string{late}) {
		t.Fatalf("remaining result=%v stored=%v", op.Remaining, stored.Op.Remaining)
	}
	if err := env.st.SetStatus(t.Context(), late, graph.StatusFailed, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.p.RunDomainOp(t.Context(), req); !errors.Is(err, graph.ErrNotDone) || !strings.Contains(err.Error(), late) {
		t.Fatalf("retry preflight=%v", err)
	}
	stored, err = env.st.DomainOperation(t.Context(), op.OpID)
	if err != nil || stored.Op.State != "incomplete" || !slices.Equal(stored.Op.Remaining, []string{late}) || len(stored.Op.Docs) != 2 {
		t.Fatalf("refused retry changed operation: %+v err=%v", stored, err)
	}
	if err := env.st.SetStatus(t.Context(), late, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a, b} {
		detectProcess(t, env, id)
	}
	op, err = env.p.RunDomainOp(t.Context(), req)
	if err != nil || op.State != "completed" || len(op.Remaining) != 0 {
		t.Fatalf("retry=%+v err=%v", op, err)
	}
	for _, id := range []string{a, b, late} {
		assertAdminDomain(t, env, id, "zorbo-admin")
	}
	records, err := env.st.DomainDecisions(t.Context(), req.OpID)
	if err != nil || len(records) != 3 {
		t.Fatalf("decisions=%+v err=%v", records, err)
	}
	if _, err := env.st.DomainDocumentIDs(t.Context(), "acme-admin"); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("source remains: %v", err)
	}
}

func TestUndoOfUndoWithSkippedMembers(t *testing.T) {
	env := adminEnv(t)
	old := adminDoc(t, env, "old", "acme-admin")
	live := adminDoc(t, env, "live", "acme-admin")
	op, err := env.p.RunDomainOp(t.Context(), ingest.DomainOpRequest{Action: "move", DocIDs: []string{old, live}, Target: "zorbo-admin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{old, live} {
		detectProcess(t, env, id)
	}
	if err := env.st.Supersede(t.Context(), old, live); err != nil {
		t.Fatal(err)
	}
	undo, err := env.p.UndoDomainOp(t.Context(), op.OpID)
	if err != nil {
		t.Fatal(err)
	}
	detectProcess(t, env, live)
	if _, err := env.p.UndoDomainOp(t.Context(), undo.OpID); err != nil {
		t.Fatalf("undo of undo: %v", err)
	}
	assertAdminDomain(t, env, live, "zorbo-admin")
}

func (s *adminInterceptStore) DeleteEmptyDomain(ctx context.Context, slug string, destinations ...string) (bool, error) {
	if s.beforeDelete != nil {
		f := s.beforeDelete
		s.beforeDelete = nil
		if err := f(); err != nil {
			return false, err
		}
	}
	deleted, err := s.Store.DeleteEmptyDomain(ctx, slug, destinations...)
	if err == nil && !deleted && s.afterDeleteCheck != nil {
		f := s.afterDeleteCheck
		s.afterDeleteCheck = nil
		f()
	}
	return deleted, err
}
func TestMergeSourceEmptiedDuringCompletion(t *testing.T) {
	env := adminEnv(t)
	adminDoc(t, env, "one", "acme-admin")
	late := adminDoc(t, env, "late", "zorbo-admin")
	store := &adminInterceptStore{Store: env.st, afterMove: func() {
		if err := env.st.AssignDomain(t.Context(), graph.Assignment{DocID: late, Domain: "acme-admin", CreatedBy: "admin", Profile: domain.Profile{"quillon": 1}}); err != nil {
			t.Fatal(err)
		}
	}, afterDeleteCheck: func() {
		if err := env.st.AssignDomain(t.Context(), graph.Assignment{DocID: late, Domain: "zorbo-admin", CreatedBy: "admin", Profile: domain.Profile{"quillon": 1}}); err != nil {
			t.Fatal(err)
		}
	}}
	ingest.SetDomainOpStore(env.p, store)
	req := ingest.DomainOpRequest{OpID: "empty-race", Action: "merge", From: "acme-admin", Target: "zorbo-admin"}
	op, err := env.p.RunDomainOp(t.Context(), req)
	if err != nil || op.State != "incomplete" {
		t.Fatalf("undeleted source marked complete: op=%+v err=%v", op, err)
	}
	op, err = env.p.RunDomainOp(t.Context(), req)
	if err != nil || op.State != "completed" {
		t.Fatalf("retry=%+v err=%v", op, err)
	}
	if _, err := env.st.DomainDocumentIDs(t.Context(), "acme-admin"); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("source remains: %v", err)
	}
}
