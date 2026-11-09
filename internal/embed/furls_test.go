package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// furlsTestEntryID mirrors the provider's content-derived entry id: the
// embeddings service caches by content hash, so ids are stable per text.
func furlsTestEntryID(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "gkg:" + hex.EncodeToString(sum[:])[:32]
}

// furlsRequestBody is one decoded request to a FURL endpoint.
type furlsRequestBody struct {
	Entries []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	} `json:"entries"`
	Dimensions *int   `json:"dimensions"`
	Model      string `json:"model"`
}

func decodeFurlsRequest(t *testing.T, r *http.Request) furlsRequestBody {
	t.Helper()
	var req furlsRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode furls request: %v", err)
	}
	return req
}

// furlsConfig builds a config pointing both sides at url (tests override as
// needed); dim 2 keeps vectors small and readable.
func furlsConfig(url string) config.Config {
	cfg := config.Config{}
	cfg.Embedder.ProductFURL = url
	cfg.Embedder.QueryFURL = url
	cfg.Embedder.APIKey = "sekret"
	cfg.Embedder.Dim = 2
	return cfg
}

func TestFurlsDocumentsShape(t *testing.T) {
	t.Run("request shape and id-ordered mapping", func(t *testing.T) {
		var gotKey string
		var got furlsRequestBody
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			gotKey = r.Header.Get("x-api-key")
			got = decodeFurlsRequest(t, r)
			// Out of input order: the provider must map results by id.
			fmt.Fprintf(w, `{"results":[{"id":%q,"embedding":[7,8]},{"id":%q,"embedding":[1,2]}]}`,
				furlsTestEntryID("beta"), furlsTestEntryID("alpha"))
		}))
		defer ts.Close()

		p := NewFurls(furlsConfig(ts.URL))
		if p.Name() != "furls" {
			t.Errorf("Name = %q, want furls", p.Name())
		}
		vecs, err := p.EmbedDocuments(context.Background(), []string{"alpha", "beta"})
		if err != nil {
			t.Fatalf("EmbedDocuments: %v", err)
		}
		if gotKey != "sekret" {
			t.Errorf("x-api-key = %q, want sekret", gotKey)
		}
		if got.Model != furlsDefaultModel {
			t.Errorf("model = %q, want the default %q", got.Model, furlsDefaultModel)
		}
		if got.Dimensions == nil || *got.Dimensions != 2 {
			t.Errorf("dimensions = %v, want the configured dim 2", got.Dimensions)
		}
		if len(got.Entries) != 2 {
			t.Fatalf("entries = %+v, want 2", got.Entries)
		}
		if got.Entries[0].ID != furlsTestEntryID("alpha") || got.Entries[0].Text != "alpha" ||
			got.Entries[1].ID != furlsTestEntryID("beta") || got.Entries[1].Text != "beta" {
			t.Errorf("entries = %+v, want content-hash ids in input order", got.Entries)
		}
		if len(vecs) != 2 || len(vecs[0]) != 2 || vecs[0][0] != 1 || vecs[1][0] != 7 {
			t.Errorf("vectors = %v, want input order (mapped by id)", vecs)
		}
	})

	t.Run("model override sent verbatim", func(t *testing.T) {
		var got furlsRequestBody
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = decodeFurlsRequest(t, r)
			fmt.Fprintf(w, `{"results":[{"id":%q,"embedding":[1,2]}]}`, furlsTestEntryID("a"))
		}))
		defer ts.Close()
		cfg := furlsConfig(ts.URL)
		cfg.Embedder.Model = "custom-model"
		p := NewFurls(cfg)
		if _, err := p.EmbedDocuments(context.Background(), []string{"a"}); err != nil {
			t.Fatalf("EmbedDocuments: %v", err)
		}
		if got.Model != "custom-model" {
			t.Errorf("model = %q, want the configured override sent verbatim", got.Model)
		}
	})

	t.Run("dimensions omitted when unset", func(t *testing.T) {
		var got furlsRequestBody
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = decodeFurlsRequest(t, r)
			fmt.Fprintf(w, `{"results":[{"id":%q,"embedding":[1,2]}]}`, furlsTestEntryID("a"))
		}))
		defer ts.Close()
		cfg := furlsConfig(ts.URL)
		cfg.Embedder.Dim = 0
		p := NewFurls(cfg)
		if _, err := p.EmbedDocuments(context.Background(), []string{"a"}); err != nil {
			t.Fatalf("EmbedDocuments: %v", err)
		}
		if got.Dimensions != nil {
			t.Errorf("dimensions = %v, want it omitted when Dim is 0", *got.Dimensions)
		}
	})

	t.Run("position fallback when ids do not match", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"results":[{"id":"other-1","embedding":[1,2]},{"id":"other-2","embedding":[3,4]}]}`)
		}))
		defer ts.Close()
		p := NewFurls(furlsConfig(ts.URL))
		vecs, err := p.EmbedDocuments(context.Background(), []string{"alpha", "beta"})
		if err != nil {
			t.Fatalf("EmbedDocuments: %v", err)
		}
		if len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][0] != 3 {
			t.Errorf("vectors = %v, want the position-aligned results", vecs)
		}
	})
}

func TestFurlsUsableEmbeddingDespiteErrorField(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		// An error may accompany a usable vector (persistence-emit nuance):
		// the vector is usable and the entry must not be retried.
		fmt.Fprintf(w, `{"results":[{"id":%q,"embedding":[1,2],"error":"persist failed","error_code":"UPSTREAM_ERROR"}]}`,
			furlsTestEntryID("a"))
	}))
	defer ts.Close()
	p := NewFurls(furlsConfig(ts.URL))
	vecs, err := p.EmbedDocuments(context.Background(), []string{"a"})
	if err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	if len(vecs) != 1 || vecs[0][0] != 1 {
		t.Errorf("vectors = %v, want the usable vector", vecs)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("requests = %d, want 1 (a usable vector is never retried)", got)
	}
}

func TestFurlsRetriesRetryableEntriesOnce(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		req := decodeFurlsRequest(t, r)
		if n == 1 {
			if len(req.Entries) != 2 {
				t.Errorf("first request entries = %d, want 2", len(req.Entries))
			}
			fmt.Fprintf(w, `{"results":[{"id":%q,"embedding":[1,2]},{"id":%q,"error":"limited","error_code":"RATE_LIMITED"}]}`,
				furlsTestEntryID("alpha"), furlsTestEntryID("beta"))
			return
		}
		if len(req.Entries) != 1 || req.Entries[0].Text != "beta" {
			t.Errorf("retry entries = %+v, want exactly the failed entry", req.Entries)
		}
		fmt.Fprintf(w, `{"results":[{"id":%q,"embedding":[3,4]}]}`, furlsTestEntryID("beta"))
	}))
	defer ts.Close()
	p := NewFurls(furlsConfig(ts.URL))
	vecs, err := p.EmbedDocuments(context.Background(), []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("requests = %d, want exactly one retry", got)
	}
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][0] != 3 {
		t.Errorf("vectors = %v, want the first-pass and retried vectors merged in input order", vecs)
	}
}

func TestFurlsNoRetryNonRetryable(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprintf(w, `{"results":[{"id":%q,"embedding":[1,2]},{"id":%q,"error":"bad input","error_code":"INVALID_INPUT"}]}`,
			furlsTestEntryID("alpha"), furlsTestEntryID("beta"))
	}))
	defer ts.Close()
	p := NewFurls(furlsConfig(ts.URL))
	_, err := p.EmbedDocuments(context.Background(), []string{"alpha", "beta"})
	if err == nil {
		t.Fatal("want error for the non-retryable entry")
	}
	if !strings.Contains(err.Error(), furlsTestEntryID("beta")) || !strings.Contains(err.Error(), "INVALID_INPUT") {
		t.Errorf("err = %v, want it to name the failed entry id and INVALID_INPUT", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("requests = %d, want 1 (non-retryable failures do not retry)", got)
	}
}

func TestFurlsMissingEmbeddingErrors(t *testing.T) {
	t.Run("empty embedding", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"results":[{"id":%q,"embedding":[1,2]},{"id":%q}]}`,
				furlsTestEntryID("alpha"), furlsTestEntryID("beta"))
		}))
		defer ts.Close()
		p := NewFurls(furlsConfig(ts.URL))
		_, err := p.EmbedDocuments(context.Background(), []string{"alpha", "beta"})
		if err == nil || !strings.Contains(err.Error(), furlsTestEntryID("beta")) || !strings.Contains(err.Error(), "no embedding") {
			t.Fatalf("err = %v, want it to name the entry and the missing embedding", err)
		}
	})
	t.Run("absent result", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// Only one result for two entries.
			fmt.Fprintf(w, `{"results":[{"id":%q,"embedding":[1,2]}]}`, furlsTestEntryID("alpha"))
		}))
		defer ts.Close()
		p := NewFurls(furlsConfig(ts.URL))
		_, err := p.EmbedDocuments(context.Background(), []string{"alpha", "beta"})
		if err == nil || !strings.Contains(err.Error(), furlsTestEntryID("beta")) {
			t.Fatalf("err = %v, want it to name the entry without a result", err)
		}
	})
}

func TestFurlsDimensionMismatchErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"results":[{"id":%q,"embedding":[1,2,3]}]}`, furlsTestEntryID("a"))
	}))
	defer ts.Close()
	p := NewFurls(furlsConfig(ts.URL)) // dim 2
	_, err := p.EmbedDocuments(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "dimension") {
		t.Fatalf("err = %v, want a dimension mismatch error", err)
	}
}

func TestFurlsNon2xxCappedBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, strings.Repeat("x", 6000)+"TAIL")
	}))
	defer ts.Close()
	p := NewFurls(furlsConfig(ts.URL))
	_, err := p.EmbedDocuments(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the HTTP status surfaced", err)
	}
	if strings.Contains(err.Error(), "TAIL") {
		t.Errorf("err = %v, want the body capped at 4KB (no tail)", err)
	}
}

func TestFurlsEmptyInput(t *testing.T) {
	p := NewFurls(furlsConfig("http://example.invalid"))
	if _, err := p.EmbedDocuments(context.Background(), nil); err == nil {
		t.Error("nil input: want error")
	}
	if _, err := p.EmbedDocuments(context.Background(), []string{}); err == nil {
		t.Error("empty input: want error")
	}
}

func TestFurlsContextCancel(t *testing.T) {
	// Consume the body first: Go's server only starts its disconnect-
	// watching background read once a request's body has been consumed
	// (without it, r.Context() never fires and httptest Close deadlocks).
	// The release channel is the belt-and-braces unblock for cleanup.
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		ts.Close()
	})
	p := NewFurls(furlsConfig(ts.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := p.EmbedDocuments(ctx, []string{"a"}); err == nil {
		t.Fatal("want a context error")
	}
}

func TestFurlsQueryShape(t *testing.T) {
	var productCalls, queryCalls int32
	product := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&productCalls, 1)
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer product.Close()
	var got furlsRequestBody
	query := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&queryCalls, 1)
		got = decodeFurlsRequest(t, r)
		fmt.Fprintf(w, `{"results":[{"id":%q,"embedding":[5,6]}]}`, furlsTestEntryID("hello"))
	}))
	defer query.Close()

	cfg := furlsConfig(product.URL)
	cfg.Embedder.QueryFURL = query.URL
	p := NewFurls(cfg)
	vec, err := p.EmbedQuery(context.Background(), "hello")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if atomic.LoadInt32(&productCalls) != 0 || atomic.LoadInt32(&queryCalls) != 1 {
		t.Errorf("calls = product %d, query %d; want the query FURL only", productCalls, queryCalls)
	}
	if len(got.Entries) != 1 || got.Entries[0].ID != furlsTestEntryID("hello") || got.Entries[0].Text != "hello" {
		t.Errorf("entries = %+v, want the single query entry", got.Entries)
	}
	if len(vec) != 2 || vec[0] != 5 || vec[1] != 6 {
		t.Errorf("vector = %v, want the returned vector", vec)
	}
}
