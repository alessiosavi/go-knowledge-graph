package mcp

import (
	"context"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
	mcpgo "github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolHandlerEnteredKey struct{}

func addTool[In, Out any](s *Server, tool *mcpgo.Tool, handle mcpgo.ToolHandlerFor[In, Out]) {
	s.tools[tool.Name] = struct{}{}
	mcpgo.AddTool(s.sdk, tool, func(ctx context.Context, req *mcpgo.CallToolRequest, args In) (*mcpgo.CallToolResult, Out, error) {
		if entered, ok := ctx.Value(toolHandlerEnteredKey{}).(*bool); ok {
			*entered = true
		}
		return handle(ctx, req, args)
	})
}

// Measure around the SDK dispatch so input validation and output encoding
// are included. A tool error before entry into the typed handler is an
// SDK argument validation or decoding error.
func (s *Server) measureToolCalls(next mcpgo.MethodHandler) mcpgo.MethodHandler {
	return func(ctx context.Context, method string, req mcpgo.Request) (mcpgo.Result, error) {
		call, ok := req.(*mcpgo.CallToolRequest)
		if s.metrics == nil || !ok || call.Params == nil {
			return next(ctx, method, req)
		}
		op := call.Params.Name
		if _, known := s.tools[op]; !known {
			return next(ctx, method, req)
		}
		start := time.Now()
		entered := false
		res, err := next(context.WithValue(ctx, toolHandlerEnteredKey{}, &entered), method, req)
		elapsed := time.Since(start)
		outcome := obs.ToolOutcome(err)
		if result, ok := res.(*mcpgo.CallToolResult); err == nil && ok && result != nil && result.IsError {
			outcome = obs.OutcomeError
			if !entered || obs.ToolOutcome(result.GetError()) == obs.OutcomeClientError {
				outcome = obs.OutcomeClientError
			}
		}
		s.metrics.ObserveRequest("mcp", op, outcome, elapsed)
		return res, err
	}
}
