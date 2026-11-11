package graph_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// Run only against the disposable, initially unauthenticated integration DB.
// Creating its first user enables auth; cleanup removes that user again.
func TestOpenAuthenticated(t *testing.T) {
	uri := os.Getenv(graphtest.URIEnv)
	if uri == "" {
		t.Skip("requires a disposable Memgraph integration database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rawRun(t, uri, "CREATE USER task32atest IDENTIFIED BY 'test-only-password'")
	drv, err := neo4j.NewDriver(uri, neo4j.BasicAuth("task32atest", "test-only-password", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer drv.Close(context.Background())
	defer func() {
		sess := drv.NewSession(context.Background(), neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
		defer sess.Close(context.Background())
		res, err := sess.Run(context.Background(), "DROP USER task32atest", nil)
		if err == nil {
			_, err = res.Consume(context.Background())
		}
		if err != nil {
			t.Errorf("remove integration auth fixture: %v", err)
		}
	}()
	cfg := config.Config{}
	cfg.Memgraph.URI = uri
	cfg.Memgraph.User = "task32atest"
	cfg.Memgraph.Password = "test-only-password"
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(context.Background())
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RunReadQuery(ctx, "RETURN 1 AS n", nil, 0); err != nil {
		t.Fatalf("authenticated read: %v", err)
	}
	testUpsert(t, st, "file", "auth-smoke", "auth-smoke")
	cfg.Memgraph.Password = "incorrect-test-password"
	if bad, err := graph.Open(ctx, cfg); err == nil {
		bad.Close(context.Background())
		t.Fatal("incorrect credentials were accepted")
	}
}
