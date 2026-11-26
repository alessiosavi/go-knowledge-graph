package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const answerTestDomain = "catalog"

type answerTestStore struct {
	*productSearchToolStore
	documents []graph.Document
	detail    graph.ProductDetailWithMentions
	found     graph.ProductSearchResult
	index     *graph.ProductMentionIndex
	readChunk func(string) (graph.Chunk, error)
}

func (s *answerTestStore) DocumentsInWindow(_ context.Context, _ graph.Status, domain string, limit int, _ graph.DateWindow) ([]graph.Document, error) {
	if domain != answerTestDomain {
		return nil, fmt.Errorf("unexpected domain %q", domain)
	}
	return s.documents[:min(limit, len(s.documents))], nil
}

func (s *answerTestStore) GetProductWithMentions(_ context.Context, lookup graph.ProductLookup) (graph.ProductDetailWithMentions, graph.ProductMentionRead, error) {
	if lookup.Domain != answerTestDomain || lookup.SeedDomain != answerTestDomain || lookup.ID != s.detail.ID {
		return graph.ProductDetailWithMentions{}, graph.ProductMentionRead{}, graph.ErrNotFound
	}
	return s.detail, graph.ProductMentionRead{}, nil
}

func (s *answerTestStore) FindProductsBudget(_ context.Context, req graph.ProductSearchRequest, _ int) (graph.ProductSearchResult, error) {
	if req.Domain != answerTestDomain {
		return graph.ProductSearchResult{}, fmt.Errorf("unexpected domain %q", req.Domain)
	}
	return s.found, nil
}

func (s *answerTestStore) LoadProductMentionIndex(context.Context) (*graph.ProductMentionIndex, error) {
	return s.index, nil
}

func (s *answerTestStore) ChunkNeighbourhood(_ context.Context, id string, _ int) (graph.Chunk, []graph.Chunk, error) {
	chunk, err := s.readChunk(id)
	return chunk, nil, err
}

func newAnswerTestServer(t *testing.T, store graphStore, steps int, replies ...stubResponse) (*Server, *llmStub) {
	t.Helper()
	stub := newLLMStub(t, replies...)
	cfg := enabledCfg(stub.srv.URL)
	cfg.Agent.MaxSteps = steps
	s := NewServer(cfg, nil, nil)
	s.store = store
	s.llm.backoff = nil
	return s, stub
}

