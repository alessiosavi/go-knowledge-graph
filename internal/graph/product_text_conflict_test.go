package graph_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestProductTextConflict(t *testing.T) {
	graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	drv, err := neo4j.NewDriver(uri, neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close(context.Background()) })

	link := productTextWrite{"edge", `
MATCH (n:ProductText {id: $prefix + 'text'})
CREATE (:Product {id: $prefix + 'product', store: $prefix})-[r:HAS_TEXT]->(n)
RETURN count(r) AS matched`, 1}
	set := productTextWrite{"property", `
MATCH (n:ProductText {id: $prefix + 'text'})
SET n.embedding = $embedding
RETURN count(n) AS matched`, 1}
	for _, pair := range [][2]productTextWrite{{link, set}, {set, link}} {
		t.Run("shared_"+pair[0].name+"_first", func(t *testing.T) {
			params := seedProductTextConflict(t, uri)
			err := runProductTextWritePair(t, drv, params, pair[0], pair[1])
			if !graph.IsTransientWriteError(err) {
				t.Fatalf("second write = %v, want a serialization conflict", err)
			}
			t.Logf("second write aborted: %q", err)
		})
	}

	const index = "t1a_product_text_conflict"
	rawRun(t, uri, "CREATE VECTOR INDEX "+index+` ON :TextVec(embedding)
WITH CONFIG {"dimension": 768, "capacity": 16, "metric": "cos", "scalar_kind": "f32"}`)
	t.Cleanup(func() { rawRun(t, uri, "DROP VECTOR INDEX "+index) })

	vector := productTextWrite{"vector", `
CREATE (v:TextVec {id: $prefix + 'vector', vec_id: $prefix + 'vec-id', space: $prefix})
SET v.embedding = $embedding
RETURN count(v) AS matched`, 1}
	importProducts := productTextWrite{"import", `
UNWIND ['text', 'other-text'] AS suffix
MATCH (n:ProductText {id: $prefix + suffix})
CREATE (:Product {id: $prefix + suffix + '-product', store: $prefix})-[r:HAS_TEXT]->(n)
RETURN count(r) AS matched`, 2}
	push := productTextWrite{"push", `
MERGE (p:Product {id: $prefix + 'existing-product'})
SET p.store = $prefix, p.title = $prefix + 'updated'
MERGE (n:ProductText {id: $prefix + 'text'})
ON CREATE SET n.store = $prefix, n.vec_id = $prefix + 'vec-id', n.text = $prefix + 'material: cotton'
MERGE (p)-[r:HAS_TEXT]->(n)
RETURN count(r) AS matched`, 1}
	for _, pair := range [][2]productTextWrite{{importProducts, vector}, {vector, importProducts}, {push, vector}, {vector, push}} {
		t.Run("split_"+pair[0].name+"_then_"+pair[1].name, func(t *testing.T) {
			params := seedProductTextConflict(t, uri)
			if err := runProductTextWritePair(t, drv, params, pair[0], pair[1]); err != nil {
				t.Fatalf("split writes must both commit: %v", err)
			}
			t.Log("both writes matched and committed")
		})
	}
}

type productTextWrite struct {
	name, query string
	matched     int64
}

func seedProductTextConflict(t *testing.T, uri string) map[string]any {
	t.Helper()
	prefix := fmt.Sprintf("t1a-%d-", time.Now().UnixNano())
	t.Cleanup(func() {
		rawRun(t, uri, fmt.Sprintf(`MATCH (n)
WHERE (n:Product OR n:ProductText OR n:TextVec) AND n.id STARTS WITH '%s'
DETACH DELETE n`, prefix))
	})
	rawRun(t, uri, fmt.Sprintf(`CREATE
(:ProductText {id: '%[1]stext', store: '%[1]s', vec_id: '%[1]svec-id', text: '%[1]smaterial: cotton'}),
(:ProductText {id: '%[1]sother-text', store: '%[1]s', vec_id: '%[1]sother-vec-id', text: '%[1]smaterial: linen'}),
(:Product {id: '%[1]sexisting-product', store: '%[1]s', title: '%[1]soriginal'})`, prefix))
	return map[string]any{"prefix": prefix, "embedding": graphtest.Vector(prefix, 768)}
}

func runProductTextWritePair(t *testing.T, drv neo4j.Driver, params map[string]any, first, second productTextWrite) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	begin := func() neo4j.ExplicitTransaction {
		sess := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
		t.Cleanup(func() { _ = sess.Close(context.Background()) })
		tx, err := sess.BeginTransaction(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Close(context.Background()) })
		return tx
	}
	firstTx, secondTx := begin(), begin()

	if err := first.run(ctx, firstTx, params); err != nil {
		t.Fatalf("first write %s: %v", first.name, err)
	}
	// Pulling the first write's result is the barrier. Keep that transaction
	// open while the second session runs its write and attempts its commit.
	err := second.run(ctx, secondTx, params)
	if err == nil {
		if commitErr := secondTx.Commit(ctx); commitErr != nil {
			err = fmt.Errorf("commit %s: %w", second.name, commitErr)
		}
	}
	if commitErr := firstTx.Commit(ctx); commitErr != nil {
		t.Fatalf("commit first write %s: %v (second write: %v)", first.name, commitErr, err)
	}
	return err
}

func (w productTextWrite) run(ctx context.Context, tx neo4j.ExplicitTransaction, params map[string]any) error {
	result, err := tx.Run(ctx, w.query, params)
	if err != nil {
		return fmt.Errorf("run %s: %w", w.name, err)
	}
	record, err := result.Single(ctx)
	if err != nil {
		return fmt.Errorf("pull %s: %w", w.name, err)
	}
	if got, _ := record.Get("matched"); got != w.matched {
		return fmt.Errorf("%s matched %v, want %d", w.name, got, w.matched)
	}
	return nil
}
