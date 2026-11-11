package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/dedup"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

// textLoader is the T8 stub for ingestion.Decode (T11 wires the real one).
func textLoader(_ context.Context, in ingest.Input) (ingest.Loaded, error) {
	switch {
	case in.Text != "":
		return ingest.Loaded{Text: in.Text, Title: in.Title}, nil
	case len(in.Bytes) > 0:
		return ingest.Loaded{Text: string(in.Bytes), Title: in.Title}, nil
	}
	return ingest.Loaded{}, errors.New("no content to load")
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	cfg.Ingest.ChunkWords = 12
	cfg.Ingest.SpoolDir = t.TempDir()
	cfg.Ingest.OverlapSentences = 0
	return cfg
}

type pipelineEnv struct {
	st   *graph.Store
	cfg  config.Config
	ix   *dedup.Index
	mock *extract.Mock
	p    *ingest.Pipeline
}

func newPipelineEnv(t *testing.T) pipelineEnv {
	t.Helper()
	st := graphtest.Store(t)
	cfg := testConfig(t)
	ix := dedup.New()
	mock := &extract.Mock{}
	p := ingest.NewPipeline(cfg, st, mock, ix, textLoader)
	return pipelineEnv{st: st, cfg: cfg, ix: ix, mock: mock, p: p}
}

func uniqueText(t *testing.T) (source, text string) {
	t.Helper()
	tag := time.Now().UnixNano()
	return fmt.Sprintf("text/%s-%d", t.Name(), tag),
		fmt.Sprintf("Ada Lovelace wrote the note numbered %d today.", tag)
}

// duplicatePair returns a source and a text made of two identical sentences
// (five tokens each) tagged per run, plus a config whose chunk target puts
// each sentence in its own chunk.
func duplicatePair(t *testing.T) (string, string) {
	t.Helper()
	tag := time.Now().UnixNano()
	sentence := fmt.Sprintf("Alpha %d beta gamma delta.", tag)
	return fmt.Sprintf("text/%s-%d", t.Name(), tag), sentence + " " + sentence
}

func mustProcess(t *testing.T, env pipelineEnv, in ingest.Input) ingest.Result {
	t.Helper()
	res, err := env.p.Submit(context.Background(), in)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Noop {
		t.Fatalf("Submit flagged Noop unexpectedly")
	}
	if err := env.p.Process(context.Background(), res.DocumentID); err != nil {
		t.Fatalf("Process: %v", err)
	}
	return res
}

func TestPipelineEndToEndWithMock(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	source, text := uniqueText(t)

	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})

	doc, err := env.st.Document(ctx, res.DocumentID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusDone {
		t.Fatalf("status = %q, want done (error: %q)", doc.Status, doc.Error)
	}
	if doc.Hash == "" || doc.SourceKind != "text" || doc.Source != source {
		t.Errorf("document identity = %+v", doc)
	}

	chunks, err := env.st.Chunks(ctx, res.DocumentID)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks persisted")
	}
	for _, c := range chunks {
		if len(c.MinhashSig) != 64 {
			t.Errorf("chunk %s has no fingerprint (%d sig values)", c.ID, len(c.MinhashSig))
		}
	}
	all, err := env.st.AllFingerprints(ctx)
	if err != nil {
		t.Fatalf("AllFingerprints: %v", err)
	}
	found := 0
	for _, cfp := range all {
		if strings.HasPrefix(cfp.ChunkID, res.DocumentID) {
			found++
		}
	}
	if found != len(chunks) {
		t.Errorf("fingerprints persisted = %d, want %d", found, len(chunks))
	}

	// EntityView works on the mock's entities (first and last word).
	view, err := env.st.EntityView(ctx, graph.EntityKey("Ada"), false)
	if err != nil {
		t.Fatalf("EntityView(ada): %v", err)
	}
	if len(view.Facts) == 0 {
		t.Fatalf("entity %q has no facts", view.Key)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", res.DocumentID)); n == 0 {
		t.Error("no fact edges persisted")
	}
}

