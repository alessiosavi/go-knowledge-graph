// Package eval scores extraction quality against a golden JSONL set:
// strict and lenient entity/relation P/R/F1, judge-scored precision,
// reference-fact coverage (recall proxy) and an optional threshold sweep.
// The report is provider-agnostic: run it with the mock provider in CI and
// with real providers (keys required) for meaningful numbers.
package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunk"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

// GoldenDoc is one golden JSONL record (design §5.6).
type GoldenDoc struct {
	Doc                    string             `json:"doc"`
	Language               string             `json:"language"`
	ExpectedEntities       []ExpectedEntity   `json:"expected_entities"`
	ExpectedRelations      []ExpectedRelation `json:"expected_relations"`
	OptionalReferenceFacts []ExpectedRelation `json:"optional_reference_facts"`
}

// ExpectedEntity is one expected entity.
type ExpectedEntity struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

// ExpectedRelation is one expected relation (head/tail are entity texts).
type ExpectedRelation struct {
	Label string `json:"label"`
	Head  string `json:"head"`
	Tail  string `json:"tail"`
}

// Metrics is a precision/recall/F1 triple with its confusion counts.
type Metrics struct {
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
	TP        int     `json:"tp"`
	FP        int     `json:"fp"`
	FN        int     `json:"fn"`
}

// DocResult is one scored golden document.
type DocResult struct {
	Doc               string  `json:"doc"`
	Language          string  `json:"language"`
	ExpectedEntities  int     `json:"expected_entities"`
	ExtractedEntities int     `json:"extracted_entities"`
	Facts             int     `json:"facts"`
	EntitiesStrict    Metrics `json:"entities_strict"`
	EntitiesLenient   Metrics `json:"entities_lenient"`
	RelationsStrict   Metrics `json:"relations_strict"`
	RelationsLenient  Metrics `json:"relations_lenient"`
	JudgePrecision    float64 `json:"judge_precision"`
	JudgedFacts       int     `json:"judged_facts"`
	JudgeErrors       int     `json:"judge_errors"`
	ReferenceCoverage float64 `json:"reference_coverage"`
}

// Report is the full evaluation output.
type Report struct {
	Provider                 string          `json:"provider"`
	ProviderVersion          string          `json:"provider_version"`
	JudgeModel               string          `json:"judge_model,omitempty"`
	EntitiesThresholds       []extract.Label `json:"entities_thresholds"`
	RelationsThresholds      []extract.Label `json:"relations_thresholds"`
	Samples                  int             `json:"samples"`
	GeneratedAt              string          `json:"generated_at"`
	Docs                     []DocResult     `json:"docs"`
	AggregateEntities        Metrics         `json:"aggregate_entities_lenient"`
	AggregateRelations       Metrics         `json:"aggregate_relations_lenient"`
	AggregateEntitiesStrict  Metrics         `json:"aggregate_entities_strict"`
	AggregateRelationsStrict Metrics         `json:"aggregate_relations_strict"`
	JudgeEnabled             bool            `json:"judge_enabled"`
	JudgedFacts              int             `json:"judged_facts"`
	JudgeErrors              int             `json:"judge_errors"`
	JudgePrecision           float64         `json:"judge_precision"`
	ReferenceCoverage        float64         `json:"reference_coverage"`
	SweepTable               string          `json:"sweep_table,omitempty"`
}

// StrictEntityKey is the exact-match identity (raw label + raw text).
func StrictEntityKey(label, text string) string {
	return label + "|" + text
}

// LenientEntityKey is the typed, normalized-match identity.
func LenientEntityKey(label, text string) string {
	return label + ":" + graph.NormalizeSurface(text)
}

// StrictRelationKey is the exact-match relation identity.
func StrictRelationKey(label, head, tail string) string {
	return label + "|" + head + "|" + tail
}

// LenientRelationKey is the normalized-match relation identity.
func LenientRelationKey(label, head, tail string) string {
	return graph.SanitizeRel(label) + "|" + graph.EntityKey(head) + "|" + graph.EntityKey(tail)
}

