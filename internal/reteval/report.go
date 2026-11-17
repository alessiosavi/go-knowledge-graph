package reteval

import (
	"fmt"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
)

type Stat struct {
	Mean float64 `json:"mean"`
	Lo   float64 `json:"lo"`
	Hi   float64 `json:"hi"`
	N    int     `json:"n"`
}
type SearchResult struct {
	Variant    string             `json:"variant"`
	Kind       string             `json:"kind"`
	Ranks      []int              `json:"ranks"`
	Recall     map[string]float64 `json:"recall"`
	MRR        float64            `json:"mrr"`
	ItemRecall map[string]float64 `json:"item_recall,omitempty"`
}
type AgentResult struct {
	Answer        string               `json:"answer"`
	Evidence      []agent.EvidenceItem `json:"evidence"`
	Steps         int                  `json:"steps"`
	LatencyMS     int64                `json:"latency_ms"`
	AnswerRecall  *float64             `json:"answer_recall,omitempty"`
	ItemRecall    *float64             `json:"item_recall,omitempty"`
	ItemPrecision *float64             `json:"item_precision,omitempty"`
	// UnverifiedExtras are answer names outside the gold list that do occur
	// in done chunk text: they may be correct, so they are excluded from
	// ItemPrecision and listed for manual review (spec B4). Extras absent
	// from every chunk are verifiably wrong and count against precision.
	UnverifiedExtras []string `json:"unverified_extras,omitempty"`
	Abstained        *bool    `json:"abstained,omitempty"`
	NeedsReview      bool     `json:"needs_review"`
	EvidenceCited    *bool    `json:"evidence_cited,omitempty"`
}
type QuestionResult struct {
	ID          string         `json:"id"`
	Stratum     string         `json:"stratum"`
	Domain      string         `json:"domain,omitempty"`
	CrossDomain []string       `json:"cross_domain,omitempty"`
	Search      []SearchResult `json:"search,omitempty"`
	Agent       *AgentResult   `json:"agent,omitempty"`
}
type FailedQuestion struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}
type Report struct {
	Schema    int                        `json:"schema"`
	Source    string                     `json:"source"`
	Ks        []int                      `json:"ks"`
	Questions []QuestionResult           `json:"questions"`
	Invalid   map[string]string          `json:"invalid"`
	Failed    []FailedQuestion           `json:"failed"`
	Strata    map[string]map[string]Stat `json:"strata"`
}

// Markdown summarizes aggregate metrics and explicitly lists invalid questions.
func Markdown(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Retrieval evaluation (%s)\n\n%d valid questions; %d invalid; %d failed.\n\n", r.Source, len(r.Questions), len(r.Invalid), len(r.Failed))
	b.WriteString("| Stratum | Metric | Mean | 95% CI | N |\n| --- | --- | ---: | --- | ---: |\n")
	for _, stratum := range sortedKeys(r.Strata) {
		for _, metric := range sortedKeys(r.Strata[stratum]) {
			s := r.Strata[stratum][metric]
			fmt.Fprintf(&b, "| %s | %s | %.3f | [%.3f, %.3f] | %d |\n", stratum, metric, s.Mean, s.Lo, s.Hi, s.N)
		}
	}
	if len(r.Invalid) > 0 {
		b.WriteString("\nInvalid questions:\n\n")
		for _, id := range sortedKeys(r.Invalid) {
			fmt.Fprintf(&b, "- %s: %s\n", id, r.Invalid[id])
		}
	}
	if len(r.Failed) > 0 {
		b.WriteString("\nFailed questions:\n\n")
		for _, q := range r.Failed {
			fmt.Fprintf(&b, "- %s: %s\n", q.ID, q.Error)
		}
	}
	n := 0
	for _, q := range r.Questions {
		if q.Agent != nil && q.Agent.NeedsReview {
			n++
		}
	}
	if n > 0 {
		fmt.Fprintf(&b, "\n%d agent answers need manual review.\n", n)
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func aggregate(r *Report) {
	values := map[string]map[string][]float64{}
	for _, q := range r.Questions {
		if values[q.Stratum] == nil {
			values[q.Stratum] = map[string][]float64{}
		}
		add := func(key string, v float64) { values[q.Stratum][key] = append(values[q.Stratum][key], v) }
		if q.Stratum != "null" {
			for _, s := range q.Search {
				prefix := s.Kind + "/" + s.Variant + "/"
				for k, v := range s.Recall {
					add(prefix+"recall@"+k, v)
				}
				add(prefix+"mrr", s.MRR)
				for k, v := range s.ItemRecall {
					add(prefix+"item_recall@"+k, v)
				}
			}
		}
		if a := q.Agent; a != nil {
			for k, p := range map[string]*float64{"answer_recall": a.AnswerRecall, "item_recall": a.ItemRecall, "item_precision": a.ItemPrecision} {
				if p != nil {
					add("agent/"+k, *p)
				}
			}
			for k, p := range map[string]*bool{"abstained": a.Abstained, "evidence_cited": a.EvidenceCited} {
				if p != nil {
					v := 0.0
					if *p {
						v = 1
					}
					add("agent/"+k, v)
				}
			}
			add("agent/steps", float64(a.Steps))
			add("agent/tool_calls", float64(len(a.Evidence)))
			add("agent/latency_ms", float64(a.LatencyMS))
		}
	}
	for stratum, metrics := range values {
		r.Strata[stratum] = map[string]Stat{}
		for key, vs := range metrics {
			s := Stat{N: len(vs)}
			for _, v := range vs {
				s.Mean += v
			}
			s.Mean /= float64(s.N)
			s.Lo, s.Hi = BootstrapCI(vs, 1000, 1)
			r.Strata[stratum][key] = s
		}
	}
}
