package graph_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestJudgeVerdictPersistence(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	uri := os.Getenv(graphtest.URIEnv)
	key := fmt.Sprintf("zorbo-verdict-%d", time.Now().UnixNano())
	t.Cleanup(func() { rawRun(t, uri, fmt.Sprintf(`MATCH (v:JudgeVerdict {key: '%s'}) DELETE v`, key)) })
	if _, ok, err := st.JudgeVerdict(ctx, key); err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v", ok, err)
	}
	for _, status := range []string{"confirmed", "unclear", "refuted"} {
		want := graph.JudgeVerdict{Key: key, Status: status, Reason: "Zorbó \"星\"", Model: "test-model", PromptVersion: "1", Confidence: 0.8, At: time.Now().UTC()}
		if err := st.PutJudgeVerdict(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, ok, err := st.JudgeVerdict(ctx, key)
		if err != nil || !ok || got != want {
			t.Fatalf("verdict = %+v, ok=%v err=%v, want %+v", got, ok, err, want)
		}
	}
	if n := rawCount(t, uri, fmt.Sprintf(`MATCH (v:JudgeVerdict {key: '%s'}) RETURN count(v) AS n`, key)); n != 1 {
		t.Fatalf("MERGE created %d nodes", n)
	}
	if !rawUniqueConstraintExists(t, uri, "JudgeVerdict", "key") {
		t.Error("JudgeVerdict(key) unique constraint missing")
	}
	if !rawIndexExists(t, uri, "JudgeVerdict", "key") {
		t.Error("JudgeVerdict(key) index missing")
	}
	for _, bad := range []graph.JudgeVerdict{{Key: key, Status: "maybe"}, {Status: "confirmed"}} {
		if err := st.PutJudgeVerdict(ctx, bad); err == nil {
			t.Errorf("invalid cache entry accepted: %+v", bad)
		}
	}
}

func TestMigrateKeepsJudgeVerdict(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	key := fmt.Sprintf("quillon-cache-%d", time.Now().UnixNano())
	want := graph.JudgeVerdict{Key: key, Status: "confirmed", Reason: "supported", Model: "test-model", PromptVersion: "1", At: time.Now().UTC()}
	if err := st.PutJudgeVerdict(ctx, want); err != nil {
		t.Fatal(err)
	}
	rawRun(t, uri, fmt.Sprintf(`CREATE (:Entity {key: '%s'})`, key))
	t.Cleanup(func() { rawRun(t, uri, fmt.Sprintf(`MATCH (n) WHERE n.key = '%s' DETACH DELETE n`, key)) })
	rawRun(t, uri, `MERGE (m:GkgMeta {id: 'extraction_schema'}) SET m.value = 2`)
	result, err := st.MigrateExtractionSchema(ctx)
	if err != nil || !result.Migrated || result.EntitiesRemoved < 1 {
		t.Fatalf("migration = %+v err=%v", result, err)
	}
	got, ok, err := st.JudgeVerdict(ctx, key)
	if err != nil || !ok || got != want {
		t.Fatalf("cache lost after migration: %+v ok=%v err=%v", got, ok, err)
	}
}

// TestJudgeVerdictWithoutTime keeps a node that lacks a parsable time
// usable: the verdict is returned with a zero At instead of poisoning the key.
func TestJudgeVerdictWithoutTime(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	key := fmt.Sprintf("zorbo-untimed-%d", time.Now().UnixNano())
	rawRun(t, uri, fmt.Sprintf(`CREATE (:JudgeVerdict {key: '%s', status: 'unclear', reason: 'r', model: 'm', prompt_version: '1'})`, key))
	t.Cleanup(func() { rawRun(t, uri, fmt.Sprintf(`MATCH (v:JudgeVerdict {key: '%s'}) DELETE v`, key)) })
	got, ok, err := st.JudgeVerdict(t.Context(), key)
	if err != nil || !ok || got.Status != "unclear" || !got.At.IsZero() {
		t.Fatalf("verdict = %+v ok=%v err=%v, want unclear with zero time", got, ok, err)
	}
}