// Score computes set-matching P/R/F1 between expected and extracted keys.
// Conventions: empty-vs-empty scores 1; expected-but-nothing-extracted
// still reports precision 1 with recall 0 (no positive predictions — the
// aggregate read for a broken provider is therefore "recall 0", not a
// misleading zero precision); empty-expected with extra output scores 0
// precision.
func Score(expected, got []string) Metrics {
	exp := dedupe(expected)
	ext := dedupe(got)
	m := Metrics{}
	for k := range ext {
		if exp[k] {
			m.TP++
		} else {
			m.FP++
		}
	}
	for k := range exp {
		if !ext[k] {
			m.FN++
		}
	}
	if m.TP+m.FP > 0 {
		m.Precision = float64(m.TP) / float64(m.TP+m.FP)
	} else {
		m.Precision = 1
	}
	if m.TP+m.FN > 0 {
		m.Recall = float64(m.TP) / float64(m.TP+m.FN)
	} else {
		m.Recall = 1
	}
	if m.Precision+m.Recall > 0 {
		m.F1 = 2 * m.Precision * m.Recall / (m.Precision + m.Recall)
	} else {
		m.F1 = 0
	}
	return m
}

// JudgeScoredPrecision scores verdicts as confirmed=1, unclear=0.5,
// refuted=0; no verdicts score 0.
func JudgeScoredPrecision(verdicts []ingest.Verdict) float64 {
	if len(verdicts) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range verdicts {
		switch v.Status {
		case "confirmed":
			sum += 1
		case "unclear":
			sum += 0.5
		}
	}
	return sum / float64(len(verdicts))
}

// Coverage is the fraction of expected reference keys present in got; an
// empty reference set scores 1.
func Coverage(expected, got []string) float64 {
	if len(expected) == 0 {
		return 1
	}
	exp := dedupe(expected)
	ext := dedupe(got)
	found := 0
	for k := range exp {
		if ext[k] {
			found++
		}
	}
	return float64(found) / float64(len(exp))
}

// ParseSweep parses "entity=0.2:0.8:0.1 relation=0.1:0.6:0.1" into both
// threshold axis values. Duplicate axes and malformed numbers are errors;
// each axis is capped so a typo cannot explode into a runaway sweep.
func ParseSweep(spec string) ([]float64, []float64, error) {
	var entities, relations []float64
	seen := map[string]bool{}
	for _, field := range strings.Fields(spec) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return nil, nil, fmt.Errorf("sweep: malformed field %q", field)
		}
		if seen[key] {
			return nil, nil, fmt.Errorf("sweep: axis %q given twice", key)
		}
		seen[key] = true
		values, err := parseRange(value)
		if err != nil {
			return nil, nil, fmt.Errorf("sweep %s: %w", key, err)
		}
		switch key {
		case "entity":
			entities = values
		case "relation":
			relations = values
		default:
			return nil, nil, fmt.Errorf("sweep: unknown axis %q", key)
		}
	}
	if len(entities) == 0 || len(relations) == 0 {
		return nil, nil, errors.New("sweep: both entity and relation ranges are required")
	}
	if len(entities)*len(relations) > maxSweepCombos {
		return nil, nil, fmt.Errorf("sweep: %d combinations exceed the cap of %d", len(entities)*len(relations), maxSweepCombos)
	}
	return entities, relations, nil
}

// maxSweepCombos caps total sweep combinations (each one re-extracts the
// whole golden set).
const maxSweepCombos = 400

