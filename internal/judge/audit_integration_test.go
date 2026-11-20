package judge_test

import (
	"reflect"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/judge"
)

func TestAuditSnapshot(t *testing.T) {
	f := newBackfillFixture(t, 1)
	// A candidate is outside the audit population.
	before, err := f.st.TypedFactsSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range before {
		if x.ID == f.ids[0] {
			t.Fatal("candidate in typed population")
		}
	}
	cs, err := f.st.UnjudgedCandidates(t.Context(), f.domain, 0)
	if err != nil || len(cs) != 1 {
		t.Fatalf("candidates=%+v err=%v", cs, err)
	}
	if ok, err := f.st.PromoteCandidate(t.Context(), cs[0].ID, cs[0].InputHash, ""); err != nil || !ok {
		t.Fatalf("promote %v %v", ok, err)
	}
	backfillQuery(t, "MATCH (c:Chunk {id: $chunk}) SET c.text = 'Before. Acme-Max uses Zorbo. After.'", map[string]any{"chunk": f.chunk})
	backfillQuery(t, "MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = $chunk SET r.head_start = 8, r.head_end = 16", map[string]any{"chunk": f.chunk})
	snapshot, err := f.st.TypedFactsSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range snapshot {
		if x.Text != "" || x.Head != "" || x.Tail != "" || x.HeadStart != 0 || x.HeadEnd != 0 {
			t.Fatalf("snapshot materialized evidence before sampling: id=%s text_bytes=%d head=%q tail=%q", x.ID, len(x.Text), x.Head, x.Tail)
		}
	}
	// Head domain is authoritative; absent domain must coalesce to empty.
	for _, domain := range []string{"quillon-domain", ""} {
		q := "MATCH (e:Entity {key: $key}) SET e.domain = $domain"
		if domain == "" {
			q = "MATCH (e:Entity {key: $key}) REMOVE e.domain"
		}
		backfillQuery(t, q, map[string]any{"key": cs[0].HeadKey, "domain": domain})
		a, err := judge.AuditSample(t.Context(), f.st, 1000000, 7)
		if err != nil {
			t.Fatal(err)
		}
		b, err := judge.AuditSample(t.Context(), f.st, 1000000, 7)
		if err != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("not deterministic: %v", err)
		}
		found := false
		for _, r := range a {
			if r.ID == f.ids[0] {
				found = true
				if r.Domain != domain || r.Head != "Acme-Max" || r.Relation != "uses" || r.Tail != "Zorbo0" || r.Evidence != "Acme-Max uses Zorbo." {
					t.Fatalf("row=%+v", r)
				}
			}
		}
		if !found {
			t.Fatal("promoted edge absent")
		}
	}
}

func TestAuditPopulationConfirmedOnly(t *testing.T) {
	f := newBackfillFixture(t, 1)
	cs, err := f.st.UnjudgedCandidates(t.Context(), f.domain, 0)
	if err != nil || len(cs) != 1 {
		t.Fatalf("candidates=%+v err=%v", cs, err)
	}
	if ok, err := f.st.PromoteCandidate(t.Context(), cs[0].ID, cs[0].InputHash, ""); err != nil || !ok {
		t.Fatalf("promote %v %v", ok, err)
	}
	for _, status := range []string{"", "unclear", "refuted", "absent", "confirmed"} {
		t.Run(status, func(t *testing.T) {
			q := "MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = $chunk SET r.judge = $status"
			if status == "absent" {
				q = "MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = $chunk REMOVE r.judge"
			}
			backfillQuery(t, q, map[string]any{"chunk": f.chunk, "status": status})
			facts, err := f.st.TypedFactsSnapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, fact := range facts {
				if fact.ID == f.ids[0] {
					found = true
				}
			}
			if found != (status == "confirmed") {
				t.Fatalf("judge=%q in audit population=%v, want %v", status, found, status == "confirmed")
			}
		})
	}
}

func TestAuditSampleFixedSeedRows(t *testing.T) {
	f := newBackfillFixture(t, 8)
	cs, err := f.st.UnjudgedCandidates(t.Context(), f.domain, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if ok, err := f.st.PromoteCandidate(t.Context(), c.ID, c.InputHash, ""); err != nil || !ok {
			t.Fatalf("promote %v %v", ok, err)
		}
	}
	backfillQuery(t, "MATCH (e:Entity {key: $key}) SET e.domain = $domain", map[string]any{"key": cs[0].HeadKey, "domain": f.domain})
	// Keep the expected output independent of evidence hydration: these are
	// the legacy PCG(42, 0) results for eight identity-ordered edges.
	rows, err := judge.AuditSample(t.Context(), f.st, 1000000, 42)
	if err != nil {
		t.Fatal(err)
	}
	var got []judge.AuditRow
	for _, row := range rows {
		if row.Domain == f.domain {
			got = append(got, row)
		}
	}
	var want []judge.AuditRow
	for _, i := range []int{3, 4, 2, 1, 5, 0, 6, 7} {
		want = append(want, judge.AuditRow{ID: f.ids[i], Domain: f.domain, Head: "Acme-Max", Relation: "uses", Tail: f.inputs[i].Tail, Evidence: "Acme-Max uses Zorbo."})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("seed=42 rows changed: got=%+v want=%+v", got, want)
	}
	t.Log("seed=42 full rows unchanged: tails=3,4,2,1,5,0,6,7 with original names and evidence")
}
