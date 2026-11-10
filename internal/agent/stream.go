package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// streamPing is the SSE heartbeat interval; tests override it.
var streamPing = 15 * time.Second

// streamEventsBuf bounds the in-flight event queue between the ask loop and
// the SSE writer.
const streamEventsBuf = 64

// StreamHandler serves POST /v1/agent/ask/stream: one question, one bounded
// read-only agent run, rendered as Server-Sent Events — `tool` when a call
// is about to execute, `delta` fragments of the answer, `done` with the
// full askResponse (the authoritative payload), `error` classified — plus a
// periodic `: ping` heartbeat. The ask runs in one goroutine feeding an
// event channel; this handler owns the response writer and serializes
// events and heartbeats through one select loop, stopping on
// request-context cancellation. Mount it behind the REST bearer policy like
// the MCP endpoint; a disabled agent answers 501 before any SSE byte. With
// streaming off the endpoint stays SSE but emits tool and done events only.
func (s *Server) StreamHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Agent.Enabled {
			writeError(w, http.StatusNotImplemented, "agent_disabled", "the ask agent is not enabled on this server")
			return
		}
		question, ok := decodeAskQuestion(w, r)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
		defer cancel()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_ = rc.Flush()

		type askResult struct {
			resp askResponse
			err  error
		}
		events := make(chan streamEvent, streamEventsBuf)
		done := make(chan askResult, 1)
		go func() {
			resp, err := s.run(ctx, question, func(ev streamEvent) error {
				select {
				case events <- ev:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			done <- askResult{resp: resp, err: err}
		}()

		writeEvent := func(name string, payload any) error {
			if err := sseWrite(w, name, payload); err != nil {
				return err
			}
			return rc.Flush()
		}

		ticker := time.NewTicker(streamPing)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
					return
				}
				if err := rc.Flush(); err != nil {
					return
				}
			case ev := <-events:
				if err := writeEvent(ev.Kind, eventPayload(ev)); err != nil {
					return
				}
			case res := <-done:
				// Drain the events queued before the result so nothing is
				// reordered past the terminal frame.
				for {
					select {
					case ev := <-events:
						if err := writeEvent(ev.Kind, eventPayload(ev)); err != nil {
							return
						}
					default:
						if res.err != nil {
							slog.Warn("agent: ask stream failed", "err", res.err)
							kind, detail := classifyStreamError(res.err)
							_ = writeEvent("error", errorBody{Error: kind, Detail: detail})
						} else {
							_ = writeEvent("done", res.resp)
						}
						return
					}
				}
			}
		}
	})
}

// eventPayload shapes one streamEvent for its SSE frame.
func eventPayload(ev streamEvent) any {
	if ev.Kind == "tool" {
		return map[string]any{"name": ev.Tool, "arguments": ev.Arguments}
	}
	return map[string]any{"text": ev.Text}
}

// classifyStreamError maps an ask failure onto the SSE error event,
// mirroring the JSON endpoint's classifications (never the endpoint or key).
func classifyStreamError(err error) (kind, detail string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return "agent_timeout", "the agent timed out before answering"
	}
	return "agent_error", err.Error()
}

// sseWrite writes one SSE frame with a JSON payload.
func sseWrite(w io.Writer, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}
