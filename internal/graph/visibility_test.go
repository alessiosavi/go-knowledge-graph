package graph_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestEntityViewVisibilityOptions(t *testing.T) {
	f := graphtest.Visibility(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		opt   graph.ViewOptions
		count int
	}{
		{"default", graph.ViewOptions{}, 2},
		{"low", graph.ViewOptions{IncludeLowConfidence: true}, 3},
		{"unverified", graph.ViewOptions{IncludeUnverified: true}, 5},
		{"both", graph.ViewOptions{IncludeLowConfidence: true, IncludeUnverified: true}, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := f.Store.EntityViewOpts(ctx, f.ID, tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			if v.Hidden != (graph.HiddenCounts{Unclear: 1, Unjudged: 2, Rejected: 1}) {
				t.Errorf("hidden=%+v", v.Hidden)
			}
			if len(v.Facts) != tc.count {
				t.Fatalf("facts=%+v, want %d", v.Facts, tc.count)
			}
			states := map[string]int{}
			for _, fact := range v.Facts {
				if fact.Unverified {
					states[fact.Judge]++
					if fact.DocID != f.DocID || fact.ChunkID != f.ChunkID || fact.Excerpt == "" {
						t.Errorf("candidate provenance=%+v", fact)
					}
					if fact.Outgoing != (fact.Rel != "pending_in") {
						t.Errorf("candidate direction=%+v", fact)
					}
				} else if fact.Judge != "confirmed" && (fact.Judge != "unclear" || !fact.LowConfidence || !tc.opt.IncludeLowConfidence) {
					t.Errorf("typed fact=%+v", fact)
				}
			}
			if tc.opt.IncludeUnverified && !reflect.DeepEqual(states, map[string]int{"": 2, "unclear": 1}) {
				t.Errorf("states=%v", states)
			}
			if !tc.opt.IncludeUnverified {
				old, err := f.Store.EntityView(ctx, f.ID, tc.opt.IncludeLowConfidence)
				if err != nil {
					t.Fatal(err)
				}
				a, _ := json.Marshal(old)
				b, _ := json.Marshal(v)
				if string(a) != string(b) {
					t.Errorf("wrapper differs: %s / %s", a, b)
				}
			}
		})
	}
	if _, err := f.Store.EntityViewOpts(ctx, "missing-visibility", graph.ViewOptions{IncludeUnverified: true}); !errors.Is(err, graph.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestTraverseVisibilityOptions(t *testing.T) {
	f := graphtest.Visibility(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, direction, rel            string
		depth, limit, edges, candidates int
		opt                             graph.ViewOptions
	}{
		{"default", "both", "", 1, 50, 2, 0, graph.ViewOptions{}},
		{"low", "both", "", 1, 50, 3, 0, graph.ViewOptions{IncludeLowConfidence: true}},
		{"unverified", "both", "", 1, 50, 5, 3, graph.ViewOptions{IncludeUnverified: true}},
		{"both", "both", "", 1, 50, 6, 3, graph.ViewOptions{IncludeLowConfidence: true, IncludeUnverified: true}},
		{"incoming", "in", "", 1, 50, 1, 1, graph.ViewOptions{IncludeUnverified: true}},
		{"relation", "out", "pending out", 1, 50, 1, 1, graph.ViewOptions{IncludeUnverified: true}},
		{"limit", "both", "", 2, 1, 1, 0, graph.ViewOptions{IncludeUnverified: true}},
		{"two_hops", "both", "", 2, 50, 5, 3, graph.ViewOptions{IncludeUnverified: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := f.Store.TraverseOpts(ctx, f.Key, tc.rel, tc.direction, tc.depth, tc.limit, tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			if v.Hidden != (graph.HiddenCounts{Unclear: 1, Unjudged: 2, Rejected: 1}) {
				t.Errorf("hidden=%+v", v.Hidden)
			}
			if len(v.Edges) != tc.edges {
				t.Fatalf("edges=%+v, want %d", v.Edges, tc.edges)
			}
			n := 0
			for _, e := range v.Edges {
				if e.Unverified {
					n++
					if e.Judge != "" && e.Judge != "unclear" {
						t.Errorf("candidate=%+v", e)
					}
					if e.DocID != f.DocID || e.ChunkID != f.ChunkID || e.Excerpt == "" {
						t.Errorf("provenance=%+v", e)
					}
				}
			}
			if n != tc.candidates {
				t.Errorf("unverified=%d, want %d", n, tc.candidates)
			}
			if !tc.opt.IncludeUnverified {
				old, err := f.Store.Traverse(ctx, f.Key, tc.rel, tc.direction, tc.depth, tc.limit, tc.opt.IncludeLowConfidence)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(old, v) {
					t.Errorf("wrapper differs: %+v / %+v", old, v)
				}
			}
		})
	}
}

func TestConfirmedOnlyVisibilityOptions(t *testing.T) {
	f := graphtest.MixedVisibility(t)
	ctx := context.Background()
	wantHidden := graph.HiddenCounts{Unclear: 1, Unjudged: 2, Rejected: 1}
	for _, tc := range []struct {
		name string
		opt  graph.ViewOptions
		want []string
	}{
		{"default", graph.ViewOptions{}, []string{"advises", "consults", "uses", "works_at"}},
		{"include_low_confidence", graph.ViewOptions{IncludeLowConfidence: true}, []string{"advises", "consults", "evaluates", "uses", "works_at"}},
		{"include_candidates", graph.ViewOptions{IncludeUnverified: true}, []string{"advises", "consults", "pending_in", "pending_out", "unclear", "uses", "works_at"}},
		{"confirmed_only", graph.ViewOptions{ConfirmedOnly: true}, []string{"advises", "works_at"}},
		{"confirmed_only_with_candidates", graph.ViewOptions{ConfirmedOnly: true, IncludeUnverified: true}, []string{"advises", "works_at"}},
		{"confirmed_only_with_low_confidence", graph.ViewOptions{ConfirmedOnly: true, IncludeLowConfidence: true}, []string{"advises", "works_at"}},
	} {
		t.Run("entity/"+tc.name, func(t *testing.T) {
			view, err := f.Store.EntityViewOpts(ctx, f.ID, tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			if view.Hidden != wantHidden {
				t.Errorf("hidden=%+v, want %+v", view.Hidden, wantHidden)
			}
			var rels []string
			for _, fact := range view.Facts {
				if tc.opt.ConfirmedOnly && (fact.Judge != "confirmed" || fact.Unverified) {
					t.Errorf("non-confirmed fact=%+v", fact)
				}
				rels = append(rels, fact.Rel)
			}
			slices.Sort(rels)
			if !slices.Equal(rels, tc.want) {
				t.Errorf("fact relations=%v, want %v", rels, tc.want)
			}
		})
		t.Run("traverse/"+tc.name, func(t *testing.T) {
			sub, err := f.Store.TraverseOpts(ctx, f.ID, "", "both", 1, 50, tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			if sub.Hidden != wantHidden {
				t.Errorf("hidden=%+v, want %+v", sub.Hidden, wantHidden)
			}
			var rels []string
			for _, edge := range sub.Edges {
				if tc.opt.ConfirmedOnly && (edge.Judge != "confirmed" || edge.Unverified) {
					t.Errorf("non-confirmed edge=%+v", edge)
				}
				rels = append(rels, edge.Rel)
			}
			slices.Sort(rels)
			if !slices.Equal(rels, tc.want) {
				t.Errorf("edge relations=%v, want %v", rels, tc.want)
			}
		})
	}
}

func TestConfirmedOnlyTraversalDoesNotExpandThroughUnclearEdges(t *testing.T) {
	f := graphtest.MixedVisibility(t)
	for _, tc := range []struct {
		name string
		opt  graph.ViewOptions
		want []string
	}{
		{"default", graph.ViewOptions{}, []string{"advises", "consults", "makes", "ships", "supplies", "uses", "works_at"}},
		{"confirmed_only", graph.ViewOptions{ConfirmedOnly: true}, []string{"advises", "makes", "works_at"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := f.Store.TraverseOpts(context.Background(), f.ID, "", "out", 2, 50, tc.opt)
			if err != nil {
				t.Fatal(err)
			}
			var rels []string
			for _, edge := range sub.Edges {
				rels = append(rels, edge.Rel)
			}
			slices.Sort(rels)
			if !slices.Equal(rels, tc.want) {
				t.Errorf("edge relations=%v, want %v", rels, tc.want)
			}
			if tc.opt.ConfirmedOnly && (len(sub.Nodes) != 3 || sub.Nodes[1].Key != f.OtherKey) {
				t.Errorf("nodes=%+v, want the start and two neighbours on confirmed edges", sub.Nodes)
			}
		})
	}
}

func TestVisibilityReadPlansUseIndexes(t *testing.T) {
	f := graphtest.Visibility(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		candidates bool
		traverse   bool
	}{
		{"entity_hidden", false, false},
		{"traverse_hidden", false, true},
		{"entity_candidates", true, false},
		{"traverse_candidates", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queries := graph.CaptureReadQueries(f.Store, func() {
				opt := graph.ViewOptions{IncludeUnverified: tc.candidates}
				var err error
				if tc.traverse {
					_, err = f.Store.TraverseOpts(ctx, f.Key, "", "both", 1, 50, opt)
				} else {
					_, err = f.Store.EntityViewOpts(ctx, f.Key, opt)
				}
				if err != nil {
					t.Fatal(err)
				}
			})
			checked := 0
			typedBatches := 0
			for _, q := range queries {
				typedBatch := strings.HasPrefix(q.Query, "UNWIND $ids AS id")
				if !typedBatch && !strings.Contains(q.Query, ":FactCandidate") && !strings.Contains(q.Query, ":RejectedFact") {
					continue
				}
				checked++
				operators := planOperators(t, f.Store, q.Query, q.Params)
				t.Logf("query %d operators: %v", checked, operators)
				if !slices.Contains(operators, "ScanAllByLabelProperties") || slices.Contains(operators, "ScanAll") {
					t.Errorf("query %d operators = %v, want ScanAllByLabelProperties and no ScanAll", checked, operators)
				}
				if typedBatch {
					typedBatches++
					if slices.Contains(operators, "ScanAllByLabel") {
						t.Errorf("typed query %d operators = %v, want no ScanAllByLabel", checked, operators)
					}
				}
			}
			want := 1
			if tc.candidates {
				want = 2
				if tc.traverse {
					want = 3
				}
			}
			if tc.traverse {
				want += 2
				if typedBatches != 2 {
					t.Errorf("checked %d typed batches, want 2", typedBatches)
				}
			}
			if checked != want {
				t.Errorf("checked %d queries, want %d", checked, want)
			}
		})
	}
	t.Run("negative_control", func(t *testing.T) {
		for _, label := range []string{"FactCandidate", "RejectedFact"} {
			operators := planOperators(t, f.Store, "MATCH (f:"+label+") WHERE f.head_key = $q OR f.tail_key = $q RETURN f")
			t.Logf("%s negative control: %v", label, operators)
			if !slices.Contains(operators, "ScanAll") {
				t.Errorf("negative control = %v, want ScanAll", operators)
			}
		}
	})
}

func TestHiddenCountsDistinctFacts(t *testing.T) {
	f := graphtest.Visibility(t)
	uri := os.Getenv(graphtest.URIEnv)
	// Each fact is extracted from three chunks; source multiplicity must not
	// inflate the state counts, including for rejected facts.
	for _, label := range []string{"FactCandidate", "RejectedFact"} {
		rawRun(t, uri, fmt.Sprintf(`MATCH (f:%s {doc_id: '%s'})
UNWIND ['-copy1', '-copy2'] AS suffix
CREATE (copy:%s) SET copy = properties(f), copy.id = f.id + suffix, copy.chunk_id = f.chunk_id + suffix`, label, f.DocID, label))
	}
	assertHidden := func(want graph.HiddenCounts) {
		t.Helper()
		for _, opt := range []graph.ViewOptions{{}, {IncludeUnverified: true, IncludeLowConfidence: true}} {
			view, err := f.Store.EntityViewOpts(context.Background(), f.ID, opt)
			if err != nil {
				t.Fatal(err)
			}
			if view.Hidden != want {
				t.Errorf("entity hidden=%+v, want %+v", view.Hidden, want)
			}
			for _, direction := range []string{"in", "out", "both"} {
				sub, err := f.Store.TraverseOpts(context.Background(), f.ID, "nonexistent", direction, 2, 1, opt)
				if err != nil {
					t.Fatal(err)
				}
				if sub.Hidden != want {
					t.Errorf("%s traverse hidden=%+v, want %+v", direction, sub.Hidden, want)
				}
			}
		}
	}
	assertHidden(graph.HiddenCounts{Unclear: 1, Unjudged: 2, Rejected: 1})
	// A tuple in two candidate states counts once in each state. A self-loop
	// must not be counted by both endpoint branches. Orphans still explain
	// missing facts even though candidate reads cannot join their endpoints.
	rawRun(t, uri, fmt.Sprintf(`CREATE
(:FactCandidate {id:'mixed-%s', doc_id:'%s', head_key:'%s', rel:'pending_out', tail_key:'%s', judge:'unclear'}),
(:FactCandidate {id:'loop-%s', doc_id:'%s', head_key:'%s', rel:'self', tail_key:'%s'}),
(:RejectedFact {doc_id:'%s', head_key:'%s', rel:'self', tail_key:'%s'}),
(:FactCandidate {id:'orphan-%s', doc_id:'%s', head_key:'absent-%s', rel:'missing', tail_key:'%s'}),
(:RejectedFact {doc_id:'%s', head_key:'absent-%s', rel:'missing', tail_key:'%s'})`,
		f.DocID, f.DocID, f.Key, f.OtherKey,
		f.DocID, f.DocID, f.Key, f.Key,
		f.DocID, f.Key, f.Key,
		f.DocID, f.DocID, f.DocID, f.Key,
		f.DocID, f.DocID, f.Key))
	assertHidden(graph.HiddenCounts{Unclear: 2, Unjudged: 4, Rejected: 3})
}

func TestTraverseDoesNotExpandCandidateEndpoints(t *testing.T) {
	f := graphtest.Visibility(t)
	uri := os.Getenv(graphtest.URIEnv)
	// Acme-Max is reached only through an unclear candidate; a confirmed
	// edge beyond it must not appear. Quillon has both a candidate and a
	// confirmed path, so its expansion remains valid.
	rawRun(t, uri, fmt.Sprintf(`MATCH (a:Entity {key:'%s'}), (b:Entity {key:'%s'})
CREATE (a)-[:candidate_only_bridge {doc_id:'%s', chunk_id:'%s'}]->(b),
(b)-[:confirmed_bridge {doc_id:'%s', chunk_id:'%s'}]->(a),
(:FactCandidate {id:'incoming-%s', doc_id:'%s', chunk_id:'%s', head_key:'%s', tail_key:'%s', rel:'pending_in'})`,
		f.ThirdKey, f.OtherKey, f.DocID, f.ChunkID, f.DocID, f.ChunkID,
		f.DocID, f.DocID, f.ChunkID, f.ThirdKey, f.Key))
	for _, direction := range []string{"out", "in", "both"} {
		t.Run(direction, func(t *testing.T) {
			sub, err := f.Store.TraverseOpts(context.Background(), f.Key, "", direction, 2, 50, graph.ViewOptions{IncludeUnverified: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range sub.Edges {
				if e.Rel == "neighbor_only" {
					t.Errorf("expanded candidate-only endpoint to another candidate: %+v", e)
				}
				if e.Rel == "candidate_only_bridge" && direction == "out" {
					t.Errorf("expanded candidate-only endpoint: %+v", e)
				}
				if e.Rel == "confirmed_bridge" && direction == "in" {
					t.Errorf("expanded incoming candidate endpoint: %+v", e)
				}
			}
			if direction != "in" {
				if !slices.ContainsFunc(sub.Edges, func(e graph.PathEdge) bool { return e.Rel == "confirmed_bridge" }) {
					t.Error("confirmed path did not expand Quillon")
				}
				if !slices.ContainsFunc(sub.Nodes, func(n graph.EntityRef) bool { return n.Key == f.ThirdKey }) {
					t.Error("candidate endpoint missing from nodes")
				}
			}
		})
	}
}

func TestTraverseExpandsTypedEndpointAfterCandidate(t *testing.T) {
	f := graphtest.Visibility(t)
	// In a both-direction hop the outgoing candidate is read before this
	// incoming typed edge. Seeing the candidate first must not prevent the
	// typed path from enqueueing the same endpoint.
	rawRun(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (a:Entity {key:'%s'}), (b:Entity {key:'%s'})
CREATE (a)-[:confirmed_in {doc_id:'%s', chunk_id:'%s'}]->(b)`, f.ThirdKey, f.Key, f.DocID, f.ChunkID))
	sub, err := f.Store.TraverseOpts(context.Background(), f.Key, "", "both", 2, 50, graph.ViewOptions{IncludeUnverified: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(sub.Edges, func(e graph.PathEdge) bool { return e.Rel == "neighbor_only" }) {
		t.Error("typed path did not expand an endpoint previously seen through a candidate")
	}
}