func parseRange(spec string) ([]float64, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("range %q needs start:end:step", spec)
	}
	start, err := parseFloat(parts[0])
	if err != nil {
		return nil, err
	}
	end, err := parseFloat(parts[1])
	if err != nil {
		return nil, err
	}
	step, err := parseFloat(parts[2])
	if err != nil {
		return nil, err
	}
	if step <= 0 || start > end {
		return nil, fmt.Errorf("range %q has no progression", spec)
	}
	// Reject explosive ranges BEFORE materializing them: the cap must bound
	// the loop, not just its output. Compare in float space — the float64→int
	// conversion of an out-of-range quotient (or +Inf) is
	// implementation-dependent and can bypass an int cap.
	q := (end - start) / step
	if math.IsNaN(q) || q >= 100 {
		return nil, fmt.Errorf("range %q is too large, want 1..100 values", spec)
	}
	// Walk a bounded counter, not a float accumulation: `v += step` can
	// fail to advance (sub-ULP steps) or walk the 1e-9 overshoot window
	// forever, neither of which q < 100 precludes.
	var out []float64
	steps := int(math.Ceil(q)) + 1
	for i := 0; i <= steps && len(out) < 100; i++ {
		v := start + float64(i)*step
		if v > end+1e-9 {
			break
		}
		r := round3(v)
		if len(out) > 0 && out[len(out)-1] >= r {
			continue // rounding collapsed the step; keep the axis unique
		}
		out = append(out, r)
	}
	if len(out) == 0 || len(out) > 100 {
		return nil, fmt.Errorf("range %q yields %d values, want 1..100", spec, len(out))
	}
	return out, nil
}

func parseFloat(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", s)
	}
	return v, nil
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

// Run evaluates the golden set with the given provider and optional judge.
// preset selects a label preset from cfg.Labels.PresetsDir; empty uses defaults.
// sweepSpec enables the threshold sweep table; out, when non-empty, writes
// "<out>.md" and "<out>.json".
func Run(ctx context.Context, cfg config.Config, prov extract.Provider, j ingest.FactJudge, goldenPath, preset, sweepSpec, out string) (Report, error) {
	entities, relations, err := ingest.LoadPreset(cfg.Labels.PresetsDir, preset)
	if err != nil {
		return Report{}, err
	}
	golden, err := loadGolden(goldenPath)
	if err != nil {
		return Report{}, err
	}

	report := Report{
		Provider:            prov.Name(),
		ProviderVersion:     prov.Version(),
		EntitiesThresholds:  entities,
		RelationsThresholds: relations,
		Samples:             len(golden),
		GeneratedAt:         time.Now().UTC().Format(time.RFC3339),
	}
	if m, ok := j.(interface{ Model() string }); ok {
		report.JudgeModel = m.Model()
	}
	report.JudgeEnabled = j != nil

	var judgeSum, coverageSum float64
	judgedDocs := 0
	for _, g := range golden {
		res, err := evaluateDoc(ctx, cfg, prov, j, entities, relations, g)
		if err != nil {
			return Report{}, err
		}
		report.Docs = append(report.Docs, res)
		report.JudgedFacts += res.JudgedFacts
		report.JudgeErrors += res.JudgeErrors
		if res.JudgedFacts > 0 {
			judgeSum += res.JudgePrecision
			judgedDocs++
		}
		coverageSum += res.ReferenceCoverage
	}
	report.AggregateEntities = meanMetrics(report.Docs, func(d DocResult) Metrics { return d.EntitiesLenient })
	report.AggregateRelations = meanMetrics(report.Docs, func(d DocResult) Metrics { return d.RelationsLenient })
	report.AggregateEntitiesStrict = meanMetrics(report.Docs, func(d DocResult) Metrics { return d.EntitiesStrict })
	report.AggregateRelationsStrict = meanMetrics(report.Docs, func(d DocResult) Metrics { return d.RelationsStrict })
	if judgedDocs > 0 {
		report.JudgePrecision = judgeSum / float64(judgedDocs)
	}
	if len(report.Docs) > 0 {
		report.ReferenceCoverage = coverageSum / float64(len(report.Docs))
	}

	if sweepSpec != "" {
		table, err := runSweep(ctx, cfg, prov, entities, relations, golden, sweepSpec)
		if err != nil {
			return Report{}, err
		}
		report.SweepTable = table
	}
	if out != "" {
		if err := writeReport(out, report); err != nil {
			return Report{}, err
		}
	}
	return report, nil
}

