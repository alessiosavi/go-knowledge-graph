package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const (
	testProductID     = "rest-push.test:1"
	testProductPut    = "/v1/products"
	testProductDelete = "/v1/products?product_id=rest-push.test%3A1&seq=2"
	testProductEvent  = `{"seq":2,"record":{"product_id":"rest-push.test:1"}}`
)

type productMutationStore struct {
	graphStore
	result graph.ProductMutationResult
	err    error
	seq    int64
	input  string
}

func (s *productMutationStore) PushProduct(ctx context.Context, seq int64, record []byte) (graph.ProductMutationResult, error) {
	return s.mutate(ctx, seq, string(record))
}

func (s *productMutationStore) RemoveProduct(ctx context.Context, id string, seq int64) (graph.ProductMutationResult, error) {
	return s.mutate(ctx, seq, id)
}

func (s *productMutationStore) mutate(ctx context.Context, seq int64, input string) (graph.ProductMutationResult, error) {
	if _, ok := ctx.Deadline(); ok {
		return graph.ProductMutationResult{}, errors.New("unexpected REST read timeout on mutation")
	}
	s.seq, s.input = seq, input
	return s.result, s.err
}

func productMutationRequest(t *testing.T, store *productMutationStore, method string) *httptest.ResponseRecorder {
	t.Helper()
	h := (&Server{store: store}).Handler()
	if method == http.MethodPut {
		return doJSON(t, h, method, testProductPut, testProductEvent)
	}
	return do(t, h, method, testProductDelete, nil, nil)
}

func TestProductMutationSuccess(t *testing.T) {
	for _, tc := range []struct{ status, method, input string }{
		{"created", http.MethodPut, `{"product_id":"rest-push.test:1"}`},
		{"updated", http.MethodPut, `{"product_id":"rest-push.test:1"}`},
		{"deleted", http.MethodDelete, testProductID},
		{"absent", http.MethodDelete, testProductID},
		{"duplicate", http.MethodPut, `{"product_id":"rest-push.test:1"}`},
		{"duplicate", http.MethodDelete, testProductID},
	} {
		t.Run(tc.method+"/"+tc.status, func(t *testing.T) {
			result := graph.ProductMutationResult{Status: tc.status, ProductID: testProductID, ID: "graph-id", SeenSeq: 2}
			store := &productMutationStore{result: result}
			rr := productMutationRequest(t, store, tc.method)
			if rr.Code != 200 || decode[graph.ProductMutationResult](t, rr) != result {
				t.Fatalf("response = %d %s, want 200 %+v", rr.Code, rr.Body, result)
			}
			if store.seq != 2 || store.input != tc.input {
				t.Fatalf("store received (%d, %q), want (2, %q)", store.seq, store.input, tc.input)
			}
		})
	}
}

func TestProductMutationErrors(t *testing.T) {
	seen := int64(2)
	tests := []struct {
		err  error
		code int
		kind string
		seen *int64
	}{
		{argerr.New("product_id must have a non-empty suffix"), 400, "invalid_request", nil},
		{&graph.ProductMutationError{Status: "stale", SeenSeq: &seen}, 409, "stale", &seen},
		{&graph.ProductMutationError{Status: "seq_conflict", SeenSeq: &seen}, 409, "seq_conflict", &seen},
		{&graph.ProductMutationError{Status: "release_changed"}, 409, "release_changed", nil},
		{&graph.ProductMutationError{Status: "release_not_materialized"}, 409, "release_not_materialized", nil},
		{&graph.ProductMutationError{Status: "store_not_bound"}, 409, "store_not_bound", nil},
		{&graph.ProductMutationError{Status: "store_cap"}, 409, "store_cap", nil},
		{&graph.ProductMutationError{Status: "retry"}, 409, "retry", nil},
		{&graph.ProductMutationError{Status: "reimport_required"}, 409, "reimport_required", nil},
		{&graph.ProductMutationError{Status: "unknown_outcome"}, 503, "unknown_outcome", nil},
		{&graph.ProductMutationError{Status: "release_future", SeenSeq: &seen}, 500, "store_error", nil},
		{errors.New("backend error"), 500, "store_error", nil},
		{graph.ErrNotFound, 500, "store_error", nil},
		{context.DeadlineExceeded, 500, "store_error", nil},
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		for _, tc := range tests {
			t.Run(method+"/"+tc.err.Error(), func(t *testing.T) {
				rr := productMutationRequest(t, &productMutationStore{err: fmt.Errorf("fixture: %w", tc.err)}, method)
				assertProductMutationError(t, rr, tc.code, tc.kind, tc.seen)
			})
		}
	}
}

