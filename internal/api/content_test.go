package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

// TestDocumentContentEndpoint pins the reader payload: document metadata,
// the folded + rendered html, and the ordered chunk index.
func TestDocumentContentEndpoint(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: apiSource(t), Hash: "hash-api-content", Title: "Content doc"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	chunk0 := "# Reader heading\n\n## Section\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```go\nfmt.Println(\"hi\")\n```\n\nFirst part of the body."
	chunk1 := "Second part of the body."
	chunks := []graph.Chunk{
		{Seq: 0, Text: chunk0, Start: 0, End: len(chunk0)},
		{Seq: 1, Text: chunk1, Start: len(chunk0) + 2, End: len(chunk0) + 2 + len(chunk1)},
	}
	if err := st.PutChunks(ctx, res.ID, chunks); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}

	h := apiHandler(t, config.Config{}, st, nil)
	rr := do(t, h, http.MethodGet, "/v1/documents/"+res.ID+"/content", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body %s", rr.Code, rr.Body.String())
	}
	got := decode[contentResponse](t, rr)
	if got.Document.ID != res.ID || got.Document.Title != "Content doc" {
		t.Errorf("document = %+v, want id %s title Content doc", got.Document, res.ID)
	}
	if len(got.Chunks) != 2 {
		t.Fatalf("chunks = %+v, want 2 entries", got.Chunks)
	}
	if got.Chunks[0].ID != res.ID+"-c0" || got.Chunks[1].ID != res.ID+"-c1" ||
		got.Chunks[0].Seq != 0 || got.Chunks[1].Seq != 1 {
		t.Errorf("chunk index = %+v, want -c0/-c1 in seq order", got.Chunks)
	}
	if got.Chunks[1].Start != len(chunk0)+2 || got.Chunks[1].End != len(chunk0)+2+len(chunk1) {
		t.Errorf("chunk 1 offsets = %+v", got.Chunks[1])
	}
	for _, want := range []string{"<h1>Reader heading</h1>", "<h2>Section</h2>", "<table>", "<pre>"} {
		if !strings.Contains(got.HTML, want) {
			t.Errorf("html misses %q\n%s", want, got.HTML)
		}
	}
	if !strings.Contains(got.HTML, "First part of the body.") || !strings.Contains(got.HTML, "Second part of the body.") {
		t.Errorf("html misses folded body text\n%s", got.HTML)
	}
}

// TestDocumentContentNotFound pins the store-error mapping for unknown ids.
func TestDocumentContentNotFound(t *testing.T) {
	st := graphtest.Store(t)
	h := apiHandler(t, config.Config{}, st, nil)
	rr := do(t, h, http.MethodGet, "/v1/documents/00000000/content", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "store_error") {
		t.Errorf("body = %s, want the store_error envelope", rr.Body.String())
	}
}

// TestDocumentContentSuperseded pins that a superseded version answers with
// its status and no text (the supersede GC removed its chunks).
func TestDocumentContentSuperseded(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	src := apiSource(t)
	old, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: src, Hash: "hash-api-content-old"})
	if err != nil {
		t.Fatalf("UpsertDocument old: %v", err)
	}
	text := "Old version body sentence."
	if err := st.PutChunks(ctx, old.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	newer, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: src, Hash: "hash-api-content-new"})
	if err != nil {
		t.Fatalf("UpsertDocument new: %v", err)
	}
	if err := st.Supersede(ctx, old.ID, newer.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	h := apiHandler(t, config.Config{}, st, nil)
	rr := do(t, h, http.MethodGet, "/v1/documents/"+old.ID+"/content", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body %s", rr.Code, rr.Body.String())
	}
	got := decode[contentResponse](t, rr)
	if got.Document.Status != "superseded" {
		t.Errorf("status = %q, want superseded", got.Document.Status)
	}
	if got.HTML != "" || len(got.Chunks) != 0 {
		t.Errorf("superseded html = %q chunks = %d, want no stored text", got.HTML, len(got.Chunks))
	}
}

// TestDocumentContentBearerPolicy pins that the content endpoint inherits
// the bearer policy — no new exemptions.
func TestDocumentContentBearerPolicy(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: apiSource(t), Hash: "hash-api-content-key"})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	cfg := config.Config{}
	cfg.Server.APIKey = "sekret"
	h := NewServer(cfg, st, nil).Handler()

	rr := do(t, h, http.MethodGet, "/v1/documents/"+res.ID+"/content", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("without a key: code = %d, want 401", rr.Code)
	}
	rr = do(t, h, http.MethodGet, "/v1/documents/"+res.ID+"/content", nil, map[string]string{"Authorization": "Bearer sekret"})
	if rr.Code != http.StatusOK {
		t.Errorf("with a key: code = %d, want 200; body %s", rr.Code, rr.Body.String())
	}
}
