package reteval

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
)

func TestAbstainedStepLimitFallback(t *testing.T) {
	for _, answer := range []string{agent.NoAnswerText, strings.ToUpper(agent.NoAnswerText)} {
		a := &AgentResult{Answer: answer}
		if err := scoreAgent(context.Background(), nil, Question{Stratum: "null"}, resolvedGold{}, a); err != nil {
			t.Fatal(err)
		}
		if a.Abstained == nil || !*a.Abstained || !a.NeedsReview {
			t.Errorf("step-limit fallback must abstain and need review: %+v", a)
		}
	}
}

func TestSearchMetrics(t *testing.T) {
	a := [][]string{{"a"}, {"b", "c"}}
	r := []string{"entity-x", "c", "entity-y", "a"}
	for _, tc := range []struct {
		k    int
		want float64
	}{{1, 0}, {2, .5}, {4, 1}} {
		if got := RecallAtK(a, r, tc.k); got != tc.want {
			t.Errorf("R@%d = %v, want %v", tc.k, got, tc.want)
		}
		if got := ItemRecallAtK(a, r, tc.k); got != tc.want {
			t.Errorf("item R@%d = %v", tc.k, got)
		}
	}
	if got := MRR(a, r); got != .5 {
		t.Errorf("MRR = %v", got)
	}
	if got := MRR(a, []string{"x"}); got != 0 {
		t.Errorf("no-match MRR = %v", got)
	}
}

func TestAnswerPartRecall(t *testing.T) {
	parts := []AnswerPart{{Names: []string{"Città Alta", "citta alta"}}, {Names: []string{"Zorbo"}}}
	if got := AnswerPartRecall(parts, "... citta alta ..."); got != .5 {
		t.Fatalf("recall = %v", got)
	}
	if got := Fold("  CITTÀ\tＡＬＴＡ\n"); got != "citta alta" {
		t.Fatalf("Fold = %q", got)
	}
}

func TestAbstained(t *testing.T) {
	for _, s := range []string{"CANNOT ANSWER", "can't answer", "no information", "not found in the", "i don't know", "non ho trovato", "non è presente", "non risulta", "nessuna informazione", "non sono in grado", "I cannot answer, but Acme owns Zorbo"} {
		if !Abstained(s) {
			t.Errorf("did not match %q", s)
		}
	}
	if Abstained("Acme owns Zorbo.") {
		t.Fatal("factual answer abstained")
	}
}

func TestBootstrapCI(t *testing.T) {
	lo, hi := BootstrapCI([]float64{0, 1, .5, 1}, 1000, 1)
	l2, h2 := BootstrapCI([]float64{0, 1, .5, 1}, 1000, 1)
	if lo != l2 || hi != h2 || lo >= hi || math.IsNaN(lo) {
		t.Fatalf("CI = %v,%v then %v,%v", lo, hi, l2, h2)
	}
	lo, hi = BootstrapCI([]float64{.5, .5, .5}, 1000, 1)
	if lo != .5 || hi != .5 {
		t.Fatalf("constant CI = %v,%v", lo, hi)
	}
}
