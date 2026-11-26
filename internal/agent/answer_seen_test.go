package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestAnswerSeenIndexFromEachTool(t *testing.T) {
	titleProduct := graph.Product{ID: "title-product", Store: "shop.example", ProductID: "shop.example:1", Domain: answerTestDomain, Title: "Lamp"}
	foundProduct := graph.Product{ID: "found-product", Store: "shop.example", ProductID: "shop.example:2", Domain: answerTestDomain}
	detailProduct := graph.Product{ID: "detail-product", Store: "shop.example", ProductID: "shop.example:3", Domain: answerTestDomain}
	linkedProduct := graph.Product{ID: "linked-product", Store: "shop.example", ProductID: "shop.example:4", Domain: answerTestDomain}
	const passage = "Lamp passage."
	st := &answerTestStore{
		productSearchToolStore: &productSearchToolStore{
			products: func(_ context.Context, _, domain string, _ int) ([]graph.Product, error) {
				if domain != answerTestDomain {
					return nil, fmt.Errorf("unexpected domain %q", domain)
				}
				return []graph.Product{titleProduct}, nil
			},
		},
		found: graph.ProductSearchResult{Hits: []graph.ProductHit{{Product: foundProduct}}, Cursor: "transport-cursor"},
		detail: graph.ProductDetailWithMentions{
			ProductDetail: graph.ProductDetail{
				Product: detailProduct,
				Pages:   []graph.ProductPage{{ID: "page-doc", ChunkIDs: []string{"page-only", "chunk-00"}}},
			},
			ProductMentionFields: &graph.ProductMentionFields{Mentions: []graph.ProductChunkMention{
				{ChunkID: "search-000", Excerpt: passage},
				{ChunkID: "mention-only", Excerpt: "Identifier match."},
			}},
		},
		index: graph.BuildProductMentionIndex([]graph.ProductIdentifierObservation{
			{Product: linkedProduct, Feature: "catalogue_code", ValueFold: new("AB12345")},
		}),
		readChunk: func(id string) (graph.Chunk, error) {
			if id == "failed" {
				return graph.Chunk{ID: id, Text: "failed result"}, fmt.Errorf("read failed")
			}
			return graph.Chunk{ID: id, DocID: "chunk-doc", Domain: answerTestDomain, Text: strings.Repeat("é", chunkread.ChunkTextCap) + " AB12345"}, nil
		},
	}
	for i := range 100 {
		st.chunks = append(st.chunks, graph.ChunkSearchRow{
			ID: fmt.Sprintf("search-%03d", i), DocID: "search-doc", Title: strings.Repeat("lamp ", 400),
			Text: passage, Domain: answerTestDomain,
		})
	}
	var ids []string
	for i := range 12 {
		ids = append(ids, fmt.Sprintf("chunk-%02d", i))
	}
	chunkArgs, err := json.Marshal(map[string][]string{"ids": ids})
	if err != nil {
		t.Fatal(err)
	}
	s, stub := newAnswerTestServer(t, st, 2, toolCallsReply(
		scriptedCall{"search", "search", `{"query":"lamp","limit":100}`},
		scriptedCall{"find", "find_products", `{"feature":"catalogue_code","value":"lamp"}`},
		scriptedCall{"product", "get_product", `{"id":"detail-product"}`},
		scriptedCall{"chunks", "get_chunks", string(chunkArgs)},
		scriptedCall{"links", "get_chunks", `{"ids":["chunk-00"]}`},
		scriptedCall{"failed", "get_chunks", `{"ids":["failed"]}`},
	), contentReply("Lamp [[shop.example:1]] [[search-000]]"))
	run := runTestAnswer(t, s, stub, []string{"shop.example"})
	if run.Outcome != outcomeAnswered || run.Text != "Lamp [[shop.example:1]] [[search-000]]" ||
		run.Steps != 1 || !slices.Equal(run.Tools, []string{"search", "find_products", "get_product", "get_chunks", "get_chunks", "get_chunks"}) {
		t.Fatalf("unexpected run: %+v", run)
	}
	results := answerToolResults(t, stub, 1)
	seen := run.Seen
	for _, p := range []graph.Product{titleProduct, foundProduct, detailProduct, linkedProduct} {
		for _, token := range []string{p.ID, p.ProductID} {
			if got, ok := seen.Product(token); !ok || got != p.ProductID {
				t.Errorf("Product(%q) = %q, %v", token, got, ok)
			}
		}
	}
	search := decodeAnswerToolResult[searchOut](t, results["search"])
	if search.HitsOmitted == 0 || seen.Chunk("search-099") {
		t.Fatal("fixture must produce an omitted search hit that stays out of the index")
	}
	for _, hit := range search.Hits {
		if hit.Kind == "chunk" && !seen.Chunk(hit.ID) {
			t.Errorf("missing delivered search hit %q", hit.ID)
		}
	}
	chunks := decodeAnswerToolResult[chunkread.Result](t, results["chunks"])
	if len(chunks.Chunks) == 0 || len(chunks.Meta.OmittedForBudget) == 0 {
		t.Fatal("fixture must deliver chunk text and omit some chunks")
	}
	linked := decodeAnswerToolResult[chunkread.Result](t, results["links"])
	if len(linked.Chunks) != 1 || linked.Chunks[0].Products == nil || len(*linked.Chunks[0].Products) != 1 {
		t.Fatal("single-chunk result must deliver the matched product link")
	}
	for _, item := range chunks.Chunks {
		if got := seen.Excerpts(item.ID); !seen.Chunk(item.ID) ||
			!slices.Equal(got, []Excerpt{{Tool: "get_chunks", Text: item.Text, Truncated: true}}) {
			t.Errorf("incorrect retained chunk evidence for %q", item.ID)
		}
	}
	for _, id := range append(chunks.Meta.OmittedForBudget, "page-only", "page-doc", "chunk-doc", "search-doc", "transport-cursor", "failed") {
		if seen.Chunk(id) || slices.Contains(seen.KnownIDs(), id) {
			t.Errorf("undelivered or metadata-only id %q was indexed", id)
		}
	}
	if got := seen.Excerpts("search-000"); !seen.Chunk("search-000") || !slices.Equal(got, []Excerpt{{Tool: "search", Text: passage}}) {
		t.Errorf("search excerpts = %v, want first source only", got)
	}
	if got := seen.Excerpts("mention-only"); !seen.Chunk("mention-only") || !slices.Equal(got, []Excerpt{{Tool: "get_product", Text: "Identifier match."}}) {
		t.Errorf("mention excerpts = %v", got)
	}
	for id, kind := range map[string]string{"page-only": "page", "mention-only": "mention"} {
		got := seen.Links(id)
		if len(got) != 1 || got[0].Kind != kind || got[0].ProductID != detailProduct.ProductID {
			t.Errorf("links for %s = %+v", id, got)
		}
	}
	links := seen.Links("chunk-00")
	if len(links) != 2 || links[0].Kind != "page" || links[1].ProductID != linkedProduct.ProductID || links[1].Kind != "" ||
		!links[1].OutsideReturnedText || !slices.Equal(links[1].Via, []string{"code"}) {
		t.Errorf("chunk links = %+v", links)
	}
}
