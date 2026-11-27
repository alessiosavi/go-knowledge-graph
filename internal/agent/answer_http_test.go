package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/docs"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"go.yaml.in/yaml/v4"
)

const validAnswerQuestion = `{"question":"Find lamps","domain":"catalog"}`

type answerHTTPStore struct {
	*answerTestStore
	cards     graph.ProductCards
	docs      map[string]graph.ChunkDocument
	domains   func(context.Context, string) (bool, []string, error)
	readCards func(context.Context) error
	readDocs  func(context.Context) error

	hydratedProducts []string
	hydratedChunks   []string
	cardReads        int
	documentReads    int
}

func (s *answerHTTPStore) DomainStores(ctx context.Context, domain string) (bool, []string, error) {
	if s.domains != nil {
		return s.domains(ctx, domain)
	}
	return domain == answerTestDomain, []string{"shop.example"}, nil
}

func (s *answerHTTPStore) ProductCards(ctx context.Context, domain string, ids []string, maxFeatures int) (graph.ProductCards, error) {
	if domain != answerTestDomain || maxFeatures != 40 {
		return graph.ProductCards{}, fmt.Errorf("unexpected cards scope %q or feature cap %d", domain, maxFeatures)
	}
	s.hydratedProducts = slices.Clone(ids)
	s.cardReads++
	if s.readCards != nil {
		return graph.ProductCards{}, s.readCards(ctx)
	}
	out := graph.ProductCards{Cards: make(map[string]graph.ProductCard), AsOf: s.cards.AsOf}
	for _, id := range ids {
		if card, ok := s.cards.Cards[id]; ok && card.Domain == domain {
			out.Cards[id] = card
		}
	}
	return out, nil
}

func (s *answerHTTPStore) ChunkDocuments(ctx context.Context, domain string, ids []string) (map[string]graph.ChunkDocument, error) {
	if domain != answerTestDomain {
		return nil, fmt.Errorf("unexpected document scope %q", domain)
	}
	s.hydratedChunks = slices.Clone(ids)
	s.documentReads++
	if s.readDocs != nil {
		return nil, s.readDocs(ctx)
	}
	out := make(map[string]graph.ChunkDocument)
	for _, id := range ids {
		if doc, ok := s.docs[id]; ok {
			out[id] = doc
		}
	}
	return out, nil
}

func newAnswerHTTPServer(t *testing.T, store *answerHTTPStore, steps int, replies ...stubResponse) (*Server, *llmStub) {
	t.Helper()
	s, stub := newAnswerTestServer(t, store, steps, replies...)
	s.cfg.Agent.AnswerEnabled = true
	return s, stub
}

func postAnswer(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, s.AnswerHandler(), http.MethodPost, "/v1/agent/answer", body, map[string]string{"Content-Type": "application/json"})
}

func expectAnswerHTTP(t *testing.T, rr *httptest.ResponseRecorder, status int, kind, detail string) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status = %d, want %d, body = %s", rr.Code, status, rr.Body)
	}
	if kind == "" {
		return
	}
	var body errorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Error != kind || (detail != "" && body.Detail != detail) {
		t.Fatalf("error response = %s, want %s %s", rr.Body, kind, detail)
	}
}

