package reteval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/agent"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

type Asker interface {
	Ask(ctx context.Context, req agent.AskRequest) (agent.AskResponse, error)
}
type Deps struct {
	Store    *graph.Store
	Embedder search.QueryEmbedder
	Agent    Asker
}
type Options struct {
	Agent      bool
	ReplayPath string
	Ks         []int
}

// Run resolves gold against the current store, evaluates search, and optionally
// scores agent answers. Replay never calls the agent provider.
func Run(ctx context.Context, d Deps, set QuestionSet, opt Options) (Report, error) {
	set, err := prepare(set)
	if err != nil {
		return Report{}, err
	}
	if opt.Agent || opt.ReplayPath != "" {
		var missing []string
		for _, q := range set.Questions {
			if (q.Stratum == "single" || q.Stratum == "multi") && len(q.Answer) == 0 {
				missing = append(missing, q.ID)
			}
		}
		if len(missing) > 0 {
			return Report{}, fmt.Errorf("reteval: answer is required for single/multi questions: %s", strings.Join(missing, ", "))
		}
	}
	if d.Store == nil {
		return Report{}, errors.New("reteval: store is required")
	}
	ks := slices.Clone(opt.Ks)
	if len(ks) == 0 {
		ks = []int{5, 10, 20}
	}
	slices.Sort(ks)
	ks = slices.Compact(ks)
	if ks[0] <= 0 {
		return Report{}, errors.New("reteval: k must be positive")
	}
	r := Report{Schema: 1, Source: "live", Ks: ks, Questions: []QuestionResult{}, Invalid: map[string]string{}, Failed: []FailedQuestion{}, Strata: map[string]map[string]Stat{}}
	var replay map[string]*AgentResult
	if opt.ReplayPath != "" {
		replay, err = loadReplay(opt.ReplayPath)
		if err != nil {
			return Report{}, err
		}
		r.Source = "replay"
	} else if opt.Agent && d.Agent == nil {
		return Report{}, errors.New("reteval: --agent requires an agent")
	}
	for _, q := range set.Questions {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		g, err := resolveGold(ctx, d.Store, q)
		if err != nil {
			return Report{}, fmt.Errorf("reteval: question %s: %w", q.ID, err)
		}
		if g.invalid != "" {
			r.Invalid[q.ID] = g.invalid
			continue
		}
		if q.Stratum == "null" {
			for _, term := range q.Absent {
				hits, err := search.Query(ctx, d.Store, nil, term, "chunk", 1)
				if err != nil {
					return Report{}, err
				}
				if len(hits) > 0 {
					g.invalid = fmt.Sprintf("absent term has text-search hits: %q", term)
					break
				}
			}
			if g.invalid != "" {
				r.Invalid[q.ID] = g.invalid
				continue
			}
		}
		qr := QuestionResult{ID: q.ID, Stratum: q.Stratum, Domain: q.Domain, CrossDomain: g.crossDomain}
		if q.Stratum != "null" {
			variants := []struct{ name, text string }{{"question", q.Question}}
			if strings.TrimSpace(q.Keywords) != "" {
				variants = append(variants, struct{ name, text string }{"keywords", q.Keywords})
			}
			for _, v := range variants {
				for _, kind := range q.Kinds {
					hits, err := search.Query(ctx, d.Store, d.Embedder, v.text, kind, ks[len(ks)-1])
					if err != nil {
						return Report{}, fmt.Errorf("reteval: %s/%s/%s: %w", q.ID, kind, v.name, err)
					}
					ranked := make([]string, len(hits))
					for i, h := range hits {
						if h.Kind == "chunk" {
							ranked[i] = h.ID
						}
					} // Entity hits still occupy ranks.
					s := SearchResult{Kind: kind, Variant: v.name, Ranks: anchorRanks(g.anchors, ranked), Recall: map[string]float64{}, MRR: MRR(g.anchors, ranked)}
					if q.Stratum == "global" {
						s.ItemRecall = map[string]float64{}
					}
					for _, k := range ks {
						key := strconv.Itoa(k)
						s.Recall[key] = RecallAtK(g.anchors, ranked, k)
						if s.ItemRecall != nil {
							s.ItemRecall[key] = ItemRecallAtK(g.items, ranked, k)
						}
					}
					qr.Search = append(qr.Search, s)
				}
			}
		}
		var a *AgentResult
		if replay != nil {
			stored := replay[q.ID]
			if stored == nil {
				return Report{}, fmt.Errorf("reteval: replay has no agent answer for %q", q.ID)
			}
			a = &AgentResult{Answer: stored.Answer, Evidence: stored.Evidence, Steps: stored.Steps, LatencyMS: stored.LatencyMS}
		} else if opt.Agent {
			start := time.Now()
			resp, err := d.Agent.Ask(ctx, agent.AskRequest{Question: q.Question, Domain: q.Domain})
			if err != nil {
				r.Failed = append(r.Failed, FailedQuestion{ID: q.ID, Error: err.Error()})
				continue
			}
			a = &AgentResult{Answer: resp.Answer, Evidence: resp.Evidence, Steps: resp.Steps, LatencyMS: time.Since(start).Milliseconds()}
		}
		if a != nil {
			if err := scoreAgent(ctx, d.Store, q, g, a); err != nil {
				return Report{}, err
			}
			qr.Agent = a
		}
		r.Questions = append(r.Questions, qr)
	}
	aggregate(&r)
	if len(r.Questions) == 0 {
		return r, errors.New("reteval: no question scored")
	}
	return r, nil
}

