package ingest

import (
	"slices"
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
		if e.Key == "widget" && (e.Name != "Widget" || e.Type != "organization") {
			t.Errorf("entity = %+v, want cleaned name and best-scoring mention type", e)
		}
	}
	if len(facts) != 1 || facts[0].HeadKey != "widget" || facts[0].TailKey != "note" {
		t.Errorf("facts = %+v, want widget -[uses]-> note", facts)
	}
	if len(audits) != 0 {
		t.Errorf("audits = %+v, want none", audits)
	}
}

func TestBuildExtractionSameSpanTwoLabels(t *testing.T) {
	for _, scores := range [][2]float64{{0.6, 0.8}, {0.8, 0.8}} {
		for _, reverse := range []bool{false, true} {
			mentions := []extract.Mention{
				{Label: "product", Text: "Shopify", Start: 0, End: 7, Score: scores[0]},
				{Label: "organization", Text: "Shopify", Start: 0, End: 7, Score: scores[1]},
			}
			if reverse {
				slices.Reverse(mentions)
			}
			p := &Pipeline{cfg: config.Config{}}
			ents, _, _ := p.buildExtraction(&graph.Document{ID: "doc"}, graph.Chunk{ID: "chunk"},
				extract.ChunkResult{Mentions: mentions}, "test")
			want := []graph.MentionSpan{{Start: 0, End: 7, Type: "organization"}}
			if len(ents) != 1 || ents[0].Key != "shopify" || ents[0].Type != "organization" || !slices.Equal(ents[0].Mentions, want) {
				t.Errorf("scores %v, reverse %v: entities = %+v, want shopify with mentions %+v", scores, reverse, ents, want)
			}
		}
	}
}

func TestBuildExtractionKeepsDistinctSpans(t *testing.T) {
	p := &Pipeline{cfg: config.Config{}}
	ents, _, _ := p.buildExtraction(&graph.Document{ID: "doc"}, graph.Chunk{ID: "chunk"}, extract.ChunkResult{
		Mentions: []extract.Mention{
			{Label: "technology", Text: "Widget", Start: 0, End: 6, Score: 0.8},
			{Label: "product", Text: "Widget", Start: 12, End: 18, Score: 0.8},
			{Label: "organization", Text: "Widget", Start: 12, End: 18, Score: 0.6},
		},
	}, "test")
	want := []graph.MentionSpan{{Start: 0, End: 6, Type: "technology"}, {Start: 12, End: 18, Type: "product"}}
	if len(ents) != 1 || ents[0].Type != "product" || !slices.Equal(ents[0].Mentions, want) {
		t.Errorf("entities = %+v, want best type product and mentions %+v", ents, want)
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
