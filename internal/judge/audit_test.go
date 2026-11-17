package judge

import (
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestAuditDeterministic(t *testing.T) {
	var facts []graph.TypedFact
	for _, d := range []struct {
		name string
		n    int
	}{{"acme", 8}, {"quillon", 3}, {"", 1}} {
		for i := 0; i < d.n; i++ {
			facts = append(facts, graph.TypedFact{ID: fmt.Sprintf("%s-%d", d.name, i), Domain: d.name, Head: "Acme-Max", Relation: "uses", Tail: "Zorbo", Text: "Before. Acme-Max uses Zorbo. After.", HeadStart: 8, HeadEnd: 16})
		}
	}
	a := sampleTypedFacts(facts, 5, 42)
	b := sampleTypedFacts(facts, 5, 42)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed produced different samples")
	}
	counts := map[string]int{}
	seen := map[string]bool{}
	for _, row := range a {
		counts[row.Domain]++
		if seen[row.ID] {
			t.Fatal("repeated edge")
		}
		seen[row.ID] = true
		if row.Evidence != "Acme-Max uses Zorbo." {
			t.Fatalf("evidence=%q", row.Evidence)
		}
		if row.Label != "" {
			t.Fatal("sample already labelled")
		}
	}
	if !reflect.DeepEqual(counts, map[string]int{"acme": 3, "quillon": 1, "": 1}) {
		t.Fatalf("allocation=%v want 3/1/1", counts)
	}
	if reflect.DeepEqual(a, sampleTypedFacts(facts, 5, 43)) {
		t.Fatal("different seeds produced identical sample")
	}
	if len(sampleTypedFacts(facts, 100, 42)) != 12 || len(sampleTypedFacts(nil, 5, 42)) != 0 || len(sampleTypedFacts(facts, 0, 42)) != 0 {
		t.Fatal("sample bounds")
	}
	t.Log("population=12 (8/3/1); sample=5 (3/1/1), deterministic seed=42")
}

func TestWilson(t *testing.T) {
	rows := make([]AuditRow, 200)
	for i := range rows {
		rows[i].Label = "ok"
		if i >= 190 {
			rows[i].Label = "wrong"
		}
	}
	p, lo, hi, n := AuditScore(rows)
	if n != 200 || p != 0.95 || math.Abs(lo-0.910) > 0.002 || math.Abs(hi-0.973) > 0.002 {
		t.Fatalf("p=%g interval=[%g,%g] n=%d", p, lo, hi, n)
	}
	rows = append(rows, AuditRow{Label: "skip"}, AuditRow{}, AuditRow{Label: "unknown"})
	p2, l2, h2, n2 := AuditScore(rows)
	if p2 != p || l2 != lo || h2 != hi || n2 != n {
		t.Fatal("unscored rows changed precision")
	}
	p, lo, hi, n = AuditScore([]AuditRow{{Label: "noise"}, {Label: "skip"}})
	if p != 0 || lo != 0 || hi <= 0 || n != 1 {
		t.Fatalf("noise: %g %g %g %d", p, lo, hi, n)
	}
	p, lo, hi, n = AuditScore(nil)
	if p != 0 || lo != 0 || hi != 0 || n != 0 {
		t.Fatal("empty score")
	}
	t.Logf("190/200: precision=0.95 Wilson95=[%.6f, %.6f]", l2, h2)
}