func loadGolden(path string) ([]GoldenDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read golden %s: %w", path, err)
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	var docs []GoldenDoc
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var g GoldenDoc
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields() // a mistyped fixture key must fail, not score 0
		if err := dec.Decode(&g); err != nil {
			return nil, fmt.Errorf("eval: golden %s line %d: %w", path, i+1, err)
		}
		if strings.TrimSpace(g.Doc) == "" {
			return nil, fmt.Errorf("eval: golden %s line %d: empty doc", path, i+1)
		}
		docs = append(docs, g)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("eval: golden %s has no records", path)
	}
	return docs, nil
}

type extractedFact struct {
	headKey, relKey, tailKey string
	headText, rel, tailText  string
	evidence                 string
	score                    float64
}

// evaluateDoc scores one golden document. Note: unlike the production
// pipeline, eval deliberately does NOT apply the MaxChunks admission cap —
// the harness scores the whole document even where ingest would refuse it.
func evaluateDoc(ctx context.Context, cfg config.Config, prov extract.Provider, j ingest.FactJudge, entities, relations []extract.Label, g GoldenDoc) (DocResult, error) {
	spans := chunk.Split(g.Doc, chunk.Options{
		TargetWords:      cfg.Ingest.ChunkWords,
		OverlapSentences: cfg.Ingest.OverlapSentences,
	})
	req := extract.Request{Entities: entities, Relations: relations, Chunks: make([]extract.RequestChunk, 0, len(spans))}
	textByID := make(map[string]string, len(spans))
	for _, s := range spans {
		id := fmt.Sprintf("eval-c%d", s.Seq)
		textByID[id] = s.Text
		req.Chunks = append(req.Chunks, extract.RequestChunk{ID: id, Text: s.Text})
	}
	res, err := prov.Extract(ctx, req)
	if err != nil {
		return DocResult{}, err
	}

	var entStrict, entLenient, relStrict, relLenient []string
	facts := map[string]extractedFact{}
	for _, cr := range res {
		for _, m := range cr.Mentions {
			entStrict = append(entStrict, StrictEntityKey(m.Label, m.Text))
			entLenient = append(entLenient, LenientEntityKey(m.Label, m.Text))
		}
		for _, cand := range cr.Candidates {
			if cand.Head < 0 || cand.Head >= len(cr.Mentions) || cand.Tail < 0 || cand.Tail >= len(cr.Mentions) {
				continue
			}
			hm, tm := cr.Mentions[cand.Head], cr.Mentions[cand.Tail]
			relStrict = append(relStrict, StrictRelationKey(cand.Label, hm.Text, tm.Text))
			lk := LenientRelationKey(cand.Label, hm.Text, tm.Text)
			relLenient = append(relLenient, lk)
			if _, ok := facts[lk]; !ok {
				facts[lk] = extractedFact{
					headKey:  LenientEntityKey(hm.Label, hm.Text),
					rel:      cand.Label,
					tailKey:  LenientEntityKey(tm.Label, tm.Text),
					headText: hm.Text, tailText: tm.Text,
					evidence: ingest.EvidenceSentence(textByID[cr.ChunkID], hm.Start, hm.End),
					score:    cand.Score,
				}
			}
		}
	}

	var expEntStrict, expEntLenient []string
	for _, e := range g.ExpectedEntities {
		expEntStrict = append(expEntStrict, StrictEntityKey(e.Label, e.Text))
		expEntLenient = append(expEntLenient, LenientEntityKey(e.Label, e.Text))
	}
	var expRelStrict, expRelLenient, refLenient []string
	for _, r := range g.ExpectedRelations {
		expRelStrict = append(expRelStrict, StrictRelationKey(r.Label, r.Head, r.Tail))
		expRelLenient = append(expRelLenient, LenientRelationKey(r.Label, r.Head, r.Tail))
	}
	for _, r := range g.OptionalReferenceFacts {
		refLenient = append(refLenient, LenientRelationKey(r.Label, r.Head, r.Tail))
	}

	out := DocResult{
		Doc:               truncate(g.Doc, 80),
		Language:          g.Language,
		ExpectedEntities:  len(g.ExpectedEntities),
		ExtractedEntities: len(dedupe(entLenient)),
		Facts:             len(facts),
		EntitiesStrict:    Score(expEntStrict, entStrict),
		EntitiesLenient:   Score(expEntLenient, entLenient),
		RelationsStrict:   Score(expRelStrict, relStrict),
		RelationsLenient:  Score(expRelLenient, relLenient),
		ReferenceCoverage: Coverage(refLenient, relLenient),
	}

	if j != nil && len(facts) > 0 {
		keys := make([]string, 0, len(facts))
		for k := range facts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var verdicts []ingest.Verdict
		for _, k := range keys {
			f := facts[k]
			verdict, err := j.VerifyFact(ctx, ingest.FactInput{
				Head: f.headText, Rel: f.rel, Tail: f.tailText,
				Evidence: f.evidence,
			})
			if err != nil {
				out.JudgeErrors++
				continue
			}
			verdicts = append(verdicts, verdict)
		}
		out.JudgedFacts = len(verdicts)
		out.JudgePrecision = JudgeScoredPrecision(verdicts)
	}
	return out, nil
}

