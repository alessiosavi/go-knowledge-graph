package agent

import (
	"context"
	"strings"
)

const answerProductExample = "shop.example:123"
const answerMarkerRule = "Mark each product by its role in your answer: a product that answers the request, " +
	"or that the shopper asked about (each product in a comparison), as [[product_id]]; " +
	"a product you suggest instead because it does not fully meet the request, as [[!alt: product_id]]; " +
	"a product you rule out, or that the shopper excludes, by name only without a marker. " +
	"If nothing meets the request, say so and use no plain product marker. " +
	"Place each product marker right after its name, using the full store-prefixed product_id " +
	"(for example Example Lamp [[" + answerProductExample + "]]); " +
	"if a product_id contains brackets, commas, spaces or a line break, use its id instead. " +
	"Mark each passage you rely on as [[chunk id]], one marker per id. Write no other ids, hashes or links."

const answerRetrievalRules = "- When the shopper names or points to specific products, find and open each of them " +
	"(search by name or code, or get_product by a known id) before answering.\n" +
	"- For how-to-use or specification questions about a product, call get_product with with_texts true, " +
	"read the documents its pages list with get_chunks, and when a page is partial or lists fewer chunks than it has, " +
	"continue with chunk neighbours or a chunk search for the product; answer every point asked from what you read. " +
	"If a point is not in what you read, say it is not documented. Do not estimate or round numbers you did not read.\n"

const answerEvidenceRules = `- Do not say a product lacks a feature unless what you read says so.
- Answer in the language of the question.
`

const answerFinalPrompt = finalPromptOpening + answerMarkerRule + "\n" +
	answerEvidenceRules + "- If a point asked was not in the evidence you read, say it could not be verified."

func answerSystemPrompt(ctx context.Context) string {
	return promptOpening + promptBeforeProductExample + answerProductExample + promptAfterProductExample +
		"- " + answerMarkerRule + "\n" +
		answerRetrievalRules + answerEvidenceRules +
		"- When the question names a period, pass since/until to search and list_documents." +
		domainPromptSuffix(ctx)
}

func answerContextMessage(conversation *AnswerContext) chatMessage {
	var content strings.Builder
	content.WriteString("Conversation context (same shop):")
	if len(conversation.ProductIDs) > 0 {
		content.WriteString("\nProduct IDs:")
		for _, id := range conversation.ProductIDs {
			content.WriteString("\n")
			content.WriteString(id)
		}
	}
	if len(conversation.History) > 0 {
		content.WriteString("\nHistory:")
		for _, turn := range conversation.History {
			content.WriteString("\n")
			content.WriteString(turn.Role)
			content.WriteString(": ")
			content.WriteString(turn.Text)
		}
	}
	return chatMessage{Role: "user", Content: content.String()}
}
