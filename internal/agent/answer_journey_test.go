package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const lampSearch = `{"query":"lamps","kind":"product"}`

func answerJourneyStore() *answerHTTPStore {
	a := answerCard(lampA, "a", answerEvidence("weight", "120 g"))
	b := answerCard(lampB, "b", answerEvidence("weight", "180 g"))
	c := answerCard(lampC, "c")
	return &answerHTTPStore{
		answerTestStore: &answerTestStore{
			productSearchToolStore: &productSearchToolStore{
				products: func(_ context.Context, _, domain string, _ int) ([]graph.Product, error) {
					if domain != answerTestDomain {
						return nil, fmt.Errorf("unexpected search scope %q", domain)
					}
					return []graph.Product{a.Product, b.Product, c.Product}, nil
				},
			},
			detail: graph.ProductDetailWithMentions{
				ProductDetail: graph.ProductDetail{Product: a.Product, Features: a.Features},
				ProductMentionFields: &graph.ProductMentionFields{Mentions: []graph.ProductChunkMention{
					{ChunkID: "chunk-a", Excerpt: "Lamp A weighs 120 g.", DocID: "doc-a"},
				}, MentionsTotal: 1},
			},
			readChunk: func(id string) (graph.Chunk, error) {
				switch id {
				case "chunk-a":
					return graph.Chunk{ID: id, DocID: "doc-a", Domain: answerTestDomain, Text: "Lamp A weighs 120 g."}, nil
				case "chunk-b":
					return graph.Chunk{ID: id, DocID: "doc-b", Domain: answerTestDomain, Text: "Lamp B weighs 180 g."}, nil
				default:
					return graph.Chunk{}, graph.ErrNotFound
				}
			},
		},
		cards: graph.ProductCards{Cards: map[string]graph.ProductCard{lampA: a, lampB: b, lampC: c}},
		docs: map[string]graph.ChunkDocument{
			"chunk-a": {DocumentID: "doc-a", Title: "Lamp A manual", Source: "manual-a.pdf"},
			"chunk-b": {DocumentID: "doc-b", Title: "Lamp B manual", Source: "manual-b.pdf"},
		},
	}
}

func performAnswerJourney(t *testing.T, s *Server, body string) AnswerResponse {
	t.Helper()
	rr := postAnswer(t, s, body)
	expectAnswerHTTP(t, rr, 200, "", "")
	var response AnswerResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Contract != "gkg-answer/1" || response.Domain != answerTestDomain || !response.Complete {
		t.Fatalf("answer is not complete: %+v", response)
	}
	return response
}

func assertAnswerHydration(t *testing.T, store *answerHTTPStore, products, chunks []string) {
	t.Helper()
	if store.cardReads != 1 || store.documentReads != 1 ||
		!slices.Equal(store.hydratedProducts, products) || !slices.Equal(store.hydratedChunks, chunks) {
		t.Fatalf("hydration reads: cards=%d %v, documents=%d %v", store.cardReads, store.hydratedProducts, store.documentReads, store.hydratedChunks)
	}
}

func TestAnswerJourneyFind(t *testing.T) {
	store := answerJourneyStore()
	s, _ := newAnswerHTTPServer(t, store, 3,
		toolCallReply("find", "search", lampSearch),
		contentReply("Pick Lamp A [["+lampA+"]]. Lamp C is unsuitable."),
	)
	result := performAnswerJourney(t, s, validAnswerQuestion)
	if result.Summary != "Pick Lamp A. Lamp C is unsuitable." || len(result.Products) != 1 ||
		result.Products[0].ProductID != lampA || len(result.Products[0].Features) != 1 {
		t.Fatalf("find = %+v", result)
	}
	if result.Trace.Steps != 1 || !slices.Equal(result.Trace.Tools, []string{"search"}) || result.Trace.MS < 0 {
		t.Errorf("trace = %+v", result.Trace)
	}
	assertAnswerHydration(t, store, []string{lampA}, nil)
}

func TestAnswerJourneyCompare(t *testing.T) {
	store := answerJourneyStore()
	s, _ := newAnswerHTTPServer(t, store, 3,
		toolCallsReply(
			scriptedCall{"find", "search", lampSearch},
			scriptedCall{"chunks", "get_chunks", `{"ids":["chunk-a","chunk-b"],"with_products":false}`},
		),
		contentReply("Lamp A [["+lampA+"]] [[chunk-a]] is lighter than Lamp B [["+lampB+"]] [[chunk-b]]."),
	)
	result := performAnswerJourney(t, s, `{"question":"Compare the two lamps","domain":"catalog"}`)
	if len(result.Products) != 2 || result.Comparison == nil || len(result.Comparison.Rows) != 1 ||
		result.Comparison.Rows[0].FeatureID != "weight" || len(result.Passages) != 2 {
		t.Fatalf("compare = %+v", result)
	}
	for i, id := range []string{"doc-a", "doc-b"} {
		if result.Passages[i].DocumentID != id {
			t.Errorf("passage %d = %+v", i, result.Passages[i])
		}
	}
	assertAnswerHydration(t, store, []string{lampA, lampB}, []string{"chunk-a", "chunk-b"})
}