func TestCrossDocumentDuplicateSkipsExtraction(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	env.cfg.Ingest.ChunkWords = 5
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)

	tag := time.Now().UnixNano()
	sentence := fmt.Sprintf("Boilerplate %d text stays identical.", tag)
	text := sentence + " " + sentence

	first := mustProcess(t, env, ingest.Input{Kind: "text", Source: fmt.Sprintf("text/%s-a-%d", t.Name(), tag), Text: text})
	if calls := env.mock.Calls(); calls != 1 {
		t.Fatalf("calls after first doc = %d, want 1", calls)
	}
	// A different document with the same content: its chunks duplicate the
	// first document's live chunks and skip extraction.
	second := mustProcess(t, env, ingest.Input{Kind: "text", Source: fmt.Sprintf("text/%s-b-%d", t.Name(), tag), Text: text})
	if calls := env.mock.Calls(); calls != 1 {
		t.Errorf("calls after cross-document duplicate = %d, want 1 (skip)", calls)
	}
	chunks, err := env.st.Chunks(ctx, second.DocumentID)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if chunks[0].DuplicateOf == "" {
		t.Error("second document's chunk not marked duplicate of the first document's chunk")
	}
	firstChunks, err := env.st.Chunks(ctx, first.DocumentID)
	if err != nil {
		t.Fatalf("Chunks(first): %v", err)
	}
	if chunks[0].DuplicateOf != firstChunks[0].ID {
		t.Errorf("duplicate_of = %q, want %q", chunks[0].DuplicateOf, firstChunks[0].ID)
	}
}

// TestRedigestUnchangedChunksKeepFacts pins that a revised version keeps
// facts for chunks that are unchanged from its superseded predecessor: the
// predecessor is superseded (its chunks deleted), so its chunks must never
// serve as dedup targets.
func TestRedigestUnchangedChunksKeepFacts(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	env.cfg.Ingest.ChunkWords = 5
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
	tag := time.Now().UnixNano()
	source := fmt.Sprintf("text/%s-%d", t.Name(), tag)
	s1 := fmt.Sprintf("Alpha %d beta gamma delta.", tag)
	s2 := fmt.Sprintf("Epsilon %d zeta eta theta.", tag)
	s3 := fmt.Sprintf("Iota %d kappa lambda mu.", tag)
	s3v := fmt.Sprintf("Nu %d xi omicron pi.", tag)

	v1 := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: s1 + " " + s2 + " " + s3})
	v2 := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: s1 + " " + s2 + " " + s3v})
	if v2.DocumentID == v1.DocumentID {
		t.Fatal("revised content must be a new document")
	}

	chunks, err := env.st.Chunks(ctx, v2.DocumentID)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("v2 chunks = %d, want 3", len(chunks))
	}
	for _, c := range chunks {
		if c.DuplicateOf != "" {
			t.Errorf("v2 chunk %s marked duplicate_of %q; same-source predecessors must not be dedup targets", c.ID, c.DuplicateOf)
		}
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", v2.DocumentID)); n != 3 {
		t.Errorf("v2 fact edges = %d, want 3 (unchanged chunks re-extracted)", n)
	}
	doc, err := env.st.Document(ctx, v1.DocumentID)
	if err != nil {
		t.Fatalf("v1 Document: %v", err)
	}
	if doc.Status != graph.StatusSuperseded {
		t.Errorf("v1 status = %q, want superseded", doc.Status)
	}
}

// TestRedigestDuplicatePairOnForceKeepsFacts pins that reprocessing a
// document with an intra-document duplicate pair reproduces the original
// marking instead of creating a mutual duplicate cycle that wipes facts.
func TestRedigestDuplicatePairOnForceKeepsFacts(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	env.cfg.Ingest.ChunkWords = 5
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
	source, text := duplicatePair(t)

	first := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	if calls := env.mock.Calls(); calls != 1 {
		t.Fatalf("calls after first = %d, want 1", calls)
	}

	// Force resubmission re-chunks over the document's own persisted chunks.
	res, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text, Force: true})
	if err != nil {
		t.Fatalf("Force Submit: %v", err)
	}
	if err := env.p.Process(ctx, res.DocumentID); err != nil {
		t.Fatalf("Force Process: %v", err)
	}
	chunks, err := env.st.Chunks(ctx, first.DocumentID)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if chunks[0].DuplicateOf != "" {
		t.Errorf("chunk0 duplicate_of = %q, want empty", chunks[0].DuplicateOf)
	}
	if chunks[1].DuplicateOf != chunks[0].ID {
		t.Errorf("chunk1 duplicate_of = %q, want %q (no mutual cycle)", chunks[1].DuplicateOf, chunks[0].ID)
	}
	uri := os.Getenv(graphtest.URIEnv)
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", first.DocumentID)); n == 0 {
		t.Error("facts wiped by forced re-chunk")
	}
}

