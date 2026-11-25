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

func TestTextVecGC(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	ctx := t.Context()
	now := time.Now().Truncate(time.Millisecond)
	if err := st.EnsureTextVecIndex(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rawRun(t, uri, "DROP VECTOR INDEX text_vec") })
	rawRun(t, uri, `CREATE (v:TextVec {id: 't2-gc', vec_id: 't2-gc'})
SET v.embedding = [x IN range(1, 768) | 1.0]
CREATE (:TextVecFailure {id: 't2-gc'})`)
	t.Cleanup(func() {
		rawRun(t, uri, `MATCH (n) WHERE n.id IN ['t2-gc', 't2-gc-neighbor'] SET n.embedding = null DETACH DELETE n`)
	})
	collect := func(at time.Time) {
		t.Helper()
		if err := st.GCTextVecs(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	orphanSince := func() any {
		t.Helper()
		rows := rawRecords(t, uri, `MATCH (v:TextVec {id:'t2-gc'}) RETURN v.orphan_since AS orphan_since`)
		if len(rows) != 1 {
			t.Fatalf("expected one vector, got %d", len(rows))
		}
		got, _ := rows[0].Get("orphan_since")
		return got
	}

	collect(now)
	if got := orphanSince(); got != now.UnixMilli() {
		t.Fatalf("first orphan time = %v, want %d", got, now.UnixMilli())
	}
	collect(now.Add(24 * time.Hour))
	if got := orphanSince(); got != now.UnixMilli() {
		t.Fatalf("second pass reset orphan time to %v", got)
	}
	if got := rawCount(t, uri, `MATCH (:TextVecFailure {id:'t2-gc'}) RETURN count(*) AS n`); got != 0 {
		t.Fatalf("unreferenced failure count = %d", got)
	}

	rawRun(t, uri, `CREATE (:ProductText {id:'t2-gc', vec_id:'t2-gc', store:'t2-gc', text:'t2-gc'})`)
	collect(now.Add(8 * 24 * time.Hour))
	if got := orphanSince(); got != nil {
		t.Fatalf("re-reference left orphan time %v", got)
	}
	rawRun(t, uri, `MATCH (p:ProductText {id:'t2-gc'}) DELETE p`)
	collect(now)
	collect(now.Add(7 * 24 * time.Hour))
	if got := orphanSince(); got != now.UnixMilli() {
		t.Errorf("vector changed at the grace boundary: %v", got)
	}
	collect(now.Add(8 * 24 * time.Hour))
	if got := rawCount(t, uri, `MATCH (:TextVec {id:'t2-gc'}) RETURN count(*) AS n`); got != 0 {
		t.Fatalf("old orphan count = %d", got)
	}
	rawRun(t, uri, `CREATE (:TextVec {id:'t2-gc-neighbor', vec_id:'t2-gc-neighbor', embedding:[x IN range(1, 768) | 1.0]})`)
	// Leave room in the search results for a stale hit alongside the live vectors.
	rows := rawRecords(t, uri, `
MATCH (v:TextVec) WHERE v.embedding IS NOT NULL
WITH count(v) + 1 AS k, [x IN range(1, 768) | 1.0] AS embedding
CALL vector_search.search('text_vec', k, embedding) YIELD node
WITH node WHERE node.id = 't2-gc'
RETURN node.id AS id`)
	if len(rows) != 0 {
		t.Fatalf("deleted vector remained searchable: %v", rows)
	}
	rawRun(t, uri, `CREATE (:ProductText {id:'t2-gc', vec_id:'t2-gc', store:'t2-gc', text:'t2-gc'})`)
	pending, err := st.PendingTextVecs(ctx, graph.ProductTextSpace, now, 256)
	if err != nil || len(pending) != 1 || pending[0].VecID != "t2-gc" {
		t.Fatalf("text after vector deletion = %+v, err=%v", pending, err)
	}
}

func TestTextVecGCBounded(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	rawRun(t, uri, `
UNWIND range(1, 1001) AS i
CREATE (:TextVec {id:'t2-gc-bounded-' + toString(i), vec_id:'t2-gc-bounded-' + toString(i)})`)
	t.Cleanup(func() {
		rawRun(t, uri, `MATCH (v:TextVec) WHERE v.id STARTS WITH 't2-gc-bounded-' DELETE v`)
	})
	now := time.Now()
	for _, pass := range []struct {
		at   time.Time
		want int64
	}{
		{now, 1000},
		{now, 1001},
		{now.Add(8 * 24 * time.Hour), 1},
		{now.Add(8 * 24 * time.Hour), 0},
	} {
		if err := st.GCTextVecs(t.Context(), pass.at); err != nil {
			t.Fatal(err)
		}
		if got := rawCount(t, uri, `MATCH (v:TextVec)
WHERE v.id STARTS WITH 't2-gc-bounded-' AND v.orphan_since IS NOT NULL RETURN count(*) AS n`); got != pass.want {
			t.Fatalf("GC at %s: marked count=%d, want %d", pass.at, got, pass.want)
		}
	}
}

func TestTextVecGCRacesReReference(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	rawRun(t, uri, `CREATE (:TextVec {id:'t2-gc-race', vec_id:'t2-gc-race', orphan_since:0})`)
	t.Cleanup(func() {
		rawRun(t, uri, `MATCH (n {id:'t2-gc-race'}) DETACH DELETE n`)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	driver, err := neo4j.NewDriver(uri, neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close(context.Background())
	sess := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx)
	tx, err := sess.BeginTransaction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close(ctx)
	ref, err := tx.Run(ctx, `CREATE (:ProductText {id:'t2-gc-race', vec_id:'t2-gc-race', text:'t2-gc-race'})`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ref.Consume(ctx); err != nil {
		t.Fatal(err)
	}
	// GC can delete the old vector while the new reference is uncommitted.
	// Committing the reference must still succeed and make the text pending.
	now := time.Now()
	if err := st.GCTextVecs(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := st.PendingTextVecs(ctx, graph.ProductTextSpace, now, 256)
	if err != nil || len(pending) != 1 || pending[0].VecID != "t2-gc-race" {
		t.Fatalf("concurrent reference = %+v, err=%v", pending, err)
	}
}

func TestTextVecIndexMismatch(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	for _, tc := range []struct {
		name, metric, scalar string
		dimension            int
	}{
		{"dimension", "cos", "f32", 8},
		{"metric", "l2sq", "f32", 768},
		{"scalar", "cos", "f16", 768},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rawRun(t, uri, fmt.Sprintf(`CREATE VECTOR INDEX text_vec ON :TextVec(embedding)
WITH CONFIG {"dimension": %d, "capacity": 32768, "metric": %q, "scalar_kind": %q}`, tc.dimension, tc.metric, tc.scalar))
			t.Cleanup(func() { rawRun(t, uri, "DROP VECTOR INDEX text_vec") })
			if err := st.EnsureTextVecIndex(t.Context()); err == nil {
				t.Fatal("mismatched index accepted")
			}
			for _, row := range rawRecords(t, uri, "SHOW VECTOR INDEX INFO") {
				name, _ := row.Get("index_name")
				if name != "text_vec" {
					continue
				}
				dim, _ := row.Get("dimension")
				metric, _ := row.Get("metric")
				scalar, _ := row.Get("scalar_kind")
				if dim != int64(tc.dimension) || metric != tc.metric || scalar != tc.scalar {
					t.Fatalf("mismatched index changed: %v", row)
				}
				return
			}
			t.Fatal("mismatched index disappeared")
		})
	}
}
