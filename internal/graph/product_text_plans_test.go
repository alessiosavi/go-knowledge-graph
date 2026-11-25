package graph

import (
	"fmt"
	"strings"
	"testing"
)

// TestProductTextQueryPlansAnchorOnText guards against plans that scan every
// node once per ProductText, which made imports and pushes grow with the
// whole graph (seen 2026-11-25: 102,688,144 node visits for 1,683 texts).
func TestProductTextQueryPlansAnchorOnText(t *testing.T) {
	f := newTextReadFixture(t, "t-plan.test", "t-plan")
	for name, q := range map[string]struct {
		query  string
		params map[string]any
	}{
		"snapshot orphans": {deleteOrphanedStoreTextsQuery, map[string]any{"store": "t-plan.test"}},
		"push orphans":     {deleteOrphanedPushedTextsQuery, map[string]any{"ids": []string{"x"}}},
		"coverage probe": {missingProductTextVectorsQuery, map[string]any{
			"stores": []string{"t-plan.test"}, "domain": "t-plan", "space": ProductTextSpace}},
	} {
		anchored := false
		for _, row := range f.query("EXPLAIN "+q.query, q.params) {
			line := fmt.Sprint(row...)
			switch {
			case strings.Contains(line, "Expand (n)<-"):
				anchored = true
			case strings.Contains(line, "ScanAll") && !strings.Contains(line, "ProductText") && !strings.Contains(line, "TextVec"):
				t.Errorf("%s: plan scans unanchored nodes: %s", name, line)
			}
		}
		if !anchored {
			t.Errorf("%s: plan does not expand from the ProductText", name)
		}
	}
}