func TestAnswerHTTPAlternatives(t *testing.T) {
	store := answerJourneyStore()
	foreign := answerCard("foreign.example:1", "foreign")
	foreign.Store = "foreign.example"
	moved := foreign
	moved.Domain = "other-catalog"
	store.cards.Cards[foreign.ProductID] = moved
	searchProducts := store.productSearchToolStore.products
	store.productSearchToolStore.products = func(ctx context.Context, query, domain string, limit int) ([]graph.Product, error) {
		products, err := searchProducts(ctx, query, domain, limit)
		return append(products, foreign.Product), err
	}
	s, _ := newAnswerHTTPServer(t, store, 3,
		toolCallsReply(
			scriptedCall{"find", "search", lampSearch},
			scriptedCall{"detail", "get_product", `{"id":"a"}`},
		),
		contentReply("Compare Lamp B [["+lampB+"]] and Lamp C [["+lampC+"]]. Lamp D is excluded. "+
			"Try Lamp A [[!alt:"+lampA+"]] [[chunk-a]]. Another option is a lamp [[!ALT: "+foreign.ProductID+"]]."),
	)
	got := performAnswerJourney(t, s, `{"question":"Compare Lamp B and Lamp C, excluding Lamp D. What else is close?","domain":"catalog"}`)
	assertAnswerHydration(t, store, []string{lampB, lampC, lampA, foreign.ProductID}, []string{"chunk-a"})
	if got.Summary != "Compare Lamp B  and Lamp C. Lamp D is excluded. Try Lamp A. Another option is a lamp." {
		t.Errorf("summary = %q", got.Summary)
	}
	if got.Dropped != (answerDropped{NotFound: 1}) {
		t.Errorf("dropped = %+v", got.Dropped)
	}
	if len(got.Alternatives) != 1 || got.Alternatives[0].ProductID != lampA ||
		!slices.Equal(got.Alternatives[0].PassageIDs, []string{"chunk-a"}) {
		t.Errorf("alternatives = %+v", got.Alternatives)
	}
	if len(got.Passages) != 1 || len(got.Passages[0].Links) != 1 ||
		got.Passages[0].Links[0].ProductID != lampA || got.Passages[0].Links[0].Kind != "mention" {
		t.Errorf("passages = %+v", got.Passages)
	}
	if !slices.Equal(answerCardGraphIDs(got.Products), []string{"b", "c"}) ||
		got.Comparison == nil || !slices.Equal(got.Comparison.ProductIDs, []string{lampB, lampC}) ||
		len(got.Comparison.Rows) != 0 {
		t.Errorf("products = %+v, comparison = %+v", got.Products, got.Comparison)
	}
}

func TestAnswerHTTPAlternativesOnly(t *testing.T) {
	store := answerJourneyStore()
	s, _ := newAnswerHTTPServer(t, store, 3,
		toolCallReply("find", "search", lampSearch),
		contentReply("[[!alt:"+lampA+"]]"),
	)
	got := performAnswerJourney(t, s, validAnswerQuestion)
	assertAnswerHydration(t, store, []string{lampA}, nil)
	if got.Products == nil || len(got.Products) != 0 || !slices.Equal(answerCardGraphIDs(got.Alternatives), []string{"a"}) ||
		got.Summary != "" || got.Comparison != nil || len(got.Warnings) != 0 || got.Dropped != (answerDropped{}) {
		t.Fatalf("alternatives-only response = %+v", got)
	}
}

func TestAnswerReview2ReadsEntireBoundedJSONBody(t *testing.T) {
	padding := strings.Repeat(" ", (1<<20)-len(validAnswerQuestion))
	for _, tc := range []struct {
		name, suffix string
		chunked      bool
		status       int
		kind         string
	}{
		{"at limit", padding, false, http.StatusOK, ""},
		{"over limit", padding + " ", false, http.StatusRequestEntityTooLarge, "body_too_large"},
		{"over limit chunked", padding + " ", true, http.StatusRequestEntityTooLarge, "body_too_large"},
		{"over limit with invalid suffix", "x" + padding, true, http.StatusRequestEntityTooLarge, "body_too_large"},
		{"trailing JSON object", "\n{}", false, http.StatusBadRequest, "invalid_json"},
		{"trailing non-JSON", "\nx", false, http.StatusBadRequest, "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newAnswerHTTPServer(t, &answerHTTPStore{}, 2, contentReply("A lamp"))
			r := httptest.NewRequest(http.MethodPost, "/v1/agent/answer", strings.NewReader(validAnswerQuestion+tc.suffix))
			r.Header.Set("Content-Type", "application/json")
			if tc.chunked {
				r.ContentLength = -1
				r.TransferEncoding = []string{"chunked"}
			}
			rr := httptest.NewRecorder()
			s.AnswerHandler().ServeHTTP(rr, r)
			expectAnswerHTTP(t, rr, tc.status, tc.kind, "")
		})
	}
}

type unreadAnswerBody struct {
	t *testing.T
}

func (b unreadAnswerBody) Read([]byte) (int, error) {
	b.t.Error("body was read despite an oversized Content-Length")
	return 0, io.EOF
}