func extractDoc(ctx context.Context, cfg config.Config, prov extract.Provider, entities, relations []extract.Label, text string) ([]extract.ChunkResult, error) {
	spans := chunk.Split(text, chunk.Options{
		TargetWords:      cfg.Ingest.ChunkWords,
		OverlapSentences: cfg.Ingest.OverlapSentences,
	})
	req := extract.Request{Entities: entities, Relations: relations,
		Chunks: make([]extract.RequestChunk, 0, len(spans))}
	for _, s := range spans {
		req.Chunks = append(req.Chunks, extract.RequestChunk{ID: fmt.Sprintf("eval-c%d", s.Seq), Text: s.Text})
	}
	return prov.Extract(ctx, req)
}

func runSweep(ctx context.Context, cfg config.Config, prov extract.Provider, entities, relations []extract.Label, golden []GoldenDoc, spec string) (string, error) {
	eValues, rValues, err := ParseSweep(spec)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("| entity | relation | entity F1 (strict) | relation F1 (strict) |\n")
	b.WriteString("| --- | --- | --- | --- |")
	for _, e := range eValues {
		for _, r := range rValues {
			eLabels := withThreshold(entities, e)
			rLabels := withThreshold(relations, r)
			var entF1, relF1 float64
			for _, g := range golden {
				res, err := extractDoc(ctx, cfg, prov, eLabels, rLabels, g.Doc)
				if err != nil {
					return "", err
				}
				var gotEnt, gotRel []string
				for _, cr := range res {
					for _, m := range cr.Mentions {
						gotEnt = append(gotEnt, StrictEntityKey(m.Label, m.Text))
					}
					for _, cand := range cr.Candidates {
						if cand.Head < 0 || cand.Head >= len(cr.Mentions) || cand.Tail < 0 || cand.Tail >= len(cr.Mentions) {
							continue
						}
						gotRel = append(gotRel, StrictRelationKey(cand.Label, cr.Mentions[cand.Head].Text, cr.Mentions[cand.Tail].Text))
					}
				}
				var expEnt, expRel []string
				for _, x := range g.ExpectedEntities {
					expEnt = append(expEnt, StrictEntityKey(x.Label, x.Text))
				}
				for _, x := range g.ExpectedRelations {
					expRel = append(expRel, StrictRelationKey(x.Label, x.Head, x.Tail))
				}
				entF1 += Score(expEnt, gotEnt).F1
				relF1 += Score(expRel, gotRel).F1
			}
			fmt.Fprintf(&b, "\n| %.2f | %.2f | %.3f | %.3f |", e, r,
				entF1/float64(len(golden)), relF1/float64(len(golden)))
		}
	}
	return b.String(), nil
}

func withThreshold(labels []extract.Label, threshold float64) []extract.Label {
	out := make([]extract.Label, len(labels))
	for i, l := range labels {
		out[i] = l
		out[i].Threshold = threshold
	}
	return out
}

