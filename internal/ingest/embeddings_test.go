package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

// failingEmbedder always fails: embeddings are additive, never gating.
type failingEmbedder struct{}

func (failingEmbedder) EmbedDocuments(context.Context, []string) ([][]float64, error) {
	return nil, errors.New("embedder down")
}

// fixedEmbedder returns the same vector for every text.
type fixedEmbedder struct{ vec []float64 }

func (f fixedEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i := range texts {
		out[i] = f.vec
	}
	return out, nil
}

// recordingEmbedder records each batch's names and answers with a per-text
// vector (graphtest.Vector).
type recordingEmbedder struct {
	mu      sync.Mutex
	batches [][]string
}

func (r *recordingEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float64, error) {
	r.mu.Lock()
	r.batches = append(r.batches, append([]string(nil), texts...))
	r.mu.Unlock()
	out := make([][]float64, len(texts))
	for i, text := range texts {
		out[i] = graphtest.Vector(text, 768)
	}
	return out, nil
}

func (r *recordingEmbedder) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.batches))
	copy(out, r.batches)
	return out
}

// TestEmbeddingBatchesAndDedupesNames pins the hook contract: distinct
// names only, at most 64 per embedder call, and every embedded entity
// actually stored.
func TestEmbeddingBatchesAndDedupesNames(t *testing.T) {
	env := newPipelineEnv(t)
	rec := &recordingEmbedder{}
	env.p.SetEmbedder(rec)

	tag := time.Now().UnixNano()
	var b strings.Builder
	// Twelve-word sentences (one per chunk) with a unique first and last
	// word, so the document yields well over 64 distinct entity names.
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&b, "auniq%d-%d", tag, i)
		for j := 0; j < 10; j++ {
			fmt.Fprintf(&b, " p%d-%d", i, j)
		}
		fmt.Fprintf(&b, " zuniq%d-%d. ", tag, i)
	}
	source := fmt.Sprintf("text/%s-%d", t.Name(), tag)
	mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: b.String()})

	batches := rec.snapshot()
	if len(batches) < 2 {
		t.Fatalf("embedder calls = %d, want at least 2 (the text yields more than 64 names)", len(batches))
	}
	seen := map[string]bool{}
	for _, batch := range batches {
		if len(batch) > 64 {
			t.Errorf("batch size %d exceeds 64", len(batch))
		}
		for _, name := range batch {
			if seen[name] {
				t.Errorf("name %q embedded twice", name)
			}
			seen[name] = true
		}
	}
	if len(seen) <= 64 {
		t.Fatalf("distinct names = %d, want > 64 (test premise)", len(seen))
	}

	// Every distinct name's entity carries an embedding (checked directly:
	// top-K membership is not a stable assertion on an accumulating shared
	// index).
	ctx := context.Background()
	ids := make([]string, 0, len(seen))
	for name := range seen {
		ids = append(ids, "'"+graph.EntityID(graph.EntityKey("person", name))+"'")
	}
	qr, err := env.st.RunReadQuery(ctx,
		fmt.Sprintf("MATCH (e:Entity) WHERE e.id IN [%s] RETURN count(e) AS total, count(e.embedding) AS embedded", strings.Join(ids, ", ")), nil, 0)
	if err != nil {
		t.Fatalf("embedding read: %v", err)
	}
	if len(qr.Rows) != 1 || qr.Rows[0]["total"] != int64(len(seen)) || qr.Rows[0]["embedded"] != int64(len(seen)) {
		t.Errorf("embedding counts = %v, want all %d entities embedded", qr.Rows, len(seen))
	}
}

// TestEmbeddingFailureDoesNotFailIngest pins the additive contract: an
// embedder outage logs and skips, the document still reaches done and its
// entities exist without embeddings.
func TestEmbeddingFailureDoesNotFailIngest(t *testing.T) {
	env := newPipelineEnv(t)
	env.p.SetEmbedder(failingEmbedder{})
	tag := time.Now().UnixNano()
	first := fmt.Sprintf("Failprobe%d", tag)
	source := fmt.Sprintf("text/%s-%d", t.Name(), tag)
	text := first + " wrote the note today."
	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})

	ctx := context.Background()
	doc, err := env.st.Document(ctx, res.DocumentID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusDone {
		t.Errorf("status = %s, want done despite the embedder failure", doc.Status)
	}
	key := graph.EntityKey("person", first)
	view, err := env.st.EntityView(ctx, key, false)
	if err != nil {
		t.Fatalf("EntityView: %v", err)
	}
	if view.Name != first {
		t.Errorf("entity name = %q, want %q", view.Name, first)
	}
	qr, err := env.st.RunReadQuery(ctx,
		fmt.Sprintf("MATCH (e:Entity {id: '%s'}) RETURN e.embedding AS emb", graph.EntityID(key)), nil, 0)
	if err != nil {
		t.Fatalf("embedding read: %v", err)
	}
	if len(qr.Rows) != 1 || qr.Rows[0]["emb"] != nil {
		t.Errorf("embedding rows = %v, want nil after an embedder failure", qr.Rows)
	}
}

// TestEmbeddingSetsEntityVectors pins the happy path end to end: after
// merge the pipeline ensures the vector index and stores each distinct
// entity name's embedding, so a vector search reaches the entity.
func TestEmbeddingSetsEntityVectors(t *testing.T) {
	env := newPipelineEnv(t)
	const dim = 768

	tag := time.Now().UnixNano()
	first := fmt.Sprintf("Embedprobe%d", tag)
	vec := graphtest.Vector(first, dim)
	env.p.SetEmbedder(fixedEmbedder{vec})
	source := fmt.Sprintf("text/%s-%d", t.Name(), tag)
	text := first + " wrote the note today."
	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})

	ctx := context.Background()
	doc, err := env.st.Document(ctx, res.DocumentID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusDone {
		t.Fatalf("status = %s, want done", doc.Status)
	}
	key := graph.EntityKey("person", first)
	ids, err := env.st.EntityVectorSearch(ctx, vec, 10)
	if err != nil {
		t.Fatalf("EntityVectorSearch: %v", err)
	}
	if !slices.Contains(ids, graph.EntityID(key)) {
		t.Errorf("vector search = %v, want the ingested entity %s", ids, graph.EntityID(key))
	}
}