// TestRedigestOutOfOrderVersions pins submit-time version ordering: a newer
// version processed first supersedes every older live version, and the
// older version's later processing is benignly discarded.
func TestRedigestOutOfOrderVersions(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	source, text1 := uniqueText(t)

	v1 := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text1})
	v2, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text1 + " Second revision."})
	if err != nil {
		t.Fatalf("Submit v2: %v", err)
	}
	v3, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text1 + " Third revision."})
	if err != nil {
		t.Fatalf("Submit v3: %v", err)
	}

	// Process the newest first, then the older in-flight version.
	if err := env.p.Process(ctx, v3.DocumentID); err != nil {
		t.Fatalf("Process v3: %v", err)
	}
	if err := env.p.Process(ctx, v2.DocumentID); err != nil {
		t.Fatalf("Process v2 (must be benignly discarded): %v", err)
	}

	for id, want := range map[string]graph.Status{v1.DocumentID: graph.StatusSuperseded, v2.DocumentID: graph.StatusSuperseded, v3.DocumentID: graph.StatusDone} {
		doc, err := env.st.Document(ctx, id)
		if err != nil {
			t.Fatalf("Document(%s): %v", id, err)
		}
		if doc.Status != want {
			t.Errorf("document %s status = %q, want %q", id, doc.Status, want)
		}
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (d:Document {source: '%s'}) WHERE d.status <> 'superseded' RETURN count(d) AS n", source)); n != 1 {
		t.Errorf("live versions = %d, want exactly 1", n)
	}
}

// TestRedigestInOrderVersions pins the normal order: each version supersedes
// the one before it while newer in-flight versions stay untouched.
func TestRedigestInOrderVersions(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	source, text1 := uniqueText(t)

	v1 := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text1})
	v2, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text1 + " Second revision."})
	if err != nil {
		t.Fatalf("Submit v2: %v", err)
	}
	v3, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text1 + " Third revision."})
	if err != nil {
		t.Fatalf("Submit v3: %v", err)
	}
	if err := env.p.Process(ctx, v2.DocumentID); err != nil {
		t.Fatalf("Process v2: %v", err)
	}
	if err := env.p.Process(ctx, v3.DocumentID); err != nil {
		t.Fatalf("Process v3: %v", err)
	}

	for id, want := range map[string]graph.Status{v1.DocumentID: graph.StatusSuperseded, v2.DocumentID: graph.StatusSuperseded, v3.DocumentID: graph.StatusDone} {
		doc, err := env.st.Document(ctx, id)
		if err != nil {
			t.Fatalf("Document(%s): %v", id, err)
		}
		if doc.Status != want {
			t.Errorf("document %s status = %q, want %q", id, doc.Status, want)
		}
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (d:Document {source: '%s'}) WHERE d.status <> 'superseded' RETURN count(d) AS n", source)); n != 1 {
		t.Errorf("live versions = %d, want exactly 1", n)
	}
	doc, err := env.st.Document(ctx, v3.DocumentID)
	if err != nil {
		t.Fatalf("Document(v3): %v", err)
	}
	if doc.Supersedes != v2.DocumentID {
		t.Errorf("v3.supersedes = %q, want %q (last superseded older version)", doc.Supersedes, v2.DocumentID)
	}
}

func TestPipelineNoopOnSameHash(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	source, text := uniqueText(t)
	in := ingest.Input{Kind: "text", Source: source, Text: text}

	mustProcess(t, env, in)
	if calls := env.mock.Calls(); calls != 1 {
		t.Fatalf("provider calls after first ingest = %d, want 1", calls)
	}

	res, err := env.p.Submit(ctx, in)
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if !res.Noop {
		t.Error("second Submit: want Noop=true")
	}
	if calls := env.mock.Calls(); calls != 1 {
		t.Errorf("provider calls after noop resubmit = %d, want 1 (unchanged)", calls)
	}
}

func TestRedigestSupersedesPrevious(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	source, text1 := uniqueText(t)

	first := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text1})
	second := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text1 + " Actually revised."})
	if second.DocumentID == first.DocumentID {
		t.Fatal("revised content must create a new document id")
	}

	old, err := env.st.Document(ctx, first.DocumentID)
	if err != nil {
		t.Fatalf("old Document: %v", err)
	}
	if old.Status != graph.StatusSuperseded {
		t.Errorf("old status = %q, want superseded", old.Status)
	}
	newer, err := env.st.Document(ctx, second.DocumentID)
	if err != nil {
		t.Fatalf("new Document: %v", err)
	}
	if newer.Status != graph.StatusDone || newer.Supersedes != first.DocumentID {
		t.Errorf("new document = %+v, want done superseding %s", newer, first.DocumentID)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", first.DocumentID)); n != 0 {
		t.Errorf("old fact edges = %d, want 0", n)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", second.DocumentID)); n == 0 {
		t.Error("new fact edges = 0, want > 0")
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (c:Chunk {doc_id: '%s'}) RETURN count(c) AS n", first.DocumentID)); n != 0 {
		t.Errorf("old chunks = %d, want 0", n)
	}
}