func writeReport(prefix string, report Report) error {
	md := renderMarkdown(report)
	if err := os.WriteFile(prefix+".md", []byte(md), 0o644); err != nil {
		return fmt.Errorf("eval: write report: %w", err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: encode report: %w", err)
	}
	if err := os.WriteFile(prefix+".json", append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("eval: write report: %w", err)
	}
	return nil
}

func renderMarkdown(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Extraction evaluation\n\n")
	fmt.Fprintf(&b, "- Provider: `%s` (`%s`)\n", r.Provider, r.ProviderVersion)
	if r.JudgeModel != "" {
		fmt.Fprintf(&b, "- Judge: `%s`\n", r.JudgeModel)
	}
	fmt.Fprintf(&b, "- Samples: %d · generated %s\n\n", r.Samples, r.GeneratedAt)
	fmt.Fprintf(&b, "## Aggregate\n\n")
	fmt.Fprintf(&b, "| metric | match | P | R | F1 |\n| --- | --- | --- | --- | --- |\n")
	fmt.Fprintf(&b, "| entities | strict | %.3f | %.3f | %.3f |\n", r.AggregateEntitiesStrict.Precision, r.AggregateEntitiesStrict.Recall, r.AggregateEntitiesStrict.F1)
	fmt.Fprintf(&b, "| entities | lenient | %.3f | %.3f | %.3f |\n", r.AggregateEntities.Precision, r.AggregateEntities.Recall, r.AggregateEntities.F1)
	fmt.Fprintf(&b, "| relations | strict | %.3f | %.3f | %.3f |\n", r.AggregateRelationsStrict.Precision, r.AggregateRelationsStrict.Recall, r.AggregateRelationsStrict.F1)
	fmt.Fprintf(&b, "| relations | lenient | %.3f | %.3f | %.3f |\n\n", r.AggregateRelations.Precision, r.AggregateRelations.Recall, r.AggregateRelations.F1)
	if r.JudgeEnabled {
		fmt.Fprintf(&b, "- Judge-scored precision: %.3f (judged %d, errors %d)\n", r.JudgePrecision, r.JudgedFacts, r.JudgeErrors)
	} else {
		fmt.Fprintf(&b, "- Judge: not enabled\n")
	}
	fmt.Fprintf(&b, "- Reference-fact coverage: %.3f\n\n", r.ReferenceCoverage)
	fmt.Fprintf(&b, "## Per document\n\n")
	fmt.Fprintf(&b, "| doc | lang | ent strict F1 | ent lenient F1 | rel strict F1 | rel lenient F1 | judge P | coverage |\n")
	fmt.Fprintf(&b, "| --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, d := range r.Docs {
		fmt.Fprintf(&b, "\n| %s | %s | %.3f | %.3f | %.3f | %.3f | %.3f | %.3f |",
			markdownCell(d.Doc), markdownCell(d.Language), d.EntitiesStrict.F1, d.EntitiesLenient.F1,
			d.RelationsStrict.F1, d.RelationsLenient.F1, d.JudgePrecision, d.ReferenceCoverage)
	}
	if r.SweepTable != "" {
		fmt.Fprintf(&b, "\n\n## Threshold sweep\n\n%s", r.SweepTable)
	}
	b.WriteString("\n")
	return b.String()
}

// markdownCell makes a value safe inside a markdown table cell.
func markdownCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

func meanMetrics(docs []DocResult, pick func(DocResult) Metrics) Metrics {
	if len(docs) == 0 {
		return Metrics{}
	}
	var sum Metrics
	for _, d := range docs {
		m := pick(d)
		sum.Precision += m.Precision
		sum.Recall += m.Recall
		sum.F1 += m.F1
		sum.TP += m.TP
		sum.FP += m.FP
		sum.FN += m.FN
	}
	n := float64(len(docs))
	return Metrics{
		Precision: sum.Precision / n, Recall: sum.Recall / n, F1: sum.F1 / n,
		TP: sum.TP, FP: sum.FP, FN: sum.FN,
	}
}

func dedupe(keys []string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
