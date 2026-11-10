// Package graphtest provides test support for code that needs a live
// Memgraph-backed graph.Store. It lives in its own package so the production
// graph package — and therefore the gkg binary — never imports testing.
// Import it only from _test.go files.
package graphtest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// URIEnv names the environment variable that enables integration tests
// against a live Memgraph instance.
const URIEnv = "GKG_TEST_MEMGRAPH_URI"

// Vector returns a deterministic synthetic embedding for seed: three sparse
// spikes derived from a hash of the seed plus a small deterministic dense
// component (0.05 against the spike norm — ≈4.4% of the vector's magnitude,
// chained sha256). The spike-only form creates a mass exact-tie plateau (most
// pairs sit at exactly distance 1.0), which makes Memgraph's approximate
// vector-search walk navigation-degenerate and was co-pinned to it
// deterministically missing specific freshly inserted nodes at small k
// (2026-11-10 CI-flake investigation); the dense component turns the plateau
// into a graded field while keeping determinism and a unique ~0-distance
// self-match. Do not remove the dense component.
func Vector(seed string, dim int) []float64 {
	h := sha256.Sum256([]byte(seed))
	vec := make([]float64, dim)
	vec[int(h[0])*3%dim] = 1
	vec[int(h[1])*5%dim] = 0.5
	vec[int(h[2])*7%dim] = 0.25
	noise := make([]float64, dim)
	for block := 0; block*32 < dim; block++ {
		hb := sha256.Sum256([]byte(fmt.Sprintf("dense|%s|%d", seed, block)))
		for j := 0; j < 32; j++ {
			i := block*32 + j
			if i >= dim {
				break
			}
			noise[i] = float64(hb[j])/127.5 - 1.0
		}
	}
	var norm float64
	for _, v := range noise {
		norm += v * v
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range vec {
			vec[i] += 0.05 * noise[i] / norm
		}
	}
	return vec
}

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
