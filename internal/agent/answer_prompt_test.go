package agent

import (
	"strings"
	"testing"
)

func TestAnswerPromptHasNoCatalogueExamples(t *testing.T) {
	const marker = "Mark each product you recommend or that answers the question as [[product_id]] right after its name, using the full store-prefixed product_id (for example Example Lamp [[shop.example:123]]); if a product_id contains brackets, commas, spaces or a line break, use its id instead. Mention products you rule out by name only, without a marker. Mark each passage you rely on as [[chunk id]], one marker per id. Write no other ids, hashes or links."
	prompt := answerSystemPrompt(withDomain(t.Context(), answerTestDomain))
	for _, p := range []string{prompt, answerFinalPrompt} {
		if !strings.Contains(p, marker) || strings.Contains(strings.ToLower(p), "cypher") || strings.Contains(p, "dentalica") {
			t.Error("answer prompt must contain the marker instruction without catalogue data or Cypher instructions")
		}
	}
	if !strings.Contains(prompt, "Domain scope: "+answerTestDomain+".") ||
		!strings.Contains(prompt, "pass since/until to search and list_documents.") ||
		!strings.Contains(prompt, "get_product needs the graph product id, or store plus the full product_id (for example shop.example:123), never a bare code.") {
		t.Error("answer prompt is missing scoped, period, or product lookup guidance")
	}
}
