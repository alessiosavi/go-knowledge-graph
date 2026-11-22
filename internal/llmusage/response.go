package llmusage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sync"
)

// The judge and agent both cap JSON responses at 4 MiB. Keep enough bytes
// for a whole JSON response or one SSE data line, while forwarding all reads.
const observationLimit = 4 << 20

type observedBody struct {
	body        io.ReadCloser
	mu          sync.Mutex
	sse         bool
	raw         []byte
	overflow    bool
	observation responseObservation
	finish      func(responseObservation) error
	finishErr   error
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finish != nil {
		b.observe(p[:n])
		if err != nil {
			if finishErr := b.complete(); finishErr != nil {
				err = finishErr
			}
		}
	}
	return n, err
}

func (b *observedBody) Close() error {
	err := b.body.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	return errors.Join(err, b.complete())
}

func (b *observedBody) complete() error {
	if b.finish != nil {
		if !b.overflow && len(b.raw) != 0 {
			b.observation.parse(b.raw, b.sse)
		}
		b.observation.finishStream()
		b.finishErr = b.finish(b.observation)
		b.finish, b.raw = nil, nil
		b.observation = responseObservation{}
	}
	return b.finishErr
}

func (b *observedBody) observe(data []byte) {
	if !b.sse {
		b.buffer(data)
		return
	}
	for len(data) > 0 {
		line, rest, hasNewline := bytes.Cut(data, []byte{'\n'})
		b.buffer(line)
		if !hasNewline {
			return
		}
		if !b.overflow {
			b.observation.parse(b.raw, true)
		}
		b.raw, b.overflow = b.raw[:0], false
		data = rest
	}
}

func (b *observedBody) buffer(data []byte) {
	if !b.overflow {
		if len(data) > observationLimit-len(b.raw) {
			b.raw, b.overflow = nil, true
			return
		}
		b.raw = append(b.raw, data...)
	}
}

type tool struct {
	Index int             `json:"index"`
	Extra json.RawMessage `json:"extra_content"`
}

type responseWire struct {
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Tools []tool `json:"tool_calls"`
		} `json:"message"`
		Delta struct {
			Tools []tool `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

type wireUsage struct {
	Prompt     *int64 `json:"prompt_tokens"`
	Completion *int64 `json:"completion_tokens"`
	Total      *int64 `json:"total_tokens"`
	Details    struct {
		Reasoning *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u *wireUsage) measured() *Usage {
	if u == nil || (u.Prompt == nil && u.Completion == nil && u.Total == nil && u.Details.Reasoning == nil) {
		return nil
	}
	count := func(n *int64) int64 {
		if n == nil {
			return 0
		}
		return *n
	}
	usage := &Usage{Prompt: count(u.Prompt), Completion: count(u.Completion), Reasoning: count(u.Details.Reasoning), Total: count(u.Total)}
	if usage.Prompt < 0 || usage.Completion < 0 || usage.Reasoning < 0 || usage.Total < 0 ||
		(usage.Total > 0 && usage.Total < usage.Prompt) ||
		(usage.Total == 0 && usage.Completion > math.MaxInt64-usage.Reasoning) {
		return nil
	}
	return usage
}

type streamTool struct{ choice, tool int }

type responseObservation struct {
	usage       *Usage
	toolCalls   int
	signatures  []json.RawMessage
	streamTools map[streamTool]json.RawMessage
}

func (o *responseObservation) parse(raw []byte, sse bool) {
	if sse {
		data, ok := bytes.CutPrefix(raw, []byte("data:"))
		if !ok {
			return
		}
		raw = bytes.TrimSpace(data)
		if bytes.Equal(raw, []byte("[DONE]")) {
			return
		}
	}
	var wire responseWire
	if json.Unmarshal(raw, &wire) != nil {
		return
	}
	if u := wire.Usage.measured(); u != nil {
		o.usage = u
	}
	for _, ch := range wire.Choices {
		if sse {
			for _, t := range ch.Delta.Tools {
				if o.streamTools == nil {
					o.streamTools = make(map[streamTool]json.RawMessage)
				}
				key := streamTool{ch.Index, t.Index}
				o.streamTools[key] = mergeExtra(o.streamTools[key], t.Extra)
			}
			continue
		}
		o.toolCalls += len(ch.Message.Tools)
		for _, t := range ch.Message.Tools {
			if nonEmptyExtra(t.Extra) {
				o.signatures = append(o.signatures, t.Extra)
			}
		}
	}
}

func (o *responseObservation) finishStream() {
	o.toolCalls += len(o.streamTools)
	for _, extra := range o.streamTools {
		if nonEmptyExtra(extra) && json.Valid(extra) {
			o.signatures = append(o.signatures, extra)
		}
	}
}

func nonEmptyExtra(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0, bytes.Equal(raw, []byte("null")), bytes.Equal(raw, []byte(`""`)):
		return false
	case raw[0] == '{' || raw[0] == '[':
		return len(bytes.TrimSpace(raw[1:len(raw)-1])) != 0
	default:
		return true
	}
}

// Gemini may deliver an extra_content object as JSON string fragments.
func mergeExtra(current, next json.RawMessage) json.RawMessage {
	if !nonEmptyExtra(next) {
		return current
	}
	assembling := len(current) != 0 && !json.Valid(current)
	if next[0] == '"' {
		var fragment string
		_ = json.Unmarshal(next, &fragment)
		start := bytes.TrimSpace([]byte(fragment))
		if assembling || bytes.HasPrefix(start, []byte("{")) || bytes.HasPrefix(start, []byte("[")) {
			next = json.RawMessage(fragment)
		}
	}
	if assembling {
		return append(current, next...)
	}
	return next
}
