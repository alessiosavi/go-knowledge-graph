package graph_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestRepairChunkMentionSpans(t *testing.T) {
	st := graphtest.Store(t)
	ctx := t.Context()
	uri := os.Getenv(graphtest.URIEnv)
	const chunkText = "## Part 3 — Section\n## Vercel: Architecture\nVercel runs the site."
	doc, chunk := entitiesTestDoc(t, st, chunkText)
	t.Cleanup(func() {
		if err := st.DeleteDocument(context.Background(), doc); err != nil {
			t.Error(err)
		}
	})
	part, vercel := "part-"+doc, "vercel-"+doc
	labelStart := strings.Index(chunkText, "Vercel")
	proseStart := strings.LastIndex(chunkText, "Vercel")
	if err := st.UpsertExtraction(ctx, doc, chunk, "mock:1", []graph.EntityUpsert{
		{Key: part, Name: "Part 3", Type: "topic", Mentions: []graph.MentionSpan{{Start: 3, End: 9, Type: "topic"}}},
		{Key: vercel, Name: "Vercel", Type: "aaa", Mentions: []graph.MentionSpan{
			{Start: labelStart, End: labelStart + 6, Type: "aaa"},
			{Start: proseStart, End: proseStart + 6, Type: "organization"},
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	rawRun(t, uri, fmt.Sprintf(`
MATCH (a:Entity {key:'%[1]s'}), (b:Entity {key:'%[2]s'})
CREATE
 (a)-[:about {chunk_id:'%[3]s', head_key:a.key, head_start:3, head_end:9, judge:'confirmed'}]->(b),
 (b)-[:label_occurrence {chunk_id:'%[3]s', tail_key:b.key, tail_start:%[4]d, tail_end:%[4]d+6}]->(b),
 (b)-[:prose_occurrence {chunk_id:'%[3]s', head_key:b.key, head_start:%[5]d, head_end:%[5]d+6, judge:'confirmed'}]->(b),
 (b)-[:other_chunk {chunk_id:'%[3]s-other', head_key:b.key, head_start:%[4]d, head_end:%[4]d+6}]->(b),
 (:FactCandidate {chunk_id:'%[3]s', doc_id:'%[6]s', tail_key:a.key, tail_start:3, tail_end:9}),
 (:FactCandidate {chunk_id:'%[3]s', doc_id:'%[6]s', head_key:b.key, head_start:%[5]d, head_end:%[5]d+6}),
 (:RejectedFact {chunk_id:'%[3]s', doc_id:'%[6]s', head_key:a.key, tail_key:b.key}),
 (:RejectedFact {chunk_id:'%[3]s', doc_id:'%[6]s', head_key:b.key, tail_key:b.key}),
 (:RejectedFact {chunk_id:'%[3]s', doc_id:'%[6]s', head_key:b.key, tail_key:'pre-existing missing endpoint'})`,
		part, vercel, chunk, labelStart, proseStart, doc))
	t.Cleanup(func() {
		rawRun(t, uri, fmt.Sprintf("MATCH (b:Entity {key:'%s'})-[r:other_chunk]->(b) DELETE r", vercel))
	})
	if err := st.SetStatus(ctx, doc, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	vec := graphtest.Vector(part, testEmbeddingDim)
	id := graph.EntityID(part)
	// Isolate the probe from unrelated entities and stale hits in entity_vec.
	index := "RepairLabelsProbe_" + id
	rawRun(t, uri, fmt.Sprintf("MATCH (e:Entity {key:'%s'}) SET e:%s", part, index))
	rawRun(t, uri, fmt.Sprintf(`CREATE VECTOR INDEX %s ON :%s(embedding) WITH CONFIG {"dimension": %d, "capacity": 16, "metric": "cos"}`, index, index, testEmbeddingDim))
	t.Cleanup(func() { rawRun(t, uri, "DROP VECTOR INDEX "+index) })
	if err := st.SetEntityEmbedding(ctx, id, vec); err != nil {
		t.Fatal(err)
	}
	waitForEmbedding := func(want bool) {
		t.Helper()
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for {
			result, err := st.RunReadQuery(pollCtx, `
CALL vector_search.search($index, 1, $vec) YIELD node
RETURN node.id AS id`, map[string]any{"index": index, "vec": vec}, 0)
			if err == nil && (len(result.Rows) == 1 && result.Rows[0]["id"] == id) == want {
				return
			}
			if pollCtx.Err() != nil {
				t.Fatalf("vector search for %s = %v, %v, want indexed=%t", id, result.Rows, err, want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	partSpan := graph.ChunkMentionSpan{Key: part, Start: 3, End: 9}
	vercelSpan := graph.ChunkMentionSpan{Key: vercel, Start: labelStart, End: labelStart + 6}
	spans := []graph.ChunkMentionSpan{partSpan, vercelSpan, partSpan, {Key: vercel, Start: 0, End: 6}}
	want := graph.ChunkSpanChanges{
		Mentions:       map[graph.ChunkMentionSpan]int{partSpan: 1, vercelSpan: 1},
		FactsConfirmed: 1, FactsOther: 1, Candidates: 1, Rejected: 1,
	}
	got, err := st.PreviewChunkMentionSpans(ctx, chunk, chunkText, spans)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("preview = %+v, %v, want %+v", got, err, want)
	}
	waitForEmbedding(true)

	got, err = st.DeleteChunkMentionSpans(ctx, chunk, "changed source text", spans)
	if err != nil || !reflect.DeepEqual(got, graph.ChunkSpanChanges{}) {
		t.Fatalf("stale chunk results = %+v, %v", got, err)
	}
	if err := st.SetStatus(ctx, doc, graph.StatusChunked, ""); err != nil {
		t.Fatal(err)
	}
	got, err = st.DeleteChunkMentionSpans(ctx, chunk, chunkText, spans)
	if err != nil || !reflect.DeepEqual(got, graph.ChunkSpanChanges{}) {
		t.Fatalf("in-progress document results = %+v, %v, want no changes", got, err)
	}
	if err := st.SetStatus(ctx, doc, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	want.EntitiesRemoved = 1
	got, err = st.DeleteChunkMentionSpans(ctx, chunk, chunkText, spans)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("delete = %+v, %v, want %+v", got, err, want)
	}
	waitForEmbedding(false)
	if n := rawCount(t, uri, fmt.Sprintf(`MATCH (e:Entity {key:'%s'}) RETURN count(e) AS n`, part)); n != 0 {
		t.Errorf("label-only entity still exists")
	}
	for query, count := range map[string]int64{
		fmt.Sprintf(`MATCH (c:Chunk {id:'%s'})-[m:MENTIONS]->(e:Entity {key:'%s'})
WHERE m.start = %d AND e.mention_count = 1 AND e.type = 'organization' RETURN count(m) AS n`, chunk, vercel, proseStart): 1,
		fmt.Sprintf(`MATCH (b:Entity {key:'%s'})-[r]->(b) RETURN count(r) AS n`, vercel):                              2,
		fmt.Sprintf(`MATCH (c:FactCandidate {chunk_id:'%s', head_start:%d}) RETURN count(c) AS n`, chunk, proseStart): 1,
		fmt.Sprintf(`MATCH (r:RejectedFact {chunk_id:'%s', head_key:'%s'}) RETURN count(r) AS n`, chunk, vercel):      2,
	} {
		if got := rawCount(t, uri, query); got != count {
			t.Errorf("%s returned %d, want %d", query, got, count)
		}
	}

	got, err = st.DeleteChunkMentionSpans(ctx, chunk, chunkText, spans)
	if err != nil || !reflect.DeepEqual(got, graph.ChunkSpanChanges{}) {
		t.Fatalf("second delete = %+v, %v", got, err)
	}
}