func TestSupersedeSweepSkipsDeletedOldVersion(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	source, text := uniqueText(t)
	v1 := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	v2, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text + " Revised."})
	if err != nil {
		t.Fatalf("Submit(v2): %v", err)
	}
	var seen []string
	ingest.SetBeforeSupersede(env.p, func(olds []string) {
		seen = slices.Clone(olds)
		if err := env.st.DeleteDocument(ctx, v1.DocumentID); err != nil {
			t.Fatalf("DeleteDocument(v1): %v", err)
		}
	})
	if err := env.p.Process(ctx, v2.DocumentID); err != nil {
		t.Errorf("Process(v2): %v, want nil", err)
	}
	if !slices.Equal(seen, []string{v1.DocumentID}) {
		t.Errorf("hook olds = %v, want [%s]", seen, v1.DocumentID)
	}
	newer, err := env.st.Document(ctx, v2.DocumentID)
	if err != nil {
		t.Fatalf("Document(v2): %v", err)
	}
	if newer.Status != graph.StatusDone {
		t.Errorf("v2 status = %q, want done", newer.Status)
	}
	if _, err := env.st.Document(ctx, v1.DocumentID); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("Document(v1): %v, want ErrNotFound", err)
	}
}

func TestSupersedeSweepSkipsAlreadySuperseded(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	source, text := uniqueText(t)
	v1 := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	v2, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text + " Second revision."})
	if err != nil {
		t.Fatalf("Submit(v2): %v", err)
	}
	v3, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text + " Third revision."})
	if err != nil {
		t.Fatalf("Submit(v3): %v", err)
	}
	hookCalled := false
	ingest.SetBeforeSupersede(env.p, func(olds []string) {
		hookCalled = true
		if !slices.Equal(olds, []string{v1.DocumentID}) {
			t.Fatalf("hook olds = %v, want [%s]", olds, v1.DocumentID)
		}
		if err := env.st.Supersede(ctx, v1.DocumentID, v3.DocumentID); err != nil {
			t.Fatalf("Supersede(v1, v3): %v", err)
		}
	})
	if err := env.p.Process(ctx, v2.DocumentID); err != nil {
		t.Errorf("Process(v2): %v, want nil", err)
	}
	if !hookCalled {
		t.Error("before-supersede hook was not called")
	}
	newer, err := env.st.Document(ctx, v2.DocumentID)
	if err != nil {
		t.Fatalf("Document(v2): %v", err)
	}
	if newer.Status != graph.StatusDone {
		t.Errorf("v2 status = %q, want done", newer.Status)
	}
}

func TestSupersedeSweepNewVersionDeleted(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	source, text := uniqueText(t)
	v1 := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	v2, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text + " Revised."})
	if err != nil {
		t.Fatalf("Submit(v2): %v", err)
	}
	var seen []string
	ingest.SetBeforeSupersede(env.p, func(olds []string) {
		seen = slices.Clone(olds)
		if err := env.st.DeleteDocument(ctx, v2.DocumentID); err != nil {
			t.Fatalf("DeleteDocument(v2): %v", err)
		}
	})
	if err := env.p.Process(ctx, v2.DocumentID); err == nil {
		t.Error("Process(v2): want an error for the deleted new version")
	} else if !strings.Contains(err.Error(), "ingest: supersede:") {
		t.Errorf("Process(v2): %v, want ingest: supersede: error", err)
	}
	if !slices.Equal(seen, []string{v1.DocumentID}) {
		t.Errorf("hook olds = %v, want [%s]", seen, v1.DocumentID)
	}
	if _, err := env.st.Document(ctx, v2.DocumentID); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("Document(v2): %v, want ErrNotFound (nothing recreated)", err)
	}
	old, err := env.st.Document(ctx, v1.DocumentID)
	if err != nil {
		t.Fatalf("Document(v1): %v", err)
	}
	if old.Status != graph.StatusDone {
		t.Errorf("v1 status = %q, want done", old.Status)
	}
}

