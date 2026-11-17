package domain

// EvalResult contains document-weighted leave-one-out and unseen-domain
// evaluation shares.
type EvalResult struct {
	Docs                                                       int
	TopChoiceAccuracy, AutoAttachShare, UnseenFalseAttachShare float64
}

// Evaluate holds out each document for the complete decision rule. Top
// choice uses the prefix choice when the prior wins, otherwise the best
// similarity domain, even if thresholds prevent an automatic attach.
// For unseen-domain evaluation, the target's whole domain is held out,
// including from IDF statistics and the prefix prior.
func Evaluate(profiles []DocProfile, p Params) EvalResult {
	p = p.orDefault()
	profiles = withoutTarget(DocProfile{}, profiles)
	result := EvalResult{Docs: len(profiles)}
	if result.Docs == 0 {
		return result
	}
	var correct, attached, falseAttached int
	for _, target := range profiles {
		decision := Decide(target, profiles, p)
		var choice string
		if decision.Action == "prefix" {
			choice = decision.Domain
		} else if len(decision.Top) > 0 {
			choice = decision.Top[0].Domain
		}
		if choice == target.Domain {
			correct++
		}
		if decision.Action == "prefix" || decision.Action == "attach" {
			attached++
		}
		unseenCorpus := make([]DocProfile, 0, len(profiles))
		for _, doc := range profiles {
			if doc.Domain != target.Domain {
				unseenCorpus = append(unseenCorpus, doc)
			}
		}
		unseen := Decide(target, unseenCorpus, p)
		if unseen.Action == "prefix" || unseen.Action == "attach" {
			falseAttached++
		}
	}
	n := float64(result.Docs)
	result.TopChoiceAccuracy = float64(correct) / n
	result.AutoAttachShare = float64(attached) / n
	result.UnseenFalseAttachShare = float64(falseAttached) / n
	return result
}