func runTestAnswer(t *testing.T, s *Server, stub *llmStub, stores []string) answerRun {
	t.Helper()
	run, err := s.runAnswer(t.Context(), AnswerRequest{Question: "Find a lamp.", Domain: answerTestDomain}, stores)
	stub.srv.Close() // Wait for all requests to be recorded.
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func answerToolResults(t *testing.T, stub *llmStub, turn int) map[string]string {
	t.Helper()
	results := map[string]string{}
	if len(stub.requests) <= turn {
		t.Fatalf("request count = %d, no request at index %d", len(stub.requests), turn)
	}
	for _, msg := range requestMessages(t, stub.requests[turn]) {
		if msg["role"] != "tool" {
			continue
		}
		id, content := msg["tool_call_id"].(string), msg["content"].(string)
		if len(content) > toolResultCap || !json.Valid([]byte(content)) {
			t.Fatalf("tool result %s has %d bytes or invalid JSON", id, len(content))
		}
		results[id] = content
	}
	return results
}

func TestAnswerToolAllowlist(t *testing.T) {
	names := []string{"cypher", "get_entity", "traverse", "status", "unknown"}
	var calls []scriptedCall
	for _, name := range names {
		calls = append(calls, scriptedCall{name, name, `{}`})
	}
	// The fake has no implementations for the disallowed tools.
	s, stub := newAnswerTestServer(t, &answerTestStore{}, 2, toolCallsReply(calls...), contentReply("No matching product."))
	run := runTestAnswer(t, s, stub, nil)
	if run.Steps != 1 || !slices.Equal(run.Tools, names) {
		t.Fatalf("trace = %d steps, %v", run.Steps, run.Tools)
	}
	var offered []string
	for _, v := range stub.requests[0]["tools"].([]any) {
		offered = append(offered, v.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	slices.Sort(offered)
	if !slices.Equal(offered, []string{"find_products", "get_chunks", "get_product", "list_documents", "list_features", "search"}) {
		t.Fatalf("offered tools = %v", offered)
	}
	results := answerToolResults(t, stub, 1)
	for _, name := range names {
		if results[name] != `{"error":"tool not available in answer mode"}` {
			t.Errorf("%s result = %s", name, results[name])
		}
	}
}

func TestAnswerNoPlannerFallback(t *testing.T) {
	s, stub := newAnswerTestServer(t, nil, 2, stubResponse{status: http.StatusBadRequest, body: "tools not supported"})
	_, err := s.runAnswer(t.Context(), AnswerRequest{Question: "Find a lamp.", Domain: answerTestDomain}, nil)
	stub.srv.Close()
	if err == nil || len(stub.requests) != 1 {
		t.Fatalf("requests = %d, err = %v, want one provider error", len(stub.requests), err)
	}
	if kind, _ := classifyStreamError(err); kind != "agent_error" {
		t.Fatalf("error class = %q", kind)
	}
}

func TestAnswerGetChunksPassesDomainStores(t *testing.T) {
	product := func(id, store, domain string) graph.Product {
		return graph.Product{ID: id, ProductID: store + ":" + id, Store: store, Domain: domain}
	}
	observations := []graph.ProductIdentifierObservation{
		{Product: product("allowed", "shop.example", answerTestDomain), Feature: "catalogue_code", ValueFold: new("AB12345")},
		{Product: product("wrong-store", "other.example", answerTestDomain), Feature: "catalogue_code", ValueFold: new("CD67890")},
		{Product: product("wrong-domain", "shop.example", "other"), Feature: "catalogue_code", ValueFold: new("EF24680")},
	}
	for _, tc := range []struct {
		name         string
		stores, want []string
	}{
		{"bound stores", []string{"shop.example"}, []string{"shop.example:allowed"}},
		{"no bound stores", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &answerTestStore{
				index: graph.BuildProductMentionIndex(observations),
				readChunk: func(id string) (graph.Chunk, error) {
					return graph.Chunk{ID: id, Domain: answerTestDomain, Text: "AB12345 and CD67890 and EF24680"}, nil
				},
			}
			s, stub := newAnswerTestServer(t, st, 2, toolCallReply("chunks", "get_chunks", `{"ids":["chunk"]}`), contentReply("Lamp"))
			run := runTestAnswer(t, s, stub, tc.stores)
			out := decodeAnswerToolResult[struct {
				Chunks []struct {
					Products []struct {
						ProductID string `json:"product_id"`
					}
					Total int `json:"products_total"`
				}
			}](t, answerToolResults(t, stub, 1)["chunks"])
			if len(out.Chunks) != 1 || out.Chunks[0].Total != len(tc.want) {
				t.Fatalf("scoped chunks = %+v", out.Chunks)
			}
			var got []string
			for _, p := range out.Chunks[0].Products {
				got = append(got, p.ProductID)
			}
			wantIDs := []string{"chunk"}
			if len(tc.want) != 0 {
				wantIDs = append(wantIDs, "allowed", tc.want[0])
			}
			if !slices.Equal(got, tc.want) || !slices.Equal(sortedAnswerIDs(run.Seen.KnownIDs()), sortedAnswerIDs(wantIDs)) {
				t.Fatalf("scoped products = %v, seen ids = %v", got, run.Seen.KnownIDs())
			}
		})
	}
}

func sortedAnswerIDs(ids []string) []string {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	return sorted
}

func TestAnswerListDocumentsFitted(t *testing.T) {
	st := &answerTestStore{}
	title := strings.Repeat("<lamp>", 250)
	for i := range 100 {
		st.documents = append(st.documents, graph.Document{ID: fmt.Sprintf("doc-%03d", i), Domain: answerTestDomain, Title: title})
	}
	s, stub := newAnswerTestServer(t, st, 2, toolCallReply("list", "list_documents", `{"limit":100}`), contentReply("Lamp"))
	run := runTestAnswer(t, s, stub, nil)
	content := answerToolResults(t, stub, 1)["list"]
	if len(content) > agentResultBudget {
		t.Fatalf("fitted result has %d bytes, budget is %d", len(content), agentResultBudget)
	}
	result := decodeAnswerToolResult[struct {
		Documents        []documentOut `json:"documents"`
		DocumentsOmitted int           `json:"documents_omitted"`
	}](t, content)
	if len(result.Documents) == 0 || len(result.Documents) >= len(st.documents) || len(run.Seen.KnownIDs()) != 0 {
		t.Fatalf("fitted documents = %d, seen ids = %v", len(result.Documents), run.Seen.KnownIDs())
	}
	if want := len(st.documents) - len(result.Documents); result.DocumentsOmitted != want {
		t.Errorf("documents_omitted = %d, want %d", result.DocumentsOmitted, want)
	}
	for i, doc := range result.Documents {
		if doc.Title != title || doc.ID != st.documents[i].ID {
			t.Fatalf("document %d did not survive intact", i)
		}
	}
}

func TestAskListDocumentsOmitsOmissionCount(t *testing.T) {
	doc := graph.Document{ID: "doc", Domain: answerTestDomain, Title: strings.Repeat("L", toolResultCap)}
	st := &answerTestStore{documents: []graph.Document{doc}}
	s, _ := newAnswerTestServer(t, st, 2, toolCallReply("list", "list_documents", `{}`), contentReply("Lamp"))
	resp, err := s.Ask(t.Context(), AskRequest{Question: "List documents.", Domain: answerTestDomain})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Evidence) != 1 || resp.Evidence[0].Tool != "list_documents" {
		t.Fatal("expected one list_documents evidence item")
	}
	result := unmarshalResult[map[string]json.RawMessage](t, resp.Evidence[0].Result)
	if _, exists := result["documents_omitted"]; exists {
		t.Error("ask evidence contains documents_omitted")
	}
	docs := unmarshalResult[[]documentOut](t, result["documents"])
	if len(docs) != 1 || docs[0].ID != doc.ID || docs[0].Title != doc.Title {
		t.Error("ask evidence did not preserve the full document")
	}
}

func TestAnswerEntitySearchFitted(t *testing.T) { testAnswerSearchFitted(t, "entity") }
func TestAnswerChunkSearchFitted(t *testing.T)  { testAnswerSearchFitted(t, "chunk") }

func testAnswerSearchFitted(t *testing.T, kind string) {
	t.Helper()
	st := &productSearchToolStore{}
	title := strings.Repeat("<lamp>", 250)
	for i := range 100 {
		id := fmt.Sprintf("hit-%03d", i)
		st.entities = append(st.entities, graph.EntitySearchRow{ID: id, Name: title, Domain: answerTestDomain})
		st.chunks = append(st.chunks, graph.ChunkSearchRow{ID: id, Title: title, Text: "Lamp passage.", Domain: answerTestDomain})
	}
	args := fmt.Sprintf(`{"query":"lamp","kind":%q,"limit":100}`, kind)
	s, stub := newAnswerTestServer(t, st, 2, toolCallReply("search", "search", args), contentReply("Lamp"))
	run := runTestAnswer(t, s, stub, nil)
	result := decodeAnswerToolResult[searchOut](t, answerToolResults(t, stub, 1)["search"])
	if len(result.Hits) == 0 || result.HitsOmitted == 0 || len(result.Hits)+result.HitsOmitted != 100 {
		t.Fatalf("retained %d and omitted %d of 100 hits", len(result.Hits), result.HitsOmitted)
	}
	for _, hit := range result.Hits {
		if hit.Kind != kind || hit.Title != title || run.Seen.Chunk(hit.ID) != (kind == "chunk") {
			t.Fatalf("unexpected retained hit %q", hit.ID)
		}
	}
	if run.Seen.Chunk("hit-099") {
		t.Error("omitted search hit was indexed")
	}
}

func TestAnswerToolErrorBounded(t *testing.T) {
	st := &answerTestStore{readChunk: func(string) (graph.Chunk, error) {
		return graph.Chunk{}, errors.New(strings.Repeat("<é>", toolResultCap))
	}}
	s, stub := newAnswerTestServer(t, st, 2, toolCallReply("chunks", "get_chunks", `{"ids":["choice"]}`), contentReply("No match."))
	run := runTestAnswer(t, s, stub, nil)
	result := decodeAnswerToolResult[struct{ Error string }](t, answerToolResults(t, stub, 1)["chunks"])
	if result.Error == "" || len(run.Seen.KnownIDs()) != 0 {
		t.Fatal("expected an error envelope and no indexed ids")
	}
}

func decodeAnswerToolResult[T any](t *testing.T, content string) T {
	t.Helper()
	var result T
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	return result
}

func TestAnswerContextMessageOrder(t *testing.T) {
	const question = "Compare them."
	for _, tc := range []struct {
		name string
		ctx  *AnswerContext
	}{
		{"absent", nil},
		{"present", &AnswerContext{ProductIDs: []string{"shop.example:123", "shop.example:456"}, History: []HistoryTurn{
			{Role: "user", Text: "  Find a lamp.\nWith dimming."},
			{Role: "assistant", Text: "Two matches.  "},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, stub := newAnswerTestServer(t, nil, 2, contentReply("Both dim."))
			run, err := s.runAnswer(t.Context(), AnswerRequest{Question: question, Domain: answerTestDomain, Context: tc.ctx}, nil)
			stub.srv.Close()
			if err != nil {
				t.Fatal(err)
			}
			msgs := requestMessages(t, stub.requests[0])
			want := 2
			if tc.ctx != nil {
				want++
			}
			if len(msgs) != want || msgs[0]["role"] != "system" || msgs[want-1]["role"] != "user" || msgs[want-1]["content"] != question {
				t.Fatalf("unexpected messages order: %v", msgs)
			}
			if tc.ctx == nil {
				return
			}
			body := msgs[1]["content"].(string)
			if msgs[1]["role"] != "user" || !strings.HasPrefix(body, "Conversation context (same shop):") {
				t.Fatal("missing separate context message")
			}
			for _, text := range []string{"shop.example:123", "shop.example:456", tc.ctx.History[0].Text, tc.ctx.History[1].Text} {
				if !strings.Contains(body, text) {
					t.Errorf("context message missing verbatim %q", text)
				}
			}
			if len(run.Seen.KnownIDs()) != 0 {
				t.Fatal("context-only ids must not become citable")
			}
		})
	}
}

func runAnswerWithBudgetFinal(t *testing.T, response stubResponse, generation *GenerationSettings) (answerRun, error) {
	t.Helper()
	s, stub := newAnswerTestServer(t, nil, 1, toolCallReply("c1", "unknown", `{}`), response)
	run, err := s.runAnswer(t.Context(), AnswerRequest{Question: "Find a lamp.", Domain: answerTestDomain, Generation: generation}, nil)
	stub.srv.Close()
	if len(stub.requests) != 2 || stub.requests[1]["tools"] != nil || stub.requests[1]["tool_choice"] != nil {
		t.Fatal("expected two requests and a tools-less final turn")
	}
	msgs := requestMessages(t, stub.requests[1])
	if msgs[len(msgs)-1]["content"] != answerFinalPrompt {
		t.Fatal("expected the answer final prompt")
	}
	return run, err
}

func TestAnswerBudgetFinalUnknownMarkOnly(t *testing.T) {
	run, err := runAnswerWithBudgetFinal(t, contentReply("[[unknown]]"), nil)
	if err != nil || !run.BudgetFinal || run.Outcome != outcomeAnswered || run.Text != "[[unknown]]" || run.Seen.Chunk("unknown") {
		t.Fatalf("run=%+v, err=%v", run, err)
	}
}

func TestAnswerBudgetFinal403(t *testing.T) { testAnswerBudgetFinalAuth(t, http.StatusForbidden) }
func TestAnswerBudgetFinal401(t *testing.T) { testAnswerBudgetFinalAuth(t, http.StatusUnauthorized) }

func testAnswerBudgetFinalAuth(t *testing.T, status int) {
	t.Helper()
	_, err := runAnswerWithBudgetFinal(t, stubResponse{status: status}, nil)
	var provider *providerError
	if !errors.As(err, &provider) || provider.status != status {
		t.Fatalf("err = %v, want an upstream %d error", err, status)
	}
}

func TestAnswerBudgetFinal500(t *testing.T) {
	run, err := runAnswerWithBudgetFinal(t, stubResponse{status: http.StatusInternalServerError}, nil)
	if err != nil || !run.BudgetFinal || run.Outcome != outcomeFinalTurnFailed || run.Text != "" {
		t.Fatalf("run=%+v, err=%v", run, err)
	}
}

func TestAnswerOutcomeTruncated(t *testing.T) {
	response := stubResponse{status: http.StatusOK, body: `{"choices":[{"message":{"content":"Compare the lamps"},"finish_reason":"length"}]}`}
	t.Run("normal", func(t *testing.T) {
		s, stub := newAnswerTestServer(t, nil, 2, response)
		run := runTestAnswer(t, s, stub, nil)
		if run.BudgetFinal || run.Outcome != outcomeTruncated || run.Text != "Compare the lamps" {
			t.Fatalf("run=%+v", run)
		}
	})
	t.Run("budget", func(t *testing.T) {
		run, err := runAnswerWithBudgetFinal(t, response, nil)
		if err != nil || !run.BudgetFinal || run.Outcome != outcomeTruncated || run.Text != "Compare the lamps" {
			t.Fatalf("run=%+v, err=%v", run, err)
		}
	})
}

func TestAnswerOutcomeBlank(t *testing.T) {
	response := stubResponse{status: http.StatusOK, body: `{"choices":[{"message":{"content":"  \n"},"finish_reason":"length"}]}`}
	s, stub := newAnswerTestServer(t, nil, 2, response)
	run := runTestAnswer(t, s, stub, nil)
	if run.Outcome != outcomeNoAnswer || strings.TrimSpace(run.Text) != "" {
		t.Fatalf("run=%+v", run)
	}
}

func TestAnswerGenerationRejection(t *testing.T) {
	settings := &GenerationSettings{Temperature: new(0.5)}
	response := stubResponse{status: http.StatusUnprocessableEntity, body: "temperature is unsupported"}
	assertRejected := func(t *testing.T, err error) {
		t.Helper()
		var rejected *generationRejection
		if !errors.As(err, &rejected) {
			t.Fatalf("err=%v, want generation rejection", err)
		}
	}
	t.Run("normal", func(t *testing.T) {
		s, stub := newAnswerTestServer(t, nil, 2, response)
		_, err := s.runAnswer(t.Context(), AnswerRequest{Question: "Find a lamp.", Domain: answerTestDomain, Generation: settings}, nil)
		stub.srv.Close()
		assertRejected(t, err)
		if len(stub.requests) != 1 {
			t.Fatal("unexpected retry of a generation rejection")
		}
	})
	t.Run("budget", func(t *testing.T) {
		_, err := runAnswerWithBudgetFinal(t, response, settings)
		assertRejected(t, err)
	})
}
