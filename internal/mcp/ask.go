package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Asker runs one bounded agent request and returns client-safe errors.
type Asker interface {
	Ask(ctx context.Context, question, domain string, progress func(context.Context, int, string)) (AskResult, error)
}

type AskResult struct {
	Answer   string
	Steps    int
	Evidence []AskEvidence
}

type AskEvidence struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

type askArgs struct {
	Question        string `json:"question" jsonschema:"natural-language question about the knowledge graph"`
	Domain          string `json:"domain,omitempty" jsonschema:"optional domain slug; empty selects all"`
	IncludeEvidence bool   `json:"include_evidence,omitempty" jsonschema:"include tool evidence within a 32 KiB result budget (default false)"`
}

type askOut struct {
	Answer    string `json:"answer"`
	Steps     int    `json:"steps"`
	Evidence  *[]any `json:"evidence,omitempty" jsonschema:"evidence objects with tool, arguments, result and an optional truncated flag"`
	Truncated bool   `json:"truncated,omitempty"`
}

// SetAsker registers ask before serving when the agent is enabled.
func (s *Server) SetAsker(asker Asker) {
	if !s.cfg.Agent.Enabled || asker == nil {
		return
	}
	addTool(s, &mcpgo.Tool{
		Name:        "ask",
		Description: "Prefer ask for natural-language questions about the graph. It answers in one call and spends the server's LLM budget each time. Use search, get_entity, get_chunks, or traverse for targeted lookups.",
	}, func(ctx context.Context, req *mcpgo.CallToolRequest, args askArgs) (*mcpgo.CallToolResult, askOut, error) {
		out, err := handleAsk(ctx, req, args, asker)
		return nil, out, err
	})
}

func handleAsk(ctx context.Context, req *mcpgo.CallToolRequest, args askArgs, asker Asker) (askOut, error) {
	question := strings.TrimSpace(args.Question)
	if question == "" {
		return askOut{}, argerr.New("question is required")
	}
	if args.Domain != "" && !graph.ValidDomain(args.Domain) {
		return askOut{}, argerr.New("invalid domain slug")
	}

	var progress func(context.Context, int, string)
	if token := req.Params.GetProgressToken(); token != nil {
		progress = func(ctx context.Context, step int, tool string) {
			_ = req.Session.NotifyProgress(ctx, &mcpgo.ProgressNotificationParams{
				ProgressToken: token,
				Progress:      float64(step),
				Message:       tool,
			})
		}
	}
	result, err := asker.Ask(ctx, question, args.Domain, progress)
	if errors.Is(err, context.DeadlineExceeded) {
		return askOut{}, errors.New("the agent timed out before answering")
	}
	if err != nil {
		return askOut{}, err
	}
	out := askOut{Answer: result.Answer, Steps: result.Steps}
	if args.IncludeEvidence {
		out.includeEvidence(result.Evidence)
	}
	return out, nil
}

const askResultCap = 32 << 10

func (out *askOut) includeEvidence(items []AskEvidence) {
	evidence := make([]any, 0)
	out.Evidence = &evidence
	// Budget one JSON copy, including the escaped answer, steps and empty
	// array. The SDK also repeats that copy as text content.
	envelope, _ := json.Marshal(out)
	if room := askResultCap - len(envelope); room >= 0 {
		if full, fits := fitAskEvidence(items, room, false); fits {
			evidence = full
			return
		}
	}
	out.Truncated = true
	envelope, _ = json.Marshal(out)
	room := askResultCap - len(envelope)
	if room < 0 {
		out.Evidence = nil
		return
	}
	evidence, _ = fitAskEvidence(items, room, true)
}

// fitAskEvidence budgets the array contents, including separators. If the
// entire array cannot fit, divide the space among the remaining items.
func fitAskEvidence(items []AskEvidence, room int, allowPreviews bool) ([]any, bool) {
	// Keep raw encoded items for byte accounting. The SDK infers a byte-array
	// schema for json.RawMessage, so expose the items as arbitrary JSON.
	evidence := make([]any, 0)
	remaining := room + 1 // The first item needs no comma.
	for i, item := range items {
		limit := remaining - 1
		if allowPreviews {
			limit = remaining/(len(items)-i) - 1
		}
		raw, err := json.Marshal(item)
		if err != nil || len(raw) > limit {
			if !allowPreviews {
				return nil, false
			}
			raw = shortenAskEvidence(item, limit)
		}
		if raw != nil {
			evidence = append(evidence, json.RawMessage(raw))
			remaining -= len(raw) + 1
		}
	}
	return evidence, true
}

func shortenAskEvidence(item AskEvidence, limit int) json.RawMessage {
	short := struct {
		AskEvidence
		Truncated bool `json:"truncated"`
	}{AskEvidence: item, Truncated: true}
	short.Result = nil
	base, err := json.Marshal(short)
	if err != nil || len(base) > limit {
		short.Arguments = nil
		base, _ = json.Marshal(short)
	}
	if len(base) > limit {
		return nil
	}
	if room := limit - len(base) - len(`,"result":`); room > 2 && len(item.Result) > 0 {
		short.Result = evidencePreview(item.Result, room)
	}
	raw, _ := json.Marshal(short)
	return raw
}

// Keep a full result if it fits. Otherwise find the longest UTF-8 prefix
// whose JSON string fits after escaping.
func evidencePreview(result json.RawMessage, room int) json.RawMessage {
	if raw, err := json.Marshal(result); err == nil && len(raw) <= room {
		return raw
	}
	encode := func(end int) json.RawMessage {
		for end > 0 && end < len(result) && !utf8.RuneStart(result[end]) {
			end--
		}
		raw, _ := json.Marshal(string(result[:end]))
		return raw
	}
	maxEnd := min(len(result), room-2)
	if raw := encode(maxEnd); len(raw) <= room {
		return raw
	}
	firstTooLong := sort.Search(maxEnd, func(end int) bool { return len(encode(end)) > room })
	return encode(firstTooLong - 1)
}
