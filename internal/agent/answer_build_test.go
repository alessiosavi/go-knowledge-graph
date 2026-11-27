package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const (
	lampA = "shop.example:1031"
	lampB = "shop.example:1032"
	lampC = "shop.example:1033"
)

func answerCard(id, graphID string, features ...graph.ProductEvidence) graph.ProductCard {
	return graph.ProductCard{
		Product:  graph.Product{ID: graphID, ProductID: id, Store: "shop.example", Domain: answerTestDomain, Title: "Lamp " + graphID},
		Features: features,
	}
}

func answerEvidence(featureID, raw string) graph.ProductEvidence {
	return graph.ProductEvidence{
		Feature: graph.ProductFeatureRev{ProductFeature: graph.ProductFeature{ID: featureID}},
		Raw:     raw, SourceKey: "spec", ReleaseID: "rel-1",
	}
}

func answerJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAnswerCardTextsCapAndFirstSeenOrder(t *testing.T) {
	card := answerCard(lampA, "a")
	seen := newSeenIndex()
	var want []graph.ProductText
	for call := range 3 {
		out := searchOut{}
		for row := range 10 {
			i := call*10 + row
			entry := graph.ProductText{Key: "intensity", Text: fmt.Sprintf("value %d", i), Checked: true}
			if i == 1 {
				entry.Key = "other"
				entry.Text = want[0].Text
			}
			want = append(want, entry)
			out.Hits = append(out.Hits, searchHitOut{
				Kind: "product", ID: card.ID, ProductID: card.ProductID, Key: &entry.Key, Excerpt: &entry.Text,
				TextTruncated: &entry.TextTruncated, Checked: &entry.Checked,
			})
		}
		// Repeat an entry with different flags, including one beyond the card cap.
		duplicate := out.Hits[8]
		duplicate.Checked = new(false)
		duplicate.ExcerptCut = new(true)
		out.Hits = append(out.Hits, duplicate)
		seen.record("search", out)
	}
	cards := graph.ProductCards{Cards: map[string]graph.ProductCard{card.ProductID: card}}
	raw := answerJSON(t, buildAnswer(answerRun{Text: "[[a]]", Seen: seen}, cards, nil, answerTestDomain))
	var response struct {
		Products []struct {
			Texts []struct {
				Tool string `json:"tool"`
				graph.ProductText
				ExcerptCut bool `json:"excerpt_cut"`
			} `json:"texts"`
			TextsOmitted int `json:"texts_omitted"`
		} `json:"products"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Products) != 1 {
		t.Fatalf("got %d cards, want 1", len(response.Products))
	}
	got := response.Products[0]
	if len(got.Texts) != 24 || got.TextsOmitted != 6 {
		t.Fatalf("got %d texts and %d omitted, want 24 and 6", len(got.Texts), got.TextsOmitted)
	}
	for i, entry := range got.Texts {
		if entry.Tool != "search" || entry.ProductText != want[i] || entry.ExcerptCut {
			t.Errorf("text %d = %+v, want original search entry %+v", i, entry, want[i])
		}
	}
}

func TestAnswerFieldRulesAlwaysPresent(t *testing.T) {
	for _, count := range []int{0, 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			seen := newSeenIndex()
			seen.addProduct("a", lampA)
			cards := graph.ProductCards{Cards: map[string]graph.ProductCard{lampA: answerCard(lampA, "a")}}
			text := "A lamp."
			if count == 1 {
				text = "A lamp [[a]]."
			}
			raw := answerJSON(t, buildAnswer(answerRun{Text: text, Seen: seen}, cards, nil, answerTestDomain))
			var body map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &body); err != nil {
				t.Fatal(err)
			}
			fields := []string{"contract", "domain", "summary", "products", "comparison", "passages", "complete", "warnings", "dropped", "as_of", "trace"}
			if len(body) != len(fields) {
				t.Errorf("expected %d fields, got %s", len(fields), raw)
			}
			for _, field := range fields {
				if _, ok := body[field]; !ok {
					t.Errorf("missing %s", field)
				}
			}
			for _, value := range []string{`"contract":"gkg-answer/1"`, `"comparison":null`, `"passages":[]`, `"warnings":[]`, `"stores":[]`, `"tools":[]`} {
				if !strings.Contains(raw, value) {
					t.Errorf("missing %s in %s", value, raw)
				}
			}
			if count == 0 && string(body["products"]) != "[]" {
				t.Errorf("products = %s", body["products"])
			}
			if count == 1 {
				for _, field := range []string{`"features":[]`, `"passage_ids":[]`, `"commerce":null`, `"features_omitted":0`} {
					if !strings.Contains(raw, field) {
						t.Errorf("missing %s in %s", field, raw)
					}
				}
			}
		})
	}
}

func TestAnswerComparisonRows(t *testing.T) {
	seen := newSeenIndex()
	cards := graph.ProductCards{Cards: make(map[string]graph.ProductCard)}
	for i, id := range []string{lampA, lampB, lampC} {
		seen.addProduct("", id)
		var features []graph.ProductEvidence
		for j := range 41 {
			if i < 2 || j != 0 {
				features = append(features, answerEvidence(fmt.Sprintf("feature%02d", j), id))
			}
		}
		cards.Cards[id] = answerCard(id, fmt.Sprint(i), features...)
	}
	run := answerRun{Text: "[[" + lampB + ", " + lampA + ", " + lampC + "]]", Seen: seen}
	got := buildAnswer(run, cards, nil, answerTestDomain)
	if got.Comparison == nil || !slices.Equal(got.Comparison.ProductIDs, []string{lampB, lampA, lampC}) || len(got.Comparison.Rows) != 12 {
		t.Fatalf("comparison = %+v", got.Comparison)
	}
	for i, row := range got.Comparison.Rows {
		if row.FeatureID != fmt.Sprintf("feature%02d", i) {
			t.Errorf("row %d = %s", i, row.FeatureID)
		}
	}
	first := got.Comparison.Rows[0].Cells
	if value, ok := first[lampC]; !ok || value != nil || len(first[lampA]) != 1 || first[lampA][0].Raw != lampA {
		t.Errorf("first row = %+v", first)
	}
	if len(got.Products[0].Features) != 40 || got.Products[0].FeaturesOmitted != 1 {
		t.Errorf("card features = %d, omitted = %d", len(got.Products[0].Features), got.Products[0].FeaturesOmitted)
	}
	// A feature beyond one card's cap cannot create a row with the other card.
	onlyShared := cards.Cards[lampC]
	onlyShared.Features = []graph.ProductEvidence{answerEvidence("feature40", "exists")}
	cards.Cards[lampC] = onlyShared
	got = buildAnswer(answerRun{Text: "[[" + lampA + ", " + lampC + "]]", Seen: seen}, cards, nil, answerTestDomain)
	if got.Comparison == nil || got.Comparison.Rows == nil || len(got.Comparison.Rows) != 0 {
		t.Errorf("comparison with no shared visible feature = %+v", got.Comparison)
	}
}

func answerPassageFixture() (answerRun, graph.ProductCards, map[string]graph.ChunkDocument) {
	seen := newSeenIndex()
	seen.addProduct("a", lampA)
	seen.addProduct("b", lampB)
	seen.addExcerpt("chunk", Excerpt{Tool: "get_chunks", Text: "Fixture passage"})
	seen.addLink("chunk", Link{ProductID: lampA, Via: []string{"ref"}})
	seen.addLink("chunk", Link{ProductID: lampB, Via: []string{"code"}})
	return answerRun{Text: "Pick A [[a]] [[chunk]].", Seen: seen}, graph.ProductCards{
		Cards: map[string]graph.ProductCard{lampA: answerCard(lampA, "a"), lampB: answerCard(lampB, "b")},
	}, map[string]graph.ChunkDocument{"chunk": {DocumentID: "doc", Title: "Lamp manual", Source: "manual.pdf"}}
}

func TestAnswerPassageLinksOnlyToCards(t *testing.T) {
	run, cards, docs := answerPassageFixture()
	got := buildAnswer(run, cards, docs, answerTestDomain)
	if len(got.Passages) != 1 || len(got.Passages[0].Links) != 1 || got.Passages[0].Links[0].ProductID != lampA {
		t.Fatalf("passages = %+v", got.Passages)
	}
	if len(got.Products) != 1 || !slices.Equal(got.Products[0].PassageIDs, []string{"chunk"}) {
		t.Errorf("products = %+v", got.Products)
	}
}

func TestAnswerExcerptsDedupFirstTool(t *testing.T) {
	run, cards, docs := answerPassageFixture()
	run.Seen.addExcerpt("chunk", Excerpt{Tool: "search", Text: "Fixture passage"})
	run.Seen.addExcerpt("chunk", Excerpt{Tool: "search", Text: "Another excerpt"})
	got := buildAnswer(run, cards, docs, answerTestDomain)
	if len(got.Passages) != 1 || len(got.Passages[0].Excerpts) != 2 {
		t.Fatalf("passages = %+v", got.Passages)
	}
	excerpts := got.Passages[0].Excerpts
	if excerpts[0].Tool != "get_chunks" || excerpts[1].Text != "Another excerpt" {
		t.Errorf("excerpts = %+v", excerpts)
	}
}

func TestAnswerMovedDocumentDropped(t *testing.T) {
	run, cards, _ := answerPassageFixture()
	got := buildAnswer(run, cards, nil, answerTestDomain)
	if len(got.Passages) != 0 || got.Dropped.NotFound != 1 || len(got.Products) != 1 || len(got.Products[0].PassageIDs) != 0 {
		t.Fatalf("answer = %+v", got)
	}
}

func TestAnswerCardFromSearchOnlyMark(t *testing.T) {
	seen := newSeenIndex()
	seen.record("search", searchOut{Hits: []searchHitOut{
		{Kind: "product", ID: "a", ProductID: lampA},
	}})
	cards := graph.ProductCards{Cards: map[string]graph.ProductCard{lampA: answerCard(lampA, "a", answerEvidence("weight", "120 g"))}}
	got := buildAnswer(answerRun{Text: "Pick [[a]].", Seen: seen}, cards, nil, answerTestDomain)
	if len(got.Products) != 1 || len(got.Products[0].Features) != 1 || got.Products[0].Features[0].Raw != "120 g" {
		t.Fatalf("products = %+v", got.Products)
	}
}

func TestAnswerCapsKeepFirstCitations(t *testing.T) {
	seen := newSeenIndex()
	cards := graph.ProductCards{Cards: make(map[string]graph.ProductCard)}
	docs := make(map[string]graph.ChunkDocument)
	var text strings.Builder
	for i := 12; i >= 0; i-- {
		chunk, product := fmt.Sprintf("chunk-%02d", i), fmt.Sprintf("shop.example:%02d", i)
		seen.addProduct("", product)
		seen.addExcerpt(chunk, Excerpt{Tool: "search", Text: "Passage"})
		cards.Cards[product] = answerCard(product, fmt.Sprint(i))
		docs[chunk] = graph.ChunkDocument{DocumentID: "doc"}
		fmt.Fprintf(&text, "[[%s]] [[%s]] [[%s]] ", product, chunk, product)
	}
	text.WriteString("[[unseen]] [[moved]]")
	seen.addExcerpt("moved", Excerpt{Tool: "search", Text: "Moved"})
	delete(docs, "chunk-12")
	got := buildAnswer(answerRun{Text: text.String(), Seen: seen}, cards, docs, answerTestDomain)
	if len(got.Products) != 10 || len(got.Passages) != 9 || got.Dropped.NotFound != 1 || got.Dropped.NotReturned != 1 {
		t.Fatalf("products=%d passages=%d dropped=%+v", len(got.Products), len(got.Passages), got.Dropped)
	}
	for i, card := range got.Products {
		if card.ProductID != fmt.Sprintf("shop.example:%02d", 12-i) {
			t.Fatalf("product %d = %s", i, card.ProductID)
		}
	}
	for i, passage := range got.Passages {
		if passage.ChunkID != fmt.Sprintf("chunk-%02d", 11-i) {
			t.Fatalf("passage %d = %s", i, passage.ChunkID)
		}
	}
}

func TestAnswerNoActiveRelease(t *testing.T) {
	run, cards, docs := answerPassageFixture()
	cards.AsOf.Stores = []graph.StoreWatermark{{Store: "shop.example"}}
	got := answerJSON(t, buildAnswer(run, cards, docs, answerTestDomain))
	for _, part := range []string{`"features":[]`, `"as_of":{"release_id":null,"activation_seq":null,"stores":[{"store":"shop.example","materialized_activation_seq":null}]}`, `"warnings":[]`} {
		if !strings.Contains(got, part) {
			t.Errorf("missing %s in %s", part, got)
		}
	}
}

func TestAnswerOutcomeMapping(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		outcome  answerOutcome
		budget   bool
		complete bool
		warnings []string
	}{
		{"blank", "  ", outcomeNoAnswer, false, false, []string{"no_answer"}},
		{"final turn failed", "", outcomeFinalTurnFailed, true, false, []string{"final_turn_failed", "budget_final"}},
		{"unknown mark", "[[unknown]]", outcomeAnswered, false, false, []string{"no_answer"}},
		{"unknown mark on budget turn", "[[unknown]]", outcomeAnswered, true, false, []string{"no_answer", "budget_final"}},
		{"truncated", "A lamp", outcomeTruncated, false, false, []string{"truncated"}},
		{"truncated on budget turn", "A lamp", outcomeTruncated, true, false, []string{"truncated", "budget_final"}},
		{"budget final", "A lamp", outcomeAnswered, true, true, []string{"budget_final"}},
		{"answered", "A lamp", outcomeAnswered, false, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildAnswer(answerRun{Text: tc.text, Outcome: tc.outcome, BudgetFinal: tc.budget, Seen: newSeenIndex()}, graph.ProductCards{}, nil, answerTestDomain)
			if got.Complete != tc.complete || !slices.Equal(got.Warnings, tc.warnings) {
				t.Errorf("complete = %v, warnings = %v", got.Complete, got.Warnings)
			}
			want := tc.text
			if tc.outcome == outcomeNoAnswer || tc.outcome == outcomeFinalTurnFailed || strings.HasPrefix(tc.text, "[[") {
				want = "No answer could be produced."
			}
			if got.Summary != want {
				t.Errorf("summary = %q, want %q", got.Summary, want)
			}
		})
	}
}

func TestAnswerPageOnlyMarkNotCited(t *testing.T) {
	seen := newSeenIndex()
	seen.addLink("page", Link{ProductID: lampA, Kind: "page"})
	got := buildAnswer(answerRun{Text: "See [[page]].", Seen: seen}, graph.ProductCards{}, nil, answerTestDomain)
	if len(got.Passages) != 0 || got.Dropped.NotReturned != 1 || got.Dropped.NotFound != 0 {
		t.Fatalf("passages = %+v, dropped = %+v", got.Passages, got.Dropped)
	}
}

func TestAnswerBracketIDViaGraphID(t *testing.T) {
	const productID = "shop.example:a[b]"
	seen := newSeenIndex()
	seen.addProduct("a", productID)
	cards := graph.ProductCards{Cards: map[string]graph.ProductCard{productID: answerCard(productID, "a")}}
	got := buildAnswer(answerRun{Text: "Pick [[a]].", Seen: seen}, cards, nil, answerTestDomain)
	if len(got.Products) != 1 || got.Products[0].ProductID != productID {
		t.Fatalf("products = %+v", got.Products)
	}
}

func TestAnswerContextOnlyMarkCounted(t *testing.T) {
	got := buildAnswer(answerRun{Text: "Pick [[shop.example:context]].", Seen: newSeenIndex()}, graph.ProductCards{}, nil, answerTestDomain)
	if got.Dropped.NotReturned != 1 || strings.Contains(answerJSON(t, got), "shop.example:context") {
		t.Fatalf("answer = %+v", got)
	}
}

func TestAnswerProductTypeNull(t *testing.T) {
	run, cards, docs := answerPassageFixture()
	raw := answerJSON(t, buildAnswer(run, cards, docs, answerTestDomain))
	if !strings.Contains(raw, `"product_type":null`) {
		t.Errorf("card has no explicit null product_type: %s", raw)
	}
}

func TestAnswerEvidenceVerbatim(t *testing.T) {
	run, cards, docs := answerPassageFixture()
	card := cards.Cards[lampA]
	evidence := answerEvidence("weight", " 120 g ")
	card.Features = []graph.ProductEvidence{evidence}
	cards.Cards[lampA] = card
	raw := answerJSON(t, buildAnswer(run, cards, docs, answerTestDomain))
	if !strings.Contains(raw, `"features":[`+answerJSON(t, evidence)+"]") || strings.Contains(raw, `"typed":`) || strings.Contains(raw, `"items":`) {
		t.Errorf("evidence projection changed: %s", raw)
	}
}

func TestAnswerSearchExcerptBeyond4KiB(t *testing.T) {
	run, cards, docs := answerPassageFixture()
	text := strings.Repeat("x", 4096) + " rated for 120 V"
	run.Seen.excerpts["chunk"] = nil
	run.Seen.addExcerpt("chunk", Excerpt{Tool: "get_chunks", Text: text[:4096], Truncated: true})
	run.Seen.addExcerpt("chunk", Excerpt{Tool: "search", Text: text})
	got := buildAnswer(run, cards, docs, answerTestDomain)
	if len(got.Passages) != 1 || len(got.Passages[0].Excerpts) != 2 ||
		!got.Passages[0].Excerpts[0].Truncated || got.Passages[0].Excerpts[1].Text != text {
		t.Fatalf("excerpt text or its truncation flag was lost")
	}
	raw := answerJSON(t, got)
	if strings.Count(raw, `"truncated":true`) != 1 || strings.Contains(raw, `"truncated":false`) {
		t.Error("truncated must appear only for the reported truncation")
	}
}

func TestAnswerMixedViaVerbatim(t *testing.T) {
	run, cards, docs := answerPassageFixture()
	run.Seen.links["chunk"] = []Link{
		{ProductID: lampA, Via: []string{"ref", "code"}, OutsideReturnedText: true},
		{ProductID: lampA, Kind: "mention"},
	}
	raw := answerJSON(t, buildAnswer(run, cards, docs, answerTestDomain))
	for _, part := range []string{`"via":["ref","code"]`, `"outside_returned_text":true`, `"kind":"mention"`} {
		if !strings.Contains(raw, part) {
			t.Errorf("missing %s in %s", part, raw)
		}
	}
	if strings.Contains(raw, `"outside_returned_text":false`) || strings.Contains(raw, `"kind":""`) || strings.Contains(raw, `"via":null`) {
		t.Errorf("link includes unreported fields: %s", raw)
	}
}

func TestAnswerPassageWithOnlyUnmarkedLink(t *testing.T) {
	run, cards, docs := answerPassageFixture()
	run.Text = "See [[chunk]]."
	got := buildAnswer(run, cards, docs, answerTestDomain)
	if len(got.Passages) != 1 || got.Passages[0].Links == nil || len(got.Passages[0].Links) != 0 || len(got.Products) != 0 {
		t.Fatalf("products = %+v, passages = %+v", got.Products, got.Passages)
	}
}

func TestAnswerZeroCardsAsOf(t *testing.T) {
	cards := graph.ProductCards{AsOf: graph.CardsAsOf{
		ReleaseID: new("rel-1"), ActivationSeq: new(int64(7)),
		Stores: []graph.StoreWatermark{{Store: "shop.example", MaterializedActivationSeq: new(int64(6))}},
	}}
	raw := answerJSON(t, buildAnswer(answerRun{Text: "No results.", Seen: newSeenIndex()}, cards, nil, answerTestDomain))
	if !strings.Contains(raw, `"products":[]`) || !strings.Contains(raw, `"as_of":{"release_id":"rel-1","activation_seq":7,"stores":[{"store":"shop.example","materialized_activation_seq":6}]}`) {
		t.Errorf("zero-card watermarks = %s", raw)
	}
}
