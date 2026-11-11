package ingest

import (
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestBuildExtractionNameIdentity(t *testing.T) {
	p := &Pipeline{cfg: config.Config{}}
	r := extract.ChunkResult{
		Mentions: []extract.Mention{
			{Label: "product", Text: "`Widget`", Score: 0.4},
			{Label: "organization", Text: "Widget’s", Score: 0.9},
			{Label: "technology", Text: "**Widget**", Score: 0.8},
			{Label: "work", Text: "Note"},
		},
		Candidates: []extract.Candidate{{Label: "uses", Head: 1, Tail: 3, Score: 0.8}},
	}
	ents, facts, audits := p.buildExtraction(&graph.Document{ID: "doc"}, graph.Chunk{ID: "chunk"}, r, "test")
	if len(ents) != 2 {
		t.Fatalf("entities = %+v, want two names regardless of type", ents)
	}
	for _, e := range ents {
		if e.Key == "widget" && (e.Name != "Widget" || e.Type != "product") {
			t.Errorf("entity = %+v, want the first occurrence's cleaned name and type", e)
		}
	}
	if len(facts) != 1 || facts[0].HeadKey != "widget" || facts[0].TailKey != "note" {
		t.Errorf("facts = %+v, want widget -[uses]-> note", facts)
	}
	if len(audits) != 0 {
		t.Errorf("audits = %+v, want none", audits)
	}
}

func TestBuildExtractionSkipsEmptyKeys(t *testing.T) {
	p := &Pipeline{cfg: config.Config{}}
	p.cfg.Ingest.StoreFloor = 0.15
	for _, empty := range []string{"", " ", "``", "**", "_", "'s", " ’s ", "...", "`'s`"} {
		t.Run(empty, func(t *testing.T) {
			r := extract.ChunkResult{
				Mentions: []extract.Mention{{Label: "product", Text: empty}, {Label: "work", Text: "Note"}},
				Candidates: []extract.Candidate{
					{Label: "uses", Head: 0, Tail: 1, Score: 0.8},
					{Label: "uses", Head: 1, Tail: 0, Score: 0.8},
					{Label: "uses", Head: 0, Tail: 1, Score: 0.1},
					{Label: "uses", Head: 1, Tail: 0, Score: 0.1},
				},
			}
			ents, facts, audits := p.buildExtraction(&graph.Document{ID: "doc"}, graph.Chunk{ID: "chunk"}, r, "test")
			if len(ents) != 1 || ents[0].Key != "note" {
				t.Errorf("entities = %+v, want only note", ents)
			}
			if len(facts) != 0 || len(audits) != 0 {
				t.Errorf("facts = %+v, audits = %+v, want none for empty endpoints", facts, audits)
			}
		})
	}
}