func TestSupersedeSweepPartialChain(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	source, text := uniqueText(t)
	v1 := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	v2, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text + " Revised."})
	if err != nil {
		t.Fatalf("Submit(v2): %v", err)
	}
	v3, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text + " Revised again."})
	if err != nil {
		t.Fatalf("Submit(v3): %v", err)
	}
	var seen []string
	ingest.SetBeforeSupersede(env.p, func(olds []string) {
		seen = slices.Clone(olds)
		if err := env.st.DeleteDocument(ctx, v1.DocumentID); err != nil {
			t.Fatalf("DeleteDocument(v1): %v", err)
		}
	})
	if err := env.p.Process(ctx, v3.DocumentID); err != nil {
		t.Errorf("Process(v3): %v, want nil", err)
	}
	if !slices.Equal(seen, []string{v1.DocumentID, v2.DocumentID}) {
		t.Errorf("hook olds = %v, want [%s %s]", seen, v1.DocumentID, v2.DocumentID)
	}
	newer, err := env.st.Document(ctx, v3.DocumentID)
	if err != nil {
		t.Fatalf("Document(v3): %v", err)
	}
	if newer.Status != graph.StatusDone || newer.Supersedes != v2.DocumentID {
		t.Errorf("v3 status = %q, supersedes = %q, want done superseding %s", newer.Status, newer.Supersedes, v2.DocumentID)
	}
	old, err := env.st.Document(ctx, v2.DocumentID)
	if err != nil {
		t.Fatalf("Document(v2): %v", err)
	}
	if old.Status != graph.StatusSuperseded {
		t.Errorf("v2 status = %q, want superseded", old.Status)
	}
	if _, err := env.st.Document(ctx, v1.DocumentID); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("Document(v1): %v, want ErrNotFound", err)
	}
}

func TestDuplicateChunkSkipsExtraction(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	env.cfg.Ingest.ChunkWords = 5
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
	source, text := duplicatePair(t)

	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	chunks, err := env.st.Chunks(ctx, res.DocumentID)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(chunks))
	}
	if chunks[1].DuplicateOf != chunks[0].ID {
		t.Errorf("second chunk duplicate_of = %q, want %q", chunks[1].DuplicateOf, chunks[0].ID)
	}
	if calls := env.mock.Calls(); calls != 1 {
		t.Errorf("provider calls = %d, want 1 (duplicate skipped)", calls)
	}
	if n := env.mock.LastChunkCount(); n != 1 {
		t.Errorf("provider saw %d chunks, want 1", n)
	}
}

func TestChunkDedupOffDisablesSkipping(t *testing.T) {
	env := newPipelineEnv(t)
	env.cfg.Ingest.ChunkDedup = "off"
	env.cfg.Ingest.ChunkWords = 5
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
	source, text := duplicatePair(t)

	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	chunks, err := env.st.Chunks(context.Background(), res.DocumentID)
	if err != nil {
		t.Fatalf("Chunks: %v", err)
	}
	for _, c := range chunks {
		if c.DuplicateOf != "" {
			t.Errorf("chunk %s marked duplicate_of %q with dedup off", c.ID, c.DuplicateOf)
		}
	}
	if n := env.mock.LastChunkCount(); n != 2 {
		t.Errorf("provider saw %d chunks, want 2 with dedup off", n)
	}
}

func TestStoreFloorDropsFacts(t *testing.T) {
	env := newPipelineEnv(t)
	uri := os.Getenv(graphtest.URIEnv)
	env.mock.Score = 0.05
	source, text := uniqueText(t)

	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", res.DocumentID)); n != 0 {
		t.Errorf("fact edges below store_floor = %d, want 0", n)
	}
	audits := rawCount(t, uri, fmt.Sprintf("MATCH (rf:RejectedFact) WHERE rf.doc_id = '%s' RETURN count(rf) AS n", res.DocumentID))
	if audits == 0 {
		t.Error("no audit rows for dropped facts")
	}
	verdict := rawRecords(t, uri, fmt.Sprintf("MATCH (rf:RejectedFact) WHERE rf.doc_id = '%s' RETURN rf.judge_verdict AS v LIMIT 1", res.DocumentID))[0]
	if got := testString(verdict, "v"); got != "store_floor" {
		t.Errorf("audit judge_verdict = %q, want store_floor", got)
	}
}

