package graphtest

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// GraphSnapshot captures the graph data and schema used by read-only assertions.
type GraphSnapshot struct {
	nodes, relationships []*neo4j.Record
	indexes, constraints []string
}

// Snapshot reads from the integration database without bootstrapping its schema.
func Snapshot(t testing.TB) GraphSnapshot {
	t.Helper()
	uri := os.Getenv(URIEnv)
	if uri == "" {
		t.Skipf("set %s to run integration tests", URIEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	driver, err := neo4j.NewDriver(uri, neo4j.NoAuth())
	if err != nil {
		t.Fatalf("open snapshot driver: %v", err)
	}
	defer driver.Close(ctx)
	session := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)

	read := func(query string) []*neo4j.Record {
		t.Helper()
		result, err := session.Run(ctx, query, nil)
		if err != nil {
			t.Fatalf("snapshot %s: %v", query, err)
		}
		rows, err := result.Collect(ctx)
		if err != nil {
			t.Fatalf("snapshot %s: %v", query, err)
		}
		return rows
	}

	return GraphSnapshot{
		nodes:         read(`MATCH (n) RETURN id(n), labels(n), properties(n) ORDER BY id(n)`),
		relationships: read(`MATCH (a)-[r]->(b) RETURN id(r), id(a), type(r), properties(r), id(b) ORDER BY id(r)`),
		// Storage GC can change the reported index counts without a write.
		indexes:     normalizeSchemaRows(t, read("SHOW INDEX INFO"), "count"),
		constraints: normalizeSchemaRows(t, read("SHOW CONSTRAINT INFO"), ""),
	}
}

// AssertUnchanged compares a fresh snapshot and reports each changed component.
func (before GraphSnapshot) AssertUnchanged(t testing.TB, operation string) {
	t.Helper()
	before.assertEqual(t, Snapshot(t), operation)
}

// AssertJudgeLeaseFenceAdvanced checks that advancing the fence was the only
// lasting change from an operation that acquired and released a fenced lease.
func (before GraphSnapshot) AssertJudgeLeaseFenceAdvanced(t testing.TB, operation string, advances int64) {
	t.Helper()
	seq := func(s *GraphSnapshot) int64 {
		t.Helper()
		for i, row := range s.nodes {
			props, _ := row.Values[2].(map[string]any)
			if props["id"] != "judge_lease_fence" || !reflect.DeepEqual(row.Values[1], []any{"GkgMeta"}) {
				continue
			}
			n, ok := props["seq"].(int64)
			if !ok || len(props) != 2 {
				t.Fatalf("%s left unexpected judge lease fence properties: %v", operation, props)
			}
			s.nodes = slices.Delete(slices.Clone(s.nodes), i, i+1)
			return n
		}
		return 0
	}
	after := Snapshot(t)
	if initial, final := seq(&before), seq(&after); final != initial+advances {
		t.Errorf("%s changed judge lease fence sequence from %d to %d, want +%d", operation, initial, final, advances)
	}
	before.assertEqual(t, after, operation)
}

func (before GraphSnapshot) assertEqual(t testing.TB, after GraphSnapshot, operation string) {
	t.Helper()
	for _, part := range []struct {
		name          string
		before, after any
	}{
		{"nodes", before.nodes, after.nodes},
		{"relationships", before.relationships, after.relationships},
		{"SHOW INDEX INFO", before.indexes, after.indexes},
		{"SHOW CONSTRAINT INFO", before.constraints, after.constraints},
	} {
		if !reflect.DeepEqual(part.before, part.after) {
			t.Errorf("%s changed graph (%s)", operation, part.name)
		}
	}
}

func normalizeSchemaRows(t testing.TB, rows []*neo4j.Record, omitColumn string) []string {
	t.Helper()
	normalized := make([]string, 0, len(rows))
	for _, row := range rows {
		fields := row.AsMap()
		if omitColumn != "" {
			delete(fields, omitColumn)
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("encode schema row: %v", err)
		}
		normalized = append(normalized, string(encoded))
	}
	slices.Sort(normalized)
	return normalized
}
