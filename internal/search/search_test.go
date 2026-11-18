package search_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

func uniqueSearchSource(t testing.TB) string {
	t.Helper()
	return fmt.Sprintf("file:%s-%d", strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()), time.Now().UnixNano())
}

// uniqueToken is a single alphanumeric search token unique per run, so
// text-search assertions never depend on the accumulated shared corpus.
func uniqueToken(t testing.TB) string {
	t.Helper()
	return fmt.Sprintf("zetaquark%d", time.Now().UnixNano())
}

func upsertDoc(t testing.TB, st *graph.Store, source string) string {
	t.Helper()
	res, err := st.UpsertDocument(context.Background(), graph.DocumentInput{Kind: "file", Source: source, Hash: "hash-a"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	return res.ID
}

// searchFixture seeds one entity and one chunk sharing a unique per-run
// token.
type searchFixture struct {
	st        *graph.Store
	token     string
	entityID  string
	entityKey string
	docID     string
	chunkID   string
}

func newSearchFixture(t testing.TB) searchFixture {
	t.Helper()
	st := graphtest.Store(t)
	ctx := context.Background()
	docID := upsertDoc(t, st, uniqueSearchSource(t))
	token := uniqueToken(t)
	// Padded beyond the 120-byte window so the snippet assertions pin real
	// clipping, not the whole-text fallback.
	text := strings.Repeat("Background filler sentence. ", 4) + "The " + token + " guidance explains the procedure step by step."
	if err := st.PutChunks(ctx, docID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	key := graph.EntityKey(token)
	if err := st.UpsertExtraction(ctx, docID, docID+"-c0", "mock:1",
		[]graph.EntityUpsert{{Key: key, Name: token, Type: "technology"}},
		nil); err != nil {
		t.Fatalf("UpsertExtraction: %v", err)
	}
	return searchFixture{st: st, token: token, entityID: graph.EntityID(key), entityKey: key, docID: docID, chunkID: docID + "-c0"}
}

func TestSearchEntitiesByText(t *testing.T) {
	fx := newSearchFixture(t)
	hits, err := search.Query(context.Background(), fx.st, nil, fx.token, "entity", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Kind == "entity" && h.ID == fx.entityID {
			found = true
		}
	}
	if !found {
		t.Errorf("hits = %+v, want the seeded entity %s", hits, fx.entityID)
	}
}

func TestSnippetTurkishDottedCapital(t *testing.T) {
	// EqualFold's SimpleFold keeps U+0130 as itself, but ToLower maps it to
	// "i" — the token was built with ToLower, so the matcher must accept
	// that mapping too.
	text := strings.Repeat("x", 150) + "İstanbul"
	snippet := search.Snippet(text, "İstanbul")
	if !strings.Contains(snippet, "İstanbul") {
		t.Errorf("snippet = %q, want the İstanbul match inside the window", snippet)
	}
}

// TestSnippetLocatesMatchAfterCaseLengthChangingRunes pins rune-wise match
// location: ToLower is not byte-length preserving (U+212A → k), so an
// offset computed on a lowercased copy drifts and the window misses the
// token.
func TestSnippetLocatesMatchAfterCaseLengthChangingRunes(t *testing.T) {
	text := strings.Repeat("\u212A", 50) + " needle " + strings.Repeat("x", 200)
	snippet := search.Snippet(text, "needle")
	if !strings.Contains(snippet, "needle") {
		t.Errorf("snippet = %q, want the matched token inside the window", snippet)
	}
	if len(snippet) > 120 {
		t.Errorf("snippet is %d bytes, want <= 120", len(snippet))
	}
}

func TestSearchChunksReturnsSnippet(t *testing.T) {
	fx := newSearchFixture(t)
	hits, err := search.Query(context.Background(), fx.st, nil, fx.token, "chunk", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Kind == "chunk" && h.ID == fx.chunkID {
			found = true
			if !strings.Contains(h.Snippet, "guidance") {
				t.Errorf("snippet = %q, want a window around the match", h.Snippet)
			}
		}
	}
	if !found {
		t.Errorf("hits = %+v, want the seeded chunk %s", hits, fx.chunkID)
	}
}

func TestAutoReturnsBothLabeled(t *testing.T) {
	fx := newSearchFixture(t)
	hits, err := search.Query(context.Background(), fx.st, nil, fx.token, "auto", 20)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	kinds := map[string]int{}
	for _, h := range hits {
		kinds[h.Kind]++
	}
	if kinds["entity"] == 0 || kinds["chunk"] == 0 {
		t.Errorf("auto hits = %+v, want both kinds labeled", hits)
	}
}

// TestSearchAliasesAreNotIndexed pins name-only entity search by design:
// aliases are excluded from entity_text_v2, so alias-only lookups miss.
func TestSearchAliasesAreNotIndexed(t *testing.T) {
	fx := newSearchFixture(t)
	alias := uniqueToken(t)
	if err := fx.st.SetEntityAliases(context.Background(), fx.entityKey, []string{alias}); err != nil {
		t.Fatalf("SetEntityAliases: %v", err)
	}
	hits, err := search.Query(context.Background(), fx.st, nil, alias, "entity", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	for _, h := range hits {
		if h.ID == fx.entityID {
			t.Fatalf("alias token matched although aliases are excluded from entity_text_v2: %+v", h)
		}
	}
	// The name leg still works.
	hits, err = search.Query(context.Background(), fx.st, nil, fx.token, "entity", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.ID == fx.entityID {
			found = true
		}
	}
	if !found {
		t.Errorf("name search lost after alias change: %+v", hits)
	}
}

// emptyEmbedder returns no vectors: the vector leg must not swallow text
// recall.
type emptyEmbedder struct{}

func (emptyEmbedder) EmbedQuery(context.Context, string) ([]float64, error) {
	return nil, nil
}

func TestFusionKeepsTextOnlyHitWhenEmbeddingMisses(t *testing.T) {
	fx := newSearchFixture(t)
	hits, err := search.Query(context.Background(), fx.st, emptyEmbedder{}, fx.token, "entity", 10)
	if err != nil {
		t.Fatalf("Query with empty embedder: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.ID == fx.entityID {
			found = true
		}
	}
	if !found {
		t.Errorf("text hit dropped when the embedding leg produced nothing: %+v", hits)
	}
}

func TestSearchUnknownKindErrors(t *testing.T) {
	fx := newSearchFixture(t)
	if _, err := search.Query(context.Background(), fx.st, nil, "x", "wat", 5); err == nil {
		t.Fatal("unknown kind: want error")
	}
}

func TestRRFHelper(t *testing.T) {
	if search.RRF(0) <= search.RRF(5) {
		t.Error("RRF must decrease with rank")
	}
	fused := search.FuseRanks([][]string{{"a", "b"}, {"b"}})
	if fused["b"] <= fused["a"] {
		t.Errorf("fusion = %v, want b (top of leg 2 + second of leg 1) ahead of a", fused)
	}
}

// vecEmbedder returns a fixed vector for every query (query-leg stub).
type vecEmbedder struct{ vec []float64 }

func (v vecEmbedder) EmbedQuery(context.Context, string) ([]float64, error) {
	return v.vec, nil
}

type vectorErrorStore struct {
	search.Store
	err error
}

func (vectorErrorStore) EntityTextSearchIn(context.Context, string, string, int) ([]graph.EntitySearchRow, error) {
	return []graph.EntitySearchRow{{ID: "text-hit", Name: "test entity"}}, nil
}

func (s vectorErrorStore) EntityVectorSearch(context.Context, []float64, int) ([]string, error) {
	return nil, s.err
}

type queryEmbedFunc func(context.Context, string) ([]float64, error)

func (f queryEmbedFunc) EmbedQuery(ctx context.Context, text string) ([]float64, error) {
	return f(ctx, text)
}

func TestEntitySearchPropagatesRequestDeadlineDuringEmbedding(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	st := vectorErrorStore{}
	emb := queryEmbedFunc(func(context.Context, string) ([]float64, error) {
		<-ctx.Done()
		return nil, errors.New("embedding request failed")
	})
	hits, err := search.Query(ctx, st, emb, "entity", "entity", 10)
	if hits != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("search returned %v, %v, want only a request deadline error", hits, err)
	}
}

func TestEntitySearchKeepsTextHitWhenEmbeddingExceedsHalfTheRemainingBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	emb := queryEmbedFunc(func(embedCtx context.Context, _ string) ([]float64, error) {
		now := time.Now()
		deadline, ok := embedCtx.Deadline()
		if !ok || deadline.After(now.Add(parentDeadline.Sub(now)/2)) {
			t.Fatalf("embedding deadline = %v (set: %v), want at most half the remaining request budget", deadline, ok)
		}
		<-embedCtx.Done()
		// Even a vector returned after the embedding deadline must be dropped.
		return []float64{1}, nil
	})
	st := vectorErrorStore{err: graph.ErrQueryTimeout}
	hits, err := search.Query(ctx, st, emb, "entity", "entity", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != "text-hit" {
		t.Errorf("search returned %v, %v, want the text hit", hits, err)
	}
}

func TestEntitySearchDoesNotGiveUntimedEmbeddingADeadline(t *testing.T) {
	emb := queryEmbedFunc(func(ctx context.Context, _ string) ([]float64, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("embedding received a deadline for an untimed search")
		}
		return nil, nil
	})
	if _, err := search.Query(t.Context(), vectorErrorStore{}, emb, "entity", "entity", 10); err != nil {
		t.Fatal(err)
	}
}

func TestEntitySearchKeepsTextHitWhenEmbeddingProviderTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	emb := queryEmbedFunc(func(context.Context, string) ([]float64, error) {
		return nil, fmt.Errorf("provider timeout: %w", context.DeadlineExceeded)
	})
	hits, err := search.Query(ctx, vectorErrorStore{}, emb, "entity", "entity", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != "text-hit" {
		t.Errorf("search returned %v, %v, want the text hit", hits, err)
	}
}

func TestEntitySearchPropagatesVectorQueryTimeout(t *testing.T) {
	emb := vecEmbedder{vec: []float64{1}}
	st := vectorErrorStore{err: fmt.Errorf("vector search: %w", graph.ErrQueryTimeout)}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, err := search.Query(ctx, st, emb, "entity", "entity", 10); !errors.Is(err, graph.ErrQueryTimeout) {
		t.Errorf("search returned %v, want a query timeout", err)
	}

	hits, err := search.Query(t.Context(), st, emb, "entity", "entity", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != "text-hit" {
		t.Errorf("vector timeout without a search deadline returned hits %v and error %v, want the text hit", hits, err)
	}

	st.err = errors.New("vector index unavailable")
	hits, err = search.Query(ctx, st, emb, "entity", "entity", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != "text-hit" {
		t.Errorf("ordinary vector failure returned hits %v and error %v, want the text hit", hits, err)
	}
}

// TestSearchUsesQueryEmbedderWhenEnabled pins the vector leg: with
// embeddings enabled and no text match, an entity surfaces purely through
// the vector index; the query vector equals the stored one, so the distance
// is zero.
func TestSearchUsesQueryEmbedderWhenEnabled(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	const dim = 768
	if err := st.EnsureVectorIndex(ctx, dim); err != nil {
		t.Fatalf("EnsureVectorIndex: %v", err)
	}
	fx := newSearchFixture(t)
	vec := graphtest.Vector(fx.token, dim)
	if err := st.SetEntityEmbedding(ctx, fx.entityID, vec); err != nil {
		t.Fatalf("SetEntityEmbedding: %v", err)
	}

	hits, err := search.Query(ctx, st, vecEmbedder{vec}, uniqueToken(t), "entity", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.ID == fx.entityID {
			found = true
		}
	}
	if !found {
		t.Errorf("hits = %+v, want the vector-seeded entity %s", hits, fx.entityID)
	}
}

// countingEmbedder records whether the vector leg asked for an embedding.
type countingEmbedder struct{ calls *int }

func (c countingEmbedder) EmbedQuery(context.Context, string) ([]float64, error) {
	*c.calls++
	return []float64{1}, nil
}

// TestQueryWithoutWordsSkipsEveryLeg covers a query of punctuation only: it
// matches nothing, and the vector leg must not return embedding neighbours
// of punctuation. A nil store proves no leg touched the database.
func TestQueryWithoutWordsSkipsEveryLeg(t *testing.T) {
	calls := 0
	for _, q := range []string{"*", "!!!", "':()+-/", "💡"} {
		for _, kind := range []string{"entity", "chunk", "auto"} {
			hits, err := search.Query(context.Background(), nil, countingEmbedder{&calls}, q, kind, 5)
			if err != nil || hits == nil || len(hits) != 0 {
				t.Errorf("Query(%q, %s) = %v, %v; want [], nil", q, kind, hits, err)
			}
		}
	}
	if calls != 0 {
		t.Errorf("embedder called %d times, want 0", calls)
	}
}

// TestChunkSnippetCentresOnLongestWord pins snippetQuery: the raw query
// "l'<word>-Max" never occurs verbatim, and its shortest word "l" occurs
// early in unrelated text, so the snippet must centre on the unique word.
func TestChunkSnippetCentresOnLongestWord(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	docID := upsertDoc(t, st, uniqueSearchSource(t))
	t.Cleanup(func() { _ = st.DeleteDocument(ctx, docID) })
	word := uniqueToken(t)
	text := "Leading filler line that mentions lots of letters l and l again. " + strings.Repeat("padding words here ", 20) + "The l'" + word + "-Max engine starts."
	if err := st.PutChunks(ctx, docID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatal(err)
	}
	hits, err := search.Query(ctx, st, nil, "l'"+word+"-Max", "chunk", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ID == docID+"-c0" {
			if !strings.Contains(h.Snippet, word) {
				t.Errorf("snippet %q does not contain the longest search word", h.Snippet)
			}
			return
		}
	}
	t.Fatalf("fixture chunk not found in %d hits", len(hits))
}
