// Package graph_test exercises the graph package black-box: constraint
// assertions and probe DDL use a raw driver connection with independent
// matching, so a defect in production matching code cannot hide behind the
// test helpers.
package graph_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestMethodStoreSkipsWithoutEnv(t *testing.T) {
	if os.Getenv(graphtest.URIEnv) != "" {
		t.Skip("GKG_TEST_MEMGRAPH_URI is set; the skip-path assertion only runs when it is unset")
	}
	t.Run("helper", func(t *testing.T) { graphtest.Store(t) })
}

func TestOpenBadURI(t *testing.T) {
	cfg := config.Config{}
	cfg.Memgraph.URI = "://not-a-uri"
	if _, err := graph.Open(context.Background(), cfg); err == nil {
		t.Fatal("Open with malformed URI: want error")
	}
}

func TestOpenUnreachable(t *testing.T) {
	cfg := config.Config{}
	cfg.Memgraph.URI = "bolt://127.0.0.1:1"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := graph.Open(ctx, cfg); err == nil {
		t.Fatal("Open against an unreachable server: want error")
	}
}

func TestBootstrapIdempotent(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()
	if err := st.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap is not idempotent: %v", err)
	}
	for _, c := range []struct{ label, prop string }{
		{"Document", "id"},
		{"Chunk", "id"},
		{"Entity", "key"},
	} {
		if !rawUniqueConstraintExists(t, uri, c.label, c.prop) {
			t.Errorf("unique constraint on (:%s).%s missing after Bootstrap", c.label, c.prop)
		}
	}
	if !rawIndexExists(t, uri, "Document", "source_kind", "source") {
		t.Error("composite index on :Document(source_kind, source) missing after Bootstrap")
	}
	if !rawIndexExists(t, uri, "Chunk", "exact_key") {
		t.Error("index on :Chunk(exact_key) missing after Bootstrap")
	}
}

// TestBootstrapIgnoresNonUniqueConstraint pins that an EXISTS constraint on
// the same label+property does not fool Bootstrap into skipping the unique
// constraint: the observable guarantee (unique constraint present) must
// survive a pre-existing non-unique constraint.
func TestBootstrapIgnoresNonUniqueConstraint(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := context.Background()

	t.Cleanup(func() {
		_ = rawExec(uri, "DROP CONSTRAINT ON (n:Document) ASSERT EXISTS (n.id)")
		if err := st.Bootstrap(context.Background()); err != nil {
			t.Errorf("restore bootstrap: %v", err)
		}
	})

	rawRun(t, uri, "DROP CONSTRAINT ON (n:Document) ASSERT n.id IS UNIQUE")
	rawRun(t, uri, "CREATE CONSTRAINT ON (n:Document) ASSERT EXISTS (n.id)")
	if rawUniqueConstraintExists(t, uri, "Document", "id") {
		t.Fatal("precondition: unique constraint on (Document).id still present")
	}

	if err := st.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap with EXISTS constraint present: %v", err)
	}
	if !rawUniqueConstraintExists(t, uri, "Document", "id") {
		t.Error("Bootstrap skipped the unique constraint because an EXISTS constraint matched label+property")
	}
}

func TestMigrateCommand(t *testing.T) {
	uri := os.Getenv(graphtest.URIEnv)
	if uri == "" {
		t.Skipf("set %s to run integration tests", graphtest.URIEnv)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "gkg")
	build := exec.Command("go", "build", "-o", bin, "./cmd/gkg")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gkg: %v\n%s", err, out)
	}
	for i := 1; i <= 2; i++ {
		cmd := exec.Command(bin, "migrate")
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "GKG_MEMGRAPH_URI="+uri)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gkg migrate run %d: %v\n%s", i, err, out)
		}
	}
}

// rawExec runs one query over a fresh raw driver connection, treating DDL
// like any other statement.
func rawExec(uri, query string) error {
	drv, err := neo4j.NewDriver(uri, neo4j.NoAuth())
	if err != nil {
		return err
	}
	defer func() { _ = drv.Close(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = sess.Close(ctx) }()
	res, err := sess.Run(ctx, query, nil)
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}

func rawRun(t *testing.T, uri, query string) {
	t.Helper()
	if err := rawExec(uri, query); err != nil {
		t.Fatalf("raw exec %q: %v", query, err)
	}
}

// rawUniqueConstraintExists queries SHOW CONSTRAINT INFO directly with an
// independent match, deliberately not sharing production matching code.
func rawUniqueConstraintExists(t *testing.T, uri, label, prop string) bool {
	t.Helper()
	for _, r := range rawRecords(t, uri, "SHOW CONSTRAINT INFO") {
		tv, _ := r.Get("constraint type")
		if !strings.Contains(fmt.Sprint(tv), "unique") {
			continue
		}
		lv, _ := r.Get("label")
		if fmt.Sprint(lv) != label {
			continue
		}
		pv, _ := r.Get("properties")
		switch p := pv.(type) {
		case string:
			if p == prop {
				return true
			}
		case []any:
			for _, e := range p {
				if fmt.Sprint(e) == prop {
					return true
				}
			}
		}
	}
	return false
}

// rawIndexExists queries SHOW INDEX INFO directly; props must match as a set.
func rawIndexExists(t *testing.T, uri, label string, props ...string) bool {
	t.Helper()
	for _, r := range rawRecords(t, uri, "SHOW INDEX INFO") {
		lv, _ := r.Get("label")
		if fmt.Sprint(lv) != label {
			continue
		}
		pv, ok := r.Get("property")
		if !ok {
			pv, _ = r.Get("properties")
		}
		var list []string
		switch p := pv.(type) {
		case string:
			list = []string{p}
		case []any:
			for _, e := range p {
				list = append(list, fmt.Sprint(e))
			}
		case []string:
			list = p
		}
		if len(list) != len(props) {
			continue
		}
		set := map[string]bool{}
		for _, e := range list {
			set[e] = true
		}
		all := true
		for _, want := range props {
			if !set[want] {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// rawRecords runs a read query over a fresh raw driver connection.
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
