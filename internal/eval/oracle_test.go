package eval

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/hygiene"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

// goldenOracle emits exactly the expected mentions and candidate relations.
// Relations may declare endpoints outside expected_entities, so those endpoints
// are added only to the relation extraction used for independent relation scoring.
type goldenOracle struct{ doc GoldenDoc }

func (goldenOracle) Name() string    { return "golden-oracle" }
func (goldenOracle) Version() string { return "golden-oracle:1" }

func (o goldenOracle) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	var out []extract.ChunkResult
	for _, c := range req.Chunks {
		r := extract.ChunkResult{ChunkID: c.ID}
		indices := map[string]int{}
		mention := func(text, label string) int {
			if i, ok := indices[text]; ok {
				return i
			}
			start := strings.Index(c.Text, text)
			if start < 0 {
				return -1
			}
			indices[text] = len(r.Mentions)
			r.Mentions = append(r.Mentions, extract.Mention{Text: text, Label: label, Start: start, End: start + len(text), Score: 1})
			return len(r.Mentions) - 1
		}
		for _, e := range o.doc.ExpectedEntities {
			mention(e.Text, e.Label)
		}
		for _, rel := range o.doc.ExpectedRelations {
			head, tail := mention(rel.Head, ""), mention(rel.Tail, "")
			if head >= 0 && tail >= 0 {
				r.Candidates = append(r.Candidates, extract.Candidate{Head: head, Tail: tail, Label: rel.Label, Score: 1})
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func TestGoldenOracle(t *testing.T) {
	files, err := filepath.Glob("../../eval/golden/*.jsonl")
	if err != nil || len(files) == 0 {
		t.Fatalf("golden files: %v (%d files)", err, len(files))
	}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		presetName := name
		if name == "sample" {
			presetName = "general"
		}
		t.Run(name, func(t *testing.T) {
			cfg, err := config.Load("")
			if err != nil {
				t.Fatal(err)
			}
			cfg.Labels.PresetsDir = "../../labels"
			cfg.Ingest.ChunkWords = 100000
			cfg.Ingest.OverlapSentences = 0
			preset, err := hygiene.LoadPreset(cfg.Labels.PresetsDir, presetName, "")
			if err != nil {
				t.Fatal(err)
			}
			entities, relations, err := ingest.LoadPreset(cfg.Labels.PresetsDir, presetName)
			if err != nil {
				t.Fatal(err)
			}
			docs, err := loadGolden(file)
			if err != nil {
				t.Fatal(err)
			}
			var scores []DocResult
			independent := 0
			for i, g := range docs {
				// Entity and relation goldens are independent sets: sample.jsonl
				// includes relations whose endpoints are not expected entities.
				entityGold := g
				entityGold.ExpectedRelations = nil
				entityResults, err := extractDoc(context.Background(), cfg, goldenOracle{entityGold}, entities, relations, g.Doc, preset)
				if err != nil {
					t.Fatal(err)
				}
				relationResults, err := extractDoc(context.Background(), cfg, goldenOracle{g}, entities, relations, g.Doc, preset)
				if err != nil {
					t.Fatal(err)
				}
				var gotEntities, gotRelations, expectedEntities, expectedRelations []string
				for _, cr := range entityResults {
					for _, m := range cr.Mentions {
						gotEntities = append(gotEntities, StrictEntityKey(m.Label, m.Name))
					}
				}
				for _, cr := range relationResults {
					for _, r := range cr.Candidates {
						gotRelations = append(gotRelations, StrictRelationKey(r.Label, cr.Mentions[r.Head].Name, cr.Mentions[r.Tail].Name))
					}
				}
				clean := normalizeGolden(g, preset)
				for _, e := range clean.ExpectedEntities {
					expectedEntities = append(expectedEntities, StrictEntityKey(e.Label, e.Text))
				}
				for _, r := range clean.ExpectedRelations {
					expectedRelations = append(expectedRelations, StrictRelationKey(r.Label, r.Head, r.Tail))
				}
				score := DocResult{EntitiesStrict: Score(expectedEntities, gotEntities), RelationsStrict: Score(expectedRelations, gotRelations)}
				if score.EntitiesStrict.F1 != 1 || score.RelationsStrict.F1 != 1 {
					t.Errorf("doc %d: entity F1=%.3f relation F1=%.3f", i, score.EntitiesStrict.F1, score.RelationsStrict.F1)
				}
				scores = append(scores, score)

				declared := map[string]bool{}
				for _, e := range g.ExpectedEntities {
					declared[e.Text] = true
				}
				complete := true
				for _, r := range g.ExpectedRelations {
					complete = complete && declared[r.Head] && declared[r.Tail]
				}
				if !complete {
					independent++
					continue
				}
				// Where endpoint declarations are complete, also exercise the
				// complete evaluation path with one oracle prediction stream.
				result, err := evaluateDoc(context.Background(), cfg, goldenOracle{g}, nil, entities, relations, g, preset)
				if err != nil {
					t.Fatal(err)
				}
				if result.EntitiesStrict.F1 != 1 || result.RelationsStrict.F1 != 1 {
					t.Errorf("evaluateDoc %d: entity F1=%.3f relation F1=%.3f", i, result.EntitiesStrict.F1, result.RelationsStrict.F1)
				}
			}
			e := meanMetrics(scores, func(d DocResult) Metrics { return d.EntitiesStrict })
			r := meanMetrics(scores, func(d DocResult) Metrics { return d.RelationsStrict })
			t.Logf("preset=%s docs=%d strict entity F1=%.3f relation F1=%.3f independent-only docs=%d", presetName, len(docs), e.F1, r.F1, independent)
		})
	}
}
