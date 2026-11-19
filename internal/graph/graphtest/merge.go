package graphtest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// SeedMergedEntity inserts an isolated ledger row and removes it on cleanup.
// As required of the merge writer (T4), every row carries both old_id and old_key.
func SeedMergedEntity(t testing.TB, oldID, oldKey, survivorID string) {
	t.Helper()
	run := lookupFixtureQuery(t)
	params := map[string]any{"id": oldID, "key": oldKey, "survivor": survivorID}
	run(`CREATE (:MergedEntity {old_id: $id, old_key: $key, survivor_id: $survivor})`, params)
	t.Cleanup(func() { run(`MATCH (m:MergedEntity {old_id: $id, old_key: $key}) DELETE m`, params) })
}

// SeedFoldScopes enables hint writes for a name lookup fixture. Use only in
// serial integration tests because this temporarily replaces the fold record.
func SeedFoldScopes(t testing.TB, scopes ...string) {
	t.Helper()
	run := lookupFixtureQuery(t)
	before := run(`MATCH (m:GkgMeta {id:'fold'}) RETURN properties(m) AS props`, nil)
	run(`MERGE (m:GkgMeta {id:'fold'})
SET m.fold_version = $version, m.scopes = $scopes, m.exceptions_keys = []`,
		map[string]any{"version": graph.FoldKeyVersion, "scopes": scopes})
	t.Cleanup(func() {
		if len(before) == 0 {
			run(`MATCH (m:GkgMeta {id:'fold'}) DELETE m`, nil)
			return
		}
		props, _ := before[0].Get("props")
		run(`MATCH (m:GkgMeta {id:'fold'}) SET m = $props`, map[string]any{"props": props})
	})
}

func lookupFixtureQuery(t testing.TB) func(string, map[string]any) []*neo4j.Record {
	t.Helper()
	uri := os.Getenv(URIEnv)
	if uri == "" {
		t.Skipf("set %s to run integration tests", URIEnv)
	}
	driver, err := neo4j.NewDriver(uri, neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })
	return func(query string, params map[string]any) []*neo4j.Record {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		session := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
		defer session.Close(ctx)
		result, err := session.Run(ctx, query, params)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := result.Collect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
}
