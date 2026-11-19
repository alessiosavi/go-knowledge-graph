package graph

import (
	"reflect"
	"testing"
)

func TestMergeFoldPlanScopeAndStats(t *testing.T) {
	st := foldIntegrationStore(t)
	ctx := t.Context()
	foldTestWrite(t, st, `CREATE (:GkgMeta {id:'fold', fold_version:$version, status:'complete',
         exceptions_keys:['@zeta:veto'], exceptions_digest:$digest}),
(a:Entity {key:'@zeta:00-a', id:'a', name:'Zórvulo Max', type:'organization', domain:'zeta',
           aliases:['Zorvulot'], mention_count:1, embedding:[i IN range(1,768) | 1.0], fold_key:'@zeta|old'}),
(b:Entity {key:'@zeta:00-b', id:'b', name:'Zorvulo-Max', type:'organization', domain:'zeta',
           mention_count:8, fold_key:'@zeta|zorvulomax'}),
(c:Entity {key:'@zeta:00-c', id:'c', name:'ZorvuloMax™', type:'product', domain:'zeta', mention_count:8}),
(:Entity {key:'@zeta:zz-d', id:'d', name:'Zorvulo  Max', type:'organization', domain:'zeta', fold_key:'@zeta|zorvulomax'}),
(:Entity {key:'@zeta:veto', id:'veto', name:'Zorvulo Max', fold_key:'@zeta|zorvulomax'}),
(:Entity {key:$bare_doc, id:'bare-doc', name:'Zorvulo Max'}),
(:Entity {key:$domain_doc, id:'domain-doc', name:'Zorvulo Max', fold_key:'@zeta|zorvulomax'}),
(:Entity {key:'@zeta:', id:'prefix-boundary', name:'Xelvimo', fold_key:'@zeta|zorvulomax'}),
(:Entity {key:'@zeta:missing-id', name:'Zorvulo Max', mention_count:99, fold_key:'stale'}),
(:Entity {key:'@zeta:empty-id', id:'', name:'Zorvulo Max'}),
(:Entity {key:'@zeta-two:a', id:'other-a', name:'Zorvulo Max', fold_key:'@zeta-two|zorvulomax'}),
(:Entity {key:'@zeta-two:b', id:'other-b', name:'ZorvuloMax™', fold_key:'@zeta-two|zorvulomax'}),
(:Entity {key:'@zeta;', id:'upper-boundary'}),
(:Entity {id:'keyless', fold_key:'orphan'}), (:Entity {key:'', id:'empty-key'}),
(x:Entity {key:'external', id:'external'}),
(d1:Document {id:'d1'}), (d2:Document {id:'d2'}),
(ch1:Chunk {id:'ch1'}), (ch2:Chunk {id:'ch2'}), (ch3:Chunk {id:'ch3'}),
(d1)-[:HAS_CHUNK]->(ch1), (d1)-[:HAS_CHUNK]->(ch2), (d2)-[:HAS_CHUNK]->(ch3),
(ch1)-[:MENTIONS]->(a), (ch2)-[:MENTIONS]->(a), (ch3)-[:MENTIONS]->(a),
(ch1)-[:MENTIONS]->(b), (ch1)-[:MENTIONS]->(c),
(a)-[:uses {judge:'confirmed'}]->(x), (a)-[:uses {judge:''}]->(x),
(a)-[:uses {judge:'unclear'}]->(x), (x)-[:uses {judge:'confirmed'}]->(a),
(a)-[:uses {judge:'confirmed'}]->(c), (a)-[:uses]->(a), (c)-[:uses]->(b), (b)-[:uses]->(x),
(:FactCandidate {id:'internal', head_key:a.key, rel:'uses', tail_key:c.key, chunk_id:'ch1'}),
(:FactCandidate {id:'canonical-only', head_key:b.key, rel:'uses', tail_key:x.key, chunk_id:'ch2'}),
(:RejectedFact {id:'incoming', head_key:'orphan', rel:'uses', tail_key:c.key, chunk_id:'ch3'}),
(:RejectedFact {id:'self', head_key:a.key, rel:'uses', tail_key:a.key, chunk_id:'ch2'})`, map[string]any{
		"version": FoldKeyVersion, "digest": "fixture-digest",
		"bare_doc":   ScopedEntityKey("doc-a", "Zorvulo Max"),
		"domain_doc": DomainKey("zeta", ScopedEntityKey("doc-b", "Zorvulo Max")),
	})
	// The first and last members of the fold span the 2000-key page boundary.
	foldTestWrite(t, st, `UNWIND range(1,2000) AS n
CREATE (:Entity {key:'@zeta:fill-' + toString(n), id:'fill-' + toString(n)})`, nil)

	plan, err := st.PlanMergeFolds(ctx, "zeta")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(plan.Groups))
	}
	g := plan.Groups[0]
	if g.Fold != "@zeta|zorvulomax" || g.Canonical != "@zeta:00-b" || !g.TypeMismatch || g.Rewire != 14 || g.Collapses != 0 || len(g.Blocking) != 0 {
		t.Fatalf("group = %+v; want canonical @zeta:00-b, 14 rewires, no collapses or blockers", g)
	}
	h := plan.Header
	if h.PlanVersion != 1 || h.FoldVersion != FoldKeyVersion || h.ExceptionsDigest != "fixture-digest" ||
		h.EntityCount != 2007 || h.SkippedRows != 2 || h.GeneratedAt.IsZero() {
		t.Fatalf("header = %+v", h)
	}
	wantSummary := MergeFoldSummary{Groups: 1, Members: 4, TypeMismatches: 1, MaxRewire: 14, P95Rewire: 14, StaleHints: 5}
	if plan.Summary != wantSummary {
		t.Fatalf("summary = %+v; want %+v", plan.Summary, wantSummary)
	}
	want := []MergeFoldMember{
		{Key: "@zeta:00-a", ID: "a", Name: "Zórvulo Max", Type: "organization", Domain: "zeta", Aliases: []string{"Zorvulot"},
			MentionCount: 1, DocumentCount: 2, EdgesOut: MergeFoldEdges{Confirmed: 2, Unjudged: 2, Other: 1},
			EdgesIn: MergeFoldEdges{Confirmed: 1, Unjudged: 1}, Candidates: 1, Rejected: 1, EmbeddingPresent: true, mentions: 3},
		{Key: "@zeta:00-b", ID: "b", Name: "Zorvulo-Max", Type: "organization", Domain: "zeta", Aliases: []string{},
			MentionCount: 8, DocumentCount: 1, EdgesOut: MergeFoldEdges{Unjudged: 1}, EdgesIn: MergeFoldEdges{Unjudged: 1}, Candidates: 1, mentions: 1},
		{Key: "@zeta:00-c", ID: "c", Name: "ZorvuloMax™", Type: "product", Domain: "zeta", Aliases: []string{},
			MentionCount: 8, DocumentCount: 1, EdgesOut: MergeFoldEdges{Unjudged: 1}, EdgesIn: MergeFoldEdges{Confirmed: 1}, Candidates: 1, Rejected: 1, mentions: 1},
		{Key: "@zeta:zz-d", ID: "d", Name: "Zorvulo  Max", Type: "organization", Domain: "zeta", Aliases: []string{}},
	}
	if !reflect.DeepEqual(g.Members, want) {
		t.Fatalf("members = %+v; want %+v", g.Members, want)
	}
	all, err := st.PlanMergeFolds(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if all.Header.EntityCount != 2012 || all.Header.SkippedRows != 4 || all.Summary.StaleHints != 5 || len(all.Groups) != 2 ||
		all.Groups[0].Fold != "@zeta-two|zorvulomax" || all.Groups[0].Canonical != "@zeta-two:a" ||
		all.Groups[0].Rewire != 0 || all.Groups[0].TypeMismatch || !reflect.DeepEqual(all.Groups[1], g) {
		t.Fatalf("unrestricted plan = %+v", all)
	}
}