// TestDeleteDocumentRemovesItsVerdicts keeps deletion permanent: a verdict
// cached for a document's request is removed with that document, verdicts
// first written for other documents stay, and a verdict that arrives after
// its document was deleted (a judge call in flight) is never written.
func TestDeleteDocumentRemovesItsVerdicts(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	uri := os.Getenv(graphtest.URIEnv)
	tag := time.Now().UnixNano()
	doc := func(name string) string {
		res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: fmt.Sprintf("text/zorbo-verdict-%s-%d", name, tag), Hash: "h"})
		if err != nil {
			t.Fatal(err)
		}
		return res.ID
	}
	docA, docB := doc("a"), doc("b")
	mine := fmt.Sprintf("zorbo-mine-%d", tag)
	other := fmt.Sprintf("zorbo-other-%d", tag)
	late := fmt.Sprintf("zorbo-late-%d", tag)
	t.Cleanup(func() {
		_ = st.DeleteDocument(context.Background(), docB)
		rawRun(t, uri, fmt.Sprintf(`MATCH (v:JudgeVerdict) WHERE v.key IN ['%s', '%s', '%s'] DELETE v`, mine, other, late))
	})
	put := func(key, docID string) {
		t.Helper()
		if err := st.PutJudgeVerdict(ctx, graph.JudgeVerdict{Key: key, Status: "confirmed", Model: "m", PromptVersion: "1", DocID: docID, At: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	put(mine, docA)
	put(other, docB)
	// A later identical request from another document keeps the first owner.
	put(mine, docB)
	if got, _, err := st.JudgeVerdict(ctx, mine); err != nil || got.DocID != docA {
		t.Fatalf("owner = %q err=%v, want the first writer %q", got.DocID, err, docA)
	}
	if err := st.DeleteDocument(ctx, docA); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.JudgeVerdict(ctx, mine); err != nil || ok {
		t.Fatalf("verdict of the deleted document still cached: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.JudgeVerdict(ctx, other); err != nil || !ok {
		t.Fatalf("another document's verdict lost: ok=%v err=%v", ok, err)
	}
	// The judge call that was in flight during the delete returns now.
	put(late, docA)
	if _, ok, err := st.JudgeVerdict(ctx, late); err != nil || ok {
		t.Fatalf("verdict written for a deleted document: ok=%v err=%v", ok, err)
	}
	if !rawIndexExists(t, uri, "JudgeVerdict", "doc_id") {
		t.Error("JudgeVerdict(doc_id) index missing")
	}
}

// TestDeleteDocumentRemovesItsAudits keeps deletion permanent for audits
// too: a :RejectedFact row carries the judge's reason for a fact of the
// document, so it is deleted with the document. Other documents' audits stay.
func TestDeleteDocumentRemovesItsAudits(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	uri := os.Getenv(graphtest.URIEnv)
	tag := time.Now().UnixNano()
	var ids []string
	for _, name := range []string{"gone", "kept"} {
		res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: fmt.Sprintf("text/zorbo-audit-%s-%d", name, tag), Hash: "h"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.ID)
		if err := st.WriteFactAudit(ctx, graph.FactAudit{DocID: res.ID, ChunkID: res.ID + "-c0", HeadKey: "quillon", TailKey: "zorbo", Rel: "uses", JudgeVerdict: "refuted", Reason: "contradicted by the evidence"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = st.DeleteDocument(context.Background(), ids[1])
		rawRun(t, uri, fmt.Sprintf(`MATCH (r:RejectedFact) WHERE r.doc_id IN ['%s', '%s'] DELETE r`, ids[0], ids[1]))
	})
	if err := st.DeleteDocument(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	count := func(id string) int64 {
		return rawCount(t, uri, fmt.Sprintf(`MATCH (r:RejectedFact {doc_id: '%s'}) RETURN count(r) AS n`, id))
	}
	if n := count(ids[0]); n != 0 {
		t.Fatalf("deleted document keeps %d audits", n)
	}
	if n := count(ids[1]); n != 1 {
		t.Fatalf("other document's audits = %d, want 1", n)
	}
	if !rawIndexExists(t, uri, "RejectedFact", "doc_id") {
		t.Error("RejectedFact(doc_id) index missing")
	}
}
