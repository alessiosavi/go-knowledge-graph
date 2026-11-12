package eval

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

var goldenSets = []struct{ preset, file string }{
	{"general", "sample.jsonl"},
	{"documentation", "documentation.jsonl"},
	{"code", "code.jsonl"},
	{"mail", "mail.jsonl"},
	{"meeting", "meeting.jsonl"},
	{"demo", "demo.jsonl"},
}

func TestGoldenSetsMatchPresets(t *testing.T) {
	for _, set := range goldenSets {
		t.Run(set.preset, func(t *testing.T) {
			entities, relations, err := ingest.LoadPreset("../../labels", set.preset)
			if err != nil {
				t.Fatal(err)
			}
			docs, err := loadGolden(filepath.Join("../../eval/golden", set.file))
			if err != nil {
				t.Fatal(err)
			}
			if len(docs) < 10 {
				t.Errorf("docs = %d, want at least 10", len(docs))
			}
			entityLabels, relationLabels := map[string]bool{}, map[string]bool{}
			for _, label := range entities {
				entityLabels[label.Name] = true
			}
			for _, label := range relations {
				relationLabels[label.Name] = true
			}
			languages := map[string]int{}
			var allText strings.Builder
			for i, doc := range docs {
				languages[doc.Language]++
				allText.WriteString(doc.Doc)
				surfaces := map[string]bool{}
				for _, entity := range doc.ExpectedEntities {
					if !entityLabels[entity.Label] {
						t.Errorf("doc %d: unknown entity label %q", i, entity.Label)
					}
					if entity.Text == "" || !strings.Contains(doc.Doc, entity.Text) {
						t.Errorf("doc %d: entity text %q absent from document", i, entity.Text)
					}
					surfaces[entity.Text] = true
				}
				for kind, facts := range map[string][]ExpectedRelation{
					"relation": doc.ExpectedRelations, "optional fact": doc.OptionalReferenceFacts,
				} {
					for _, fact := range facts {
						if !relationLabels[fact.Label] {
							t.Errorf("doc %d: unknown %s label %q", i, kind, fact.Label)
						}
						for _, endpoint := range []string{fact.Head, fact.Tail} {
							if endpoint == "" || (!surfaces[endpoint] && !strings.Contains(doc.Doc, endpoint)) {
								t.Errorf("doc %d: %s endpoint %q absent from entities and document", i, kind, endpoint)
							}
						}
					}
				}
			}
			if set.preset == "mail" {
				if languages["it"] < 3 || languages["en"] < 3 {
					t.Errorf("mail language mix = %v, want at least 3 it and 3 en", languages)
				}
				for _, text := range []string{"è", "'", "’"} {
					if !strings.Contains(allText.String(), text) {
						t.Errorf("mail lacks Italian accent/apostrophe %q", text)
					}
				}
			}
		})
	}
}

func TestGoldenSetsMockEval(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Labels.PresetsDir = "../../labels"
	for _, set := range goldenSets {
		t.Run(set.preset, func(t *testing.T) {
			path := filepath.Join("../../eval/golden", set.file)
			docs, err := loadGolden(path)
			if err != nil {
				t.Fatal(err)
			}
			report, err := Run(context.Background(), cfg, &extract.Mock{}, nil, path, set.preset, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if report.Samples != len(docs) || len(report.Docs) != len(docs) {
				t.Fatalf("samples/results = %d/%d, want %d/%d", report.Samples, len(report.Docs), len(docs), len(docs))
			}
			for i, doc := range docs {
				if got := report.Docs[i]; got.Doc == "" || !strings.HasPrefix(doc.Doc, strings.TrimSuffix(got.Doc, "…")) || got.Language != doc.Language {
					t.Errorf("doc %d (%s) missing or changed in report: %+v", i, doc.Language, got)
				}
			}
		})
	}
}
