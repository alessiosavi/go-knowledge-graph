package judge

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestPlanRejudgeAuditInputErrors(t *testing.T) {
	missingID := graph.FactAuditID("missing-head", "uses", "missing-tail", "missing-chunk")
	for _, tc := range []struct {
		name       string
		labels     []AuditRow
		detail     string
		needsStore bool
	}{
		{"invalid label", []AuditRow{{ID: missingID, Label: "maybe"}}, "invalid label", false},
		{"empty id", []AuditRow{{Label: "ok"}}, "empty labelled id", false},
		{"duplicate id", []AuditRow{{ID: missingID, Label: "ok"}, {ID: missingID, Label: "ok"}}, "duplicate labelled id", false},
		{"missing eligible edge", []AuditRow{{ID: missingID, Label: "ok"}}, "missing eligible typed edge", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var st *graph.Store
			if tc.needsStore {
				st = graphtest.Store(t)
			}
			path := filepath.Join(t.TempDir(), "manifest.jsonl")
			_, err := PlanRejudge(t.Context(), st, "fake-judge", "", path, RejudgeSelection{Domain: t.Name(), Labels: tc.labels})
			if !errors.Is(err, ErrRejudgeAuditInput) || !strings.Contains(err.Error(), tc.detail) {
				t.Errorf("PlanRejudge error = %v, want ErrRejudgeAuditInput for %s", err, tc.detail)
			}
		})
	}
}
