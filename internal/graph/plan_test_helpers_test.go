package graph_test

import (
	"context"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// planOperators extracts operator names from Memgraph's EXPLAIN rows.
func planOperators(t *testing.T, st *graph.Store, query string) []string {
	t.Helper()
	plan, err := st.RunReadQuery(context.Background(), "EXPLAIN "+query, map[string]any{"q": "zorbo"}, 0)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var operators []string
	for _, row := range plan.Rows {
		line, ok := row["QUERY PLAN"].(string)
		if !ok {
			t.Fatalf("EXPLAIN row has no string QUERY PLAN: %+v", row)
		}
		_, operator, ok := strings.Cut(line, " * ")
		if !ok || len(strings.Fields(operator)) == 0 {
			t.Fatalf("unrecognized EXPLAIN operator: %q", line)
		}
		operators = append(operators, strings.Fields(operator)[0])
	}
	if len(operators) == 0 {
		t.Fatal("EXPLAIN returned no operators")
	}
	return operators
}