func TestAssertFloorMarksLowConfidence(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	env.mock.Score = 0.3
	source, text := uniqueText(t)

	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	r := rawRecords(t, uri, fmt.Sprintf(
		"MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN r.low_confidence AS l, r.confidence AS c LIMIT 1", res.DocumentID))
	if len(r) != 1 {
		t.Fatalf("fact edges = %d, want 1", len(r))
	}
	if got, _ := r[0].Get("l"); got != true {
		t.Errorf("low_confidence = %v, want true for score 0.3", got)
	}
	if got := testFloat(r[0], "c"); got != 0.3 {
		t.Errorf("confidence = %v, want 0.3", got)
	}

	view, err := env.st.EntityView(ctx, graph.EntityKey("Ada"), false)
	if err != nil {
		t.Fatalf("EntityView: %v", err)
	}
	for _, f := range view.Facts {
		if f.DocID == res.DocumentID {
			t.Error("low-confidence fact visible in default EntityView")
		}
	}
	view, err = env.st.EntityView(ctx, graph.EntityKey("Ada"), true)
	if err != nil {
		t.Fatalf("EntityView(include): %v", err)
	}
	if !slices.ContainsFunc(view.Facts, func(f graph.FactView) bool { return f.DocID == res.DocumentID && f.LowConfidence }) {
		t.Error("low-confidence fact not present with include flag")
	}
}

func TestMaxChunksCapsDocument(t *testing.T) {
	env := newPipelineEnv(t)
	env.cfg.Ingest.MaxChunks = 1
	env.cfg.Ingest.ChunkWords = 3
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	source := fmt.Sprintf("text/%s-%d", t.Name(), time.Now().UnixNano())
	text := "One two three. Four five six. Seven eight nine."

	res, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := env.p.Process(ctx, res.DocumentID); err == nil {
		t.Fatal("Process over the chunk cap: want error")
	}
	doc, err := env.st.Document(ctx, res.DocumentID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusFailed || !strings.Contains(doc.Error, "too many chunks") {
		t.Errorf("document = %q/%q, want failed with 'too many chunks'", doc.Status, doc.Error)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (c:Chunk {doc_id: '%s'}) RETURN count(c) AS n", res.DocumentID)); n != 0 {
		t.Errorf("chunks written over the cap = %d, want 0", n)
	}
	if calls := env.mock.Calls(); calls != 0 {
		t.Errorf("provider calls = %d, want 0", calls)
	}
}

func TestExtractorErrorFailsCleanly(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	env.mock.Err = errors.New("extractor exploded")
	source, text := uniqueText(t)

	res, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	err = env.p.Process(ctx, res.DocumentID)
	if err == nil {
		t.Fatal("Process with failing extractor: want error")
	}
	doc, err := env.st.Document(ctx, res.DocumentID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusFailed {
		t.Errorf("status = %q, want failed", doc.Status)
	}
	if !strings.Contains(doc.Error, "extract") {
		t.Errorf("error = %q, want stage context", doc.Error)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", res.DocumentID)); n != 0 {
		t.Errorf("half-written facts after extractor failure = %d, want 0", n)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Chunk {doc_id: '%s'})-[:MENTIONS]->(:Entity) RETURN count(*) AS n", res.DocumentID)); n != 0 {
		t.Errorf("half-written mentions after extractor failure = %d, want 0", n)
	}
}

func TestProcessIsIdempotentWhenDone(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	source, text := uniqueText(t)
	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
	if calls := env.mock.Calls(); calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if err := env.p.Process(ctx, res.DocumentID); err != nil {
		t.Fatalf("second Process on done document: %v", err)
	}
	if calls := env.mock.Calls(); calls != 1 {
		t.Errorf("calls after second Process = %d, want 1", calls)
	}
}

// reloadableLoader simulates a restart-capable loader: submissions carry
// text inline, reloads resolve from a per-source content map.
func reloadableLoader(contents map[string]string) ingest.TextLoader {
	return func(_ context.Context, in ingest.Input) (ingest.Loaded, error) {
		switch {
		case in.Text != "":
			return ingest.Loaded{Text: in.Text, Title: in.Title}, nil
		case len(in.Bytes) > 0:
			return ingest.Loaded{Text: string(in.Bytes), Title: in.Title}, nil
		}
		if text, ok := contents[in.Source]; ok {
			return ingest.Loaded{Text: text, Title: in.Title}, nil
		}
		return ingest.Loaded{}, errors.New("no content to load")
	}
}

// badProvider returns deliberately malformed extraction output: a mention
// without a label, candidates referencing it, and a candidate without a
// label alongside one valid pair.
type badProvider struct{}

func (badProvider) Name() string    { return "bad" }
func (badProvider) Version() string { return "bad:1" }
func (badProvider) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	out := make([]extract.ChunkResult, 0, len(req.Chunks))
	for _, c := range req.Chunks {
		out = append(out, extract.ChunkResult{
			ChunkID: c.ID,
			Mentions: []extract.Mention{
				{Label: "", Text: "Ada", Start: 0, End: 3, Score: 0.9}, // unlabeled
				{Label: "person", Text: "Ada", Start: 0, End: 3, Score: 0.9},
				{Label: "person", Text: "today.", Start: 37, End: 43, Score: 0.9},
			},
			Candidates: []extract.Candidate{
				{Label: "relates", Head: 0, Tail: 1, Score: 0.9}, // endpoint mention unlabeled
				{Label: "", Head: 1, Tail: 2, Score: 0.9},        // relation without a label
				{Label: "relates", Head: 1, Tail: 2, Score: 0.9}, // valid
			},
		})
	}
	return out, nil
}

