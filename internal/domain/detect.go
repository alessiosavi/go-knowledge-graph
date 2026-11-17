package domain

import (
	"fmt"
	"math"
	"sort"
)

// Params controls the attach threshold, margin, and prefix-prior override.
type Params struct {
	Tau, Delta, TauOverride float64
}

// DefaultParams are the spec's thresholds: τ 0.05, δ 0.02, τ_override 0.15.
// Decide and Evaluate use them for an all-zero Params, which would
// otherwise attach every document at score 0.
func DefaultParams() Params { return Params{Tau: 0.05, Delta: 0.02, TauOverride: 0.15} }

// ParamsFromConfig resolves configured thresholds: each zero field takes its
// DefaultParams value on its own. Ingest detection, `gkg domain evaluate` and
// the v4 migration all use it, so they cannot decide with different values.
func ParamsFromConfig(tau, delta, tauOverride float64) Params {
	p := DefaultParams()
	if tau != 0 {
		p.Tau = tau
	}
	if delta != 0 {
		p.Delta = delta
	}
	if tauOverride != 0 {
		p.TauOverride = tauOverride
	}
	return p
}

func (p Params) orDefault() Params {
	if p == (Params{}) {
		return DefaultParams()
	}
	return p
}

// DomainScore is a domain's mean of its top three document cosines.
type DomainScore struct {
	Domain string
	Score  float64
}

// Decision records an assignment, its top three scores, and the parameters
// used. Decide produces prefix, attach, or new; callers handling re-ingest
// can record inherit or proposed-move with the same representation.
type Decision struct {
	Action, Domain string
	Top            []DomainScore
	Params         Params
	Note           string
}

// Score ranks domains by descending similarity, breaking ties by domain
// id. corpus must exclude the target; Decide does that by document id.
func Score(target Profile, corpus []DocProfile) []DomainScore {
	corpus = withoutTarget(DocProfile{}, corpus)
	df := documentFrequencies(corpus)
	targetVector := normalizedVector(target, df, len(corpus))
	perDomain := make(map[string][]float64)
	for _, doc := range corpus {
		cosine := vectorCosine(targetVector, normalizedVector(doc.Profile, df, len(corpus)))
		perDomain[doc.Domain] = append(perDomain[doc.Domain], cosine)
	}
	scores := make([]DomainScore, 0, len(perDomain))
	for domain, cosines := range perDomain {
		sort.Sort(sort.Reverse(sort.Float64Slice(cosines)))
		top := cosines[:min(3, len(cosines))]
		var sum float64
		for _, cosine := range top {
			sum += cosine
		}
		scores = append(scores, DomainScore{Domain: domain, Score: sum / float64(len(top))})
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].Score != scores[j].Score {
			return scores[i].Score > scores[j].Score
		}
		return scores[i].Domain < scores[j].Domain
	})
	return scores
}

func normalizedVector(profile Profile, df map[string]int, n int) []weightedTerm {
	terms := weightedTerms(profile, df, n)
	var sum float64
	for _, term := range terms {
		sum += term.weight * term.weight
	}
	if sum > 0 {
		norm := math.Sqrt(sum)
		for i := range terms {
			terms[i].weight /= norm
		}
	}
	return terms
}

func vectorCosine(a, b []weightedTerm) float64 {
	var sum float64
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i].name < b[j].name:
			i++
		case a[i].name > b[j].name:
			j++
		default:
			sum += a[i].weight * b[j].weight
			i++
			j++
		}
	}
	return min(sum, 1) // Roundoff must not produce a cosine above one.
}

// Decide applies the unanimous source-prefix prior (and its override),
// then the similarity threshold and margin, otherwise creating a domain.
func Decide(target DocProfile, corpus []DocProfile, p Params) Decision {
	p = p.orDefault()
	corpus = withoutTarget(target, corpus)
	scores := Score(target.Profile, corpus)
	decision := decideScores(target, scores, p)
	if prior, ok := prefixDomain(target.Source, corpus); ok {
		if len(scores) > 0 && scores[0].Domain != prior && scores[0].Score >= p.TauOverride {
			decision.Action, decision.Domain = "attach", scores[0].Domain
			decision.Note = fmt.Sprintf("prefix prior %q overridden by detection %q", prior, scores[0].Domain)
		} else {
			decision.Action, decision.Domain = "prefix", prior
		}
	}
	return decision
}

func decideScores(target DocProfile, scores []DomainScore, p Params) Decision {
	decision := Decision{Action: "new", Params: p, Top: append([]DomainScore(nil), scores[:min(3, len(scores))]...)}
	if len(scores) > 0 {
		var second float64
		if len(scores) > 1 {
			second = scores[1].Score
		}
		if scores[0].Score >= p.Tau && scores[0].Score-second >= p.Delta {
			decision.Action, decision.Domain = "attach", scores[0].Domain
			return decision
		}
	}
	decision.Domain = NewDomainID(target.DocID)
	return decision
}
