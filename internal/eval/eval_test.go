package eval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func TestScoreStrictVsLenient(t *testing.T) {
	strict := Score(
		[]string{StrictEntityKey("person", "Città")},
		[]string{StrictEntityKey("person", "città")},
	)
	if strict.F1 != 0 {
		t.Errorf("strict F1 = %v, want 0 (case differs)", strict.F1)
	}
	lenient := Score(
		[]string{LenientEntityKey("person", "Città")},
		[]string{LenientEntityKey("person", "città")},
	)
	if lenient.F1 != 1 || lenient.Precision != 1 || lenient.Recall != 1 {
		t.Errorf("lenient metrics = %+v, want perfect match", lenient)
	}

	// Partial overlap.
	m := Score([]string{"a", "b", "c", "d"}, []string{"b", "c", "x"})
	if m.TP != 2 || m.FP != 1 || m.FN != 2 {
		t.Errorf("counts = %+v, want TP=2 FP=1 FN=2", m)
	}
	if m.Precision != 2.0/3.0 || m.Recall != 0.5 {
		t.Errorf("P/R = %v/%v", m.Precision, m.Recall)
	}
}

func TestLenientKeys(t *testing.T) {
	if got := LenientEntityKey("product", "`Widget’s`"); got != "product:widget" {
		t.Errorf("LenientEntityKey = %q, want product:widget", got)
	}
	m := Score([]string{LenientEntityKey("product", "Widget")}, []string{LenientEntityKey("organization", "Widget")})
	if m.F1 != 0 {
		t.Errorf("different labels scored %+v, want no match", m)
	}
	if got := LenientRelationKey("Works At", "`Ada`", "Lab’s"); got != "works_at|ada|lab" {
		t.Errorf("LenientRelationKey = %q, want works_at|ada|lab", got)
	}
}

func TestJudgeScoredPrecisionCounts(t *testing.T) {
	verdicts := []ingest.Verdict{
		{Status: "confirmed"}, {Status: "confirmed"}, {Status: "confirmed"},
		{Status: "refuted"},
		{Status: "unclear"},
	}
	if got := JudgeScoredPrecision(verdicts); got != 0.7 {
		t.Errorf("judge precision = %v, want 0.7", got)
	}
	if got := JudgeScoredPrecision(nil); got != 0 {
		t.Errorf("judge precision with no verdicts = %v, want 0", got)
	}
}

func TestReferenceFactCoverage(t *testing.T) {
	expected := []string{"a", "b", "c", "d"}
	got := []string{"a", "c", "z"}
	if cov := Coverage(expected, got); cov != 0.5 {
		t.Errorf("coverage = %v, want 0.5", cov)
	}
	if cov := Coverage(nil, got); cov != 1 {
		t.Errorf("coverage with no references = %v, want 1", cov)
	}
}

func TestParseSweep(t *testing.T) {
	entities, relations, err := ParseSweep("entity=0.2:0.6:0.2 relation=0.1:0.3:0.2")
	if err != nil {
		t.Fatalf("ParseSweep: %v", err)
	}
	if len(entities) != 3 || entities[0] != 0.2 || entities[2] != 0.6 {
		t.Errorf("entities = %v", entities)
	}
	if len(relations) != 2 || relations[0] != 0.1 || relations[1] != 0.3 {
		t.Errorf("relations = %v", relations)
	}
	for _, bad := range []string{
		"entity=abc",
		"entity=0.6:0.2:0.2",
		"relation=0.1:0.3:0.2",
		"entity=0.2:0.6:0",
		"entity=0.2abc:0.6:0.2",                 // trailing garbage
		"entity=0.2:0.6:0.2 entity=0.5:0.5:0.5", // duplicate axis
		"entity=0:1:0.001 relation=0:1:0.001",   // combo cap
	} {
		if _, _, err := ParseSweep(bad); err == nil {
			t.Errorf("ParseSweep(%q) = nil error, want failure", bad)
		}
	}
	// Rounding collapses sub-milli steps; keep the axis unique.
	vals, _, err := ParseSweep("entity=0.0004:0.002:0.0004 relation=0.1:0.1:0.1")
	if err != nil {
		t.Fatalf("ParseSweep(sub-milli): %v", err)
	}
	for i := 1; i < len(vals); i++ {
		if vals[i] <= vals[i-1] {
			t.Errorf("axis not strictly increasing: %v", vals)
		}
	}
}

