package mcp

import (
	"context"
	"fmt"
	"slices"
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
						want := 2
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

func TestConfirmedOnlyReadsMCP(t *testing.T) {
	f := graphtest.MixedVisibility(t)
	cfg := config.Config{}
	cfg.Facts.AllowUnverifiedReads = true
	cs := connectInMemory(t, cfg, f.Store, nil)
	for _, tc := range []struct {
		tool, arg, field string
	}{
		{"get_entity", "id", "facts"},
		{"traverse", "entity", "edges"},
	} {
		for _, confirmedOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/confirmed_only=%v", tc.tool, confirmedOnly), func(t *testing.T) {
				args := map[string]any{tc.arg: f.ID}
				want := []string{"advises", "consults", "uses", "works_at"}
				if confirmedOnly {
					args["confirmed_only"] = true
					args["include_unverified"] = true
					want = []string{"advises", "works_at"}
				}
				out := structured[map[string]any](t, callTool(t, cs, tc.tool, args))
				var rels []string
				for _, raw := range out[tc.field].([]any) {
					row := raw.(map[string]any)
					rel := row["rel"].(string)
					if rel == "advises" && row["judge"] != "confirmed" {
						t.Errorf("advises judge=%v, want confirmed", row["judge"])
					}
					rels = append(rels, rel)
				}
				slices.Sort(rels)
				if !slices.Equal(rels, want) {
					t.Errorf("%s relations=%v, want %v", tc.field, rels, want)
				}
			})
		}
	}
}

func TestConfirmedOnlyMCPSchema(t *testing.T) {
	cs := connectInMemory(t, config.Config{}, nil, nil)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "get_entity" && tool.Name != "traverse" {
			continue
		}
		schema := tool.InputSchema.(map[string]any)
		properties := schema["properties"].(map[string]any)
		field, ok := properties["confirmed_only"].(map[string]any)
		if !ok || field["type"] != "boolean" || field["description"] != "return only facts the judge confirmed. Overrides include_low_confidence and include_unverified" {
			t.Errorf("%s confirmed_only schema=%v", tool.Name, field)
		}
		field, ok = properties["include_low_confidence"].(map[string]any)
		if !ok || field["description"] != "include all typed facts below the assert floor. Judge-confirmed facts are always shown regardless of this flag" {
			t.Errorf("%s include_low_confidence schema=%v", tool.Name, field)
		}
	}
}