func TestAnswerReview2_4RejectsOversizedContentLengthBeforeReading(t *testing.T) {
	s, _ := newAnswerHTTPServer(t, &answerHTTPStore{}, 2)
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/answer", unreadAnswerBody{t: t})
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = responseBodyCap + 1
	rr := httptest.NewRecorder()
	s.AnswerHandler().ServeHTTP(rr, r)
	expectAnswerHTTP(t, rr, http.StatusRequestEntityTooLarge, "body_too_large", "")
}

func TestAnswerDisabled501(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := config.Config{}
		cfg.Agent.Enabled = enabled
		s := NewServer(cfg, nil, nil)
		expectAnswerHTTP(t, postAnswer(t, s, "broken json"), 501, "agent_disabled", "")
	}
}

func TestAnswerDomainValidation(t *testing.T) {
	store := &answerHTTPStore{}
	s, stub := newAnswerHTTPServer(t, store, 2)
	for _, tc := range []struct{ body, detail string }{
		{`{"question":"Find lamps"}`, "domain_required"},
		{`{"question":"Find lamps","domain":"NOT VALID"}`, "invalid_domain"},
		{`{"question":"Find lamps","domain":"missing"}`, "unknown_domain"},
	} {
		expectAnswerHTTP(t, postAnswer(t, s, tc.body), 400, "invalid_request", tc.detail)
	}
	expectAnswerHTTP(t, postAnswer(t, s, `{"question":"  ","domain":"catalog"}`), 400, "invalid_request", "question is required")
	stub.srv.Close()
	if len(stub.requests) != 0 {
		t.Fatal("invalid requests reached the provider")
	}
}

func TestAnswerContextValidation(t *testing.T) {
	s, stub := newAnswerHTTPServer(t, &answerHTTPStore{}, 2, contentReply("A lamp"))
	for _, tc := range []struct {
		id     string
		status int
		kind   string
	}{
		{"shop.example:", 200, ""},
		{"other.example:1", 400, "invalid_context"},
		{"shop.example", 400, "invalid_context"},
		{"SHOP.example:1", 400, "invalid_context"},
	} {
		body := answerJSON(t, AnswerRequest{Question: "Find lamps", Domain: answerTestDomain, Context: &AnswerContext{ProductIDs: []string{tc.id}}})
		expectAnswerHTTP(t, postAnswer(t, s, body), tc.status, tc.kind, "")
	}
	stub.srv.Close()
	if len(stub.requests) != 1 {
		t.Errorf("provider got %d requests, want 1", len(stub.requests))
	}
}

func TestAnswerHydrationError502(t *testing.T) {
	failed := func(context.Context) error { return errors.New("fixture graph read failed") }
	for _, tc := range []struct {
		name  string
		store *answerHTTPStore
	}{
		{"domains", &answerHTTPStore{domains: func(context.Context, string) (bool, []string, error) { return false, nil, failed(nil) }}},
		{"cards", &answerHTTPStore{readCards: failed}},
		{"documents", &answerHTTPStore{readDocs: failed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newAnswerHTTPServer(t, tc.store, 2, contentReply("A lamp"))
			expectAnswerHTTP(t, postAnswer(t, s, validAnswerQuestion), 502, "hydration_error", "")
		})
	}
}

func TestAgentStatusAnswerField(t *testing.T) {
	for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}, {true, true}} {
		cfg := config.Config{}
		cfg.Agent.Enabled, cfg.Agent.AnswerEnabled = flags[0], flags[1]
		s := NewServer(cfg, nil, nil)
		rr := doReq(t, s.StatusHandler(), http.MethodGet, "/v1/agent/status", "", nil)
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body["answer"] != (flags[0] && flags[1]) {
			t.Errorf("flags %v: status = %s", flags, rr.Body)
		}
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Required   []string
				Properties map[string]struct{ Type string }
			}
		}
	}
	if err := yaml.Unmarshal(docs.OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	status := spec.Components.Schemas["AgentStatus"]
	if !slices.Contains(status.Required, "answer") || status.Properties["answer"].Type != "boolean" {
		t.Error("AgentStatus must require a boolean answer field")
	}

}