func TestScoreDocumentsVacuousPrecision(t *testing.T) {
	m := Score([]string{"x"}, nil)
	if m.Precision != 1 || m.Recall != 0 || m.F1 != 0 {
		t.Errorf("metrics = %+v, want documented convention P=1 R=0 F1=0", m)
	}
}

// recordingProvider captures each request.
type recordingProvider struct{ calls []extract.Request }

func (p *recordingProvider) Name() string    { return "recording" }
func (p *recordingProvider) Version() string { return "recording:1" }
func (p *recordingProvider) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	p.calls = append(p.calls, req)
	return nil, nil
}

func TestSweepSendsAxisThresholds(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	golden := writeGolden(t, sampleDocs())
	prov := &recordingProvider{}
	if _, err := Run(context.Background(), cfg, prov, nil, golden, "", "entity=0.7:0.7:0.1 relation=0.3:0.3:0.1", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sawDefault, sawSweep bool
	for _, call := range prov.calls {
		if len(call.Entities) == 0 || len(call.Relations) == 0 {
			continue
		}
		if call.Entities[0].Threshold == 0.4 && call.Relations[0].Threshold == 0.25 {
			sawDefault = true
		}
		if call.Entities[0].Threshold == 0.7 && call.Relations[0].Threshold == 0.3 {
			sawSweep = true
		}
	}
	if !sawDefault || !sawSweep {
		t.Errorf("thresholds per axis not observed: default=%v sweep=%v", sawDefault, sawSweep)
	}
}

func TestEvalUsesPreset(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Labels.PresetsDir = t.TempDir()
	preset := "entities:\n  - name: service\n    text: software service\n    threshold: 0.35\nrelations:\n  - name: calls\n    text: calls service\n    threshold: 0.2\n"
	if err := os.WriteFile(filepath.Join(cfg.Labels.PresetsDir, "custom.yaml"), []byte(preset), 0o600); err != nil {
		t.Fatal(err)
	}
	golden := writeGolden(t, []map[string]any{{
		"doc": "Gateway calls Catalog",
		"expected_entities": []map[string]string{
			{"label": "service", "text": "Gateway"}, {"label": "service", "text": "Catalog"},
		},
		"expected_relations": []map[string]string{{"label": "calls", "head": "Gateway", "tail": "Catalog"}},
	}})
	prov := &extract.Mock{}
	out := filepath.Join(t.TempDir(), "report")
	report, err := Run(context.Background(), cfg, prov, nil, golden, "custom", "", out)
	if err != nil {
		t.Fatal(err)
	}
	if prov.Calls() != 1 || len(report.Docs) != 1 || report.AggregateEntitiesStrict.F1 != 1 || report.AggregateRelationsStrict.F1 != 1 {
		t.Fatalf("preset extraction: calls=%d report=%+v, want service/calls perfect match", prov.Calls(), report)
	}
	if len(report.EntitiesThresholds) != 1 || report.EntitiesThresholds[0].Name != "service" ||
		len(report.RelationsThresholds) != 1 || report.RelationsThresholds[0].Name != "calls" {
		t.Fatalf("report labels = %+v / %+v", report.EntitiesThresholds, report.RelationsThresholds)
	}
	data, err := os.ReadFile(out + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Preset    string                        `json:"preset"`
		Entities  []struct{ Name, Text string } `json:"entities_thresholds"`
		Relations []struct{ Name, Text string } `json:"relations_thresholds"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Preset != "custom" || len(saved.Entities) != 1 || len(saved.Relations) != 1 {
		t.Fatalf("report preset/thresholds missing: %s", data)
	}
	if saved.Entities[0].Name != "service" || saved.Entities[0].Text != "software service" ||
		saved.Relations[0].Name != "calls" || saved.Relations[0].Text != "calls service" {
		t.Errorf("report label names/text missing: %s", data)
	}

	for _, tc := range []struct{ name, content string }{
		{"nope", ""},
		{"broken", "entities: ["},
		{"invalid", "entities:\n  - name: service\n    scope: global\n"},
		{"ambiguous", "relations:\n  - name: calls\n    text: uses\n  - name: uses\n"},
		{"../escape", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.content != "" {
				if err := os.WriteFile(filepath.Join(cfg.Labels.PresetsDir, tc.name+".yaml"), []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			prov := &extract.Mock{}
			_, err := Run(context.Background(), cfg, prov, nil, golden, tc.name, "", "")
			if err == nil || !strings.Contains(err.Error(), tc.name) || prov.Calls() != 0 {
				t.Fatalf("invalid preset: error=%v calls=%d, want preset error before extraction", err, prov.Calls())
			}
		})
	}

	// Sweeps must keep the preset's text and identity metadata.
	provRequests := &recordingProvider{}
	_, err = Run(context.Background(), cfg, provRequests, nil, golden, "custom", "entity=0.7:0.7:0.1 relation=0.3:0.3:0.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(provRequests.calls) != 2 {
		t.Fatalf("calls = %d, want baseline and sweep", len(provRequests.calls))
	}
	for _, req := range provRequests.calls {
		if len(req.Entities) != 1 || req.Entities[0].Text != "software service" ||
			len(req.Relations) != 1 || req.Relations[0].Text != "calls service" {
			t.Errorf("preset metadata lost: %+v", req)
		}
	}
	if req := provRequests.calls[1]; req.Entities[0].Threshold != 0.7 || req.Relations[0].Threshold != 0.3 {
		t.Errorf("sweep thresholds = %+v / %+v", req.Entities, req.Relations)
	}
}

// modelJudge is a stub FactJudge exposing a model name.
type modelJudge struct {
	calls []ingest.FactInput
	err   error
}

func (m *modelJudge) Model() string { return "stub-judge" }
func (m *modelJudge) VerifyFact(_ context.Context, in ingest.FactInput) (ingest.Verdict, error) {
	m.calls = append(m.calls, in)
	if m.err != nil {
		return ingest.Verdict{}, m.err
	}
	return ingest.Verdict{Status: "confirmed", Confidence: 0.9}, nil
}

func TestRunJudgeMetadataAndAccounting(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	golden := writeGolden(t, sampleDocs())

	j := &modelJudge{}
	report, err := Run(context.Background(), cfg, &extract.Mock{}, j, golden, "", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.JudgeModel != "stub-judge" || !report.JudgeEnabled {
		t.Errorf("judge metadata = %q enabled=%v", report.JudgeModel, report.JudgeEnabled)
	}
	if report.JudgedFacts == 0 || report.JudgeErrors != 0 || report.JudgePrecision != 1 {
		t.Errorf("judge accounting = judged %d errors %d precision %v", report.JudgedFacts, report.JudgeErrors, report.JudgePrecision)
	}

	// Deterministic judge call order across runs.
	first := append([]ingest.FactInput(nil), j.calls...)
	if _, err := Run(context.Background(), cfg, &extract.Mock{}, j, golden, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if j.calls[i] != first[i] {
			t.Fatalf("judge call order not deterministic at %d", i)
		}
	}

	// A failing judge is accounted, never scored as genuine zeros.
	failing := &modelJudge{err: errors.New("down")}
	report, err = Run(context.Background(), cfg, &extract.Mock{}, failing, golden, "", "", "")
	if err != nil {
		t.Fatalf("Run with failing judge: %v", err)
	}
	if report.JudgedFacts != 0 || report.JudgeErrors == 0 {
		t.Errorf("failing judge accounting = judged %d errors %d", report.JudgedFacts, report.JudgeErrors)
	}
}

func TestGoldenLoaderStrictAndBOM(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "typo.jsonl")
	if err := os.WriteFile(bad, []byte(`{"doc":"text","expected_entites":[]}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGolden(bad); err == nil {
		t.Error("mistyped fixture key: want error")
	}
	bom := filepath.Join(dir, "bom.jsonl")
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"doc":"BOM document text.","language":"en","expected_entities":[],"expected_relations":[],"optional_reference_facts":[]}`+"\n")...)
	if err := os.WriteFile(bom, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGolden(bom); err != nil {
		t.Errorf("BOM-prefixed golden: %v", err)
	}
}

func TestMarkdownEscapesCells(t *testing.T) {
	md := renderMarkdown(Report{
		Provider: "p", ProviderVersion: "v",
		Docs: []DocResult{{Doc: "Alpha | beta\ngamma", Language: "en"}},
	})
	if strings.Contains(md, "Alpha | beta") {
		t.Error("pipe not escaped in a markdown cell")
	}
	if !strings.Contains(md, `Alpha \| beta gamma`) {
		t.Errorf("escaped cell missing:\n%s", md)
	}
}

func writeGolden(t *testing.T, docs []map[string]any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "golden.jsonl")
	var b strings.Builder
	for _, d := range docs {
		line, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sampleDocs() []map[string]any {
	return []map[string]any{
		{
			"doc":      "Ada Lovelace wrote notes about engines. Ada Lovelace lived in London.",
			"language": "en",
			"expected_entities": []map[string]string{
				{"label": "person", "text": "Ada"},
				{"label": "person", "text": "London."},
			},
			"expected_relations": []map[string]string{
				{"label": "relates", "head": "Ada", "tail": "engines."},
			},
			"optional_reference_facts": []map[string]string{
				{"label": "relates", "head": "Ada", "tail": "London."},
			},
		},
		{
			"doc":      "La città perché è bella. La città perché è viva.",
			"language": "it",
			"expected_entities": []map[string]string{
				{"label": "person", "text": "La"},
			},
			"expected_relations":       []map[string]string{},
			"optional_reference_facts": []map[string]string{},
		},
	}
}

func TestSweepTableIsStrict(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	// The golden expects lowercase "ada"; the mock extracts "Ada" (first
	// word): strict keys differ, lenient keys match. The sweep table must
	// report the strict (0) numbers, matching its header.
	golden := writeGolden(t, []map[string]any{
		{
			"doc":      "Ada wrote notes.",
			"language": "en",
			"expected_entities": []map[string]string{
				{"label": "person", "text": "ada"},
			},
			"expected_relations":       []map[string]string{},
			"optional_reference_facts": []map[string]string{},
		},
	})
	report, err := Run(context.Background(), cfg, &extract.Mock{}, nil, golden, "", "entity=0.5:0.5:0.1 relation=0.5:0.5:0.1", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(report.SweepTable, "0.000 | 0.000") {
		t.Errorf("sweep table should carry strict zeros for the case-mismatch fixture:\n%s", report.SweepTable)
	}
	if strings.Contains(report.SweepTable, "1.000") {
		t.Errorf("sweep table carries lenient numbers under a strict header:\n%s", report.SweepTable)
	}
}

func TestShippedGoldenLoads(t *testing.T) {
	docs, err := loadGolden(filepath.Join("..", "..", "eval", "golden", "sample.jsonl"))
	if err != nil {
		t.Fatalf("shipped golden corpus does not load: %v", err)
	}
	if len(docs) != 10 {
		t.Errorf("shipped corpus has %d docs, want 10", len(docs))
	}
	langs := map[string]int{}
	for _, d := range docs {
		langs[d.Language]++
	}
	if langs["en"] != 5 || langs["it"] != 5 {
		t.Errorf("language mix = %v, want 5 en + 5 it", langs)
	}
}

func TestParseSweepTerminatesOnNonAdvancingSteps(t *testing.T) {
	// These shapes made the old float accumulation loop spin forever:
	// sub-ULP steps (v += step doesn't change v) and the overshoot window
	// with equal bounds. Every one must terminate with a single value.
	for spec, want := range map[string]float64{
		"entity=0.5:0.5:1e-17 relation=0.1:0.6:0.1": want0(0.5),
		"entity=1e16:1e16:1 relation=0.1:0.6:0.1":   want0(1e16),
		"entity=0:0:1e-19 relation=0.1:0.6:0.1":     want0(0),
		"entity=0:0:1e-30 relation=0.1:0.6:0.1":     want0(0),
	} {
		entities, _, err := ParseSweep(spec)
		if err != nil {
			t.Errorf("ParseSweep(%q): %v", spec, err)
			continue
		}
		if len(entities) != 1 || entities[0] != want {
			t.Errorf("ParseSweep(%q) = %v, want [%v]", spec, entities, want)
		}
	}
}

func want0(v float64) float64 { return v }

func TestParseSweepRejectsRunawayRangesFast(t *testing.T) {
	for _, bad := range []string{
		"entity=0:10000:0.001 relation=0.1:0.6:0.1",   // 10^7 values
		"entity=0:1:0.000000001 relation=0.1:0.6:0.1", // 10^9 values
		"entity=0:1000000:0.001 relation=0.1:0.6:0.1", // 10^9 values
		"entity=0:Inf:1 relation=0:1:0.1",             // +Inf bound
		"entity=0:1e300:1 relation=0:1:0.1",           // out-of-int-range quotient
		"entity=0:1:1e-19 relation=0:1:0.1",           // quotient above MaxInt64
		"entity=0:1:NaN relation=0:1:0.1",             // NaN step
		"entity=-Inf:Inf:1 relation=0:1:0.1",          // NaN quotient from infinities
	} {
		if _, _, err := ParseSweep(bad); err == nil {
			t.Errorf("ParseSweep(%q) = nil error, want the pre-cap rejection", bad)
		}
	}
}

func TestRunProducesStructuredReportWithMock(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	golden := writeGolden(t, sampleDocs())
	out := filepath.Join(t.TempDir(), "report")

	report, err := Run(context.Background(), cfg, &extract.Mock{}, nil, golden, "", "", out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Provider != "mock" || report.ProviderVersion != "mock:1" {
		t.Errorf("metadata = %s %s", report.Provider, report.ProviderVersion)
	}
	if len(report.Docs) != 2 {
		t.Fatalf("docs = %d, want 2", len(report.Docs))
	}
	if len(report.EntitiesThresholds) == 0 || len(report.RelationsThresholds) == 0 {
		t.Error("threshold metadata missing")
	}
	if report.Docs[0].Facts == 0 {
		t.Error("mock extraction should produce facts for the first doc")
	}

	// Deterministic: a second run reproduces the metrics.
	again, err := Run(context.Background(), cfg, &extract.Mock{}, nil, golden, "", "", "")
	if err != nil {
		t.Fatalf("Run again: %v", err)
	}
	if !metricsEqual(report.Docs[0].EntitiesStrict, again.Docs[0].EntitiesStrict) ||
		!metricsEqual(report.Docs[0].RelationsLenient, again.Docs[0].RelationsLenient) {
		t.Error("report is not deterministic")
	}

	// Output files.
	for _, suffix := range []string{".md", ".json"} {
		data, err := os.ReadFile(out + suffix)
		if err != nil {
			t.Fatalf("missing report file %s: %v", suffix, err)
		}
		if len(data) == 0 {
			t.Errorf("report %s is empty", suffix)
		}
		if suffix == ".json" {
			var saved map[string]json.RawMessage
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if string(saved["preset"]) != `""` {
				t.Errorf("default preset = %s, want explicit empty string", saved["preset"])
			}
		}
	}
}

func metricsEqual(a, b Metrics) bool {
	return a.Precision == b.Precision && a.Recall == b.Recall && a.F1 == b.F1 && a.TP == b.TP && a.FP == b.FP && a.FN == b.FN
}

func TestSweepProducesTable(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	golden := writeGolden(t, sampleDocs())

	report, err := Run(context.Background(), cfg, &extract.Mock{}, nil, golden, "",
		"entity=0.2:0.6:0.2 relation=0.1:0.3:0.2", "")
	if err != nil {
		t.Fatalf("Run with sweep: %v", err)
	}
	if report.SweepTable == "" {
		t.Fatal("sweep table missing")
	}
	rows := strings.Count(report.SweepTable, "\n|") - 1 // minus separator row
	if rows != 6 {
		t.Errorf("sweep table rows = %d, want 6 (3 entity x 2 relation)", rows)
	}
	for _, want := range []string{"entity", "relation", "0.2", "0.6", "0.3"} {
		if !strings.Contains(report.SweepTable, want) {
			t.Errorf("sweep table missing %q:\n%s", want, report.SweepTable)
		}
	}
}

func TestEmptyGoldenErrors(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), cfg, &extract.Mock{}, nil, empty, "", "", ""); err == nil {
		t.Fatal("empty golden file: want error")
	}
	if _, err := Run(context.Background(), cfg, &extract.Mock{}, nil, filepath.Join(t.TempDir(), "missing.jsonl"), "", "", ""); err == nil {
		t.Fatal("missing golden file: want error")
	}
}
