package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestAgentVisibilityKeepsLowConfidenceOptIn(t *testing.T) {
	f := graphtest.Visibility(t)
	cfg := config.Config{}
	cfg.Facts.AllowUnverifiedReads = true // Agent tools must still keep candidates hidden.
	s := NewServer(cfg, f.Store, nil)
	for _, low := range []bool{false, true} {
		for _, name := range []string{"get_entity", "traverse"} {
			t.Run(fmt.Sprintf("%s/low=%v", name, low), func(t *testing.T) {
				args := fmt.Sprintf(`{"id":%q,"entity":%q,"direction":"both","include_low_confidence":%v,"include_unverified":true}`, f.ID, f.ID, low)
				res, err := s.execTool(context.Background(), name, json.RawMessage(args))
				if err != nil {
					t.Fatal(err)
				}
				out := unmarshalResult[map[string]any](t, res)
				hidden, ok := out["hidden"].(map[string]any)
				if !ok || hidden["unclear"] != float64(1) || hidden["unjudged"] != float64(2) || hidden["rejected"] != float64(1) {
					t.Errorf("hidden=%v", out["hidden"])
				}
				field := "facts"
				if name == "traverse" {
					field = "edges"
				}
				rows := out[field].([]any)
				want := 1
				if low {
					want++
				}
				if len(rows) != want {
					t.Fatalf("%s=%v, want %d", field, rows, want)
				}
				for _, row := range rows {
					r := row.(map[string]any)
					if r["judge"] != "confirmed" || r["unverified"] == true {
						t.Errorf("unexpected fact=%v", r)
					}
				}
			})
		}
	}
}
