// Package graphtest provides test support for code that needs a live
// Memgraph-backed graph.Store. It lives in its own package so the production
// graph package — and therefore the gkg binary — never imports testing.
// Import it only from _test.go files.
package graphtest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// URIEnv names the environment variable that enables integration tests
// against a live Memgraph instance.
const URIEnv = "GKG_TEST_MEMGRAPH_URI"

// Store opens a graph.Store against URIEnv and bootstraps the schema.
// Without the variable the test is skipped, so `go test ./...` stays green
// offline. It waits up to 30s for the server to accept connections (CI
// service containers take a moment to become ready).
func Store(t *testing.T) *graph.Store {
	t.Helper()
	uri := os.Getenv(URIEnv)
	if uri == "" {
		t.Skipf("set %s to run integration tests", URIEnv)
	}
	cfg := config.Config{}
	cfg.Memgraph.URI = uri

	var st *graph.Store
	var err error
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err = graph.Open(ctx, cfg)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("open memgraph at %s: %v", uri, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := st.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}