func TestAnswerHistoryBounds(t *testing.T) {
	makeTurns := func(n, size int) []HistoryTurn {
		turns := make([]HistoryTurn, n)
		for i := range turns {
			turns[i] = HistoryTurn{Role: "user", Text: strings.Repeat("x", size)}
		}
		return turns
	}
	for _, tc := range []struct {
		name    string
		history []HistoryTurn
		status  int
	}{
		{"six", makeTurns(6, 1), 200},
		{"seven", makeTurns(7, 1), 400},
		{"1000 characters", []HistoryTurn{{Role: "assistant", Text: strings.Repeat("è", 1000)}}, 200},
		{"1001 characters", makeTurns(1, 1001), 400},
		{"total at 4 KiB", append(makeTurns(4, 1000), makeTurns(1, 96)...), 200},
		{"total above 4 KiB", append(makeTurns(4, 1000), makeTurns(1, 97)...), 400},
		{"unsupported role", []HistoryTurn{{Role: "system", Text: "Instructions"}}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, stub := newAnswerHTTPServer(t, &answerHTTPStore{}, 2, contentReply("A lamp"))
			body := answerJSON(t, AnswerRequest{Question: "Find lamps", Domain: answerTestDomain, Context: &AnswerContext{History: tc.history}})
			kind, calls := "", 1
			if tc.status == 400 {
				kind, calls = "invalid_context", 0
			}
			expectAnswerHTTP(t, postAnswer(t, s, body), tc.status, kind, "")
			stub.srv.Close()
			if len(stub.requests) != calls {
				t.Errorf("provider got %d requests, want %d", len(stub.requests), calls)
			}
		})
	}
}

func TestAnswerReview4HistoryTurnRequiresTextAndRole(t *testing.T) {
	for _, tc := range []struct {
		name, turn string
		status     int
		kind       string
	}{
		{"missing text", `{"role":"user"}`, http.StatusBadRequest, "invalid_context"},
		{"null text", `{"role":"user","text":null}`, http.StatusBadRequest, "invalid_context"},
		{"missing role", `{"text":"A lamp"}`, http.StatusBadRequest, "invalid_context"},
		{"explicit empty text", `{"role":"user","text":""}`, http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newAnswerHTTPServer(t, &answerHTTPStore{}, 2, contentReply("A lamp"))
			body := `{"question":"Find lamps","domain":"catalog","context":{"history":[` + tc.turn + `]}}`
			expectAnswerHTTP(t, postAnswer(t, s, body), tc.status, tc.kind, "")
		})
	}
}

func TestAnswerReview2_3RepeatedHistoryDecodesFreshTurns(t *testing.T) {
	for _, tc := range []struct{ name, turn, detail string }{
		{"missing text", `{"role":"assistant"}`, "context history text is required"},
		{"missing role", `{"text":"A lamp"}`, "context history roles must be user or assistant"},
		{"null turn", `null`, "context history roles must be user or assistant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newAnswerHTTPServer(t, &answerHTTPStore{}, 2, contentReply("A lamp"))
			body := `{"question":"Find lamps","domain":"catalog","context":{"history":[{"role":"user","text":"Find a lamp"}],"history":[` + tc.turn + `]}}`
			expectAnswerHTTP(t, postAnswer(t, s, body), http.StatusBadRequest, "invalid_context", tc.detail)
		})
	}
}

func TestAnswerDisabledCombos(t *testing.T) {
	for _, flags := range [][2]bool{{false, true}, {true, false}} {
		cfg := config.Config{}
		cfg.Agent.Enabled, cfg.Agent.AnswerEnabled = flags[0], flags[1]
		expectAnswerHTTP(t, postAnswer(t, NewServer(cfg, nil, nil), validAnswerQuestion), 501, "agent_disabled", "")
	}
}