// TestMalformedProviderSkipsBadEntries pins that malformed provider output
// (unlabeled mentions, unlabeled relations) is skipped entry-by-entry
// instead of failing the whole document.
func TestMalformedProviderSkipsBadEntries(t *testing.T) {
	st := graphtest.Store(t)
	cfg := testConfig(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	p := ingest.NewPipeline(cfg, st, badProvider{}, dedup.New(), textLoader)
	source, text := uniqueText(t)

	res, err := p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := p.Process(ctx, res.DocumentID); err != nil {
		t.Fatalf("Process: %v", err)
	}
	doc, err := st.Document(ctx, res.DocumentID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusDone {
		t.Fatalf("status = %q (%s), want done", doc.Status, doc.Error)
	}
	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", res.DocumentID)); n != 1 {
		t.Errorf("fact edges = %d, want 1 (only the valid candidate)", n)
	}
}

// TestRecoveryReloadHashMismatch pins that a restart whose reload returns
// different content fails the document instead of processing it under the
// stale hash.
func TestRecoveryReloadHashMismatch(t *testing.T) {
	st := graphtest.Store(t)
	cfg := testConfig(t)
	ctx := context.Background()
	source := fmt.Sprintf("text/%s-%d", t.Name(), time.Now().UnixNano())
	text1 := fmt.Sprintf("Ada Lovelace wrote the original note %d today.", time.Now().UnixNano())
	contents := map[string]string{source: text1}

	p1 := ingest.NewPipeline(cfg, st, &extract.Mock{}, dedup.New(), reloadableLoader(contents))
	res, err := p1.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text1})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Restart: the loader now returns changed content for the same source.
	contents[source] = text1 + " Completely different ending."
	// Missing-spool/legacy fallback still verifies the reloaded content.
	cfg.Ingest.SpoolDir = t.TempDir()
	p2 := ingest.NewPipeline(cfg, st, &extract.Mock{}, dedup.New(), reloadableLoader(contents))
	err = p2.Process(ctx, res.DocumentID)
	if err == nil {
		t.Fatal("Process after source change: want error")
	}
	doc, derr := st.Document(ctx, res.DocumentID)
	if derr != nil {
		t.Fatalf("Document: %v", derr)
	}
	if doc.Status != graph.StatusFailed || !strings.Contains(doc.Error, "source content changed") {
		t.Errorf("status/error = %q/%q, want failed with source-change message", doc.Status, doc.Error)
	}
	if doc.Hash == "" {
		t.Error("document hash lost")
	}
}

