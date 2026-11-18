package graphtest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// VisibilityFixture has two unjudged candidates, one unclear candidate, one
// rejection, two confirmed edges (one low-confidence), and one low-confidence
// unclear edge around Zorbo.
// Another candidate touches only its neighbours and must not count for Zorbo.
type VisibilityFixture struct {
	Store                                       *graph.Store
	ID, Key, OtherKey, ThirdKey, DocID, ChunkID string
}

func Visibility(t *testing.T) VisibilityFixture {
	t.Helper()
	return visibility(t, false)
}

// MixedVisibility adds unclear and unjudged typed edges at the start and
// second-hop edges that exercise verdict filtering during expansion.
func MixedVisibility(t *testing.T) VisibilityFixture {
	t.Helper()
	return visibility(t, true)
}

func visibility(t *testing.T, mixed bool) VisibilityFixture {
	t.Helper()
	st := Store(t)
	ctx := context.Background()
	tag := fmt.Sprintf("visibility%d", time.Now().UnixNano())
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: "text:" + tag, Hash: tag})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, doc.ID); err != nil {
			t.Error(err)
		}
	})
	text := "Zorbo and Quillon Ltd use Acme-Max with Plinko."
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: text}, {Seq: 1, Text: text}}); err != nil {
		t.Fatal(err)
	}
	ents := []graph.EntityUpsert{
		{Key: "zorbo-" + tag, Name: "Zorbo", Type: "person"},
		{Key: "quillon-" + tag, Name: "Quillon Ltd", Type: "organization"},
		{Key: "acme-" + tag, Name: "Acme-Max", Type: "product"},
		{Key: "plinko-" + tag, Name: "Plinko", Type: "product"},
	}
	facts := []graph.FactUpsert{
		{HeadKey: ents[0].Key, TailKey: ents[1].Key, Rel: "pending_out", Confidence: .8},
		{HeadKey: ents[1].Key, TailKey: ents[0].Key, Rel: "pending_in", Confidence: .8},
		{HeadKey: ents[0].Key, TailKey: ents[2].Key, Rel: "unclear", Confidence: .4, LowConfidence: true},
		{HeadKey: ents[2].Key, TailKey: ents[3].Key, Rel: "neighbor_only", Confidence: .8},
	}
	for i := range facts {
		facts[i].JudgeInput = []byte(fmt.Sprintf(`{"fixture":%d}`, i))
		facts[i].HeadMention = graph.Span{Start: 0, End: 5}
		facts[i].TailMention = graph.Span{Start: 10, End: 21}
	}
	cid := doc.ID + "-c0"
	if err := st.UpsertExtractionOpts(ctx, doc.ID, cid, "mock:1", ents, facts, graph.WriteOptions{FactsAsCandidates: true}); err != nil {
		t.Fatal(err)
	}
	unclear := facts[2]
	if ok, err := st.SetCandidateJudge(ctx, graph.CandidateID(unclear.HeadKey, unclear.Rel, unclear.TailKey, cid), fmt.Sprintf("%x", sha256.Sum256(unclear.JudgeInput)), "unclear"); err != nil || !ok {
		t.Fatalf("set unclear: %v, %v", ok, err)
	}
	if err := st.WriteFactAudit(ctx, graph.FactAudit{HeadKey: ents[0].Key, TailKey: ents[1].Key, Rel: "declined", DocID: doc.ID, ChunkID: cid, JudgeVerdict: "refuted", Reason: "synthetic refusal"}); err != nil {
		t.Fatal(err)
	}
	edges := []graph.FactUpsert{
		{HeadKey: ents[0].Key, TailKey: ents[1].Key, Rel: "works_at", Confidence: .9},
		{HeadKey: ents[0].Key, TailKey: ents[1].Key, Rel: "advises", Confidence: .4, LowConfidence: true},
		{HeadKey: ents[0].Key, TailKey: ents[2].Key, Rel: "evaluates", Confidence: .4, LowConfidence: true},
	}
	judges := map[string]string{"works_at": "confirmed", "advises": "confirmed", "evaluates": "unclear"}
	if mixed {
		edges = append(edges,
			graph.FactUpsert{HeadKey: ents[0].Key, TailKey: ents[2].Key, Rel: "consults", Confidence: .8},
			graph.FactUpsert{HeadKey: ents[0].Key, TailKey: ents[3].Key, Rel: "uses", Confidence: .8},
			graph.FactUpsert{HeadKey: ents[1].Key, TailKey: ents[3].Key, Rel: "supplies", Confidence: .8},
			graph.FactUpsert{HeadKey: ents[2].Key, TailKey: ents[3].Key, Rel: "ships", Confidence: .4, LowConfidence: true},
		)
		judges["consults"] = "unclear"
		judges["supplies"] = "unclear"
		judges["ships"] = "confirmed"
	}
	if err := st.UpsertExtraction(ctx, doc.ID, doc.ID+"-c1", "mock:1", ents, edges); err != nil {
		t.Fatal(err)
	}
	for _, f := range edges {
		if judge := judges[f.Rel]; judge != "" {
			if err := st.SetFactJudge(ctx, f.HeadKey, f.Rel, f.TailKey, doc.ID+"-c1", judge); err != nil {
				t.Fatal(err)
			}
		}
	}
	return VisibilityFixture{Store: st, ID: graph.EntityID(ents[0].Key), Key: ents[0].Key, OtherKey: ents[1].Key, ThirdKey: ents[2].Key, DocID: doc.ID, ChunkID: cid}
}
