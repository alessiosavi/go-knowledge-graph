package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

var update = flag.Bool("update", false, "rewrite the ask wire snapshots")

const (
	askWireQuestion = `{"question":"What does the desk lamp offer?","domain":"catalog"}`
	askWireDocID    = "11111111111111111111111111111111"
	askWireChunkID  = askWireDocID + "-c0"
	askWireAnswer   = "**Desk lamp** (`shop.example:1`) has two brightness settings.\n\n" +
		"- Source: `" + askWireChunkID + "`."
	askProviderGolden       = "ask-provider-requests.golden.json"
	askStreamProviderGolden = "ask-stream-provider-requests.golden.json"
)

func TestAskWireSnapshot(t *testing.T) {
	s, stub := newAskWireServer(t, false)
	rr := postAsk(t, s.AskHandler(), askWireQuestion)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("/ask status=%d content-type=%q, want 200 application/json\n%s", rr.Code, rr.Header().Get("Content-Type"), rr.Body)
	}
	checkAskProviderGolden(t, askProviderGolden, stub)
	checkAskWireGolden(t, "ask.golden.json", rr.Body.Bytes())
}

func checkAskProviderGolden(t *testing.T, name string, stub *llmStub) {
	t.Helper()
	stub.srv.Close() // Wait for the stub's writes before reading its capture.
	if got := len(stub.requestBodies); got != 2 {
		t.Fatalf("%s: provider turns = %d, want 2", name, got)
	}

	requests := make([][]byte, len(stub.requestBodies))
	for i, raw := range stub.requestBodies {
		requests[i] = digestAskSystemMessages(t, i+1, raw)
	}
	requestJSON := append([]byte{'['}, bytes.Join(requests, []byte{','})...)
	requestJSON = append(requestJSON, ']')
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, requestJSON, "", "  "); err != nil {
		t.Fatalf("indent provider requests: %v", err)
	}
	checkAskWireGolden(t, name, append(pretty.Bytes(), '\n'))
}

func TestAskStreamWireSnapshot(t *testing.T) {
	// Keep the heartbeat beyond the ask deadline. The snapshot captures all
	// emitted frames, and TestStreamHandlerHeartbeat covers the timed ping.
	oldPing := streamPing
	streamPing = time.Hour
	t.Cleanup(func() { streamPing = oldPing })

	s, stub := newAskWireServer(t, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/agent/ask/stream", strings.NewReader(askWireQuestion))
	req.Header.Set("Content-Type", "application/json")
	rr := &askStreamRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.StreamHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("/ask/stream status=%d content-type=%q, want 200 text/event-stream\n%s", rr.Code, rr.Header().Get("Content-Type"), rr.Body)
	}
	frameEnd := []byte("\n\n")
	offset := 0
	for rest := rr.Body.Bytes(); len(rest) != 0; {
		frame, remaining, ok := bytes.Cut(rest, frameEnd)
		if !ok {
			t.Fatalf("incomplete SSE frame at byte %d", offset)
		}
		offset += len(frame) + len(frameEnd)
		if !slices.Contains(rr.flushOffsets, offset) {
			t.Fatalf("SSE frame ending at byte %d was not flushed (flush offsets: %v)", offset, rr.flushOffsets)
		}
		rest = remaining
	}
	checkAskWireGolden(t, "ask-stream.golden.txt", rr.Body.Bytes())
	checkAskProviderGolden(t, askStreamProviderGolden, stub)
}

type askStreamRecorder struct {
	*httptest.ResponseRecorder
	flushOffsets []int
}

func (r *askStreamRecorder) Flush() {
	r.flushOffsets = append(r.flushOffsets, r.Body.Len())
	r.ResponseRecorder.Flush()
}

