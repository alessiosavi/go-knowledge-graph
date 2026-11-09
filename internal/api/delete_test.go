package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

// deleteSeed creates one document version with a chunk and returns its id.
func deleteSeed(t *testing.T, st *graph.Store, source, hash string) string {
	t.Helper()
	ctx := context.Background()
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{
		Kind: "file", Source: source, Hash: hash, Title: "Delete doc",
	})
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	text := "Delete me sentence."
	if err := st.PutChunks(ctx, res.ID, []graph.Chunk{{Seq: 0, Text: text, Start: 0, End: len(text)}}); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	return res.ID
}

// TestDeleteDocumentEndpoint pins the plain (per-version) delete: 200 with
// the removed count, the document gone, and a second delete a store-error
// 404.
func TestDeleteDocumentEndpoint(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	id := deleteSeed(t, st, apiSource(t), "hash-api-delete")
	h := apiHandler(t, config.Config{}, st, nil)

	rr := do(t, h, http.MethodDelete, "/v1/documents/"+id, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE: code = %d, body %s", rr.Code, rr.Body.String())
	}
	got := decode[deleteResponse](t, rr)
	if got.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", got.Deleted)
	}
	if _, err := st.Document(ctx, id); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("Document after delete = %v, want ErrNotFound", err)
	}
	rr = do(t, h, http.MethodDelete, "/v1/documents/"+id, nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("second DELETE: code = %d, want 404; body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "store_error") {
		t.Errorf("second DELETE body = %s, want the store_error envelope", rr.Body.String())
	}
}

// TestDeleteDocumentKeepsOtherVersions pins the per-version default: the
// DELETE without the chain flag removes exactly the addressed version.
func TestDeleteDocumentKeepsOtherVersions(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	src := apiSource(t)
	v1 := deleteSeed(t, st, src, "hash-api-del-v1")
	res2, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: src, Hash: "hash-api-del-v2"})
	if err != nil {
		t.Fatalf("UpsertDocument v2: %v", err)
	}
	h := apiHandler(t, config.Config{}, st, nil)

	rr := do(t, h, http.MethodDelete, "/v1/documents/"+v1, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE v1: code = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := decode[deleteResponse](t, rr); got.Deleted != 1 {
		t.Errorf("deleted = %d, want 1 (per-version default)", got.Deleted)
	}
	if _, err := st.Document(ctx, v1); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("v1 after delete = %v, want ErrNotFound", err)
	}
	if _, err := st.Document(ctx, res2.ID); err != nil {
		t.Errorf("v2 must survive the per-version delete: %v", err)
	}
}

// TestDeleteDocumentChainFlag pins ?chain=1: every version of the identity
// is removed and the count reflects the chain size.
func TestDeleteDocumentChainFlag(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	src := apiSource(t)
	v1 := deleteSeed(t, st, src, "hash-api-chain-v1")
	res2, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "file", Source: src, Hash: "hash-api-chain-v2"})
	if err != nil {
		t.Fatalf("UpsertDocument v2: %v", err)
	}
	if err := st.Supersede(ctx, v1, res2.ID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	h := apiHandler(t, config.Config{}, st, nil)

	rr := do(t, h, http.MethodDelete, "/v1/documents/"+v1+"?chain=1", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE ?chain=1: code = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := decode[deleteResponse](t, rr); got.Deleted != 2 {
		t.Errorf("deleted = %d, want 2 (the whole chain)", got.Deleted)
	}
	for _, id := range []string{v1, res2.ID} {
		if _, err := st.Document(ctx, id); !errors.Is(err, graph.ErrNotFound) {
			t.Errorf("version %s after chain delete = %v, want ErrNotFound", id, err)
		}
	}
	ids, err := st.SameIdentityDocumentIDs(ctx, "file", src)
	if err != nil {
		t.Fatalf("SameIdentityDocumentIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("identity still holds %d documents after chain delete", len(ids))
	}
}

// TestDeleteDocumentNotFound pins the missing-id contract.
func TestDeleteDocumentNotFound(t *testing.T) {
	st := graphtest.Store(t)
	h := apiHandler(t, config.Config{}, st, nil)
	rr := do(t, h, http.MethodDelete, "/v1/documents/00000000", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "store_error") {
		t.Errorf("body = %s, want the store_error envelope", rr.Body.String())
	}
}

// TestDeleteDocumentBearerPolicy pins that the DELETE endpoint inherits the
// bearer policy — no new exemptions.
func TestDeleteDocumentBearerPolicy(t *testing.T) {
	st := graphtest.Store(t)
	id := deleteSeed(t, st, apiSource(t), "hash-api-del-key")
	cfg := config.Config{}
	cfg.Server.APIKey = "sekret"
	h := NewServer(cfg, st, nil).Handler()

	rr := do(t, h, http.MethodDelete, "/v1/documents/"+id, nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("without a key: code = %d, want 401", rr.Code)
	}
	rr = do(t, h, http.MethodDelete, "/v1/documents/"+id, nil, map[string]string{"Authorization": "Bearer sekret"})
	if rr.Code != http.StatusOK {
		t.Errorf("with a key: code = %d, want 200; body %s", rr.Code, rr.Body.String())
	}
}
