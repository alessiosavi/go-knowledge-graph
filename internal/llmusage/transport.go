package llmusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type idKey struct{}

// WithID tags an item or ask for the duration of its provider requests.
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, idKey{}, id)
}

// Usage is the provider-reported token usage of a metered request.
type Usage struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Reasoning  int64 `json:"reasoning"`
	Total      int64 `json:"total"`
}

// CallIDKey selects the identifier field in calls JSON.
type CallIDKey string

const (
	ItemIDKey CallIDKey = "item_id"
	AskIDKey  CallIDKey = "ask_id"
)

// Call is the safe, request-ordered telemetry for one provider attempt.
type Call struct {
	AskID           string `json:"ask_id,omitempty"`
	ItemID          string `json:"item_id,omitempty"`
	Host            string `json:"host"`
	Status          int    `json:"status"`
	LatencyMS       int64  `json:"latency_ms"`
	Stream          bool   `json:"stream"`
	Tools           bool   `json:"tools"`
	ResponseFormat  string `json:"response_format"`
	ToolCalls       int    `json:"tool_calls"`
	SignatureIn     bool   `json:"signature_in"`
	SignatureEchoed *bool  `json:"signature_echoed"`
	Usage           *Usage `json:"usage"`
}

// Transport meters one LLM host (including its port, if present).
// Calls for different IDs can run concurrently. Keep each ID's turns sequential
// so that its previous call has finished before checking for an echoed signature.
type Transport struct {
	base      *http.Transport
	ledger    *Ledger
	host      string
	callIDKey CallIDKey
	mu        sync.Mutex
	logs      map[string]*callLog
}

type callLog struct {
	calls      []Call
	signatures []json.RawMessage
}

// NewTransport clones the standard transport and configures the calls ID key.
func NewTransport(ledger *Ledger, host string, callIDKey CallIDKey) (*Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok || ledger == nil || host == "" {
		return nil, errors.New("eval transport needs the standard HTTP transport, a ledger and an LLM host")
	}
	if callIDKey != ItemIDKey && callIDKey != AskIDKey {
		return nil, errors.New("eval transport calls ID key must be item_id or ask_id")
	}
	return &Transport{base: base.Clone(), ledger: ledger, host: host, callIDKey: callIDKey, logs: make(map[string]*callLog)}, nil
}

// CloseIdleConnections releases the underlying transport's idle connections.
func (t *Transport) CloseIdleConnections() { t.base.CloseIdleConnections() }

// TakeCalls returns and clears the calls for a completed item or ask. Close
// all its response bodies before collecting it.
func (t *Transport) TakeCalls(id string) []Call {
	t.mu.Lock()
	defer t.mu.Unlock()
	if log := t.logs[id]; log != nil {
		delete(t.logs, id)
		return log.calls
	}
	return []Call{}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Host, t.host) {
		return t.base.RoundTrip(req)
	}
	started := time.Now()
	id, _ := req.Context().Value(idKey{}).(string)
	body, readErr := readRequest(req.Body)
	meta := requestMetadata(body)
	call := Call{Host: t.host, Stream: meta.Stream, Tools: len(meta.Tools) != 0, ResponseFormat: meta.ResponseFormat.Type}
	if t.callIDKey == AskIDKey {
		call.AskID = id
	} else {
		call.ItemID = id
	}
	if id == "" {
		return nil, stopLedger(errors.Join(ErrLedger, errors.New("metered request is missing an item or ask ID")))
	}
	log, index := t.begin(id, &call, meta.signatures())
	record := func(signatures []json.RawMessage) {
		call.LatencyMS = time.Since(started).Milliseconds()
		t.mu.Lock()
		defer t.mu.Unlock()
		log.calls[index] = call
		if index == len(log.calls)-1 {
			log.signatures = signatures
		}
	}
	if readErr != nil {
		record(nil)
		return nil, readErr
	}
	reserve, err := t.ledger.reserve(id, int64(len(body)))
	if err != nil {
		record(nil)
		return nil, err
	}
	send := req.Clone(req.Context())
	if req.Body != nil && req.Body != http.NoBody {
		send.Body = io.NopCloser(bytes.NewReader(body))
		// Each body replay needs its own reservation. http.Client redirects
		// can still replay via GetBody on the original request.
		send.GetBody = nil
	}
	resp, err := t.base.RoundTrip(send)
	if err != nil {
		record(nil)
		return nil, errors.Join(err, t.ledger.settle(reserve, nil))
	}
	call.Status = resp.StatusCode
	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	isSSE := strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream") ||
		(meta.Stream && resp.StatusCode >= 200 && resp.StatusCode < 300 && !strings.EqualFold(strings.TrimSpace(mediaType), "application/json"))
	resp.Body = &observedBody{
		body: resp.Body,
		sse:  isSSE,
		finish: func(observation responseObservation) error {
			call.Usage = observation.usage
			call.ToolCalls = observation.toolCalls
			call.SignatureIn = len(observation.signatures) != 0
			record(observation.signatures)
			return t.ledger.settle(reserve, call.Usage)
		},
	}
	return resp, nil
}

func readRequest(body io.ReadCloser) ([]byte, error) {
	if body == nil || body == http.NoBody {
		return nil, nil
	}
	defer body.Close()
	return io.ReadAll(body)
}

func (t *Transport) begin(id string, call *Call, echoed []json.RawMessage) (*callLog, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	log := t.logs[id]
	if log == nil {
		log = &callLog{}
		t.logs[id] = log
	}
	if len(log.signatures) != 0 {
		allEchoed := signaturesMatch(log.signatures, echoed)
		call.SignatureEchoed = &allEchoed
	}
	log.signatures = nil
	index := len(log.calls)
	log.calls = append(log.calls, *call)
	return log, index
}

func signaturesMatch(previous, echoed []json.RawMessage) bool {
	counts := make(map[string]int, len(echoed))
	for _, raw := range echoed {
		counts[string(raw)]++
	}
	for _, raw := range previous {
		key := string(raw)
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

type requestMeta struct {
	Stream         bool              `json:"stream"`
	Tools          []json.RawMessage `json:"tools"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
	Messages []struct {
		Role      string `json:"role"`
		ToolCalls []tool `json:"tool_calls"`
	} `json:"messages"`
}

func requestMetadata(raw []byte) requestMeta {
	var m requestMeta
	_ = json.Unmarshal(raw, &m)
	return m
}

func (m requestMeta) signatures() []json.RawMessage {
	var extras []json.RawMessage
	for _, msg := range m.Messages {
		if msg.Role == "assistant" {
			for _, t := range msg.ToolCalls {
				if nonEmptyExtra(t.Extra) {
					extras = append(extras, t.Extra)
				}
			}
		}
	}
	return extras
}
