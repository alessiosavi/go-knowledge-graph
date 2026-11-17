package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunkread"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestAskDomainPrompt(t *testing.T) {
	for _, mode := range []string{"tools", "plan"} {
		for _, scope := range []struct {
			name   string
			domain string
			suffix string
		}{
			{name: "unscoped"},
			{name: "scoped", domain: "a", suffix: "\nDomain scope: a. Answer tools enforce request scope; model arguments cannot widen it. Cypher is not domain-enforced; apply the stated domain yourself."},
		} {
			t.Run(mode+"/"+scope.name, func(t *testing.T) {
				replies := []stubResponse{contentReply("Quillon")}
				prompts := []string{systemPrompt}
				if mode == "plan" {
					replies = []stubResponse{{status: http.StatusBadRequest, body: `{"error":"tools unsupported"}`}, contentReply(`{"answer":"Quillon"}`)}
					prompts = append(prompts, planSystemPrompt)
				}
				stub := newLLMStub(t, replies...)
				s := NewServer(enabledCfg(stub.srv.URL), nil, nil)
				if _, err := s.Ask(context.Background(), AskRequest{Question: "Quillon?", Domain: scope.domain}); err != nil {
					t.Fatal(err)
				}
				if len(stub.requests) != len(prompts) {
					t.Fatalf("provider calls=%d, want %d", len(stub.requests), len(prompts))
				}
				for i, prompt := range prompts {
					msgs := stub.requests[i]["messages"].([]any)
					got := msgs[0].(map[string]any)["content"].(string)
					if want := prompt + scope.suffix; got != want {
						t.Errorf("system prompt %d = %q, want %q", i, got, want)
					}
				}
			})
		}
	}
}

func TestAskInvalidDomain(t *testing.T) {
	stub := newLLMStub(t)
	s := NewServer(enabledCfg(stub.srv.URL), nil, nil)
	for _, route := range []struct {
		path    string
		handler http.Handler
	}{
		{"/v1/agent/ask", s.AskHandler()},
		{"/v1/agent/ask/stream", s.StreamHandler()},
	} {
		t.Run(route.path, func(t *testing.T) {
			rr := doReq(t, route.handler, http.MethodPost, route.path, `{"question":"Quillon?","domain":"BAD"}`, nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("invalid domain HTTP %d: %s", rr.Code, rr.Body.String())
			}
			var body errorBody
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error != "invalid_request" || body.Detail != "invalid domain slug" {
				t.Fatalf("invalid domain response=%+v", body)
			}
		})
	}
	if len(stub.requests) != 0 {
		t.Fatalf("invalid domain reached provider: %d calls", len(stub.requests))
	}
}

func TestAskScopeEnforced(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	word := fmt.Sprintf("zorbo%d", time.Now().UnixNano())
	ids := map[string]string{}
	for _, d := range []string{"a", "b"} {
		doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: word + d, Hash: word, Domain: d})
		if err != nil {
			t.Fatal(err)
		}
		ids[d] = doc.ID
		t.Cleanup(func() { _ = st.DeleteDocument(ctx, doc.ID) })
		if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Seq: 0, Text: word}}); err != nil {
			t.Fatal(err)
		}
		key := graph.DomainKey(d, word)
		if err := st.UpsertExtractionOpts(ctx, doc.ID, doc.ID+"-c0", "mock", []graph.EntityUpsert{{Key: key, Name: word}}, nil, graph.WriteOptions{Domain: d}); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"ask", "http", "stream", "plan"} {
		t.Run(mode, func(t *testing.T) {
			calls := []stubResponse{
				toolCallReply("1", "get_chunks", fmt.Sprintf(`{"ids":[%q],"domain":"b"}`, ids["b"]+"-c0")),
				toolCallReply("2", "search", fmt.Sprintf(`{"query":%q,"kind":"chunk","domain":"b"}`, word)),
				toolCallReply("3", "get_entity", fmt.Sprintf(`{"id":%q,"domain":"b"}`, graph.EntityID(graph.DomainKey("b", word)))),
				toolCallReply("4", "traverse", fmt.Sprintf(`{"entity":%q,"domain":"b"}`, graph.DomainKey("b", word))),
				toolCallReply("5", "list_documents", `{"domain":"b","limit":1000}`), contentReply("Quillon")}
			if mode == "plan" {
				calls = []stubResponse{{status: 400, body: `{"error":"tools unsupported"}`}, contentReply(fmt.Sprintf(`{"tool_calls":[{"name":"get_chunks","arguments":{"ids":[%q],"domain":"b"}},{"name":"search","arguments":{"query":%q,"kind":"chunk","domain":"b"}}]}`, ids["b"]+"-c0", word)), contentReply("Quillon")}
			}
			stub := newLLMStub(t, calls...)
			cfg := config.Config{}
			cfg.Agent.Enabled = true
			cfg.Agent.BaseURL = stub.srv.URL
			cfg.Agent.Model = "stub"
			cfg.Agent.MaxSteps = 8
			s := NewServer(cfg, st, nil)
			var resp AskResponse
			if mode == "http" || mode == "stream" {
				rr := httptest.NewRecorder()
				r := httptest.NewRequest("POST", "/", strings.NewReader(`{"question":"Quillon?","domain":"a"}`))
				if mode == "http" {
					s.AskHandler().ServeHTTP(rr, r)
					if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
						t.Fatal(err)
					}
				} else {
					s.StreamHandler().ServeHTTP(rr, r)
					for _, line := range strings.Split(rr.Body.String(), "\n") {
						if strings.HasPrefix(line, "data: ") {
							var out AskResponse
							if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &out) == nil && out.Answer != "" {
								resp = out
							}
						}
					}
				}
				if rr.Code != 200 {
					t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
				}
			} else {
				var err error
				resp, err = s.Ask(ctx, AskRequest{Question: "Quillon?", Domain: "a"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(resp.Evidence) < 2 {
				t.Fatalf("evidence=%+v", resp)
			}
			var chunks chunkread.Result
			_ = json.Unmarshal(resp.Evidence[0].Result, &chunks)
			if len(chunks.Chunks) != 0 || len(chunks.Meta.Missing) != 1 {
				t.Fatalf("out-of-scope chunks=%s", resp.Evidence[0].Result)
			}
			var hits struct{ Hits []struct{ Domain string } }
			_ = json.Unmarshal(resp.Evidence[1].Result, &hits)
			if len(hits.Hits) != 1 || hits.Hits[0].Domain != "a" {
				t.Fatalf("search=%s", resp.Evidence[1].Result)
			}
			if mode != "plan" {
				for _, i := range []int{2, 3} {
					if !strings.Contains(string(resp.Evidence[i].Result), "not found") {
						t.Errorf("outside entity=%s", resp.Evidence[i].Result)
					}
				}
				var docs documentsOut
				_ = json.Unmarshal(resp.Evidence[4].Result, &docs)
				for _, d := range docs.Documents {
					if d.Domain != "a" {
						t.Errorf("document=%+v", d)
					}
				}
			}
			msgs := stub.requests[0]["messages"].([]any)
			if !strings.Contains(msgs[0].(map[string]any)["content"].(string), "Domain scope: a") {
				t.Fatal("scope absent from system prompt")
			}
		})
	}
}