// TestRecoveryReloadHashMatch pins the positive restart path: re-decoded
// content matching the stored hash processes to done.
func TestRecoveryReloadHashMatch(t *testing.T) {
	st := graphtest.Store(t)
	cfg := testConfig(t)
	ctx := context.Background()
	source := fmt.Sprintf("text/%s-%d", t.Name(), time.Now().UnixNano())
	text := fmt.Sprintf("Ada Lovelace wrote the reloadable note %d today.", time.Now().UnixNano())
	contents := map[string]string{source: text}

	p1 := ingest.NewPipeline(cfg, st, &extract.Mock{}, dedup.New(), reloadableLoader(contents))
	res, err := p1.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Fresh pipeline without a cache or spool: only the loader can supply text.
	cfg.Ingest.SpoolDir = t.TempDir()
	mock := &extract.Mock{}
	load := reloadableLoader(contents)
	reloads := 0
	p2 := ingest.NewPipeline(cfg, st, mock, dedup.New(), func(ctx context.Context, in ingest.Input) (ingest.Loaded, error) {
		reloads++
		return load(ctx, in)
	})
	if err := p2.Process(ctx, res.DocumentID); err != nil {
		t.Fatalf("Process on reload path: %v", err)
	}
	if reloads != 1 {
		t.Errorf("loader calls = %d, want 1", reloads)
	}
	doc, err := st.Document(ctx, res.DocumentID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if doc.Status != graph.StatusDone {
		t.Errorf("status = %q (%s), want done", doc.Status, doc.Error)
	}
	if calls := mock.Calls(); calls != 1 {
		t.Errorf("provider calls = %d, want 1", calls)
	}
}

// TestLabelPresetSurvivesRestart pins that the requested preset name is
// persisted on the document and used when extraction happens after a
// restart (no in-memory submission).
func TestLabelPresetSurvivesRestart(t *testing.T) {
	st := graphtest.Store(t)
	cfg := testConfig(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	dir := t.TempDir()
	preset := "entities:\n  - name: widget\n    threshold: 0.4\n    scope: document\nrelations:\n  - name: powers\n    threshold: 0.25\n"
	if err := os.WriteFile(filepath.Join(dir, "custom.yaml"), []byte(preset), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Labels.PresetsDir = dir
	source := fmt.Sprintf("text/%s-%d", t.Name(), time.Now().UnixNano())
	name := fmt.Sprintf("PresetWidget%d", time.Now().UnixNano())
	text := fmt.Sprintf("%s wrote the preset note today.", name)
	contents := map[string]string{source: text}

	p1 := ingest.NewPipeline(cfg, st, &extract.Mock{}, dedup.New(), reloadableLoader(contents))
	res, err := p1.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text, Labels: "custom"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Restart before processing.
	p2 := ingest.NewPipeline(cfg, st, &extract.Mock{}, dedup.New(), reloadableLoader(contents))
	if err := p2.Process(ctx, res.DocumentID); err != nil {
		t.Fatalf("Process on restart: %v", err)
	}

	if n := rawCount(t, uri, fmt.Sprintf("MATCH (:Entity)-[r:powers]->(:Entity) WHERE r.doc_id = '%s' RETURN count(r) AS n", res.DocumentID)); n != 1 {
		t.Errorf("fact edges with preset relation = %d, want 1 (preset must survive restart)", n)
	}
	view, err := st.EntityView(ctx, graph.ScopedEntityKey(res.DocumentID, name), false)
	if err != nil {
		t.Fatalf("EntityView(%s): %v", name, err)
	}
	if view.Type != "widget" {
		t.Errorf("entity type = %q, want widget (preset label)", view.Type)
	}
}

// TestLabelPresetValidation pins eager validation of preset names and
// labels: bad names fail at Submit, and preset labels without alphanumeric
// content are rejected before any processing.
func TestLabelPresetValidation(t *testing.T) {
	env := newPipelineEnv(t)
	ctx := context.Background()
	source, text := uniqueText(t)

	if _, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text, Labels: "../evil"}); err == nil {
		t.Error("Submit with escaping preset name: want error")
	}
	if _, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text, Labels: "does-not-exist"}); err == nil {
		t.Error("Submit with missing preset: want error")
	}

	dir := t.TempDir()
	bad := "entities:\n  - name: person\n    threshold: 0.4\nrelations:\n  - name: \"!!!\"\n    threshold: 0.25\n"
	if err := os.WriteFile(filepath.Join(dir, "badrel.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	env.cfg.Labels.PresetsDir = dir
	env.p = ingest.NewPipeline(env.cfg, env.st, env.mock, env.ix, textLoader)
	if _, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: source, Text: text, Labels: "badrel"}); err == nil {
		t.Error("Submit with relation label without alphanumeric content: want error")
	}
}

func rawCount(t *testing.T, uri, query string) int64 {
	t.Helper()
	return testInt64(rawRecords(t, uri, query)[0], "n")
}

func testString(r *neo4j.Record, key string) string {
	v, _ := r.Get(key)
	s, _ := v.(string)
	return s
}

func testInt64(r *neo4j.Record, key string) int64 {
	v, _ := r.Get(key)
	n, _ := v.(int64)
	return n
}

func testFloat(r *neo4j.Record, key string) float64 {
	v, _ := r.Get(key)
	switch f := v.(type) {
	case float64:
		return f
	case int64:
		return float64(f)
	}
	return 0
}

func rawRecords(t *testing.T, uri, query string) []*neo4j.Record {
	t.Helper()
	drv, err := neo4j.NewDriver(uri, neo4j.NoAuth())
	if err != nil {
		t.Fatalf("raw driver: %v", err)
	}
	defer func() { _ = drv.Close(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer func() { _ = sess.Close(ctx) }()
	res, err := sess.Run(ctx, query, nil)
	if err != nil {
		t.Fatalf("raw query %q: %v", query, err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		t.Fatalf("raw collect %q: %v", query, err)
	}
	return recs
}
