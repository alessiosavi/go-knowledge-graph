package graph

// This file hosts test support that must be importable by other packages'
// tests (for example internal/ingest). The go tool only accepts functions of
// the exact shape func Test<Name>(t *testing.T) inside _test.go files, and a
// helper that returns *Store cannot satisfy that, so TestStore lives here as
// regular package code.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// testURIEnv names the environment variable that enables integration tests
// against a live Memgraph instance.
const testURIEnv = "GKG_TEST_MEMGRAPH_URI"

// TestStore opens a Store against GKG_TEST_MEMGRAPH_URI and bootstraps the
// schema. Without the variable the test is skipped, so `go test ./...` stays
// green offline. It waits up to 30s for the server to accept connections
// (CI service containers take a moment to become ready).
func TestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv(testURIEnv)
	if uri == "" {
		t.Skipf("set %s to run integration tests", testURIEnv)
	}
	cfg := config.Config{}
	cfg.Memgraph.URI = uri

	var st *Store
	var err error
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err = Open(ctx, cfg)
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
