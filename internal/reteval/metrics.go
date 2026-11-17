package reteval

import (
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Fold applies NFKD, removes combining marks, lowercases and collapses space.
func Fold(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsMark(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, norm.NFKD.String(s))
	return strings.Join(strings.Fields(s), " ")
}

func anchorRanks(anchors [][]string, ranked []string) []int {
	pos := make(map[string]int, len(ranked))
	for i, id := range ranked {
		if id != "" && pos[id] == 0 {
			pos[id] = i + 1
		}
	}
	ranks := make([]int, len(anchors))
	for i, ids := range anchors {
		for _, id := range ids {
			if r := pos[id]; r > 0 && (ranks[i] == 0 || r < ranks[i]) {
				ranks[i] = r
			}
		}
	}
	return ranks
}

func RecallAtK(anchors [][]string, ranked []string, k int) float64 {
	if len(anchors) == 0 {
		return 0
	}
	n := 0
	for _, r := range anchorRanks(anchors, ranked) {
		if r > 0 && r <= k {
			n++
		}
	}
	return float64(n) / float64(len(anchors))
}

func MRR(anchors [][]string, ranked []string) float64 {
	first := 0
	for _, r := range anchorRanks(anchors, ranked) {
		if r > 0 && (first == 0 || r < first) {
			first = r
		}
	}
	if first == 0 {
		return 0
	}
	return 1 / float64(first)
}

func ItemRecallAtK(items [][]string, ranked []string, k int) float64 {
	return RecallAtK(items, ranked, k)
}

func named(names []string, answer string) bool {
	for _, n := range names {
		if f := Fold(n); f != "" && strings.Contains(answer, f) {
			return true
		}
	}
	return false
}

func AnswerPartRecall(parts []AnswerPart, answer string) float64 {
	if len(parts) == 0 {
		return 0
	}
	n := 0
	answer = Fold(answer)
	for _, p := range parts {
		if named(p.Names, answer) {
			n++
		}
	}
	return float64(n) / float64(len(parts))
}

// Abstained is provisional: callers must flag every match for manual review,
// since a matching answer may also contain a factual claim.
func Abstained(answer string) bool {
	answer = Fold(answer)
	for _, p := range []string{"cannot answer", "can't answer", "no information", "not found in the", "i don't know", "non ho trovato", "non è presente", "non risulta", "nessuna informazione", "non sono in grado"} {
		if strings.Contains(answer, Fold(p)) {
			return true
		}
	}
	return false
}

// BootstrapCI returns the percentile 95% interval of resampled means.
func BootstrapCI(values []float64, resamples int, seed uint64) (lo, hi float64) {
	if len(values) == 0 || resamples <= 0 {
		return 0, 0
	}
	rng := rand.New(rand.NewPCG(seed, seed))
	means := make([]float64, resamples)
	for i := range means {
		for range values {
			means[i] += values[rng.IntN(len(values))]
		}
		means[i] /= float64(len(values))
	}
	slices.Sort(means)
	percentile := func(p float64) float64 {
		idx := p * float64(len(means)-1)
		a := int(math.Floor(idx))
		b := int(math.Ceil(idx))
		return means[a] + (means[b]-means[a])*(idx-float64(a))
	}
	return percentile(.025), percentile(.975)
}