func newAskWireServer(t *testing.T, stream bool) (*Server, *llmStub) {
	t.Helper()
	searchCall := scriptedCall{"search-1", "search", `{"query":"desk lamp","limit":2}`}
	getCall := scriptedCall{"get-1", "get_product", `{"store":"shop.example","product_id":"shop.example:1"}`}
	stub := newLLMStub(t, toolCallsReply(searchCall, getCall), contentReply(askWireAnswer))
	if stream {
		stub.withStream(
			sseTool(0, searchCall.id, searchCall.name, searchCall.args),
			sseTool(1, getCall.id, getCall.name, getCall.args),
			sseFinish("tool_calls"), sseDone,
		).withStream(sseContent("**Desk lamp**"), sseContent(askWireAnswer[len("**Desk lamp**"):]), sseFinish("stop"), sseDone)
	}

	product := graph.Product{
		ID: "22222222222222222222222222222222", Store: "shop.example", ProductID: "shop.example:1",
		Domain: "catalog", Title: "Desk lamp", Category: "Lighting", Link: "https://shop.example/products/1",
	}
	const heightDefinition = "Adjustable height of the lamp."
	detail := graph.ProductDetailWithMentions{
		ProductDetail: graph.ProductDetail{
			Product: product,
			Features: []graph.ProductEvidence{{
				Feature: graph.ProductFeatureRev{
					ProductFeature: graph.ProductFeature{
						ID: "height", Name: "Height", Kind: "measure", Cardinality: "multi", CanonicalUnit: new("mm"),
					},
					Key: "0123456789abcdef/height", Rev: "0123456789abcdef", Definition: heightDefinition,
					DefinitionDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(heightDefinition))), Replaces: new(""),
				},
				SourceKey: "height", Raw: "30–40 cm", Items: []string{"30–40 cm"}, SplitStatus: "split",
				Typed: &productobs.TypedValue{
					Status: "ok", Qualifier: new("range"), Inclusive: new(true), UnitIn: new("cm"), CanonicalUnit: new("mm"),
					Lo: new("30"), Hi: new("40"), LoCanonical: new("300"), HiCanonical: new("400"),
				},
				Numeric: true, ReleaseID: "release-1",
			}},
			Pages: []graph.ProductPage{{
				ID: askWireDocID, Title: "Desk lamp guide", Source: "https://shop.example/guides/lamp",
				UpdatedAt: "2026-01-02T00:00:00Z", Via: []string{"url"}, ChunkIDs: []string{askWireChunkID}, ChunkCount: 1,
			}},
		},
		ProductMentionFields: &graph.ProductMentionFields{Mentions: []graph.ProductChunkMention{}},
	}
	documentDate := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	dateSource := "caller"
	cfg := enabledCfg(stub.srv.URL)
	maxTokens := 2048
	cfg.Agent.Stream, cfg.Agent.MaxTokens = stream, &maxTokens
	s := NewServer(cfg, nil, nil)
	s.store = &productSearchToolStore{
		graphStore: agentMentionStore{detail: detail},
		chunks: []graph.ChunkSearchRow{{
			Domain: "catalog", ID: askWireChunkID, DocID: askWireDocID, Title: "Desk lamp guide",
			Text: "The desk lamp has two brightness settings.", DocumentDate: &documentDate, DocumentDateSource: &dateSource,
		}},
		products: func(context.Context, string, string, int) ([]graph.Product, error) {
			return []graph.Product{product}, nil
		},
	}
	return s, stub
}

type askPromptDigest struct {
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

func TestAskSystemMessageDigestRejectsMalformedRequest(t *testing.T) {
	const inputEnv = "GKG_TEST_ASK_DIGEST_INPUT"
	if input := os.Getenv(inputEnv); input != "" {
		digestAskSystemMessages(t, 1, []byte(input))
		return
	}
	for _, tc := range []struct {
		name, input, want string
	}{
		{"missing messages", `{}`, "expected provider messages array"},
		{"null messages", `{"messages":null}`, "expected provider messages array"},
		{"missing content", `{"messages":[{"role":"system"}]}`, "expected system message content string"},
		{"null content", `{"messages":[{"role":"system","content":null}]}`, "expected system message content string"},
		{"numeric content", `{"messages":[{"role":"system","content":42}]}`, "expected system message content string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAskSystemMessageDigestRejectsMalformedRequest$")
			cmd.Env = append(cmd.Environ(), inputEnv+"="+tc.input)
			out, err := cmd.CombinedOutput()
			if err == nil || !bytes.Contains(out, []byte(tc.want)) {
				t.Errorf("redaction must abort with %q, got %v\n%s", tc.want, err, out)
			}
		})
	}
}