func TestMergeFoldPlanFactIDs(t *testing.T) {
	st := foldIntegrationStore(t)
	foldTestWrite(t, st, `CREATE (:GkgMeta {id:'fold', fold_version:$version, status:'complete'}),
(:Entity {key:'a', id:'a', name:'Vèlzuno', mention_count:3}), (:Entity {key:'a-old', id:'a-old', name:'Velzuno'}),
(:Entity {key:'a-older', id:'a-older', name:'Velzuno™'}),
(:Entity {key:'b', id:'b', name:'Kelzunor', mention_count:3}), (:Entity {key:'b-old', id:'b-old', name:'Kelzunor™'}),
(:Entity {key:'c', id:'c', name:'Pelzuno', mention_count:3}), (:Entity {key:'c-old', id:'c-old', name:'Pelzuno™'})`,
		map[string]any{"version": FoldKeyVersion})
	for _, tc := range []struct {
		label string
		id    func(string, string, string, string) string
	}{
		{"FactCandidate", CandidateID},
		{"RejectedFact", FactAuditID},
	} {
		rows := []map[string]any{
			// Existing equal tuples collapse, including a row referencing losers in two groups.
			{"head": "a-old", "tail": "b-old", "chunk": "same", "doc": "d"},
			{"head": "a", "tail": "b", "chunk": "same", "doc": "d"},
			// The destination and the existing different tuple hash the same pipe-joined input.
			{"head": "a-old", "tail": "outside|part", "chunk": "blocked", "doc": "d"},
			{"head": "a", "tail": "outside", "chunk": "part|blocked", "doc": "d"},
			// A projected-only collision after replacing losers at opposite ends.
			{"head": "a-old", "tail": "b|part", "chunk": "future", "doc": "d"},
			{"head": "a", "tail": "b-old", "chunk": "part|future", "doc": "d"},
			// Two loser rows project to the same tuple, with no existing destination.
			{"head": "a-old", "tail": "outside", "chunk": "future-same", "doc": "d"},
			{"head": "a-older", "tail": "outside", "chunk": "future-same", "doc": "d"},
			// Missing chunks in distinct documents cannot collapse.
			{"head": "c-old", "tail": "outside", "chunk": "", "doc": "first"},
			{"head": "c", "tail": "outside", "chunk": "", "doc": "second"},
		}
		for _, row := range rows {
			row["id"] = tc.id(row["head"].(string), "uses", row["tail"].(string), row["chunk"].(string))
		}
		foldTestWrite(t, st, `UNWIND $rows AS row CREATE (:`+tc.label+` {id:row.id, head_key:row.head, rel:'uses',
tail_key:row.tail, chunk_id:row.chunk, doc_id:row.doc})`, map[string]any{"rows": rows})
	}
	plan, err := st.PlanMergeFolds(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Summary.BlockingGroups != 3 || len(plan.Groups) != 3 {
		t.Fatalf("collision summary = %+v, groups = %+v", plan.Summary, plan.Groups)
	}
	want := map[string]struct{ rewire, collapses, blockers int }{
		"a": {10, 4, 4}, "b": {4, 2, 2}, "c": {2, 0, 2},
	}
	for _, g := range plan.Groups {
		w := want[g.Canonical]
		if g.Rewire != w.rewire || g.Collapses != w.collapses || len(g.Blocking) != w.blockers {
			t.Errorf("group %s: rewire/collapses/blocking = %d/%d/%+v; want %+v",
				g.Canonical, g.Rewire, g.Collapses, g.Blocking, w)
		}
		for _, block := range g.Blocking {
			rewritten, conflicting := block.Rewritten, block.Conflicting
			if block.SourceID == "" || block.ConflictingID == "" || rewritten == conflicting ||
				block.TargetID != FactAuditID(rewritten.HeadKey, rewritten.Rel, rewritten.TailKey, rewritten.ChunkID) ||
				block.TargetID != FactAuditID(conflicting.HeadKey, conflicting.Rel, conflicting.TailKey, conflicting.ChunkID) ||
				block.Projected != (rewritten.ChunkID == "future" || rewritten.ChunkID == "part|future") ||
				(!block.Projected && block.ConflictingID != block.TargetID) {
				t.Errorf("invalid collision: %+v", block)
			}
		}
	}
}
