package mcp

import (
	"fmt"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestUnverifiedReadsMCP(t *testing.T) {
	f := graphtest.Visibility(t)
	for _, allow := range []bool{false, true} {
		cfg := config.Config{}
		cfg.Facts.AllowUnverifiedReads = allow
		cs := connectInMemory(t, cfg, f.Store, nil)
		for _, include := range []bool{false, true} {
			for _, low := range []bool{false, true} {
				for _, name := range []string{"get_entity", "traverse"} {
					t.Run(fmt.Sprintf("%s/allow=%v/include=%v/low=%v", name, allow, include, low), func(t *testing.T) {
						args := map[string]any{"include_unverified": include, "include_low_confidence": low}
						field := "facts"
						if name == "get_entity" {
							args["id"] = f.ID
						} else {
							args["entity"] = f.ID
							args["direction"] = "both"
							field = "edges"
						}
						out := structured[map[string]any](t, callTool(t, cs, name, args))
						hidden, ok := out["hidden"].(map[string]any)
						if !ok || hidden["unclear"] != float64(1) || hidden["unjudged"] != float64(2) || hidden["rejected"] != float64(1) {
							t.Errorf("hidden=%v", out["hidden"])
						}
						if got, _ := out["unverified_ignored"].(bool); got != (include && !allow) {
							t.Errorf("ignored=%v", out["unverified_ignored"])
						}
						want := 1
						if low {
							want++
						}
						if allow && include {
							want += 3
						}
						rows := out[field].([]any)
						if len(rows) != want {
							t.Fatalf("%s=%v, want %d", field, rows, want)
						}
						n := 0
						for _, row := range rows {
							r := row.(map[string]any)
							if r["unverified"] == true {
								n++
								if _, exists := r["judge"]; !exists {
									t.Error("candidate missing judge")
								}
							}
						}
						if allow && include && n != 3 {
							t.Errorf("unverified=%d", n)
						}
					})
				}
			}
		}
	}
}