// digestAskSystemMessages hashes the decoded system content to keep real
// storefront examples out of the fixture. All other fields keep their wire
// representation apart from indentation. The response snapshots use the
// complete, verbatim bodies. No volatile fields are masked.
func digestAskSystemMessages(t *testing.T, turn int, body []byte) []byte {
	t.Helper()
	messages, start := askJSONField(t, body, "messages")
	if messages == nil {
		t.Fatalf("turn %d: expected provider messages array, field is missing", turn)
	}
	dec := json.NewDecoder(bytes.NewReader(messages))
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		t.Fatalf("turn %d: expected provider messages array, got %v (%v)", turn, token, err)
	}
	var redacted []byte
	previous := 0
	for dec.More() {
		var message json.RawMessage
		if err := dec.Decode(&message); err != nil {
			t.Fatalf("turn %d: decode provider message: %v", turn, err)
		}
		rawRole, _ := askJSONField(t, message, "role")
		if rawRole == nil {
			continue
		}
		var role string
		if err := json.Unmarshal(rawRole, &role); err != nil {
			t.Fatalf("turn %d: decode provider message role: %v", turn, err)
		}
		if role != "system" {
			continue
		}
		content, offset := askJSONField(t, message, "content")
		var prompt *string
		if err := json.Unmarshal(content, &prompt); err != nil || prompt == nil {
			t.Fatalf("turn %d: expected system message content string (%v)", turn, err)
		}
		digest, err := json.Marshal(askPromptDigest{fmt.Sprintf("%x", sha256.Sum256([]byte(*prompt))), len(*prompt)})
		if err != nil {
			t.Fatalf("turn %d: encode system prompt digest: %v", turn, err)
		}
		contentStart := start + int(dec.InputOffset()) - len(message) + offset
		redacted = append(redacted, body[previous:contentStart]...)
		redacted = append(redacted, digest...)
		previous = contentStart + len(content)
	}
	return append(redacted, body[previous:]...)
}

// askJSONField returns the raw member value and its byte offset so replacing
// that value leaves the remaining JSON literals and key order intact.
func askJSONField(t *testing.T, object []byte, field string) (json.RawMessage, int) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(object))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("expected object looking for %q, got %v (%v)", field, token, err)
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			t.Fatalf("decode key looking for %q: %v", field, err)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			t.Fatalf("decode field %q: %v", key, err)
		}
		if key == field {
			return raw, int(dec.InputOffset()) - len(raw)
		}
	}
	return nil, 0
}

func checkAskWireGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "ask-wire", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(want, got) {
		return
	}
	if name == askProviderGolden || name == askStreamProviderGolden {
		wantDigests, gotDigests := askPromptDigests(t, want), askPromptDigests(t, got)
		for i := range min(len(wantDigests), len(gotDigests)) {
			if !slices.Equal(wantDigests[i], gotDigests[i]) {
				t.Errorf("turn %d: /ask system prompt bytes changed (-want +got)\n- %v\n+ %v", i+1, wantDigests[i], gotDigests[i])
			}
		}
	}
	// Quote a small window at the first mismatch so framing and whitespace
	// changes are visible even in the compact one-line payloads.
	i := 0
	for i < min(len(want), len(got)) && want[i] == got[i] {
		i++
	}
	lo := max(0, i-32)
	t.Errorf("%s differs at byte %d (line %d), -want (%d bytes) +got (%d bytes)\n- %q\n+ %q",
		path, i+1, bytes.Count(want[:i], []byte{'\n'})+1, len(want), len(got),
		want[lo:min(len(want), i+64)], got[lo:min(len(got), i+64)])
}

func askPromptDigests(t *testing.T, snapshot []byte) [][]string {
	t.Helper()
	var turns []struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(snapshot, &turns); err != nil {
		t.Fatalf("decode provider snapshot: %v", err)
	}
	digests := make([][]string, len(turns))
	for i, turn := range turns {
		for _, message := range turn.Messages {
			if message.Role != "system" || message.Content == nil {
				continue
			}
			var digest askPromptDigest
			if err := json.Unmarshal(message.Content, &digest); err != nil {
				t.Fatalf("turn %d: decode system prompt digest: %v", i+1, err)
			}
			digests[i] = append(digests[i], fmt.Sprintf("sha256=%s bytes=%d", digest.SHA256, digest.Bytes))
		}
	}
	return digests
}