func TestAnswerContextIDBounds(t *testing.T) {
	var ids []string
	for i := range 11 {
		ids = append(ids, fmt.Sprintf("shop.example:%d", i))
	}
	for _, tc := range []struct {
		ids    []string
		status int
		kind   string
	}{
		{ids[:10], 200, ""},
		{ids, 400, "invalid_context"},
		{[]string{"shop.example:" + strings.Repeat("x", 1024-len("shop.example:"))}, 200, ""},
		{[]string{"shop.example:" + strings.Repeat("x", 1025-len("shop.example:"))}, 400, "invalid_context"},
	} {
		s, _ := newAnswerHTTPServer(t, &answerHTTPStore{}, 2, contentReply("A lamp"))
		body := answerJSON(t, AnswerRequest{Question: "Find lamps", Domain: answerTestDomain, Context: &AnswerContext{ProductIDs: tc.ids}})
		expectAnswerHTTP(t, postAnswer(t, s, body), tc.status, tc.kind, "")
	}
}

func TestAnswerGenerationRejected400BeforeProvider(t *testing.T) {
	store := &answerHTTPStore{domains: func(context.Context, string) (bool, []string, error) {
		t.Error("invalid generation settings reached the graph")
		return true, []string{"shop.example"}, nil
	}}
	s, stub := newAnswerHTTPServer(t, store, 2)
	for _, body := range []string{
		`{"question":"Find lamps","domain":"catalog","generation":{"temperature":3}}`,
		`{"question":"Find lamps","domain":"catalog","generation":{"top_k":1}}`,
	} {
		expectAnswerHTTP(t, postAnswer(t, s, body), 400, "invalid_request", "")
	}
	stub.srv.Close()
	if len(stub.requests) != 0 {
		t.Error("invalid generation settings reached the provider")
	}
}

func waitForAnswerDeadline(t *testing.T, ctx context.Context) error {
	t.Helper()
	if _, ok := ctx.Deadline(); !ok {
		t.Error("graph call has no deadline")
		return errors.New("graph call has no deadline")
	}
	<-ctx.Done()
	return errors.New("graph fails after the deadline")
}

func TestAnswerDeadlineDuringDomainLookup504(t *testing.T) {
	store := &answerHTTPStore{domains: func(ctx context.Context, _ string) (bool, []string, error) {
		return false, nil, waitForAnswerDeadline(t, ctx)
	}}
	s, stub := newAnswerHTTPServer(t, store, 2)
	s.timeout = 25 * time.Millisecond
	expectAnswerHTTP(t, postAnswer(t, s, validAnswerQuestion), 504, "agent_timeout", "")
	stub.srv.Close()
	if len(stub.requests) != 0 {
		t.Error("expired domain lookup reached the provider")
	}
}

// The deadline can pass before the context timer publishes its error.
type elapsedAnswerContext struct{ context.Context }

func (elapsedAnswerContext) Deadline() (time.Time, bool) {
	return time.Now().Add(-time.Second), true
}

func TestAnswerDeadlineDuringHydration504(t *testing.T) {
	t.Run("deadline clock elapsed", func(t *testing.T) {
		ctx := elapsedAnswerContext{t.Context()}
		rr := httptest.NewRecorder()
		writeAnswerReadError(rr, ctx, errors.New("graph fails after the deadline"))
		expectAnswerHTTP(t, rr, 504, "agent_timeout", "")
	})
	for _, read := range []string{"cards", "documents"} {
		t.Run(read, func(t *testing.T) {
			var deadline time.Time
			store := &answerHTTPStore{domains: func(ctx context.Context, _ string) (bool, []string, error) {
				deadline, _ = ctx.Deadline()
				return true, []string{"shop.example"}, nil
			}}
			fail := func(ctx context.Context) error {
				if got, _ := ctx.Deadline(); !got.Equal(deadline) {
					t.Errorf("hydration deadline = %s, want original deadline %s", got, deadline)
				}
				return waitForAnswerDeadline(t, ctx)
			}
			if read == "cards" {
				store.readCards = fail
			} else {
				store.readDocs = fail
			}
			s, _ := newAnswerHTTPServer(t, store, 2, contentReply("A lamp"))
			s.timeout = 200 * time.Millisecond
			expectAnswerHTTP(t, postAnswer(t, s, validAnswerQuestion), 504, "agent_timeout", "")
			if store.cardReads != 1 || (read == "documents" && store.documentReads != 1) {
				t.Errorf("hydration reads: cards=%d, documents=%d", store.cardReads, store.documentReads)
			}
		})
	}
}