func loadReplay(path string) (map[string]*AgentResult, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reteval: replay: %w", err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("reteval: replay: %w", err)
	}
	if r.Schema != 1 {
		return nil, errors.New("reteval: replay schema must be 1")
	}
	out := map[string]*AgentResult{}
	seen := map[string]bool{}
	for _, q := range r.Questions {
		if seen[q.ID] {
			return nil, fmt.Errorf("reteval: duplicate replay id %q", q.ID)
		}
		seen[q.ID] = true
		out[q.ID] = q.Agent
	}
	return out, nil
}

func scoreAgent(ctx context.Context, st *graph.Store, q Question, g resolvedGold, a *AgentResult) error {
	a.NeedsReview = Abstained(a.Answer)
	if len(q.Answer) > 0 {
		v := AnswerPartRecall(q.Answer, a.Answer)
		a.AnswerRecall = &v
	}
	if len(g.anchors) > 0 {
		cited := false
		for _, ids := range g.anchors {
			for _, id := range ids {
				for _, ev := range a.Evidence {
					if evidenceCitesChunk(ev.Result, id) {
						cited = true
					}
				}
			}
		}
		a.EvidenceCited = &cited
	}
	if q.Stratum == "null" {
		v := Abstained(a.Answer)
		a.Abstained = &v
	}
	if q.Stratum != "global" {
		return nil
	}
	answer := Fold(a.Answer)
	matched := 0
	for _, item := range q.AnswerItems {
		if named(item.Names, answer) {
			matched++
		}
	}
	recall := float64(matched) / float64(len(q.AnswerItems))
	a.ItemRecall = &recall
	// Deterministic provisional item extraction: Markdown list labels and
	// bold names. Free-form prose requires manual review of precision.
	a.NeedsReview = true
	extra := []string{}
	seen := map[string]bool{}
	for _, name := range answerNames(a.Answer) {
		f := Fold(name)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		known := false
		for _, item := range q.AnswerItems {
			for _, alias := range item.Names {
				if f == Fold(alias) {
					known = true
				}
			}
		}
		if !known {
			extra = append(extra, name)
		}
	}
	found := make([]bool, len(extra))
	if len(extra) > 0 {
		if err := st.DoneChunkTexts(ctx, q.Domain, func(_ string, text string) error {
			folded := Fold(text)
			for i, name := range extra {
				if strings.Contains(folded, Fold(name)) {
					found[i] = true
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	absent := 0
	for i, name := range extra {
		if found[i] {
			a.UnverifiedExtras = append(a.UnverifiedExtras, name)
		} else {
			absent++
		}
	}
	precision := 1.0
	if matched+absent > 0 {
		precision = float64(matched) / float64(matched+absent)
	} else if matched == 0 {
		precision = 0
	}
	a.ItemPrecision = &precision
	return nil
}

// Tool results nest chunk identities in hits, facts, edges and Cypher rows.
// Text fields mentioning an id do not establish a citation.
func evidenceCitesChunk(raw json.RawMessage, id string) bool {
	var item struct {
		ID      string `json:"id"`
		ChunkID string `json:"chunk_id"`
	}
	if json.Unmarshal(raw, &item) == nil && id != "" && (item.ID == id || item.ChunkID == id) {
		return true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil {
		for _, child := range fields {
			if evidenceCitesChunk(child, id) {
				return true
			}
		}
		return false
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		for _, child := range items {
			if evidenceCitesChunk(child, id) {
				return true
			}
		}
	}
	return false
}

var listLabel = regexp.MustCompile(`^\s*(?:[-+*]|[0-9]+[.)])\s+(.+)$`)
var boldName = regexp.MustCompile(`\*\*([^*]+)\*\*`)

func answerNames(answer string) []string {
	var out []string
	for _, line := range strings.Split(answer, "\n") {
		bold := boldName.FindAllStringSubmatch(line, -1)
		if len(bold) > 0 {
			for _, m := range bold {
				out = append(out, m[1])
			}
			continue
		}
		if m := listLabel.FindStringSubmatch(line); m != nil {
			name := m[1]
			for _, sep := range []string{":", " — ", " – ", " - ", " ["} {
				name, _, _ = strings.Cut(name, sep)
			}
			out = append(out, strings.Trim(strings.TrimSpace(name), "`*_ ."))
		}
	}
	return out
}