func assertProductMutationError(t *testing.T, rr *httptest.ResponseRecorder, code int, kind string, seen *int64) {
	t.Helper()
	if rr.Code != code || rr.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %s, want %d JSON %s", rr.Code, rr.Body, code, kind)
	}
	body := decode[map[string]any](t, rr)
	detail, _ := body["detail"].(string)
	if body["error"] != kind || detail == "" {
		t.Errorf("error payload = %v, want error %q and detail", body, kind)
	}
	value, present := body["seen_seq"]
	if seen == nil && (present || len(body) != 2) {
		t.Errorf("error payload = %v, want only error and detail", body)
	}
	if seen != nil && (!present || value != float64(*seen) || len(body) != 3) {
		t.Errorf("error payload = %v, want error, detail and seen_seq=%d", body, *seen)
	}
}

func TestProductPutStrictEnvelope(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"trailing JSON", testProductEvent + `{}`},
		{"trailing non-JSON", testProductEvent + `x`},
		{"duplicate seq", `{"seq":1,"seq":2,"record":{}}`},
		{"duplicate record", `{"seq":2,"record":{},"record":{}}`},
		{"unknown member", `{"seq":2,"record":{},"extra":null}`},
		{"missing seq", `{"record":{}}`},
		{"missing record", `{"seq":2}`},
		{"non-object", `[]`},
		{"null", `null`},
		{"empty body", ""},
		{"non-positive seq", `{"seq":0,"record":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &productMutationStore{}
			rr := doJSON(t, (&Server{store: store}).Handler(), http.MethodPut, testProductPut, tc.body)
			assertProductMutationError(t, rr, 400, "invalid_request", nil)
			if store.seq != 0 {
				t.Fatal("invalid envelope reached the store")
			}
		})
	}
}

func TestProductPutBodyCap(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		for _, delta := range []int{0, 1} {
			t.Run(fmt.Sprintf("chunked=%t/over_cap_by=%d", chunked, delta), func(t *testing.T) {
				store := &productMutationStore{}
				body := testProductEvent + strings.Repeat(" ", productobs.MaxPushEventBytes-len(testProductEvent)+delta)
				req := httptest.NewRequest(http.MethodPut, testProductPut, strings.NewReader(body))
				if chunked {
					req.ContentLength = -1
					req.TransferEncoding = []string{"chunked"}
				}
				rr := httptest.NewRecorder()
				(&Server{store: store}).Handler().ServeHTTP(rr, req)
				if delta == 0 {
					if rr.Code != 200 || store.seq != 2 {
						t.Fatalf("exact cap: response = %d %s, store seq = %d", rr.Code, rr.Body, store.seq)
					}
				} else {
					assertProductMutationError(t, rr, 413, "body_too_large", nil)
					if store.seq != 0 {
						t.Fatal("oversize body reached the store")
					}
				}
			})
		}
	}
}

func TestProductDeleteQuery(t *testing.T) {
	for _, query := range []string{
		"", "?seq=1", "?product_id=test:1", "?product_id=&seq=1", "?product_id=test:1&seq=",
		"?product_id=test:1&product_id=test:1&seq=1", "?product_id=test:1&seq=1&seq=1",
		"?product_id=test:1&seq=1.0", "?product_id=test:1&seq=no", "?product_id=test:1&seq=0",
		"?product_id=test:1&seq=-1", "?product_id=test:1&seq=9223372036854775808",
		"?product_id=test:1&seq=1&extra=1", "?product_id=test:1&seq=1&%GG=x",
	} {
		t.Run(query, func(t *testing.T) {
			store := &productMutationStore{}
			h := (&Server{store: store}).Handler()
			rr := do(t, h, http.MethodDelete, testProductPut+query, nil, nil)
			assertProductMutationError(t, rr, 400, "invalid_request", nil)
			if store.seq != 0 {
				t.Fatal("invalid query reached the store")
			}
		})
	}
}

func TestProductUnsupportedMethods(t *testing.T) {
	h := (&Server{}).Handler()
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		rr := do(t, h, method, testProductPut, nil, nil)
		if rr.Code != 405 {
			t.Errorf("%s /v1/products returned %d, want 405", method, rr.Code)
		}
		got := rr.Header().Get("Allow")
		if got != "DELETE, GET, HEAD, PUT" {
			t.Errorf("Allow = %q, want DELETE, GET, HEAD, PUT", got)
		}
	}
}

func TestProductPutBodyReadError(t *testing.T) {
	store := &productMutationStore{}
	body := io.MultiReader(strings.NewReader(testProductEvent), failingProductBody{})
	rr := do(t, (&Server{store: store}).Handler(), http.MethodPut, testProductPut, body, nil)
	assertProductMutationError(t, rr, 400, "invalid_request", nil)
	if store.seq != 0 {
		t.Fatal("incomplete body reached the store")
	}
}

type failingProductBody struct{}

func (failingProductBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
