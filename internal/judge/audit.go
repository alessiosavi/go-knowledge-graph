package judge

import (
	"context"
	"math"
	"math/rand/v2"
	"sort"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

type AuditRow struct {
	ID       string `json:"id"`
	Domain   string `json:"domain"`
	Head     string `json:"head"`
	Relation string `json:"relation"`
	Tail     string `json:"tail"`
	Evidence string `json:"evidence"`
	Label    string `json:"label"`
}

// AuditSample takes a proportional stratified sample of one ordered snapshot.
func AuditSample(ctx context.Context, st *graph.Store, n int, seed uint64) ([]AuditRow, error) {
	facts, err := st.TypedFactsSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	selected, err := st.TypedFactsEvidence(ctx, selectTypedFacts(facts, n, seed))
	if err != nil {
		return nil, err
	}
	return auditRows(selected), nil
}

func sampleTypedFacts(facts []graph.TypedFact, n int, seed uint64) []AuditRow {
	return auditRows(selectTypedFacts(facts, n, seed))
}

func selectTypedFacts(facts []graph.TypedFact, n int, seed uint64) []graph.TypedFact {
	out := make([]graph.TypedFact, 0)
	if n <= 0 || len(facts) == 0 {
		return out
	}
	n = min(n, len(facts))
	byDomain := make(map[string][]graph.TypedFact)
	for _, f := range facts {
		byDomain[f.Domain] = append(byDomain[f.Domain], f)
	}
	type stratum struct {
		domain           string
		count, remainder int
	}
	strata := make([]stratum, 0, len(byDomain))
	assigned := 0
	for domain, fs := range byDomain {
		q := n * len(fs)
		count := q / len(facts)
		strata = append(strata, stratum{domain, count, q % len(facts)})
		assigned += count
	}
	// Largest remainder; lexical domain order breaks ties deterministically.
	sort.Slice(strata, func(i, j int) bool {
		if strata[i].remainder != strata[j].remainder {
			return strata[i].remainder > strata[j].remainder
		}
		return strata[i].domain < strata[j].domain
	})
	for i := 0; i < n-assigned; i++ {
		strata[i].count++
	}
	sort.Slice(strata, func(i, j int) bool { return strata[i].domain < strata[j].domain })
	for _, s := range strata {
		fs := byDomain[s.domain]
		rng := rand.New(rand.NewPCG(seed, 0))
		rng.Shuffle(len(fs), func(i, j int) { fs[i], fs[j] = fs[j], fs[i] })
		out = append(out, fs[:s.count]...)
	}
	return out
}

func auditRows(facts []graph.TypedFact) []AuditRow {
	out := make([]AuditRow, 0, len(facts))
	for _, f := range facts {
		out = append(out, AuditRow{ID: f.ID, Domain: f.Domain, Head: f.Head, Relation: f.Relation, Tail: f.Tail, Evidence: ingest.EvidenceSentence(f.Text, f.HeadStart, f.HeadEnd)})
	}
	return out
}

// AuditScore excludes skip and unlabelled rows. The interval is Wilson's
// two-sided 95% interval with z=1.96; no scored rows returns all zeros.
func AuditScore(rows []AuditRow) (precision, lo, hi float64, n int) {
	ok := 0
	for _, r := range rows {
		switch r.Label {
		case "ok":
			ok++
			n++
		case "wrong", "noise":
			n++
		}
	}
	if n == 0 {
		return 0, 0, 0, 0
	}
	precision = float64(ok) / float64(n)
	const z = 1.96
	z2 := z * z
	nf := float64(n)
	den := 1 + z2/nf
	center := (precision + z2/(2*nf)) / den
	half := z * math.Sqrt(precision*(1-precision)/nf+z2/(4*nf*nf)) / den
	lo, hi = max(0, center-half), min(1, center+half)
	if ok == 0 {
		lo = 0
	}
	if ok == n {
		hi = 1
	}
	return precision, lo, hi, n
}
