package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestAgentMergedSelectorProvenance(t *testing.T) {
	f := graphtest.Visibility(t)
	oldID, oldKey := graph.EntityID("retired-"+f.Key), "retired-"+f.Key
	graphtest.SeedMergedEntity(t, oldID, oldKey, f.ID)
	s := NewServer(config.Config{}, f.Store, nil)
	for _, tc := range []struct{ tool, field string }{{"get_entity", "id"}, {"traverse", "entity"}} {
		t.Run(tc.tool, func(t *testing.T) {
			var exact map[string]any
			for _, sel := range []string{f.ID, f.Key, oldID, oldKey} {
				args := json.RawMessage(fmt.Sprintf(`{%q:%q}`, tc.field, sel))
				res, err := s.execTool(t.Context(), tc.tool, args)
				if err != nil {
					t.Fatal(err)
				}
				out := unmarshalResult[map[string]any](t, res)
				requested, hasRequested := out["requested"]
				via, hasVia := out["resolved_via"]
				if sel == oldID || sel == oldKey {
					if requested != sel || via != "merged" {
						t.Errorf("selector %q: provenance %v/%v", sel, requested, via)
					}
				} else if hasRequested || hasVia {
					t.Errorf("exact selector %q carried provenance: %v", sel, out)
				}
				delete(out, "requested")
				delete(out, "resolved_via")
				if exact == nil {
					exact = out
				} else if !reflect.DeepEqual(out, exact) {
					t.Errorf("selector %q: payload differs from live ID", sel)
				}
			}
		})
	}
}

type agentNameLookupStore struct {
	graphStore
	candidate graph.EntityCandidate
}

func (s agentNameLookupStore) EntityCandidatesByName(context.Context, string, string) ([]graph.EntityCandidate, error) {
	return []graph.EntityCandidate{s.candidate}, nil
}

func (s agentNameLookupStore) EntityViewOpts(_ context.Context, id string, _ graph.ViewOptions) (graph.EntityView, error) {
	if id != s.candidate.ID {
		return graph.EntityView{}, graph.ErrNotFound
	}
	return graph.EntityView{EntityRef: s.candidate.EntityRef}, nil
}

func TestAgentNameMatchThroughIDHandoff(t *testing.T) {
	s := NewServer(config.Config{}, nil, nil)
	for _, tier := range []string{graph.EntityMatchExact, graph.EntityMatchFold} {
		t.Run(tier, func(t *testing.T) {
			s.store = agentNameLookupStore{candidate: graph.EntityCandidate{
				EntityRef: graph.EntityRef{ID: "zelvix-id", Key: "zel-vix", Name: "Zel-vix"}, Match: tier,
			}}
			out, err := s.execTool(t.Context(), "get_entity", json.RawMessage(`{"name":"Zelvix"}`))
			if err != nil {
				t.Fatal(err)
			}
			fields := unmarshalResult[map[string]any](t, out)
			match, ok := fields["match"]
			if fields["id"] != "zelvix-id" || ok != (tier == graph.EntityMatchFold) || (ok && match != tier) {
				t.Errorf("name lookup output = %+v", fields)
			}
		})
	}
}

func TestAgentVisibilityIncludesConfirmedByDefault(t *testing.T) {
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
				want := 2
				if low {
					want++
				}
				if len(rows) != want {
					t.Fatalf("%s=%v, want %d", field, rows, want)
				}
				for _, row := range rows {
					r := row.(map[string]any)
					wantJudge := "confirmed"
					if low && r["rel"] == "evaluates" {
						wantJudge = "unclear"
					}
					if r["judge"] != wantJudge || r["unverified"] == true {
						t.Errorf("unexpected fact=%v", r)
					}
				}
			})
		}
	}
}