func TestAnswerJourneyDescribe(t *testing.T) {
	store := answerJourneyStore()
	s, _ := newAnswerHTTPServer(t, store, 3,
		toolCallReply("describe", "get_product", `{"id":"a"}`),
		contentReply("Lamp A [["+lampA+"]] weighs 120 g [[chunk-a]]."),
	)
	result := performAnswerJourney(t, s, `{"question":"Describe Lamp A","domain":"catalog"}`)
	if len(result.Products) != 1 || !slices.Equal(result.Products[0].PassageIDs, []string{"chunk-a"}) || len(result.Passages) != 1 {
		t.Fatalf("describe = %+v", result)
	}
	passage := result.Passages[0]
	if passage.DocumentTitle != "Lamp A manual" || len(passage.Excerpts) != 1 || passage.Excerpts[0].Text != "Lamp A weighs 120 g." ||
		len(passage.Links) != 1 || passage.Links[0].Kind != "mention" {
		t.Errorf("passage = %+v", passage)
	}
	assertAnswerHydration(t, store, []string{lampA}, []string{"chunk-a"})
}

func TestAnswerJourneyFollowUp(t *testing.T) {
	store := answerJourneyStore()
	s, stub := newAnswerHTTPServer(t, store, 3,
		toolCallReply("find", "search", lampSearch),
		contentReply("Try A [["+lampA+"]] and B [["+lampB+"]]."),
		toolCallReply("find", "search", lampSearch),
		contentReply("A [["+lampA+"]] weighs less than B [["+lampB+"]]."),
	)
	first := performAnswerJourney(t, s, validAnswerQuestion)
	var ids []string
	for _, card := range first.Products {
		ids = append(ids, card.ProductID)
	}
	if !slices.Equal(ids, []string{lampA, lampB}) {
		t.Fatalf("first find products = %v", ids)
	}
	question := AnswerRequest{Question: "Which is lighter?", Domain: answerTestDomain, Context: &AnswerContext{
		ProductIDs: ids, History: []HistoryTurn{{Role: "assistant", Text: first.Summary}},
	}}
	result := performAnswerJourney(t, s, answerJSON(t, question))
	stub.srv.Close()
	if len(result.Products) != 2 || result.Comparison == nil || !slices.Equal(result.Comparison.ProductIDs, ids) {
		t.Fatalf("follow-up = %+v", result)
	}
	if len(stub.requests) != 4 {
		t.Fatalf("provider received %d requests, want 4", len(stub.requests))
	}
	msgs := requestMessages(t, stub.requests[2])
	if len(msgs) != 3 || msgs[1]["role"] != "user" || msgs[2]["content"] != question.Question {
		t.Fatalf("follow-up messages = %+v", msgs)
	}
	ctx, _ := msgs[1]["content"].(string)
	if !strings.Contains(ctx, lampA+"\n"+lampB) || !strings.Contains(ctx, first.Summary) {
		t.Fatalf("conversation context = %q", ctx)
	}
}

func TestAnswerJourneyPromptsHaveNoCatalogueExamples(t *testing.T) {
	store := answerJourneyStore()
	s, stub := newAnswerHTTPServer(t, store, 1,
		toolCallReply("find", "search", lampSearch),
		contentReply("Pick A [["+lampA+"]]."),
	)
	result := performAnswerJourney(t, s, validAnswerQuestion)
	stub.srv.Close()
	if len(stub.requests) != 2 || !slices.Equal(result.Warnings, []string{"budget_final"}) {
		t.Fatalf("want initial and budget-final turns, got %d requests and warnings %v", len(stub.requests), result.Warnings)
	}
	initial := requestMessages(t, stub.requests[0])
	final := requestMessages(t, stub.requests[1])
	for _, prompt := range []any{initial[0]["content"], final[len(final)-1]["content"]} {
		prompt, ok := prompt.(string)
		if !ok || strings.Contains(prompt, askProductExample) || !strings.Contains(prompt, "[[product_id]]") {
			t.Error("answer prompt omitted citation instructions or included the ask catalogue example")
		}
	}
}
