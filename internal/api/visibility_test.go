package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

// Legacy typed facts emit an empty domain when absent, while omitting an empty
// judge status and zero visibility fields.
func TestLegacyReadJSONUnchanged(t *testing.T) {
	v := graph.EntityView{EntityRef: graph.EntityRef{ID: "x", Key: "zorbo", Name: "Zorbo", Type: "person"},
		Facts: []graph.FactView{{Rel: "uses", Other: graph.EntityRef{ID: "y", Key: "acme", Name: "Acme-Max", Type: "product"}, Confidence: .9, Outgoing: true, DocID: "d", ChunkID: "c", Excerpt: "Zorbo uses Acme-Max."}}}
	sub := graph.Subgraph{Nodes: []graph.EntityRef{v.EntityRef}, Edges: []graph.PathEdge{{From: "zorbo", To: "acme", Rel: "uses", Confidence: .9, DocID: "d", ChunkID: "c", Excerpt: "Zorbo uses Acme-Max."}}}
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{"entity", toEntityDTO(v), `{"id":"x","key":"zorbo","name":"Zorbo","type":"person","domain":"","aliases":[],"mention_count":0,"facts":[{"rel":"uses","other":{"id":"y","key":"acme","name":"Acme-Max","type":"product","domain":""},"confidence":0.9,"low_confidence":false,"outgoing":true,"doc_id":"d","chunk_id":"c","excerpt":"Zorbo uses Acme-Max."}],"together":[]}`},
		{"traverse", toTraverseResponse(sub), `{"nodes":[{"id":"x","key":"zorbo","name":"Zorbo","type":"person","domain":""}],"edges":[{"from":"zorbo","to":"acme","rel":"uses","confidence":0.9,"low_confidence":false,"doc_id":"d","chunk_id":"c","excerpt":"Zorbo uses Acme-Max."}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.want {
				t.Errorf("wire=%s, want %s", raw, tc.want)
			}
		})
	}
}

func TestUnverifiedReadsREST(t *testing.T) {
	f := graphtest.Visibility(t)
	for _, allow := range []bool{false, true} {
		cfg := config.Config{}
		cfg.Facts.AllowUnverifiedReads = allow
		h := apiHandler(t, cfg, f.Store, nil)
		for _, include := range []bool{false, true} {
			for _, low := range []bool{false, true} {
				for _, traverse := range []bool{false, true} {
					t.Run(fmt.Sprintf("allow=%v/include=%v/low=%v/traverse=%v", allow, include, low, traverse), func(t *testing.T) {
						path := fmt.Sprintf("/v1/entities/%s?include_unverified=%v&include_low_confidence=%v", f.ID, include, low)
						rr := do(t, h, http.MethodGet, path, nil, nil)
						field := "facts"
						if traverse {
							rr = doJSON(t, h, http.MethodPost, "/v1/traverse", fmt.Sprintf(`{"entity":%q,"direction":"both","include_unverified":%v,"include_low_confidence":%v}`, f.ID, include, low))
							field = "edges"
						}
						if rr.Code != 200 {
							t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
						}
						out := decode[map[string]any](t, rr)
						hidden, ok := out["hidden"].(map[string]any)
						if !ok || hidden["unclear"] != float64(1) || hidden["unjudged"] != float64(2) || hidden["rejected"] != float64(1) {
							t.Errorf("hidden=%v", out["hidden"])
						}
						if got, _ := out["unverified_ignored"].(bool); got != (include && !allow) {
							t.Errorf("ignored=%v, want %v", out["unverified_ignored"], include && !allow)
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
						unverified, unclear := 0, 0
						for _, row := range rows {
							v := row.(map[string]any)
							if v["unverified"] == true {
								unverified++
								if v["judge"] == "unclear" {
									unclear++
								}
								if _, exists := v["judge"]; !exists {
									t.Error("candidate missing judge state")
								}
							}
						}
						if allow && include && (unverified != 3 || unclear != 1) {
							t.Errorf("unverified=%d unclear=%d", unverified, unclear)
						}
					})
				}
			}
		}
	}
}

func TestConfirmedOnlyReadsREST(t *testing.T) {
	f := graphtest.MixedVisibility(t)
	cfg := config.Config{}
	cfg.Facts.AllowUnverifiedReads = true
	h := apiHandler(t, cfg, f.Store, nil)
	for _, tc := range []struct {
		name, query, bodyFields string
		want                    []string
	}{
		{"default", "", "", []string{"advises", "consults", "uses", "works_at"}},
		{"confirmed_only", "?confirmed_only=true&include_unverified=true", `,"confirmed_only":true,"include_unverified":true`, []string{"advises", "works_at"}},
	} {
		for _, field := range []string{"facts", "edges"} {
			t.Run(tc.name+"/"+field, func(t *testing.T) {
				var rr *httptest.ResponseRecorder
				if field == "edges" {
					rr = doJSON(t, h, http.MethodPost, "/v1/traverse", fmt.Sprintf(`{"entity":%q%s}`, f.ID, tc.bodyFields))
				} else {
					rr = do(t, h, http.MethodGet, "/v1/entities/"+f.ID+tc.query, nil, nil)
				}
				if rr.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
				}
				out := decode[map[string]json.RawMessage](t, rr)
				var rows []struct{ Rel, Judge string }
				if err := json.Unmarshal(out[field], &rows); err != nil {
					t.Fatal(err)
				}
				var rels []string
				for _, row := range rows {
					if row.Rel == "advises" && row.Judge != "confirmed" {
						t.Errorf("advises judge=%q, want confirmed", row.Judge)
					}
					rels = append(rels, row.Rel)
				}
				slices.Sort(rels)
				if !slices.Equal(rels, tc.want) {
					t.Errorf("%s relations=%v, want %v", field, rels, tc.want)
				}
			})
		}
	}
}
