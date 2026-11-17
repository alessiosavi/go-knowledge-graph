package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/hygiene"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

type hygieneFixture struct{}

func TestBuiltinHygieneValues(t *testing.T) {
	preset, err := hygiene.LoadPreset(filepath.Join(t.TempDir(), "absent"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	labels, _ := ingest.DefaultLabels()
	want := 0
	for _, label := range labels {
		value := label.Scope == "document"
		if preset.Values[label.Name] != value {
			t.Errorf("built-in label %q value=%t, want %t", label.Name, preset.Values[label.Name], value)
		}
		if value {
			want++
		}
	}
	if len(preset.Values) != want {
		t.Errorf("built-in Values has %d entries, want %d", len(preset.Values), want)
	}
}

func (hygieneFixture) Name() string    { return "hygiene-fixture" }
func (hygieneFixture) Version() string { return "hygiene-fixture:1" }
func (hygieneFixture) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	var out []extract.ChunkResult
	for _, c := range req.Chunks {
		r := extract.ChunkResult{ChunkID: c.ID}
		for _, name := range []string{"l'Acme", "Quillon Ltd", "Descrizione", "Zorbo Pumps"} {
			if start := strings.Index(c.Text, name); start >= 0 {
				r.Mentions = append(r.Mentions, extract.Mention{Text: name, Label: "organization", Start: start, End: start + len(name), Score: .9})
			}
		}
		r.Candidates = []extract.Candidate{{Head: 0, Tail: 1, Label: "uses", Score: .9}, {Head: 2, Tail: 1, Label: "uses", Score: .9}}
		out = append(out, r)
	}
	return out, nil
}

type hygieneJudge struct{ inputs []ingest.FactInput }

func (j *hygieneJudge) VerifyFact(_ context.Context, in ingest.FactInput) (ingest.Verdict, error) {
	j.inputs = append(j.inputs, in)
	return ingest.Verdict{Status: "confirmed"}, nil
}

func TestRunHygieneAndSweep(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Labels.PresetsDir = "../../labels"
	g := GoldenDoc{Doc: "## Zorbo Pumps\n\nl'Acme uses Quillon Ltd. Descrizione repeats.",
		ExpectedEntities:       []ExpectedEntity{{Label: "organization", Text: "l'Acme"}, {Label: "organization", Text: "Quillon Ltd"}, {Label: "organization", Text: "Descrizione"}},
		ExpectedRelations:      []ExpectedRelation{{Label: "uses", Head: "l'Acme", Tail: "Quillon Ltd"}, {Label: "uses", Head: "Descrizione", Tail: "Quillon Ltd"}},
		OptionalReferenceFacts: []ExpectedRelation{{Label: "uses", Head: "l'Acme", Tail: "Quillon Ltd"}, {Label: "uses", Head: "Descrizione", Tail: "Quillon Ltd"}},
	}
	j := &hygieneJudge{}
	golden := filepath.Join(t.TempDir(), "golden.jsonl")
	data, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(golden, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), cfg, hygieneFixture{}, j, golden, "general", "entity=0.4:0.4:0.1 relation=0.3:0.3:0.1", "")
	if err != nil {
		t.Fatal(err)
	}
	d := report.Docs[0]
	if d.EntitiesStrict.F1 != 1 || d.EntitiesLenient.F1 != 1 || d.RelationsStrict.F1 != 1 || d.RelationsLenient.F1 != 1 || d.ExpectedEntities != 2 || d.ExtractedEntities != 2 || d.Facts != 1 || d.ReferenceCoverage != 1 {
		t.Errorf("eval hygiene metrics=%+v", d)
	}
	if !strings.Contains(report.SweepTable, "| 0.40 | 0.30 | 1.000 | 1.000 |") {
		t.Errorf("sweep does not apply hygiene:\n%s", report.SweepTable)
	}
	if len(j.inputs) != 1 || j.inputs[0].Head != "l'Acme" || j.inputs[0].Tail != "Quillon Ltd" {
		t.Errorf("judge must receive raw evidence names: %+v", j.inputs)
	}
}
