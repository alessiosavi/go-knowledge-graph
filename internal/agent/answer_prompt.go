package agent

import (
	"context"
	"strings"
)

const answerProductExample = "shop.example:123"
const answerMarkerRule = "Mark each product you recommend or that answers the question as [[product_id]] right after its name, using the full store-prefixed product_id (for example Example Lamp [[" + answerProductExample + "]]); if a product_id contains brackets, commas, spaces or a line break, use its id instead. Mention products you rule out by name only, without a marker. Mark each passage you rely on as [[chunk id]], one marker per id. Write no other ids, hashes or links."

const answerFinalPrompt = finalPromptOpening + answerMarkerRule

func answerSystemPrompt(ctx context.Context) string {
	return promptOpening + "- " + answerMarkerRule + "\n" +
		promptBeforeProductExample + answerProductExample + promptAfterProductExample +
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
