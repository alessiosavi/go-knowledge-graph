package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	ents, facts, audits := p.buildExtraction(&graph.Document{ID: "doc"}, graph.Chunk{ID: "chunk"}, r, "test", nil)
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
				extract.ChunkResult{Mentions: mentions}, "test", nil)
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
	}, "test", nil)
	want := []graph.MentionSpan{{Start: 0, End: 6, Type: "technology"}, {Start: 12, End: 18, Type: "product"}}
	if len(ents) != 1 || ents[0].Type != "product" || !slices.Equal(ents[0].Mentions, want) {
		t.Errorf("entities = %+v, want best type product and mentions %+v", ents, want)
	}
}

func TestBuildExtractionSkipsEmptyKeys(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("scoped=%t", scoped), func(t *testing.T) {
			testBuildExtractionSkipsEmptyKeys(t, map[string]bool{"product": scoped})
		})
	}
}

func testBuildExtractionSkipsEmptyKeys(t *testing.T, docScoped map[string]bool) {
	t.Helper()
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
			ents, facts, audits := p.buildExtraction(&graph.Document{ID: "doc"}, graph.Chunk{ID: "chunk"}, r, "test", docScoped)
			if len(ents) != 1 || ents[0].Key != "note" {
				t.Errorf("entities = %+v, want only note", ents)
			}
			if len(facts) != 0 || len(audits) != 0 {
				t.Errorf("facts = %+v, audits = %+v, want none for empty endpoints", facts, audits)
			}
		})
	}
}

func TestBuildExtractionDocumentScope(t *testing.T) {
	p := &Pipeline{cfg: config.Config{}}
	p.cfg.Ingest.StoreFloor = 0.15
	r := extract.ChunkResult{
		Mentions: []extract.Mention{
			{Label: "amount", Text: "€500", Start: 0, End: 6, Score: 0.9},
			{Label: "organization", Text: "Shopify", Start: 7, End: 14, Score: 0.9},
		},
		Candidates: []extract.Candidate{
			{Label: "amount_of", Head: 0, Tail: 1, Score: 0.8},
			{Label: "billed", Head: 1, Tail: 0, Score: 0.8},
			{Label: "amount_of", Head: 0, Tail: 1, Score: 0.1},
			{Label: "billed", Head: 1, Tail: 0, Score: 0.1},
		},
	}
	keys := make(map[string]bool)
	for _, docID := range []string{"A", "B"} {
		ents, facts, audits := p.buildExtraction(&graph.Document{ID: docID}, graph.Chunk{ID: docID + "-c0"}, r, "test", map[string]bool{"amount": true})
		amountKey := graph.ScopedEntityKey(docID, "€500")
		organizationKey := graph.EntityKey("Shopify")
		if len(ents) != 2 {
			t.Fatalf("doc %s: entities = %+v, want two", docID, ents)
		}
		byKey := make(map[string]graph.EntityUpsert)
		for _, ent := range ents {
			byKey[ent.Key] = ent
		}
		if ent, ok := byKey[amountKey]; !ok || ent.Name != "€500" || ent.Type != "amount" || !slices.Equal(ent.Mentions, []graph.MentionSpan{{Start: 0, End: 6, Type: "amount"}}) {
			t.Errorf("doc %s: amount = %+v, want scoped amount with its mention", docID, ent)
		}
		if ent, ok := byKey[organizationKey]; !ok || ent.Name != "Shopify" {
			t.Errorf("doc %s: organization = %+v, want unscoped Shopify", docID, ent)
		}
		if len(facts) != 2 || facts[0].HeadKey != amountKey || facts[0].TailKey != organizationKey || facts[1].HeadKey != organizationKey || facts[1].TailKey != amountKey {
			t.Errorf("doc %s: facts = %+v, want scoped keys on both endpoint positions", docID, facts)
		}
		if len(audits) != 2 || audits[0].HeadKey != amountKey || audits[0].TailKey != organizationKey || audits[1].HeadKey != organizationKey || audits[1].TailKey != amountKey {
			t.Errorf("doc %s: audits = %+v, want scoped keys on both endpoint positions", docID, audits)
		}
		keys[amountKey] = true
	}
	if len(keys) != 2 {
		t.Errorf("amount keys = %v, want distinct document keys", keys)
	}
}

func TestLabelsForRejectsScopedRelation(t *testing.T) {
	p := &Pipeline{cfg: config.Config{}}
	p.cfg.Labels.PresetsDir = t.TempDir()
	preset := "entities:\n  - name: amount\n    scope: document\nrelations:\n  - name: amount_of\n    scope: document\n"
	if err := os.WriteFile(filepath.Join(p.cfg.Labels.PresetsDir, "custom.yaml"), []byte(preset), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := p.labelsFor("custom")
	if err == nil || !strings.Contains(err.Error(), "amount_of") || !strings.Contains(err.Error(), "entity labels") {
		t.Fatalf("labelsFor = %v, want error naming relation amount_of", err)
	}
}

func TestJudgeNameUsesMentionText(t *testing.T) {
	if got := judgeName("€500", "#doc1:€500"); got != "€500" {
		t.Errorf("judgeName = %q, want the mention text", got)
	}
	if got := judgeName("  ", "ada lovelace"); got != "ada lovelace" {
		t.Errorf("judgeName without text = %q, want the key", got)
	}
}

func TestLabelsForRejectsUnknownScope(t *testing.T) {
	for _, section := range []string{"entities", "relations"} {
		t.Run(section, func(t *testing.T) {
			p := &Pipeline{cfg: config.Config{}}
			p.cfg.Labels.PresetsDir = t.TempDir()
			preset := fmt.Sprintf("%s:\n  - name: amount\n    scope: global\n", section)
			if err := os.WriteFile(filepath.Join(p.cfg.Labels.PresetsDir, "custom.yaml"), []byte(preset), 0o644); err != nil {
				t.Fatal(err)
			}
			_, _, err := p.labelsFor("custom")
			if err == nil || !strings.Contains(err.Error(), "custom") || !strings.Contains(err.Error(), "amount") {
				t.Fatalf("labelsFor = %v, want error naming preset custom and label amount", err)
			}
		})
	}
}
